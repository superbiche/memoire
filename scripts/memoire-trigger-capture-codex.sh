#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-trigger-capture-codex.sh — Codex Stop hook: make the turn that just
# ended reach the local queue within seconds. The hook carries no content: it
# runs the accepted rollout reader (memoire-capture-codex-rollout.sh --tail) on
# the one rollout, so the ids are the tick's (codex:<thread>:<ordinal>).
#
# Codex's hook budget (1 s default, registered timeout 3) makes an inline poll
# impossible, so the hook DETACHES FIRST: it parses the input, checks the
# switches, starts this same script (--run) in a session of its own with stdio
# off, and exits (well under 100 ms). The detached run re-reads the switches
# itself, polls at most 5 s in 500 ms steps until the rollout tail holds the
# turn's closing record (task_complete / turn_aborted, for this turn_id when the
# hook gave one), or a line holding the last 200 characters of
# last_assistant_message, or the size has not changed across two intervals; then
# it runs the reader. When the closing record never appears the reader runs
# anyway and the log says so (closing=absent): its turn event then comes from the
# next trigger or the tick (reader header "Turn projection"); latency only.
# The last_assistant_message tail reaches the detached run through its
# environment (MEMOIRE_TRIGGER_NEEDLE) and reaches jq the same way: never an
# argument of any process, never a file.
#
# transcript_path is nullable in the Stop input: null -> one log line, exit 0.
# Opt-in: MEMOIRE_HOOK_CAPTURE=on AND MEMOIRE_CAPTURE_CODEX=on in
# $MEMOIRE_HOME/capture-tick.env (single key, last wins, on|1|true, never
# sourced; the process environment is the fallback for the leg gate only);
# MEMOIRE_CAPTURE=off kills it. Fail-open: any error exits 0 with one log line.
# Log: $MEMOIRE_HOME/log/trigger-codex.log, one line per stage (hook, run):
# ids, sizes, counts, timings, outcome; never content.
#
# Knobs (validated and clamped): MEMOIRE_TRIGGER_POLL_MS (5000, at most 5000),
# MEMOIRE_TRIGGER_STEP_MS (500, at least 100),
# MEMOIRE_TRIGGER_READER_TIMEOUT_S (30), MEMOIRE_CAPTURE_CODEX_BIN (reader path,
# as the tick).
#
# registered-timeout-s: 3

SELF="${BASH_SOURCE[0]}"
# shellcheck source=memoire-trigger-lib.sh
source "$(dirname "$SELF")/memoire-trigger-lib.sh" || exit 0

READER="${MEMOIRE_CAPTURE_CODEX_BIN:-$TR_DIR/memoire-capture-codex-rollout.sh}"

