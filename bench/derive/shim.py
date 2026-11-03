#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Bench-local prompt-adapter shim for extraction/memory SPECIALIST models.

The derive bench compares candidates through memoire's own derivation path, so
a candidate is just an endpoint-registry entry. That works for any model that
speaks the derive prompt. It does not work for specialists: NuExtract3 wants a
JSON template plus a document, and MemReader wants a system prompt with tools
and answers in tool_calls. Neither reads instructions.

This shim is the translation layer, and it is deliberately BENCH-LOCAL:
memoire's derive path stays byte-identical and simply points at the shim as
its endpoint base_url. If a specialist wins the bench, the production
prompt-shape contract gets designed THEN, with evidence — not guessed now.

  python3 bench/derive/shim.py --mode nuextract|memreader \\
    --listen 127.0.0.1:8001 --upstream http://127.0.0.1:8002 [--log FILE]

Fleet-law note (llm-calls): the shim is TRANSPORT MIDDLEWARE sitting on
memoire's own provider call. Memoire remains the LLM caller; the shim never
initiates a call of its own, and it holds no credential — the incoming
Authorization header is forwarded verbatim and never logged.

Honesty rules, in priority order:

1. An upstream error passes through with its status and body unchanged, so
   the local-qwen adapter's error mapping stays true (memoire:D0053/D0057).
2. A well-formed upstream response the shim cannot translate passes through
   RAW. The strict parser downstream then records a malformed run honestly —
   capture, never mask (memoire:D0057).
3. The shim never fabricates an observation, and never returns empty content:
   an empty extraction becomes a valid abstention envelope, because empty
   content is classified as a REFUSAL by the adapter.
