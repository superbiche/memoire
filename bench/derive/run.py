#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Run driver for the derive bench (docs/DERIVE-BENCH-DESIGN.md §2).

One invocation = one candidate over every session of a frozen eval set. Per
session it builds a throwaway standalone-SQLite home (memoire:D0247 posture),
seeds it from the fixture through the real ingest surface, derives through the
real seam against a named endpoint-registry entry (memoire:D0075/D0076), and
writes the proposed observation set plus run metadata to
tmp/derive-bench/<date>/<candidate>/<session>.json.

Derivation is WINDOWED exactly as production windows it: a session is not one
prompt. `scripts/memoire-derive-sweep.sh` cuts each session at its own RHYTHM
first — a long pause, or N canonical events since the last cut, both read from
the binary's `-cut-points` helper (memoire:D0261, Fibery #343) — and halves a
segment on its median canonical epoch only when that segment still exceeds the
byte ceiling. Halving is the FALLBACK, never the first splitter.
`derive_session()` and `derive_window()` below are that port; a session
therefore yields an ordered list of LEGS, and the result's observations are the
aggregate across them.

The trailing segment ends at the last event so far, so it IS the open tail: its
leg carries `-provisional`, which marks the run and its observations as "so
far" statements a later cut may supersede (memoire:D0260/D0262, Fibery #344). A
session with no cut is one open tail, fail-open paths included, and halving an
open tail keeps the marker on the RIGHT leg only.

Knobs: `--no-rhythm-cuts` restores the pre-#352 halving-only lane, and
`--cut-gap-seconds` / `--cut-max-events` / `--cut-max-segments` override the
detector defaults. An unset knob is NOT passed, so the binary governs and the
numbers stay in one place. Whole-session single-shot was retired 2026-08-24 as
unrepresentative: it sent 45k-368k-token prompts that production never sends,
and failed outright on 6 of 11 reference sessions.

Failure is data (memoire:D0048): a refused, failed, partial, or skipped run is
recorded with its genre and moves on. Nothing is retried.

Usage:
  run.py --candidate qwen35-9b --endpoint-id local-gpu --model qwen3.5-9b \
         --set my-eval

Environment:
  MEMOIRE_MODEL_ENDPOINTS  endpoint registry (default $MEMOIRE_HOME/model-endpoints.json)
  <endpoint api_key_env>   the endpoint's bearer, read by NAME and forwarded
                           into the derive process; never printed, never stored
