#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# Smoke: Codex rollout transcript -> file queue -> tool-window read surface.
# Proves the foreign-harness capture contract end-to-end: paths_touched from
# tool_input.file_path, original occurred-at preserved, failed patches skipped,
# idempotent re-ingest, prompt parity behind --include-prompts.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="${MEMOIRE_BIN:-$ROOT/bin/memoire-api}"
PORT="${MEMOIRE_CODEX_ROLLOUT_PORT:-37791}"
BASE="http://127.0.0.1:$PORT/v1"
HOME_DIR="$(mktemp -d "${TMPDIR:-/tmp}/memoire-codex-rollout.XXXXXX")"
FIXTURE="$ROOT/tests/foreign/fixtures/codex-rollout-sample.jsonl"
SESSION_ID="019f2000-smoke-7000-8000-codexsample1"
PATCH_EPOCH="$(date -u -d "2026-07-02T00:10:00Z" +%s)"
PID=""

cleanup() {
  if [[ -n "${PID:-}" ]] && kill -0 "$PID" 2>/dev/null; then
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  rm -rf "$HOME_DIR"
}
trap cleanup EXIT

fail() {
  echo "Memoire codex rollout smoke failed: $*" >&2
  cat "$HOME_DIR/server.log" >&2 || true
  exit 1
}

wait_ready() {
  for _ in $(seq 1 50); do
    if curl -fsS "$BASE/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  fail "API did not become ready"
}

if [[ ! -x "$BIN" ]]; then
  echo "Binary not found or not executable: $BIN" >&2
  echo "Run: make build" >&2
  exit 1
fi

MEMOIRE_ADDR="127.0.0.1:$PORT" \
  MEMOIRE_HOME="$HOME_DIR" \
  MEMOIRE_DB="$HOME_DIR/memoire.db" \
  MEMOIRE_SCHEMA_DIR="$ROOT/schema" \
  "$BIN" > "$HOME_DIR/server.log" 2>&1 &
PID="$!"
wait_ready

ingest() {
  MEMOIRE_HOME="$HOME_DIR" \
    "$ROOT/scripts/memoire-capture-codex-rollout.sh" --include-prompts "$FIXTURE" >/dev/null 2>&1
}

ingest

TOKEN="$(tr -d '\r\n' < "$HOME_DIR/token")"
window() {
  curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE/events/tool-window?session_id=$SESSION_ID&$1"
}

body=""
for _ in $(seq 1 30); do
  body="$(window "event_type=codex_PatchApply")"
  if [[ "$(jq -r '.total' <<<"$body")" == "3" ]]; then
    break
  fi
  sleep 0.2
done

[[ "$(jq -r '.total' <<<"$body")" == "3" ]] || fail "expected 3 patch events, got: $body"

paths="$(jq -r '[.events[].paths_touched[]] | sort | join(",")' <<<"$body")"
expected="/tmp/codex-smoke/alpha.txt,/tmp/codex-smoke/beta.yml,/tmp/codex-smoke/gamma.yml"
[[ "$paths" == "$expected" ]] || fail "paths_touched mismatch: $paths"

grep -q "should-not-appear" <<<"$body" && fail "failed patch was ingested"

epochs="$(jq -r '[.events[].occurred_at_epoch] | unique | join(",")' <<<"$body")"
[[ "$epochs" == "$PATCH_EPOCH" ]] || fail "occurred_at not preserved from rollout: $epochs != $PATCH_EPOCH"

in_window="$(window "event_type=codex_PatchApply&start=$((PATCH_EPOCH - 60))&end=$((PATCH_EPOCH + 60))" | jq -r '.total')"
[[ "$in_window" == "3" ]] || fail "bounded window should contain 3 events, got $in_window"

out_window="$(window "event_type=codex_PatchApply&end=$((PATCH_EPOCH - 60))" | jq -r '.total')"
[[ "$out_window" == "0" ]] || fail "window before patch time should be empty, got $out_window"

tool_names="$(jq -r '[.events[].tool_name] | unique | join(",")' <<<"$body")"
[[ "$tool_names" == "apply_patch" ]] || fail "tool_name mismatch: $tool_names"

# Idempotency: re-ingesting the same rollout must not create duplicates.
ingest
sleep 1
rerun_total="$(window "event_type=codex_PatchApply" | jq -r '.total')"
[[ "$rerun_total" == "3" ]] || fail "re-ingest not idempotent: $rerun_total events"

prompt_total="$(window "event_type=codex_UserMessage" | jq -r '.total')"
[[ "$prompt_total" == "1" ]] || fail "expected 1 prompt event, got $prompt_total"

curl -fsS -H "Authorization: Bearer $TOKEN" \
  "$BASE/search?q=codexpromptneedle&project=codex-smoke-project&limit=5" \
  | grep -q "codexpromptneedle" || fail "prompt text not search-visible"

# Assistant turns (turn-ingest Phase 1): agent_message becomes
# codex_assistant_turn, always on, idempotent, search-visible.
turn_total="$(window "event_type=codex_assistant_turn" | jq -r '.total')"
[[ "$turn_total" == "1" ]] || fail "expected 1 assistant-turn event, got $turn_total"

curl -fsS -H "Authorization: Bearer $TOKEN" \
  "$BASE/search?q=codexturnneedle&project=codex-smoke-project&limit=5" \
  | grep -q "codexturnneedle" || fail "assistant-turn text not search-visible"

# Honest exit code: a failing queue writer must make the script exit nonzero,
# never report partial capture as success (Codex review finding, 2026-07-02).
FAKE_BIN="$HOME_DIR/failing-queue-bin"
printf '#!/usr/bin/env bash\nexit 1\n' > "$FAKE_BIN"
chmod +x "$FAKE_BIN"
if MEMOIRE_HOME="$HOME_DIR" MEMOIRE_QUEUE_BIN="$FAKE_BIN" \
  "$ROOT/scripts/memoire-capture-codex-rollout.sh" "$FIXTURE" >/dev/null 2>&1; then
  fail "ingester exited 0 despite every queue write failing"
fi

echo "memoire codex rollout capture smoke passed"
