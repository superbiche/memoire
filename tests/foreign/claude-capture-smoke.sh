#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="${MEMOIRE_BIN:-$ROOT/bin/memoire-api}"
PORT="${MEMOIRE_CLAUDE_CAPTURE_PORT:-37783}"
BASE="http://127.0.0.1:$PORT/v1"
HOME_DIR="$(mktemp -d "${TMPDIR:-/tmp}/memoire-claude-capture.XXXXXX")"
PID=""

cleanup() {
  if [[ -n "${PID:-}" ]] && kill -0 "$PID" 2>/dev/null; then
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  rm -rf "$HOME_DIR"
}
trap cleanup EXIT

wait_ready() {
  for _ in $(seq 1 50); do
    if curl -fsS "$BASE/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  echo "Memoire API did not become ready" >&2
  cat "$HOME_DIR/server.log" >&2 || true
  exit 1
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

NEEDLE="claude-capture-$(date +%s)"
RAW_ONLY="zzclauderawonly$(date +%s)"
PROMPT_NEEDLE="claude-prompt-capture-$(date +%s)"
PROMPT_RAW_ONLY="zzclaudepromptrawonly$(date +%s)"
FAIL_RAW_ONLY="zzclaudefailrawonly$(date +%s)"
payload="$(
  jq -nc \
    --arg needle "$NEEDLE" \
    --arg raw_only "$RAW_ONLY" \
    --arg cwd "$ROOT" \
    '{
      session_id: "claude-capture-smoke-session",
      cwd: $cwd,
      hook_event_name: "PostToolUse",
      tool_name: "Bash",
      memoire_text: $needle,
      tool_use_id: "tool-smoke-123",
      tool_input: {command: "printf smoke"},
      tool_response: {stdout: $raw_only}
    }'
)"

printf '%s' "$payload" | \
  MEMOIRE_HOME="$HOME_DIR" \
  MEMOIRE_CLAUDE_STRICT=1 \
  "$ROOT/scripts/memoire-capture-claude.sh" \
    --project memoire-claude-capture \
    --text "$NEEDLE" >/dev/null

prompt_payload="$(
  jq -nc \
    --arg prompt "$PROMPT_NEEDLE" \
    --arg raw_only "$PROMPT_RAW_ONLY" \
    --arg cwd "$ROOT" \
    '{
      session_id: "claude-capture-smoke-session",
      cwd: $cwd,
      hook_event_name: "UserPromptSubmit",
      prompt: $prompt,
      raw_only: $raw_only
    }'
)"

printf '%s' "$prompt_payload" | \
  MEMOIRE_HOME="$HOME_DIR" \
  MEMOIRE_CLAUDE_STRICT=1 \
  "$ROOT/scripts/memoire-capture-claude.sh" \
    --project memoire-claude-capture >/dev/null

# A failed tool call reaches the hook as PostToolUseFailure, never as
# PostToolUse: top-level `error`, no tool_response.
failure_payload="$(
  jq -nc \
    --arg raw_only "$FAIL_RAW_ONLY" \
    --arg cwd "$ROOT" \
    '{
      session_id: "claude-capture-smoke-session",
      cwd: $cwd,
      hook_event_name: "PostToolUseFailure",
      tool_name: "Bash",
      tool_use_id: "tool-smoke-fail-456",
      tool_input: {command: "false"},
      error: ("Exit code 1\n" + $raw_only),
      is_interrupt: false,
      duration_ms: 12
    }'
)"

printf '%s' "$failure_payload" | \
  MEMOIRE_HOME="$HOME_DIR" \
  MEMOIRE_CLAUDE_STRICT=1 \
  "$ROOT/scripts/memoire-capture-claude.sh" \
    --project memoire-claude-capture >/dev/null

TOKEN="$(tr -d '\r\n' < "$HOME_DIR/token")"
for _ in $(seq 1 30); do
  if curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE/search?q=$NEEDLE&project=memoire-claude-capture&limit=5" | grep -q "$NEEDLE" &&
     curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE/search?q=$PROMPT_NEEDLE&project=memoire-claude-capture&limit=5" | grep -q "$PROMPT_NEEDLE"; then
    if ! grep -R -q "$RAW_ONLY" "$HOME_DIR/raw"; then
      echo "Memoire Claude capture smoke failed: raw payload did not preserve tool_response" >&2
      exit 1
    fi
    if ! grep -R -q "$PROMPT_RAW_ONLY" "$HOME_DIR/raw"; then
      echo "Memoire Claude capture smoke failed: raw prompt payload was not preserved" >&2
      exit 1
    fi
    if curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE/search?q=$RAW_ONLY&project=memoire-claude-capture&limit=5" | grep -q "$RAW_ONLY"; then
      echo "Memoire Claude capture smoke failed: raw-only tool_response became search-visible" >&2
      exit 1
    fi
    if curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE/search?q=$PROMPT_RAW_ONLY&project=memoire-claude-capture&limit=5" | grep -q "$PROMPT_RAW_ONLY"; then
      echo "Memoire Claude capture smoke failed: raw-only prompt payload became search-visible" >&2
      exit 1
    fi
    if ! grep -R -q "$FAIL_RAW_ONLY" "$HOME_DIR/raw"; then
      echo "Memoire Claude capture smoke failed: raw payload did not preserve the tool failure error" >&2
      exit 1
    fi
    if ! grep -R -q "claude_PostToolUseFailure" "$HOME_DIR/raw"; then
      echo "Memoire Claude capture smoke failed: tool failure was not typed claude_PostToolUseFailure" >&2
      exit 1
    fi
    if ! grep -R -q "claude-posttoolusefailure-" "$HOME_DIR/raw"; then
      echo "Memoire Claude capture smoke failed: tool failure has no stable tool_use_id-based source event id" >&2
      exit 1
    fi
    if curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE/search?q=$FAIL_RAW_ONLY&project=memoire-claude-capture&limit=5" | grep -q "$FAIL_RAW_ONLY"; then
      echo "Memoire Claude capture smoke failed: raw-only tool failure error became search-visible" >&2
      exit 1
    fi
    echo "memoire claude capture smoke passed"
    exit 0
  fi
  sleep 0.2
done

echo "Memoire Claude capture smoke failed" >&2
cat "$HOME_DIR/server.log" >&2 || true
exit 1
