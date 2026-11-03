#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-derive-auto.sh — Claude Code SESSION CHECKPOINT hook. Since L0010
# (2026-11-01) it captures only: the derive intent below is opt-in through
# MEMOIRE_DERIVE_NOTIFY=1 (derivation retired; Hindsight owns derived memory).
# Before that it was NOTIFY-ONLY since R-10.2 (memoire:D165, Q1 ruling).
#
# One script serves two hook events (their payloads share session_id, cwd and
# transcript_path): SessionEnd, and PreCompact — compaction is a checkpoint,
# not an ending, and waiting for session end left every mid-session derivation
# window holding tool events without the assistant text that explains them
# (operator ruling 2026-09-08, Fibery #342). The quiet timer
# (memoire-capture-tick.sh) is the third moment. Re-fires are cheap by
# construction: the notify is idempotent server-side and the capture adapter
# runs --tail, so a checkpoint ships only the bytes appended since the last
# one. The hook must ALWAYS exit 0 — a PreCompact hook exiting 2 would BLOCK
# compaction.
#
# The hook does exactly two things, both fail-open:
#   1. Captures the session's own transcript TAIL into the LOCAL file queue
#      (live turn ingest, 2026-07-12 — unchanged: capture stays per-machine
#      by nature; on a hub client the forwarder ships the queue).
#   2. POSTs a derive intent to the API: POST /v1/derive/notify
#      {project, session_id}. Recording an intent NEVER triggers immediate
#      derivation — the hub's hourly sweep serves intents and journals the
#      notify->completion latency (Q1's mandatory instrumentation).
#
# What it deliberately NO LONGER does (R-10 service axis): run a derive
# binary, open the store, read db.env, or hold model credentials. A session
# machine needs only the queue, the resolver, and a client token. Notify is
# idempotent server-side (same (project, session) collapses into one
# intent), so hook re-fires are safe.
#
# - Kill switch: MEMOIRE_AUTO_DERIVE=0 short-circuits to an instant clean
#   exit (one log marker, no capture, no network). Same switch as the sweep.
# - MEMOIRE_BASE_URL selects the API (default local). On a thin client set
#   it to the hub URL; MEMOIRE_TOKEN_FILE names the bearer (0600, never
#   argv — the header travels via process substitution).

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MEMOIRE_HOME="${MEMOIRE_HOME:-$HOME/.memoire}"
BASE_URL="${MEMOIRE_BASE_URL:-http://${MEMOIRE_ADDR:-127.0.0.1:37778}}"
TOKEN_FILE="${MEMOIRE_TOKEN_FILE:-$MEMOIRE_HOME/token}"
CAPTURE_BIN="${MEMOIRE_CAPTURE_CC_BIN:-$SCRIPT_DIR/memoire-capture-claude-transcripts.sh}"
CAPTURE_TIMEOUT="${MEMOIRE_CAPTURE_TIMEOUT:-300}"
NOTIFY_TIMEOUT="${MEMOIRE_NOTIFY_TIMEOUT:-10}"
QUEUE_DIR="${MEMOIRE_QUEUE_DIR:-$MEMOIRE_HOME/queue}"
DRAIN_TIMEOUT="${MEMOIRE_DRAIN_TIMEOUT:-60}"
# Fail-open on knob typos (ortho review 2026-07-12): a nonnumeric timeout
# falls back to its default. CAPTURE_TIMEOUT must be POSITIVE: GNU timeout
# treats 0 as "no limit", which would reintroduce the unbounded-adapter hang.
[[ "$CAPTURE_TIMEOUT" =~ ^[1-9][0-9]*$ ]] || CAPTURE_TIMEOUT=300
[[ "$NOTIFY_TIMEOUT" =~ ^[1-9][0-9]*$ ]] || NOTIFY_TIMEOUT=10
[[ "$DRAIN_TIMEOUT" =~ ^[0-9]+$ ]] || DRAIN_TIMEOUT=60
LOG_DIR="$MEMOIRE_HOME/log"
LOG_FILE="$LOG_DIR/derive-auto.log"

fail_open() { exit 0; }

# The attribution fork (memoire-attribute-claude.py) is a headless session of its
# own, and the store never receives it: its checkpoints capture nothing and
# announce no intent. Silent, before the kill switch's marker and any input.
[[ -n "${MEMOIRE_ATTRIB_FORK:-}" ]] && exit 0

