#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-capture-codex-tick.sh — the timer body of the Codex rollout capture.
# Every N minutes it runs the Codex rollout adapter (memoire-capture-codex-
# rollout.sh --tail --scan) over the recently changed rollouts of the sessions
# directory. It is a script of its own, with its own lock, its own wall-clock
# bound and its own systemd timer and unit (capture-codex-tick-timer.sh), so a
# slow or hung Codex run can never delay or prevent the Claude capture tick
# (memoire-capture-tick.sh, which does not know about Codex).
#
# One tick writes ONE line to $MEMOIRE_HOME/log/capture-codex-tick.log:
#   tick_codex files=N events=N failed_files=N stopped=0|1 lines_left=N rc=R
#              [timed_out=1] [rescans=N] [stop="..."] [fail="..."]
# files, events, failed_files, stopped and lines_left are the adapter's
# summary; rc is the adapter's exit code (124: the bound expired and TERM
# ended it; 137: it ignored TERM and was killed KILL_S seconds later);
# rescans=N counts the files whose cursor the adapter rejected and read again
# from line 0 (absent when none);
# stop="..." carries the adapter's own stop line (the file, the line reached,
# the lines left) and fail="..." its first failure lines, each sanitised
# (printable ASCII) and cut to 160 characters. A skipped tick logs
# `tick_codex skipped=<reason>`.
#
# Exit code: 0 whenever the tick itself ran, whatever the adapter did (its rc
# is in the line, so a timer does not flap on it); 0 for every precondition
# skip. There is no other exit code.
#
# Bound: the adapter runs under `timeout -k KILL_S CODEX_TIMEOUT`, stdout and
# stderr going to files (never through a pipe or a command substitution), stdin
# closed and the tick lock's descriptor closed, so nothing a child keeps open
# can hold this script or the lock: it ends within
# CODEX_TIMEOUT + KILL_S seconds of starting the adapter. The adapter itself
# stops at a batch boundary a margin before the bound (exit 0, cursor written,
# stopped=1), so a big backlog drains over successive ticks, oldest file first.
#
# Knobs (all defaulted):
#   MEMOIRE_HOME                   default ~/.memoire (log, lock, cursor root)
#   MEMOIRE_CAPTURE_CODEX          on|1|true enables; unset|off|0|false disable
#                                  (silently); anything else logs
#                                  skipped=codex_switch_unrecognised
#   MEMOIRE_CODEX_TICK_TIMEOUT     default 240  - wall-clock bound, seconds
#   MEMOIRE_CODEX_TICK_KILL_S      default 10   - delay between TERM and KILL
#   MEMOIRE_CODEX_SESSIONS_DIR     default ~/.codex/sessions
#   MEMOIRE_CODEX_ARCHIVED_SESSIONS_DIR  default ~/.codex/archived_sessions
#   MEMOIRE_TICK_QUIET_S           default 120     - skip files touched more recently
#   MEMOIRE_TICK_SINCE_S           default 172800  - 48h scan bound
#   MEMOIRE_CAPTURE_CODEX_BIN      adapter path override
# The systemd unit reads the same optional EnvironmentFile as the Claude tick
# (~/.memoire/capture-tick.env).
#
# A tick killed before its EXIT trap ran (SIGKILL) leaves its output directory
# $MEMOIRE_HOME/tmp/codex-tick.*; the next tick, holding the lock, removes it.
#
# Kill switch: MEMOIRE_CAPTURE=off|0|false disables the tick.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MEMOIRE_HOME="${MEMOIRE_HOME:-$HOME/.memoire}"
LOG_DIR="$MEMOIRE_HOME/log"
LOG_FILE="$LOG_DIR/capture-codex-tick.log"

log() {
  mkdir -p "$LOG_DIR" 2>/dev/null || return 0
  printf '%s %s\n' "$(date -Is)" "$*" >> "$LOG_FILE" 2>/dev/null || true
}

# liaison:D239 session-level kill switch, honored before any other dependency.
case "${MEMOIRE_CAPTURE:-}" in
  off|0|false) log "tick_codex skipped=capture_disabled"; exit 0 ;;
esac
case "${MEMOIRE_CAPTURE_CODEX:-}" in
  on|1|true) ;;
  ""|off|0|false) exit 0 ;;
  *) log "tick_codex skipped=codex_switch_unrecognised"; exit 0 ;;
esac

BIN="${MEMOIRE_CAPTURE_CODEX_BIN:-$SCRIPT_DIR/memoire-capture-codex-rollout.sh}"
TIMEOUT_S="${MEMOIRE_CODEX_TICK_TIMEOUT:-240}"
KILL_S="${MEMOIRE_CODEX_TICK_KILL_S:-10}"
SESSIONS_DIR="${MEMOIRE_CODEX_SESSIONS_DIR:-$HOME/.codex/sessions}"
ARCHIVED_DIR="${MEMOIRE_CODEX_ARCHIVED_SESSIONS_DIR:-$HOME/.codex/archived_sessions}"
QUIET="${MEMOIRE_TICK_QUIET_S:-120}"
SINCE="${MEMOIRE_TICK_SINCE_S:-172800}"
# Fail-open on knob typos: a nonnumeric value falls back to its default. The
# bounds must be POSITIVE — GNU timeout reads 0 as "no limit".
[[ "$TIMEOUT_S" =~ ^[1-9][0-9]*$ ]] || TIMEOUT_S=240
[[ "$KILL_S" =~ ^[1-9][0-9]*$ ]] || KILL_S=10
[[ "$QUIET" =~ ^[0-9]+$ ]] || QUIET=120
[[ "$SINCE" =~ ^[1-9][0-9]*$ ]] || SINCE=172800

