#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# Smoke: defense-at-source redaction (GOVERNANCE-DESIGN.md ruling 7).
# Reproduces the real-world failure shape — a Claude Code PostToolUse hook
# shipping changed secret-file contents — and proves the secret never reaches
# queue files, raw/ custody copies, SQLite, or search results.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="${MEMOIRE_BIN:-$ROOT/bin/memoire-api}"
PORT="${MEMOIRE_REDACTION_PORT:-37793}"
BASE="http://127.0.0.1:$PORT/v1"
HOME_DIR="$(mktemp -d "${TMPDIR:-/tmp}/memoire-redaction.XXXXXX")"
PID=""

FAKE_ENV_SECRET="hunter2hunter2secret"
FAKE_PEM_LINE="MIIEfakefakekeymaterialfakefake"
SAFE_REF="op://Vault/db/password"

cleanup() {
  if [[ -n "${PID:-}" ]] && kill -0 "$PID" 2>/dev/null; then
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  rm -rf "$HOME_DIR"
}
trap cleanup EXIT

fail() {
  echo "Memoire redaction smoke failed: $*" >&2
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
  echo "Binary not found: $BIN — run: make build" >&2
  exit 1
fi

MEMOIRE_ADDR="127.0.0.1:$PORT" \
  MEMOIRE_HOME="$HOME_DIR" \
  MEMOIRE_DB="$HOME_DIR/memoire.db" \
  MEMOIRE_SCHEMA_DIR="$ROOT/schema" \
  "$BIN" > "$HOME_DIR/server.log" 2>&1 &
PID="$!"
wait_ready

# The failure shape: a Write tool call captured by PostToolUse, carrying a
# changed .env file plus key material in the tool response.
payload="$(
  jq -nc \
    --arg secret "$FAKE_ENV_SECRET" \
    --arg pem "$FAKE_PEM_LINE" \
    --arg ref "$SAFE_REF" \
    '{
      session_id: "redaction-smoke-session",
      cwd: "/tmp/redaction-smoke",
      hook_event_name: "PostToolUse",
      tool_name: "Write",
      tool_use_id: "tool-redact-1",
      tool_input: {
        file_path: "/tmp/redaction-smoke/.env",
        content: ("DATABASE_PASSWORD=" + $secret + "\nSAFE=" + $ref)
      },
      tool_response: {stdout: ("-----BEGIN RSA PRIVATE" + " KEY-----\n" + $pem + "\n-----END RSA PRIVATE" + " KEY-----")}
    }'
)"

# --session-id pins the session explicitly: the capture script prefers
# CLAUDE_CODE_SESSION_ID from the caller's environment, which leaks the
# *operator's* live CC session id when this smoke runs inside a CC session.
printf '%s' "$payload" | \
  MEMOIRE_HOME="$HOME_DIR" \
  MEMOIRE_CLAUDE_STRICT=1 \
  "$ROOT/scripts/memoire-capture-claude.sh" \
    --project memoire-redaction-smoke \
    --session-id redaction-smoke-session >/dev/null

# Wait until the event is drained and visible.
TOKEN="$(tr -d '\r\n' < "$HOME_DIR/token")"
drained=0
for _ in $(seq 1 40); do
  total="$(curl -fsS -H "Authorization: Bearer $TOKEN" \
    "$BASE/events/tool-window?session_id=redaction-smoke-session" | jq -r '.total')"
  if [[ "$total" == "1" ]]; then
    drained=1
    break
  fi
  sleep 0.2
done
[[ "$drained" == "1" ]] || fail "event never drained"

# The core guarantee: the secret exists NOWHERE under MEMOIRE_HOME — not in
# queue files, not in raw/ custody copies, not in the SQLite db or WAL.
if grep -R -a -q "$FAKE_ENV_SECRET" "$HOME_DIR"; then
  fail "env secret found under MEMOIRE_HOME"
fi
if grep -R -a -q "$FAKE_PEM_LINE" "$HOME_DIR"; then
  fail "PEM key material found under MEMOIRE_HOME"
fi

# The safe reference survives, and the event records that redaction happened.
window="$(curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE/events/tool-window?session_id=redaction-smoke-session")"
grep -q "op://Vault/db/password" <<<"$window" || fail "op:// reference did not survive"
grep -q "REDACTED:env-secret-assignment" <<<"$window" || fail "env-secret marker missing from stored event"
grep -q "REDACTED:pem-block" <<<"$window" || fail "pem marker missing from stored event"

