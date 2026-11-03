#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-attribute-claude.sh — Claude Code `Stop` hook: attribution at Stop
# (rework candidate docs/rework/retrieval-consumer/attribution-at-stop.md,
# impact view attribution-increment-impact.md). Thin entry: kill switches and
# the cheap skips run here; the transcript scan, the forked question and the
# queue write live in scripts/memoire-attribute-claude.py.
#
# One attribution = one transient systemd user unit (`systemd-run --user`).
# The unit's control group is the lifecycle boundary: the worker, the fork and
# whatever the fork starts live in it and end with it. The user manager is the
# supervisor and enforces the runtime cap (RuntimeMaxSec). Where no user
# manager answers, the hook does nothing and logs the skip: there is no
# unsupervised fallback.
#
# Every attribution runs in the background: this script starts the unit and
# returns. It never follows a unit and makes ONE call to the user manager, the
# start, under a deadline. The whole table:
#
#   the start is...   the hook...                      turn waits   payload
#   confirmed         returns                          tens of ms   the worker's; the unit removes
#                                                                   it at its end if the worker
#                                                                   could not
#   refused           logs the skip, returns           tens of ms   removed here
#   not answered      logs "not confirmed", returns;   5 s (+1 s    the unit's if it did start;
#   within 5 s        a unit that did start runs       if the       otherwise swept by a later
#                     supervised, as any other         client has   run, after an hour
#                                                      to be
#                                                      killed)
#
# The hook's exit status is decided HERE and is always 0: an attribution that
# cannot run is an absent signal, logged, never an error to the turn.
trap 'exit 0' EXIT

case "${MEMOIRE_CAPTURE:-}" in off|0|false) exit 0 ;; esac
case "${MEMOIRE_ATTRIB:-}" in off|0|false) exit 0 ;; esac
case "${MEMOIRE_ATTRIB_MODE:-}" in off|0|false) exit 0 ;; esac
# The forked session runs the same Stop hook: never recurse.
[[ -n "${MEMOIRE_ATTRIB_FORK:-}" ]] && exit 0

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKER="${MEMOIRE_ATTRIB_WORKER:-$ROOT/scripts/memoire-attribute-claude.py}"
RESOLVER="${MEMOIRE_RESOLVE_PROJECT:-$ROOT/scripts/memoire-resolve-project.sh}"
MEMOIRE_HOME_DIR="${MEMOIRE_HOME:-${HOME:-}/.memoire}"
LOG_FILE="${MEMOIRE_ATTRIB_LOG:-$MEMOIRE_HOME_DIR/hooks/attrib.log}"
CLAUDE_BIN="${MEMOIRE_CLAUDE_BIN:-claude}"
DEFAULT_CAP=200
LAUNCH_DEADLINE=5   # seconds for the manager to confirm the unit's start

log() {
  mkdir -p "$(dirname "$LOG_FILE")" 2>/dev/null || true
  printf '%s memoire-attribute-claude: %s\n' "$(date -Is)" "$*" >> "$LOG_FILE" 2>/dev/null || true
}

# bounded <seconds> <command...>: run it, stop it at the deadline (exit 124, or
# 137 when it had to be killed). Never read its output through a pipe: a child
# it left behind would hold the pipe, and the hook with it.
bounded() { local s="$1"; shift; timeout -k 1 "$s" "$@"; }

# A path systemd can take inside a unit property without quoting questions.
plain_path() { [[ "$1" =~ ^/[A-Za-z0-9._/+@-]+$ ]]; }

# Same resolver helpers as memoire-capture-claude.sh (liaison:D238/D239):
# fail-open to basename, kill switch only on an explicit "off".
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
memoire_capture_off() {
  [[ "$("$RESOLVER" --capture-enabled "$1" "${2:-}" 2>/dev/null)" == "off" ]]
}

[[ -t 0 ]] && exit 0
for tool in jq python3 systemd-run timeout rm; do
  if ! command -v "$tool" >/dev/null 2>&1; then log "skip: $tool not found"; exit 0; fi
