#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
#
# memoire-subagent-lib.sh — sourced by memoire-subagent-context-claude.sh and
# memoire-subagent-context-codex.sh. Shared here: the opt-in switches, the
# resolver gate, the bounded reads, the catch-up poll, the log line, the
# private working directory, and the selection (memoire-subagent-select.jq,
# the one rule). Per-store extraction stays in the extractor .jq files.
# Fail-open: nothing here may leave a hook with a non-zero exit or any output
# but the block.

# Deterministic text handling: nothing here counts characters in bash (counts
# are jq code points), so the caller's locale must not matter.
export LC_ALL=C

SA_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MEMOIRE_HOME="${MEMOIRE_HOME:-$HOME/.memoire}"
RESOLVER="${MEMOIRE_RESOLVE_PROJECT:-$SA_DIR/memoire-resolve-project.sh}"
SA_LOG="${MEMOIRE_SUBAGENT_LOG:-$MEMOIRE_HOME/hooks/subagent.log}"
SA_ENV_FILE="$MEMOIRE_HOME/capture-tick.env"
SA_TMPROOT="$MEMOIRE_HOME/tmp/subagent-context"

# Bounds (env, no redeploy). Validated: junk falls back to the default.
sa_int() { local v="$1" d="$2"; [[ "$v" =~ ^[1-9][0-9]{0,8}$ ]] && printf '%s' "$v" || printf '%s' "$d"; }
SA_MAX_CHARS="$(sa_int "${MEMOIRE_SUBAGENT_MAX_CHARS:-}" 8000)"
(( SA_MAX_CHARS < 600 )) && SA_MAX_CHARS=600
SA_MAX_ENTRIES="$(sa_int "${MEMOIRE_SUBAGENT_MAX_ENTRIES:-}" 60)"
SA_WINDOW_BYTES=524288      # fill window: last 512 KiB ...
SA_WINDOW_MAX=4194304       # ... widened to at most 4 MiB while it holds too few entries
SA_ANCHOR_BYTES=16777216    # anchor scan: last 16 MiB

SA_STATUS="start" SA_ENTRIES=0 SA_CHARS=0 SA_POLLS=0 SA_POLL_WHY="-" SA_BAD=0
SA_HARNESS="" SA_SESSION="" SA_AGENT="" SA_ATYPE=""
SA_TMP="" SA_CHILD=""

sa_now_ms() {
  if [[ -n "${EPOCHREALTIME:-}" ]]; then local t="${EPOCHREALTIME//[.,]/}"; printf '%s' "$(( t / 1000 ))"
  else printf '%s' "$(( $(date +%s%N) / 1000000 ))"; fi
}
SA_T0="$(sa_now_ms)"

# One log line per invocation: ids, sizes, timings. Never content. Written
# before the block is emitted (sa_write_log), else on exit.
SA_LOGGED=0
sa_write_log() {
  (( SA_LOGGED )) && return 0
  SA_LOGGED=1
  local ms=$(( $(sa_now_ms) - SA_T0 ))
  { mkdir -p "$(dirname "$SA_LOG")" && printf '%s memoire-subagent-context-%s: status=%s session=%s agent=%s type=%s entries=%s chars=%s polls=%s poll_end=%s bad_lines=%s elapsed_ms=%s\n' \
      "$(date -Is)" "$SA_HARNESS" "$SA_STATUS" "$SA_SESSION" "$SA_AGENT" "$SA_ATYPE" "$SA_ENTRIES" "$SA_CHARS" "$SA_POLLS" "$SA_POLL_WHY" "$SA_BAD" "$ms" >> "$SA_LOG"; } 2>/dev/null || true
}

# Heavy work runs as a background job of its own process group and the hook
# waits for it: bash defers a trapped signal until a FOREGROUND command
# returns, but `wait` is interrupted at once, so a hook told to stop kills its
# jq/grep pipelines instead of leaving them running.
sa_kill_child() {
  [[ -n "$SA_CHILD" ]] && kill -TERM -- "-$SA_CHILD" 2>/dev/null
  SA_CHILD=""
}
# sa_run <stdout-file> <cmd...>: stdout to the file, stderr and stdin discarded.
sa_run() {
  local out="$1" rc; shift
  "$@" > "$out" 2>/dev/null < /dev/null &
  SA_CHILD=$!
  wait "$SA_CHILD"; rc=$?
  SA_CHILD=""
  return "$rc"
}

sa_finalize() {
  sa_kill_child
  sa_write_log
  [[ -n "$SA_TMP" && -d "$SA_TMP" ]] && rm -rf "$SA_TMP" 2>/dev/null
  exit 0
}
sa_on_signal() { SA_STATUS="error=signal"; exit 0; }
sa_begin() {
  SA_HARNESS="$1"
  set -m   # background jobs get their own process group (sa_kill_child)
  trap sa_finalize EXIT
  trap sa_on_signal TERM INT HUP
}
sa_done() { SA_STATUS="$1"; exit 0; }