"""

import argparse
import json
import re
import sys
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# The sealed kind set (services/api/internal/modelderive/types.go SealedKinds).
# Duplicated here because the shim cannot call Go; tests/derive-bench-test.sh
# reads SealedKinds() out of that file and fails on drift — a kind added to
# the ontology but missing from a specialist's template or system prompt would
# silently make that kind unextractable for the whole bench.
SEALED_KINDS = ("decision", "preference", "constraint", "task", "fact",
                "open_question", "gotcha")

# Everything the shim reads out of the derive prompt is marker-based. The
# markers are builder.go's own headings (buildPrompt: "## Source Window",
# "## Source Events", writeEvent: "### event %d").
#
# Every marker here is matched ANCHORED at line start and in SEQUENCE, never
# by substring search: writeEvent embeds event bodies verbatim (builder.go
# 393-416) and renderAlreadyCaptured embeds observation texts verbatim, so any
# single marker line is forgeable by captured content. A whole rendered
# sequence is not.
SOURCE_WINDOW_HEADING = "## Source Window"
SOURCE_EVENTS_HEADING = "## Source Events"
EVENT_ID_RE = re.compile(r"^### event (\d+)$")
# writeEvent (builder.go:380-381) emits the header and this line as a pair,
# with nothing between them: the pair is what tells a real event header from a
# body line that merely looks like one.
OCCURRED_AT_PREFIX = "- occurred_at_epoch: "
# The metadata keys writeEvent emits before any verbatim payload line
# (builder.go:381-390). Scanning for `- source_event_id:` stops at the first
# key outside this set, so a payload body cannot inject a fake mapping.
EVENT_HEAD_KEYS = ("occurred_at_epoch", "event_type", "source_adapter",
                   "source_event_id", "project", "session_id")
FENCE_RE = re.compile(r"^\s*```(?:json)?\s*(.*?)\s*```\s*$", re.S)
THINK_RE = re.compile(r"<think>.*?</think>", re.S)
TOOL_CALL_RE = re.compile(r"<tool_call>\s*(.*?)\s*</tool_call>", re.S)

# NuExtract3 is a TEMPLATE extractor: the template is the schema, and the
# model fills it. A list-of-lists value is NuExtract's multi-enum notation —
# the model answers with a one-element list drawn from the options.
NUEXTRACT_TEMPLATE = {
    "observations": [{
        "kind": [list(SEALED_KINDS)],
        "text": "string",
        "rationale": "string",
        "source_event_ids": ["verbatim-string"],
    }]
}

MEMREADER_SYSTEM = (
    "You are a memory extraction engine reading a developer session window. "
    "Store every durable, grounded memory the window supports by calling "
    "add_memory. Each stored memory MUST carry, as its FIRST tag, exactly one "
    "of these kinds: " + ", ".join(SEALED_KINDS) + ". Each memory MUST also "
    "carry source_event_ids: the numeric ids of the `### event <id>` blocks "
    "the memory is grounded in. Store nothing the window does not support; if "
    "the window carries no durable memory, call ignore_memory."
)

MEMREADER_TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "add_memory",
            "description": "Store durable memories extracted from the window.",
            "parameters": {
                "type": "object",
                "properties": {
                    "memory_list": {
                        "type": "array",
                        "items": {
                            "type": "object",
                            "properties": {
                                "key": {"type": "string"},
                                "memory_type": {
                                    "type": "string",
                                    "enum": ["LongTermMemory", "UserMemory"],
                                },
                                "value": {"type": "string"},
                                "tags": {"type": "array", "items": {"type": "string"}},
                                "source_event_ids": {
                                    "type": "array", "items": {"type": "string"},
                                },
                            },
                            "required": ["key", "value", "tags", "source_event_ids"],
                        },
                    },
                    "summary": {"type": "string"},
                },
                "required": ["memory_list"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "search_memory",
            "description": "Search existing memories.",
            "parameters": {
                "type": "object",
                "properties": {"query": {"type": "string"}},
                "required": ["query"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "buffer_memory",
            "description": "Defer: the window is incomplete on its own.",
            "parameters": {
                "type": "object",
                "properties": {"reason": {"type": "string"}},
                "required": ["reason"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "ignore_memory",
            "description": "The window carries no durable memory.",
            "parameters": {
                "type": "object",
                "properties": {"reason": {"type": "string"}},
                "required": ["reason"],
            },
        },
    },
]

# Specialists emit no confidence. A CONSTANT is the honest encoding: it says
# "this model does not self-report", and it sits at the mid-band the contract
# defines as plausible-but-ambiguous, above the 0.4 drop floor
# (memoire:D0021). It is not a quality signal and must not be read as one.
SPECIALIST_CONFIDENCE = 0.5

# Emitted at observation level when the model grounded nothing and the shim
# fell back to the whole window. It lands in model_response_extra by the
# lenient-capture rule (memoire:D0057) — visible in the pack and auditable,
# never silent.
FALLBACK_NOTE = "source_refs_fallback"


class ShimError(Exception):
    """A translation failure. The caller passes the upstream body through."""


def tool_choice_rejected(body_bytes):
    """True only when a 400 body explicitly blames tool_choice.

    `tool_choice` is the one request field a server may simply not support,
    and that 400 is worth one retry without it. EVERY OTHER 400 is a real
    upstream verdict — a rejected profile, a bad parameter, a model that is
    not loaded — and retrying it would translate the second answer and hide
    the failure the bench exists to record.

    The signal must be EXPLICIT: llama.cpp reports the offending field by name
    in {"error":{"message":..., "type":..., "code":...}}, so any string field
    of the error object naming `tool_choice` counts, and nothing else does. A
    body that says "tool choice" without the underscore is prose, not a field
    name, and reads as False.
    """
    try:
        parsed = json.loads(body_bytes or b"")
    except (ValueError, TypeError):
        return False
    if not isinstance(parsed, dict):
        return False
    error = parsed.get("error")
    if not isinstance(error, dict):
        return False
    return any("tool_choice" in value
               for value in error.values() if isinstance(value, str))


def _next_nonblank(lines, start):
    for i in range(start, len(lines)):
        if lines[i].strip():
            return i
    return None


def _anchor_source_events(lines):
    """Find the builder's rendered window+events sequence.

    buildPrompt (builder.go:364-368) renders three things as a unit and
    unconditionally: `## Source Window`, the `project=` window line, and
    `## Source Events`. Matching that triple — rather than searching for the
    events heading on its own — is what stops an already-captured observation
    whose TEXT quotes a heading literal from hijacking the section start.

    Returns (window_line, index of the first line after the events heading),
    or None when the prompt is not a derive prompt.
    """
    for i, line in enumerate(lines):
        if line.rstrip() != SOURCE_WINDOW_HEADING:
            continue
        window_at = _next_nonblank(lines, i + 1)
        if window_at is None or not lines[window_at].startswith("project="):
            continue
        events_at = _next_nonblank(lines, window_at + 1)
        if events_at is None or lines[events_at].rstrip() != SOURCE_EVENTS_HEADING:
            continue
        return lines[window_at].strip(), events_at + 1
    return None


def classify_source_strings(pairs):
    """Split (source_event_id, numeric id) pairs into unique map + ambiguous.

    A string carried by two DIFFERENT events names neither of them: resolving
    it to one would fabricate precise provenance the window does not support.
    memoire's own parser rejects such a claim as ungrounded rather than
    picking a winner (parser.go:263-269), and the shim is no more lenient —
    an ambiguous string resolves NOWHERE, exactly like an unknown one.

    The same string repeated under the SAME event is one distinct id, so it
    stays unique: builder.go can render the same event only once.
    """
    seen = {}
    for text, numeric in pairs:
        seen.setdefault(text, []).append(numeric)
    unique = {}
    ambiguous = set()
    for text, numerics in seen.items():
        if len(set(numerics)) == 1:
            unique[text] = numerics[0]
        else:
            ambiguous.add(text)
    return unique, ambiguous


def _scan_events(lines):
    """Walk the events section: (numeric ids, [(source_event_id, numeric id)]).

    A `### event <id>` line counts only when the NEXT line is the
    occurred_at_epoch writeEvent always pairs with it — event bodies are
    embedded verbatim, so a prompt or an assistant turn quoting a header would
    otherwise mint a phantom id.

    The string pairs exist because local models cite the `source_event_id`
    STRINGS rather than the numeric header ids, despite the prompt asking for
    the numbers (builder.go:350-355). Memoire's own parser maps exact string
    matches for the incumbent path; the shim extends the same leniency, or the
    bench's grounding axis measures the shim instead of the model. The pairs
    are returned RAW — classify_source_strings decides which of them name one
    event unambiguously.
    """
    ids = []
    pairs = []
    for i, line in enumerate(lines):
        match = EVENT_ID_RE.match(line.rstrip())
        if not match:
            continue
        if i + 1 >= len(lines) or not lines[i + 1].startswith(OCCURRED_AT_PREFIX):
            continue
        event_id = match.group(1)
        if event_id not in ids:
            ids.append(event_id)
        for meta in lines[i + 1:]:
            key, sep, value = meta.partition(": ")
            key = key[2:] if key.startswith("- ") else ""
            if not sep or key not in EVENT_HEAD_KEYS:
                break
            if key == "source_event_id" and value.strip():
                pairs.append((value.strip(), event_id))
    return ids, pairs


def extract_sections(prompt):
    """Split the derive prompt into (window_line, events_text, ids, id_map).

    Marker-based on builder.go's headings. Everything else — the prose
    instructions, the sealed-kind catalogue, the output schema, the
    already-captured section — is DROPPED: specialists do not read
    instructions, and the write-time near-dup gate handles duplicates.

    `## Source Events` is the LAST thing buildPrompt writes (it returns right
    after the event loop), so the section runs to the END of the prompt: a
    body line starting `## ` is captured content, not a new section.

    `id_map` carries only the source_event_id strings that name ONE event;
    a string shared by several events is ambiguous and is left out.
    """
    lines = (prompt if isinstance(prompt, str) else "").splitlines()
    anchored = _anchor_source_events(lines)
    if anchored is None:
        raise ShimError("prompt carries no anchored '## Source Events' section")
    window_line, start = anchored
    body_lines = lines[start:]
    event_ids, pairs = _scan_events(body_lines)
    id_map, _ambiguous = classify_source_strings(pairs)
    return window_line, "\n".join(body_lines).strip("\n"), event_ids, id_map


def document_text(window_line, events_text):
    """The document a specialist reads: the window line plus the events."""
    if window_line:
        return f"{window_line}\n\n{events_text}"
    return events_text


def strip_fence(text):
    """Unwrap one Markdown code fence, if the whole answer is fenced."""
    m = FENCE_RE.match(text or "")
    return m.group(1) if m else (text or "")


def ground_ids(claimed, window_ids, id_map=None):
    """Keep the claimed ids that name a window event; else fall back to all.

    An id grounds in EITHER namespace the event block shows: the numeric
    header id, or the `source_event_id` string, which is what local models
    actually cite. Strings resolve to their numeric id and dedupe against it,
    so a claim of ["94101", "u-1"] is one event, not two. Only UNIQUE strings
    resolve — `id_map` never carries an ambiguous one, so a string shared by
    several events grounds nothing, as it does in memoire's own parser.

    In-window validation is memoire's own job (the parser rejects out-of-window
    ids), so this only avoids feeding it garbage. The fallback is deliberately
    generous AND marked: a model that grounded nothing gets its observations
    judged on the grounding axis honestly rather than dropped silently.
    """
    id_map = id_map or {}
    keep = []
    for raw in claimed or []:
        text = str(raw).strip()
        numeric = text if text in window_ids else id_map.get(text, "")
        if numeric in window_ids and numeric not in keep:
            keep.append(numeric)
    if keep:
        return [int(i) for i in keep], False
    return [int(i) for i in window_ids], True


def observation(kind, text, rationale, ids, fell_back):
    obs = {
        "kind": kind,
        "text": text,
        "confidence": SPECIALIST_CONFIDENCE,
        "source_event_ids": ids,
        "rationale": rationale,
        "provenance": "inferred",
    }
    if fell_back:
        obs["adapter_note"] = FALLBACK_NOTE
    return obs


def envelope(observations):
    """The sealed derive response schema, as a JSON string."""
    return json.dumps({"schema_version": "1", "observations": observations})


# --- mode: nuextract --------------------------------------------------------

def nuextract_request(incoming, window_line, events_text):
    return {
        "model": incoming.get("model"),
        "messages": [{"role": "user",
                      "content": document_text(window_line, events_text)}],
        # llama.cpp (build 10396) accepts per-request chat_template_kwargs;
        # the template IS the schema for a NuExtract model. If a server
        # refuses the field, its 400 passes straight through to the adapter
        # and the launch flags carry the template instead.
        "chat_template_kwargs": {
            "template": json.dumps(NUEXTRACT_TEMPLATE),
            "enable_thinking": False,
        },
        "temperature": 0.2,
        "max_tokens": incoming.get("max_tokens"),
        "stream": False,
    }


def nuextract_translate(content, window_ids, id_map=None):
    """NuExtract's filled template -> the sealed envelope."""
    try:
        parsed = json.loads(strip_fence(content))
    except ValueError as err:
        raise ShimError(f"upstream content is not JSON: {err}")
    if not isinstance(parsed, dict):
        raise ShimError("upstream content is not a JSON object")
    raw = parsed.get("observations")
    if raw is None:
        raise ShimError("upstream content carries no observations key")
    if not isinstance(raw, list):
        raise ShimError("observations is not a list")

    out = []
    for item in raw:
        if not isinstance(item, dict):
            continue
        text = (item.get("text") or "").strip() if isinstance(item.get("text"), str) else ""
        kind = item.get("kind")
        # Multi-enum notation: the model answers with a one-element list.
        while isinstance(kind, list):
            kind = kind[0] if kind else None
        kind = (kind or "").strip() if isinstance(kind, str) else ""
        # An unfilled template slot is a NON-observation, not a malformation:
        # NuExtract leaves nulls where it found nothing.
        if not text or not kind:
            continue
        if kind not in SEALED_KINDS:
            continue
        rationale = item.get("rationale")
        rationale = rationale.strip() if isinstance(rationale, str) else ""
        ids, fell_back = ground_ids(item.get("source_event_ids"), window_ids, id_map)
        out.append(observation(kind, text, rationale, ids, fell_back))
    return envelope(out)


