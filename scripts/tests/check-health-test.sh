#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
# check-health.sh write-stall: a null last_write_epoch (hub seed not landed) is
# skipped, a stale one fails, a fresh one passes. curl is a fixture; no network.
set -uo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
cat > "$T/curl" <<'SH'
#!/bin/bash
case "$*" in *readiness*) echo '{"status":"ready"}' ;; *) cat "$HEALTH_FIXTURE" ;; esac
SH
chmod +x "$T/curl"
now="$(date +%s)"; fails=0
run() { # <body> <want-rc> <name>
  printf '%s\n' "$1" > "$T/h.json"
  HEALTH_FIXTURE="$T/h.json" PATH="$T:$PATH" MEMOIRE_HOME="$T" MEMOIRE_MAX_LAST_WRITE_AGE_SECONDS=300 \
    bash "$root/scripts/check-health.sh" >/dev/null 2>&1
  local rc=$?
  if [[ $rc == "$2" ]]; then echo "ok: $3"; else echo "FAIL: $3 (rc=$rc, want $2)"; fails=$((fails + 1)); fi
}
run '{"last_write_epoch":null,"queue_depth":0}' 0 "write-stall: null last write is skipped, not a stall"
run "{\"last_write_epoch\":$((now - 3600)),\"queue_depth\":0}" 1 "write-stall: a stale last write fails"
run "{\"last_write_epoch\":$now,\"queue_depth\":0}" 0 "write-stall: a fresh last write passes"
exit $(( fails > 0 ))
