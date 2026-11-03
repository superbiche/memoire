#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Synthetic Muse Code session stores for muse-store-capture.sh.

Every record is synthetic. The shapes follow the Muse Code 1.4.7 session log
(session.jsonl: line 1 a retained frame, then one event record per line; nested
subagent sessions under <session>/subagent/<child_session_id>/session.jsonl,
storage_layout nested_session_v1), mapped on throwaway sessions only
(format map: /var/tmp/memory-audit/muse-capture/format-map.md, not in-tree).

Usage: muse-store-fixtures.py <op> <sessions_root> [options]
  new      --id SID --workspace DIR [--date YYYY/MM/DD] [--t0 MICROS]
  turn     --id SID --prompt TEXT [--reply TEXT ...] [--run RID]
           [--terminal completed|cancelled|none] [--tool NAME=ARGS:RESULT ...]
           [--reasoning TEXT] [--model NAME] [--task]
  spawn    --id SID --child CID --subagent AID [--role ROLE] [--reply TEXT]
  internal --id SID --child CID [--reply TEXT]
  end      --id SID [--exit-reason clean]
  garbage  --id SID            append one unparseable line
  gap      --id SID [--count N] [--marker M]
                               append N identical retained-marker lines (an
                               omitted ephemeral record; muse export turns
                               each into one gap event)
  dup      --id SID            re-append the last record line verbatim (a
                               duplicate record id)
  touch    --id SID --age SEC  set the mtime of every file of the session tree
