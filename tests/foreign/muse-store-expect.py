#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Assertions for muse-store-capture.sh over the adapter's queue lines.

Usage: muse-store-expect.py <check> <events.jsonl> [args...]
  common                     every line well-formed (fields, adapter, ids, times)
  records STORE [SID...]     every line of every session file of the given root
                             sessions (default: all) is exactly one event whose
                             payload carries it verbatim; nothing else but
                             projections and facts for those sessions
  typed                      event_type and text follow the mapping table
  gaps SID N                 SID has exactly N muse_gap events, N equals the
                             newest facts diagnostics.gaps
  turns SID RUN TEXT PROMPT  exactly one muse_assistant_turn for (SID, RUN) with
                             that text and prompt ("-" for any, RUN included)
  noturn SID [RUN]           no muse_assistant_turn for SID (or for that run)
  facts SID KEY=JSON...      the newest muse_session of SID has those fact values
  factscount SID N           SID has N distinct muse_session events
  link SID PARENT SUBAGENT ROLE INTERNAL   every event of SID carries that link
                             ("null" for null; INTERNAL true|false)
  absent SID                 no event mentions SID
  present SID                at least one event has session_id SID
  count N                    exactly N lines
  ids-subset A B             every source_event_id of file B is in file A, none of B in A twice
  disjoint A B               no source_event_id appears in both files
  text SID EVENT_TYPE TEXT   one event of that type for SID has exactly that text
  cwd CWD                    every event has that cwd
  project NAME               every event has that project
