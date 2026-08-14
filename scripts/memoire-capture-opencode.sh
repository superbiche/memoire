#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# liaison:D239 session-level kill switch: honored directly (not via the resolver) so
# it works even if the resolver script is missing or broken.
case "${MEMOIRE_CAPTURE:-}" in off|0|false) exit 0 ;; esac

# Ingest opencode assistant turns into Memoire's file queue so opencode
# sessions have a queryable memory window (opencode parity slice, 2026-07-12).
#
# opencode stores sessions in a single live WAL-mode SQLite DB
# (~/.local/share/opencode/opencode.db, drizzle-managed). Unlike codex/qwen
# there are no JSONL transcript files, so selection is SQL
# (session.time_updated > cutoff), not file mtimes. The DB is live user
# state: every connection opens READ-ONLY (file:...?mode=ro) and no SQL
# statement here mutates it. (mode=ro is a logical guarantee on the main
# database; SQLite may still create -wal/-shm companions when absent — see
# https://www.sqlite.org/wal.html#read_only_databases — so this script must
# never be pointed at a DB whose directory it must not touch.)
#
# Every COMPLETED assistant message's text parts (part.data.type == "text";
# "reasoning" parts are a separate type and are excluded by the WHERE
# clause — no thinking ingest, per the CC transcript-backfill ruling) are
# grouped per message in part-id order, joined with blank lines, and become
# one opencode_assistant_turn event — prose in the event text (capped at
# MEMOIRE_BACKFILL_TEXT_MAX, default 6000 chars, text_truncated marker in
# the payload), provenance + backfill-generation marker alongside.
# Subagent sessions (session.parent_id IS NOT NULL) are excluded, like CC
# sidechains. Messages without message.data.time.completed are excluded:
# the sweep scans a LIVE store, and a mid-generation message would otherwise
# be frozen forever as a partial turn under its deterministic source id
# (queue ingest is INSERT OR IGNORE — first write wins; ortho review
# 2026-07-13, MAJOR). Turns that never complete (aborted/errored) are
# deliberately not captured.
#
# Re-running is idempotent: source_event_id is deterministic per
# (session_id, message_id) — opencode primary keys are stable ULID-ish ids,
# so no fingerprint hashing is needed.

SCRIPT_PATH="${BASH_SOURCE[0]}"
SCRIPT_DIR="${SCRIPT_PATH%/*}"
if [[ "$SCRIPT_DIR" == "$SCRIPT_PATH" ]]; then
  SCRIPT_DIR="."
fi
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
QUEUE_BIN="${MEMOIRE_QUEUE_BIN:-$ROOT/bin/memoire-queue}"
RESOLVER="${MEMOIRE_RESOLVE_PROJECT:-$ROOT/scripts/memoire-resolve-project.sh}"
DB_FILE="${OPENCODE_DB:-$HOME/.local/share/opencode/opencode.db}"
PROJECT_OVERRIDE="${MEMOIRE_PROJECT:-}"
SINCE="24 hours ago"
SCAN=0
DRY_RUN=0
TURN_TEXT_MAX="${MEMOIRE_BACKFILL_TEXT_MAX:-6000}"
GENERATION="${MEMOIRE_BACKFILL_GENERATION:-2026-07-12-opencode-turn-ingest}"

usage() {
  printf '%s\n' \
    "Usage: memoire-capture-opencode.sh [options] [session-id ...]" \
    "" \
    "Reads the opencode SQLite session store (read-only) and writes assistant" \
    "turns (opencode_assistant_turn, always on) into Memoire's file queue." \
    "With explicit session-id arguments it ingests those sessions; with --scan" \
    "it ingests every top-level session updated since a cutoff." \
    "Re-runs are idempotent (deterministic source_event_id)." \
    "" \
    "Options:" \
    "  --scan               Scan the session store instead of taking session ids." \
    "  --since SPEC         Cutoff for --scan, as a \`date -d\` spec." \
    "                       Default: \"24 hours ago\"." \
    "  --db PATH            opencode SQLite DB. Default:" \
    "                       ~/.local/share/opencode/opencode.db (OPENCODE_DB)." \
    "  --project NAME       Project override. Default: shared resolver over the" \
    "                       session directory (registry id, else final path" \
    "                       component; MEMOIRE_PROJECT still overrides)." \
    "  --dry-run            Print the events that would be queued, write nothing." \
    "  -h, --help           Show this help." >&2
}

log() {
  printf 'memoire-capture-opencode: %s\n' "$*" >&2
}

# Resolve a project id via the shared paths->id resolver (liaison:D238), fail-open to
# basename. session.directory is the session home for every turn, so it is both
# the event dir and the session home.
#   memoire_resolve_v <session-directory>  (sets MRP_KEY/ROOT/SOURCE; globals,
#   not command substitution — root/source ride the queue write for identity-
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

