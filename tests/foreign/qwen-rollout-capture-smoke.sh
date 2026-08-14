#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# Smoke: qwen-code transcript -> file queue -> tool-window read surface.
# The cage stub must fail this with 0 qwen_FileEdit events until the parser is
# implemented. The assertions mirror the Codex rollout smoke, but use qwen's
# successful functionCall + tool_result join as the mutation success boundary.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="${MEMOIRE_BIN:-$ROOT/bin/memoire-api}"
PORT="${MEMOIRE_QWEN_ROLLOUT_PORT:-37792}"
BASE="http://127.0.0.1:$PORT/v1"
HOME_DIR="$(mktemp -d "${TMPDIR:-/tmp}/memoire-qwen-rollout.XXXXXX")"
FIXTURE="$ROOT/tests/foreign/fixtures/qwen-rollout-sample.jsonl"
CORRUPT_FIXTURE="$ROOT/tests/foreign/fixtures/qwen-rollout-corrupt-after-first-line.jsonl"
SESSION_ID="qwen-smoke-session-20260707"
MUTATION_EPOCH="$(date -u -d "2026-07-06T22:50:10Z" +%s)"
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
  echo "Memoire qwen rollout smoke failed: $*" >&2
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
  echo "Run: command make build" >&2
  exit 1
fi

assert_capture_dependency_allowlist() {
  local script="$ROOT/scripts/memoire-capture-qwen-rollout.sh"
  local forbidden
  forbidden="$(grep -nE '(^[[:space:]]*|[|;&][[:space:]]*|[$][(][[:space:]]*|< <[(][[:space:]]*)(sort|mktemp|rm|awk|head|basename|sed|cut|tail|xargs|python|python3|perl|node|ruby|go|date|tr|wc|tee|readlink|realpath)([[:space:]|;&)<>]|$)' "$script" || true)"
  if [[ -n "$forbidden" ]]; then
    fail "capture script uses commands outside allowlist: $forbidden"
  fi
}

MEMOIRE_ADDR="127.0.0.1:$PORT" \
  MEMOIRE_HOME="$HOME_DIR" \
  MEMOIRE_DB="$HOME_DIR/memoire.db" \
  MEMOIRE_SCHEMA_DIR="$ROOT/schema" \
  "$BIN" > "$HOME_DIR/server.log" 2>&1 &
PID="$!"
wait_ready

ingest() {
  MEMOIRE_HOME="$HOME_DIR" \
    "$ROOT/scripts/memoire-capture-qwen-rollout.sh" --include-prompts "$FIXTURE" >/dev/null 2>&1
}

TOKEN="$(tr -d '\r\n' < "$HOME_DIR/token")"
window() {
  curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE/events/tool-window?session_id=$SESSION_ID&$1"
}

CORRUPT_STDOUT="$HOME_DIR/corrupt-ingest.out"
CORRUPT_STDERR="$HOME_DIR/corrupt-ingest.err"
if MEMOIRE_HOME="$HOME_DIR" \
  "$ROOT/scripts/memoire-capture-qwen-rollout.sh" --include-prompts "$FIXTURE" "$CORRUPT_FIXTURE" \
  > "$CORRUPT_STDOUT" 2> "$CORRUPT_STDERR"; then
  fail "corrupt transcript ingest exited 0"
fi
# 4 events: 2 file edits + 1 assistant turn + 1 prompt (--include-prompts).
grep -q "memoire-capture-qwen-rollout: done: files=1 failed_files=1 events=4 failed_events=0" \
  "$CORRUPT_STDERR" || fail "corrupt transcript summary mismatch: $(cat "$CORRUPT_STDERR")"

body=""
for _ in $(seq 1 30); do
  body="$(window "event_type=qwen_FileEdit")"
  if [[ "$(jq -r '.total' <<<"$body")" == "2" ]]; then
    break
  fi
  sleep 0.2
done

[[ "$(jq -r '.total' <<<"$body")" == "2" ]] || fail "expected 2 qwen_FileEdit events, got: $body"

grep -q "must not be queued" <<<"$body" && fail "payload leaked file content"

search_leak="$(curl -fsS -H "Authorization: Bearer $TOKEN" \
  "$BASE/search?q=must%20not%20be%20queued&project=qwen-smoke-project&limit=5")"
grep -q "must not be queued" <<<"$search_leak" && fail "search leaked file content"

paths="$(jq -r '[.events[].paths_touched[]] | sort | join(",")' <<<"$body")"
expected="/tmp/qwen-smoke/alpha.txt,/tmp/qwen-smoke/beta.md"
[[ "$paths" == "$expected" ]] || fail "paths_touched mismatch: $paths"

grep -q "should-not-appear" <<<"$body" && fail "failed edit was ingested"

