#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-conductor-prewarm.sh — warm the Conductor-managed HL5 profile
# before the nightly sweep (memoire:D163). The Conductor cold path is
# WOL + model load ≈ 2-4 min and its proxy returns 503 (queue_timeout_s)
# while warming; without a pre-warm the sweep's first windows would burn as
# failed runs. This fires a 1-token completion and retries until the proxy
# answers 200 or the deadline passes. Fail-open: a cold conductor must not
# abort the sweep — runs that still hit a warming proxy fail honestly and
# retry next night.

MEMOIRE_HOME="${MEMOIRE_HOME:-$HOME/.memoire}"
LOG_DIR="$MEMOIRE_HOME/log"
LOG_FILE="$LOG_DIR/derive-auto.log"
# conductor-proxy-v1 (reveille, ratified 2026-08-02): the canonical
# Conductor endpoint; the raw :3000 socket is retired as a consumer
# address. In-cluster callers override with the ClusterIP service URL.
BASE_URL="${MEMOIRE_CONDUCTOR_URL:-https://reveille.superhome.cloud}"
# Warm the model the sweep will actually use: MEMOIRE_DERIVE_MODEL is
# the lane's model env (2026-08-03: the old standalone default warmed a
# retired alias the Conductor 404s — every prewarm burned its deadline).
MODEL="${MEMOIRE_PREWARM_MODEL:-${MEMOIRE_DERIVE_MODEL:-brain}}"
KEY_ENV="${MEMOIRE_DERIVE_KEY_ENV:-REVEILLE_API_KEY}"
DEADLINE_S="${MEMOIRE_PREWARM_DEADLINE_S:-480}"
# One attempt may legitimately block ~queue_timeout_s (240s) before a 503,
# but never past the overall deadline (round-1 LOW-1: each attempt is capped
# by the REMAINING budget, so the deadline is hard).
ATTEMPT_TIMEOUT_S="${MEMOIRE_PREWARM_ATTEMPT_TIMEOUT_S:-300}"

mkdir -p "$LOG_DIR" 2>/dev/null || exit 0
mark() { printf '%s prewarm %s\n' "$(date -Is)" "$1" >> "$LOG_FILE" 2>/dev/null; }

# systemd credential -> env var (same convention as the sweep script).
CRED_NAME="$(printf '%s' "$KEY_ENV" | tr '[:upper:]' '[:lower:]')"
if [[ -n "${CREDENTIALS_DIRECTORY:-}" && -r "$CREDENTIALS_DIRECTORY/$CRED_NAME" ]]; then
  export "$KEY_ENV"="$(cat "$CREDENTIALS_DIRECTORY/$CRED_NAME")"
fi
printenv "$KEY_ENV" >/dev/null 2>&1 || { mark "skipped=key_missing env=$KEY_ENV"; exit 0; }
command -v curl >/dev/null 2>&1 || { mark "skipped=curl_missing"; exit 0; }

# The bearer must never enter argv (round-1 HIGH-2: /proc/<pid>/cmdline is
# world-readable while a cold-path attempt blocks). curl reads the header
# from a 0600 temp file instead.
HDR_FILE="$(umask 077 && mktemp "${TMPDIR:-/tmp}/memoire-prewarm-hdr.XXXXXX")" || {
  mark "skipped=mktemp_failed"; exit 0
}
trap 'rm -f "$HDR_FILE"' EXIT
printf 'Authorization: Bearer %s\n' "$(printenv "$KEY_ENV")" > "$HDR_FILE"

# Model name is operator-configurable: escape for JSON so a quote or
# backslash cannot malform the body (round-1 LOW-4).
MODEL_ESC="${MODEL//\\/\\\\}"
MODEL_ESC="${MODEL_ESC//\"/\\\"}"
BODY="{\"model\":\"$MODEL_ESC\",\"messages\":[{\"role\":\"user\",\"content\":\"ok\"}],\"max_tokens\":1}"

deadline=$(( $(date +%s) + DEADLINE_S ))
attempt=0
while :; do
  now=$(date +%s)
  remaining=$(( deadline - now ))
  if (( remaining <= 0 )); then
    mark "deadline model=$MODEL attempts=$attempt last_code=${code:-none}"
    exit 0
  fi
  cap=$(( remaining < ATTEMPT_TIMEOUT_S ? remaining : ATTEMPT_TIMEOUT_S ))
  attempt=$((attempt + 1))
  code="$(curl -s -o /dev/null -w '%{http_code}' -m "$cap" \
    -H @"$HDR_FILE" \
    -H "Content-Type: application/json" \
    -d "$BODY" \
    "$BASE_URL/v1/chat/completions" 2>/dev/null)"
  if [[ "$code" == "200" ]]; then
    mark "ok model=$MODEL attempts=$attempt"
    exit 0
  fi
  # Sleep at most to the deadline (never spin on fast failures, never
  # oversleep past the budget; the loop top emits the deadline marker).
  sleep_s=$(( deadline - $(date +%s) ))
  (( sleep_s > 15 )) && sleep_s=15
  (( sleep_s > 0 )) && sleep "$sleep_s"
done
