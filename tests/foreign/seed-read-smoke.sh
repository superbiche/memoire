#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BASE="${MEMOIRE_BASE:-http://127.0.0.1:37778/v1}"
HOME_DIR="${MEMOIRE_HOME:-$ROOT/tmp/foreign-smoke-home}"
TOKEN_FILE="${MEMOIRE_TOKEN_FILE:-$HOME_DIR/token}"
PROJECT="${MEMOIRE_FOREIGN_PROJECT:-memoire-foreign-smoke}"
NEEDLE_PREFIX="${MEMOIRE_FOREIGN_NEEDLE_PREFIX:-foreign-read}"
SESSION_ID="foreign-read-smoke-$(date +%s)"
NEEDLE="$NEEDLE_PREFIX-$(date +%s)"

if [[ ! -r "$TOKEN_FILE" ]]; then
  echo "token file not readable: $TOKEN_FILE" >&2
  exit 1
fi

TOKEN="$(tr -d '\r\n' < "$TOKEN_FILE")"

curl -fsS \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -X POST "$BASE/events" \
  -d "{
    \"schema_version\":1,
    \"project\":\"$PROJECT\",
    \"session_id\":\"$SESSION_ID\",
    \"source_adapter\":\"foreign-smoke-seed\",
    \"source_event_id\":\"$NEEDLE\",
    \"event_type\":\"note\",
    \"occurred_at_epoch\":$(date +%s),
    \"payload\":{\"message\":\"$NEEDLE\"},
    \"text\":\"$NEEDLE\"
  }" >/dev/null

cat <<OUT
Seeded Memoire foreign read smoke.
BASE=$BASE
TOKEN_FILE=$TOKEN_FILE
PROJECT=$PROJECT
NEEDLE=$NEEDLE
OUT
