#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-capture-opencode-tick.sh — the timer body of the OpenCode capture.
# Every N minutes it runs the OpenCode adapter (memoire-capture-opencode.sh
# --scan) over the sessions of the OpenCode store that changed recently. It is
# a script of its own, with its own lock, its own wall-clock bound and its own
# systemd timer and unit (capture-opencode-tick-timer.sh), so a slow or hung
# OpenCode run can never delay or prevent the Claude capture tick
# (memoire-capture-tick.sh) or the Codex one (memoire-capture-codex-tick.sh),
# neither of which knows about OpenCode.
#
# One tick writes ONE line (two when it fell back, see below) to
# $MEMOIRE_HOME/log/capture-opencode-tick.log:
#   tick_opencode sessions=N events=N failed_sessions=N rejected=N blobs=N
#                 stopped=0|1 left=N source=primary|archive rc=R [timed_out=1]
#                 [no_database=1] [stop="..."] [fail="..."]
# sessions, events, failed_sessions, rejected, blobs, stopped, left and source
# are the adapter's summary (source=archive: the run fell back to the archive
# copy, which lags the live store by up to about 25 hours; a run that fell back
# logs ONE extra line, `tick_opencode source=archive reason="..."`, with the
# adapter's own reason, sanitised); rc is the adapter's exit code (124: the bound expired and
# TERM ended it; 137: it ignored TERM and was killed KILL_S seconds later);
# stop="..." carries the adapter's own stop line (why it stopped, how many
# sessions are left) and fail="..." its first failure lines, each sanitised
# (printable ASCII) and cut to 160 characters. A skipped tick logs
# `tick_opencode skipped=<reason>`.
#
# Exit code: 0 whenever the tick itself ran, whatever the adapter did (its rc
# is in the line, so a timer does not flap on it); 0 for every precondition
# skip. There is no other exit code.
#
# Bound: the adapter runs under `timeout -k KILL_S OPENCODE_TIMEOUT`, stdout and
# stderr going to files (never through a pipe or a command substitution), stdin
# closed and the tick lock's descriptor closed, so nothing a child keeps open
# can hold this script or the lock: it ends within
# OPENCODE_TIMEOUT + KILL_S seconds of starting the adapter. The adapter itself
# stops at a batch boundary a margin before the bound (exit 0, cursors written,
# stopped=1), so a big backlog drains over successive ticks, oldest change first.
# A TERM sent to the whole process group (systemd's stop, `timeout`) is a clean
# stop as well: the adapter keeps the durable prefix of its cursor and exits 0.
#
# Knobs (all defaulted):
#   MEMOIRE_HOME                   default ~/.memoire (log, lock, cursor root)
#   MEMOIRE_CAPTURE_OPENCODE       on|1|true enables; unset|off|0|false disable
#                                  (silently); anything else logs
#                                  skipped=opencode_switch_unrecognised
#   MEMOIRE_OPENCODE_TICK_TIMEOUT  default 240 - wall-clock bound, seconds
#   MEMOIRE_OPENCODE_TICK_KILL_S   default 10  - delay between TERM and KILL
#   MEMOIRE_TICK_QUIET_S           default 120    - leave rows changed more recently
#   MEMOIRE_TICK_SINCE_S           default 172800 - 48h scan bound
#   MEMOIRE_CAPTURE_OPENCODE_BIN   adapter path override
# The database, its fallback and the tool-output directories are the adapter's
# own parameters (MEMOIRE_OPENCODE_DB, MEMOIRE_OPENCODE_DB_FALLBACK,
# MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR, ...): set them in the environment file. The
# systemd unit reads the same optional EnvironmentFile as the Claude tick
# (~/.memoire/capture-tick.env).
#
# A tick killed before its EXIT trap ran (SIGKILL) leaves its output directory
# $MEMOIRE_HOME/tmp/opencode-tick.*; the next tick, holding the lock, removes it.
# (The adapter's own work directory under $MEMOIRE_HOME/tmp/opencode-capture is
# swept by the adapter.)
#
# Kill switch: MEMOIRE_CAPTURE=off|0|false disables the tick.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MEMOIRE_HOME="${MEMOIRE_HOME:-$HOME/.memoire}"
LOG_DIR="$MEMOIRE_HOME/log"
LOG_FILE="$LOG_DIR/capture-opencode-tick.log"

log() {
  mkdir -p "$LOG_DIR" 2>/dev/null || return 0
  printf '%s %s\n' "$(date -Is)" "$*" >> "$LOG_FILE" 2>/dev/null || true
}

# Session-level kill switch, honored before any other dependency.
case "${MEMOIRE_CAPTURE:-}" in
  off|0|false) log "tick_opencode skipped=capture_disabled"; exit 0 ;;
esac
case "${MEMOIRE_CAPTURE_OPENCODE:-}" in
  on|1|true) ;;
  ""|off|0|false) exit 0 ;;
  *) log "tick_opencode skipped=opencode_switch_unrecognised"; exit 0 ;;
esac

