#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Synthetic acpx session records and journal streams for acpx-journal-capture.sh.

Usage: acpx-journal-fixtures.py <scenario> <dir> [key=value ...]

Writes an acpx record (<record_id>.json) and its journal stream files into <dir>
(an acpx sessions dir, or an archive sessions dir with segments/<record_id>/),
then prints one JSON object describing what an adapter must emit: the record id,
session id, and per turn the request id, prompt text, message texts, final text,
status and stop reason. Every value is synthetic.

The stream format mirrors acpx 0.19 (schema acpx.session.journal.v1): journal
meta lines (segment, turn_started, turn_result) interleaved with the raw ACP
JSON-RPC traffic (initialize, session/new|resume|load, session/prompt,
session/update notifications, responses). JSON-RPC ids restart at every
reconnect; the journal request_id is the stable turn key.
"""
import hashlib
import json
import os
import sys
import uuid

J = "acpx.session.journal.v1"


def uid(seed):
    return str(uuid.UUID(hashlib.sha256(seed.encode()).hexdigest()[:32]))


class Stream:
    def __init__(self, record_id, session_id):
        self.record_id = record_id
        self.session_id = session_id
        self.segments = [[]]
        self.rpc = 0

    def meta(self, **kw):
        self.segments[-1].append(dict(schema=J, **kw))

    def new_segment(self):
        seq = len(self.segments)
        self.segments.append([])
        self.meta(type="segment", record_id=self.record_id, sequence=seq,
                  message_sequence=seq * 100, request_id=None)

    def connect(self, method="session/resume"):
        self.rpc = 0
        self.segments[-1].append({"jsonrpc": "2.0", "id": 0, "method": "initialize",
                                  "params": {"protocolVersion": 1}})
        self.segments[-1].append({"jsonrpc": "2.0", "id": 0, "result": {"protocolVersion": 1}})
        self.rpc = 1
        self.segments[-1].append({"jsonrpc": "2.0", "id": 1, "method": method,
                                  "params": {"sessionId": self.session_id, "cwd": "/x"}})
        self.segments[-1].append({"jsonrpc": "2.0", "id": 1, "result": {}})
        self.rpc = 2

    def update(self, upd):
        self.segments[-1].append({"jsonrpc": "2.0", "method": "session/update",
                                  "params": {"sessionId": self.session_id, "update": upd}})

    def chunks(self, text, message_id=None, kind="agent_message_chunk", size=7):
        for i in range(0, len(text), size):
            upd = {"sessionUpdate": kind, "content": {"type": "text", "text": text[i:i + size]}}
            if message_id is not None:
                upd["messageId"] = message_id
            self.update(upd)

    def tool(self, n):
        tid = "call-%d" % n
        self.update({"sessionUpdate": "tool_call", "toolCallId": tid, "kind": "execute",
                     "title": "ls", "status": "in_progress", "rawInput": {"command": "ls"}})
        self.update({"sessionUpdate": "tool_call_update", "toolCallId": tid, "status": "completed"})

    def turn(self, request_id, prompt, messages, *, status="completed", stop="end_turn",
             model="test-model-1", ids=True, thought="private reasoning text", complete=True,
             split_after_message=None):
        self.meta(type="turn_started", request_id=request_id)
        rid = self.rpc
        self.rpc += 1
        self.segments[-1].append({"jsonrpc": "2.0", "id": rid, "method": "session/prompt",
                                  "params": {"sessionId": self.session_id,
                                             "prompt": [{"type": "text", "text": p} for p in prompt]}})
        self.chunks(thought, "th-" + request_id, kind="agent_thought_chunk")
        for n, msg in enumerate(messages):
            self.chunks(msg, ("msg-%s-%d" % (request_id, n)) if ids else None)
            if n < len(messages) - 1:
                self.tool(n)
            if split_after_message == n:
                self.new_segment()
        if not complete:
            return
        meta = {"quota": {"model_usage": [{"model": model, "token_count": {"totalTokens": 10}}]}}
        self.segments[-1].append({"jsonrpc": "2.0", "id": rid,
                                  "result": {"stopReason": stop, "usage": {}, "_meta": meta}})
        self.meta(type="turn_result", request_id=request_id,
                  result={"status": status, "stopReason": stop, "_meta": meta})


def record(record_id, session_id, agent, cwd, name="worker"):
    return {
        "schema": "acpx.session.v1", "acpx_record_id": record_id, "acp_session_id": session_id,
        "agent_command": agent, "cwd": cwd, "name": name,
        "created_at": "2026-10-01T10:00:00.000Z", "last_used_at": "2026-10-01T11:00:00.000Z",
        "last_prompt_at": "2026-10-01T10:59:00.000Z", "closed": False,
        "messages": [{"User": {"id": "u1", "content": [{"Text": "record copy, not read"}]}}],
        "event_log": {"active_path": "", "segment_count": 1},
    }


def write(dirpath, rec, stream, layout="live"):
    os.makedirs(dirpath, exist_ok=True)
    rid = rec["acpx_record_id"]
    with open(os.path.join(dirpath, rid + ".json"), "w") as f:
        json.dump(rec, f)
    segs = stream.segments
    active = segs[-1]
    rotated = segs[:-1]
    if layout == "live":
        for n, seg in enumerate(rotated, start=1):
            write_lines(os.path.join(dirpath, "%s.stream.%d.ndjson" % (rid, n)), seg)
    else:  # archive: rotated segments under segments/<rid>/<sha256>.ndjson
        sd = os.path.join(dirpath, "segments", rid)
        os.makedirs(sd, exist_ok=True)
        for seg in rotated:
            body = "".join(json.dumps(l, ensure_ascii=False) + "\n" for l in seg)
            h = hashlib.sha256(body.encode()).hexdigest()
            with open(os.path.join(sd, h + ".ndjson"), "w") as f:
                f.write(body)
    write_lines(os.path.join(dirpath, rid + ".stream.ndjson"), active)


def write_lines(path, lines):
    with open(path, "w") as f:
        for l in lines:
            f.write(json.dumps(l, ensure_ascii=False) + "\n")


def expect_turn(request_id, prompt, messages, status="completed", stop="end_turn",
                model="test-model-1", complete=True):
    full = "".join(prompt)
    return {"request_id": request_id, "prompt": full,
            "prompt_sha256": hashlib.sha256(full.encode()).hexdigest(),
            "messages": messages, "final": messages[-1] if messages else "",
            "status": status if complete else None, "stop_reason": stop if complete else None,
            "model": model if complete else None, "complete": complete}


def scenario(name, d, opts):
    agent = opts.get("agent", "/home/u/.local/share/mise/shims/codex-acp")
    cwd = opts.get("cwd", "/tmp/acpx-cap-proj")
    seed = opts.get("seed", name)
    rid, sid = uid(seed + "/record"), uid(seed + "/session")
    s = Stream(rid, sid)
    s.meta(type="segment", record_id=rid, sequence=0, message_sequence=0, request_id=None)
    turns = []
    if name == "basic":
        s.segments[-1].append({"jsonrpc": "2.0", "id": 0, "method": "initialize", "params": {}})
        s.segments[-1].append({"jsonrpc": "2.0", "id": 0, "result": {}})
        s.segments[-1].append({"jsonrpc": "2.0", "id": 1, "method": "session/new", "params": {"cwd": cwd}})
        s.segments[-1].append({"jsonrpc": "2.0", "id": 1, "result": {"sessionId": sid}})
        s.rpc = 2
        for n in range(2):
            r = uid(seed + "/turn%d" % n)
            prompt = ["Task %d: inspect the repo.\n" % n, "Report findings."]
            msgs = ["Looking at the files (turn %d)." % n,
                    "Final report %d: the parser drops empty lines; fix in reader.go." % n]
            s.turn(r, prompt, msgs)
            turns.append(expect_turn(r, prompt, msgs))
    elif name == "reconnect":
        # Two connections: JSON-RPC ids restart, the journal request ids differ.
        # A session/load replay of the first turn's text precedes the second prompt.
        s.connect("session/new")
        r1 = uid(seed + "/t1")
        s.turn(r1, ["first prompt"], ["first answer"])
        turns.append(expect_turn(r1, ["first prompt"], ["first answer"]))
        s.connect("session/load")
        s.chunks("REPLAYED first answer", "replay-1")  # replay outside any turn
        r2 = uid(seed + "/t2")
        s.turn(r2, ["second prompt"], ["second answer"])
        turns.append(expect_turn(r2, ["second prompt"], ["second answer"]))
    elif name == "noids":
        # Agent without messageId: tool calls separate messages.
        s.connect("session/new")
        r = uid(seed + "/t")
        msgs = ["step one", "step two", "final without ids"]
        s.turn(r, ["go"], msgs, ids=False)
        turns.append(expect_turn(r, ["go"], msgs))
    elif name == "incomplete":
        s.connect("session/new")
        r1 = uid(seed + "/done")
        s.turn(r1, ["done prompt"], ["done answer"])
        turns.append(expect_turn(r1, ["done prompt"], ["done answer"]))
        r2 = uid(seed + "/open")
        s.turn(r2, ["open prompt"], ["partial progress"], complete=False)
        turns.append(expect_turn(r2, ["open prompt"], ["partial progress"], complete=False))
    elif name == "cancelled":
        s.connect("session/new")
        r = uid(seed + "/c")
        s.turn(r, ["long task"], ["started"], status="cancelled", stop="cancelled")
        turns.append(expect_turn(r, ["long task"], ["started"], status="cancelled", stop="cancelled"))
    elif name == "long":
        s.connect("session/new")
        r = uid(seed + "/l")
        unit = "Résumé ✓ naïve 𝔘𝔫𝔦𝔠𝔬𝔡𝔢 line.\n"
        final = (unit * (15000 // len(unit) + 1))[:15000]
        prompt = ["P" * 9000]
        s.turn(r, prompt, ["short note", final])
        turns.append(expect_turn(r, prompt, ["short note", final]))
    elif name == "segmented":
        # A turn spans a rotation boundary; rotated segment files are named by
        # content hash in the archive layout, so order comes from the segment
        # meta sequence, never from file names.
        s.connect("session/new")
        r1 = uid(seed + "/a")
        s.turn(r1, ["before rotation"], ["one", "two"])
        turns.append(expect_turn(r1, ["before rotation"], ["one", "two"]))
        r2 = uid(seed + "/b")
        msgs = ["msg before boundary", "msg after boundary", "final after boundary"]
        s.turn(r2, ["spanning prompt"], msgs, split_after_message=0)
        turns.append(expect_turn(r2, ["spanning prompt"], msgs))
        s.new_segment()
        r3 = uid(seed + "/c")
        s.turn(r3, ["third"], ["third final"])
        turns.append(expect_turn(r3, ["third"], ["third final"]))
    elif name == "rpconly":
        # Old format: no journal meta lines at all.
        s.segments[-1] = []
        s.connect("session/new")
        s.segments[-1].append({"jsonrpc": "2.0", "id": 2, "method": "session/prompt",
                               "params": {"sessionId": sid, "prompt": [{"type": "text", "text": "x"}]}})
        s.chunks("y", "m")
        s.segments[-1].append({"jsonrpc": "2.0", "id": 2, "result": {"stopReason": "end_turn"}})
    else:
        sys.exit("unknown scenario " + name)
    rec = record(rid, sid, agent, cwd, name=opts.get("name", "worker"))
    write(d, rec, s, layout=opts.get("layout", "live"))
    print(json.dumps({"record_id": rid, "session_id": sid, "cwd": cwd, "turns": turns},
                     ensure_ascii=False))


if __name__ == "__main__":
    if len(sys.argv) < 3:
        sys.exit(__doc__)
    opts = dict(a.split("=", 1) for a in sys.argv[3:])
    scenario(sys.argv[1], sys.argv[2], opts)
