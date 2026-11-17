#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# pi-capture-tick-test.sh — the timer body of the host-side Pi sweep
# (scripts/memoire-capture-pi-tick.sh) against a fake adapter in a scratch home: switches, the
# summary line, skips, the lock, the bound. No Pi, no hub, no systemd.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
command -v node >/dev/null || { echo "SKIP: node not available"; exit 0; }
command -v timeout >/dev/null || { echo "SKIP: timeout not available"; exit 0; }
command -v flock >/dev/null || { echo "SKIP: flock not available"; exit 0; }
TICK="$ROOT/scripts/memoire-capture-pi-tick.sh"
NODE_REAL="$(node -p 'process.execPath')"   # the real binary: a version-manager shim needs the real HOME
W="$(mktemp -d "${TMPDIR:-/tmp}/pi-tick.XXXXXX")"
trap 'rm -rf "$W"' EXIT
export HOME="$W/home"; mkdir -p "$HOME"
export MEMOIRE_HOME="$W/mh"
unset MEMOIRE_CAPTURE MEMOIRE_CAPTURE_PI
export MEMOIRE_NODE="$NODE_REAL"
LOG="$MEMOIRE_HOME/log/capture-pi-tick.log"
fails=0
check() { if ! eval "$2"; then echo "FAIL: $1"; fails=$((fails+1)); fi; }

cat > "$W/adapter.mjs" <<'JS'
import fs from "node:fs";
fs.appendFileSync(process.env.CALLS, process.argv.slice(2).join(" ") + "\n");
if (process.env.HANG) { process.on("SIGTERM", () => {}); await new Promise((r) => setTimeout(r, 30000)); }
console.error("memoire-capture-pi: queue write failed: kind=agent session=x");
console.log(process.env.SUMMARY ?? "2\t9\t1\t0\t0\t3\t0\tprimary\t4");
JS
export MEMOIRE_CAPTURE_PI_BIN="$W/adapter.mjs" CALLS="$W/calls"

# switch off / unset: silent
"$TICK"; check "unset switch: silent, adapter not run" '[[ ! -e "$LOG" && ! -e "$CALLS" ]]'
MEMOIRE_CAPTURE_PI=off "$TICK"; check "off: silent" '[[ ! -e "$CALLS" ]]'
MEMOIRE_CAPTURE_PI=maybe "$TICK"; check "unrecognised switch is logged" 'grep -q "skipped=pi_switch_unrecognised" "$LOG"'
MEMOIRE_CAPTURE_PI=on MEMOIRE_CAPTURE=off "$TICK"; check "kill switch is logged" 'grep -q "skipped=capture_disabled" "$LOG"'
check "nothing ran yet" '[[ ! -e "$CALLS" ]]'

# on: one line with the adapter's summary, the adapter gets the scan arguments
MEMOIRE_CAPTURE_PI=on "$TICK"
check "adapter called once with --scan --summary --quiet-for --max-seconds, and no lookback window" '[[ "$(wc -l < "$CALLS")" == 1 ]] && grep -q -- "--scan --summary --quiet-for 120 --max-seconds 210" "$CALLS" && ! grep -q -- "--since" "$CALLS"'
check "summary line" 'tail -n 1 "$LOG" | grep -q "tick_pi sessions=2 events=9 failed_sessions=1 rejected=0 quarantined=4 blobs=0 stopped=0 left=3 source=primary rc=0 fail=\"memoire-capture-pi: queue write failed"'

# a garbage summary falls back to zeros
SUMMARY="junk" MEMOIRE_CAPTURE_PI=on "$TICK"
check "garbage summary reads as zeros" 'tail -n 1 "$LOG" | grep -q "tick_pi sessions=0 events=0 failed_sessions=0"'

# node missing
MEMOIRE_NODE="$W/no-such-node" MEMOIRE_CAPTURE_PI=on "$TICK"; check "node missing is a logged skip" 'tail -n 1 "$LOG" | grep -q "skipped=node_missing"'

# lock held by another tick
( exec 9>"$MEMOIRE_HOME/capture-pi-tick.lock"; flock -n 9; MEMOIRE_CAPTURE_PI=on "$TICK" )
check "a running tick is not overlapped" 'tail -n 1 "$LOG" | grep -q "skipped=locked"'

# bound: a hung adapter that ignores TERM is killed; the tick still exits 0 and logs timed_out
start=$(date +%s)
HANG=1 MEMOIRE_PI_TICK_TIMEOUT=2 MEMOIRE_PI_TICK_KILL_S=1 MEMOIRE_CAPTURE_PI=on "$TICK"; rc=$?
check "hung adapter: tick exits 0 within the bound" '[[ $rc == 0 && $(( $(date +%s) - start )) -le 6 ]] && tail -n 1 "$LOG" | grep -q "timed_out=1"'

# a lock the tick cannot take: it refuses to run (it used to proceed unlocked)
rm -f "$CALLS"; mkdir -p "$MEMOIRE_HOME/capture-pi-tick.lock.d"
( LOCKDIR="$MEMOIRE_HOME"; rm -f "$LOCKDIR/capture-pi-tick.lock"; mkdir "$LOCKDIR/capture-pi-tick.lock"; MEMOIRE_CAPTURE_PI=on "$TICK" )
check "an unobtainable lock is a logged refusal, the adapter does not run" 'tail -n 1 "$LOG" | grep -q "skipped=lock_unavailable" && [[ ! -e "$CALLS" ]]'
rmdir "$MEMOIRE_HOME/capture-pi-tick.lock" 2>/dev/null || true

# the installer writes units only on `install`; usage otherwise (nothing is installed here)
check "installer: usage on no argument" '! "$ROOT/scripts/capture-pi-tick-timer.sh" 2>/dev/null'

if (( fails )); then echo "pi-capture-tick-test: $fails failure(s)"; exit 1; fi
echo "pi-capture-tick-test: OK"
