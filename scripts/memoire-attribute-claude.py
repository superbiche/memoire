#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Attribution-at-Stop worker for Claude Code (rework candidate
docs/rework/retrieval-consumer/attribution-at-stop.md; the measured hook v3 of the
replay, memoire-replay spike-ad3b8ed9 hooks/stop-attrib.py, ported to production).

Called by scripts/memoire-attribute-claude.sh with the Stop hook payload file (owned
and deleted here). Steps, each journaled under $MEMOIRE_HOME/hooks/attrib/<session>.jsonl:

  1. scan the transcript from the last human turn; no `mcp__memoire__*` call -> skip;
  2. governor: one attribution per operator turn, by an ATOMIC claim file per
     (session, turn start) created with O_EXCL: a subagent report reopening the stop
     cycle, or two Stop deliveries racing, find the claim taken and skip;
  3. ask the question in a fork of the session, headless, same model, no tools:
     `claude -p --resume <sid> --fork-session --session-id <fork id> --model <m> --tools ""
     --output-format json`, the fork id chosen HERE and registered before launch, the
     harness env and any Anthropic token stripped, MEMOIRE_ATTRIB_FORK=1 set so the fork's
     own hooks stay silent;
  4. dispose of the fork's transcript, on success AND on failure (the transcript ingest
     and restore-state treat one jsonl as one session, impact view A1; the ingest also
     skips every registered fork id, which covers the time the fork is running);
  5. parse the fenced `memoire` block, mark each verdict served or not against the ids
     the turn was actually served, and write ONE `memory_usage` event through
     bin/memoire-queue; a fork failure is journaled and writes no event (an absent
     signal is absent, never "unused").