# Kill switch: log one marker line (best effort) and exit before any other
# dependency or input check.
if [[ "${MEMOIRE_AUTO_DERIVE:-1}" == "0" ]]; then
  mkdir -p "$LOG_DIR" 2>/dev/null \
    && printf '%s auto skipped=auto_derive_disabled\n' "$(date -Is)" >> "$LOG_FILE" 2>/dev/null
  exit 0
fi

# jq is a SHARED prerequisite (hook-input parsing feeds both actions).
command -v jq >/dev/null 2>&1 || fail_open
# curl/token are NOTIFY-ONLY prerequisites and must never suppress the
# transcript capture (round-1 MED-5: capture custody is local and needs no
# network or bearer — a missing token during onboarding must not lose the
# transcript). Their absence downgrades notify to an honest log marker.
# L0010 (ruling a13 = A, 2026-11-01): derivation is retired, so the derive
# intent is off by default; the checkpoint now only captures. Same opt-in
# shape as the quiet tick's MEMOIRE_TICK_NOTIFY.
NOTIFY_SKIP=""
[[ "${MEMOIRE_DERIVE_NOTIFY:-0}" == "1" ]] || NOTIFY_SKIP="off_derivation_retired"
[[ -n "$NOTIFY_SKIP" ]] || command -v curl >/dev/null 2>&1 || NOTIFY_SKIP="skipped_no_curl"
[[ -n "$NOTIFY_SKIP" || -r "$TOKEN_FILE" ]] || NOTIFY_SKIP="skipped_no_token"

HOOK_JSON="$(cat 2>/dev/null || true)"
SESSION_ID="$(printf '%s' "$HOOK_JSON" | jq -r '.session_id // empty' 2>/dev/null | tr -cd '[:alnum:]-')"
# Which checkpoint fired, and why. Both are log-only and character-scrubbed:
# they never reach argv or a request body. PreCompact carries `trigger`
# (manual|auto), SessionEnd carries `reason`.
HOOK_EVENT="$(printf '%s' "$HOOK_JSON" | jq -r '.hook_event_name // empty' 2>/dev/null | tr -cd '[:alnum:]_-')"
[[ -n "$HOOK_EVENT" ]] || HOOK_EVENT="SessionEnd"
HOOK_CAUSE="$(printf '%s' "$HOOK_JSON" | jq -r '.trigger // .reason // empty' 2>/dev/null | tr -cd '[:alnum:]_-')"
CWD_VALUE="$(printf '%s' "$HOOK_JSON" | jq -r '.cwd // empty' 2>/dev/null)"
TRANSCRIPT_PATH="$(printf '%s' "$HOOK_JSON" | jq -r '.transcript_path // empty' 2>/dev/null)"
[[ -n "$SESSION_ID" ]] || fail_open
[[ -n "$CWD_VALUE" ]] || CWD_VALUE="${CLAUDE_PROJECT_DIR:-$PWD}"
# Project key MUST match what capture used (supplement 2026-07-18,
# "derive-auto resolver hole"): route through the shared resolver
# (MEMOIRE_PROJECT > .memoire > registry > basename), fail-open to basename
# exactly like the capture adapters.
RESOLVER="${MEMOIRE_RESOLVE_PROJECT:-$SCRIPT_DIR/memoire-resolve-project.sh}"
PROJECT="${MEMOIRE_PROJECT:-}"
if [[ -z "$PROJECT" ]]; then
  PROJECT="$("$RESOLVER" "$CWD_VALUE" 2>/dev/null)" || PROJECT=""
fi
[[ -n "$PROJECT" ]] || PROJECT="$(basename "$CWD_VALUE")"
# Control characters in a hostile cwd must not reach argv or the log.
PROJECT="${PROJECT//[![:print:]]/ }"
[[ -n "$PROJECT" ]] || fail_open

mkdir -p "$LOG_DIR" 2>/dev/null || fail_open