# --- mode: memreader --------------------------------------------------------

def memreader_request(incoming, window_line, events_text, tool_choice=True):
    body = {
        "model": incoming.get("model"),
        "messages": [
            {"role": "system", "content": MEMREADER_SYSTEM},
            {"role": "user",
             "content": document_text(window_line, events_text)},
        ],
        "tools": MEMREADER_TOOLS,
        "temperature": 0.2,
        # Deliberately NOT clamped to the model card's 1024: derive windows
        # need room, and the endpoint profile already bounds this.
        "max_tokens": incoming.get("max_tokens"),
        "stream": False,
    }
    if tool_choice:
        body["tool_choice"] = "required"
    return body


def collect_tool_calls(message):
    """Every add_memory-shaped call, from both transports.

    llama.cpp with --jinja parses tool calls into message.tool_calls, but a
    thinking model can also leave raw <tool_call> blocks in content. Cover
    both: which one arrives is a serving-config detail the bench must not
    depend on.
    """
    calls = []
    for call in (message.get("tool_calls") or []):
        fn = (call or {}).get("function") or {}
        name = fn.get("name") or ""
        args = fn.get("arguments")
        if isinstance(args, str):
            try:
                args = json.loads(args)
            except ValueError:
                continue
        if isinstance(args, dict):
            calls.append((name, args))

    content = message.get("content") or ""
    for blob in TOOL_CALL_RE.findall(THINK_RE.sub("", content)):
        try:
            call = json.loads(blob)
        except ValueError:
            continue
        if not isinstance(call, dict):
            continue
        name = call.get("name") or ""
        args = call.get("arguments")
        if isinstance(args, str):
            try:
                args = json.loads(args)
            except ValueError:
                continue
        if isinstance(args, dict):
            calls.append((name, args))
    return calls


