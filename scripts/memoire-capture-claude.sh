#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# liaison:D239 session-level kill switch: honored directly (not via the resolver) so
# it works even if the resolver script is missing or broken.
case "${MEMOIRE_CAPTURE:-}" in off|0|false) exit 0 ;; esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QUEUE_BIN="${MEMOIRE_QUEUE_BIN:-$ROOT/bin/memoire-queue}"
RESOLVER="${MEMOIRE_RESOLVE_PROJECT:-$ROOT/scripts/memoire-resolve-project.sh}"

# Resolve a project id via the shared paths->id resolver (liaison:D238/liaison:D239).
# Fail-open to basename so a resolver bug never loses a hook capture. Sets
# MRP_KEY plus MRP_ROOT/MRP_SOURCE (resolved root + source, forwarded to the
# queue writer for server-side identity-conflict detection, liaison:D239 item 4).
# Globals, not command substitution: a subshell would drop root/source.
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
# MEMOIRE_CAPTURE=off). Off ONLY on an explicit "off" from the resolver — a
# missing or failing resolver stays fail-open and capture proceeds.
#   memoire_capture_off <event-dir> [session-home-dir]
memoire_capture_off() {
  [[ "$("$RESOLVER" --capture-enabled "$1" "${2:-}" 2>/dev/null)" == "off" ]]
}
PROJECT="${MEMOIRE_PROJECT:-}"
SESSION_ID="${MEMOIRE_SESSION_ID:-${CLAUDE_CODE_SESSION_ID:-}}"
CWD_VALUE="${MEMOIRE_CWD:-${CLAUDE_PROJECT_DIR:-$PWD}}"
EVENT_TYPE="${MEMOIRE_EVENT_TYPE:-claude_hook}"
TEXT="${MEMOIRE_TEXT:-}"
STRICT="${MEMOIRE_CLAUDE_STRICT:-0}"
LOG_FILE="${MEMOIRE_CLAUDE_HOOK_LOG:-${MEMOIRE_HOME:-$HOME/.memoire}/hooks/claude.log}"

finish_error() {
  local code="${1:-1}"
  shift || true
  local msg="$*"
  if [[ -n "$msg" ]]; then
    mkdir -p "$(dirname "$LOG_FILE")" 2>/dev/null || true
    printf '%s memoire-capture-claude: %s\n' "$(date -Is)" "$msg" >> "$LOG_FILE" 2>/dev/null || true
    if [[ "$STRICT" == "1" ]]; then
      printf 'memoire-capture-claude: %s\n' "$msg" >&2
    fi
  fi
  if [[ "$STRICT" == "1" ]]; then
    exit "$code"
  fi
  exit 0
}

usage() {
  cat <<'USAGE' >&2
Usage: memoire-capture-claude.sh [options]

Reads one Claude Code hook JSON object from stdin and writes it into Memoire's
file queue. The raw hook JSON is preserved as payload; search-visible text stays
compact hook metadata unless MEMOIRE_TEXT or --text is set. The script is
fail-open by default for hook safety; set MEMOIRE_CLAUDE_STRICT=1 in tests.

Options:
  --project NAME          Project name. Defaults to MEMOIRE_PROJECT, else the
                          shared resolver over the hook cwd (registry id, else basename).
  --session-id ID         Session id. Defaults to MEMOIRE_SESSION_ID or CLAUDE_CODE_SESSION_ID.
  --event-type TYPE       Event type override.
  --cwd PATH              Working directory metadata. Default: CLAUDE_PROJECT_DIR or current dir.
  --text TEXT             Search-visible text override.
  -h, --help              Show this help.
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --project)
      PROJECT="${2:-}"
      shift 2
      ;;
    --session-id)
      SESSION_ID="${2:-}"
      shift 2
      ;;
    --event-type)
      EVENT_TYPE="${2:-}"
      shift 2
      ;;
    --cwd)
      CWD_VALUE="${2:-}"
      shift 2
      ;;
    --text)
      TEXT="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    -*)
      usage
      finish_error 2 "unknown option: $1"
      ;;
    *)
      usage
      finish_error 2 "unexpected argument: $1"
      ;;
  esac
done

if [[ ! -x "$QUEUE_BIN" ]]; then
  finish_error 1 "queue writer not found: $QUEUE_BIN; run make build"
fi

if [[ -t 0 ]]; then
  usage
  finish_error 2 "expected Claude hook JSON on stdin"
fi

if ! command -v jq >/dev/null 2>&1; then
  finish_error 1 "jq is required"
fi

payload_file="$(mktemp "${TMPDIR:-/tmp}/memoire-claude-payload.XXXXXX.json")"
cleanup() {
  rm -f "$payload_file"
}
trap cleanup EXIT

