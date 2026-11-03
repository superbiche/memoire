#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-trigger-capture-opencode.sh <sessionID> — OpenCode session.idle
# trigger. Spawned DETACHED by the plugin's `event` hook (scripts/opencode-
# plugin/memoire.js; the plugin never awaits it); it carries no content: it
# runs the accepted reader (memoire-capture-opencode.sh) on the one session, so
# the ids are the tick's (opencode:part:<part_id>:u<ms>, opencode:turn:<msg>).
#
#   1. switches, re-read here (the plugin child does not inherit the tick unit's
#      EnvironmentFile): MEMOIRE_CAPTURE=off kills; MEMOIRE_HOOK_CAPTURE and
#      MEMOIRE_CAPTURE_OPENCODE must both be on|1|true in
#      $MEMOIRE_HOME/capture-tick.env (single key, last wins, never sourced;
#      the process environment is the fallback for the leg gate only).
#   2. live-path rule (operator ruling): runs only when the resolved database is
#      the LIVE one (~/.local/share/opencode/opencode.db: MEMOIRE_OPENCODE_DB
#      unset, or resolving to it) and exists; on the archive, or a database that
#      is not the live path, it logs `source=archive skipped` and exits: a
#      trigger on a lagging copy measures nothing. The database parameters of
#      the env file (MEMOIRE_OPENCODE_DB, _DB_FALLBACK, _TOOL_OUTPUT_DIR[_FALLBACK])
#      are applied as the tick's unit applies them, when the environment lacks them.
#   3. one wrapper per session at a time (flock on hooks/oc-<sid>.lock). An idle
#      that finds the lock held touches hooks/oc-<sid>.pending and leaves; the
#      running wrapper, after its run, removes the marker and runs the reader
#      once more; after releasing the lock it looks at the marker once more and, when an
#      idle marked it meanwhile, takes the lock back for a second cycle (two cycles at
#      most: a storm costs a bounded number of reads, a marker left after that waits for
#      the next idle or the tick).
#   4. 5 s debounce (MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S): runs of one session
#      start at least that far apart (state file hooks/oc-<sid>.last, epoch ms);
#      a wrapper that comes early waits out the rest instead of dropping.
#   5. settle (MEMOIRE_OPENCODE_TRIGGER_SETTLE_S, 3): sleep, then
#      MEMOIRE_OPENCODE_STUCK_MS=0 memoire-capture-opencode.sh --quiet-for
#      <settle-1> --max-seconds <N> <sessionID>. The completed message is then
#      older than the reader's quiet gate and chunk cut; STUCK_MS=0 lifts the
#      stuck gate (an explicit session run must not be gated by the tick's rule).
#
# State files of sessions idle for more than 7 days are swept at start (50 files at
# most per run); the log is not rotated here: it is the operator's to rotate.
# Log: $MEMOIRE_HOME/log/trigger-opencode.log, one line per run (session id,
# rows/events, elapsed, outcome, source as the reader reports it); never content. Fail-open: exit 0 always.
# Knobs (validated): MEMOIRE_OPENCODE_TRIGGER_MAX_SECONDS (30),
# MEMOIRE_CAPTURE_OPENCODE_BIN (reader path, as the tick).

SELF="${BASH_SOURCE[0]}"
# shellcheck source=memoire-trigger-lib.sh
source "$(dirname "$SELF")/memoire-trigger-lib.sh" || exit 0
tr_begin opencode run

SID="$(tr_clean "${1:-}")"; tr_field "session=$SID"
READER="${MEMOIRE_CAPTURE_OPENCODE_BIN:-$TR_DIR/memoire-capture-opencode.sh}"
[[ "$SID" =~ ^[A-Za-z0-9_-]{1,128}$ ]] || tr_done "error=bad-session-id"
tr_gates MEMOIRE_CAPTURE_OPENCODE

# The tick unit's EnvironmentFile carries the database parameters; the plugin's
# child does not inherit it. Only these four names, only when the environment
# lacks them.
for k in MEMOIRE_OPENCODE_DB MEMOIRE_OPENCODE_DB_FALLBACK MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR_FALLBACK; do
  if [[ -z "${!k:-}" ]]; then v="$(tr_key_value "$k")"; [[ -n "$v" ]] && export "$k=$v"; fi
done
LIVE_DB="$HOME/.local/share/opencode/opencode.db"
DB="${MEMOIRE_OPENCODE_DB:-${OPENCODE_DB:-$LIVE_DB}}"
live_path() { [[ "$(readlink -f -- "$1" 2>/dev/null)" == "$(readlink -f -- "$LIVE_DB" 2>/dev/null)" ]]; }
if ! live_path "$DB" || [[ ! -f "$DB" ]]; then tr_field "source=archive"; tr_done "skipped"; fi

command -v timeout >/dev/null 2>&1 || tr_done "error=timeout-missing"
command -v flock >/dev/null 2>&1 || tr_done "error=flock-missing"
[[ -x "$READER" ]] || tr_done "error=reader-missing"
HOOKS="$MEMOIRE_HOME/hooks"
mkdir -p "$HOOKS" 2>/dev/null || tr_done "error=state-dir"
LOCK="$HOOKS/oc-$SID.lock" LASTF="$HOOKS/oc-$SID.last" PENDING="$HOOKS/oc-$SID.pending"
exec 9>>"$LOCK" 2>/dev/null || tr_done "error=lock-open"
if ! flock -n 9; then
  touch "$PENDING" 2>/dev/null
  tr_done "pending-marked"
fi