epochs="$(jq -r '[.events[].occurred_at_epoch] | unique | join(",")' <<<"$body")"
[[ "$epochs" == "$MUTATION_EPOCH" ]] || fail "occurred_at not preserved from transcript: $epochs != $MUTATION_EPOCH"

in_window="$(window "event_type=qwen_FileEdit&start=$((MUTATION_EPOCH - 60))&end=$((MUTATION_EPOCH + 60))" | jq -r '.total')"
[[ "$in_window" == "2" ]] || fail "bounded window should contain 2 events, got $in_window"

out_window="$(window "event_type=qwen_FileEdit&end=$((MUTATION_EPOCH - 60))" | jq -r '.total')"
[[ "$out_window" == "0" ]] || fail "window before mutation time should be empty, got $out_window"

tool_names="$(jq -r '[.events[].tool_name] | unique | join(",")' <<<"$body")"
[[ "$tool_names" == "edit,write_file" ]] || fail "tool_name mismatch: $tool_names"

change_types="$(jq -r '[.events[].tool_input.change_type] | sort | join(",")' <<<"$body")"
[[ "$change_types" == "update,write" ]] || fail "change_type mismatch: $change_types"

# Idempotency: re-ingesting the same transcript must not create duplicates.
ingest
sleep 1
rerun_total="$(window "event_type=qwen_FileEdit" | jq -r '.total')"
[[ "$rerun_total" == "2" ]] || fail "re-ingest not idempotent: $rerun_total events"

prompt_total="$(window "event_type=qwen_UserMessage" | jq -r '.total')"
[[ "$prompt_total" == "1" ]] || fail "expected 1 prompt event, got $prompt_total"

curl -fsS -H "Authorization: Bearer $TOKEN" \
  "$BASE/search?q=qwenpromptneedle&project=qwen-smoke-project&limit=5" \
  | grep -q "qwenpromptneedle" || fail "prompt text not search-visible"

# Assistant turns (turn-ingest Phase 1): visible text parts become one
# qwen_assistant_turn, always on and idempotent; thought parts never ingest.
turn_total="$(window "event_type=qwen_assistant_turn" | jq -r '.total')"
[[ "$turn_total" == "1" ]] || fail "expected 1 assistant-turn event, got $turn_total"

curl -fsS -H "Authorization: Bearer $TOKEN" \
  "$BASE/search?q=qwenturnneedle&project=qwen-smoke-project&limit=5" \
  | grep -q "qwenturnneedle" || fail "assistant-turn text not search-visible"

thought_leak="$(curl -fsS -H "Authorization: Bearer $TOKEN" \
  "$BASE/search?q=qwenthoughtneedle&project=qwen-smoke-project&limit=5")"
grep -q "qwenthoughtneedle" <<<"$thought_leak" && fail "thought part leaked into capture"

dry_run="$(MEMOIRE_HOME="$HOME_DIR" "$ROOT/scripts/memoire-capture-qwen-rollout.sh" --dry-run --include-prompts "$FIXTURE")"
grep -q '"source_event_id":"qwen-fileedit-' <<<"$dry_run" || fail "dry-run missing qwen-fileedit source_event_id prefix"
grep -q '"source_event_id":"qwen-usermessage-' <<<"$dry_run" || fail "dry-run missing qwen-usermessage source_event_id prefix"
grep -q '"source_event_id":"qwen-turn:qwen-smoke-session-20260707:qwen-line-0002"' <<<"$dry_run" \
  || fail "dry-run missing deterministic qwen-turn source_event_id"

# Honest exit code: a failing queue writer must make the script exit nonzero,
# never report partial capture as success.
FAKE_BIN="$HOME_DIR/failing-queue-bin"
printf '#!/usr/bin/env bash\nexit 1\n' > "$FAKE_BIN"
chmod +x "$FAKE_BIN"
if MEMOIRE_HOME="$HOME_DIR" MEMOIRE_QUEUE_BIN="$FAKE_BIN" \
  "$ROOT/scripts/memoire-capture-qwen-rollout.sh" "$FIXTURE" \
  > "$HOME_DIR/failing-queue.out" 2> "$HOME_DIR/failing-queue.err"; then
  fail "ingester exited 0 despite every queue write failing"
fi
grep -q "memoire-capture-qwen-rollout: queue write failed for /tmp/qwen-smoke/alpha.txt" \
  "$HOME_DIR/failing-queue.err" || fail "missing alpha queue-failure log"
grep -q "memoire-capture-qwen-rollout: queue write failed for /tmp/qwen-smoke/beta.md" \
  "$HOME_DIR/failing-queue.err" || fail "missing beta queue-failure log"
# 3 failures: 2 file edits + 1 assistant turn (prompts off in this run).
grep -q "failed_events=3" "$HOME_DIR/failing-queue.err" || fail "missing failed_events=3 accounting"

assert_capture_dependency_allowlist

echo "memoire qwen rollout capture smoke passed"
