#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# liaison:D239 session-level kill switch: honored directly (not via the resolver) so
# it works even if the resolver script is missing or broken.
case "${MEMOIRE_CAPTURE:-}" in off|0|false) exit 0 ;; esac

# Backfill assistant turns from Claude Code session transcripts into Memoire's
# file queue (Lane 3 turn-ingest step 1, ruled 2026-07-10).
#
# Scope per the operator ruling: assistant `text` content blocks only (no
# thinking, no tool_use — tool events are already captured live), main-session
# transcripts only (subagents/ dirs excluded, isSidechain lines skipped).
# Events carry source_adapter=claude_code_transcript_backfill and a
# backfill_generation marker in the payload so D97 retro-sweep tooling can
# target exactly this generation. Idempotent: source_event_id is deterministic
# per (session, line uuid) and the store dedups on it.
#
# Usage:
#   memoire-capture-claude-transcripts.sh [--dry-run] <transcript.jsonl|dir> ...
#
# Directories are scanned for *.jsonl, excluding */subagents/*. Project is
# resolved per line via the shared paths->id resolver, attributed to the
# session home (first cwd seen for that session id), with basename as the
# fail-open fallback (MEMOIRE_PROJECT still overrides). Text is capped at
# MEMOIRE_BACKFILL_TEXT_MAX (default 6000) chars.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QUEUE_BIN="${MEMOIRE_QUEUE_BIN:-$ROOT/bin/memoire-queue}"
RESOLVER="${MEMOIRE_RESOLVE_PROJECT:-$ROOT/scripts/memoire-resolve-project.sh}"
TEXT_MAX="${MEMOIRE_BACKFILL_TEXT_MAX:-6000}"

# Resolve a project id via the shared paths->id resolver (liaison:D238/liaison:D239),
# fail-open to basename. Sets MRP_KEY/MRP_ROOT/MRP_SOURCE — globals, not
# command substitution (a subshell would drop root/source, which ride the
# queue write for identity-conflict detection, liaison:D239 item 4).
#   memoire_resolve_v <event-dir> [session-home-dir]
MRP_KEY="" MRP_ROOT="" MRP_SOURCE=""
memoire_resolve_v() {
  local out
  MRP_KEY="" MRP_ROOT="" MRP_SOURCE=""
  if out="$("$RESOLVER" --verbose "$1" "${2:-}" 2>/dev/null)" && [[ -n "$out" ]]; then
    IFS=$'\t' read -r MRP_KEY MRP_ROOT MRP_SOURCE <<<"$out"
  fi
  if [[ -z "$MRP_KEY" ]]; then
    local d="${2:-$1}"
    MRP_KEY="$(basename "$d" 2>/dev/null)" || MRP_KEY="$d"
    MRP_ROOT="" MRP_SOURCE=""
  fi
}

# liaison:D239 capture kill switches (`capture: false` in the nearest .memoire, or
# MEMOIRE_CAPTURE=off). Off ONLY on an explicit "off" from the resolver —
# fail-open otherwise. Cached per session id: the gate runs once per session,
# not once per transcript line.
declare -A MRP_CAPTURE_OFF
memoire_capture_off_session() {
  local sid="$1" dir="$2" home="$3"
  if [[ -z "${MRP_CAPTURE_OFF[$sid]:-}" ]]; then
    if [[ "$("$RESOLVER" --capture-enabled "$dir" "$home" 2>/dev/null)" == "off" ]]; then
      MRP_CAPTURE_OFF[$sid]=1
    else
      MRP_CAPTURE_OFF[$sid]=0
    fi
  fi
  [[ "${MRP_CAPTURE_OFF[$sid]}" == "1" ]]
}

# Session-home attribution: first cwd seen per session id becomes the whole
# session's home, so a session that roams across repos still attributes to its
# launch project. Per-event cwd stays stored on each event unchanged.
declare -A MRP_SESSION_HOME
GENERATION="${MEMOIRE_BACKFILL_GENERATION:-2026-07-10-cc-transcript-backfill}"
DRY_RUN=0

