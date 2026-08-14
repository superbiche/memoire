#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# MEMOIRE_BASE_URL is the ratified single knob for every API consumer
# (docs/R10-DESIGN.md); MEMOIRE_BASE (a full /v1 base) stays as the
# explicit compatibility override and wins when both are set.
BASE="${MEMOIRE_BASE:-${MEMOIRE_BASE_URL:-http://127.0.0.1:37778}/v1}"
HOME_DIR="${MEMOIRE_HOME:-$HOME/.memoire}"

MAX_QUEUE_DEPTH="${MEMOIRE_MAX_QUEUE_DEPTH:-20}"
MAX_QUEUE_AGE_SECONDS="${MEMOIRE_MAX_QUEUE_AGE_SECONDS:-300}"
MAX_DISK_PERCENT="${MEMOIRE_MAX_DISK_PERCENT:-85}"
MAX_WAL_BYTES="${MEMOIRE_MAX_WAL_BYTES:-104857600}"
MAX_LAST_WRITE_AGE_SECONDS="${MEMOIRE_MAX_LAST_WRITE_AGE_SECONDS:-0}"

failures=()

fail() {
  failures+=("$1")
}

json_number() {
  local key="$1"
  sed -nE "s/.*\"$key\"[[:space:]]*:[[:space:]]*([0-9]+).*/\\1/p"
}

health="$(curl -fsS "$BASE/health" 2>/dev/null)" || {
  echo "FAIL service-unreachable: $BASE/health" >&2
  exit 1
}

readiness="$(curl -fsS "$BASE/readiness" 2>/dev/null)" || {
  echo "FAIL service-not-ready: $BASE/readiness" >&2
  exit 1
}

if [[ "$readiness" != *'"status":"ready"'* && "$readiness" != *'"status": "ready"'* ]]; then
  fail "service-not-ready: readiness returned $readiness"
fi

queue_depth="$(printf '%s\n' "$health" | json_number queue_depth)"
oldest_queue_age="$(printf '%s\n' "$health" | json_number oldest_queue_age_seconds)"
wal_bytes="$(printf '%s\n' "$health" | json_number wal_bytes)"
last_write_epoch="$(printf '%s\n' "$health" | json_number last_write_epoch)"

queue_depth="${queue_depth:-0}"
oldest_queue_age="${oldest_queue_age:-0}"
wal_bytes="${wal_bytes:-0}"
last_write_epoch="${last_write_epoch:-0}"

if (( queue_depth > MAX_QUEUE_DEPTH )); then
  fail "queue-depth: depth=$queue_depth threshold=$MAX_QUEUE_DEPTH"
fi

if (( oldest_queue_age > MAX_QUEUE_AGE_SECONDS )); then
  fail "queue-age: age_seconds=$oldest_queue_age threshold=$MAX_QUEUE_AGE_SECONDS"
fi

if (( wal_bytes > MAX_WAL_BYTES )); then
  fail "wal-pressure: wal_bytes=$wal_bytes threshold=$MAX_WAL_BYTES"
fi

if [[ -d "$HOME_DIR" ]]; then
  disk_percent="$(df -P "$HOME_DIR" | awk 'NR==2 { gsub(/%/, "", $5); print $5 }')"
  disk_percent="${disk_percent:-0}"
  if (( disk_percent > MAX_DISK_PERCENT )); then
    fail "disk-pressure: used_percent=$disk_percent threshold=$MAX_DISK_PERCENT path=$HOME_DIR"
  fi
else
  fail "home-missing: $HOME_DIR"
fi

if (( MAX_LAST_WRITE_AGE_SECONDS > 0 )); then
  if (( last_write_epoch == 0 )); then
    fail "write-stall: no writes recorded"
  else
    now_epoch="$(date +%s)"
    age=$((now_epoch - last_write_epoch))
    if (( age > MAX_LAST_WRITE_AGE_SECONDS )); then
      fail "write-stall: last_write_age_seconds=$age threshold=$MAX_LAST_WRITE_AGE_SECONDS"
    fi
  fi
fi

if (( ${#failures[@]} > 0 )); then
  printf 'FAIL Memoire health check\n' >&2
  printf '%s\n' "${failures[@]}" >&2
  exit 1
fi

printf 'OK Memoire health check: queue_depth=%s queue_age_seconds=%s wal_bytes=%s home=%s\n' \
  "$queue_depth" "$oldest_queue_age" "$wal_bytes" "$HOME_DIR"