Lifecycle: the entry script runs this worker as the main process of a transient
systemd user unit. The unit's control group holds the worker, the fork and whatever
the fork starts; the user manager enforces the runtime cap and stops the whole group
(SIGTERM, then SIGKILL). There is no timeout here. When the unit is stopped the worker
takes the ordinary failure path: fork group stopped, transcript disposed of, failure
journaled, no event. A worker killed outright leaves that to the unit's end and to the
next run's sweep. The hook's exit status is the entry script's business, never this
program's.
"""
import hashlib
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import traceback
import uuid

HOOK_VERSION = "4"
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MEMOIRE_HOME = os.environ.get("MEMOIRE_HOME") or os.path.expanduser("~/.memoire")
ATTRIB_DIR = os.path.join(MEMOIRE_HOME, "hooks", "attrib")
FORKS_DIR = os.path.join(ATTRIB_DIR, "forks")
CLAIMS_DIR = os.path.join(ATTRIB_DIR, "claims")
# One fork session id per line; scripts/memoire-capture-claude-transcripts.sh skips them.
FORK_IDS = os.path.join(ATTRIB_DIR, "fork-ids")
QUEUE_BIN = os.environ.get("MEMOIRE_QUEUE_BIN") or os.path.join(ROOT, "bin", "memoire-queue")
CLAUDE_BIN = os.environ.get("MEMOIRE_CLAUDE_BIN") or "claude"
# Set by the entry script: the unit this worker is the main process of.
UNIT = os.environ.get("MEMOIRE_ATTRIB_UNIT") or ""
# A label for the event. Every attribution the hook starts is `background`; `direct`
# is the worker run by hand, outside a unit.
MODE = "background" if UNIT else "direct"
DEFAULT_CAP = 200


def runtime_cap():
    """The unit's runtime cap in seconds, as the entry script validated it. Used here
    only to tell a stale fork transcript from one still being written."""
    try:
        cap = int(os.environ.get("MEMOIRE_ATTRIB_TIMEOUT") or DEFAULT_CAP)
    except ValueError:
        return DEFAULT_CAP
    return cap if cap > 0 else DEFAULT_CAP


DEFAULT_MODEL = os.environ.get("MEMOIRE_ATTRIB_MODEL") or ""
FORK_DISPOSITION = "delete" if os.environ.get("MEMOIRE_ATTRIB_FORK_TRANSCRIPT") == "delete" else "move"
VERDICTS = ("used", "unused", "misapplied", "contradicted-by-files", "fetch-failed")
ID_RE = re.compile(r"\[(\d{5,7})\]")
LINE_RE = re.compile(r"^\s*\[(\d{5,7})\]\s*([a-z-]+)\s*:\s*(.*?)\s*$")
BLOCK_RE = re.compile(r"```memoire[ \t]*\r?\n(.*?)```", re.S)
STRIP_ENV = ("CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SSE_PORT",
             "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")

# The measured wording (replay hook v3, runs 43-45 and 17-19). Changing a word changes
# the measure: re-run an arm before editing.
QUESTION = (
    "Before you stop: for each claim in the reply you just gave (everything you wrote since the last "
    "operator message), say where it came from: a file you read (name it), a Memoire hit (give its [id]), or "
    "your own inference from the code or the ticket. Put the Memoire lines in one fenced block tagged "
    "`memoire`, one line per observation you fetched or relied on in this turn, in this exact form and "
    "nothing else inside the block:\n\n[id] <verdict>: <the claim it carried, or the reason>\n\nwhere "
    "<verdict> is exactly one of: used (relied on in the reply), unused (seen or fetched, not relied on), "
    "misapplied (relied on, then found not to fit this case), contradicted-by-files (a file you read says "
    "otherwise; name it), fetch-failed (you asked for its body and did not get it). File and inference "
    "lines are free text outside the block. Then list the claims, if any, that rest on Memoire alone, in two "
    "groups: memory-only (you checked: no file, command output or ticket text carries it) and unverified "
    "(not checked against the files yet). If no claim came from Memoire, say so plainly and mark every "
    "observation you fetched as unused."
)


def safe_name(s):
    return re.sub(r"[^A-Za-z0-9._-]", "_", s or "unknown")[:120]


def journal(session_id, **kw):
    os.makedirs(ATTRIB_DIR, exist_ok=True)
    kw["ts"] = time.time()
    kw["mono"] = time.monotonic()
    with open(os.path.join(ATTRIB_DIR, safe_name(session_id) + ".jsonl"), "a") as f:
        f.write(json.dumps(kw, ensure_ascii=False) + "\n")


def load_entries(path):
    out = []
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            for line in f:
                try:
                    out.append(json.loads(line))
                except ValueError:
                    pass
    except OSError:
        return None
    return out


def turn_start(entries):
    """Index of the last operator prompt: a human user entry that is not meta. Fallback:
    the last prompt-shaped user entry."""
    start = None
    for i, e in enumerate(entries):
        if e.get("type") == "user" and not e.get("isMeta") and (
            (e.get("origin") or {}).get("kind") == "human" or e.get("turnOrigin") == "human"
        ):
            start = i
    if start is None:
        start = 0
        for i, e in enumerate(entries):
            if e.get("type") != "user" or e.get("isMeta"):
                continue
            c = (e.get("message") or {}).get("content")
            if isinstance(c, str) or (
                isinstance(c, list) and c and all(isinstance(b, dict) and b.get("type") == "text" for b in c)
            ):
                start = i
    return start


def memoire_calls(entries, start):
    uses, calls = {}, []
    for e in entries[start:]:
        if e.get("type") != "assistant":
            continue
        for b in (e.get("message") or {}).get("content") or []:
            if isinstance(b, dict) and b.get("type") == "tool_use" and str(b.get("name", "")).startswith("mcp__memoire__"):
                uses[b.get("id")] = b.get("name")
                calls.append(b.get("name"))
    return uses, calls


def served_ids(entries, start, uses):
    served = set()
    for e in entries[start:]:
        if e.get("type") != "user":
            continue
        c = (e.get("message") or {}).get("content")
        for b in (c if isinstance(c, list) else []):
            if isinstance(b, dict) and b.get("type") == "tool_result" and b.get("tool_use_id") in uses:
                s = b.get("content")
                s = s if isinstance(s, str) else json.dumps(s, ensure_ascii=False)
                served |= set(ID_RE.findall(s))
    return sorted(served, key=int)


def session_model(entries):
    for e in reversed(entries):
        if e.get("type") == "assistant":
            m = (e.get("message") or {}).get("model")
            if m and not str(m).startswith("<"):
                return m
    return None


def parse_groups(after_block):
    """Best-effort read of the two 'Memoire alone' groups after the block: a line naming
    memory-only or unverified opens the group; bullet or bracketed lines fill it; 'none'
    leaves it empty. The raw text is kept in the event for a human read."""
    groups = {"memory_only": [], "unverified": []}
    current = None
    for raw in after_block.splitlines():
        line = raw.strip()
        if not line:
            continue
        low = line.lower()
        head = None
        if "memory-only" in low or "memory only" in low:
            head = "memory_only"
        elif "unverified" in low:
            head = "unverified"
        if head and (low.startswith(("memory", "unverified", "**", "#", "- **", "* **")) or low.endswith(":")):
            current = head
            rest = line.split(":", 1)[1].strip() if ":" in line else ""
            if rest and rest.lower().rstrip(".") not in ("none", "aucun", "aucune", "nothing"):
                groups[current].append(rest)
            continue
        if current and re.match(r"^\s*([-*•]|\d+[.)])\s+", raw):
            groups[current].append(re.sub(r"^\s*([-*•]|\d+[.)])\s+", "", raw).strip())
    return groups


def parse_attribution(text, served):
    """Parse the fenced block. `served` is the set of ids the turn was served (strings):
    the model's text is untrusted, so a verdict naming an id outside that set is kept in
    the event, marked served=false, and never projected (ids named must be a subset of
    ids served, the card's first deterministic check)."""
    m = BLOCK_RE.search(text or "")
    if not m:
        return {"parse_ok": False, "verdicts": [], "memory_only": [], "unverified": [], "block": "",
                "ids_subset_served": True, "unserved_ids": []}
    verdicts, ok, unserved = [], True, []
    for raw in m.group(1).splitlines():
        if not raw.strip():
            continue
        lm = LINE_RE.match(raw)
        if not lm or lm.group(2) not in VERDICTS:
            ok = False
            verdicts.append({"id": None, "verdict": "unparsed", "reason": raw.strip(), "served": False})
            continue
        is_served = lm.group(1) in served
        if not is_served and int(lm.group(1)) not in unserved:
            unserved.append(int(lm.group(1)))
        verdicts.append({"id": int(lm.group(1)), "verdict": lm.group(2), "reason": lm.group(3), "served": is_served})
    groups = parse_groups(text[m.end():])
    return {"parse_ok": ok, "verdicts": verdicts, "block": m.group(1),
            "ids_subset_served": not unserved, "unserved_ids": unserved, **groups}


def claim_turn(session_id, turn_uuid):
    """Atomic once-per-turn claim. True when this process owns the turn."""
    os.makedirs(CLAIMS_DIR, exist_ok=True)
    name = hashlib.sha256(("%s:%s" % (session_id, turn_uuid)).encode()).hexdigest()
    try:
        fd = os.open(os.path.join(CLAIMS_DIR, name), os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    except FileExistsError:
        return False
    with os.fdopen(fd, "w") as f:
        f.write(json.dumps({"session_id": session_id, "turn_start_uuid": turn_uuid, "pid": os.getpid(), "ts": time.time()}) + "\n")
    return True


def register_fork_id(fork_sid):
    os.makedirs(ATTRIB_DIR, exist_ok=True)
    with open(FORK_IDS, "a") as f:
        f.write(fork_sid + "\n")


class Stopped(Exception):
    """The unit is being stopped (cap reached, operator stop, session closed)."""


_FORK = {"proc": None}


def _on_signal(signum, _frame):
    # One stop is enough: what follows is the disposal, inside the unit's stop window.
    for sig in (signal.SIGTERM, signal.SIGHUP, signal.SIGINT):
        signal.signal(sig, signal.SIG_IGN)
    raise Stopped("worker stopped by signal %d" % signum)


def _stop_fork():
    """Stop the fork's process group: terminate, then kill what is left of it."""
    proc = _FORK["proc"]
    if proc is None:
        return
    for sig, wait in ((signal.SIGTERM, 2), (signal.SIGKILL, 1)):
        try:
            os.killpg(proc.pid, sig)
        except OSError:
            break  # the group is gone
        try:
            proc.wait(timeout=wait)
        except subprocess.TimeoutExpired:
            pass
    _FORK["proc"] = None


def run_fork(session_id, fork_sid, model, cwd):
    env = {k: v for k, v in os.environ.items() if k not in STRIP_ENV}
    env["MEMOIRE_ATTRIB_FORK"] = "1"
    cmd = [CLAUDE_BIN, "-p", "--resume", session_id, "--fork-session", "--session-id", fork_sid]
    if model:
        cmd += ["--model", model]
    cmd += ["--tools", "", "--output-format", "json", QUESTION]
    t0 = time.monotonic()
    try:
        try:
            # Its own process group, so a stop reaches what the fork started too. The
            # unit's control group remains the boundary for anything that leaves the group.
            proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=env,
                                    stdin=subprocess.DEVNULL, start_new_session=True,
                                    cwd=cwd if cwd and os.path.isdir(cwd) else None)
        except OSError as e:  # missing binary, ...
            return None, {"fork_error": repr(e)[:300], "secs": round(time.monotonic() - t0, 1)}
        _FORK["proc"] = proc
        stdout, stderr = proc.communicate()
        _FORK["proc"] = None
    except Stopped as e:
        _stop_fork()
        return None, {"fork_error": "%s, fork stopped" % e, "secs": round(time.monotonic() - t0, 1)}
    secs = round(time.monotonic() - t0, 1)
    try:
        out = json.loads(stdout) if stdout.strip() else {}
    except ValueError:
        out = {}
    if not isinstance(out, dict):
        out = {}
    meta = {"fork_exit": proc.returncode, "fork_stderr": stderr[-400:], "secs": secs,
            "fork_session_id": out.get("session_id"), "fork_cost_usd": out.get("total_cost_usd"),
            "fork_duration_ms": out.get("duration_ms"), "fork_is_error": out.get("is_error")}
    if proc.returncode != 0 or out.get("is_error") or not isinstance(out.get("result"), str):
        meta["fork_error"] = "exit %s, is_error=%s, result=%s" % (proc.returncode, out.get("is_error"), type(out.get("result")).__name__)
        return None, meta
    return out, meta


def dispose_fork_transcript(transcript_path, fork_sids):
    """Move (default) or delete every transcript the fork may have written: the id chosen
    here and, if the CLI reported another one, that one too. Called on success and failure."""
    results = []
    for fork_sid in fork_sids:
        if not fork_sid:
            continue
        src = os.path.join(os.path.dirname(transcript_path or ""), safe_name(fork_sid) + ".jsonl")
        if os.path.abspath(src) == os.path.abspath(transcript_path or ""):
            continue  # never the parent
        if not os.path.isfile(src):
            continue
        try:
            if FORK_DISPOSITION == "delete":
                os.remove(src)
                results.append("deleted")
            else:
                os.makedirs(FORKS_DIR, exist_ok=True)
                shutil.move(src, os.path.join(FORKS_DIR, os.path.basename(src)))
                results.append("moved")
        except OSError as e:
            results.append("error: " + repr(e)[:200])
    return ",".join(results) if results else "absent"


def sweep_stale_forks(transcript_path):
    """A worker killed outright never reaches its disposal. Each run disposes of
    registered fork transcripts left in this project's tree, once they are older than
    any fork still running can be."""
    try:
        with open(FORK_IDS) as f:
            ids = [l.strip() for l in f if l.strip()][-200:]
    except OSError:
        return 0
    base = os.path.dirname(transcript_path or "")
    stale, now, cap = [], time.time(), runtime_cap()
    for fid in ids:
        path = os.path.join(base, safe_name(fid) + ".jsonl")
        try:
            if now - os.path.getmtime(path) > cap + 60:
                stale.append(fid)
        except OSError:
            continue
    if stale:
        dispose_fork_transcript(transcript_path, stale)
    return len(stale)


def write_event(session_id, cwd, payload, text, source_event_id):
    fd, path = tempfile.mkstemp(prefix="memoire-attrib-event.", suffix=".json")
    with os.fdopen(fd, "w") as f:
        json.dump(payload, f, ensure_ascii=False)
    args = [QUEUE_BIN, "-project", os.environ.get("MEMOIRE_PROJECT") or os.path.basename(cwd or "") or "unknown",
            "-session-id", session_id, "-source-adapter", "claude-code", "-event-type", "memory_usage",
            "-cwd", cwd or "", "-payload-file", path, "-text", text, "-source-event-id", source_event_id]
    root, source = os.environ.get("MEMOIRE_PROJECT_ROOT"), os.environ.get("MEMOIRE_PROJECT_SOURCE")
    if root:
        args += ["-project-root", root, "-project-source", source or ""]
    try:
        r = subprocess.run(args, capture_output=True, text=True, timeout=30)
        return r.returncode, (r.stderr or r.stdout)[-300:]
    except Exception as e:
        return 1, repr(e)[:300]
    finally:
        try:
            os.remove(path)
        except OSError:
            pass


def main():
    if len(sys.argv) < 2:
        return 0
    payload_path = sys.argv[1]
    try:
        with open(payload_path) as f:
            d = json.load(f)
    except (OSError, ValueError) as e:
        # e.g. the manager refused the start to the entry script, which removed it
        print("%s memoire-attribute-claude worker: no usable payload (%s), nothing done" % (
            time.strftime("%Y-%m-%dT%H:%M:%S%z"), type(e).__name__), file=sys.stderr)
        return 0
    finally:
        try:
            os.remove(payload_path)
        except OSError:
            pass
    sid = d.get("session_id") or ""
    tp = d.get("transcript_path") or ""
    cwd = d.get("cwd") or ""
    last = d.get("last_assistant_message") or ""
    if not sid or not tp:
        journal(sid, event="skip", reason="no session or transcript in payload")
        return 0
    entries = load_entries(tp)
    if entries is None:
        journal(sid, event="skip", reason="transcript unreadable", transcript=tp)
        return 0
    swept = sweep_stale_forks(tp)
    if swept:
        journal(sid, event="sweep", stale_forks=swept)
    start = turn_start(entries)
    turn_uuid = (entries[start].get("uuid") if entries else None) or ("index-%d" % start)
    uses, calls = memoire_calls(entries, start)
    if not calls:
        journal(sid, event="skip", reason="no memoire call", turn_start=start, turn_start_uuid=turn_uuid, entries=len(entries))
        return 0
    if not claim_turn(sid, turn_uuid):
        journal(sid, event="skip", reason="already asked this turn", turn_start=start, turn_start_uuid=turn_uuid, entries=len(entries))
        return 0
    served = served_ids(entries, start, uses)
    model = DEFAULT_MODEL or session_model(entries)
    fork_sid = str(uuid.uuid4())
    register_fork_id(fork_sid)
    journal(sid, event="ask", hook_version=HOOK_VERSION, mode=MODE, model=model, calls=calls, served=served,
            fork_session_id=fork_sid, pid=os.getpid(), unit=UNIT,
            turn_start=start, turn_start_uuid=turn_uuid, entries=len(entries), reply_chars=len(last))
    out, meta = run_fork(sid, fork_sid, model, cwd)
    reported = meta.get("fork_session_id")
    if reported and reported != fork_sid:
        register_fork_id(str(reported))
    disposition = dispose_fork_transcript(tp, [fork_sid, reported if reported != fork_sid else None])
    if out is None:
        journal(sid, event="record", turn_start_uuid=turn_uuid, attribution="", fork_transcript=disposition, **meta)
        return 0
    attribution = out.get("result") or ""
    parsed = parse_attribution(attribution, set(served))
    cited = sorted({int(x) for x in ID_RE.findall(last)})
    counts = {v: 0 for v in VERDICTS}
    for v in parsed["verdicts"]:
        if v["verdict"] in counts and v.get("served"):
            counts[v["verdict"]] += 1
    payload = {
        "hook_version": HOOK_VERSION, "mode": MODE, "turn_start_uuid": turn_uuid, "turn_start_index": start,
        "calls": calls, "served_ids": [int(x) for x in served], "cited_ids": cited,
        "verdicts": parsed["verdicts"], "memory_only": parsed["memory_only"], "unverified": parsed["unverified"],
        "parse_ok": parsed["parse_ok"], "ids_subset_served": parsed["ids_subset_served"],
        "unserved_ids": parsed["unserved_ids"], "attribution_text": attribution,
        "fork": {"session_id": fork_sid, "reported_session_id": reported, "model": model, "cost_usd": meta.get("fork_cost_usd"),
                 "duration_ms": meta.get("fork_duration_ms"), "is_error": meta.get("fork_is_error"),
                 "secs": meta.get("secs"), "transcript": disposition},
    }
    text = "Memoire attribution at Stop: used=%d unused=%d misapplied=%d contradicted=%d fetch_failed=%d served=%d unserved=%d parse_ok=%s" % (
        counts["used"], counts["unused"], counts["misapplied"], counts["contradicted-by-files"], counts["fetch-failed"],
        len(served), len(parsed["unserved_ids"]), "true" if parsed["parse_ok"] else "false")
    source_event_id = "claude-memory-usage-" + hashlib.sha256(("%s:%s" % (sid, turn_uuid)).encode()).hexdigest()
    rc, msg = write_event(sid, cwd, payload, text, source_event_id)
    journal(sid, event="record", turn_start_uuid=turn_uuid, attr_chars=len(attribution), parse_ok=parsed["parse_ok"], ids_subset_served=parsed["ids_subset_served"],
            counts=counts, cited=cited, fork_transcript=disposition, queue_exit=rc, queue_msg=msg if rc else "", **meta)
    return 0


def run():
    """The unit's exit status: 0 when the worker ran to a journaled end, 1 when it
    broke. Neither reaches the harness: the entry script decides the hook's status."""
    for sig in (signal.SIGTERM, signal.SIGHUP, signal.SIGINT):
        signal.signal(sig, _on_signal)
    try:
        return main()
    except Stopped as e:
        _stop_fork()
        print("%s memoire-attribute-claude worker: %s, outside the fork" % (
            time.strftime("%Y-%m-%dT%H:%M:%S%z"), e), file=sys.stderr)
        return 0
    except Exception:
        _stop_fork()
        print("%s memoire-attribute-claude worker: failed" % time.strftime("%Y-%m-%dT%H:%M:%S%z"), file=sys.stderr)
        traceback.print_exc()
        return 1


if __name__ == "__main__":
    sys.exit(run())