def memreader_translate(message, window_ids, id_map=None):
    """MemReader's tool calls -> the sealed envelope.

    add_memory contributes one observation per memory_list entry. The other
    three natives (search/buffer/ignore) contribute nothing — an
    ignore-only answer is a real abstention, and the empty envelope says so.
    """
    calls = collect_tool_calls(message)
    if not calls:
        raise ShimError("upstream message carries no parseable tool call")

    out = []
    saw_add = False
    for name, args in calls:
        if name != "add_memory":
            continue
        saw_add = True
        for entry in (args.get("memory_list") or []):
            if not isinstance(entry, dict):
                continue
            text = entry.get("value")
            text = text.strip() if isinstance(text, str) else ""
            if not text:
                continue
            tags = [t for t in (entry.get("tags") or []) if isinstance(t, str)]
            # The kind is the FIRST sealed tag, as the system prompt asks.
            # A model that tagged something else still produced a memory:
            # "fact" is the neutral kind, and mis-kinding is a quality
            # signal the bench should measure, not a reason to drop it.
            kind = next((t for t in tags if t in SEALED_KINDS), "fact")
            key = entry.get("key")
            rationale = key.strip() if isinstance(key, str) else ""
            ids, fell_back = ground_ids(entry.get("source_event_ids"), window_ids, id_map)
            out.append(observation(kind, text, rationale, ids, fell_back))
    if not saw_add:
        # search/buffer/ignore only: a deliberate no-store verdict.
        return envelope([])
    return envelope(out)