done
PY_BIN="$(command -v python3)"
RM_BIN="$(command -v rm)"
CLAUDE_ABS="$(command -v "$CLAUDE_BIN" 2>/dev/null)" || CLAUDE_ABS=""
if [[ -z "$CLAUDE_ABS" ]]; then log "skip: claude binary not found ($CLAUDE_BIN)"; exit 0; fi
if [[ ! -f "$WORKER" || ! -r "$WORKER" ]]; then log "skip: worker not found or not readable: $WORKER"; exit 0; fi
# The unit does not run in this directory: it gets absolute paths.
[[ "$CLAUDE_ABS" == /* ]] || CLAUDE_ABS="$PWD/$CLAUDE_ABS"
[[ "$WORKER" == /* ]] || WORKER="$PWD/$WORKER"

CAP="${MEMOIRE_ATTRIB_TIMEOUT:-$DEFAULT_CAP}"
if [[ ! "$CAP" =~ ^[1-9][0-9]{0,4}$ ]]; then
  log "invalid MEMOIRE_ATTRIB_TIMEOUT ($CAP): using ${DEFAULT_CAP}s"
  CAP="$DEFAULT_CAP"
fi

# The payload waits for the worker in the user runtime directory (memory-backed,
# private to the user, the same directory the user manager lives in).
RUN_DIR="${XDG_RUNTIME_DIR:-}"
if [[ -z "$RUN_DIR" || ! -d "$RUN_DIR" ]] || ! plain_path "$RUN_DIR"; then
  log "skip: no usable XDG_RUNTIME_DIR, no user systemd manager to supervise the attribution"; exit 0
fi
RUN_DIR="$RUN_DIR/memoire-attrib"
if ! mkdir -p -m 700 "$RUN_DIR" 2>/dev/null; then log "skip: cannot create $RUN_DIR"; exit 0; fi
# A payload no unit ever took (hook stopped before the start): gone after an hour.
find "$RUN_DIR" -maxdepth 1 -name 'memoire-attrib-*.json' -mmin +60 -delete 2>/dev/null || true
payload_file="$(mktemp "$RUN_DIR/memoire-attrib-XXXXXXXXXX.json" 2>/dev/null)" || { log "skip: cannot create the payload file"; exit 0; }
UNIT="$(basename "$payload_file" .json)"
if ! cat > "$payload_file" || ! jq -e 'type == "object"' "$payload_file" >/dev/null 2>&1; then
  rm -f "$payload_file"; exit 0
fi
# Re-entry after another Stop hook blocked: nothing to do, this hook never blocks.
if [[ "$(jq -r '.stop_hook_active // false' "$payload_file")" == "true" ]]; then
  rm -f "$payload_file"; exit 0
fi

proj_cwd="$(jq -r '.cwd // .project_dir // empty' "$payload_file")"
[[ -n "$proj_cwd" ]] || proj_cwd="${CLAUDE_PROJECT_DIR:-$PWD}"
if memoire_capture_off "$proj_cwd"; then
  log "skip: capture off (kill switch) cwd=$proj_cwd"; rm -f "$payload_file"; exit 0
fi
PROJECT="${MEMOIRE_PROJECT:-}"
if [[ -z "$PROJECT" ]]; then
  memoire_resolve_v "$proj_cwd"
  PROJECT="$MRP_KEY"
fi
session_id="$(jq -r '.session_id // empty' "$payload_file")"

# The unit gets NAMED values only, none of them a secret: nothing else of the
# session's environment reaches it (the user manager's own environment does).
run=(systemd-run --user --collect --quiet --service-type=exec --unit="$UNIT"
  --description="Memoire attribution at Stop"
  -p RuntimeMaxSec="$CAP" -p KillMode=control-group -p TimeoutStopSec=5
  --setenv=PATH="$PATH" --setenv=HOME="${HOME:-}"
  --setenv=MEMOIRE_HOME="$MEMOIRE_HOME_DIR" --setenv=MEMOIRE_PROJECT="$PROJECT"
  --setenv=MEMOIRE_PROJECT_ROOT="${MRP_ROOT:-}" --setenv=MEMOIRE_PROJECT_SOURCE="${MRP_SOURCE:-}"
  --setenv=MEMOIRE_ATTRIB_UNIT="$UNIT"
  --setenv=MEMOIRE_ATTRIB_TIMEOUT="$CAP" --setenv=MEMOIRE_CLAUDE_BIN="$CLAUDE_ABS")
for name in MEMOIRE_QUEUE_BIN MEMOIRE_ATTRIB_MODEL MEMOIRE_ATTRIB_FORK_TRANSCRIPT CLAUDE_CONFIG_DIR; do
  [[ -n "${!name:-}" ]] && run+=(--setenv="$name=${!name}")
done
if plain_path "$RM_BIN"; then
  run+=(-p "ExecStopPost=$RM_BIN -f $payload_file")
fi
mkdir -p "$(dirname "$LOG_FILE")" 2>/dev/null || true
if plain_path "$LOG_FILE"; then
  run+=(-p "StandardOutput=append:$LOG_FILE" -p "StandardError=append:$LOG_FILE")
fi

# The only call to the user manager. The worker owns and deletes the payload
# file; the unit's ExecStopPost removes it when the worker could not.
if [[ -n "${MEMOIRE_ATTRIB_MODE:-}" ]]; then
  log "MEMOIRE_ATTRIB_MODE=${MEMOIRE_ATTRIB_MODE} has no effect: every attribution runs in the background (only off is read)"
fi
log "unit $UNIT starting cap=${CAP}s session=$session_id"
bounded "$LAUNCH_DEADLINE" "${run[@]}" -- "$PY_BIN" "$WORKER" "$payload_file" >> "$LOG_FILE" 2>&1 </dev/null
rc=$?
if [[ "$rc" == 124 || "$rc" == 137 ]]; then
  # The payload stays: a unit that did start owns it. Otherwise a later run sweeps it.
  log "unit $UNIT start not confirmed within ${LAUNCH_DEADLINE}s: not followed; if it did start it runs supervised under its cap"
elif [[ "$rc" != 0 ]]; then
  log "skip: unit $UNIT not started (systemd-run exit $rc)"
  rm -f "$payload_file"
fi
exit 0
