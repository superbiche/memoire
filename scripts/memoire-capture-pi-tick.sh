#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-capture-pi-tick.sh — the timer body of the host-side Pi session sweep.
# Every N minutes it runs memoire-capture-pi.mjs --scan over the Pi session files that changed
# recently: ordinary, sandboxed, T3 and ACP sessions (the agent's sessions directory), the Conclave
# coordinator seat's own store and the caged workers' run directories. It is a script of its own,
# with its own lock, wall-clock bound and (capture-pi-tick-timer.sh) systemd timer and unit, so a
# slow run never delays another capture tick, and none of them knows about Pi.
#
# One tick writes ONE line to $MEMOIRE_HOME/log/capture-pi-tick.log:
#   tick_pi sessions=N events=N failed_sessions=N rejected=N blobs=0 stopped=0|1 left=N source=primary rc=R
#           [timed_out=1] [fail="..."]
# (the adapter's summary: sessions that queued events, events queued, failed chunks, rejected
# files, stopped at the time bound, files left for later: still being written or past the bound).
# A skipped tick logs `tick_pi skipped=<reason>`. Exit code 0 whenever the tick itself ran.
#
# Bound: the adapter runs under `timeout -k KILL_S PI_TICK_TIMEOUT`, stdout and stderr to files,
# stdin closed and the lock descriptor closed; it stops itself a margin before the bound at a file
# boundary (cursors are written per chunk, so a stop loses nothing). A backlog drains over
# successive ticks, oldest change first.
#
# Knobs (all defaulted):
#   MEMOIRE_HOME                 default ~/.memoire
#   MEMOIRE_CAPTURE_PI           on|1|true enables; unset|off|0|false disable silently; anything
#                                else logs skipped=pi_switch_unrecognised
#   MEMOIRE_PI_TICK_TIMEOUT      default 240  - wall-clock bound, seconds
#   MEMOIRE_PI_TICK_KILL_S       default 10   - delay between TERM and KILL
#   MEMOIRE_TICK_QUIET_S         default 120  - leave files changed more recently (still written)
#   MEMOIRE_TICK_SINCE_S         default 172800 - 48h scan bound (a full backfill is a manual run:
#                                memoire-capture-pi.mjs --scan with no --since)
#   MEMOIRE_CAPTURE_PI_BIN       adapter path override
#   MEMOIRE_NODE                 node binary (default: node on PATH)
# The session roots are the adapter's own parameters (MEMOIRE_PI_SESSION_ROOTS, MEMOIRE_PI_RUNS_DIR,
# LIAISON_PI_SEAT_STATE, LIAISON_CONCLAVE_HOME): set them in the environment file the systemd unit
# reads (~/.memoire/capture-tick.env).
# Kill switch: MEMOIRE_CAPTURE=off|0|false disables the tick.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MEMOIRE_HOME="${MEMOIRE_HOME:-$HOME/.memoire}"
LOG_DIR="$MEMOIRE_HOME/log"
LOG_FILE="$LOG_DIR/capture-pi-tick.log"

log() {
  mkdir -p "$LOG_DIR" 2>/dev/null || return 0
  printf '%s %s\n' "$(date -Is)" "$*" >> "$LOG_FILE" 2>/dev/null || true
}

case "${MEMOIRE_CAPTURE:-}" in
  off|0|false) log "tick_pi skipped=capture_disabled"; exit 0 ;;
esac
case "${MEMOIRE_CAPTURE_PI:-}" in
  on|1|true) ;;
  ""|off|0|false) exit 0 ;;
  *) log "tick_pi skipped=pi_switch_unrecognised"; exit 0 ;;
esac

