#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# liaison:D239 session-level kill switch: honored directly (not via the resolver) so
# it works even if the resolver script is missing or broken.
case "${MEMOIRE_CAPTURE:-}" in off|0|false) exit 0 ;; esac
# The attribution fork (memoire-attribute-claude.py) is a headless session of its
# own: its hooks stay silent so the fork is never captured or injected into.
[[ -n "${MEMOIRE_ATTRIB_FORK:-}" ]] && exit 0

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

# notify_desktop TITLE BODY — a persistent desktop toast (critical urgency, no
# expiry) so a lost capture is seen even though the hook stays fail-open
# (operator ruling a1, 2026-10-08: the redaction key failing to load fails the
# write, and that failure must be loud). Best effort: no notify-send or no
# session bus means no toast, never a hook failure; the same bus discovery as
# memoire-capture-health.sh. MEMOIRE_CLAUDE_NO_NOTIFY=1 silences it (tests).
notify_desktop() {
  [[ "${MEMOIRE_CLAUDE_NO_NOTIFY:-}" == 1 ]] && return 0
  command -v notify-send >/dev/null 2>&1 || return 0
  local bus="${DBUS_SESSION_BUS_ADDRESS:-}"
  if [[ -z "$bus" && -n "${XDG_RUNTIME_DIR:-}" && -S "$XDG_RUNTIME_DIR/bus" ]]; then bus="unix:path=$XDG_RUNTIME_DIR/bus"; fi
  [[ -n "$bus" ]] || return 0
  DBUS_SESSION_BUS_ADDRESS="$bus" timeout 5 notify-send -u critical -t 0 -a memoire-capture-claude -- "$1" "$2" >/dev/null 2>&1 || true
}

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
    case "$msg" in
      "queue write failed"*) notify_desktop "Memoire: a Claude capture was NOT written" "$msg"$'\n'"Log: $LOG_FILE" ;;
    esac
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
  rm -f "$payload_file" "$payload_file.pat"
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
elif [[ -n "$SESSION_ID" && -n "$tool_use_id" && "${hook_event,,}" == "posttoolusefailure" ]]; then
  # A failed call never fires PostToolUse, so its id space is its own: the
  # same tool_use_id can never name both a success and a failure event.
  source_event_id="claude-posttoolusefailure-$(printf '%s' "$SESSION_ID:$tool_use_id" | sha256sum | awk '{print $1}')"
elif [[ -n "$SESSION_ID" && "${hook_event,,}" == "userpromptsubmit" ]]; then
  # Position: how many identical prompts the transcript already holds. The hook
  # fires before Claude Code appends the prompt, so the first occurrence sees 0
  # and keeps the historical fingerprint (stored ids never change); a later
  # identical prompt sees k>0 and hashes k in. A re-fire at the same position
  # sees the same k. Missing, unreadable or unscannable transcript -> 0 (fail open; all but missing logged).
  prompt_ordinal=0
  transcript_path="$(jq -r '.transcript_path // empty' "$payload_file")"
  # A missing file is the first prompt of a session (Claude Code creates the
  # transcript after the hook): ordinal 0, silent. An existing file that cannot
  # be read or scanned is logged.
  if [[ -n "$transcript_path" && -e "$transcript_path" ]]; then
    mkdir -p "$(dirname "$LOG_FILE")" 2>/dev/null || true
    if [[ -r "$transcript_path" ]]; then
      # The transcript stores a prompt as `"role":"user","content":<json string>}`;
      # tool results and assistant text never match (arrays, escaped quotes).
      # One fixed-string grep pass, pattern on stdin (never in a file: it holds
      # the prompt text); the scan is cut off at 2 s, inside the hook's 5 s.
      count=""
      if pat="$(jq -j '"\"role\":\"user\",\"content\":" + ((.prompt // .user_prompt // .userPrompt // .message // .text // "") | tojson) + "}"' "$payload_file" 2>/dev/null)"; then
        count="$(printf '%s' "$pat" | timeout 2 grep -cF -f - -- "$transcript_path" 2>/dev/null)" || true
      fi
      if [[ "$count" =~ ^[0-9]+$ ]]; then
        prompt_ordinal="$count"
      else
        echo "$(date -Is) prompt ordinal: transcript scan failed path=$transcript_path; using ordinal 0" >> "$LOG_FILE" 2>/dev/null || true
      fi
    else
      echo "$(date -Is) prompt ordinal: transcript unreadable path=$transcript_path; using ordinal 0" >> "$LOG_FILE" 2>/dev/null || true
    fi
  fi
  prompt_fingerprint="$(jq -c --argjson n "$prompt_ordinal" '{session_id:(.session_id // .sessionId // ""), hook_event_name:(.hook_event_name // .hookEvent // .event // ""), prompt:(.prompt // .user_prompt // .userPrompt // .message // .text // ""), cwd:(.cwd // .project_dir // "")} + (if $n > 0 then {ordinal:$n} else {} end)' "$payload_file" | sha256sum | awk '{print $1}')"
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

# Queue paths are transport bookkeeping, not model context. Preserve writer errors
# for the existing failure log/toast and strict-mode status.
qerr="$("$QUEUE_BIN" "${args[@]}" 2>&1 1>/dev/null)"; qrc=$?
if (( qrc != 0 )); then
  finish_error 1 "queue write failed: $(printf '%s' "$qerr" | head -c 300 | tr '\n' ' ')"
fi
