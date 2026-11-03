#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""TEST-ONLY OpenAI-compatible endpoint for the derive-bench test.

Stands in for a candidate's llama-server so the run driver can be exercised
end to end with no network, no GPU, and no real key. It speaks exactly the two
routes the local-qwen adapter uses — the `GET /v1/models` availability probe
(memoire:D0054, which also enforces model presence) and
`POST /v1/chat/completions` (memoire:D0053) — and enforces the bearer shape so
the secret-by-name path is proven rather than assumed.

Like the standalone smoke's byokstub, the canned envelope is GROUNDED on a real
event id parsed out of the incoming prompt: the strict parser rejects any
candidate whose source_event_ids fall outside the window, so an ungrounded stub
would silently produce a zero-observation run.

The chosen address is the first stdout line, so a caller can read the port.
Every request is then logged as a `REQUEST <method> <path>` line, which is how
the suite proves a NEGATIVE: a window the split refuses must reach no provider
at all, and counting these lines across a run is the only evidence that holds.
"""

import argparse
import json
import re
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

# The placeholder credential the test exports; never a real key.
STUB_KEY = "derive-bench-stub-placeholder-key"
EVENT_ID_RE = re.compile(r"### event (\d+)")

CONFIG = {"model": "bench-model", "observations": 1,
          "content_filter_request": 0, "empty_content_request": 0,
          "canned_response_file": ""}

# Chat completions are counted so a test can make the Nth one refuse. The two
# refusal shapes are NOT interchangeable: the adapter maps a content_filter
# finish reason and an empty answer onto the same `refused` bool, and the
# bench has to be able to tell them apart downstream.
CHAT_REQUESTS = {"n": 0}

# Observation texts must be far apart in TRIGRAM space, not merely distinct:
# the production near-dup gate compares each window's candidates against what
# earlier windows already wrote, and two texts differing only in an id are
# exactly the template-bookkeeping shape it collapses on purpose. A stub whose
# every answer was "...for event N" would therefore yield one surviving
# observation per session no matter how many windows ran, and a multi-window
# aggregation test asserting otherwise would fail against correct behavior.
VOCAB = ["migration", "checkpoint", "quorum", "backpressure", "shard",
         "vacuum", "throttle", "lease", "snapshot", "handshake", "beacon"]


class Handler(BaseHTTPRequestHandler):
    # Silences the per-request access log; the parameter keeps the base class's
    # name (shadowing the builtin) so the override stays substitutable.
    def log_message(self, format, *args):  # noqa: A002 — keep the test output clean
        pass

    def _record(self):
        print(f"REQUEST {self.command} {self.path}", flush=True)

    def _json(self, code, payload):
        body = json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _authorized(self):
        if self.headers.get("Authorization") != "Bearer " + STUB_KEY:
            self._json(401, {"error": {"message": "bad bearer"}})
            return False
        return True

    def do_GET(self):
        self._record()
        if self.path.rstrip("/") != "/v1/models":
            self._json(404, {"error": {"message": "no such route"}})
            return
        if not self._authorized():
            return
        self._json(200, {"object": "list", "data": [{"id": CONFIG["model"]}]})

    def do_POST(self):
        self._record()
        if self.path.rstrip("/") != "/v1/chat/completions":
            self._json(404, {"error": {"message": "no such route"}})
            return
        if not self._authorized():
            return
        CHAT_REQUESTS["n"] += 1
        nth = CHAT_REQUESTS["n"]
        usage = {"prompt_tokens": 100, "completion_tokens": 0, "total_tokens": 100}
        if nth == CONFIG["content_filter_request"]:
            # Refusal branch 1: the model declined. Content is present and
            # would parse; the finish reason alone makes it a refusal.
            self._json(200, {
                "choices": [{"message": {"content": "declined by policy"},
                             "finish_reason": "content_filter"}],
                "usage": usage,
            })
            return
        if nth == CONFIG["empty_content_request"]:
            # Refusal branch 2: the model answered with nothing. Whitespace
            # rather than "" on purpose — the adapter trims before testing, so
            # this exercises that trim instead of assuming it.
            self._json(200, {
                "choices": [{"message": {"content": "   "},
                             "finish_reason": "stop"}],
                "usage": usage,
            })
            return
        length = int(self.headers.get("Content-Length") or 0)
        try:
            body = json.loads(self.rfile.read(length) or b"{}")
        except ValueError:
            self._json(400, {"error": {"message": "body is not JSON"}})
            return
        messages = body.get("messages") or []
        if not messages:
            self._json(400, {"error": {"message": "no messages"}})
            return
        if CONFIG["canned_response_file"]:
            # Specialist mode: answer with a canned upstream body instead of
            # the grounded default. The shim (bench/derive/shim.py) is what is
            # under test then, so the answer must be in the SPECIALIST's own
            # shape — a filled NuExtract template or a MemReader tool call —
            # not in the derive response schema.
            with open(CONFIG["canned_response_file"], encoding="utf-8") as fh:
                canned = json.load(fh)
            self._json(200, canned)
            return
        prompt = messages[0].get("content") or ""
        ids = [int(m) for m in EVENT_ID_RE.findall(prompt)]
        if not ids:
            self._json(400, {"error": {"message": "prompt carried no event blocks"}})
            return
        observations = []
        for i in range(CONFIG["observations"]):
            event_id = ids[i % len(ids)]
            subject = VOCAB[event_id % len(VOCAB)]
            obj = VOCAB[(event_id * 5 + 3) % len(VOCAB)]
            observations.append({
                "kind": "fact",
                "text": f"the {subject} stub answered about {obj} for event {event_id}",
                "confidence": 0.9,
                "source_event_ids": [event_id],
            })
        content = json.dumps({
            "schema_version": "1",
            "observations": observations,
            "unknown_candidates": [],
        })
        self._json(200, {
            "choices": [{"message": {"content": content}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 100, "completion_tokens": 25, "total_tokens": 125},
        })


def main(argv):
    parser = argparse.ArgumentParser(prog="openai-stub.py")
    parser.add_argument("--model", default=CONFIG["model"])
    parser.add_argument("--observations", type=int, default=CONFIG["observations"])
    parser.add_argument("--content-filter-request", type=int, default=0,
                        help="1-based chat-completion ordinal to answer with a "
                             "content_filter refusal (0 = never)")
    parser.add_argument("--empty-content-request", type=int, default=0,
                        help="1-based chat-completion ordinal to answer with "
                             "empty content (0 = never)")
    parser.add_argument("--canned-response-file", default="",
                        help="JSON file holding a whole chat-completion body to "
                             "answer with verbatim; used to stand in for a "
                             "SPECIALIST upstream behind bench/derive/shim.py")
    args = parser.parse_args(argv)
    CONFIG["model"] = args.model
    CONFIG["observations"] = args.observations
    CONFIG["content_filter_request"] = args.content_filter_request
    CONFIG["empty_content_request"] = args.empty_content_request
    CONFIG["canned_response_file"] = args.canned_response_file

    server = HTTPServer(("127.0.0.1", 0), Handler)
    print(f"LISTEN {server.server_address[0]}:{server.server_address[1]}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main(sys.argv[1:])
