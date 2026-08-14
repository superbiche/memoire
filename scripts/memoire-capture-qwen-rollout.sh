#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# liaison:D239 session-level kill switch: honored directly (not via the resolver) so
# it works even if the resolver script is missing or broken.
case "${MEMOIRE_CAPTURE:-}" in off|0|false) exit 0 ;; esac

# Ingest qwen-code rollout transcripts into Memoire's file queue so foreign
# (non-Claude-Code) sessions have a queryable tool-event window.
#
# qwen-code v0.17.0 stores one JSONL file per session at:
#   ~/.qwen/projects/<cwd-slug>/chats/<sessionId>.jsonl
#
# This script parses those transcripts and emits one queue event per successful
# file-mutating tool call (edit, write_file) joined by functionCall.id ==
# tool_result.callId with status=="success".  With --include-prompts it also
# emits qwen_UserMessage events for user prompts.
#
# Assistant turns (turn-ingest campaign Phase 1, 2026-07-11): every assistant
# line's visible text parts (thought parts excluded — no thinking ingest, per
# the CC transcript-backfill ruling) become one qwen_assistant_turn event —
# prose in the event text (capped at MEMOIRE_BACKFILL_TEXT_MAX, default 6000
# chars, text_truncated marker in the payload), provenance +
# backfill-generation marker alongside. Always on.
#
# Re-running over the same transcript files is idempotent: source_event_id is
# deterministic per (sessionId, callId, path) for tool events and per
# (sessionId, line uuid) for assistant turns.

SCRIPT_PATH="${BASH_SOURCE[0]}"
SCRIPT_DIR="${SCRIPT_PATH%/*}"
if [[ "$SCRIPT_DIR" == "$SCRIPT_PATH" ]]; then
  SCRIPT_DIR="."
fi
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
QUEUE_BIN="${MEMOIRE_QUEUE_BIN:-$ROOT/bin/memoire-queue}"
RESOLVER="${MEMOIRE_RESOLVE_PROJECT:-$ROOT/scripts/memoire-resolve-project.sh}"
SESSIONS_DIR="${QWEN_SESSIONS_DIR:-$HOME/.qwen/projects}"
PROJECT_OVERRIDE="${MEMOIRE_PROJECT:-}"
SINCE="24 hours ago"
SCAN=0
INCLUDE_PROMPTS=0
DRY_RUN=0
PROMPT_TEXT_MAX=4000
TURN_TEXT_MAX="${MEMOIRE_BACKFILL_TEXT_MAX:-6000}"
GENERATION="${MEMOIRE_BACKFILL_GENERATION:-2026-07-11-qwen-turn-ingest}"

usage() {
  printf '%s\n' \
    "Usage: memoire-capture-qwen-rollout.sh [options] [session.jsonl ...]" \
    "" \
    "Parses qwen-code JSONL transcripts and writes tool events plus assistant turns" \
    "(qwen_assistant_turn, always on) into Memoire's file queue.  With explicit file" \
    "arguments it ingests those transcripts; with --scan" \
    "it ingests every transcript under the sessions dir modified since a cutoff." \
    "Re-runs are idempotent (deterministic source_event_id)." \
    "" \
    "Options:" \
    "  --scan               Scan the sessions dir instead of taking file arguments." \
    "  --since SPEC         Cutoff for --scan, as a \`find -newermt\` spec." \
    "                       Default: \"24 hours ago\"." \
    "  --sessions-dir DIR   qwen sessions dir. Default: ~/.qwen/projects" \
    "                       (QWEN_SESSIONS_DIR)." \
    "  --project NAME       Project override. Default: shared resolver over the" \
    "                       transcript cwd (registry id, else final path" \
    "                       component; MEMOIRE_PROJECT still overrides)." \
    "  --include-prompts    Also emit qwen_UserMessage events for user prompts." \
    "  --dry-run            Print the events that would be queued, write nothing." \
    "  -h, --help           Show this help." >&2
}

log() {
  printf 'memoire-capture-qwen-rollout: %s\n' "$*" >&2
}