# Search must not find the secret either.
if curl -fsS -H "Authorization: Bearer $TOKEN" \
  "$BASE/search?q=$FAKE_ENV_SECRET&limit=5" | grep -q "$FAKE_ENV_SECRET"; then
  fail "secret is search-visible"
fi

# HTTP backstop: a direct POST with a token leaks nothing — including a
# secret-shaped map KEY and a large integer that must survive bit-exact.
curl -fsS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -X POST "$BASE/events" -d '{
    "schema_version": 1,
    "project": "memoire-redaction-smoke",
    "session_id": "redaction-smoke-http",
    "source_adapter": "curl",
    "event_type": "note",
    "payload": {"message": "leaked key AKIA''IOSFODNN7EXAMPLE in a note", "AKIA''J7EXAMPLEKEYNAME": "as a key", "big": 9007199254740993}
  }' >/dev/null

# Batch endpoint gets the same backstop.
curl -fsS -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -X POST "$BASE/events/batch" -d '{
    "events": [{
      "schema_version": 1,
      "project": "memoire-redaction-smoke",
      "session_id": "redaction-smoke-batch",
      "source_adapter": "curl",
      "event_type": "note",
      "payload": {"message": "batch leak ghp_''abcdefghijklmnopqrstuvwxyz0123456789"}
    }]
  }' >/dev/null
# Gap shapes (Fibery #616, 2026-10-06): a bare value under a secret-named
# JSON field, and userinfo credentials inside a URL.
FAKE_JSON_SECRET="j4xQm9vL2zT8""w7Rf3Kd56Hc02Yb"
FAKE_URL_PASS="u3Wb8kD5nV1q""9Lp02Xs47Tc63Ma"
# Tool-shaped like the capture path (raw hook JSON as payload): the JSON
# secret field rides in the tool input, the URL credentials in the tool
# response — the real transcript shape.
gaps_payload="$(
  jq -nc \
    --arg s "$FAKE_JSON_SECRET" \
    --arg p "$FAKE_URL_PASS" \
    '{
      schema_version: 1,
      project: "memoire-redaction-smoke",
      session_id: "redaction-smoke-gaps",
      source_adapter: "curl",
      event_type: "claude_PostToolUse",
      cwd: "/tmp/redaction-smoke",
      payload: {
        hook_event_name: "PostToolUse",
        tool_name: "Write",
        tool_input: {file_path: "/tmp/redaction-smoke/config.json", content: ("{\"apiKey\":\"" + $s + "\"}")},
        tool_response: {stdout: ("postgresql://memoire:" + $p + "@memoire-pg-rw:5432/memoire")}
      }
    }'
)"
printf '%s' "$gaps_payload" | curl -fsS -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -X POST "$BASE/events" -d @- >/dev/null

sleep 1
for leaked in "AKIA""IOSFODNN7EXAMPLE" "AKIA""J7EXAMPLEKEYNAME" "ghp_""abcdefghijklmnopqrstuvwxyz0123456789" "$FAKE_JSON_SECRET" "$FAKE_URL_PASS"; do
  if grep -R -a -q "$leaked" "$HOME_DIR"; then
    fail "HTTP backstop let $leaked through"
  fi
done
grep -R -a -q "9007199254740993" "$HOME_DIR" || fail "large integer corrupted by redaction round-trip"

# Both gap rules must have fired in the stored event (wait for the drain).
gaps_total=0
for _ in $(seq 1 40); do
  gaps_total="$(curl -fsS -H "Authorization: Bearer $TOKEN" \
    "$BASE/events/tool-window?session_id=redaction-smoke-gaps" | jq -r '.total')"
  if [[ "$gaps_total" == "1" ]]; then
    break
  fi
  sleep 0.2
done
[[ "$gaps_total" == "1" ]] || fail "gaps event never drained"
gaps_window="$(curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE/events/tool-window?session_id=redaction-smoke-gaps")"
grep -q "REDACTED:json-secret-field" <<<"$gaps_window" || fail "json-secret-field marker missing from stored event"
grep -q "REDACTED:url-credentials" <<<"$gaps_window" || fail "url-credentials marker missing from stored event"

echo "memoire redaction smoke passed"
