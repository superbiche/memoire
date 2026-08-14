#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# turn-ingest-adapter-test.sh — regression test for the codex/qwen
# assistant-turn adapters (turn-ingest campaign Phase 1, slice 2). Pure
# --dry-run over the committed smoke fixtures: no API, no queue writes, no
# provider. The live end-to-end path stays covered by the foreign smokes
# (make codex-rollout-smoke / qwen-rollout-smoke).
#
# Covered: agent_message → codex_assistant_turn and non-thought text parts →
# qwen_assistant_turn; deterministic source_event_ids; thought parts never
# emit; the MEMOIRE_BACKFILL_TEXT_MAX cap truncates the event text and flags
# text_truncated in the payload; the backfill-generation marker rides along.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CODEX_SCRIPT="$ROOT/scripts/memoire-capture-codex-rollout.sh"
QWEN_SCRIPT="$ROOT/scripts/memoire-capture-qwen-rollout.sh"
CODEX_FIXTURE="$ROOT/tests/foreign/fixtures/codex-rollout-sample.jsonl"
QWEN_FIXTURE="$ROOT/tests/foreign/fixtures/qwen-rollout-sample.jsonl"

command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

# --- codex: agent_message becomes codex_assistant_turn -----------------------
codex_out="$("$CODEX_SCRIPT" --dry-run "$CODEX_FIXTURE" 2>/dev/null)"
codex_turns="$(jq -c 'select(.event_type=="codex_assistant_turn")' <<<"$codex_out")"
[[ "$(grep -c . <<<"$codex_turns")" == 1 ]] || fail "codex fixture must yield exactly 1 assistant turn"
grep -q "codexturnneedle" <<<"$codex_turns" || fail "codex turn text missing from dry-run output"
jq -e '.source_event_id | startswith("codex-turn-")' <<<"$codex_turns" >/dev/null \
  || fail "codex turn source_event_id must carry the codex-turn- prefix"
jq -e '.payload.backfill_generation | length > 0' <<<"$codex_turns" >/dev/null \
  || fail "codex turn payload missing backfill_generation"
jq -e '.payload.text_truncated == false' <<<"$codex_turns" >/dev/null \
  || fail "short codex turn must not be flagged truncated"

# Determinism: a second dry-run emits the identical source_event_id.
codex_id1="$(jq -r '.source_event_id' <<<"$codex_turns")"
codex_id2="$("$CODEX_SCRIPT" --dry-run "$CODEX_FIXTURE" 2>/dev/null \
  | jq -r 'select(.event_type=="codex_assistant_turn") | .source_event_id')"
[[ "$codex_id1" == "$codex_id2" ]] || fail "codex turn source_event_id not deterministic"

# Duplicate turns stay distinct: agent_message has no message id, so the
# transcript line number is part of the identity — two identical messages
# with the same timestamp must yield two events with different
# source_event_ids (orthogonal review 2026-07-11, MINOR).
DUP_FIXTURE="$(mktemp)"
trap 'rm -f "$DUP_FIXTURE"' EXIT
{
  jq -c 'select(.type=="session_meta")' "$CODEX_FIXTURE"
  printf '%s\n' '{"timestamp":"2026-07-02T00:10:30.000Z","type":"event_msg","payload":{"type":"agent_message","message":"same turn twice"}}'
  printf '%s\n' '{"timestamp":"2026-07-02T00:10:30.000Z","type":"event_msg","payload":{"type":"agent_message","message":"same turn twice"}}'
} > "$DUP_FIXTURE"
dup_ids="$("$CODEX_SCRIPT" --dry-run "$DUP_FIXTURE" 2>/dev/null \
  | jq -r 'select(.event_type=="codex_assistant_turn") | .source_event_id')"
[[ "$(grep -c . <<<"$dup_ids")" == 2 ]] || fail "duplicate agent_messages must yield two turn events"
[[ "$(sort -u <<<"$dup_ids" | grep -c .)" == 2 ]] || fail "identical duplicate turns must get distinct source_event_ids"

# Truncation: with a tiny cap the text shrinks and the payload says so.
codex_capped="$(MEMOIRE_BACKFILL_TEXT_MAX=14 "$CODEX_SCRIPT" --dry-run "$CODEX_FIXTURE" 2>/dev/null \
  | jq -c 'select(.event_type=="codex_assistant_turn")')"
