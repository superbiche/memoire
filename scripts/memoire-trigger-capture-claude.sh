#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-trigger-capture-claude.sh — Claude Code Stop hook: make the turn that
# just ended reach the local queue within seconds instead of at the next tick.
# The hook carries no content: it runs the accepted transcript reader
# (memoire-capture-claude-transcripts.sh --tail) on the one transcript, so the
# ids are the tick's (cc-turn:<session>:<uuid>) and the hub collapses duplicates.
#
# The transcript does not yet hold the turn's final assistant message when Stop
# fires (probe 5); it appears about 115 ms later (probe 4). So the hook polls
# first (amendment C1): at most 1.5 s in 250 ms steps, on jq-decoded text
# (never a raw grep of JSONL), stopping when the newest assistant text of the
# transcript contains the last 200 characters of last_assistant_message, or when
# the file size has not changed across two intervals (three samples); then it
# starts the reader in a session of its own (this same script, --run), waits at
# most 1 s for it, and exits 0. Worst case inline: 1.5 s (on the clock, the last step cut) + 1 s,
# under the 5 s timeout; a plain Stop hook (liaison has no async passthrough), so every turn
# end waits for it. A reader still running at exit finishes on its own and logs
# its own line: latency only, never loss (the tick is the recovery leg).
#
# Opt-in: MEMOIRE_HOOK_CAPTURE=on in $MEMOIRE_HOME/capture-tick.env, read per
# invocation (single key, last assignment wins, on|1|true, never sourced), by the
# hook AND again by the detached run. MEMOIRE_CAPTURE=off kills it. Skipped:
# MEMOIRE_ATTRIB_FORK set, stop_hook_active true (a Stop re-entered by a blocking
# hook is not a new turn). Fail-open: any error exits 0 with one log line.
# Log: $MEMOIRE_HOME/log/trigger-claude.log, one line per stage (hook, run):
# session id, sizes, counts, timings, outcome; never content.
#
# Knobs (validated and clamped; junk falls back): MEMOIRE_TRIGGER_POLL_MS (1500,
# at most 1500), MEMOIRE_TRIGGER_STEP_MS (250, at least 100),
# MEMOIRE_TRIGGER_WAIT_MS (1000, at most 1000),
# MEMOIRE_TRIGGER_READER_TIMEOUT_S (30), MEMOIRE_CAPTURE_CC_BIN (reader path, as
# the tick).
#
# registered-timeout-s: 5

SELF="${BASH_SOURCE[0]}"
# shellcheck source=memoire-trigger-lib.sh
source "$(dirname "$SELF")/memoire-trigger-lib.sh" || exit 0

READER="${MEMOIRE_CAPTURE_CC_BIN:-$TR_DIR/memoire-capture-claude-transcripts.sh}"

# ---- detached run: re-read the switches, run the reader, log one line --------
if [[ "${1:-}" == "--run" ]]; then
  tr_begin claude run
  TRANSCRIPT="${2:-}"; SID="$(tr_clean "${3:-}")"; tr_field "session=$SID"
  [[ -n "${MEMOIRE_ATTRIB_FORK:-}" ]] && tr_done "attrib-fork"
  tr_gates
  [[ -n "$TRANSCRIPT" && -r "$TRANSCRIPT" ]] || tr_done "error=transcript-unreadable"
  [[ -x "$READER" ]] || tr_done "error=reader-missing"
  command -v timeout >/dev/null 2>&1 || tr_done "error=timeout-missing"
  TMO="$(tr_int "${MEMOIRE_TRIGGER_READER_TIMEOUT_S:-}" 30)"
  umask 077
  mkdir -p "$MEMOIRE_HOME/tmp" 2>/dev/null
  RUN_DIR="$(mktemp -d "$MEMOIRE_HOME/tmp/trigger-claude.XXXXXX" 2>/dev/null)" || tr_done "error=mktemp"
  trap 'rm -rf -- "$RUN_DIR"; tr_finish' EXIT
  tr_field "bytes=$(sa_size "$TRANSCRIPT")"
  timeout -k 2 "$TMO" "$READER" --tail --emit-sessions "$TRANSCRIPT" </dev/null >"$RUN_DIR/out" 2>"$RUN_DIR/err"
  rc=$?
  turns="$(awk -F'\t' '{n += $3} END {print n + 0}' "$RUN_DIR/out" 2>/dev/null)"
  tr_field "turns=${turns:-0} rc=$rc"
  if (( rc == 124 || rc == 137 )); then TR_STATUS="error=timeout"
  elif grep -q 'skipped=locked' "$RUN_DIR/err" 2>/dev/null; then TR_STATUS="skipped=locked"
  elif (( rc != 0 )); then TR_STATUS="error=reader"
  elif (( ${turns:-0} > 0 )); then TR_STATUS="captured"
  else TR_STATUS="empty"; fi
  exit 0