# ---- detached run -------------------------------------------------------------------
if [[ "${1:-}" == "--run" ]]; then
  tr_begin codex run
  ROLLOUT="${2:-}"; TURN="$(tr_clean "${3:-}")"; SID="$(tr_clean "${4:-}")"
  # The needle stays in a shell variable; jq gets it through its own environment (argv is
  # readable by every local user), the reader gets neither.
  NEEDLE="${MEMOIRE_TRIGGER_NEEDLE:-}"; unset MEMOIRE_TRIGGER_NEEDLE
  tr_field "session=$SID turn=$TURN"
  tr_gates MEMOIRE_CAPTURE_CODEX
  [[ -n "$ROLLOUT" && -r "$ROLLOUT" ]] || tr_done "error=rollout-unreadable"
  command -v jq >/dev/null 2>&1 || tr_done "error=jq-missing"
  [[ -x "$READER" ]] || tr_done "error=reader-missing"
  command -v timeout >/dev/null 2>&1 || tr_done "error=timeout-missing"

  # "<closing records matching the turn> <1 when a line holds the message tail>".
  # Without a turn_id every closing record counts, and the run waits for one more
  # than the tail held when it started (an earlier turn's record is not this one's).
  codex_scan() {
    tr_tail_lines "$ROLLOUT" 262144 | sa_json_lines | MEMOIRE_TRIGGER_NEEDLE="$NEEDLE" jq -r -n --arg turn "$TURN" '
      $ENV.MEMOIRE_TRIGGER_NEEDLE as $n
      | [inputs | fromjson? | select(type == "object")] as $l
      | [$l[] | select((.type == "event_msg") and ((.payload.type? // "") | IN("task_complete", "turn_aborted"))
           and ($turn == "" or (.payload.turn_id? // "") == $turn))] as $c
      | "\($c | length) \(if $n != "" and ($l | any(.[]; [.. | strings] | any(contains($n)))) then 1 else 0 end)"' 2>/dev/null
  }
  codex_closed() {
    local c n; read -r c n < <(codex_scan)
    [[ "$c" =~ ^[0-9]+$ ]] || return 1
    [[ "$n" == 1 ]] && return 0
    if [[ -n "$TURN" ]]; then (( c > 0 )); else (( c > C0 )); fi
  }
  read -r C0 _ < <(codex_scan); [[ "$C0" =~ ^[0-9]+$ ]] || C0=0
  POLL_MS="$(tr_clamp "${MEMOIRE_TRIGGER_POLL_MS:-}" 5000 0 5000)"
  STEP_MS="$(tr_clamp "${MEMOIRE_TRIGGER_STEP_MS:-}" 500 100 5000)"
  STEP_S="$(printf '%d.%03d' $(( STEP_MS / 1000 )) $(( STEP_MS % 1000 )))"
  P0="$(sa_now_ms)" POLLS=0 CLOSING="absent" LAST="$(sa_size "$ROLLOUT")" SAME=0
  while :; do
    if codex_closed; then CLOSING="present"; break; fi
    (( $(sa_now_ms) - P0 >= POLL_MS )) && { CLOSING="timeout"; break; }
    sleep "$STEP_S"
    POLLS=$(( POLLS + 1 ))
    S="$(sa_size "$ROLLOUT")"
    if [[ "$S" == "$LAST" ]]; then SAME=$(( SAME + 1 )); else SAME=0 LAST="$S"; fi
    (( SAME >= 2 )) && { CLOSING="stable-absent"; break; }
  done
  tr_field "closing=$CLOSING polls=$POLLS poll_ms=$(( $(sa_now_ms) - P0 )) bytes=$(sa_size "$ROLLOUT")"

  # Switches re-read after the wait: a flip during the poll is honoured.
  tr_gates MEMOIRE_CAPTURE_CODEX
  TMO="$(tr_int "${MEMOIRE_TRIGGER_READER_TIMEOUT_S:-}" 30)"
  umask 077
  mkdir -p "$MEMOIRE_HOME/tmp" 2>/dev/null
  RUN_DIR="$(mktemp -d "$MEMOIRE_HOME/tmp/trigger-codex.XXXXXX" 2>/dev/null)" || tr_done "error=mktemp"
  trap 'rm -rf -- "$RUN_DIR"; tr_finish' EXIT
  timeout -k 2 "$TMO" "$READER" --tail --summary --max-seconds "$(( TMO > 5 ? TMO - 5 : 1 ))" "$ROLLOUT" </dev/null >"$RUN_DIR/out" 2>"$RUN_DIR/err"
  rc=$?
  FILES=0 EVENTS=0 FAILED=0 STOPPED=0 LEFT=0
  IFS=$'\t' read -r FILES EVENTS FAILED STOPPED LEFT < <(tail -n 1 "$RUN_DIR/out" 2>/dev/null)
  for n in FILES EVENTS FAILED STOPPED LEFT; do [[ "${!n}" =~ ^[0-9]+$ ]] || printf -v "$n" 0; done
  tr_field "events=$EVENTS failed=$FAILED rc=$rc"
  if (( rc == 124 || rc == 137 )); then TR_STATUS="error=timeout"
  elif grep -q 'skipped=locked' "$RUN_DIR/err" 2>/dev/null; then TR_STATUS="skipped=locked"
  elif (( rc != 0 || FAILED > 0 )); then TR_STATUS="error=reader"
  elif (( EVENTS > 0 )); then TR_STATUS="captured"
  else TR_STATUS="empty"; fi
  exit 0
fi

# ---- hook: parse, gate, detach, exit ---------------------------------------------
tr_begin codex hook
HOOK_JSON="$(cat 2>/dev/null || true)"
tr_gates MEMOIRE_CAPTURE_CODEX
command -v jq >/dev/null 2>&1 || tr_done "error=jq-missing"

{ IFS= read -r SID; IFS= read -r ROLLOUT; IFS= read -r TURN; IFS= read -r NEEDLE_B64; } < <(
  printf '%s' "$HOOK_JSON" | jq -r '
    def c: tostring | .[0:512] | gsub("[\u0000-\u001f\u007f-\u009f  ]"; "") | .[0:128];
    (.session_id // "" | c), (.transcript_path // "" | if type == "string" then gsub("[\n\r]"; "") else "" end),
    (.turn_id // "" | c),
    ((.last_assistant_message // "" | if type == "string" then sub("\\s+$"; "") | .[-200:] else "" end) | @base64)' 2>/dev/null) || true
SID="$(tr_clean "${SID:-}")"; TURN="$(tr_clean "${TURN:-}")"; tr_field "session=$SID turn=$TURN"
[[ -n "${ROLLOUT:-}" ]] || tr_done "transcript_path=null"

MEMOIRE_TRIGGER_NEEDLE="$(printf '%s' "${NEEDLE_B64:-}" | base64 -d 2>/dev/null)" tr_spawn /dev/null bash "$SELF" --run "$ROLLOUT" "$TURN" "$SID"
TR_STATUS="spawned"
exit 0