log() { printf 'memoire-capture-claude-transcripts: %s\n' "$*" >&2; }

FILES=()
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    -h|--help) sed -n '3,22p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)
      if [[ -d "$arg" ]]; then
        while IFS= read -r f; do FILES+=("$f"); done \
          < <(find "$arg" -name '*.jsonl' -not -path '*/subagents/*' | sort)
      elif [[ -f "$arg" ]]; then
        FILES+=("$arg")
      else
        log "skipping missing path: $arg"
      fi
      ;;
  esac
done

if [[ ${#FILES[@]} -eq 0 ]]; then
  log "no transcript files given"
  exit 1
fi
if [[ "$DRY_RUN" == "0" && ! -x "$QUEUE_BIN" ]]; then
  log "queue writer not found: $QUEUE_BIN; run make build"
  exit 1
fi

total_events=0
total_files=0
for FILE in "${FILES[@]}"; do
  count=0
  while IFS=$'\t' read -r uuid ts sid cwd truncated text_b64; do
    [[ -z "$uuid" || -z "$sid" ]] && continue
    if [[ -z "${MRP_SESSION_HOME[$sid]:-}" && -n "$cwd" ]]; then
      MRP_SESSION_HOME[$sid]="$cwd"
    fi
    # Kill-switch gate (even under MEMOIRE_PROJECT: override and toggle are
    # orthogonal, liaison:D239 item 6).
    memoire_capture_off_session "$sid" "${cwd:-}" "${MRP_SESSION_HOME[$sid]:-}" && continue
    if [[ -n "${MEMOIRE_PROJECT:-}" ]]; then
      project="$MEMOIRE_PROJECT" MRP_ROOT="" MRP_SOURCE=""
    else
      memoire_resolve_v "${cwd:-unknown}" "${MRP_SESSION_HOME[$sid]:-}"
      project="$MRP_KEY"
    fi
    rootargs=()
    [[ -n "$MRP_ROOT" ]] && rootargs=(-project-root "$MRP_ROOT" -project-source "$MRP_SOURCE")
    text="$(printf '%s' "$text_b64" | base64 -d 2>/dev/null)" || continue
    [[ -z "$text" ]] && continue
    payload=$(jq -cn --arg f "$FILE" --arg u "$uuid" --arg g "$GENERATION" --argjson tr "$truncated" \
      '{transcript_file: $f, line_uuid: $u, backfill_generation: $g, text_truncated: $tr}')
    if [[ "$DRY_RUN" == "1" ]]; then
      printf 'DRY %s %s %s %s chars=%s\n' "$project" "$sid" "$ts" "$uuid" "${#text}"
    else
      "$QUEUE_BIN" \
        -project "$project" \
        -session-id "$sid" \
        -cwd "${cwd:-}" \
        -event-type claude_assistant_turn \
        -source-adapter claude_code_transcript_backfill \
        -source-event-id "cc-turn:$sid:$uuid" \
        -occurred-at "$ts" \
        -text "$text" \
        -payload-json "$payload" \
        ${rootargs[@]+"${rootargs[@]}"} >/dev/null || log "queue write failed: $FILE $uuid"
    fi
    count=$((count + 1))
  done < <(jq -r --argjson max "$TEXT_MAX" '
    select(.type == "assistant")
    | select(.isSidechain != true)
    | . as $l
    | ([.message.content[]? | select(.type == "text") | .text] | join("\n\n")) as $t
    | select(($t | length) > 0)
    | [$l.uuid, $l.timestamp, $l.sessionId, ($l.cwd // ""),
       (if ($t | length) > $max then "true" else "false" end),
       ($t[0:$max] | @base64)]
    | @tsv' "$FILE" 2>/dev/null)
  if [[ "$count" -gt 0 ]]; then
    log "$(basename "$FILE"): $count assistant turns"
    total_files=$((total_files + 1))
    total_events=$((total_events + count))
  fi
done
log "done: $total_events events from $total_files files (dry_run=$DRY_RUN)"
