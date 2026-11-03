#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Independent model of the acpx-journal events, compared with an adapter's batch.

Usage: acpx-journal-expect.py <expect.json> <events.jsonl> <mode> <quiet> <source> [project]

  expect.json   the fixture description printed by acpx-journal-fixtures.py
  events.jsonl  the batch lines the adapter emitted (all records; filtered here
                by session id)
  mode          boundaries | full
  quiet         1 when an incomplete turn is past the quiet window, else 0
  source        live | archive
  project       expected project id (default: acpx-cap-test)

Exit 0 when the emitted events for this session are exactly the modelled set,
field for field; otherwise print each difference and exit 1.
"""
import json
import sys

MAX = 6000
CREATED = "2026-10-01T10:00:00"


def split(eid, etype, text, payload):
    """One event, or a first part plus its :rest continuation."""
    if len(text) <= MAX:
        return [(eid, etype, text, dict(payload))]
    first = dict(payload, text_truncated=True)
    rest = dict(payload, continues_event_id=eid, text_offset=MAX)
    return [(eid, etype, text[:MAX], first), (eid + ":rest", etype, text[MAX:], rest)]


def model(exp, mode, quiet, source):
    sid = exp["session_id"]
    out = []
    for ordinal, t in enumerate(exp["turns"], start=1):
        if not t["complete"] and not quiet:
            continue
        base = "acpx:%s:%s" % (sid, t["request_id"])
        common = {"acpx_record_id": exp["record_id"], "request_id": t["request_id"],
                  "turn_ordinal": ordinal, "source": source}
        out += split(base + ":prompt", "acpx_prompt", t["prompt"],
                     dict(common, prompt_sha256=t["prompt_sha256"]))
        msgs = t["messages"] if not t["complete"] else t["messages"][:-1]
        if mode == "full":
            for n, m in enumerate(msgs, start=1):
                out += split("%s:msg:%d" % (base, n), "acpx_assistant_message", m,
                             dict(common, message_ordinal=n))
        if t["complete"]:
            out += split(base + ":result", "acpx_turn_result", t["final"],
                         dict(common, prompt_sha256=t["prompt_sha256"], status=t["status"],
                              stop_reason=t["stop_reason"], model=t["model"],
                              message_count=len(t["messages"])))
    return out


def main():
    exp = json.load(open(sys.argv[1]))
    lines = [json.loads(l) for l in open(sys.argv[2]) if l.strip()]
    mode, quiet, source = sys.argv[3], sys.argv[4] == "1", sys.argv[5]
    project = sys.argv[6] if len(sys.argv) > 6 else "acpx-cap-test"
    got = [e for e in lines if e.get("session_id") == exp["session_id"]]
    want = model(exp, mode, quiet, source)
    errs = []
    if [e.get("source_event_id") for e in got] != [w[0] for w in want]:
        errs.append("ids/order: got %s want %s" % ([e.get("source_event_id") for e in got],
                                                   [w[0] for w in want]))
    byid = {e.get("source_event_id"): e for e in got}
    for eid, etype, text, payload in want:
        e = byid.get(eid)
        if e is None:
            continue
        for k, v in (("event_type", etype), ("source_adapter", "acpx-journal"),
                     ("session_id", exp["session_id"]), ("cwd", exp["cwd"]),
                     ("project", project)):
            if e.get(k) != v:
                errs.append("%s: %s=%r want %r" % (eid, k, e.get(k), v))
        if e.get("text") != text:
            errs.append("%s: text differs (got %d chars, want %d)" % (eid, len(e.get("text") or ""), len(text)))
        if not str(e.get("occurred_at", "")).startswith(CREATED):
            errs.append("%s: occurred_at=%r want the record created_at" % (eid, e.get("occurred_at")))
        p = e.get("payload") or {}
        for k, v in payload.items():
            if p.get(k) != v:
                errs.append("%s: payload.%s=%r want %r" % (eid, k, p.get(k), v))
        if "text_truncated" not in payload and p.get("text_truncated"):
            errs.append("%s: unexpected text_truncated" % eid)
        if p.get("agent") is None or p.get("name") is None:
            errs.append("%s: payload.agent and payload.name are required" % eid)
        blob = json.dumps(e, ensure_ascii=False)
        if "private reasoning text" in blob or "REPLAYED" in blob:
            errs.append("%s: thought or replayed text leaked into the event" % eid)
    for err in errs:
        print("  " + err)
    sys.exit(1 if errs else 0)


if __name__ == "__main__":
    main()