# Resolve a project id via the shared paths->id resolver (liaison:D238), fail-open to
# basename. The transcript's session cwd is a single per-session launch dir, so
# it is both the event dir and the session home.
#   memoire_resolve_v <session-cwd>  (sets MRP_KEY/ROOT/SOURCE; globals, not
#   command substitution — root/source ride the queue write for identity-
#   conflict detection, liaison:D239 item 4, and a subshell would drop them)
MRP_KEY="" MRP_ROOT="" MRP_SOURCE=""
memoire_resolve_v() {
  local out
  MRP_KEY="" MRP_ROOT="" MRP_SOURCE=""
  if out="$("$RESOLVER" --verbose "$1" 2>/dev/null)" && [[ -n "$out" ]]; then
    IFS=$'\t' read -r MRP_KEY MRP_ROOT MRP_SOURCE <<<"$out"
  fi
  if [[ -z "$MRP_KEY" ]]; then
    local d="$1"
    MRP_KEY="${d##*/}"
    MRP_ROOT="" MRP_SOURCE=""
  fi
}

FILES=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --scan) SCAN=1; shift ;;
    --since) SINCE="${2:-}"; shift 2 ;;
    --sessions-dir) SESSIONS_DIR="${2:-}"; shift 2 ;;
    --project) PROJECT_OVERRIDE="${2:-}"; shift 2 ;;
    --include-prompts) INCLUDE_PROMPTS=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) usage; log "unknown option: $1"; exit 2 ;;
    *) FILES+=("$1"); shift ;;
  esac
done

if ! command -v jq >/dev/null 2>&1; then
  log "jq is required"
  exit 1
fi

if [[ "$DRY_RUN" == "0" && ! -x "$QUEUE_BIN" ]]; then
  log "queue writer not found: $QUEUE_BIN; run make build"
  exit 1
fi