# State files of sessions idle for more than 7 days (oc-<sid>.{lock,last,pending}) are
# removed here, 50 files at most per run; a session counts as idle by its .last file
# (its own age when there is none), and a lock still held is left alone. The log is not
# rotated here: it is the operator's, as the tick logs are.
sweep_state() {
  local n=0 f base sid l
  while IFS= read -r f; do
    base="${f##*/}"; sid="${base#oc-}"; sid="${sid%.*}"
    [[ "$sid" == "$SID" ]] && continue
    l="$HOOKS/oc-$sid.last"
    [[ "$f" != "$l" && -e "$l" && -z "$(find "$l" -maxdepth 0 -mtime +7 2>/dev/null)" ]] && continue
    [[ "$f" == *.lock ]] && ! flock -n "$f" true 2>/dev/null && continue
    rm -f -- "$f" 2>/dev/null
    (( ++n >= 50 )) && break
  done < <(find "$HOOKS" -maxdepth 1 -type f -name 'oc-*' \( -name '*.lock' -o -name '*.last' -o -name '*.pending' \) -mtime +7 2>/dev/null)
  return 0
}
sweep_state

DEBOUNCE_S="$(tr_int "${MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S:-}" 5 0)"
SETTLE_S="$(tr_int "${MEMOIRE_OPENCODE_TRIGGER_SETTLE_S:-}" 3 0)"
MAX_S="$(tr_int "${MEMOIRE_OPENCODE_TRIGGER_MAX_SECONDS:-}" 30)"
QUIET_S=$(( SETTLE_S > 0 ? SETTLE_S - 1 : 0 ))
RUNS=0 EVENTS=0 FAILED=0 RC=0 OUTCOME="empty" SOURCE="live" CYCLES=0
umask 077
mkdir -p "$MEMOIRE_HOME/tmp" 2>/dev/null
RUN_DIR="$(mktemp -d "$MEMOIRE_HOME/tmp/trigger-opencode.XXXXXX" 2>/dev/null)" || tr_done "error=mktemp"
trap 'rm -rf -- "$RUN_DIR"; tr_finish' EXIT

one_run() {
  # debounce: wait out the rest of the interval since the previous start
  local last now wait_ms
  last="$(cat "$LASTF" 2>/dev/null)"; [[ "$last" =~ ^[0-9]+$ ]] || last=0
  now="$(sa_now_ms)"; wait_ms=$(( last + DEBOUNCE_S * 1000 - now ))
  if (( wait_ms > 0 )); then sleep "$(printf '%d.%03d' $(( wait_ms / 1000 )) $(( wait_ms % 1000 )))"; fi
  printf '%s' "$(sa_now_ms)" > "$LASTF" 2>/dev/null
  (( SETTLE_S > 0 )) && sleep "$SETTLE_S"
  # the switches are read again before the read itself
  case "${MEMOIRE_CAPTURE:-}" in off|0|false) OUTCOME="capture-off"; return 1 ;; esac
  tr_capture_off_file && { OUTCOME="disabled"; return 1; }
  sa_switch_on MEMOIRE_HOOK_CAPTURE && tr_leg_switch MEMOIRE_CAPTURE_OPENCODE || { OUTCOME="disabled"; return 1; }
  RUNS=$(( RUNS + 1 ))
  MEMOIRE_OPENCODE_STUCK_MS=0 timeout -k 5 $(( MAX_S + 10 )) "$READER" --db "$DB" --summary --quiet-for "$QUIET_S" --max-seconds "$MAX_S" "$SID" \
    </dev/null >"$RUN_DIR/out" 2>"$RUN_DIR/err" 9>&-
  RC=$?
  local e f src
  IFS=$'\t' read -r _ e f _ _ _ _ src < <(tail -n 1 "$RUN_DIR/out" 2>/dev/null)
  # the reader falls back to the archive by its own rule; the log says which source it read
  [[ "$src" == archive ]] && SOURCE="archive"
  [[ "$e" =~ ^[0-9]+$ ]] && EVENTS=$(( EVENTS + e ))
  [[ "$f" =~ ^[0-9]+$ ]] && FAILED=$(( FAILED + f ))
  if (( RC == 124 || RC == 137 )); then OUTCOME="error=timeout"
  elif grep -q 'skipped=locked' "$RUN_DIR/err" 2>/dev/null; then OUTCOME="skipped=locked"
  elif (( RC != 0 || FAILED > 0 )); then OUTCOME="error=reader"
  elif (( EVENTS > 0 )); then OUTCOME="captured"
  else OUTCOME="empty"; fi
  return 0
}

# One cycle: a run, then one trailing run when an idle marked the session meanwhile.
# After the lock is released a marker left by an idle that arrived after the last
# check (during the trailing run, or between the check and the release) is looked at
# again: the wrapper takes the lock back and runs once more. Two cycles at most (the
# debounce spaces the runs); a marker still there after that is left for the next
# idle or the tick (pending-left).
while :; do
  CYCLES=$(( CYCLES + 1 ))
  rm -f -- "$PENDING" 2>/dev/null
  one_run || { TR_STATUS="$OUTCOME"; exit 0; }
  if [[ -e "$PENDING" ]]; then
    rm -f -- "$PENDING" 2>/dev/null
    tr_field "trailing=1"
    one_run || { TR_STATUS="$OUTCOME"; exit 0; }
  fi
  exec 9>&-
  [[ -e "$PENDING" ]] && (( CYCLES < 2 )) || break
  tr_field "relock=1"
  exec 9>>"$LOCK" 2>/dev/null && flock -n 9 || break
done
[[ -e "$PENDING" ]] && tr_field "pending-left=1"
tr_field "runs=$RUNS events=$EVENTS failed=$FAILED rc=$RC source=$SOURCE"
TR_STATUS="$OUTCOME"
exit 0