[[ "$(jq -r '.text' <<<"$codex_capped")" == "codexturnneedl" ]] \
  || fail "codex turn text not capped at MEMOIRE_BACKFILL_TEXT_MAX"
jq -e '.payload.text_truncated == true' <<<"$codex_capped" >/dev/null \
  || fail "capped codex turn must be flagged truncated"

# --- qwen: non-thought text parts become qwen_assistant_turn ------------------
qwen_out="$("$QWEN_SCRIPT" --dry-run "$QWEN_FIXTURE" 2>/dev/null)"
qwen_turns="$(jq -c 'select(.event_type=="qwen_assistant_turn")' <<<"$qwen_out")"
[[ "$(grep -c . <<<"$qwen_turns")" == 1 ]] || fail "qwen fixture must yield exactly 1 assistant turn"
grep -q "qwenturnneedle" <<<"$qwen_turns" || fail "qwen turn text missing from dry-run output"
grep -q "qwenthoughtneedle" <<<"$qwen_turns" && fail "qwen thought part must never emit"
[[ "$(jq -r '.source_event_id' <<<"$qwen_turns")" == "qwen-turn:qwen-smoke-session-20260707:qwen-line-0002" ]] \
  || fail "qwen turn source_event_id must be deterministic per (session, line uuid)"
jq -e '.payload.backfill_generation | length > 0' <<<"$qwen_turns" >/dev/null \
  || fail "qwen turn payload missing backfill_generation"
jq -e '.payload.text_truncated == false' <<<"$qwen_turns" >/dev/null \
  || fail "short qwen turn must not be flagged truncated"

qwen_capped="$(MEMOIRE_BACKFILL_TEXT_MAX=14 "$QWEN_SCRIPT" --dry-run "$QWEN_FIXTURE" 2>/dev/null \
  | jq -c 'select(.event_type=="qwen_assistant_turn")')"
[[ "$(jq -r '.text' <<<"$qwen_capped")" == "qwenturnneedle" ]] \
  || fail "qwen turn text not capped at MEMOIRE_BACKFILL_TEXT_MAX"
jq -e '.payload.text_truncated == true' <<<"$qwen_capped" >/dev/null \
  || fail "capped qwen turn must be flagged truncated"

# --- resolver wiring: session cwd resolves through the registry ---------------
# Proves both rollout adapters run the shared resolver on the session cwd
# (codex fixture cwd = /tmp/codex-smoke-project, qwen = /tmp/qwen-smoke-project)
# rather than taking a bare basename.
REG="$(mktemp --suffix=.json)"
trap 'rm -f "$DUP_FIXTURE" "$REG"' EXIT
cat > "$REG" <<'JSON'
{
  "schema": "liaison-memoire-projects/1",
  "projects": {
    "codex-mapped": { "display": "codex-smoke-project", "paths": ["/tmp/codex-smoke-project"] },
    "qwen-mapped":  { "display": "qwen-smoke-project",  "paths": ["/tmp/qwen-smoke-project"] }
  }
}
JSON

codex_proj="$(MEMOIRE_PROJECTS_REGISTRY="$REG" "$CODEX_SCRIPT" --dry-run "$CODEX_FIXTURE" 2>/dev/null \
  | jq -r 'select(.event_type=="codex_assistant_turn") | .project' | head -1)"
[[ "$codex_proj" == "codex-mapped" ]] \
  || fail "codex adapter must resolve session cwd through the registry (got '$codex_proj')"

qwen_proj="$(MEMOIRE_PROJECTS_REGISTRY="$REG" "$QWEN_SCRIPT" --dry-run "$QWEN_FIXTURE" 2>/dev/null \
  | jq -r 'select(.event_type=="qwen_assistant_turn") | .project' | head -1)"
[[ "$qwen_proj" == "qwen-mapped" ]] \
  || fail "qwen adapter must resolve session cwd through the registry (got '$qwen_proj')"

# --project override still beats the registry on both adapters.
codex_forced="$(MEMOIRE_PROJECTS_REGISTRY="$REG" "$CODEX_SCRIPT" --dry-run --project forced "$CODEX_FIXTURE" 2>/dev/null \
  | jq -r 'select(.event_type=="codex_assistant_turn") | .project' | head -1)"
[[ "$codex_forced" == "forced" ]] || fail "codex --project override must beat the registry"

echo "turn-ingest-adapter-test: OK"