# Detach ALL remaining work — the foreground hook does only local parsing and
# spawn, so session end is never held open by capture or network I/O. The
# worker: (1) captures the transcript, bounded and credential-inert; (2) POSTs
# the intent; (3) logs one sanitized outcome line. All values travel via the
# environment — nothing is spliced into the inner script.
export MDA_PROJECT="$PROJECT" MDA_SESSION_ID="$SESSION_ID" \
  MDA_BASE_URL="$BASE_URL" MDA_TOKEN_FILE="$TOKEN_FILE" \
  MDA_CAPTURE_BIN="$CAPTURE_BIN" MDA_TRANSCRIPT="$TRANSCRIPT_PATH" \
  MDA_CAPTURE_TIMEOUT="$CAPTURE_TIMEOUT" MDA_NOTIFY_TIMEOUT="$NOTIFY_TIMEOUT" \
  MDA_NOTIFY_SKIP="$NOTIFY_SKIP" \
  MDA_QUEUE_DIR="$QUEUE_DIR" MDA_DRAIN_TIMEOUT="$DRAIN_TIMEOUT" \
  MDA_EVENT="$HOOK_EVENT" MDA_CAUSE="$HOOK_CAUSE"
nohup bash -c '
  set -u
  # Live turn ingest: capture the ended session transcript into the local
  # queue. Bounded (timeout): a hung adapter must not stall the notify. No
  # coreutils timeout -> skip capture rather than run unbounded.
  CAPTURE="skip"
  DRAIN="skip"
  if [[ -n "$MDA_TRANSCRIPT" && -r "$MDA_TRANSCRIPT" && -x "$MDA_CAPTURE_BIN" ]] \
    && command -v timeout >/dev/null 2>&1; then
    timeout "$MDA_CAPTURE_TIMEOUT" "$MDA_CAPTURE_BIN" --tail "$MDA_TRANSCRIPT" >/dev/null 2>&1
    CAPTURE="rc=$?"
    # Bounded drain-wait BEFORE the notify (memoire:D165 round-4 MED-1
    # hardening): an empty pending+inflight means custody left this
    # machine — drained into the local store, or acked into the hub queue
    # by the forwarder — before the intent is announced, so the notify
    # rarely precedes its own capture. The residual hub-drainer gap is a
    # documented pending-ratification limitation; a timeout notifies
    # anyway with an honest marker (the safety sweep owns the residue).
    if [[ "$MDA_NOTIFY_SKIP" != off_derivation_retired ]]; then
    DRAIN="ok"
    DEADLINE=$(( $(date +%s) + MDA_DRAIN_TIMEOUT ))
    while :; do
      DEPTH="$(find "$MDA_QUEUE_DIR/pending" "$MDA_QUEUE_DIR/inflight" \
        -maxdepth 1 -name "*.json" 2>/dev/null | wc -l)"
      [[ "$DEPTH" -eq 0 ]] && break
      if [[ "$(date +%s)" -ge "$DEADLINE" ]]; then
        DRAIN="timeout pending_left=$DEPTH"
        break
      fi
      sleep 1
    done
    fi
  fi
  # Notify: idempotent server-side; the bearer travels via a process-
  # substitution header, never argv. Body built by jq so a hostile project
  # key cannot malform the JSON. A missing curl/token was downgraded to an
  # honest skip marker by the foreground gate — capture already ran above.
  if [[ -n "$MDA_NOTIFY_SKIP" ]]; then
    CODE="$MDA_NOTIFY_SKIP"
  else
    BODY="$(jq -cn --arg p "$MDA_PROJECT" --arg s "$MDA_SESSION_ID" \
      "{project: \$p, session_id: \$s}")"
    CODE="$(curl -s -o /dev/null -w "%{http_code}" -m "$MDA_NOTIFY_TIMEOUT" \
      -H @<(printf "Authorization: Bearer %s" "$(tr -d "\r\n" < "$MDA_TOKEN_FILE")") \
      -H "Content-Type: application/json" \
      -X POST -d "$BODY" \
      "$MDA_BASE_URL/v1/derive/notify" 2>/dev/null)" || CODE="curl_failed"
  fi
  CAUSE=""
  [[ -n "$MDA_CAUSE" ]] && CAUSE=" cause=$MDA_CAUSE"
  printf "%s event=%s%s notify project=%s session=%s capture=%s drain=%s notify_code=%s\n" \
    "$(date -Is)" "$MDA_EVENT" "$CAUSE" "$MDA_PROJECT" "$MDA_SESSION_ID" "$CAPTURE" "$DRAIN" "$CODE"
' >> "$LOG_FILE" 2>&1 &
disown

exit 0