if ! cat > "$payload_file"; then
  finish_error 1 "failed to read hook JSON from stdin"
fi

if ! jq -e 'type == "object"' "$payload_file" >/dev/null 2>&1; then
  finish_error 2 "Claude hook payload must be a JSON object"
fi

if [[ -z "$PROJECT" ]]; then
  # For a live CC hook the payload .cwd is the session home (CC does not roam
  # across projects mid-session), so it doubles as the attribution dir.
  proj_cwd="$(jq -r '.cwd // .project_dir // empty' "$payload_file")"
  [[ -n "$proj_cwd" ]] || proj_cwd="$CWD_VALUE"
  memoire_resolve_v "$proj_cwd"
  PROJECT="$MRP_KEY"
fi

# Kill-switch gate (applies even when PROJECT came from env/--project: the
# project override and the capture toggle are orthogonal, liaison:D239 item 6).
if memoire_capture_off "${proj_cwd:-$CWD_VALUE}"; then
  echo "$(date -Is) capture off (kill switch) cwd=${proj_cwd:-$CWD_VALUE}" >> "$LOG_FILE" 2>/dev/null || true
  exit 0
fi

if [[ -z "$SESSION_ID" ]]; then
  SESSION_ID="$(jq -r '.session_id // .sessionId // empty' "$payload_file")"
fi
if [[ -z "$SESSION_ID" ]]; then
  SESSION_ID="claude-code-$(date +%Y%m%dT%H%M%SZ)-$$"
fi

hook_event="$(jq -r '.hook_event_name // .hookEvent // .event // empty' "$payload_file")"
tool_name="$(jq -r '.tool_name // .toolName // empty' "$payload_file")"
tool_use_id="$(jq -r '.tool_use_id // .toolUseID // .tool_use.id // .tool_use.id // empty' "$payload_file")"
if [[ "$EVENT_TYPE" == "claude_hook" && -n "$hook_event" ]]; then
  EVENT_TYPE="claude_${hook_event}"
fi

if [[ -z "$TEXT" ]]; then
  if [[ "${hook_event,,}" != "userpromptsubmit" ]]; then
    parts=("Claude Code hook")
    [[ -n "$hook_event" ]] && parts+=("event=$hook_event")
    [[ -n "$tool_name" ]] && parts+=("tool=$tool_name")
    [[ -n "$tool_use_id" ]] && parts+=("tool_use_id=$tool_use_id")
    [[ -n "$PROJECT" ]] && parts+=("project=$PROJECT")
    TEXT="${parts[*]}"
  fi
fi

source_event_id=""
if [[ -n "$SESSION_ID" && -n "$tool_use_id" && "${hook_event,,}" == "posttooluse" ]]; then
  source_event_id="claude-posttooluse-$(printf '%s' "$SESSION_ID:$tool_use_id" | sha256sum | awk '{print $1}')"
elif [[ -n "$SESSION_ID" && "${hook_event,,}" == "userpromptsubmit" ]]; then
  prompt_fingerprint="$(jq -c '{session_id:(.session_id // .sessionId // ""), hook_event_name:(.hook_event_name // .hookEvent // .event // ""), prompt:(.prompt // .user_prompt // .userPrompt // .message // .text // ""), cwd:(.cwd // .project_dir // "")}' "$payload_file" | sha256sum | awk '{print $1}')"
  source_event_id="claude-userpromptsubmit-$prompt_fingerprint"
elif [[ -n "$SESSION_ID" && ( "$EVENT_TYPE" == "claude_SessionStart" || "$EVENT_TYPE" == "claude_SessionEnd" || "$EVENT_TYPE" == "session_start" || "$EVENT_TYPE" == "session_end" ) ]]; then
  source_event_id="claude-session-marker-$(printf '%s' "$SESSION_ID:$EVENT_TYPE" | sha256sum | awk '{print $1}')"
else
  source_event_id="claude-payload-$(sha256sum "$payload_file" | awk '{print $1}')"
fi

args=(
  -project "$PROJECT"
  -session-id "$SESSION_ID"
  -source-adapter "claude-code"
  -event-type "$EVENT_TYPE"
  -cwd "$CWD_VALUE"
  -payload-file "$payload_file"
  -text "$TEXT"
)
if [[ -n "$source_event_id" ]]; then
  args+=(-source-event-id "$source_event_id")
fi
if [[ -n "${MRP_ROOT:-}" ]]; then
  args+=(-project-root "$MRP_ROOT" -project-source "$MRP_SOURCE")
fi

if ! "$QUEUE_BIN" \
  "${args[@]}"; then
  finish_error 1 "queue write failed"
fi