BIN="${MEMOIRE_CAPTURE_PI_BIN:-$SCRIPT_DIR/memoire-capture-pi.mjs}"
NODE="${MEMOIRE_NODE:-node}"
TIMEOUT_S="${MEMOIRE_PI_TICK_TIMEOUT:-240}"
KILL_S="${MEMOIRE_PI_TICK_KILL_S:-10}"
QUIET="${MEMOIRE_TICK_QUIET_S:-120}"
SINCE="${MEMOIRE_TICK_SINCE_S:-172800}"
[[ "$TIMEOUT_S" =~ ^[1-9][0-9]*$ ]] || TIMEOUT_S=240
[[ "$KILL_S" =~ ^[1-9][0-9]*$ ]] || KILL_S=10
[[ "$QUIET" =~ ^[0-9]+$ ]] || QUIET=120
[[ "$SINCE" =~ ^[1-9][0-9]*$ ]] || SINCE=172800

LOCK_FILE="$MEMOIRE_HOME/capture-pi-tick.lock"
mkdir -p "$MEMOIRE_HOME" 2>/dev/null
HAVE_LOCK=0
if command -v flock >/dev/null 2>&1 && touch "$LOCK_FILE" 2>/dev/null; then
  exec 8>"$LOCK_FILE"
  flock -n 8 || { log "tick_pi skipped=locked"; exit 0; }
  HAVE_LOCK=1
else
  log "tick_pi lock unavailable, proceeding"
fi

if (( HAVE_LOCK )); then
  for STALE in "$MEMOIRE_HOME"/tmp/pi-tick.*; do
    [[ -d "$STALE" && ! -L "$STALE" ]] && rm -rf -- "$STALE"
  done
fi

command -v timeout >/dev/null 2>&1 || { log "tick_pi skipped=timeout_bin_missing"; exit 0; }
command -v "$NODE" >/dev/null 2>&1 || { log "tick_pi skipped=node_missing"; exit 0; }
[[ -r "$BIN" ]] || { log "tick_pi skipped=capture_bin_missing"; exit 0; }

umask 077
mkdir -p "$MEMOIRE_HOME/tmp" 2>/dev/null
RUN_DIR="$(mktemp -d "$MEMOIRE_HOME/tmp/pi-tick.XXXXXX" 2>/dev/null)" \
  || { log "tick_pi skipped=no_work_dir"; exit 0; }
trap 'rm -rf -- "$RUN_DIR"' EXIT
OUT_FILE="$RUN_DIR/out" ERR_FILE="$RUN_DIR/err"

MARGIN=$((TIMEOUT_S / 4))
(( MARGIN > 30 )) && MARGIN=30
{
  timeout -k "$KILL_S" "$TIMEOUT_S" "$NODE" "$BIN" \
    --scan --summary --quiet-for "$QUIET" \
    --since "@$(( $(date +%s) - SINCE ))" \
    --max-seconds "$(( TIMEOUT_S - MARGIN ))" \
    </dev/null >"$OUT_FILE" 2>"$ERR_FILE" 8>&-
  RC=$?
} 2>/dev/null

SESSIONS=0 EVENTS=0 FAILED=0 REJECTED=0 STOPPED=0 LEFT=0 BLOBS=0 SOURCE=primary
IFS=$'\t' read -r SESSIONS EVENTS FAILED REJECTED STOPPED LEFT BLOBS SOURCE < <(tail -n 1 "$OUT_FILE" 2>/dev/null)
for N in SESSIONS EVENTS FAILED REJECTED STOPPED LEFT BLOBS; do
  [[ "${!N}" =~ ^[0-9]+$ ]] || printf -v "$N" 0
done
SOURCE=primary

clean() { LC_ALL=C tr -c '[:print:]\n' ' ' | tr -d '"' | cut -c1-160; }
FAIL_LINES="$(grep -E 'queue write failed|unreadable|reject' "$ERR_FILE" 2>/dev/null | head -n 2 | clean | paste -sd'|' -)"
EXTRA=""
(( RC == 124 || RC == 137 )) && EXTRA=" timed_out=1"
[[ -n "$FAIL_LINES" ]] && EXTRA="$EXTRA fail=\"$FAIL_LINES\""
log "tick_pi sessions=$SESSIONS events=$EVENTS failed_sessions=$FAILED rejected=$REJECTED blobs=$BLOBS stopped=$STOPPED left=$LEFT source=$SOURCE rc=$RC$EXTRA"
exit 0
