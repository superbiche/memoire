#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# liaison:D239 session-level kill switch: honored directly (not via the resolver) so
# it works even if the resolver script is missing or broken.
case "${MEMOIRE_CAPTURE:-}" in off|0|false) exit 0 ;; esac

# Ingest Codex CLI rollout transcripts into Memoire's file queue so foreign
# (non-Claude-Code) producer sessions have a queryable tool-event window.
#
# Codex has no PostToolUse-hook equivalent, so capture is transcript-driven:
# this script parses rollout JSONL files under ~/.codex/sessions/ and emits one
# queue event per file touched by a successful apply_patch
# (event_msg/patch_apply_end). Events carry the harness session id
# (== CODEX_THREAD_ID inside the session), the original occurred-at timestamp,
# and tool_input.file_path — the shape Memoire's tool-window query derives
# paths_touched from, which Liaison's produced-in-window corroboration matches
# against.
#
# Re-running over the same rollout files is idempotent: source_event_id is
# deterministic per (session, call, path) and the store dedups on it.
#
# Payload carries paths + provenance (rollout file), not patch content; the
# rollout file remains the raw source.
#
# Assistant turns (turn-ingest campaign Phase 1, 2026-07-11): every
# event_msg/agent_message with text becomes one codex_assistant_turn event —
# the turn's visible prose in the event text (capped at
# MEMOIRE_BACKFILL_TEXT_MAX, default 6000 chars, text_truncated marker in the
# payload), provenance + backfill-generation marker alongside. Deterministic
# source_event_id per (session, timestamp, message), so re-runs dedup
# server-side like every other event this script emits.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QUEUE_BIN="${MEMOIRE_QUEUE_BIN:-$ROOT/bin/memoire-queue}"
RESOLVER="${MEMOIRE_RESOLVE_PROJECT:-$ROOT/scripts/memoire-resolve-project.sh}"
SESSIONS_DIR="${CODEX_SESSIONS_DIR:-$HOME/.codex/sessions}"
PROJECT_OVERRIDE="${MEMOIRE_PROJECT:-}"
SINCE="24 hours ago"
SCAN=0
INCLUDE_PROMPTS=0
DRY_RUN=0
PROMPT_TEXT_MAX=4000
TURN_TEXT_MAX="${MEMOIRE_BACKFILL_TEXT_MAX:-6000}"
GENERATION="${MEMOIRE_BACKFILL_GENERATION:-2026-07-11-codex-turn-ingest}"

usage() {
  cat <<'USAGE' >&2
Usage: memoire-capture-codex-rollout.sh [options] [rollout.jsonl ...]

Parses Codex CLI rollout transcripts and writes tool events plus assistant
turns (codex_assistant_turn, always on) into Memoire's file queue. With
explicit file arguments it ingests those rollouts; with --scan it ingests
every rollout under the sessions dir modified since a cutoff. Re-runs are
idempotent (deterministic source_event_id).

Options:
  --scan               Scan the sessions dir instead of taking file arguments.
  --since SPEC         Cutoff for --scan, as a `find -newermt` spec.
                       Default: "24 hours ago".
  --sessions-dir DIR   Codex sessions dir. Default: ~/.codex/sessions
                       (CODEX_SESSIONS_DIR).
  --project NAME       Project override. Default: shared resolver over the
                       rollout's session cwd (registry id, else basename;
                       MEMOIRE_PROJECT still overrides).
  --include-prompts    Also emit codex_UserMessage events for user prompts.
  --dry-run            Print the events that would be queued, write nothing.
  -h, --help           Show this help.
USAGE
}

log() {
  printf 'memoire-capture-codex-rollout: %s\n' "$*" >&2
}

# Resolve a project id via the shared paths->id resolver (liaison:D238), fail-open to
# basename. The rollout's session_meta cwd is a single per-session launch dir,
# so it is both the event dir and the session home.
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
    MRP_KEY="$(basename "$1" 2>/dev/null)" || MRP_KEY="$1"
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
  done < <(find "$SESSIONS_DIR" -name 'rollout-*.jsonl' -newermt "$SINCE" 2>/dev/null | sort)