if [[ "$SCAN" == "1" ]]; then
  if [[ ${#FILES[@]} -gt 0 ]]; then
    log "--scan and explicit file arguments are mutually exclusive"
    exit 2
  fi
  if [[ ! -d "$SESSIONS_DIR" ]]; then
    log "sessions dir not found: $SESSIONS_DIR"
    exit 1
  fi
  while IFS= read -r f; do
    FILES+=("$f")
  done < <(find "$SESSIONS_DIR" -path '*/chats/*.jsonl' -newermt "$SINCE" 2>/dev/null)
fi

if [[ ${#FILES[@]} -eq 0 ]]; then
  log "no rollout files to ingest"
  exit 0
fi

total_events=0
total_files=0
failed_files=0
failed_events=0
payload_file="${TMPDIR:-/tmp}/memoire-qwen-payload.${RANDOM}${RANDOM}.json"
cleanup() {
  unset -f cleanup emit_event fingerprint_path ingest_file 2>/dev/null
  unset payload_file 2>/dev/null
}
trap cleanup EXIT

emit_event() {
  # emit_event PROJECT SESSION_ID CWD EVENT_TYPE OCCURRED_AT SOURCE_EVENT_ID TEXT
  local project="$1" session_id="$2" cwd="$3" event_type="$4"
  local occurred_at="$5" source_event_id="$6" text="$7"
  if [[ "$DRY_RUN" == "1" ]]; then
    jq -c --arg project "$project" \
          --arg session_id "$session_id" \
          --arg event_type "$event_type" \
          --arg occurred_at "$occurred_at" \
          --arg source_event_id "$source_event_id" \
          --arg text "$text" \
      '{project:$project, session_id:$session_id, event_type:$event_type,
        occurred_at:$occurred_at, source_event_id:$source_event_id,
        text:$text, payload:.}' "$payload_file"
    return 0
  fi
  if "$QUEUE_BIN" \
      -project "$project" \
      -session-id "$session_id" \
      -source-adapter "qwen-cli" \
      -event-type "$event_type" \
      -cwd "$cwd" \
      -occurred-at "$occurred_at" \
      -source-event-id "$source_event_id" \
      -payload-file "$payload_file" \
      -text "$text" \
      ${MRP_ROOT:+-project-root "$MRP_ROOT" -project-source "$MRP_SOURCE"} >/dev/null; then
    return 0
  else
    return 1
  fi
}

fingerprint_path() {
  local fingerprint
  fingerprint="$(printf '%s' "$1" | sha256sum)"
  echo "${fingerprint%% *}"
}

ingest_file() {
  local file="$1"

  local meta session_id cwd project rel_file
  if ! jq -c 'empty' "$file" >/dev/null 2>&1; then
    log "skip (unparsable): $file"
    return 1
  fi

  meta="$(jq -c -n 'first(inputs | select(.sessionId)) // empty' "$file" 2>/dev/null)"
  if [[ -z "$meta" ]]; then
    log "skip (no session metadata): $file"
    return 1
  fi

  session_id="$(jq -r '.sessionId' <<<"$meta")"
  cwd="$(jq -r '.cwd // ""' <<<"$meta")"

  # liaison:D239 kill-switch gate: off ONLY on an explicit "off" (fail-open), applies
  # even under --project/MEMOIRE_PROJECT (override and toggle are orthogonal).
  if [[ -n "$cwd" && "$("$RESOLVER" --capture-enabled "$cwd" 2>/dev/null)" == "off" ]]; then
    log "skip (capture off): $file"
    return 0
  fi

  project="$PROJECT_OVERRIDE" MRP_ROOT="" MRP_SOURCE=""
  if [[ -z "$project" && -n "$cwd" ]]; then
    memoire_resolve_v "$cwd"
    project="$MRP_KEY"
  fi
  if [[ -z "$project" ]]; then
    project="qwen-unknown"
  fi

  rel_file="${file/#$HOME/\~}"

  # Build success map: callId -> timestamp for successful tool results.
  local success_map
  success_map="$(jq -c '
    [inputs | select(.type=="tool_result" and .toolCallResult.status=="success")
     | {key:.toolCallResult.callId, value:.timestamp}]
    | from_entries
  ' "$file" 2>/dev/null)"

  local file_events=0
  local file_failures=0

  # Extract successful file-mutating candidates and emit events.
  while IFS= read -r line; do
    [[ -z "$line" ]] && continue

    local call_id tool_name file_path change_type occurred_at fingerprint text
    call_id="$(jq -r '.call_id' <<<"$line")"
    tool_name="$(jq -r '.tool_name' <<<"$line")"
    file_path="$(jq -r '.file_path' <<<"$line")"
    change_type="$(jq -r '.change_type' <<<"$line")"
    occurred_at="$(jq -r '.occurred_at' <<<"$line")"

    fingerprint="$(fingerprint_path "$session_id:$call_id:$file_path")"
    text="Qwen ${tool_name} ${change_type} ${file_path}"

    jq -nc --arg tool_name "$tool_name" \
           --arg file_path "$file_path" \
           --arg change_type "$change_type" \
           --arg call_id "$call_id" \
           --arg transcript_file "$rel_file" \
      '{tool_name:$tool_name,
        tool_input:({file_path:$file_path, change_type:$change_type}),
        call_id:$call_id,
        transcript_file:$transcript_file}' > "$payload_file"

    if emit_event "$project" "$session_id" "$cwd" "qwen_FileEdit" \
        "$occurred_at" "qwen-fileedit-${fingerprint}" "$text"; then
      file_events=$((file_events + 1))
    else
      log "queue write failed for ${file_path}"
      file_failures=$((file_failures + 1))
    fi
  done < <(jq -c --argjson success_map "$success_map" '
    select(.type=="assistant") as $line
    | $line.message.parts[]?
    | select(.functionCall?)
    | .functionCall as $call
    | select($call.name=="edit" or $call.name=="write_file")
    | {
        call_id: $call.id,
        tool_name: $call.name,
        file_path: ($call.args.file_path // ""),
        change_type: (if $call.name=="edit" then "update" else "write" end),
        occurred_at: ($success_map[$call.id] // empty)
      }
  ' "$file" 2>/dev/null | while IFS= read -r candidate; do
    local fp oa
    fp="$(jq -r '.file_path' <<<"$candidate")"
    oa="$(jq -r '.occurred_at' <<<"$candidate")"
    if [[ -n "$fp" && -n "$oa" ]]; then
      printf '%s\n' "$candidate"
    fi
  done)

  # Assistant turns: one qwen_assistant_turn per assistant line with visible
  # (non-thought) text. Base64 carries the capped text through @tsv intact;
  # the line uuid keys the deterministic source_event_id.
  local turn_uuid turn_ts turn_truncated turn_b64 turn_text
  while IFS=$'\t' read -r turn_uuid turn_ts turn_truncated turn_b64; do
    [[ -z "$turn_uuid" || -z "$turn_b64" ]] && continue
    turn_text="$(printf '%s' "$turn_b64" | base64 -d 2>/dev/null)" || continue
    [[ -z "$turn_text" ]] && continue

    jq -nc --arg transcript_file "$rel_file" \
           --arg line_uuid "$turn_uuid" \
           --arg g "$GENERATION" \
           --argjson tr "$turn_truncated" \
      '{transcript_file:$transcript_file, line_uuid:$line_uuid,
        backfill_generation:$g, text_truncated:$tr}' > "$payload_file"

    if emit_event "$project" "$session_id" "$cwd" "qwen_assistant_turn" \
        "$turn_ts" "qwen-turn:${session_id}:${turn_uuid}" "$turn_text"; then
      file_events=$((file_events + 1))
    else
      log "queue write failed for assistant turn ${turn_uuid} in ${file}"
      file_failures=$((file_failures + 1))
    fi
  done < <(jq -r --argjson max "$TURN_TEXT_MAX" '
    select(.type=="assistant")
    | . as $l
    | ([.message.parts[]? | select((.text // "") != "" and .thought != true) | .text] | join("\n\n")) as $t
    | select(($t | length) > 0)
    | [($l.uuid // ""), $l.timestamp,
       (if ($t | length) > $max then "true" else "false" end),
       ($t[0:$max] | @base64)]
    | @tsv' "$file" 2>/dev/null)

  # Prompt extraction (only with --include-prompts).
  if [[ "$INCLUDE_PROMPTS" == "1" ]]; then
    while IFS= read -r line; do
      [[ -z "$line" ]] && continue

      local ts message fingerprint text
      ts="$(jq -r '.ts' <<<"$line")"
      message="$(jq -r '.message' <<<"$line")"
      [[ -z "$message" ]] && continue

      fingerprint="$(fingerprint_path "$session_id:$ts:$message")"
      text="${message:0:$PROMPT_TEXT_MAX}"

      jq -nc --arg message "$message" \
             --arg transcript_file "$rel_file" \
        '{message:$message, transcript_file:$transcript_file}' > "$payload_file"

      if emit_event "$project" "$session_id" "$cwd" "qwen_UserMessage" \
          "$ts" "qwen-usermessage-${fingerprint}" "$text"; then
        file_events=$((file_events + 1))
      else
        log "queue write failed for prompt at ${ts} in ${file}"
        file_failures=$((file_failures + 1))
      fi
    done < <(jq -c --arg include_prompts "$INCLUDE_PROMPTS" '
      if $include_prompts == "1" then
        select(.type=="user" and .message.role=="user")
        | {
            ts: .timestamp,
            message: ([.message.parts[]?.text // empty] | join("\n"))
          }
      else empty end
    ' "$file" 2>/dev/null)
  fi

  log "$rel_file: session=$session_id project=$project events=$file_events failures=$file_failures"
  total_events=$((total_events + file_events))
  failed_events=$((failed_events + file_failures))
  if [[ $file_failures -gt 0 ]]; then
    return 1
  fi
  return 0
}

for file in "${FILES[@]}"; do
  if [[ ! -r "$file" ]]; then
    log "skip (unreadable): $file"
    failed_files=$((failed_files + 1))
    continue
  fi
  if ingest_file "$file"; then
    total_files=$((total_files + 1))
  else
    failed_files=$((failed_files + 1))
  fi
done

log "done: files=$total_files failed_files=$failed_files events=$total_events failed_events=$failed_events"
if [[ $failed_files -gt 0 || $failed_events -gt 0 ]]; then
  exit 1
fi
exit 0
