#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="${MEMOIRE_BIN:-$ROOT/bin/memoire-api}"
PORT="${MEMOIRE_CODEX_CAPTURE_PORT:-37780}"
BASE="http://127.0.0.1:$PORT/v1"
HOME_DIR="$(mktemp -d "${TMPDIR:-/tmp}/memoire-codex-capture.XXXXXX")"
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

NEEDLE="codex-capture-$(date +%s)"
printf "%s" "$NEEDLE" | \
  MEMOIRE_HOME="$HOME_DIR" \
  "$ROOT/scripts/memoire-capture-codex.sh" \
    --project memoire-codex-capture >/dev/null

TOKEN="$(tr -d '\r\n' < "$HOME_DIR/token")"
for _ in $(seq 1 30); do
  if curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE/search?q=$NEEDLE&project=memoire-codex-capture&limit=5" | grep -q "$NEEDLE"; then
    echo "memoire codex capture smoke passed"
    exit 0
  fi
  sleep 0.2
done

echo "Memoire Codex capture smoke failed" >&2
cat "$HOME_DIR/server.log" >&2 || true
exit 1
