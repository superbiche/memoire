#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="${MEMOIRE_BIN:-$ROOT/bin/memoire-api}"
PORT="${MEMOIRE_E2E_PORT:-37779}"
BASE="http://127.0.0.1:$PORT/v1"
HOME_DIR="$(mktemp -d "${TMPDIR:-/tmp}/memoire-restart-smoke.XXXXXX")"
PID=""

cleanup() {
  if [[ -n "${PID:-}" ]] && kill -0 "$PID" 2>/dev/null; then
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  rm -rf "$HOME_DIR"
}
trap cleanup EXIT

start_api() {
  MEMOIRE_ADDR="127.0.0.1:$PORT" \
    MEMOIRE_HOME="$HOME_DIR" \
    MEMOIRE_DB="$HOME_DIR/memoire.db" \
    MEMOIRE_SCHEMA_DIR="$ROOT/schema" \
    "$BIN" > "$HOME_DIR/server.log" 2>&1 &
  PID="$!"
  wait_ready
}

stop_api() {
  if [[ -n "${PID:-}" ]] && kill -0 "$PID" 2>/dev/null; then
    kill "$PID"
    wait "$PID" 2>/dev/null || true
  fi
  PID=""
}

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

auth_header() {
  TOKEN="$(tr -d '\r\n' < "$HOME_DIR/token")"
  AUTH=(-H "Authorization: Bearer $TOKEN")
}

post_event() {
  local source_id="$1"
  local text="$2"
  curl -fsS "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -X POST "$BASE/events" \
    -d "{
      \"schema_version\":1,
      \"project\":\"memoire-e2e\",
      \"session_id\":\"restart-smoke\",
      \"source_adapter\":\"restart-smoke\",
      \"source_event_id\":\"$source_id\",
      \"event_type\":\"note\",
      \"occurred_at_epoch\":$(date +%s),
      \"payload\":{\"message\":\"$text\"},
      \"text\":\"$text\"
    }" >/dev/null
}

search_for() {
  local q="$1"
  curl -fsS "${AUTH[@]}" "$BASE/search?q=$q&project=memoire-e2e&limit=10"
}

write_queue_file() {
  local source_id="$1"
  local text="$2"
  mkdir -p "$HOME_DIR/queue/pending"
  local file="$HOME_DIR/queue/pending/$(date +%s%3N)-restart-smoke.json"
  local tmp="$file.tmp"
  cat > "$tmp" <<JSON
{
  "schema_version": 1,
  "project": "memoire-e2e",
  "session_id": "restart-smoke",
  "source_adapter": "restart-smoke-queue",
  "source_event_id": "$source_id",
  "event_type": "note",
  "occurred_at_epoch": $(date +%s),
  "payload": {"message": "$text"},
  "text": "$text"
}
JSON
  mv "$tmp" "$file"
}

if [[ ! -x "$BIN" ]]; then
  echo "Binary not found or not executable: $BIN" >&2
  echo "Run: make build" >&2
  exit 1
fi

start_api
auth_header
post_event "restart-http-1" "restart-http durable-needle"
for _ in $(seq 1 30); do
  if search_for "durable-needle" | grep -q durable-needle; then
    break
  fi
  sleep 0.2
done
search_for "durable-needle" | grep -q durable-needle

stop_api
start_api
auth_header
search_for "durable-needle" | grep -q durable-needle

stop_api
write_queue_file "restart-queue-1" "restart-queue durable-needle"
start_api
auth_header
for _ in $(seq 1 30); do
  if search_for "restart-queue" | grep -q "restart-queue"; then
    echo "memoire restart smoke passed"
    exit 0
  fi
  sleep 0.2
done

echo "Queued event was not ingested after restart" >&2
cat "$HOME_DIR/server.log" >&2 || true
exit 1
