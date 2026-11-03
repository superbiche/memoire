#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-capture-tick.sh — the quiet-timer body of the streaming capture
# (operator ruling 2026-09-08, Fibery #342). Every N minutes it captures the
# TAIL of every recently active Claude Code transcript, so assistant text
# lands in Memoire within one tick of being written instead of waiting for
# session end. The other two capture moments are the checkpoint hook's
# PreCompact and SessionEnd fires (memoire-derive-auto.sh).
#
# The whole tick is one bounded call to the CC transcript adapter with
# --tail --quiet-for --since --emit-sessions; the per-transcript byte cursors
# live in the adapter, so a tick over an idle corpus is a stat-only scan.
# Nothing here derives, opens a store, or holds a model credential — the
# harness seam is capture-only (memoire:D231).
#
# Knobs (all defaulted):
#   MEMOIRE_HOME              default ~/.memoire (log, lock, cursor root)
#   MEMOIRE_CC_PROJECTS_DIR   default ~/.claude/projects
#   MEMOIRE_TICK_QUIET_S      default 120     — skip files touched more recently
#   MEMOIRE_TICK_SINCE_S      default 172800  — 48h scan bound
#   MEMOIRE_TICK_TIMEOUT      default 600     — wall-clock bound on the adapter
#   MEMOIRE_TICK_NOTIFY       default 0       — 1 POSTs /v1/derive/notify per
#                                               (project, session) that gained turns
#   MEMOIRE_CAPTURE_CC_BIN    adapter path override (mirrors derive-auto)
#   MEMOIRE_BASE_URL / MEMOIRE_TOKEN_FILE / MEMOIRE_NOTIFY_TIMEOUT — notify only
#
# Kill switches: MEMOIRE_CAPTURE=off|0|false disables the tick entirely;
# MEMOIRE_AUTO_DERIVE=0 disables only the notify.
#
# Notify is OFF by default, deliberately: the hourly sweep already enumerates
# outstanding work from uncovered events, so the tick changes what a derivation
# window CONTAINS, not how often one runs. Notify-on-tick is a priority hint,
# filed as Fibery #343.
#
# Every failure a timer should not flap on (missing adapter, missing jq, no
# projects dir, a second tick already running) is one log marker and exit 0.
# The summary's rc= is the ADAPTER's exit code, reported honestly: rc=1 with
# files_new=0 is the ordinary empty scan (the adapter's long-standing "no
# transcript files given" exit, e.g. when every transcript in the --since
# window is younger than the quiet gate), not a failure to chase.
# The first tick after install bootstraps every cursorless transcript in the
# window and may well hit MEMOIRE_TICK_TIMEOUT; cursors are written per file,
# so the next tick resumes exactly where it stopped.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MEMOIRE_HOME="${MEMOIRE_HOME:-$HOME/.memoire}"
LOG_DIR="$MEMOIRE_HOME/log"
LOG_FILE="$LOG_DIR/capture-tick.log"

log() {
  mkdir -p "$LOG_DIR" 2>/dev/null || return 0
  printf '%s %s\n' "$(date -Is)" "$*" >> "$LOG_FILE" 2>/dev/null || true
}

# liaison:D239 session-level kill switch, honored before any other dependency.
case "${MEMOIRE_CAPTURE:-}" in
  off|0|false) log "tick skipped=capture_disabled"; exit 0 ;;
esac

PROJECTS_DIR="${MEMOIRE_CC_PROJECTS_DIR:-$HOME/.claude/projects}"
QUIET="${MEMOIRE_TICK_QUIET_S:-120}"
SINCE="${MEMOIRE_TICK_SINCE_S:-172800}"
TICK_TIMEOUT="${MEMOIRE_TICK_TIMEOUT:-600}"
NOTIFY="${MEMOIRE_TICK_NOTIFY:-0}"
CAPTURE_BIN="${MEMOIRE_CAPTURE_CC_BIN:-$SCRIPT_DIR/memoire-capture-claude-transcripts.sh}"
BASE_URL="${MEMOIRE_BASE_URL:-http://${MEMOIRE_ADDR:-127.0.0.1:37778}}"
TOKEN_FILE="${MEMOIRE_TOKEN_FILE:-$MEMOIRE_HOME/token}"
NOTIFY_TIMEOUT="${MEMOIRE_NOTIFY_TIMEOUT:-10}"
# Fail-open on knob typos: a nonnumeric value falls back to its default. The
# adapter timeout must be POSITIVE — GNU timeout reads 0 as "no limit".
[[ "$QUIET" =~ ^[0-9]+$ ]] || QUIET=120
[[ "$SINCE" =~ ^[1-9][0-9]*$ ]] || SINCE=172800
[[ "$TICK_TIMEOUT" =~ ^[1-9][0-9]*$ ]] || TICK_TIMEOUT=600
[[ "$NOTIFY_TIMEOUT" =~ ^[1-9][0-9]*$ ]] || NOTIFY_TIMEOUT=10