need_operand() { # option-name
  # A value-taking option without its operand must be a usage error: under
  # `set -u` without `set -e`, a failed `shift 2` would otherwise leave the
  # argument list unchanged and loop forever (ortho review 2026-07-13).
  if [[ $# -lt 2 || -z "${2:-}" ]]; then
    usage
    log "missing operand for $1"
    exit 2
  fi
}

SESSION_IDS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --scan) SCAN=1; shift ;;
    --since) need_operand "$@"; SINCE="$2"; shift 2 ;;
    --db) need_operand "$@"; DB_FILE="$2"; shift 2 ;;
    --project) need_operand "$@"; PROJECT_OVERRIDE="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) usage; log "unknown option: $1"; exit 2 ;;
    *) SESSION_IDS+=("$1"); shift ;;
  esac
done

# A nonsense cap must not silently kill capture (jq --argjson would fail and
# an empty result would masquerade as "no turns"): fall back to the default,
# same fail-open validation discipline as the sweep's timeout knobs.
[[ "$TURN_TEXT_MAX" =~ ^[1-9][0-9]*$ ]] || TURN_TEXT_MAX=6000

if ! command -v jq >/dev/null 2>&1; then
  log "jq is required"
  exit 1
fi
if ! command -v sqlite3 >/dev/null 2>&1; then
  log "sqlite3 is required"
  exit 1
fi
if [[ "$DRY_RUN" == "0" && ! -x "$QUEUE_BIN" ]]; then
  log "queue writer not found: $QUEUE_BIN; run make build"
  exit 1
fi
if [[ ! -r "$DB_FILE" ]]; then
  log "opencode db not found: $DB_FILE"
  exit 1
fi

