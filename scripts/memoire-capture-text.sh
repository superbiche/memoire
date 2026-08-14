#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# liaison:D239 session-level kill switch: honored directly (not via the resolver) so
# it works even if the resolver script is missing or broken.
case "${MEMOIRE_CAPTURE:-}" in off|0|false) exit 0 ;; esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QUEUE_BIN="${MEMOIRE_QUEUE_BIN:-$ROOT/bin/memoire-queue}"
RESOLVER="${MEMOIRE_RESOLVE_PROJECT:-$ROOT/scripts/memoire-resolve-project.sh}"

# Resolve a project id via the shared paths->id resolver (liaison:D238). Fail-open to
# basename: a missing/broken resolver must never lose a capture. The resolver
# itself already honors the MEMOIRE_PROJECT env override, so callers only reach
# here when no explicit project was given.
#   memoire_resolve_v <event-dir> [session-home-dir]  (sets MRP_KEY/ROOT/SOURCE;
#   globals not command substitution — a subshell would drop root/source, which
#   ride the queue write for identity-conflict detection, liaison:D239 item 4)
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
PROJECT="${MEMOIRE_PROJECT:-}"
SESSION_ID="${MEMOIRE_SESSION_ID:-}"
SOURCE_ADAPTER="${MEMOIRE_SOURCE_ADAPTER:-manual-text}"
EVENT_TYPE="${MEMOIRE_EVENT_TYPE:-note}"
TEXT=""
CWD_VALUE="${MEMOIRE_CWD:-$PWD}"

usage() {
  cat <<'USAGE' >&2
Usage: memoire-capture-text.sh [options] [--] [text]

Writes one text note into Memoire's file queue through memoire-queue.
The Memoire API does not need to be running.

Options:
  --project NAME          Project name. Defaults to MEMOIRE_PROJECT, else the
                          shared resolver over the cwd (registry id, else basename).
  --session-id ID         Session id. Defaults to MEMOIRE_SESSION_ID or generated value.
  --source-adapter NAME   Producer/harness name. Default: manual-text.
  --event-type TYPE       Event type. Default: note.
  --cwd PATH              Working directory metadata. Default: current directory.
  --text TEXT             Text to capture. If omitted, stdin is used when piped.
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
    --source-adapter)
      SOURCE_ADAPTER="${2:-}"
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
    --)
      shift
      if [[ $# -gt 0 ]]; then
        TEXT="$*"
      fi
      break
      ;;
    -*)
      echo "unknown option: $1" >&2
      usage
      exit 2
      ;;
    *)
      TEXT="$*"
      break
      ;;
  esac
done

if [[ ! -x "$QUEUE_BIN" ]]; then
  echo "queue writer not found: $QUEUE_BIN" >&2
  echo "Run: make build" >&2
  exit 1
fi

if [[ -z "$TEXT" && ! -t 0 ]]; then
  TEXT="$(cat)"
fi
TEXT="$(printf '%s' "$TEXT" | sed -e 's/[[:space:]]*$//')"
if [[ -z "$TEXT" ]]; then
  echo "no text provided" >&2
  usage
  exit 2
fi

if [[ -z "$PROJECT" ]]; then
  # CWD_VALUE is the launch/session dir for text captures (no roaming), so it
  # doubles as the session home for attribution.
  memoire_resolve_v "$CWD_VALUE"
  PROJECT="$MRP_KEY"
fi

# liaison:D239 kill-switch gate: off ONLY on an explicit "off" (fail-open), applies
# even when PROJECT came from env/--project (override and toggle are
# orthogonal, liaison:D239 item 6).
if [[ "$("$RESOLVER" --capture-enabled "$CWD_VALUE" 2>/dev/null)" == "off" ]]; then
  exit 0
fi
if [[ -z "$SESSION_ID" ]]; then
  SESSION_ID="${SOURCE_ADAPTER}-$(date +%Y%m%dT%H%M%SZ)-$$"
fi

queue_args=(
  -project "$PROJECT"
  -session-id "$SESSION_ID"
  -source-adapter "$SOURCE_ADAPTER"
  -event-type "$EVENT_TYPE"
  -cwd "$CWD_VALUE"
  -text "$TEXT"
)
if [[ -n "${MRP_ROOT:-}" ]]; then
  queue_args+=(-project-root "$MRP_ROOT" -project-source "$MRP_SOURCE")
fi
"$QUEUE_BIN" "${queue_args[@]}"