"""

import argparse
import datetime
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import benchlib  # noqa: E402

BENCH_DIR = os.path.dirname(os.path.abspath(__file__))
REPO_ROOT = os.path.dirname(os.path.dirname(BENCH_DIR))
FIRST_PORT = 37881
PORT_TRIES = 20
# Seed drain-wait stall ceiling: fail only when the drained count stops
# MOVING for this long (progress-based; see seed_session).
DRAIN_STALL_S = 120.0
# The sweep's window ceiling, the last resort when neither the flag nor the
# endpoint's profile names one. Pinned to
# scripts/memoire-derive-sweep.sh's MEMOIRE_SWEEP_MAX_PROMPT_BYTES by
# tests/derive-bench-test.sh: the incumbent's observations came through it.
DEFAULT_MAX_PROMPT_BYTES = 70000


def parse_args(argv):
    parser = argparse.ArgumentParser(
        prog="run.py", description="Run one derive-bench candidate over an eval set."
    )
    parser.add_argument("--candidate", required=True,
                        help="label for this candidate, e.g. qwen35-9b")
    parser.add_argument("--endpoint-id", required=True,
                        help="endpoint id from the model endpoint registry")
    parser.add_argument("--model", required=True,
                        help="model name, allowlisted for that endpoint")
    parser.add_argument("--set", dest="set_name", default="default",
                        help="frozen eval set to run")
    parser.add_argument("--session", dest="sessions", action="append", default=[],
                        help="restrict the run to these sessions; repeatable")
    parser.add_argument("--data-dir", default=os.path.join(BENCH_DIR, "data"),
                        help="fixture + throwaway-home root (gitignored)")
    parser.add_argument("--out",
                        help="results dir (default tmp/derive-bench/<UTC date>)")
    parser.add_argument("--endpoints", default=os.environ.get("MEMOIRE_MODEL_ENDPOINTS", ""),
                        help="endpoint registry path")
    parser.add_argument("--execute-timeout", default="10m",
                        help="ceiling for one provider call (memoire-derive -execute-timeout)")
    parser.add_argument("--seed-stagger-ms", type=float, default=2.0,
                        help="delay between seeded events; see seed_session()")
    # Both defaults are PRODUCTION CONSTANTS, mirroring
    # scripts/memoire-derive-sweep.sh's MEMOIRE_SWEEP_MAX_PROMPT_BYTES (70000)
    # and MEMOIRE_SWEEP_MAX_SPLIT_DEPTH (6). The sweep exposes both as env
    # overrides, so exposing them as flags here is parity, not a bench knob —
    # and derive-bench-test.sh grep-asserts them against the script.
    #
    # The ceiling defaults to None so the endpoint's profile can supply it:
    # explicit flag > profile > DEFAULT_MAX_PROMPT_BYTES.
    parser.add_argument("--max-prompt-bytes", type=int, default=None,
                        help="window ceiling; over it the window is halved on the "
                             "median canonical epoch (default: the candidate "
                             f"endpoint's profile, else {DEFAULT_MAX_PROMPT_BYTES})")
    parser.add_argument("--split-depth", type=int, default=6,
                        help="recursion cap for the window split (production default 6)")
    # Rhythm cuts (memoire:D0261, Fibery #343). --no-rhythm-cuts mirrors the
    # sweep's MEMOIRE_SWEEP_RHYTHM_CUTS=0; the three thresholds mirror its
    # MEMOIRE_SWEEP_CUT_GAP_SECONDS / _CUT_MAX_EVENTS / _CUT_MAX_SEGMENTS. They
    # default to None, which is the sweep's own discipline: an unset knob is not
    # passed, so the binary's default governs and the numbers live in ONE place
    # (scripts/memoire-derive-sweep.sh:44-47 and :125-132). That is also why 1c
    # pins no rhythm constant — there is none to drift.
    parser.add_argument("--no-rhythm-cuts", action="store_true",
                        help="skip the rhythm pass and window by halving alone "
                             "(the pre-2026-09-08 lane)")
    parser.add_argument("--cut-gap-seconds", type=int, default=None,
                        help="long-pause cut threshold in seconds "
                             "(default: the binary's)")
    parser.add_argument("--cut-max-events", type=int, default=None,
                        help="cut after this many canonical events since the "
                             "last cut (default: the binary's)")
    parser.add_argument("--cut-max-segments", type=int, default=None,
                        help="maximum rhythm segments per pass, 0 = unbounded "
                             "(default: the binary's)")
    parser.add_argument("--no-min-content-guard", action="store_true",
                        help="derive even a degenerate (near-empty) window — the "
                             "explicit opt-out a thin abstention-probe session needs "
                             "to reach the model at all")
    parser.add_argument("--keep-home", action="store_true",
                        help="keep each throwaway home for post-mortem")
    return parser.parse_args(argv)


def resolve_endpoints_path(explicit):
    if explicit:
        return explicit
    home = os.environ.get("MEMOIRE_HOME") or os.path.join(
        os.path.expanduser("~"), ".memoire"
    )
    return os.path.join(home, "model-endpoints.json")


def load_endpoint(path, endpoint_id, model):
    """Find the candidate's registry entry and its secret's env-var NAME.

    memoire:D0055/D0058: the bearer is resolved by NAME. This driver reads the
    name from the registry and forwards the value into the derive process's
    environment; the value is never printed, logged, or written to a result.
    memoire:D0077: a configured-but-unset secret is a pre-flight operator
    error, not a recorded failed run.
    """
    try:
        registry = benchlib.read_json(path)
    except OSError as err:
        raise benchlib.BenchError(f"cannot read endpoint registry {path}: {err.strerror}")
    entries = registry.get("endpoints") if isinstance(registry, dict) else registry
    for entry in entries or []:
        if entry.get("endpoint_id") == endpoint_id:
            models = entry.get("models") or []
            if model not in models:
                raise benchlib.BenchError(
                    f"model {model!r} is not allowlisted for endpoint "
                    f"{endpoint_id!r} in {path}"
                )
            # An optional derive profile is validated here, on the same terms
            # memoire validates it: a candidate served under a geometry the
            # binary would refuse is not a candidate this bench can compare.
            if entry.get("profile") is not None:
                benchlib.validate_profile(endpoint_id, entry["profile"])
            return entry
    raise benchlib.BenchError(f"endpoint id {endpoint_id!r} is not in {path}")


def resolve_prompt_ceiling(args, entry):
    """Settle the window ceiling: explicit flag > endpoint profile > default.

    The resolved number is written back onto args, so every site that already
    reads args.max_prompt_bytes — the derive argv, the split test, the result
    record — inherits one resolution. args.thinking carries the endpoint's
    serving declaration into the result so a bench artifact states the
    configuration it ran under (docs/DERIVE-PROFILE-DESIGN.md).
    """
    profile = entry.get("profile")
    args.thinking = (profile or {}).get("thinking")
    if args.max_prompt_bytes is not None:
        args.limits_source = "flag"
        return
    if profile:
        args.max_prompt_bytes = benchlib.effective_max_prompt_bytes(profile)
        args.limits_source = "profile"
        return
    args.max_prompt_bytes = DEFAULT_MAX_PROMPT_BYTES
    args.limits_source = "default"


def endpoint_secret(entry):
    name = (entry.get("api_key_env") or "").strip()
    if not name:
        return None, None
    value = os.environ.get(name, "")
    if not value:
        raise benchlib.BenchError(
            f"endpoint {entry.get('endpoint_id')!r} declares api_key_env {name} "
            "but it is unset — export it before running the bench"
        )
    return name, value


def build_binaries(bin_dir):
    os.makedirs(bin_dir, exist_ok=True)
    built = {}
    for name, pkg in (("memoire-api", "./services/api/cmd/api"),
                      ("memoire-derive", "./services/api/cmd/derive")):
        target = os.path.join(bin_dir, name)
        proc = subprocess.run(
            ["go", "build", "-tags", "sqlite_fts5", "-o", target, pkg],
            cwd=REPO_ROOT, capture_output=True, text=True,
        )
        if proc.returncode != 0:
            raise benchlib.BenchError(f"go build {name} failed: {proc.stderr.strip()}")
        built[name] = target
    return built


def api_env(home, port):
    """The scrubbed environment for a throwaway standalone column.

    Only these variables are visible to the process. MEMOIRE_DB is deliberately
    absent: an ambient value takes precedence over MEMOIRE_HOME and would point
    the bench at the operator's real store (the memoire:D0162 review finding).
    """
    return {
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "HOME": home,
        "MEMOIRE_HOME": home,
        "MEMOIRE_ADDR": f"127.0.0.1:{port}",
        "MEMOIRE_SCHEMA_DIR": os.path.join(REPO_ROOT, "schema"),
    }


class ThrowawayColumn:
    """A standalone-SQLite memoire on a scratch home: API, store, drainer."""

    def __init__(self, api_bin, home, log_path):
        self.api_bin = api_bin
        self.home = home
        self.log_path = log_path
        self.proc = None
        self.port = None
        self.client = None

    def api(self):
        """The client of a STARTED column.

        `start()` either binds a client or raises, so a column that exists past
        it always has one. Stating that invariant here rather than leaving each
        call site to assume it means a use-before-start fails with this
        sentence instead of an AttributeError on None.
        """
        if self.client is None:
            raise benchlib.BenchError(
                "throwaway column used before start() — no API is listening on it"
            )
        return self.client

    def start(self):
        for port in range(FIRST_PORT, FIRST_PORT + PORT_TRIES):
            os.makedirs(self.home, exist_ok=True)
            with open(self.log_path, "wb") as log:
                proc = subprocess.Popen(
                    [self.api_bin], env=api_env(self.home, port),
                    stdout=log, stderr=subprocess.STDOUT,
                )
            client = self._await_ownership(proc, port)
            if client:
                self.proc, self.port, self.client = proc, port, client
                return self
            # Bind loss, boot death, or a foreign listener: reap and step on.
            proc.kill()
            proc.wait(timeout=10)
        raise benchlib.BenchError(
            f"no throwaway API became ready on ports {FIRST_PORT}-"
            f"{FIRST_PORT + PORT_TRIES - 1} (log: {self.log_path})"
        )

    def _await_ownership(self, proc, port):
        """A health answer can come from a foreign listener that won the bind.

        Ours must still be alive and accept the token IT minted into OUR home
        (the standalone smoke's ownership check).
        """
        base = f"http://127.0.0.1:{port}"
        token_path = os.path.join(self.home, "token")
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if proc.poll() is not None:
                return None
            try:
                token = benchlib.read_token(token_path)
                client = benchlib.Client(base, token)
                client.stats()
                return client
            except benchlib.BenchError:
                time.sleep(0.1)
        return None

    def stop(self):
        if self.proc and self.proc.poll() is None:
            self.proc.send_signal(signal.SIGTERM)
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=10)


def seed_session(column, fixture, stagger_ms):
    """Replay the fixture into the throwaway home through POST /v1/events/batch.

    One event per request, staggered. The queue writer names each pending file
    `<unix-millis>-<adapter>-<sha256>.json` and the drainer processes them in
    sorted-name order, so two events written inside the same millisecond can
    swap places — and event ids feed `derive.CanonicalInputHash` and the prompt
    order for events sharing an epoch second. A stagger wide enough to advance
    the millisecond clock makes id assignment a function of the fixture alone,
    which is what makes two candidates comparable.
    """
    events = benchlib.sort_events(fixture["events"])
    started = time.monotonic()
    for event in events:
        body = {"events": [benchlib.replay_envelope(event)]}
        result = column.api().post("/v1/events/batch", body)
        rejected = result.get("rejected") or []
        if rejected:
            raise benchlib.BenchError(
                f"ingest rejected event {event['id']}: {rejected[0].get('reason')}"
            )
        time.sleep(stagger_ms / 1000.0)
    seed_ms = int((time.monotonic() - started) * 1000)

    # Progress-based drain wait: a per-event time budget mistakes a LOADED
    # machine for a broken drainer (measured live: a bench-external CPU hog
    # slowed the drain ~10x and failed every large session at 0.5s/event
    # while the drainer was making steady progress). The failure signal is
    # therefore stalled COUNT, not elapsed time: as long as event_count
    # climbs the wait continues; two minutes with no movement is a dead
    # drainer, however idle or busy the host.
    want = len(events)
    seen = 0
    stall_deadline = time.monotonic() + DRAIN_STALL_S
    while seen < want:
        now = int(column.api().stats().get("event_count", 0))
        if now > seen:
            seen = now
            stall_deadline = time.monotonic() + DRAIN_STALL_S
        elif time.monotonic() > stall_deadline:
            raise benchlib.BenchError(
                f"drain stalled at {seen}/{want} replayed events "
                f"({DRAIN_STALL_S:.0f}s with no progress)"
            )
        else:
            time.sleep(0.5)
    drain_ms = int((time.monotonic() - started) * 1000) - seed_ms
    return {"seed_ms": seed_ms, "drain_ms": drain_ms, "events_seeded": want}


def verify_replay(column, fixture):
    """Prove the throwaway store holds the fixture, in the fixture's order.

    Reads the seeded events back through the same tool-window surface the
    freezer used and compares the (event_type, occurred_at_epoch) sequence,
    ordered the way derivation orders its input. A mismatch means the fixture
    did not survive the round trip — the run's comparison would be meaningless,
    so it is recorded as a failure genre rather than derived over.

    The throwaway ids are persisted as `event_ids`, in fixture order. They are
    the only bridge between a candidate observation's `source_event_ids` and
    the fixture ordinals a judge reads in the transcript: the ids are minted by
    whichever throwaway home this run used and mean nothing anywhere else, so
    a result that does not carry them cannot be packed.
    """
    session_id = fixture["session_id"]
    seen = []
    for event_type in fixture.get("event_types") or benchlib.CANONICAL_INPUT_EVENT_TYPES:
        for event_id in column.api().tool_window_ids(session_id, event_type):
            record = column.api().event(event_id)
            seen.append({
                "id": int(record["id"]),
                "event_type": record["event_type"],
                "occurred_at_epoch": int(record["occurred_at_epoch"]),
            })
    seen = benchlib.sort_events(seen)
    want = [(e["event_type"], int(e["occurred_at_epoch"]))
            for e in benchlib.sort_events(fixture["events"])]
    got = [(e["event_type"], e["occurred_at_epoch"]) for e in seen]
    event_ids = [e["id"] for e in seen]
    if want == got:
        return {"ok": True, "events": len(got), "event_ids": event_ids}
    first_diff = next(
        (i for i, (a, b) in enumerate(zip(want, got)) if a != b), min(len(want), len(got))
    )
    return {
        "ok": False,
        "events_expected": len(want),
        "events_seen": len(got),
        "first_divergence_index": first_diff,
        "event_ids": event_ids,
    }


def derive_env(home, endpoints_path, secret_name, secret_value):
    """The derive CLI's environment: the same scrub, plus the two knobs it needs.

    The registry stays the USER's file (the candidate endpoint is real), while
    MEMOIRE_HOME is the throwaway store — so the run reads the operator's
    endpoint config and writes nowhere near the operator's data.
    """
    env = api_env(home, 0)
    del env["MEMOIRE_ADDR"]  # the CLI opens the store directly; it serves nothing
    env["MEMOIRE_MODEL_ENDPOINTS"] = endpoints_path
    if secret_name:
        env[secret_name] = secret_value
    return env


def window_args(args, fixture, lo, hi):
    """The common window argv, mirroring the sweep's `derive_args`.

    Epochs are appended only when PINNED (> 0), so a whole-session window keeps
    its historical run identity rather than being silently re-scoped to
    resolved bounds. `-max-prompt-bytes` rides both the dry-run and the execute
    exactly as the sweep sends it: the prompt's advisory already-captured
    section yields to that headroom, so a window the dry-run sized as fitting
    cannot be pushed over the ceiling at execute time by the observations
    earlier legs just wrote.
    """
    argv = [
        "-provider=local-qwen",
        f"-model={args.model}",
        f"-endpoint-id={args.endpoint_id}",
        f"-project={fixture['project']}",
        f"-session-id={fixture['session_id']}",
        "-json",
        f"-max-prompt-bytes={args.max_prompt_bytes}",
    ]
    if lo > 0:
        argv.append(f"-start-epoch={lo}")
    if hi > 0:
        argv.append(f"-end-epoch={hi}")
    return argv


def run_cli(derive_bin, env, argv):
    started = time.monotonic()
    proc = subprocess.run([derive_bin] + argv, env=env, capture_output=True, text=True)
    elapsed_ms = int((time.monotonic() - started) * 1000)
    outcome = {}
    if proc.stdout.strip():
        try:
            outcome = json.loads(proc.stdout)
        except ValueError:
            outcome = {}
    return {
        "argv": argv,
        "exit_code": proc.returncode,
        "elapsed_ms": elapsed_ms,
        "outcome": outcome,
        "stdout_tail": proc.stdout.strip()[-2000:],
        "stderr_tail": proc.stderr.strip()[-2000:],
    }


def top_level_int(outcome, field):
    """The sweep's `json_int`, faithfully.

    The shell reads the CLI's MarshalIndent output with a two-space-indent sed,
    so it matches TOP-LEVEL integer fields only — nested `prompt_meta` or
    `usage` fields never match. Reading the parsed object's top level is the
    same contract. A missing, non-integer, or non-numeric value returns None,
    which collapses the shell's `[[ ! "$pb" =~ ^[0-9]+$ ]]` test — the shell
    reaches that branch for a nonzero exit, non-JSON stdout, and an absent
    field alike, and so does this.
    """
    value = outcome.get(field)
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    if isinstance(value, float) and value != int(value):
        return None
    # The shell's character class is [0-9]+ — unsigned. A negative value would
    # not match there and falls to the fail-open branch, so it must not be
    # accepted here either.
    if value < 0:
        return None
    return int(value)


def run_derive(derive_bin, env, args, fixture, lo=0, hi=0, provisional=False):
    argv = ["-strategy=model", "-execute",
            f"-execute-timeout={args.execute_timeout}"] + window_args(args, fixture, lo, hi)
    if args.no_min_content_guard:
        argv.append("-no-min-content-guard")
    if provisional:
        # The open tail's marker rides the EXECUTE argv only, exactly as the
        # sweep sends it (scripts/memoire-derive-sweep.sh:354-359): the dry-run
        # shares window_args, and a sizing probe has no run to mark. The binary
        # refuses -provisional without -execute (cmd/derive/main.go:473-474).
        argv.append("-provisional")
    result = run_cli(derive_bin, env, argv)
    result["derive_ms"] = result["elapsed_ms"]
    return result


def dry_run_window(derive_bin, env, args, fixture, lo, hi):
    """Size one window. No provider call, nothing persisted.

    `-no-min-content-guard` is deliberately NOT forwarded here, matching the
    sweep: the guard is constructed in `runModelExecute` only, so the dry-run
    sizes every window honestly whatever the guard setting, and passing the
    flag would imply an effect it does not have. A window the guard will skip
    is still measured, and the skip surfaces as that leg's genre at execute.
    """
    return run_cli(derive_bin, env,
                   ["-strategy=model", "-dry-run"] + window_args(args, fixture, lo, hi))


def median_epoch(derive_bin, env, fixture, lo, hi):
    """The sweep's `median_epoch`: the CLI owns the query, empty output is None.

    The binary takes its canonical type list from the prompt builder itself, so
    the split always sees exactly the events the builder admits. It opens the
    store directly — the throwaway home holds exactly the fixture's events, so
    every median, and therefore every split boundary, is a function of the
    fixture alone. Failure and an empty window are both empty output; the
    caller's unsplittable-window marker covers them.
    """
    proc = subprocess.run(
        [derive_bin, "-median-epoch",
         f"-project={fixture['project']}", f"-session-id={fixture['session_id']}",
         f"-start-epoch={lo}", f"-end-epoch={hi}"],
        env=env, capture_output=True, text=True,
    )
    # The shell tests the VALUE against ^[0-9]+$ and ignores the exit status,
    # so a median is trusted whenever one was printed. `str.isdigit()` is not
    # that test — it admits superscripts and other non-ASCII digits that int()
    # then rejects (the same Unicode trap as the near-dup port), so the regex
    # is spelled out rather than approximated.
    if not re.fullmatch(r"[0-9]+", proc.stdout.strip()):
        return None
    return int(proc.stdout.strip())


def printable(text):
    """The shell's `tr -c '[:print:]' ' '`, for a tail that lands in a result."""
    return "".join(c if 32 <= ord(c) < 127 else " " for c in text)


def cut_knob_args(args):
    """The rhythm knobs, passed only when SET.

    The sweep's discipline (scripts/memoire-derive-sweep.sh:44-47, :125-132):
    an unset knob is not passed, so the binary's default governs and the
    thresholds live in one place. `None` here is that unset state.
    """
    argv = []
    for flag, value in (("-cut-gap-seconds", args.cut_gap_seconds),
                        ("-cut-max-events", args.cut_max_events),
                        ("-cut-max-segments", args.cut_max_segments)):
        if value is not None:
            argv.append(f"{flag}={value}")
    return argv


def rhythm_block(args, status, cuts=(), enabled=True, detail=""):
    """The result's `rhythm` object: what the detector said, and under which knobs."""
    block = {
        "enabled": enabled,
        "status": status,
        "cuts": list(cuts),
        "segments": len(cuts) + 1,
        "knobs": {name.lstrip("-"): int(value) for name, value in
                  (kv.split("=", 1) for kv in cut_knob_args(args))},
    }
    if detail:
        block["detail"] = detail
    return block


def cut_points(ctx):
    """Port of the sweep's `cut_points` (:456-477) plus the ALL-OR-NOTHING parse
    its caller performs on the output (`derive_session`, :562-587).

    Returns the `rhythm` block the result records. Rhythm cuts are the FIRST
    splitter (memoire:D0261): the binary owns the detector, so the cuts land
    where the prompt builder's own population has a seam.

    Fail-open, but NOT silently. A detector failure would quietly revert the
    bench to pure time slicing — the very lane #352 exists to stop benching —
    so rc != 0 records `failed` with the rc and a printable tail, and one
    unusable line condemns the whole list as `malformed`. Empty output is the
    ordinary "this window needs no cut" answer, not a degradation.
    """
    args = ctx["args"]
    if args.no_rhythm_cuts:
        return rhythm_block(args, "disabled", enabled=False)
    fixture = ctx["fixture"]
    argv = [ctx["derive_bin"], "-cut-points",
            f"-project={fixture['project']}",
            f"-session-id={fixture['session_id']}"]
    if args.no_min_content_guard:
        # The detector's floor consults the SAME min-content guard the
        # orchestrator applies (cmd/derive/main.go:1286-1287, cutpoints.go
        # invariant 3). The sweep has no guard knob to forward; the bench does,
        # and it forwards it to execute — so withholding it here would let the
        # detector refuse a cut the bench's own execute posture accepts.
        argv.append("-no-min-content-guard")
    proc = subprocess.run(argv + cut_knob_args(args), env=ctx["env"], text=True,
                          stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if proc.returncode != 0:
        return rhythm_block(args, "failed",
                            detail=f"rc={proc.returncode} "
                                   + printable(proc.stdout)[-200:])
    # `$(...)` strips trailing newlines and the shell's `<<<` adds exactly one
    # back, so a trailing blank line is not a line. An INTERIOR blank one is.
    raw = proc.stdout.rstrip("\n")
    if not raw:
        return rhythm_block(args, "ok")
    cuts = []
    last = 0
    for line in raw.split("\n"):
        # Unusable = not a plausible epoch (1-19 digits, so the arithmetic
        # cannot overflow) or not strictly greater than the previous cut, which
        # also rejects a 0 the helper should never emit. `re.fullmatch` with an
        # explicit [0-9] class, never `str.isdigit()`: that admits superscripts
        # and other non-ASCII digits the shell's character class refuses — the
        # same Unicode trap `median_epoch` above documents.
        if not re.fullmatch(r"[0-9]{1,19}", line) or int(line) <= last:
            return rhythm_block(args, "malformed",
                                detail="line=" + printable(line)[-60:])
        last = int(line)
        cuts.append(last)
    return rhythm_block(args, "ok", cuts=cuts)


def derive_session(ctx, legs):
    """Port of `derive_session` in scripts/memoire-derive-sweep.sh (:554-611).

    Rhythm FIRST, halving as the fallback (memoire:D0261). Cuts c1..ck yield the
    segments [0, c1], [c1+1, c2], ..., [ck+1, 0], reusing the epoch-inclusive
    convention the halving split already applies. The OUTER edges stay UNPINNED
    on purpose (sweep comment :541-548): the CLI accepts a half-pinned window
    when -session-id is given, and the first and last legs keep the run identity
    the sweep would give them.

    Segments enter at depth 1: a rhythm cut IS a split, so it consumes one level
    of the halving budget rather than handing every segment a fresh one.
    Detection ran once for the whole window, so no segment holds a further cut
    point and the pass never recurses.
    """
    cuts = ctx["rhythm"]["cuts"]
    if not cuts:
        # The whole-session window ends at the last event so far, so it IS the
        # open tail (Fibery #344) — including on the fail-open and malformed
        # paths, which land here exactly as the shell lands them.
        derive_window(ctx, 0, 0, 0, legs, provisional=True, segment=0)
        return
    lo = 0
    for index, cut in enumerate(cuts):
        # An inner segment is CLOSED at a cut the detector already saw: its arc
        # ended, so it is never provisional.
        derive_window(ctx, lo, cut, 1, legs, provisional=False, segment=index)
        lo = cut + 1
    derive_window(ctx, lo, 0, 1, legs, provisional=True, segment=len(cuts))


def derive_window(ctx, lo, hi, depth, legs, provisional=False, segment=0):
    """Port of `derive_window` in scripts/memoire-derive-sweep.sh (:481-536).

    Size the window with a local dry-run; execute it if it fits, otherwise
    halve it on the median canonical-event epoch and recurse. Production's
    incumbent observations came through exactly this algorithm, so the bench
    must window the same way or it is not measuring the same chain.

    Order is load-bearing and matches the shell statement for statement:
    fit-check BEFORE the depth check, depth check BEFORE bounds resolution
    (a window that is still unpinned at the depth cap is skipped with its
    bounds unresolved, exactly as the shell leaves it), and the recursion is
    depth-first on the LEFT leg — so legs execute oldest window first.

    That ordering is part of the determinism contract, not an accident: the
    write-time near-dup gate compares each leg's candidates against the
    observations already in the throwaway store, so leg N's output gates
    leg N+1. Reordering the legs changes what survives.

    `provisional` is the open-tail marker this window inherits from its rhythm
    segment, and `segment` is that segment's 0-based index in rhythm order.
    Both ride every leg the walk produces, skipped ones included.
    """
    dry = dry_run_window(ctx["derive_bin"], ctx["env"], ctx["args"], ctx["fixture"], lo, hi)
    ctx["dry_run_ms"] += dry["elapsed_ms"]
    prompt_bytes = top_level_int(dry["outcome"], "prompt_bytes")

    if prompt_bytes is None:
        # Unexpected dry-run failure: fall back to a direct execute, which
        # records its own honest outcome. Fail-open on the sizing probe, never
        # silent — the shell's identical branch.
        execute_leg(ctx, lo, hi, depth, None, legs, provisional, segment)
        return
    if prompt_bytes <= ctx["args"].max_prompt_bytes:
        execute_leg(ctx, lo, hi, depth, prompt_bytes, legs, provisional, segment)
        return
    if depth >= ctx["args"].split_depth:
        legs.append(skipped_leg(lo, hi, depth, prompt_bytes, "split_depth_exceeded",
                                provisional, segment))
        return

    if lo == 0 or hi == 0:
        lo = top_level_int(dry["outcome"], "window_start_epoch")
        hi = top_level_int(dry["outcome"], "window_end_epoch")
        if lo is None or hi is None:
            legs.append(skipped_leg(0, 0, depth, prompt_bytes, "unsplittable_window",
                                    provisional, segment,
                                    reason="no resolved bounds"))
            return

    mid = median_epoch(ctx["derive_bin"], ctx["env"], ctx["fixture"], lo, hi)
    # Left leg is [lo, mid], right leg is [mid+1, hi]: keep the right leg
    # non-empty by pulling the split point below hi when the median sits on it.
    # Bounds are INCLUSIVE — an off-by-one here drops or double-derives events.
    if mid is not None and mid >= hi:
        mid = hi - 1
    if mid is None or mid < lo:
        legs.append(skipped_leg(lo, hi, depth, prompt_bytes, "unsplittable_window",
                                provisional, segment))
        return

    # Halving an open-tail window: only the RIGHT leg still ends at the tail, so
    # the marker travels with it. The left leg is a closed interval whose events
    # are all in the past, exactly like an inner rhythm segment (sweep :531-535).
    derive_window(ctx, lo, mid, depth + 1, legs, False, segment)
    derive_window(ctx, mid + 1, hi, depth + 1, legs, provisional, segment)


def skipped_leg(lo, hi, depth, prompt_bytes, genre, provisional=False, segment=0,
                reason=""):
    """A leg the port refuses itself — NO CLI call, so no provider call.

    These are the coverage gaps: the candidate structurally never saw these
    events. The window is recorded so the scorecard can say so at the
    aggregate; it is never surfaced in a pack, where only one side can have
    gaps and a marker would unblind.
    """
    leg = {
        "window_start_epoch": lo,
        "window_end_epoch": hi,
        "depth": depth,
        "provisional": provisional,
        "segment": segment,
        "prompt_bytes": prompt_bytes,
        "genre": genre,
        "derive_ms": 0,
        "run": {},
        "coverage_gap": True,
    }
    if reason:
        leg["reason"] = reason
    return leg


def execute_leg(ctx, lo, hi, depth, prompt_bytes, legs, provisional=False, segment=0):
    """One persisted derivation run for a window that fits (the sweep's run_execute)."""
    derived = run_derive(ctx["derive_bin"], ctx["env"], ctx["args"], ctx["fixture"],
                         lo, hi, provisional)
    leg = {
        "window_start_epoch": lo,
        "window_end_epoch": hi,
        "depth": depth,
        "provisional": provisional,
        "segment": segment,
        "prompt_bytes": prompt_bytes,
        "genre": classify_leg(derived),
        "derive_ms": derived["derive_ms"],
        "exit_code": derived["exit_code"],
        "run": {},
        "coverage_gap": False,
    }
    if derived["stderr_tail"]:
        leg["stderr_tail"] = derived["stderr_tail"]
    leg["outcome"] = leg_outcome(derived["outcome"])
    run_id = derived["outcome"].get("run_id", "")
    if run_id:
        row = ctx["column"].api().derivation_run(run_id)
        meta = row.get("metadata")
        meta = meta if isinstance(meta, dict) else {}
        diagnosis = leg_diagnosis(meta)
        if diagnosis:
            leg["diagnosis"] = diagnosis
        leg["run"] = {
            "run_id": run_id,
            "status": row.get("status", ""),
            # What the BINARY wrote, read back from the run row's meta_json
            # (modelderive/orchestrate.go:783-787). A declared schema addition
            # with its own test, not a ride-along on leg_diagnosis's failure
            # whitelist: leg["provisional"] is what the port ASKED for, and
            # leg["run"]["provisional"] is what the store RECORDED — only both
            # together prove -provisional crossed the seam.
            "provisional": bool(meta.get("provisional")),
            "prompt_version": row.get("prompt_version", ""),
            "derivation_version": row.get("derivation_version", ""),
            "input_hash": row.get("input_hash", ""),
            "data_boundary": row.get("data_boundary", ""),
            "window_start_epoch": row.get("window_start_epoch", 0),
            "window_end_epoch": row.get("window_end_epoch", 0),
            "observation_count": row.get("observation_count", 0),
            "proposed_observation_count": row.get("proposed_observation_count", 0),
        }
    legs.append(leg)


def leg_outcome(outcome):
    """The CLI's own verdict for one leg — the fields `-json` actually emits.

    `refused` is a BOOL there and nothing more: the execute path prints
    `outcome.Refused`, so the CLI alone cannot say WHICH refusal happened.
    `leg_diagnosis` below recovers that from the run row.
    """
    return {
        "status": outcome.get("status", ""),
        "refused": bool(outcome.get("refused", False)),
        "duplicate": bool(outcome.get("duplicate", False)),
        "written_count": outcome.get("written_count", 0),
        "rejected_count": outcome.get("rejected_count", 0),
        "unknown_count": outcome.get("unknown_count", 0),
        "skipped": outcome.get("skipped", ""),
    }


def leg_diagnosis(metadata):
    """WHY a leg refused or failed, read from the run row's meta_json.

    This is a WHITELIST, never a strip list, and the reason is specific: that
    same metadata carries `raw_response` (memoire:D44) — the model's entire
    reply to a prompt built from the user's transcript. The store retains it
    for operator audit; a bench result is a comparison artifact, not a custody
    one, and must never become a second home for transcript-derived content.
    Only the CLASSIFICATION crosses: what KIND of failure this was, never what
    the model said.

    The distinction is load-bearing for a ruling rather than merely tidy. The
    adapter maps two different things onto one refusal (qwen_adapter.go): a
    `content_filter` finish reason, and an empty model answer. A prompt
    adapter plausibly fixes the second; nothing fixes the first.
    """
    if not isinstance(metadata, dict):
        return {}
    out = {}
    refusal = metadata.get("refusal")
    if isinstance(refusal, str) and refusal:
        out["refusal"] = refusal
    failure = metadata.get("failure")
    if isinstance(failure, dict):
        if failure.get("stage"):
            out["failure_stage"] = str(failure["stage"])
        if failure.get("error"):
            # Clipped: it names the failure mode, but it is provider-controlled
            # text with no business growing unbounded in a result file.
            out["failure_error"] = str(failure["error"])[:300]
    # candidate_reason_counts stays behind deliberately: class-keyed and
    # harmless today, but the whitelist's strength is its narrowness — a
    # field joins it as a declared schema addition with its own tests, not
    # as a ride-along (review 2026-08-24-derive-bench-slice3 F1).
    return out


def classify_leg(derived):
    """One leg's genre. The session's genre summarizes the legs (memoire:D0048)."""
    if derived["exit_code"] != 0:
        return "cli_error"
    outcome = derived["outcome"]
    if not outcome:
        return "no_cli_output"
    if outcome.get("skipped"):
        return "skipped:" + str(outcome["skipped"])
    if outcome.get("refused"):
        return "refused"
    status = outcome.get("status", "")
    if status == "completed":
        return "ok"
    if status in ("partial", "failed"):
        return "run_" + status
    return "unknown_status:" + str(status)


SKIP_GENRES = ("split_depth_exceeded", "unsplittable_window")


def leg_is_gap(leg):
    return leg["genre"] in SKIP_GENRES


def session_genre(legs, replay):
    """The session's genre: "ok" only when every leg is ok.

    Otherwise a three-bucket summary — the leg genres carry the detail, and a
    partial session is neither hidden nor promoted to a failure (memoire:D0048).
    """
    if not replay.get("ok"):
        return "replay_mismatch"
    if not legs:
        return "no_legs"
    ok = sum(1 for leg in legs if leg["genre"] == "ok")
    skipped = sum(1 for leg in legs
                  if leg_is_gap(leg) or leg["genre"].startswith("skipped:"))
    if ok == len(legs):
        return "ok"
    return f"legs:ok={ok},failed={len(legs) - ok - skipped},skipped={skipped}"


def session_run_summary(legs, rhythm=None):
    """Session-level provenance, now that a run is a VECTOR of leg runs.

    `input_hash_vector` carries every leg's window and hash in execution order,
    skipped legs included with an empty hash: the splits are a function of the
    fixture alone, so comparing this vector across candidates verifies split
    determinism at the same time as it verifies they saw the same input. The
    rhythm cuts ride beside it for the same reason: they are the FIRST splitter,
    so two candidates that cut differently never saw the same windows.
    """
    rhythm = rhythm or {}
    cuts = list(rhythm.get("cuts") or [])
    return {
        "legs": len(legs),
        "prompt_versions": sorted({leg["run"].get("prompt_version", "")
                                   for leg in legs if leg["run"].get("prompt_version")}),
        "derivation_versions": sorted({leg["run"].get("derivation_version", "")
                                       for leg in legs if leg["run"].get("derivation_version")}),
        "data_boundaries": sorted({leg["run"].get("data_boundary", "")
                                   for leg in legs if leg["run"].get("data_boundary")}),
        "input_hash_vector": [
            [leg["window_start_epoch"], leg["window_end_epoch"],
             leg["run"].get("input_hash", "")]
            for leg in legs
        ],
        "rhythm_cuts": cuts,
        "segments": len(cuts) + 1,
        "observation_count": sum(leg["run"].get("observation_count", 0) for leg in legs),
        "proposed_observation_count": sum(
            leg["run"].get("proposed_observation_count", 0) for leg in legs),
        "coverage_gap": any(leg_is_gap(leg) for leg in legs),
        "coverage_gap_windows": [
            [leg["window_start_epoch"], leg["window_end_epoch"], leg["genre"]]
            for leg in legs if leg_is_gap(leg)
        ],
    }


def trim_observation(record):
    """A judge-facing observation row: content plus grounding, no store noise."""
    return {
        "kind": record.get("kind", ""),
        "text": record.get("text", ""),
        "confidence": record.get("confidence", 0),
        "source_event_ids": record.get("source_event_ids") or [],
        "review_state": record.get("review_state", ""),
        "content_hash": record.get("content_hash", ""),
        "noise_candidate": record.get("noise_candidate", 0),
        "can_instruct": record.get("can_instruct", 0),
    }


def run_one(args, binaries, endpoints_path, secret, fixture_path, out_dir):
    fixture = benchlib.load_fixture(fixture_path)
    session_id = fixture["session_id"]
    home = os.path.join(args.data_dir, "homes", benchlib.safe_name(args.candidate),
                        benchlib.safe_name(session_id))
    shutil.rmtree(home, ignore_errors=True)
    os.makedirs(home, exist_ok=True)

    result = {
        "bench": "derive",
        "candidate": args.candidate,
        "endpoint_id": args.endpoint_id,
        "model": args.model,
        "set": args.set_name,
        "project": fixture["project"],
        "session_id": session_id,
        "fixture": {
            "path": os.path.relpath(fixture_path, REPO_ROOT),
            "frozen_at_utc": fixture.get("frozen_at_utc", ""),
            "events": fixture["counts"]["events"],
            "by_type": fixture["counts"]["by_type"],
        },
        "min_content_guard": not args.no_min_content_guard,
        "max_prompt_bytes": args.max_prompt_bytes,
        "effective_max_prompt_bytes": args.max_prompt_bytes,
        "limits_source": getattr(args, "limits_source", "default"),
        "thinking": getattr(args, "thinking", None),
        "split_depth_cap": args.split_depth,
        # The rhythm pass has not run yet — it needs the seeded store. This
        # placeholder carries the KNOBS, which are argv-derived and final here,
        # and the ordinary "no cut needed" verdict; the real block replaces it
        # below. A session that dies in seeding keeps it, which is honest: no
        # window was cut because no window was derived (memoire:D0048 posture,
        # the same one `result["legs"] = legs` takes).
        "rhythm": rhythm_block(args, "disabled" if args.no_rhythm_cuts else "ok",
                               enabled=not args.no_rhythm_cuts),
        "started_at_utc": datetime.datetime.now(datetime.timezone.utc)
        .replace(microsecond=0).isoformat(),
    }

    column = ThrowawayColumn(binaries["memoire-api"], home,
                             os.path.join(home, "api.log"))
    try:
        column.start()
        result["timings"] = seed_session(column, fixture, args.seed_stagger_ms)
        result["replay"] = verify_replay(column, fixture)

        ctx = {
            "derive_bin": binaries["memoire-derive"],
            "env": derive_env(home, endpoints_path, *secret),
            "args": args, "fixture": fixture, "column": column, "dry_run_ms": 0,
        }
        # Rhythm cuts FIRST (memoire:D0261): the detector reads the seeded
        # store, so it runs after the seed and before the windowing walk.
        ctx["rhythm"] = cut_points(ctx)
        result["rhythm"] = ctx["rhythm"]
        legs = []
        # Bound into the result BEFORE the walk: the list is mutated in place,
        # so a mid-walk failure still lands the legs already completed rather
        # than discarding the session's work (memoire:D0048).
        result["legs"] = legs
        derive_session(ctx, legs)
        result["timings"]["dry_run_ms"] = ctx["dry_run_ms"]
        result["timings"]["derive_ms"] = sum(leg["derive_ms"] for leg in legs)
        result["genre"] = session_genre(legs, result["replay"])

        # Observations are aggregated across the completed legs, fetched per
        # leg run id IN LEG ORDER — the order the near-dup gate saw them.
        observations = []
        for leg in legs:
            run_id = leg["run"].get("run_id", "")
            if not run_id:
                continue
            observations.extend(
                trim_observation(o) for o in column.api().observations(
                    fixture["project"], session_id, derivation_run_id=run_id
                )
            )
        result["observations"] = observations
        result["run"] = session_run_summary(legs, result["rhythm"])
    except benchlib.BenchError as err:
        # A setup failure is data too: the run lands with its genre and the
        # driver moves to the next session.
        result.setdefault("timings", {})
        result.setdefault("replay", {"ok": False})
        result["genre"] = "seed_failed"
        result["error"] = str(err)
        result.setdefault("legs", [])
        result.setdefault("observations", [])
        result["run"] = session_run_summary(result["legs"], result["rhythm"])
    finally:
        column.stop()
        if not args.keep_home:
            shutil.rmtree(home, ignore_errors=True)

    path = os.path.join(out_dir, benchlib.safe_name(session_id) + ".json")
    benchlib.write_json(path, result)
    return result, path


def main(argv):
    args = parse_args(argv)
    set_dir = os.path.join(args.data_dir, benchlib.safe_name(args.set_name))
    index_path = os.path.join(set_dir, "set.json")
    if not os.path.exists(index_path):
        raise benchlib.BenchError(
            f"eval set {args.set_name!r} is not frozen ({index_path} missing) — "
            "run freeze.py first"
        )
    sessions = args.sessions or benchlib.read_json(index_path).get("sessions") or []
    if not sessions:
        raise benchlib.BenchError(f"eval set {args.set_name!r} lists no sessions")

    endpoints_path = resolve_endpoints_path(args.endpoints)
    entry = load_endpoint(endpoints_path, args.endpoint_id, args.model)
    secret = endpoint_secret(entry)
    resolve_prompt_ceiling(args, entry)

    out_dir = args.out or os.path.join(
        REPO_ROOT, "tmp", "derive-bench",
        datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d"),
        benchlib.safe_name(args.candidate),
    )
    os.makedirs(out_dir, exist_ok=True)

    binaries = build_binaries(os.path.join(args.data_dir, "bin"))
    print(f"run: candidate={args.candidate} endpoint={args.endpoint_id} "
          f"model={args.model} sessions={len(sessions)} -> {out_dir}")

    results = []
    for session_id in sessions:
        fixture_path = benchlib.fixture_path(args.data_dir, args.set_name, session_id)
        if not os.path.exists(fixture_path):
            print(f"  {session_id}: SKIP (no fixture at {fixture_path})")
            continue
        result, path = run_one(args, binaries, endpoints_path, secret,
                               fixture_path, out_dir)
        results.append(result)
        summary = result.get("run", {})
        gaps = summary.get("coverage_gap_windows") or []
        print(f"  {session_id}: genre={result['genre']} "
              f"legs={summary.get('legs', 0)} "
              f"observations={len(result['observations'])} "
              f"derive_ms={result['timings'].get('derive_ms', 0)}"
              f"{' COVERAGE-GAP=' + str(len(gaps)) if gaps else ''} -> {path}")

    # The leg vector must be identical for every candidate over the same
    # fixture — same windows AND same input hashes. It is recorded per run so
    # the scorecard can prove it rather than assume it.
    summary_path = os.path.join(out_dir, "_summary.json")
    benchlib.write_json(summary_path, {
        "candidate": args.candidate,
        "endpoint_id": args.endpoint_id,
        "model": args.model,
        "set": args.set_name,
        "finished_at_utc": datetime.datetime.now(datetime.timezone.utc)
        .replace(microsecond=0).isoformat(),
        "runs": [
            {
                "session_id": r["session_id"],
                "genre": r["genre"],
                "observations": len(r["observations"]),
                "legs": r.get("run", {}).get("legs", 0),
                "input_hash_vector": r.get("run", {}).get("input_hash_vector", []),
                "prompt_versions": r.get("run", {}).get("prompt_versions", []),
                "coverage_gap": r.get("run", {}).get("coverage_gap", False),
                "coverage_gap_windows": r.get("run", {}).get("coverage_gap_windows", []),
                "derive_ms": r["timings"].get("derive_ms", 0),
            }
            for r in results
        ],
    })
    print(f"run: {len(results)} run(s) -> {summary_path}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except benchlib.BenchError as err:
        print(f"run: {err}", file=sys.stderr)
        sys.exit(1)