# Single instance: ticks must never overlap each other (they would contend on
# every cursor lock and do the same work twice).
LOCK_FILE="$MEMOIRE_HOME/capture-tick.lock"
mkdir -p "$MEMOIRE_HOME" 2>/dev/null
if command -v flock >/dev/null 2>&1 && touch "$LOCK_FILE" 2>/dev/null; then
  exec 8>"$LOCK_FILE"
  flock -n 8 || { log "tick skipped=locked"; exit 0; }
else
  log "tick lock unavailable, proceeding"
fi

command -v jq >/dev/null 2>&1 || { log "tick skipped=no_jq"; exit 0; }
[[ -x "$CAPTURE_BIN" ]] || { log "tick skipped=capture_bin_missing"; exit 0; }
[[ -d "$PROJECTS_DIR" ]] || { log "tick skipped=no_projects_dir"; exit 0; }
command -v timeout >/dev/null 2>&1 || { log "tick skipped=timeout_bin_missing"; exit 0; }

OUT_FILE="$(mktemp "${TMPDIR:-/tmp}/memoire-capture-tick.XXXXXX")" \
  || { log "tick skipped=mktemp_failed"; exit 0; }
trap 'rm -f "$OUT_FILE"' EXIT

# Adapter stderr is dropped on purpose: the tick's contract is ONE summary
# line per run (plus one line per notified session). Per-file detail belongs
# to a manual adapter run.
timeout "$TICK_TIMEOUT" "$CAPTURE_BIN" \
  --tail --quiet-for "$QUIET" --since "$SINCE" --emit-sessions "$PROJECTS_DIR" \
  >"$OUT_FILE" 2>/dev/null
RC=$?

FILES_NEW=0
TURNS=0
while IFS=$'\t' read -r tsv_project tsv_session tsv_turns; do
  [[ -n "$tsv_project" && -n "$tsv_session" ]] || continue
  [[ "$tsv_turns" =~ ^[0-9]+$ ]] || continue
  FILES_NEW=$((FILES_NEW + 1))
  TURNS=$((TURNS + tsv_turns))
done < "$OUT_FILE"

# Distinct (project, session) pairs — one transcript can carry more than one
# session id after a resume, and two transcripts can carry the same one.
SESSIONS_FILE="$OUT_FILE.sessions"
cut -f1,2 < "$OUT_FILE" | LC_ALL=C sort -u > "$SESSIONS_FILE" 2>/dev/null
SESSIONS="$(grep -c . "$SESSIONS_FILE" 2>/dev/null)"
[[ "$SESSIONS" =~ ^[0-9]+$ ]] || SESSIONS=0

NOTIFY_SUMMARY="off"
if [[ "$NOTIFY" == "1" && "$SESSIONS" -gt 0 ]]; then
  if [[ "${MEMOIRE_AUTO_DERIVE:-1}" == "0" ]]; then
    NOTIFY_SUMMARY="off_auto_derive_disabled"
  elif ! command -v curl >/dev/null 2>&1; then
    NOTIFY_SUMMARY="off_no_curl"
  elif [[ ! -r "$TOKEN_FILE" ]]; then
    NOTIFY_SUMMARY="off_no_token"
  else
    ok=0
    total=0
    while IFS=$'\t' read -r np ns; do
      [[ -n "$np" && -n "$ns" ]] || continue
      total=$((total + 1))
      # Body built by jq (a hostile project key cannot malform it); the bearer
      # travels via a process-substitution header file, never argv.
      body="$(jq -cn --arg p "$np" --arg s "$ns" '{project: $p, session_id: $s}')"
      code="$(curl -s -o /dev/null -w '%{http_code}' -m "$NOTIFY_TIMEOUT" \
        -H @<(printf 'Authorization: Bearer %s' "$(tr -d '\r\n' < "$TOKEN_FILE")") \
        -H 'Content-Type: application/json' \
        -X POST -d "$body" \
        "$BASE_URL/v1/derive/notify" 2>/dev/null)" || code="curl_failed"
      [[ "$code" =~ ^2[0-9][0-9]$ ]] && ok=$((ok + 1))
      log "tick notify project=$np session=$ns code=$code"
    done < "$SESSIONS_FILE"
    NOTIFY_SUMMARY="$ok/$total"
  fi
fi
rm -f "$SESSIONS_FILE" 2>/dev/null

log "tick files_new=$FILES_NEW turns=$TURNS sessions=$SESSIONS notify=$NOTIFY_SUMMARY rc=$RC"
exit 0