# --- proxy ------------------------------------------------------------------

def translate_response(mode, upstream_json, window_ids, id_map=None):
    """Rewrite an upstream chat response into a sealed-envelope response.

    Raises ShimError when the answer cannot be translated. An upstream JSON
    body of an unexpected SHAPE raises whatever indexing into it raises — the
    caller treats any exception the same way, and passes the upstream body
    through untouched.
    """
    choices = upstream_json.get("choices") or []
    if not choices:
        raise ShimError("upstream response carries no choices")
    message = (choices[0] or {}).get("message") or {}
    if mode == "nuextract":
        content = message.get("content") or ""
        if not content.strip():
            raise ShimError("upstream content is empty")
        rewritten = nuextract_translate(content, window_ids, id_map)
    else:
        rewritten = memreader_translate(message, window_ids, id_map)
    return {
        "id": upstream_json.get("id", "shim"),
        "object": "chat.completion",
        "model": upstream_json.get("model"),
        "choices": [{
            "index": 0,
            "message": {"role": "assistant", "content": rewritten},
            "finish_reason": "stop",
        }],
        "usage": upstream_json.get("usage") or {},
    }


class Shim(BaseHTTPRequestHandler):
    mode = "nuextract"
    upstream = "http://127.0.0.1:8002"
    log_path = None

    def log_message(self, format, *args):  # noqa: A002 — keep stdout clean
        pass

    def note(self, line):
        """Append one shim log line. NEVER carries the bearer or a payload."""
        if not self.log_path:
            return
        with open(self.log_path, "a", encoding="utf-8") as fh:
            fh.write(line.rstrip("\n") + "\n")

    def _send(self, code, body, content_type="application/json"):
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _forward(self, path, payload):
        """One upstream call. Returns (status, body_bytes, content_type).

        The Authorization header rides verbatim and is never logged. An HTTP
        error is a RESULT here, not an exception: its status and body go back
        to the adapter unchanged.
        """
        req = urllib.request.Request(self.upstream.rstrip("/") + path, method="GET")
        if payload is not None:
            req = urllib.request.Request(
                self.upstream.rstrip("/") + path,
                data=json.dumps(payload).encode("utf-8"),
                headers={"Content-Type": "application/json"},
                method="POST",
            )
        auth = self.headers.get("Authorization")
        if auth:
            req.add_header("Authorization", auth)
        try:
            with urllib.request.urlopen(req) as resp:
                return resp.status, resp.read(), resp.headers.get("Content-Type", "application/json")
        except urllib.error.HTTPError as err:
            return err.code, err.read(), err.headers.get("Content-Type", "application/json")
        except urllib.error.URLError as err:
            self.note(f"upstream unreachable path={path} reason={err.reason}")
            return 502, json.dumps(
                {"error": {"message": f"shim: upstream unreachable: {err.reason}"}}
            ).encode("utf-8"), "application/json"

    def do_GET(self):
        if self.path.rstrip("/") != "/v1/models":
            self._send(404, b'{"error":{"message":"shim: no such route"}}')
            return
        # Availability probe (memoire:D0054): proxied verbatim, so the model
        # allowlist the adapter checks is the UPSTREAM's truth.
        status, body, ctype = self._forward("/v1/models", None)
        self._send(status, body, ctype)

    def do_POST(self):
        if self.path.rstrip("/") != "/v1/chat/completions":
            self._send(404, b'{"error":{"message":"shim: no such route"}}')
            return
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            incoming = json.loads(raw or b"{}")
        except ValueError:
            self._send(400, b'{"error":{"message":"shim: body is not JSON"}}')
            return

        messages = incoming.get("messages") or []
        prompt = ""
        for msg in messages:
            if (msg or {}).get("role") == "user":
                prompt = msg.get("content") or ""
                break
        try:
            window_line, events_text, window_ids, id_map = extract_sections(prompt)
        except ShimError as err:
            self.note(f"prompt not translatable: {err}")
            self._send(400, json.dumps(
                {"error": {"message": f"shim: {err}"}}).encode("utf-8"))
            return

        if self.mode == "nuextract":
            payload = nuextract_request(incoming, window_line, events_text)
            status, body, ctype = self._forward("/v1/chat/completions", payload)
        else:
            payload = memreader_request(incoming, window_line, events_text)
            status, body, ctype = self._forward("/v1/chat/completions", payload)
            if status == 400 and tool_choice_rejected(body):
                # tool_choice is the one field a server may simply not
                # support, and a 400 on it would fail EVERY leg of the run.
                # Retry once without it: the tools are still offered, only
                # the obligation to call one is dropped. A second 400 is a
                # real error and passes through untouched.
                #
                # The gate is what keeps this honest: a 400 that does NOT name
                # tool_choice is a real upstream verdict, and its FIRST result
                # stands — retrying it would translate the second answer and
                # bury a profile or parameter failure the bench must record.
                self.note("upstream rejected tool_choice (400) — retrying without it")
                payload = memreader_request(incoming, window_line, events_text,
                                            tool_choice=False)
                status, body, ctype = self._forward("/v1/chat/completions", payload)

        if status != 200:
            # Upstream errors pass through untouched: the adapter's failure
            # mapping (429/503 vs other non-2xx) must stay honest.
            self.note(f"upstream status={status} passed through events={len(window_ids)}")
            self._send(status, body, ctype)
            return
        try:
            upstream_json = json.loads(body)
        except ValueError:
            self.note("upstream 200 body was not JSON — passed through")
            self._send(status, body, ctype)
            return
        try:
            rewritten = translate_response(self.mode, upstream_json, window_ids, id_map)
            count = len(json.loads(
                rewritten["choices"][0]["message"]["content"])["observations"])
        except Exception as err:  # noqa: BLE001 — deliberate, see below
            # Well-formed answer the shim cannot translate: pass it through
            # RAW so the strict parser records the malformation honestly.
            # ANY failure counts, not just ShimError: a JSON-valid body of an
            # unexpected SHAPE used to raise out of do_POST and reset the
            # connection, which turns a 200 the adapter should have recorded
            # into a transport error. Only a ShimError's message is logged —
            # it is the shim's own text; another exception's could quote the
            # body.
            detail = str(err) if isinstance(err, ShimError) else type(err).__name__
            self.note(f"translation failed ({detail}) — upstream body passed "
                      f"through bytes={len(body)}")
            self._send(status, body, ctype)
            return
        self.note(f"translated mode={self.mode} events={len(window_ids)} observations={count}")
        self._send(200, json.dumps(rewritten).encode("utf-8"))


def main(argv):
    parser = argparse.ArgumentParser(prog="shim.py")
    parser.add_argument("--mode", required=True, choices=["nuextract", "memreader"])
    parser.add_argument("--listen", default="127.0.0.1:8001",
                        help="loopback bind address; port 0 picks a free one")
    parser.add_argument("--upstream", default="http://127.0.0.1:8002",
                        help="the specialist's own OpenAI-compatible server")
    parser.add_argument("--log", help="append one line per request (never secrets)")
    args = parser.parse_args(argv)

    host, _, port = args.listen.rpartition(":")
    host = host or "127.0.0.1"
    if host not in ("127.0.0.1", "localhost", "::1"):
        # The shim forwards a bearer token. Binding it off-loopback would put
        # a credential-forwarding proxy on the network.
        parser.error("--listen must bind loopback (127.0.0.1, localhost, ::1)")

    Shim.mode = args.mode
    Shim.upstream = args.upstream
    Shim.log_path = args.log

    server = ThreadingHTTPServer((host, int(port)), Shim)
    print(f"LISTEN {server.server_address[0]}:{server.server_address[1]}", flush=True)
    print(f"MODE {args.mode} UPSTREAM {args.upstream}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main(sys.argv[1:])