Exit 0 when the check holds; otherwise print the reasons and exit 1.
"""
import hashlib
import json
import os
import re
import sys

PROJECTION_TYPES = {"muse_assistant_turn", "muse_session"}
ISO = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$")
FIELDS = ("project", "session_id", "source_adapter", "event_type", "source_event_id",
          "occurred_at", "cwd", "payload")
LINK_FIELDS = ("root_session_id", "parent_session_id", "subagent_id", "subagent_role", "muse_internal")
errors = []


def err(msg):
    errors.append(msg)


def load(path):
    out = []
    with open(path, encoding="utf-8") as fh:
        for n, line in enumerate(fh, 1):
            if not line.strip():
                continue
            try:
                out.append(json.loads(line))
            except ValueError as e:
                err(f"line {n} is not JSON: {e}")
    return out


def session_files(store, roots):
    """Yield (sid, path) for each root session (and its children) under store."""
    for dirpath, _, files in os.walk(store):
        if "session.jsonl" not in files:
            continue
        sid = os.path.basename(dirpath)
        parent = os.path.basename(os.path.dirname(dirpath))
        root = os.path.basename(os.path.dirname(os.path.dirname(dirpath))) if parent == "subagent" else sid
        if roots and root not in roots:
            continue
        yield sid, os.path.join(dirpath, "session.jsonl")


def ev_kind(env):
    p = env.get("payload") if isinstance(env, dict) else None
    ev = p.get("event") if isinstance(p, dict) else None
    return (env.get("payload_type"), ev.get("kind") if isinstance(ev, dict) else None,
            p.get("kind") if isinstance(p, dict) else None, ev if isinstance(ev, dict) else {})


def expected_type(e):
    pl = e["payload"]
    if pl.get("export_kind") == "retained_frame":
        return "muse_retained_frame", None
    if pl.get("export_kind") == "unparseable_line":
        return "muse_unparseable_line", pl.get("raw_text")
    if pl.get("export_kind") == "gap":
        return "muse_gap", None
    env = pl.get("envelope")
    if not isinstance(env, dict):
        return "muse_record", None
    pt, k, pk, ev = ev_kind(env)
    if pt == "runtime.session" and k == "started" and pk == "run" and ev.get("prompt"):
        return "muse_user_prompt", ev["prompt"]
    if k == "assistant_message_committed":
        return "muse_assistant_message", ev.get("text")
    if k == "reasoning_committed":
        return "muse_reasoning", ev.get("text") or None
    if k == "assistant_tool_calls_committed":
        return "muse_tool_calls", "\n".join(f"{c.get('name')} {c.get('args')}" for c in ev.get("tool_calls", []))
    if k == "tool_result_batch_committed":
        return "muse_tool_results", "\n\n".join(r.get("text", "") for r in ev.get("results", []))
    if k == "model_completed":
        return "muse_model_completed", None
    if k == "terminal":
        return "muse_turn_terminal", None
    if pt == "session.end":
        return "muse_session_end", None
    if isinstance(pt, str) and pt.startswith("subagent.control."):
        return "muse_subagent_control", None
    return "muse_record", None


def check_common(evs):
    seen = {}
    for i, e in enumerate(evs):
        for f in FIELDS:
            if f not in e:
                err(f"event {i} lacks {f}")
        if e.get("source_adapter") != "muse-store":
            err(f"event {i} source_adapter={e.get('source_adapter')!r}")
        if not ISO.match(str(e.get("occurred_at", ""))):
            err(f"event {i} occurred_at={e.get('occurred_at')!r}")
        pl = e.get("payload")
        if not isinstance(pl, dict) or pl.get("schema") != "muse-export-v1":
            err(f"event {i} payload.schema missing or wrong")
            continue
        for f in LINK_FIELDS:
            if f not in pl:
                err(f"event {i} payload lacks {f}")
        sid = e.get("session_id", "")
        eid = e.get("source_event_id", "")
        if not re.match(rf"^muse:{re.escape(sid)}:(r|frame|bad|turn|session|gap):[^:]+$", eid):
            err(f"event {i} source_event_id {eid!r} does not match session {sid}")
        if eid in seen:
            err(f"duplicate source_event_id {eid}")
        seen[eid] = i
        if e.get("event_type") not in PROJECTION_TYPES and "text" in e and e["text"] in ("", None):
            err(f"event {i} carries an empty text field")


def check_records(evs, store, roots):
    by_id = {e["source_event_id"]: e for e in evs}
    wanted = set()
    for sid, path in session_files(store, roots):
        # A gap carries no id: it is named by its position, muse:<sid>:gap:<k>@<anchor>,
        # anchor = id of the nearest preceding record line (a duplicate record
        # line included), "start" before any record; k counts the gaps seen so
        # far after that anchor id anywhere in the file, so a re-appearing
        # anchor continues its count instead of restarting it.
        anchor, counts = "start", {}
        with open(path, encoding="utf-8") as fh:
            for n, line in enumerate(fh, 1):
                raw = line.rstrip("\n")
                try:
                    rec = json.loads(raw)
                except ValueError:
                    eid = f"muse:{sid}:bad:{hashlib.sha256(raw.encode()).hexdigest()[:16]}"
                    wanted.add(eid)
                    e = by_id.get(eid)
                    if not e:
                        err(f"{sid} line {n}: unparseable line not captured as {eid}")
                    elif e["payload"].get("raw_text") != raw or e.get("text") != raw:
                        err(f"{sid} line {n}: unparseable line not carried verbatim")
                    continue
                if "retained_marker" in rec:
                    counts[anchor] = counts.get(anchor, 0) + 1
                    eid = f"muse:{sid}:gap:{counts[anchor]}@{anchor}"
                    wanted.add(eid)
                    e = by_id.get(eid)
                    if not e:
                        err(f"{sid} line {n}: gap not captured as {eid}")
                        continue
                    pl = e["payload"]
                    g = {"marker": rec.get("retained_marker"), "omitted_record": rec.get("omitted_record"),
                         "stream": rec.get("stream")}
                    if pl.get("export_kind") != "gap" or any(pl.get(f) != g.get(f) for f in
                                                              ("marker", "omitted_record", "stream")):
                        err(f"{sid} line {n}: gap payload does not carry marker/omitted_record/stream verbatim")
                    if e.get("session_id") != sid:
                        err(f"{sid} line {n}: session_id={e.get('session_id')}")
                    continue
                if "retained_frame" in rec:
                    eid = f"muse:{sid}:frame:{rec.get('outer_log_ordinal')}"
                else:
                    eid = f"muse:{sid}:r:{rec.get('id')}"
                    if rec.get("id") is not None:
                        anchor = rec.get("id")
                wanted.add(eid)
                e = by_id.get(eid)
                if not e:
                    err(f"{sid} line {n}: not captured ({eid})")
                    continue
                if e["payload"].get("envelope") != rec:
                    err(f"{sid} line {n}: payload.envelope differs from the logged record")
                if e.get("session_id") != sid:
                    err(f"{sid} line {n}: session_id={e.get('session_id')}")
                if "recorded_at" in rec:
                    import datetime
                    want = datetime.datetime.fromtimestamp(rec["recorded_at"] / 1e6, datetime.timezone.utc)
                    want = want.strftime("%Y-%m-%dT%H:%M:%S.") + f"{want.microsecond // 1000:03d}Z"
                    if e.get("occurred_at") != want:
                        err(f"{sid} line {n}: occurred_at {e.get('occurred_at')} != {want}")
    sids = {sid for sid, _ in session_files(store, roots)}
    for e in evs:
        if e.get("session_id") in sids and e.get("event_type") not in PROJECTION_TYPES \
                and e["source_event_id"] not in wanted:
            err(f"unexpected event {e['source_event_id']} ({e.get('event_type')})")


def check_typed(evs):
    for e in evs:
        if e.get("event_type") in PROJECTION_TYPES:
            continue
        t, text = expected_type(e)
        if e.get("event_type") != t:
            err(f"{e['source_event_id']}: event_type {e.get('event_type')} != {t}")
        if text is None:
            if "text" in e:
                err(f"{e['source_event_id']}: unexpected text")
        elif e.get("text") != text:
            err(f"{e['source_event_id']}: text differs (len {len(e.get('text') or '')} vs {len(text)})")


def newest_facts(evs, sid):
    facts = [e for e in evs if e.get("event_type") == "muse_session" and e.get("session_id") == sid]
    return facts


def main():
    if len(sys.argv) < 3:
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    check, path, args = sys.argv[1], sys.argv[2], sys.argv[3:]
    evs = load(path) if check not in ("ids-subset", "disjoint") else []
    if check == "common":
        check_common(evs)
    elif check == "records":
        check_records(evs, args[0], set(args[1:]))
    elif check == "typed":
        check_typed(evs)
    elif check == "turns":
        sid, run, text, prompt = args
        t = [e for e in evs if e.get("event_type") == "muse_assistant_turn" and e.get("session_id") == sid
             and (run == "-" or e["payload"].get("run_id") == run)]
        if len(t) != 1:
            err(f"{len(t)} turn events for {sid} run {run}")
        else:
            e = t[0]
            if e["source_event_id"] != f"muse:{sid}:turn:{e['payload'].get('run_id')}":
                err(f"turn id {e['source_event_id']}")
            if text != "-" and e.get("text") != text:
                err(f"turn text {e.get('text')!r} != {text!r}")
            if prompt != "-" and e["payload"].get("prompt") != prompt:
                err(f"turn prompt {e['payload'].get('prompt')!r} != {prompt!r}")
    elif check == "noturn":
        t = [e for e in evs if e.get("event_type") == "muse_assistant_turn" and e.get("session_id") == args[0]
             and (len(args) < 2 or e["payload"].get("run_id") == args[1])]
        if t:
            err(f"{len(t)} unexpected turn events for {args[0]}")
    elif check in ("facts", "factscount"):
        f = newest_facts(evs, args[0])
        if check == "factscount":
            if len({e["source_event_id"] for e in f}) != int(args[1]):
                err(f"{len(f)} muse_session events for {args[0]}, want {args[1]}")
        elif not f:
            err(f"no muse_session for {args[0]}")
        else:
            facts = f[-1]["payload"]
            if not re.match(rf"^muse:{re.escape(args[0])}:session:[0-9a-f]{{16}}$", f[-1]["source_event_id"]):
                err(f"facts id {f[-1]['source_event_id']}")
            for kv in args[1:]:
                k, v = kv.split("=", 1)
                got = facts.get(k, "<absent>")
                want = json.loads(v)
                if isinstance(want, dict) and isinstance(got, dict):
                    for wk, wv in want.items():
                        if got.get(wk) != wv:
                            err(f"facts {k}.{wk}={got.get(wk)!r}, want {wv!r}")
                elif got != want:
                    err(f"facts {k}={got!r}, want {want!r}")
    elif check == "link":
        sid, parent, sub, role, internal = args
        conv = lambda s: None if s == "null" else s
        mine = [e for e in evs if e.get("session_id") == sid]
        if not mine:
            err(f"no events for {sid}")
        for e in mine:
            pl = e["payload"]
            for k, want in (("parent_session_id", conv(parent)), ("subagent_id", conv(sub)),
                            ("subagent_role", conv(role)), ("muse_internal", internal == "true")):
                if pl.get(k) != want:
                    err(f"{e['source_event_id']}: {k}={pl.get(k)!r}, want {want!r}")
                    break
    elif check == "gaps":
        sid, n = args[0], int(args[1])
        got = [e for e in evs if e.get("event_type") == "muse_gap" and e.get("session_id") == sid]
        if len(got) != n:
            err(f"{sid}: {len(got)} muse_gap events, want {n}")
        facts = newest_facts(evs, sid)
        diag = (facts[-1].get("payload") or {}).get("diagnostics") if facts else None
        if not isinstance(diag, dict) or diag.get("gaps") != n:
            err(f"{sid}: newest facts diagnostics.gaps={None if not isinstance(diag, dict) else diag.get('gaps')}, want {n}")
    elif check == "absent":
        for e in evs:
            if args[0] in json.dumps(e):
                err(f"{e.get('source_event_id')} mentions {args[0]}")
                break
    elif check == "present":
        if not any(e.get("session_id") == args[0] for e in evs):
            err(f"no event for {args[0]}")
    elif check == "count":
        if len(evs) != int(args[0]):
            err(f"{len(evs)} lines, want {args[0]}")
    elif check in ("ids-subset", "disjoint"):
        a = [e["source_event_id"] for e in load(path)]
        b = [e["source_event_id"] for e in load(args[0])]
        if check == "ids-subset":
            missing = set(b) - set(a)
            if missing:
                err(f"{len(missing)} ids of {args[0]} not in {path}: {sorted(missing)[:3]}")
        else:
            both = set(a) & set(b)
            if both:
                err(f"{len(both)} ids in both: {sorted(both)[:3]}")
    elif check == "text":
        sid, t, text = args
        if not any(e.get("session_id") == sid and e.get("event_type") == t and e.get("text") == text for e in evs):
            err(f"no {t} for {sid} with that text")
    elif check == "cwd":
        for e in evs:
            if e.get("cwd") != args[0]:
                err(f"{e['source_event_id']}: cwd {e.get('cwd')!r}")
                break
    elif check == "project":
        for e in evs:
            if e.get("project") != args[0]:
                err(f"{e['source_event_id']}: project {e.get('project')!r}")
                break
    else:
        print(f"unknown check {check}", file=sys.stderr)
        sys.exit(2)
    if errors:
        for m in errors[:20]:
            print(m)
        if len(errors) > 20:
            print(f"... {len(errors) - 20} more")
        sys.exit(1)


if __name__ == "__main__":
    main()