# Single instance: ticks must never overlap each other. This lock is the Codex
# tick's own; it shares nothing with the Claude tick's.
LOCK_FILE="$MEMOIRE_HOME/capture-codex-tick.lock"
mkdir -p "$MEMOIRE_HOME" 2>/dev/null
HAVE_LOCK=0
if command -v flock >/dev/null 2>&1 && touch "$LOCK_FILE" 2>/dev/null; then
  exec 8>"$LOCK_FILE"
  flock -n 8 || { log "tick_codex skipped=locked"; exit 0; }
  HAVE_LOCK=1
else
  log "tick_codex lock unavailable, proceeding"
fi

# Output directories of an earlier tick that was killed (SIGKILL, power cut)
# before its EXIT trap ran. Only under the lock: no other tick is then alive.
if (( HAVE_LOCK )); then
  for STALE in "$MEMOIRE_HOME"/tmp/codex-tick.*; do
    [[ -d "$STALE" && ! -L "$STALE" ]] && rm -rf -- "$STALE"
  done
fi

command -v jq >/dev/null 2>&1 || { log "tick_codex skipped=no_jq"; exit 0; }
JQ_VERSION="$(jq --version 2>/dev/null)"
if [[ "$JQ_VERSION" =~ ^jq-([0-9]+)\.([0-9]+) ]] \
   && { (( BASH_REMATCH[1] > 1 )) || (( BASH_REMATCH[1] == 1 && BASH_REMATCH[2] >= 7 )); }; then
  :
else
  log "tick_codex skipped=jq_too_old"; exit 0
fi
command -v timeout >/dev/null 2>&1 || { log "tick_codex skipped=timeout_bin_missing"; exit 0; }
[[ -x "$BIN" ]] || { log "tick_codex skipped=capture_bin_missing"; exit 0; }
if [[ ! -d "$SESSIONS_DIR" && ! -d "$ARCHIVED_DIR" ]]; then
  log "tick_codex skipped=no_sessions_dir"; exit 0
fi

# Output files: private, removed at exit. Content-free (the adapter's summary
# and log lines carry file names, thread ids and counts).
umask 077
mkdir -p "$MEMOIRE_HOME/tmp" 2>/dev/null
RUN_DIR="$(mktemp -d "$MEMOIRE_HOME/tmp/codex-tick.XXXXXX" 2>/dev/null)" \
  || { log "tick_codex skipped=no_work_dir"; exit 0; }
trap 'rm -rf -- "$RUN_DIR"' EXIT
OUT_FILE="$RUN_DIR/out" ERR_FILE="$RUN_DIR/err"

# The adapter stops itself a margin before the hard bound.
MARGIN=$((TIMEOUT_S / 4))
(( MARGIN > 30 )) && MARGIN=30
# (timeout kills itself with KILL after it had to KILL a child; the group
# redirection keeps the shell's "Killed" notice out of the journal.)
{
  timeout -k "$KILL_S" "$TIMEOUT_S" "$BIN" \
    --tail --scan --summary --quiet-for "$QUIET" \
    --since "@$(( $(date +%s) - SINCE ))" \
    --max-seconds "$(( TIMEOUT_S - MARGIN ))" \
    --sessions-dir "$SESSIONS_DIR" \
    --archived-sessions-dir "$ARCHIVED_DIR" \
    </dev/null >"$OUT_FILE" 2>"$ERR_FILE" 8>&-
  RC=$?
} 2>/dev/null

FILES=0 EVENTS=0 FAILED=0 STOPPED=0 LEFT=0
IFS=$'\t' read -r FILES EVENTS FAILED STOPPED LEFT < <(tail -n 1 "$OUT_FILE" 2>/dev/null)
for N in FILES EVENTS FAILED STOPPED LEFT; do
  [[ "${!N}" =~ ^[0-9]+$ ]] || printf -v "$N" 0
done

# The adapter's stop line and failure lines, sanitised and bounded.
clean() { LC_ALL=C tr -c '[:print:]\n' ' ' | tr -d '"' | cut -c1-160; }
STOP_LINE="$(grep -m1 -E 'bound reached at line' "$ERR_FILE" 2>/dev/null | clean)"
FAIL_LINES="$(grep -E 'stopped at line|queue write failed|record scan failed|classifier state unreadable|read failed|stat failed|cursor write failed|jq >= 1.7|queue writer not found' "$ERR_FILE" 2>/dev/null \
  | head -n 2 | clean | paste -sd'|' -)"
EXTRA=""
(( RC == 124 || RC == 137 )) && EXTRA=" timed_out=1"
RESCANS="$(grep -c 'cursor does not match the file' "$ERR_FILE" 2>/dev/null)"
[[ "$RESCANS" =~ ^[1-9][0-9]*$ ]] && EXTRA="$EXTRA rescans=$RESCANS"
[[ -n "$STOP_LINE" ]] && EXTRA="$EXTRA stop=\"$STOP_LINE\""
[[ -n "$FAIL_LINES" ]] && EXTRA="$EXTRA fail=\"$FAIL_LINES\""
log "tick_codex files=$FILES events=$EVENTS failed_files=$FAILED stopped=$STOPPED lines_left=$LEFT rc=$RC$EXTRA"
exit 0