if [[ "$SCAN" == "1" && ${#SESSION_IDS[@]} -gt 0 ]]; then
  log "--scan and explicit session-id arguments are mutually exclusive"
  exit 2
fi
if [[ "$SCAN" == "0" && ${#SESSION_IDS[@]} -eq 0 ]]; then
  log "no sessions to ingest (pass session ids or --scan)"
  exit 0
fi

# Session filter: either time cutoff (ms epoch, session.time_updated) or an
# explicit id list. Ids are validated to a safe charset before being inlined
# into SQL — opencode ids are ULID-ish ([a-z0-9_]), anything else is refused.
SESSION_FILTER=""
if [[ "$SCAN" == "1" ]]; then
  CUTOFF_S="$(date -d "$SINCE" +%s 2>/dev/null)" || CUTOFF_S=""
  if [[ -z "$CUTOFF_S" ]]; then
    log "unparsable --since spec: $SINCE"
    exit 2
  fi
  SESSION_FILTER="s.time_updated > ${CUTOFF_S}000"
else
  id_list=""
  for sid in "${SESSION_IDS[@]}"; do
    if [[ ! "$sid" =~ ^[A-Za-z0-9_-]+$ ]]; then
      log "refusing suspicious session id: $sid"
      exit 2
    fi
    id_list+="${id_list:+,}'$sid'"
  done
  SESSION_FILTER="s.id IN ($id_list)"
fi

# One row per text part, JSON-encoded (embedded newlines escape to \n, so
# each row stays one line). Grouping per message happens in jq below.
# part.data.type is pinned to the literal 'text' — 'reasoning', 'tool',
# 'step-start', 'step-finish' never match (fixture test pins these values so
# drizzle schema drift fails loudly instead of capturing nothing).
ROWS="$(sqlite3 "file:${DB_FILE}?mode=ro" ".timeout 3000" "
SELECT json_object(
  'sid', s.id,
  'mid', m.id,
  'dir', s.directory,
  'created', COALESCE(json_extract(m.data, '\$.time.created'), m.time_created),
  'text', json_extract(p.data, '\$.text')
)
FROM part p
JOIN message m ON p.message_id = m.id
JOIN session s ON m.session_id = s.id
WHERE json_extract(m.data, '\$.role') = 'assistant'
  AND json_extract(m.data, '\$.time.completed') IS NOT NULL
  AND json_extract(p.data, '\$.type') = 'text'
  AND s.parent_id IS NULL
  AND ${SESSION_FILTER}
ORDER BY s.id, m.id, p.id;
" 2>&1)"
rc=$?
if [[ $rc -ne 0 ]]; then
  log "sqlite query failed (rc=$rc): $(printf '%s' "$ROWS" | head -c 300)"
  exit 1
fi

if [[ -z "$ROWS" ]]; then
  log "no assistant turns to ingest"
  exit 0
fi

# Group parts into one turn per message: join text parts with blank lines,
# cap, flag truncation, format occurred_at (ms epoch -> RFC3339 UTC).
# A jq failure must surface as an error, not masquerade as "no turns"
# (ortho review 2026-07-13).
TURNS="$(printf '%s\n' "$ROWS" | jq -c -s --argjson max "$TURN_TEXT_MAX" '
  group_by([.sid, .mid])[]
  | {sid: .[0].sid, mid: .[0].mid, dir: (.[0].dir // ""), created: .[0].created}
    + (([.[].text | select(. != null)] | join("\n\n")) as $t
       | {text: $t[0:$max], truncated: (($t | length) > $max)})
  | select((.text | length) > 0)
  | .occurred_at = (((.created // 0) / 1000 | floor) | todate)
')"
rc=$?
if [[ $rc -ne 0 ]]; then
  log "turn grouping failed (jq rc=$rc)"
  exit 1
fi

if [[ -z "$TURNS" ]]; then
  log "no assistant turns to ingest"
  exit 0
fi

total_events=0
failed_events=0
session_count="$(printf '%s\n' "$TURNS" | jq -r '.sid' | sort -u | grep -c .)"
payload_file="${TMPDIR:-/tmp}/memoire-opencode-payload.${RANDOM}${RANDOM}.json"
trap 'rm -f "$payload_file"' EXIT
rel_db="${DB_FILE/#$HOME/\~}"

emit_event() {
  # emit_event PROJECT SESSION_ID CWD OCCURRED_AT SOURCE_EVENT_ID TEXT
  local project="$1" session_id="$2" cwd="$3"
  local occurred_at="$4" source_event_id="$5" text="$6"
  if [[ "$DRY_RUN" == "1" ]]; then
    jq -c --arg project "$project" \
          --arg session_id "$session_id" \
          --arg event_type "opencode_assistant_turn" \
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
      -source-adapter "opencode-cli" \
      -event-type "opencode_assistant_turn" \
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

# Base64 carries the capped text through @tsv intact (same transport as the
# qwen adapter).
declare -A CAPTURE_OFF_CACHE
while IFS=$'\t' read -r sid mid dir occurred_at truncated text_b64; do
  [[ -z "$sid" || -z "$mid" || -z "$text_b64" ]] && continue
  # A decode failure is a lost event, not a skip: count it so the exit code
  # reflects the loss (ortho review 2026-07-13).
  if ! text="$(printf '%s' "$text_b64" | base64 -d 2>/dev/null)"; then
    log "base64 decode failed for message ${mid} in session ${sid}"
    failed_events=$((failed_events + 1))
    continue
  fi
  [[ -z "$text" ]] && continue

  # liaison:D239 kill-switch gate: off ONLY on an explicit "off" (fail-open), applies
  # even under --project/MEMOIRE_PROJECT (override and toggle are orthogonal).
  # Cached per session directory — the loop is per message.
  if [[ -n "$dir" ]]; then
    if [[ -z "${CAPTURE_OFF_CACHE[$dir]:-}" ]]; then
      if [[ "$("$RESOLVER" --capture-enabled "$dir" 2>/dev/null)" == "off" ]]; then
        CAPTURE_OFF_CACHE[$dir]=1
      else
        CAPTURE_OFF_CACHE[$dir]=0
      fi
    fi
    [[ "${CAPTURE_OFF_CACHE[$dir]}" == "1" ]] && continue
  fi

  project="$PROJECT_OVERRIDE" MRP_ROOT="" MRP_SOURCE=""
  if [[ -z "$project" && -n "$dir" ]]; then
    memoire_resolve_v "$dir"
    project="$MRP_KEY"
  fi
  if [[ -z "$project" ]]; then
    project="opencode-unknown"
  fi

  jq -nc --arg db_file "$rel_db" \
         --arg message_id "$mid" \
         --arg g "$GENERATION" \
         --argjson tr "$truncated" \
    '{db_file:$db_file, message_id:$message_id,
      backfill_generation:$g, text_truncated:$tr}' > "$payload_file"

  if emit_event "$project" "$sid" "$dir" \
      "$occurred_at" "opencode-turn:${sid}:${mid}" "$text"; then
    total_events=$((total_events + 1))
  else
    log "queue write failed for message ${mid} in session ${sid}"
    failed_events=$((failed_events + 1))
  fi
done < <(printf '%s\n' "$TURNS" | jq -r '
  [.sid, .mid, .dir, .occurred_at, (.truncated | tostring), (.text | @base64)]
  | @tsv')

log "done: sessions=$session_count events=$total_events failed_events=$failed_events"
if [[ $failed_events -gt 0 ]]; then
  exit 1
fi
exit 0
