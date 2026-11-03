#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-subagent-context-codex.sh — Codex SubagentStart hook: inject the
# parent thread's recent activity into the sub-agent's context, from a local
# read of the parent's rollout. Same shape as the Claude injector; the store
# extractor is memoire-subagent-extract-codex.jq (record classification of the
# accepted reader, memoire-capture-codex-rollout.sh). Always injects, even when
# the model forked the history: an 8000-char duplicate is cheap, and the
# native start hook names the child's rollout, resolved through its metadata.
#
# Opt-in: MEMOIRE_SUBAGENT_CONTEXT=on in $MEMOIRE_HOME/capture-tick.env, read per
# invocation. Poll (B3): stop on a spawn call with no output yet in the tail,
# or after 500 ms of stable size; bound 1 s; elapsed time logged before the
# block is emitted. The session, agent and type fields written to the log (and the
# session in the pointer) are stripped of control characters and bounded to 128
# characters. Fail-open: any error exits 0, no output, one log line. A bad
# line in the rollout is skipped and counted (bad_lines=N), never fatal.
#
# registered-timeout-s: 4

# shellcheck source=memoire-subagent-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/memoire-subagent-lib.sh" || exit 0
sa_begin codex
sa_read_stdin
sa_common_gates

IFS=$'\x1f' read -r SA_SESSION TRANSCRIPT SA_AGENT SA_ATYPE PCWD < <(
  printf '%s' "$HOOK_JSON" | jq -r 'def c: tostring | .[0:512] | gsub("[\u0000-\u001f\u007f-\u009f\u2028\u2029]"; "") | .[0:128]; [(.session_id // "" | c), .transcript_path // "", (.agent_id // "" | c), (.agent_type // "" | c), .cwd // ""] | join("\u001f")' 2>/dev/null) || true
[[ -n "$PCWD" ]] || PCWD="$PWD"
[[ -n "$TRANSCRIPT" && -r "$TRANSCRIPT" ]] || sa_done "error=transcript-unreadable"

sa_dir_gate "$PCWD" "$PWD"

sa_mktmp || sa_done "error=mktemp"
sa_run "$SA_TMP/parent.json" python3 "$SA_DIR/memoire-codex-parent.py" "$TRANSCRIPT" "$SA_AGENT" || sa_done "error=parent-resolver"
PARENT_ERROR="$(jq -r '.error // empty' "$SA_TMP/parent.json")"
[[ -z "$PARENT_ERROR" ]] || sa_done "error=$PARENT_ERROR"
IFS=$'\x1f' read -r TRANSCRIPT PARENT_ID PCWD CHILD_CWD < <(
  jq -r '[.path, .id, .cwd, .child_cwd] | join("\u001f")' "$SA_TMP/parent.json")
SA_SESSION="$PARENT_ID"
sa_dir_gate "$PCWD" "$CHILD_CWD"

codex_unmatched_spawn() {
  tail -c 262144 "$1" 2>/dev/null | jq -R -n '
    reduce (inputs | fromjson? | select(type == "object" and .type == "response_item")) as $l ({u: {}, r: {}};
      ($l.payload // {}) as $p
      | if ($p.type == "function_call" or $p.type == "custom_tool_call") and (($p.name // "") | test("spawn")) then .u[$p.call_id // ""] = 1
        elif $p.type == "function_call_output" or $p.type == "custom_tool_call_output" then .r[$p.call_id // ""] = 1
        else . end)
    | ((.u | keys) - (.r | keys) | length) > 0' 2>/dev/null | grep -q true
}
sa_poll codex_unmatched_spawn "$TRANSCRIPT" 1000 500 0.05

# Inherited parent records of a forked sub-agent thread are the parent's own
# history: excluded below the line-0 subagent_history_start_ordinal.
HIST="$(head -n 1 "$TRANSCRIPT" 2>/dev/null | jq -r '(.payload.subagent_history_start_ordinal // 0) | tostring' 2>/dev/null)" || HIST=0
[[ "$HIST" =~ ^[0-9]+$ ]] || HIST=0

# Anchor (B1, B2a): the first client-authored user message after the newest
# task_started in the parent (the hook's turn_id belongs to the child), scanned over at
# most 16 MiB independent of the fill window. One streaming pass: awk keeps
# the first 40 user-message lines after the newest parent task_started;
# jq then classifies only those.
# Nothing parses the whole turn just to find its first message.
codex_candidates() {
  sa_tail_lines "$1" "$SA_ANCHOR_BYTES" | awk '
    /"type": ?"task_started"/ { active = 1; n = 0; delete lines; next }
    /"role": ?"user"/ { if (active && n < 40) lines[++n] = $0 }
    END { for (i = 1; i <= n; i++) print lines[i] }'
}
sa_run "$SA_TMP/cand.jsonl" codex_candidates "$TRANSCRIPT"
sa_run "$SA_TMP/anchor.json" sa_extract "$SA_TMP/cand.jsonl" --arg mode anchor --argjson anchor null --argjson cut false --argjson hist "$HIST" \
  -f "$SA_DIR/memoire-subagent-extract-codex.jq" || sa_done "error=parse"
ANCHOR="$(<"$SA_TMP/anchor.json")"; [[ -n "$ANCHOR" ]] || ANCHOR=null

sa_window_entries "$TRANSCRIPT" "$SA_DIR/memoire-subagent-extract-codex.jq" "$ANCHOR" --argjson hist "$HIST" || sa_done "error=parse"

PROJECT="$(sa_project "$PCWD")"
POINTER="$(sa_pointer "$PARENT_ID" "$PROJECT" "$TRANSCRIPT")"
sa_select "$SA_TMP/entries.json" "$POINTER" "$SA_TMP/block.txt" || sa_done "error=select"
BLOCK="$(<"$SA_TMP/block.txt")"
[[ -n "$BLOCK" ]] || sa_done "empty"
SA_CHARS="$(jq -n --arg c "$BLOCK" '$c | length' 2>/dev/null)" || SA_CHARS=0
SA_STATUS="injected"; sa_write_log
sa_emit "$BLOCK" || true
exit 0
