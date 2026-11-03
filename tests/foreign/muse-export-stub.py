#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Stand-in for `muse export` (Muse Code 1.4.7, export_schema_version 1).

Invoked as: muse export --session <session.jsonl> --out <file> [--redacted]
It decodes the subset of the export envelope the capture adapter relies on:
  top level  export_schema_version, exporter_version, redaction, diagnostics,
             session_terminated_abnormally, sessions[0] (session_id,
             root_session_id, turn_count, accepted_spawns, session_end)
  events[]   {kind: "record", recorded_at, envelope: <the record as logged>,
              derived: {decoded, session_id, trajectory_id, step_id, turn_count}}
             {kind: "retained_frame", outer_log_ordinal, envelope: <frame>,
              derived: {decoded: true}}
             {kind: "unparseable_line", raw_text: <the line, newline stripped>}
             {kind: "gap", marker, omitted_record: {durability, omission_class,
              payload_kind, payload_schema_version, payload_type, record_type},
              stream: {id, kind}}  -- a record the exporter omitted (seen live:
              marker "omitted_live_only", counted in diagnostics.gaps and
              diagnostics.omitted_live_only); it carries no id, sequence or time
Known divergence: the real binary rejects a synthetic retained frame (its
content_sha256 cannot be reproduced) and reports it as an unparseable_line;
the stub decodes it. The contract check skips the frame line for that reason.
A retained-marker log line {"schema_version", "retained_marker": M, "stream",
"position": {"sequence", "id"}, "omitted_record"} becomes one gap event
{kind: "gap", marker: M, omitted_record, stream} at that position, counted in
diagnostics.gaps and omitted_live_only (as the real binary does for a valid
marker). A repeated record id is exported again as a record event with
derived {decoded: false, duplicate: true, session_id, trajectory_id} and
counted in diagnostics.duplicate_records (the real binary keeps it; it does
not drop it).
`muse-store-capture.sh contract` checks these fields against the real binary.

Knobs: MUSE_STUB_LOG (append the --session argument, one line per call),
MUSE_STUB_FAIL (substring of the session path -> exit 1, nothing written),
MUSE_STUB_SCHEMA (export_schema_version to report, default 1),
MUSE_STUB_APPEND (a file holding lines to append to the --session file AFTER the
export is taken, then removed: a producer appending while the exporter runs).
"""
import json
import os
import sys


def usage(msg):
    print(f"muse: {msg}\nusage: muse export [--session <id|path>] [--last] [--out <file>] [--redacted]",
          file=sys.stderr)
    sys.exit(2)


def main(argv):
    if not argv or argv[0] != "export":
        usage("only `export` is stubbed")
    args, session, out = argv[1:], None, None
    i = 0
    while i < len(args):
        if args[i] == "--session" and i + 1 < len(args):
            session = args[i + 1]
            i += 2
        elif args[i] == "--out" and i + 1 < len(args):
            out = args[i + 1]
            i += 2
        elif args[i] == "--redacted":
            i += 1
        else:
            usage(f"unexpected argument {args[i]}")
    if not session or not out:
        usage("--session and --out are required")
    log = os.environ.get("MUSE_STUB_LOG")
    if log:
        with open(log, "a") as fh:
            fh.write(session + "\n")
    fail = os.environ.get("MUSE_STUB_FAIL")
    if fail and fail in session:
        print("muse: export failed (stub)", file=sys.stderr)
        sys.exit(1)
    if not os.path.isfile(session):
        usage(f"--session {session} is neither an existing file nor a session id")

    sid = os.path.basename(os.path.dirname(session))
    events, unparseable, turns, spawns, end = [], 0, 0, [], None
    seen = set()
    duplicates = 0
    gaps, live_only = 0, 0
    with open(session, encoding="utf-8") as fh:
        for line in fh:
            try:
                rec = json.loads(line)
            except ValueError:
                unparseable += 1
                events.append({"kind": "unparseable_line", "raw_text": line.rstrip("\n")})
                continue
            if "retained_marker" in rec:
                gaps += 1
                if rec.get("retained_marker") == "omitted_live_only":
                    live_only += 1
                events.append({"kind": "gap", "marker": rec.get("retained_marker"),
                               "omitted_record": rec.get("omitted_record"), "stream": rec.get("stream")})
                continue
            if "retained_frame" in rec:
                events.append({"kind": "retained_frame", "outer_log_ordinal": rec.get("outer_log_ordinal"),
                               "envelope": rec, "derived": {"decoded": True}})
                continue
            if rec.get("id") in seen:
                duplicates += 1
                events.append({"kind": "record", "recorded_at": rec.get("recorded_at"), "envelope": rec,
                               "derived": {"decoded": False, "duplicate": True, "session_id": sid,
                                           "trajectory_id": f"trajectory_{sid}"}})
                continue
            seen.add(rec.get("id"))
            p = rec.get("payload") or {}
            ev = p.get("event") if isinstance(p, dict) else None
            if rec.get("payload_type") == "runtime.session" and isinstance(ev, dict) \
                    and ev.get("kind") == "started" and p.get("kind") == "run":
                turns += 1
            if rec.get("payload_type") == "subagent.control.resume_context_recorded":
                r = p.get("record") or {}
                spawns.append({"agent_path": f"main/{r.get('role')}/{len(spawns) + 1}",
                               "parent_session_id": r.get("parent_session_id"),
                               "role": r.get("role"), "subagent_id": r.get("subagent_id")})
            if rec.get("payload_type") == "session.end":
                r = p.get("record") or {}
                end = {"exit_reason": r.get("exit_reason"), "uptime_ms": r.get("uptime_ms")}
            events.append({"kind": "record", "recorded_at": rec.get("recorded_at"), "envelope": rec,
                           "derived": {"decoded": True, "session_id": sid,
                                       "trajectory_id": f"trajectory_{sid}",
                                       "step_id": rec.get("sequence"), "turn_count": turns}})
    doc = {
        "diagnostics": {"duplicate_records": duplicates, "gaps": gaps, "omitted_live_only": live_only,
                        "unknown_payload_kinds": 0, "unparseable_lines": unparseable},
        "events": events,
        "export_schema_version": int(os.environ.get("MUSE_STUB_SCHEMA", "1")),
        "exporter_version": {"display": "Muse Code 1.4.7 (stub)", "semver": "1.4.7", "sha": "stub"},
        "redaction": "raw",
        "session_terminated_abnormally": False,
        "sessions": [{"accepted_spawns": spawns, "is_copied_context": False, "root_session_id": sid,
                      "saw_history_gap": False, "session_end": end, "session_id": sid,
                      "step_count": len(events), "trajectory_id": f"trajectory_{sid}",
                      "turn_count": turns}],
    }
    with open(out, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, ensure_ascii=False)
    append = os.environ.get("MUSE_STUB_APPEND")
    if append and os.path.isfile(append):
        with open(append, encoding="utf-8") as src, open(session, "a", encoding="utf-8") as dst:
            dst.write(src.read())
        os.remove(append)
    print(f"wrote session export to {out}")


if __name__ == "__main__":
    main(sys.argv[1:])
