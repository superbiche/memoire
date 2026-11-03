#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
#
# memoire-trigger-lib.sh — sourced by memoire-trigger-capture-{claude,codex,
# opencode}.sh. The capture triggers carry no content: they run the accepted
# reader on the one session concerned (same ids as the timer tick, the hub
# collapses duplicates). Shared here: the switches (read per invocation from
# $MEMOIRE_HOME/capture-tick.env through the sub-agent library's single-key
# reader, never sourced), the one log line per run, the exit-0 finisher and the
# small process helpers. Fail-open: nothing here may leave a trigger with a
# non-zero exit or any output on stdout.
#
# Switches (increment 2, design 2.7 + amendment C4):
#   MEMOIRE_CAPTURE=off|0|false   kills every leg
#   MEMOIRE_HOOK_CAPTURE          opt-in, default off: the three trigger legs only
#   MEMOIRE_CAPTURE_CODEX / MEMOIRE_CAPTURE_OPENCODE   the existing leg gates
#                                 (env file first, then the process environment)
# The log line never carries content: ids, sizes, counts, timings.

# shellcheck source=memoire-subagent-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/memoire-subagent-lib.sh" || return 1

TR_DIR="$SA_DIR"
TR_HARNESS="" TR_STAGE="" TR_LOG="" TR_STATUS="start" TR_FIELDS="" TR_LOGGED=0
TR_T0="$(sa_now_ms)"

# tr_int <value> <default> [min]: positive integer or the default.
tr_int() { local v="$1" d="$2" m="${3:-1}"; if [[ "$v" =~ ^[0-9]{1,9}$ ]] && (( v >= m )); then printf '%s' "$v"; else printf '%s' "$d"; fi; }

# tr_clamp <value> <default> <min> <max>: the value when it is an integer, held
# between <min> and <max>; the default for junk. The knobs read from the
# environment go through it: no environment can push a hook past its budget.
tr_clamp() {
  local v d
  v="$(tr_int "$1" "$2" 0)"; d="$v"
  (( d < $3 )) && d="$3"
  (( d > $4 )) && d="$4"
  printf '%s' "$d"
}

# tr_clean: control characters stripped, 128 characters at most (ids come from
# the harness).
tr_clean() { printf '%s' "$1" | LC_ALL=C tr -d '\000-\037\177' | cut -c1-128; }

# tr_field k=v ...: fields appended to the run's log line.
tr_field() { TR_FIELDS="$TR_FIELDS $*"; }

tr_write_log() {
  (( TR_LOGGED )) && return 0
  TR_LOGGED=1
  local ms=$(( $(sa_now_ms) - TR_T0 ))
  { mkdir -p "$(dirname "$TR_LOG")" && printf '%s memoire-trigger-%s: stage=%s status=%s%s elapsed_ms=%s\n' \
      "$(date -Is)" "$TR_HARNESS" "$TR_STAGE" "$TR_STATUS" "$TR_FIELDS" "$ms" >> "$TR_LOG"; } 2>/dev/null || true
}

# tr_begin <harness> <stage>: the log target and the exit-0 finisher.
tr_begin() {
  TR_HARNESS="$1" TR_STAGE="$2"
  TR_LOG="${MEMOIRE_TRIGGER_LOG:-$MEMOIRE_HOME/log/trigger-$1.log}"
  trap tr_finish EXIT
  trap 'TR_STATUS="error=signal"; exit 0' TERM INT HUP
}
tr_finish() { tr_write_log; exit 0; }
tr_done() { TR_STATUS="$1"; exit 0; }

# tr_leg_switch <KEY>: a leg's own gate. The env file wins when it carries the
# key (on|1|true); without the key, the process environment decides. Used for
# the existing MEMOIRE_CAPTURE_CODEX / _OPENCODE gates only.
tr_leg_switch() {
  local key="$1" v
  sa_switch_on "$key" && return 0
  [[ -r "$SA_ENV_FILE" ]] && grep -qE "^[[:space:]]*(export[[:space:]]+)?${key}[[:space:]]*=" "$SA_ENV_FILE" 2>/dev/null && return 1
  v="${!key:-}"; v="${v,,}"
  [[ "$v" == on || "$v" == 1 || "$v" == true ]]
}

# tr_capture_off_file: true when the last MEMOIRE_CAPTURE assignment of the env
# file is off|0|false (the ticks get it from that file through the unit's
# EnvironmentFile; a hook child has no EnvironmentFile, so it reads it here).
tr_capture_off_file() {
  local v; v="$(tr_key_value MEMOIRE_CAPTURE | tr '[:upper:]' '[:lower:]')"
  [[ "$v" == off || "$v" == 0 || "$v" == false ]]
}

# tr_gates [leg-switch-key]: exits (status capture-off for the process
# environment, disabled for the env file or a gate) unless MEMOIRE_CAPTURE
# allows, MEMOIRE_HOOK_CAPTURE is on, and the leg's own gate is on.
tr_gates() {
  case "${MEMOIRE_CAPTURE:-}" in off|0|false) tr_done "capture-off" ;; esac
  tr_capture_off_file && tr_done "disabled"
  sa_switch_on MEMOIRE_HOOK_CAPTURE || tr_done "disabled"
  [[ -z "${1:-}" ]] || tr_leg_switch "$1" || tr_done "disabled"
}

# tr_key_value <KEY>: the last assignment of one key of the env file (same
# extraction as sa_switch_on); empty when absent.
tr_key_value() {
  local line val
  [[ -r "$SA_ENV_FILE" ]] || return 0
  line="$(grep -E "^[[:space:]]*(export[[:space:]]+)?${1}[[:space:]]*=" "$SA_ENV_FILE" 2>/dev/null | tail -n 1)" || return 0
  [[ -n "$line" ]] || return 0
  val="${line#*=}"; val="${val%%#*}"
  printf '%s' "$val" | tr -d '\r' | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' -e "s/^[\"']//" -e "s/[\"']\$//"
}

# tr_alive <pid>: running and not a zombie.
tr_alive() {
  kill -0 "$1" 2>/dev/null || return 1
  ! grep -q '^State:[[:space:]]*Z' "/proc/$1/status" 2>/dev/null
}
# tr_wait_pid <pid> <ms>: returns as soon as the pid is gone, else after <ms>.
tr_wait_pid() {
  local t0 now; t0="$(sa_now_ms)"
  while tr_alive "$1"; do
    now="$(sa_now_ms)"; (( now - t0 >= $2 )) && return 1
    sleep 0.05
  done
  return 0
}

# tr_spawn <stdout-stderr-file|/dev/null> <cmd...>: start <cmd> in a session of
# its own with stdin closed and stdout/stderr on the file; sets TR_PID. Without
# setsid the command is still started, in the caller's session.
tr_spawn() {
  local out="$1"; shift
  if command -v setsid >/dev/null 2>&1; then setsid "$@" >>"$out" 2>&1 </dev/null &
  else "$@" >>"$out" 2>&1 </dev/null &
  fi
  TR_PID=$!
}

# tr_tail_lines <file> <bytes>: the last <bytes> of a file, whole lines only.
tr_tail_lines() {
  if (( $(sa_size "$1") > $2 )); then tail -c "$2" "$1" 2>/dev/null | tail -n +2; else cat "$1" 2>/dev/null; fi
}
