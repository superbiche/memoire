#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-subagent-context-claude.sh — Claude Code SubagentStart hook: inject
# the PARENT session's recent activity (the prompt that opened its turn, its
# recent tool calls with a result excerpt, its recent assistant text) into the
# sub-agent's context. A local read of the parent's own transcript; no hub, no
# queue, no capture trigger. SubagentStart carries the parent's session_id and
# transcript_path (probe-verified on 2.1.285).
#
# Opt-in: MEMOIRE_SUBAGENT_CONTEXT=on in $MEMOIRE_HOME/capture-tick.env, read per
# invocation; without it the hook logs "disabled" and emits nothing. The block
# is sent to the sub-agent's model, which on Claude Code is the parent's
# provider. Selection rule and caps: memoire-subagent-select.jq (shared).
# The session, agent and type fields written to the log (and the session in the pointer)
# are stripped of control characters and bounded to 128 characters.
# Fail-open: any error exits 0 with no output and one log line. A bad line in
# the transcript is skipped and counted (bad_lines=N in the log), never fatal.
#
# registered-timeout-s: 5

# shellcheck source=memoire-subagent-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/memoire-subagent-lib.sh" || exit 0
sa_begin claude
sa_read_stdin
sa_common_gates

IFS=$'\x1f' read -r SA_SESSION TRANSCRIPT SA_AGENT SA_ATYPE PCWD < <(
  printf '%s' "$HOOK_JSON" | jq -r 'def c: tostring | .[0:512] | gsub("[\u0000-\u001f\u007f-\u009f\u2028\u2029]"; "") | .[0:128]; [(.session_id // "" | c), .transcript_path // "", (.agent_id // "" | c), (.agent_type // "" | c), .cwd // ""] | join("\u001f")' 2>/dev/null) || true
[[ -n "$PCWD" ]] || PCWD="${CLAUDE_PROJECT_DIR:-$PWD}"
[[ -n "$TRANSCRIPT" && -r "$TRANSCRIPT" ]] || sa_done "error=transcript-unreadable"

# Parent's directory and the sub-agent's (the hook process directory).
sa_dir_gate "$PCWD" "$PWD" "${CLAUDE_PROJECT_DIR:-}"

sa_mktmp || sa_done "error=mktemp"

# Catch-up poll (B4): the delegation call lands in the transcript a moment
# after the hook starts. Stop on an Agent/Task tool_use with no tool_result
# yet; fallback size stable for 500 ms; bound 1.5 s.
claude_unmatched_spawn() {
  tail -c 262144 "$1" 2>/dev/null | jq -R -n '
    reduce (inputs | fromjson? | select(type == "object")) as $l ({u: {}, r: {}};
      if $l.type == "assistant" then reduce (($l.message.content // []) | if type == "array" then .[] else empty end | select(type == "object" and .type == "tool_use" and (.name == "Agent" or .name == "Task"))) as $t (.; .u[$t.id] = 1)
      elif $l.type == "user" then reduce (($l.message.content // []) | if type == "array" then .[] else empty end | select(type == "object" and .type == "tool_result")) as $t (.; .r[$t.tool_use_id] = 1)
      else . end)
    | ((.u | keys) - (.r | keys) | length) > 0' 2>/dev/null | grep -q true
}
sa_poll claude_unmatched_spawn "$TRANSCRIPT" 1500 500 0.1

# Anchor (B2a): newest prompt line, its own backwards scan bounded at 16 MiB,
# independent of the fill window, streamed (no copy). grep prefilters user
# lines that are not tool_result envelopes; jq classifies the newest candidates.
claude_candidates() {
  sa_tail_lines "$1" "$SA_ANCHOR_BYTES" | grep -a '"type":"user"' | grep -av '"type":"tool_result"' | tail -n 60
}
sa_run "$SA_TMP/cand.jsonl" claude_candidates "$TRANSCRIPT"
sa_run "$SA_TMP/anchor.json" sa_extract "$SA_TMP/cand.jsonl" --arg mode anchor --argjson anchor null --argjson cut false \
  -f "$SA_DIR/memoire-subagent-extract-claude.jq" || sa_done "error=parse"
ANCHOR="$(<"$SA_TMP/anchor.json")"; [[ -n "$ANCHOR" ]] || ANCHOR=null

sa_window_entries "$TRANSCRIPT" "$SA_DIR/memoire-subagent-extract-claude.jq" "$ANCHOR" || sa_done "error=parse"

PROJECT="$(sa_project "$PCWD")"
POINTER="$(sa_pointer "$SA_SESSION" "$PROJECT" "$TRANSCRIPT")"
sa_select "$SA_TMP/entries.json" "$POINTER" "$SA_TMP/block.txt" || sa_done "error=select"
BLOCK="$(<"$SA_TMP/block.txt")"
[[ -n "$BLOCK" ]] || sa_done "empty"
SA_CHARS="$(jq -n --arg c "$BLOCK" '$c | length' 2>/dev/null)" || SA_CHARS=0
SA_STATUS="injected"; sa_write_log
sa_emit "$BLOCK" || true
exit 0