Records are appended; sequence, recorded_at and record ids are deterministic
(sequence = previous + 1, recorded_at = t0 + sequence * 1000 us, id = uuid5).
"""
import argparse
import hashlib
import json
import os
import sys
import time
import uuid

NS = uuid.UUID("6f1c8d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f")
T0 = 1792253635000000


def die(msg):
    print(f"muse-store-fixtures: {msg}", file=sys.stderr)
    sys.exit(2)


def find_dir(root, sid):
    for dirpath, dirnames, _ in os.walk(root):
        if os.path.basename(dirpath) == sid and os.path.isfile(os.path.join(dirpath, "session.jsonl")):
            return dirpath
    die(f"no session {sid} under {root}")


def last_state(path):
    seq, t0 = 0, T0
    meta = os.path.join(os.path.dirname(path), ".fixture-t0")
    if os.path.exists(meta):
        t0 = int(open(meta).read().strip())
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            try:
                rec = json.loads(line)
            except ValueError:
                continue
            if isinstance(rec.get("sequence"), int):
                seq = max(seq, rec["sequence"])
            for child in rec.get("children", []) or []:
                try:
                    seq = max(seq, json.loads(child["record_json"]).get("sequence", 0))
                except (ValueError, KeyError, TypeError):
                    pass
    return seq, t0


class Log:
    def __init__(self, path, sid, t0=None):
        self.path, self.sid = path, sid
        self.seq, self.t0 = last_state(path) if os.path.exists(path) else (0, t0 or T0)
        if t0 is not None:
            self.t0 = t0

    def record(self, payload_type, payload, schema=1):
        self.seq += 1
        rec = {
            "schema_version": 1,
            "id": str(uuid.uuid5(NS, f"{self.sid}:{self.seq}")),
            "stream": {"kind": "session", "id": self.sid},
            "sequence": self.seq,
            "recorded_at": self.t0 + self.seq * 1000,
            "record_type": "event",
            "durability": "durable",
            "causation_id": None,
            "payload_type": payload_type,
            "payload_schema_version": schema,
            "payload": payload,
        }
        return rec

    def write(self, *records):
        with open(self.path, "a", encoding="utf-8") as fh:
            for rec in records:
                fh.write(json.dumps(rec, ensure_ascii=False, separators=(",", ":")) + "\n")


def gap_marker(sid, marker, seq, mid):
    """A retained marker for an omitted ephemeral record (Muse Code 1.4.7 shape,
    accepted by the real exporter as a gap)."""
    return {"schema_version": 1, "retained_marker": marker,
            "stream": {"kind": "session", "id": sid},
            "position": {"sequence": seq, "id": mid},
            "omitted_record": {"durability": "ephemeral", "omission_class": "live_only",
                               "payload_kind": "run", "payload_schema_version": 1,
                               "payload_type": "runtime.session.progress", "record_type": "event"}}


def new_session(root, sid, workspace, date, t0, child=False, parent_dir=None):
    d = os.path.join(parent_dir, "subagent", sid) if child else os.path.join(root, date, sid)
    os.makedirs(d, exist_ok=True)
    path = os.path.join(d, "session.jsonl")
    if os.path.exists(path):
        die(f"session {sid} exists")
    with open(os.path.join(d, ".fixture-t0"), "w") as fh:
        fh.write(str(t0))
    open(os.path.join(d, ".session.lock"), "w").write('{"pid":1,"host":"fixture"}\n')
    log = Log(path, sid, t0)
    children = [
        log.record("runtime.session.permission_format_declared", {"format": "profile_v1"}),
        log.record("runtime.session.permission_profile_selected", {"profile": "ask-me"}),
    ]
    frame = {
        "retained_frame": "session_permission_transaction",
        "frame_schema_version": 1,
        "outer_log_ordinal": 1,
        "transaction_id": str(uuid.uuid5(NS, f"{sid}:frame")),
        "children": [
            {"child_index": i, "record_json": json.dumps(c, separators=(",", ":"))}
            for i, c in enumerate(children)
        ],
    }
    frame["content_sha256"] = "sha256:" + hashlib.sha256(
        json.dumps(frame["children"], separators=(",", ":")).encode()).hexdigest()
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(json.dumps(frame, separators=(",", ":")) + "\n")
    meta = {"model_id": "muse-spark-1.2", "provider_id": "meta"}
    if not child:
        meta.update({"tool_surface_version": "2", "workspace_root": workspace})
    recs = [
        log.record("runtime.session.capture_selection",
                   {"publication_id": str(uuid.uuid5(NS, f"{sid}:pub")), "schema_version": 1,
                    "selection": "armed", "session_stream": {"id": sid, "kind": "session"}}),
        log.record("runtime.session.metadata", {"kind": "metadata", "record": meta}),
    ]
    if not child:
        recs.append(log.record("session.opened.observed", {"kind": "session_opened", "record": {
            "client_info_name": "muse_code_acp", "client_info_version": "0.10.0", "resume": False,
            "schema_version": 1, "security_mode": "normal", "session_id": sid}}))
    log.write(*recs)
    return d


def run_event(log, rid, event, kind="run"):
    return log.record("runtime.session", {"event": event, "kind": kind, "run_id": rid})


def turn(path, sid, prompt, replies, rid, terminal, tools, reasoning, model, task_noise=True):
    log = Log(path, sid)
    rid = rid or str(uuid.uuid5(NS, f"{sid}:run:{log.seq}"))
    recs = [run_event(log, rid, {"kind": "started", "prompt": prompt})]
    if task_noise:
        tid = str(uuid.uuid5(NS, f"{rid}:task"))
        recs.append(log.record("runtime.session",
                               {"event": {"kind": "started", "task_id": tid}, "kind": "task",
                                "run_id": rid, "task_id": tid}))
        recs.append(log.record("runtime.session",
                               {"event": {"kind": "completed", "task_id": tid}, "kind": "task",
                                "run_id": rid, "task_id": tid}))
    if reasoning is not None:
        recs.append(run_event(log, rid, {"kind": "reasoning_committed", "text": reasoning,
                                         "encrypted_content": "ZmFrZS1lbmNyeXB0ZWQtcmVhc29uaW5n"}))
    usage = {"input_tokens": 1200, "output_tokens": 80, "reasoning_tokens": 10,
             "cache_read_tokens": 0, "cache_write_tokens": 0, "cached_tokens": 0}
    if tools:
        calls, results = [], []
        for i, spec in enumerate(tools):
            name, rest = spec.split("=", 1)
            args, result = rest.rsplit(":", 1)
            cid = f"call_{rid[:8]}_{i}"
            calls.append({"name": name, "args": args, "call_id": cid})
            results.append({"text": result, "tool_call_id": cid})
        mid = str(uuid.uuid5(NS, f"{rid}:tools"))
        recs.append(run_event(log, rid, {"kind": "model_completed", "model": model, "duration_ms": 900,
                                         "finish_reason": "tool_calls", "usage": usage}))
        recs.append(run_event(log, rid, {"kind": "assistant_tool_calls_committed", "message_id": mid,
                                         "tool_calls": calls}))
        recs.append(run_event(log, rid, {"kind": "tool_result_batch_committed", "batch_id": mid,
                                         "results": results}))
    for i, text in enumerate(replies):
        recs.append(run_event(log, rid, {"kind": "assistant_message_committed",
                                         "message_id": str(uuid.uuid5(NS, f"{rid}:msg:{i}")),
                                         "text": text}))
    if replies:
        recs.append(run_event(log, rid, {"kind": "model_completed", "model": model, "duration_ms": 1100,
                                         "finish_reason": "stop", "usage": usage}))
    if terminal != "none":
        recs.append(run_event(log, rid, {"kind": "terminal", "terminal": terminal, "reason": None,
                                         "turn_duration_ms": 2000}))
    log.write(*recs)
    return rid


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("op")
    ap.add_argument("root")
    ap.add_argument("--id", required=True)
    ap.add_argument("--workspace", default="/tmp/muse-cap-proj")
    ap.add_argument("--date", default="2026/10/17")
    ap.add_argument("--t0", type=int, default=T0)
    ap.add_argument("--prompt", default="")
    ap.add_argument("--reply", action="append", default=[])
    ap.add_argument("--run")
    ap.add_argument("--terminal", default="completed", choices=["completed", "cancelled", "none"])
    ap.add_argument("--tool", action="append", default=[])
    ap.add_argument("--reasoning")
    ap.add_argument("--model", default="muse-spark-1.2")
    ap.add_argument("--child")
    ap.add_argument("--subagent")
    ap.add_argument("--role", default="workflow-agent")
    ap.add_argument("--exit-reason", default="clean")
    ap.add_argument("--age", type=int, default=0)
    ap.add_argument("--count", type=int, default=1)
    ap.add_argument("--marker", default="omitted_live_only")
    a = ap.parse_args()
    root = os.path.join(a.root, "")[:-1]

    if a.op == "new":
        print(new_session(root, a.id, a.workspace, a.date, a.t0))
        return
    d = find_dir(root, a.id)
    path = os.path.join(d, "session.jsonl")
    if a.op == "turn":
        print(turn(path, a.id, a.prompt, a.reply, a.run, a.terminal, a.tool, a.reasoning, a.model))
    elif a.op in ("spawn", "internal"):
        if not a.child:
            die("--child required")
        log = Log(path, a.id)
        if a.op == "spawn":
            if not a.subagent:
                die("--subagent required")
            log.write(
                log.record("subagent.control.spawn_accepted", {"kind": "subagent_control", "record": {
                    "kind": "spawn_accepted", "agent_lineage_admission": {
                        "agent_instance_id": a.subagent, "immediate_parent_agent_id": a.id}}}, schema=4),
                log.record("subagent.control.resume_context_recorded", {"kind": "subagent_control", "record": {
                    "kind": "resume_context_recorded", "parent_session_id": a.id, "role": a.role,
                    "spawn_command_id": str(uuid.uuid5(NS, f"{a.subagent}:spawn")),
                    "subagent_id": a.subagent}}),
                log.record("subagent.control.child_session_bound", {"kind": "subagent_control", "record": {
                    "child_session_id": a.child, "kind": "child_session_bound", "schema_version": 1,
                    "storage_layout": "nested_session_v1", "subagent_id": a.subagent}}),
            )
        log_t0 = Log(path, a.id).t0
        cd = new_session(root, a.child, None, None, log_t0 + 500000, child=True, parent_dir=d)
        cpath = os.path.join(cd, "session.jsonl")
        turn(cpath, a.child, "Child task: " + (a.role if a.op == "spawn" else "reminder"),
             a.reply or ["child answer"], None, "completed", [], None, a.model, task_noise=False)
        if a.op == "spawn":
            log = Log(path, a.id)
            log.write(log.record("subagent.control.result_ready", {"kind": "subagent_control", "record": {
                "kind": "result_ready", "result_report_outcome": "reported", "subagent_id": a.subagent}}))
        print(cd)
    elif a.op == "end":
        log = Log(path, a.id)
        log.write(log.record("session.end", {"kind": "session_end", "record": {
            "exit_reason": a.exit_reason, "schema_version": 1, "session_id": a.id, "uptime_ms": 4242}}))
    elif a.op == "garbage":
        with open(path, "a", encoding="utf-8") as fh:
            fh.write('{"truncated": \n')
    elif a.op == "gap":
        seq, _ = last_state(path)
        with open(path, "a", encoding="utf-8") as fh:
            for n in range(a.count):
                fh.write(json.dumps(gap_marker(a.id, a.marker, seq + 1,
                                               str(uuid.uuid5(NS, f"{a.id}:gap:{seq}:{n}"))),
                                    separators=(",", ":")) + "\n")
    elif a.op == "dup":
        last = None
        with open(path, encoding="utf-8") as fh:
            for line in fh:
                try:
                    rec = json.loads(line)
                except ValueError:
                    continue
                if "payload_type" in rec:
                    last = line if line.endswith("\n") else line + "\n"
        if last is None:
            die("no record to duplicate")
        with open(path, "a", encoding="utf-8") as fh:
            fh.write(last)
    elif a.op == "touch":
        t = time.time() - a.age
        for dirpath, _, files in os.walk(d):
            for f in files:
                os.utime(os.path.join(dirpath, f), (t, t))
    else:
        die(f"unknown op {a.op}")


if __name__ == "__main__":
    main()
