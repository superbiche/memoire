#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

BASE="${MEMOIRE_BASE:-http://127.0.0.1:37778/v1}"
HOME_DIR="${MEMOIRE_HOME:-$HOME/.memoire}"
TOKEN_FILE="${MEMOIRE_TOKEN_FILE:-$HOME_DIR/token}"

if [[ ! -r "$TOKEN_FILE" ]]; then
  echo "token file not readable: $TOKEN_FILE" >&2
  exit 1
fi

TOKEN="$(tr -d '\r\n' < "$TOKEN_FILE")"
AUTH=(-H "Authorization: Bearer $TOKEN")
SESSION_ID="smoke-$(date +%s)"
SOURCE_EVENT_ID="smoke-$(date +%s%N)"
NEEDLE="memoire-smoke-$SOURCE_EVENT_ID"

curl -fsS "${AUTH[@]}" "$BASE/stats" >/dev/null

EVENT_ID="$(
  curl -fsS "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -X POST "$BASE/events" \
    -d "{
      \"schema_version\":1,
      \"project\":\"memoire\",
      \"session_id\":\"$SESSION_ID\",
      \"source_adapter\":\"smoke\",
      \"source_event_id\":\"$SOURCE_EVENT_ID\",
      \"event_type\":\"note\",
      \"occurred_at\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",
      \"payload\":{\"message\":\"$NEEDLE\"},
      \"text\":\"$NEEDLE\"
    }" | sed -E 's/.*"queue_id":"([^"]+)".*/\1/'
)"

for _ in $(seq 1 30); do
  if curl -fsS "${AUTH[@]}" "$BASE/search?q=$NEEDLE&project=memoire&limit=5" | grep -q "$NEEDLE"; then
    break
  fi
  sleep 0.2
done
curl -fsS "${AUTH[@]}" "$BASE/search?q=$NEEDLE&project=memoire&limit=5" | grep -q "$NEEDLE"
curl -fsS "${AUTH[@]}" "$BASE/sessions/$SESSION_ID" | grep -q '"observation_count":1'
curl -fsS "$BASE/search?token=$TOKEN&q=$NEEDLE&project=memoire&limit=5" | grep -q "$NEEDLE"
curl -fsS "${AUTH[@]}" \
  -H 'Content-Type: application/json' \
  -X POST "$BASE/context" \
  -d "{\"q\":\"$NEEDLE\",\"project\":\"memoire\",\"limit\":5}" | grep -q "$NEEDLE"
curl -fsS \
  -H 'Content-Type: application/json' \
  -X POST "$BASE/context?token=$TOKEN" \
  -d "{\"q\":\"$NEEDLE\",\"project\":\"memoire\",\"limit\":5}" | grep -q "$NEEDLE"
curl -fsS "${AUTH[@]}" "$BASE/timeline?project=memoire&limit=5" | grep -q "$NEEDLE"

echo "memoire smoke passed: queue $EVENT_ID"