fi

# ---- hook ------------------------------------------------------------------------
tr_begin claude hook
# The hook input is read before any gate: a hook that exits without reading its
# stdin can hand the harness an EPIPE.
HOOK_JSON="$(cat 2>/dev/null || true)"
[[ -n "${MEMOIRE_ATTRIB_FORK:-}" ]] && tr_done "attrib-fork"
tr_gates
command -v jq >/dev/null 2>&1 || tr_done "error=jq-missing"

{ IFS= read -r SID; IFS= read -r TRANSCRIPT; IFS= read -r ACTIVE; IFS= read -r NEEDLE_B64; } < <(
  printf '%s' "$HOOK_JSON" | jq -r '
    def c: tostring | .[0:512] | gsub("[\u0000-\u001f\u007f-\u009f  ]"; "") | .[0:128];
    (.session_id // "" | c), (.transcript_path // "" | if type == "string" then . | gsub("[\n\r]"; "") else "" end),
    (.stop_hook_active // false | tostring),
    ((.last_assistant_message // "" | if type == "string" then sub("\\s+$"; "") | .[-200:] else "" end) | @base64)' 2>/dev/null) || true
SID="$(tr_clean "${SID:-}")"; tr_field "session=$SID"
[[ "${ACTIVE:-}" == "true" ]] && tr_done "stop-hook-active"
[[ -n "${TRANSCRIPT:-}" && -r "$TRANSCRIPT" ]] || tr_done "error=transcript-unreadable"
NEEDLE="$(printf '%s' "${NEEDLE_B64:-}" | base64 -d 2>/dev/null)" || NEEDLE=""

# Bounded poll on decoded text: the newest assistant text block of the tail. The
# needle reaches jq through its environment only (argv is readable by every local
# user through /proc).
claude_final_text() { # <file> <needle>
  tr_tail_lines "$1" 262144 | sa_json_lines | MEMOIRE_TRIGGER_NEEDLE="$2" jq -n '
    $ENV.MEMOIRE_TRIGGER_NEEDLE as $n
    | [inputs | fromjson? | select(type == "object" and .type == "assistant" and ((.isSidechain // false) | not))
     | [(.message.content? // empty) | if type == "array" then .[] else empty end
        | select(type == "object" and .type == "text") | .text // empty]
     | select(length > 0)] | (last // []) | any(.[]; contains($n))' 2>/dev/null | grep -q true
}
# Every knob is clamped: no environment can push the inline path past its budget
# (poll <= 1500 ms, step >= 100 ms, wait <= 1000 ms).
POLL_MS="$(tr_clamp "${MEMOIRE_TRIGGER_POLL_MS:-}" 1500 0 1500)"
STEP_MS="$(tr_clamp "${MEMOIRE_TRIGGER_STEP_MS:-}" 250 100 1500)"
WAIT_MS="$(tr_clamp "${MEMOIRE_TRIGGER_WAIT_MS:-}" 1000 0 1000)"
P0="$(sa_now_ms)" POLLS=0 WHY="bound" SIZE0="$(sa_size "$TRANSCRIPT")" LAST="$SIZE0" SAME=0
while :; do
  if [[ -n "$NEEDLE" ]] && claude_final_text "$TRANSCRIPT" "$NEEDLE"; then WHY="signal"; break; fi
  # the bound is on the clock: the last sleep is cut to what remains, and nothing
  # (no scan) runs after it
  REMAIN=$(( POLL_MS - ($(sa_now_ms) - P0) ))
  (( REMAIN <= 0 )) && break
  NAP=$(( STEP_MS < REMAIN ? STEP_MS : REMAIN ))
  sleep "$(printf '%d.%03d' $(( NAP / 1000 )) $(( NAP % 1000 )))"
  POLLS=$(( POLLS + 1 ))
  S="$(sa_size "$TRANSCRIPT")"
  if [[ "$S" == "$LAST" ]]; then SAME=$(( SAME + 1 )); else SAME=0 LAST="$S"; fi
  (( SAME >= 2 )) && { WHY="stable"; break; }
  (( $(sa_now_ms) - P0 >= POLL_MS )) && break
done
tr_field "polls=$POLLS poll_end=$WHY poll_ms=$(( $(sa_now_ms) - P0 )) bytes=$(sa_size "$TRANSCRIPT")"

# The reader runs detached (own session, own log); the hook waits for it at most
# WAIT_MS, then leaves: a reader still running finishes by itself.
tr_spawn /dev/null bash "$SELF" --run "$TRANSCRIPT" "$SID"
if tr_wait_pid "$TR_PID" "$WAIT_MS"; then tr_field "run=done"; else tr_field "run=detached"; fi
TR_STATUS="spawned"
exit 0