BIN="${MEMOIRE_CAPTURE_OPENCODE_BIN:-$SCRIPT_DIR/memoire-capture-opencode.sh}"
TIMEOUT_S="${MEMOIRE_OPENCODE_TICK_TIMEOUT:-240}"
KILL_S="${MEMOIRE_OPENCODE_TICK_KILL_S:-10}"
QUIET="${MEMOIRE_TICK_QUIET_S:-120}"
SINCE="${MEMOIRE_TICK_SINCE_S:-172800}"
# Fail-open on knob typos: a nonnumeric value falls back to its default. The
# bounds must be POSITIVE — GNU timeout reads 0 as "no limit".
[[ "$TIMEOUT_S" =~ ^[1-9][0-9]*$ ]] || TIMEOUT_S=240
[[ "$KILL_S" =~ ^[1-9][0-9]*$ ]] || KILL_S=10
[[ "$QUIET" =~ ^[0-9]+$ ]] || QUIET=120
[[ "$SINCE" =~ ^[1-9][0-9]*$ ]] || SINCE=172800

# Single instance: ticks must never overlap each other. This lock is the
# OpenCode tick's own; it shares nothing with the Claude or the Codex tick's.
LOCK_FILE="$MEMOIRE_HOME/capture-opencode-tick.lock"
mkdir -p "$MEMOIRE_HOME" 2>/dev/null
HAVE_LOCK=0
if command -v flock >/dev/null 2>&1 && touch "$LOCK_FILE" 2>/dev/null; then
  exec 8>"$LOCK_FILE"
  flock -n 8 || { log "tick_opencode skipped=locked"; exit 0; }
  HAVE_LOCK=1
else
  log "tick_opencode lock unavailable, proceeding"
fi

# Output directories of an earlier tick that was killed (SIGKILL, power cut)
# before its EXIT trap ran. Only under the lock: no other tick is then alive.
if (( HAVE_LOCK )); then
  for STALE in "$MEMOIRE_HOME"/tmp/opencode-tick.*; do
    [[ -d "$STALE" && ! -L "$STALE" ]] && rm -rf -- "$STALE"
  done
fi

command -v timeout >/dev/null 2>&1 || { log "tick_opencode skipped=timeout_bin_missing"; exit 0; }
[[ -x "$BIN" ]] || { log "tick_opencode skipped=capture_bin_missing"; exit 0; }

# Output files: private, removed at exit. Content-free (the adapter's summary
# and log lines carry session ids and counts).
umask 077
mkdir -p "$MEMOIRE_HOME/tmp" 2>/dev/null
RUN_DIR="$(mktemp -d "$MEMOIRE_HOME/tmp/opencode-tick.XXXXXX" 2>/dev/null)" \
  || { log "tick_opencode skipped=no_work_dir"; exit 0; }
trap 'rm -rf -- "$RUN_DIR"' EXIT
OUT_FILE="$RUN_DIR/out" ERR_FILE="$RUN_DIR/err"

# The adapter stops itself a margin before the hard bound.
MARGIN=$((TIMEOUT_S / 4))
(( MARGIN > 30 )) && MARGIN=30
# (timeout kills itself with KILL after it had to KILL a child; the group
# redirection keeps the shell's "Killed" notice out of the journal.)
{
  timeout -k "$KILL_S" "$TIMEOUT_S" "$BIN" \
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
[[ "$SOURCE" == "archive" ]] || SOURCE=primary

# The adapter's stop line and failure lines, sanitised and bounded.
clean() { LC_ALL=C tr -c '[:print:]\n' ' ' | tr -d '"' | cut -c1-160; }
STOP_LINE="$(grep -m1 -E 'stopped cleanly on' "$ERR_FILE" 2>/dev/null | clean)"
FAIL_LINES="$(grep -E 'queue write failed|read failed|session read failed|mapping failed|selection query failed|cursor write failed|scratch write failed|no usable database|sqlite3 >= 3\.44|jq is required|queue writer not found|cannot create a work directory|refused the source|schema v[12] requested|cannot read the migration floor' "$ERR_FILE" 2>/dev/null \
  | head -n 2 | clean | paste -sd'|' -)"
EXTRA=""
(( RC == 124 || RC == 137 )) && EXTRA=" timed_out=1"
grep -q 'no database to read' "$ERR_FILE" 2>/dev/null && EXTRA="$EXTRA no_database=1"
[[ -n "$STOP_LINE" ]] && EXTRA="$EXTRA stop=\"$STOP_LINE\""
[[ -n "$FAIL_LINES" ]] && EXTRA="$EXTRA fail=\"$FAIL_LINES\""
log "tick_opencode sessions=$SESSIONS events=$EVENTS failed_sessions=$FAILED rejected=$REJECTED blobs=$BLOBS stopped=$STOPPED left=$LEFT source=$SOURCE rc=$RC$EXTRA"
if [[ "$SOURCE" == "archive" ]]; then
  REASON="$(grep -m1 -E 'falling back to the archive copy|no usable database' "$ERR_FILE" 2>/dev/null | clean)"
  log "tick_opencode source=archive reason=\"${REASON:-not recorded}\""
fi
exit 0