fi

if [[ ${#FILES[@]} -eq 0 ]]; then
  log "no rollout files to ingest"
  exit 0
fi

total_events=0
total_files=0
failed_files=0
failed_events=0
payload_file="$(mktemp "${TMPDIR:-/tmp}/memoire-codex-payload.XXXXXX.json")"
cleanup() {
  rm -f "$payload_file"
}
trap cleanup EXIT

emit_event() {
  # emit_event PROJECT SESSION_ID CWD EVENT_TYPE OCCURRED_AT SOURCE_EVENT_ID TEXT
  # payload JSON is read from $payload_file.
  local project="$1" session_id="$2" cwd="$3" event_type="$4"
  local occurred_at="$5" source_event_id="$6" text="$7"
  if [[ "$DRY_RUN" == "1" ]]; then
    jq -c --arg project "$project" --arg session_id "$session_id" \
      --arg event_type "$event_type" --arg occurred_at "$occurred_at" \
      --arg source_event_id "$source_event_id" --arg text "$text" \
      '{project:$project, session_id:$session_id, event_type:$event_type,
        occurred_at:$occurred_at, source_event_id:$source_event_id,
        text:$text, payload:.}' "$payload_file"
    return 0
  fi
  "$QUEUE_BIN" \
    -project "$project" \
    -session-id "$session_id" \
    -source-adapter "codex-cli" \
    -event-type "$event_type" \
    -cwd "$cwd" \
    -occurred-at "$occurred_at" \
    -source-event-id "$source_event_id" \
    -payload-file "$payload_file" \
    -text "$text" \
    ${MRP_ROOT:+-project-root "$MRP_ROOT" -project-source "$MRP_SOURCE"} >/dev/null
}

ingest_file() {
  local file="$1"
  local meta session_id cwd project rel_file
  meta="$(head -1 "$file" | jq -c 'select(.type=="session_meta")' 2>/dev/null)"
  if [[ -z "$meta" ]]; then
    log "skip (no session_meta): $file"
    return 1
  fi
  session_id="$(jq -r '.payload.session_id // .payload.id // empty' <<<"$meta")"
  cwd="$(jq -r '.payload.cwd // empty' <<<"$meta")"
  if [[ -z "$session_id" ]]; then
    log "skip (no session id): $file"
    return 1
  fi
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
    project="codex-unknown"
  fi
  rel_file="${file/#$HOME/\~}"

  local file_events=0
  local file_failures=0
  local line ts call_id turn_id path change_type move_path fingerprint
  while IFS= read -r line; do
    ts="$(jq -r '.ts' <<<"$line")"
    call_id="$(jq -r '.call_id // ""' <<<"$line")"
    turn_id="$(jq -r '.turn_id // ""' <<<"$line")"
    path="$(jq -r '.path' <<<"$line")"
    change_type="$(jq -r '.change_type' <<<"$line")"
    move_path="$(jq -r '.move_path // ""' <<<"$line")"
    [[ -z "$path" ]] && continue
    fingerprint="$(printf '%s' "$session_id:$call_id:$path" | sha256sum | awk '{print $1}')"
    jq -nc --arg path "$path" --arg change_type "$change_type" \
      --arg move_path "$move_path" --arg call_id "$call_id" \
      --arg turn_id "$turn_id" --arg rollout "$rel_file" \
      '{tool_name:"apply_patch",
        tool_input:({file_path:$path, change_type:$change_type}
          + (if $move_path == "" then {} else {move_path:$move_path} end)),
        call_id:$call_id, turn_id:$turn_id, rollout_file:$rollout}' \
      > "$payload_file"
    if emit_event "$project" "$session_id" "$cwd" "codex_PatchApply" \
      "$ts" "codex-patchapply-$fingerprint" \
      "Codex apply_patch $change_type $path"; then
      file_events=$((file_events + 1))
    else
      log "queue write failed for $path in $file"
      file_failures=$((file_failures + 1))
    fi
  done < <(jq -c '
    select(.type=="event_msg" and .payload.type=="patch_apply_end" and .payload.success==true)
    | {ts:.timestamp, call_id:(.payload.call_id // ""), turn_id:(.payload.turn_id // "")} as $m
    | (.payload.changes // {}) | to_entries[]
    | ($m + {path:.key, change_type:(.value.type // "update"), move_path:(.value.move_path // "")})
    | ., (if .move_path != "" then (. + {path:.move_path, change_type:"move_dest", move_path:""}) else empty end)
  ' "$file" 2>/dev/null)

  # Assistant turns: one codex_assistant_turn per agent_message with text.
  # agent_message carries no message id, so the transcript line number joins
  # the fingerprint as the per-line identity: two otherwise-identical turns
  # (same timestamp, same text) stay distinct events (orthogonal review
  # 2026-07-11). The full message still binds content; the capped copy is
  # what lands in the event text.
  local turn_ts turn_idx turn_msg turn_text turn_truncated turn_fp
  while IFS= read -r line; do
    turn_ts="$(jq -r '.ts' <<<"$line")"
    turn_idx="$(jq -r '.idx' <<<"$line")"
    turn_msg="$(jq -r '.message' <<<"$line")"
    [[ -z "$turn_msg" ]] && continue
    turn_fp="$(printf '%s' "$session_id:$turn_idx:$turn_ts:$turn_msg" | sha256sum | awk '{print $1}')"
    turn_truncated=false
    if (( ${#turn_msg} > TURN_TEXT_MAX )); then
      turn_truncated=true
    fi
    turn_text="${turn_msg:0:$TURN_TEXT_MAX}"
    jq -nc --arg rollout "$rel_file" --arg g "$GENERATION" --argjson tr "$turn_truncated" \
      '{rollout_file:$rollout, backfill_generation:$g, text_truncated:$tr}' > "$payload_file"
    if emit_event "$project" "$session_id" "$cwd" "codex_assistant_turn" \
      "$turn_ts" "codex-turn-$turn_fp" "$turn_text"; then
      file_events=$((file_events + 1))
    else
      log "queue write failed for assistant turn at $turn_ts in $file"
      file_failures=$((file_failures + 1))
    fi
  done < <(jq -cn '
    inputs
    | input_line_number as $idx
    | select(.type=="event_msg" and .payload.type=="agent_message")
    | {ts:.timestamp, idx:$idx, message:(.payload.message // "")}
    | select(.message != "")
  ' "$file" 2>/dev/null)

  if [[ "$INCLUDE_PROMPTS" == "1" ]]; then
    local message text
    while IFS= read -r line; do
      ts="$(jq -r '.ts' <<<"$line")"
      message="$(jq -r '.message' <<<"$line")"
      [[ -z "$message" ]] && continue
      fingerprint="$(printf '%s' "$session_id:$ts:$message" | sha256sum | awk '{print $1}')"
      text="${message:0:$PROMPT_TEXT_MAX}"
      jq -nc --arg message "$message" --arg rollout "$rel_file" \
        '{message:$message, rollout_file:$rollout}' > "$payload_file"
      if emit_event "$project" "$session_id" "$cwd" "codex_UserMessage" \
        "$ts" "codex-usermessage-$fingerprint" "$text"; then
        file_events=$((file_events + 1))
      else
        log "queue write failed for prompt at $ts in $file"
        file_failures=$((file_failures + 1))
      fi
    done < <(jq -c '
      select(.type=="event_msg" and .payload.type=="user_message")
      | {ts:.timestamp, message:(.payload.message // "")}
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

# Honest exit code: any skipped/failed file or any dropped event is a failure,
# so automated ingestion (timers, burns) cannot read partial capture as success
# (Codex review finding, 2026-07-02).
log "done: files=$total_files failed_files=$failed_files events=$total_events failed_events=$failed_events"
if [[ $failed_files -gt 0 || $failed_events -gt 0 ]]; then
  exit 1
fi
exit 0