# The hook input is read before any gate: a hook that exits without reading
# its stdin can hand the harness an EPIPE.
sa_read_stdin() { HOOK_JSON="$(cat 2>/dev/null || true)"; }

# Private working directory under the Memoire home (never $TMPDIR or /tmp):
# mode 0700, named run.<pid>.<rand>, removed on every exit path by the EXIT
# trap. A run that was killed (SIGKILL cannot be trapped) leaves its directory;
# the next run removes those whose pid is dead, or older than ten minutes.
sa_mktmp() {
  local d pid
  ( umask 077; mkdir -p "$SA_TMPROOT" ) 2>/dev/null || return 1
  for d in "$SA_TMPROOT"/run.*; do
    [[ -d "$d" ]] || continue
    pid="${d##*/run.}"; pid="${pid%%.*}"
    if [[ "$pid" =~ ^[0-9]+$ ]] && ! kill -0 "$pid" 2>/dev/null; then rm -rf "$d" 2>/dev/null
    elif [[ -n "$(find "$d" -maxdepth 0 -mmin +10 2>/dev/null)" ]]; then rm -rf "$d" 2>/dev/null; fi
  done
  SA_TMP="$( umask 077; mktemp -d "$SA_TMPROOT/run.$$.XXXXXX" 2>/dev/null )" || { SA_TMP=""; return 1; }
}

# Switch file: extract the single key (last assignment wins), never source it.
# on|1|true enables; anything else, or no file, disables. Whitespace is the
# ASCII set only (the plugin's parser is the same).
sa_switch_on() {
  local key="$1" line val
  [[ -r "$SA_ENV_FILE" ]] || return 1
  line="$(grep -E "^[[:space:]]*(export[[:space:]]+)?${key}[[:space:]]*=" "$SA_ENV_FILE" 2>/dev/null | tail -n 1)" || return 1
  [[ -n "$line" ]] || return 1
  val="${line#*=}"
  val="${val%%#*}"
  val="$(printf '%s' "$val" | tr -d '\r' | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' -e "s/^[\"']//" -e "s/[\"']\$//" | tr '[:upper:]' '[:lower:]')"
  [[ "$val" == on || "$val" == 1 || "$val" == true ]]
}

# Resolver gate (readers' idiom): off only on an explicit "off".
sa_capture_off() { [[ "$("$RESOLVER" --capture-enabled "$1" 2>/dev/null)" == "off" ]]; }
sa_project() {
  local p=""
  p="$("$RESOLVER" "$1" 2>/dev/null)" || p=""
  [[ -n "$p" ]] || p="$(basename "$1" 2>/dev/null)"
  printf '%s' "$p"
}

sa_size() { stat -c %s "$1" 2>/dev/null || echo 0; }

# sa_tail_lines <src> <bytes>: stream the last <bytes> of src, whole lines only
# (no copy on disk).
sa_tail_lines() {
  if (( $(sa_size "$1") > $2 )); then tail -c "$2" "$1" 2>/dev/null | tail -n +2; else cat "$1" 2>/dev/null; fi
}

# sa_lines <src> <bytes> <dest>: the last <bytes> of src, whole lines only
# (the partial first line and an unterminated last line are dropped).
sa_lines() {
  local src="$1" bytes="$2" dest="$3" size
  size="$(sa_size "$src")"
  if (( size > bytes )); then
    tail -c "$bytes" "$src" 2>/dev/null | tail -n +2 > "$dest" || return 1
  else
    cat "$src" > "$dest" 2>/dev/null || return 1
  fi
  if [[ -s "$dest" ]] && (( $(tail -c 1 "$dest" | wc -l) == 0 )); then sed -i '$d' "$dest" 2>/dev/null; fi
  return 0
}

# sa_poll <check-fn> <file> <bound_ms> <stable_ms> <step_s>: returns when the
# check succeeds (signal), the size has been stable for stable_ms, or the bound.
sa_poll() {
  local fn="$1" file="$2" bound="$3" stable="$4" step="$5"
  local t0 now last last_change s
  t0="$(sa_now_ms)"; last="$(sa_size "$file")"; last_change="$t0"
  while :; do
    if sa_run /dev/null "$fn" "$file"; then SA_POLL_WHY="signal"; return 0; fi
    sleep "$step"
    SA_POLLS=$(( SA_POLLS + 1 ))
    now="$(sa_now_ms)"; s="$(sa_size "$file")"
    if [[ "$s" != "$last" ]]; then last="$s"; last_change="$now"; fi
    if (( now - last_change >= stable )); then SA_POLL_WHY="stable"; return 0; fi
    if (( now - t0 >= bound )); then SA_POLL_WHY="bound"; return 0; fi
  done
}

# sa_json_lines: each input line becomes one JSON string, byte for byte, so the
# extractors read a JSON stream (never `jq -R`). jq -R decodes every read buffer
# of a long line on its own, and a multibyte character that straddles two
# buffers becomes U+FFFD (the house reader, memoire-capture-codex-rollout.sh,
# documents this and avoids -R for the same reason). Byte-level sed (C locale):
# backslash, quote and the control characters JSON forbids raw are escaped,
# every other byte, any UTF-8 sequence included, passes through untouched. A
# line that is not valid UTF-8 has its invalid bytes replaced by U+FFFD by the
# JSON reader (a valid line is never altered). The extractors then parse each
# string with `try fromjson`, so a bad line (malformed, or a lone-surrogate
# escape) is skipped and counted, never fatal.
sa_json_lines() {
  LC_ALL=C sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/\t/\\t/g' -e 's/\r/\\r/g' \
    -e '/[\x00-\x08\x0b-\x1f]/{s/\x00/\\u0000/g;s/\x01/\\u0001/g;s/\x02/\\u0002/g;s/\x03/\\u0003/g;s/\x04/\\u0004/g;s/\x05/\\u0005/g;s/\x06/\\u0006/g;s/\x07/\\u0007/g;s/\x08/\\b/g;s/\x0b/\\u000b/g;s/\x0c/\\f/g;s/\x0e/\\u000e/g;s/\x0f/\\u000f/g;s/\x10/\\u0010/g;s/\x11/\\u0011/g;s/\x12/\\u0012/g;s/\x13/\\u0013/g;s/\x14/\\u0014/g;s/\x15/\\u0015/g;s/\x16/\\u0016/g;s/\x17/\\u0017/g;s/\x18/\\u0018/g;s/\x19/\\u0019/g;s/\x1a/\\u001a/g;s/\x1b/\\u001b/g;s/\x1c/\\u001c/g;s/\x1d/\\u001d/g;s/\x1e/\\u001e/g;s/\x1f/\\u001f/g}' \
    -e 's/^/"/' -e 's/$/"/'
}
# sa_extract <file> <jq args...>: the extractor over the lines of <file> (JSON mode).
sa_extract() { local f="$1"; shift; sa_json_lines < "$f" | jq -c -n "$@"; }

# sa_window_entries <transcript> <extractor.jq> <anchor-json> [jq args...]:
# entries of the fill window into $SA_TMP/entries.json. The window is the last
# 512 KiB; while it holds fewer than MAX_ENTRIES entries and the file has more
# (Claude writes a large Read result twice on one line), it widens to 2 MiB,
# then 4 MiB. Sets SA_ENTRIES and SA_BAD.
sa_window_entries() {
  local src="$1" ex="$2" anchor="$3" size win="$SA_WINDOW_BYTES" cut; shift 3
  size="$(sa_size "$src")"
  while :; do
    cut=false; (( size > win )) && cut=true
    sa_lines "$src" "$win" "$SA_TMP/win.jsonl" || return 1
    sa_run "$SA_TMP/entries.json" sa_extract "$SA_TMP/win.jsonl" --arg mode window --argjson anchor "$anchor" --argjson cut "$cut" "$@" \
      -f "$ex" || return 1
    SA_ENTRIES="$(jq '.entries | length' "$SA_TMP/entries.json" 2>/dev/null)" || return 1
    SA_BAD="$(jq '.bad_lines // 0' "$SA_TMP/entries.json" 2>/dev/null)" || SA_BAD=0
    (( SA_ENTRIES >= SA_MAX_ENTRIES || win >= SA_WINDOW_MAX || win >= size )) && return 0
    win=$(( win * 4 )); (( win > SA_WINDOW_MAX )) && win="$SA_WINDOW_MAX"
  done
}

# sa_pointer <session> <project> <where>
sa_pointer() {
  printf 'Parent session %s (project %s). This block is the parent'"'"'s recent activity only. Older context: search Memoire (memoire_search) under project %s, or read %s.' "$1" "$2" "$2" "$3"
}
SA_HEADER='# [memoire] parent context — what the parent session did just before this sub-agent started'

# sa_select <entries.json> <pointer> <out-file>: writes the block (may be empty).
sa_select() {
  sa_run "$3" jq -r -f "$SA_DIR/memoire-subagent-select.jq" --argjson maxc "$SA_MAX_CHARS" --argjson maxe "$SA_MAX_ENTRIES" \
    --arg header "$SA_HEADER" --arg pointer "$2" "$1"
}

# sa_emit <block>: the SubagentStart additionalContext shape.
sa_emit() {
  jq -cn --arg c "$1" '{hookSpecificOutput: {hookEventName: "SubagentStart", additionalContext: $c}}'
}

# sa_common_gates: shared preamble. Exits (via sa_done) when the guard, the
# switch, or the resolver gate says no.
sa_common_gates() {
  [[ -n "${MEMOIRE_ATTRIB_FORK:-}" ]] && sa_done "attrib-fork"
  sa_switch_on MEMOIRE_SUBAGENT_CONTEXT || sa_done "disabled"
  command -v jq >/dev/null 2>&1 || sa_done "error=jq-missing"
}
# sa_dir_gate <dir>...: capture:false (or MEMOIRE_CAPTURE=off) on any -> no injection.
sa_dir_gate() {
  local d
  for d in "$@"; do
    [[ -n "$d" ]] || continue
    sa_capture_off "$d" && sa_done "capture-off"
  done
  return 0
}
