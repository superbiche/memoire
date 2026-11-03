#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# subagent-context-test.sh — tests of the sub-agent parent-context injectors
# (scripts/memoire-subagent-context-claude.sh, -codex.sh, the shared library
# and selection rule, and the OpenCode plugin's child-session leg through
# subagent-context-plugin-test.mjs). Core lane (`make test-core`): offline,
# hermetic — fixtures generated here, a scratch MEMOIRE_HOME and HOME, no
# harness, no network, never the operator's project registry.
#
#   e2e       (E1.4) the Bash result before the Agent call is in the block
#   switch    opt-in parsing: no file, off, on|1|true, quotes, export, last wins,
#             comments, and the file is never sourced
#   exclude   (B7) meta, compaction, interruption, envelopes, reminders,
#             attachments, sidechains, the Memoire SessionStart block
#   select    (B2) omission line, caps, ordering, anchor beyond the fill window,
#             anchor beyond 16 MiB, pre-anchor entries above the boundary line
#   poll      (B4) catch-up on an unmatched Agent call, stable fallback
#   failopen  (B9c) jq missing, transcript unreadable, malformed line
#   gates     (B9d) capture:false on the parent's directory, then the child's
#   codex     (B9a) real 0.159.1 record shapes; (B9e) time against the timeout
#   parity    the block structure is the same across bash Claude, bash Codex
#             and the plugin for equivalent input
#   plugin    (B9b, B9f, B5, B8) through the node test
#   round 3   N2 require-ran needs the suite's own OK line; A1 the block fits
#             every accepted MAX_CHARS (parity extended); A2 log fields are
#             stripped and bounded; N5 wall-time limits scale with a baseline

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CLAUDE="$ROOT/scripts/memoire-subagent-context-claude.sh"
CODEX="$ROOT/scripts/tests/codex-subagent-fixture.py"
CODEX_HOOK="$ROOT/scripts/memoire-subagent-context-codex.sh"
TESTDIR="$ROOT/scripts/tests"

command -v jq >/dev/null || { echo "SKIP: jq not available (required by the injectors)"; exit 0; }

# The tests state their locale: character counts and emoji must not depend on
# the caller's (the scripts themselves run under LC_ALL=C and count in jq).
for loc in C.UTF-8 en_US.UTF-8 C.utf8; do
  if locale -a 2>/dev/null | grep -qix "$loc"; then export LC_ALL="$loc"; break; fi
done
: "${LC_ALL:=C.UTF-8}"; export LC_ALL

# node is resolved BEFORE HOME is overridden: a version-manager shim needs the
# real HOME to find its runtime. A skipped node suite says why and the runner
# (require-ran.sh) fails the target.
NODE=""; NODE_WHY=""
if command -v node >/dev/null 2>&1; then NODE="$(node -p 'process.execPath' 2>/dev/null)"; fi
if [[ -z "$NODE" ]]; then NODE_WHY="node not found on PATH, or it does not run (version-manager shim without its runtime?)"
elif ! "$NODE" -e 'process.exit(globalThis.crypto?.randomUUID ? 0 : 1)' 2>/dev/null; then NODE_WHY="$NODE ($("$NODE" --version 2>&1)) has no global crypto.randomUUID"; fi

fails=0
fail() { echo "FAIL: $*" >&2; fails=$(( fails + 1 )); }
ok() { local msg="$1"; shift; "$@" || fail "$msg"; }
has() { grep -qF -- "$2" <<<"$1"; }
chars() { printf '%s' "$1" | jq -Rs 'length'; }   # code points, whatever the locale
lacks() { ! grep -qF -- "$2" <<<"$1"; }

SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/subagent-context-test.XXXXXX")"
trap 'rm -rf "$SCRATCH"' EXIT
export HOME="$SCRATCH/home"; mkdir -p "$HOME"
export MEMOIRE_HOME="$SCRATCH/mh"; mkdir -p "$MEMOIRE_HOME/hooks"
export MEMOIRE_PROJECTS_REGISTRY="$SCRATCH/no-registry.json"
export TMPDIR="$SCRATCH/tmp"; mkdir -p "$TMPDIR"
export TZ=UTC
unset MEMOIRE_PROJECT MEMOIRE_CAPTURE MEMOIRE_ATTRIB_FORK MEMOIRE_SUBAGENT_MAX_CHARS MEMOIRE_SUBAGENT_MAX_ENTRIES
LOGF="$MEMOIRE_HOME/hooks/subagent.log"
PROJ="$SCRATCH/proj"; mkdir -p "$PROJ"

# N5: wall-time limits scale with a baseline measured now (a fixed jq workload, best of 3;
# 150 ms on the development workstation): limit x baseline/150, clamped to [1.0, 3.0].
# Printed, so a red run shows the limits it used.
BASELINE_MS=999999
for _i in 1 2 3; do
  _s="$(date +%s%N)"; jq -n 'reduce range(0;1000000) as $i (0; .+$i)' >/dev/null 2>&1; _m=$(( ($(date +%s%N) - _s) / 1000000 ))
  (( _m < BASELINE_MS )) && BASELINE_MS="$_m"
done
SCALE_PCT=$(( BASELINE_MS * 100 / 150 )); (( SCALE_PCT < 100 )) && SCALE_PCT=100; (( SCALE_PCT > 300 )) && SCALE_PCT=300
lim() { echo $(( $1 * SCALE_PCT / 100 )); }
echo "test baseline: jq workload ${BASELINE_MS} ms (reference 150 ms), scale ${SCALE_PCT}% (range 100..300); limits used: big file $(lim 2000) ms, poll signal $(lim 1500) ms, poll bound [1400, $(lim 2300)) ms, codex margin limits x ${SCALE_PCT}%, spawn signal $(lim 800) ms" >&2

switch_on() { printf 'MEMOIRE_SUBAGENT_CONTEXT=on\n' > "$MEMOIRE_HOME/capture-tick.env"; }
loglines() { [[ -f "$LOGF" ]] && wc -l < "$LOGF" || echo 0; }
lastlog() { tail -n 1 "$LOGF" 2>/dev/null; }

# ---- fixture builders ---------------------------------------------------------
iso() { printf '2026-09-30T10:%02d:%02d.000Z' $(( $1 / 60 )) $(( $1 % 60 )); }
cl_user()   { jq -nc --arg t "$1" --arg ts "$(iso "$2")" --arg u "u$2" '{type:"user",uuid:$u,isSidechain:false,timestamp:$ts,message:{role:"user",content:$t}}'; }
cl_text()   { jq -nc --arg t "$1" --arg ts "$(iso "$2")" --arg u "a$2" '{type:"assistant",uuid:$u,isSidechain:false,timestamp:$ts,message:{role:"assistant",content:[{type:"text",text:$t}]}}'; }
cl_use()    { jq -nc --arg id "$1" --arg n "$2" --argjson in "$3" --arg ts "$(iso "$4")" --arg u "a$4" '{type:"assistant",uuid:$u,isSidechain:false,timestamp:$ts,message:{role:"assistant",content:[{type:"tool_use",id:$id,name:$n,input:$in}]}}'; }
cl_result() { jq -nc --arg id "$1" --arg t "$2" --argjson e "${4:-false}" --arg ts "$(iso "$3")" --arg u "r$3" '{type:"user",uuid:$u,isSidechain:false,timestamp:$ts,message:{role:"user",content:[{type:"tool_result",tool_use_id:$id,content:$t,is_error:$e}]}}'; }

payload() { # payload <transcript> [cwd] [turn_id]
  jq -nc --arg t "$1" --arg c "${2:-$PROJ}" --arg turn "${3:-}" --arg sid "${SID:-sess-1}" \
    '{session_id:$sid,transcript_path:$t,cwd:$c,agent_id:"agent-1",agent_type:"general-purpose",turn_id:$turn,hook_event_name:"SubagentStart"}'
}
ctx() { jq -r '.hookSpecificOutput.additionalContext // empty' 2>/dev/null; }
run_claude() { payload "$@" | ( cd "${RUN_CWD:-$PROJ}" && "$CLAUDE" ); }
run_codex() { payload "$@" | ( cd "${RUN_CWD:-$PROJ}" && "$CODEX" ); }

# ---- e2e (E1.4): deterministic ---------------------------------------------------
switch_on
T1="$SCRATCH/t1.jsonl"
{
  cl_user "Run the marker command then delegate to a sub-agent." 0
  cl_use tb1 Bash '{"command":"echo MARKER-7Q3X","description":"marker"}' 2
  cl_result tb1 "MARKER-7Q3X" 3
  cl_text "Delegating now." 4
  cl_use ta1 Agent '{"description":"probe","prompt":"what did the parent just do"}' 5
} > "$T1"
before="$(loglines)"
out="$(run_claude "$T1")"; rc=$?
blk="$(ctx <<<"$out")"
ok "e2e: exit 0" test "$rc" -eq 0
ok "e2e: anchor prompt present" has "$blk" "> [prompt] Run the marker command then delegate to a sub-agent."
ok "e2e: Bash call and its RESULT are in the block" has "$blk" "tool Bash command=echo MARKER-7Q3X description=marker → ok MARKER-7Q3X"
ok "e2e: Agent call shows as running" has "$blk" "tool Agent description=probe prompt=what did the parent just do → running"
ok "e2e: local HH:MM:SS timestamps" has "$blk" "- 10:00:02 tool Bash"
ok "e2e: pointer is the last line" test "$(tail -n 1 <<<"$blk")" = "Parent session sess-1 (project proj). This block is the parent's recent activity only. Older context: search Memoire (memoire_search) under project proj, or read $T1."
ok "e2e: block within the 8000 cap" test "$(chars "$blk")" -le 8000
ok "e2e: one log line" test "$(( $(loglines) - before ))" -eq 1
ok "e2e: log carries ids and sizes only" bash -c "tail -n 1 '$LOGF' | grep -q 'status=injected session=sess-1 agent=agent-1 type=general-purpose entries=4 chars=' && ! tail -n 1 '$LOGF' | grep -q MARKER"
ok "e2e: emission shape" test "$(jq -r '.hookSpecificOutput.hookEventName' <<<"$out")" = "SubagentStart"

# ---- switch matrix -----------------------------------------------------------------
sw() { # sw <file content or "-" for no file> <expect inject|none>
  rm -f "$MEMOIRE_HOME/capture-tick.env"
  [[ "$1" == "-" ]] || printf '%b' "$1" > "$MEMOIRE_HOME/capture-tick.env"
  local o; o="$(run_claude "$T1")"
  if [[ "$2" == inject ]]; then [[ -n "$(ctx <<<"$o")" ]] || fail "switch: '$1' should inject"
  else [[ -z "$o" ]] || fail "switch: '$1' should NOT inject"; fi
}
sw "-" none
sw "MEMOIRE_SUBAGENT_CONTEXT=off\n" none
sw "MEMOIRE_SUBAGENT_CONTEXT=\n" none
sw "MEMOIRE_SUBAGENT_CONTEXT=yes\n" none
sw "OTHER=on\n" none
sw "#MEMOIRE_SUBAGENT_CONTEXT=on\n" none
sw "XMEMOIRE_SUBAGENT_CONTEXT=on\n" none
sw "MEMOIRE_SUBAGENT_CONTEXT=on\n" inject
sw "MEMOIRE_SUBAGENT_CONTEXT=1\n" inject
sw "MEMOIRE_SUBAGENT_CONTEXT=true\n" inject
sw "MEMOIRE_SUBAGENT_CONTEXT=ON\n" inject
sw "MEMOIRE_SUBAGENT_CONTEXT=\"on\"\n" inject
sw "export MEMOIRE_SUBAGENT_CONTEXT='on'  # opted in\n" inject
sw "MEMOIRE_SUBAGENT_CONTEXT=on\nMEMOIRE_SUBAGENT_CONTEXT=off\n" none
sw "MEMOIRE_SUBAGENT_CONTEXT=off\nMEMOIRE_SUBAGENT_CONTEXT=on\n" inject
sw "MEMOIRE_CAPTURE_CODEX=on\nMEMOIRE_SUBAGENT_CONTEXT=on\r\n" inject
# never sourced
rm -f "$SCRATCH/sourced"
printf 'MEMOIRE_SUBAGENT_CONTEXT=on\nEVIL=$(touch %s/sourced)\n`touch %s/sourced`\n' "$SCRATCH" "$SCRATCH" > "$MEMOIRE_HOME/capture-tick.env"
run_claude "$T1" >/dev/null
ok "switch: the env file is never sourced" test ! -e "$SCRATCH/sourced"
# disabled leaves a log line and no output
sw "-" none
ok "switch: disabled is logged" has "$(lastlog)" "status=disabled"
# the attribution fork guard
switch_on
out="$(MEMOIRE_ATTRIB_FORK=1 run_claude "$T1")"
ok "switch: MEMOIRE_ATTRIB_FORK stays silent" test -z "$out"

# ---- exclusions (B7) ---------------------------------------------------------------
switch_on
T2="$SCRATCH/t2.jsonl"
{
  cl_user "REAL-PROMPT-OPENING-THE-TURN" 0
  jq -nc '{type:"user",uuid:"m1",isSidechain:false,isMeta:true,timestamp:"2026-09-30T10:00:01.000Z",message:{role:"user",content:"META-LINE-TEXT"}}'
  jq -nc '{type:"user",uuid:"m2",isSidechain:false,isCompactSummary:true,timestamp:"2026-09-30T10:00:01.500Z",message:{role:"user",content:"COMPACT-SUMMARY-TEXT"}}'
  cl_user "[Request interrupted by user]" 2
  cl_user "<system-reminder>SYSREMINDER-TEXT</system-reminder>" 3
  cl_user "<task-notification>TASKNOTIF-TEXT</task-notification>" 4
  cl_user "Another Claude session sent a message:
<agent-message>AGENTMSG-TEXT</agent-message>" 5
  cl_user "# [memoire] recent context — SESSIONSTART-MEMOIRE-BLOCK" 6
  jq -nc '{type:"attachment",uuid:"h1",isSidechain:false,timestamp:"2026-09-30T10:00:07.000Z",attachment:{type:"hook_additional_context",content:"HOOKCTX-TEXT"}}'
  jq -nc '{type:"user",uuid:"s1",isSidechain:true,timestamp:"2026-09-30T10:00:08.000Z",message:{role:"user",content:"SIDECHAIN-PROMPT"}}'
  jq -nc '{type:"assistant",uuid:"s2",isSidechain:true,timestamp:"2026-09-30T10:00:09.000Z",message:{role:"assistant",content:[{type:"text",text:"SIDECHAIN-TEXT"}]}}'
  jq -nc '{type:"assistant",uuid:"sy",isSidechain:false,timestamp:"2026-09-30T10:00:10.000Z",message:{role:"assistant",model:"<synthetic>",content:[{type:"text",text:"SYNTHETIC-ASSISTANT"}]}}'
  jq -nc '{type:"assistant",uuid:"th",isSidechain:false,timestamp:"2026-09-30T10:00:11.000Z",message:{role:"assistant",content:[{type:"thinking",thinking:"THINKING-TEXT"}]}}'
  jq -nc '{type:"user",uuid:"mx",isSidechain:false,timestamp:"2026-09-30T10:00:12.000Z",message:{role:"user",content:[{type:"text",text:"<system-reminder>MIXED-REMINDER</system-reminder>"},{type:"text",text:"MIXED-REAL-TEXT"}]}}'
  cl_text "ASSISTANT-KEPT" 13
  cl_use tk1 Bash '{"command":"ls"}' 14
  cl_result tk1 "RESULT-KEPT" 15
} > "$T2"
blk="$(run_claude "$T2" | ctx)"
for bad in META-LINE-TEXT COMPACT-SUMMARY-TEXT "Request interrupted" SYSREMINDER-TEXT TASKNOTIF-TEXT AGENTMSG-TEXT SESSIONSTART-MEMOIRE-BLOCK HOOKCTX-TEXT SIDECHAIN-PROMPT SIDECHAIN-TEXT SYNTHETIC-ASSISTANT THINKING-TEXT MIXED-REMINDER; do
  ok "exclude: $bad must not appear" lacks "$blk" "$bad"
done
ok "exclude: the mixed message keeps its real text as a prompt entry" has "$blk" "> [prompt] MIXED-REAL-TEXT"
ok "exclude: the real anchor is the opening prompt or later real prompt" bash -c "grep -q 'REAL-PROMPT-OPENING-THE-TURN\|MIXED-REAL-TEXT' <<<\"\$1\"" _ "$blk"
ok "exclude: assistant text and tool result kept" bash -c "grep -q 'ASSISTANT-KEPT' <<<\"\$1\" && grep -q 'RESULT-KEPT' <<<\"\$1\"" _ "$blk"
# a prompt that mentions tool_result in its text is still a prompt (escaped quotes never match the prefilter)
T2b="$SCRATCH/t2b.jsonl"
{ cl_user 'why does {"type":"tool_result"} appear in the docs?' 0; cl_text "answer" 1; } > "$T2b"
blk="$(run_claude "$T2b" | ctx)"
ok "exclude: a prompt quoting tool_result stays the anchor" has "$blk" '> [prompt] why does {"type":"tool_result"} appear in the docs?'

# ---- selection rule (B2) -------------------------------------------------------------
# omission line + caps + entry count
T3="$SCRATCH/t3.jsonl"
{
  cl_user "TURN-OPENING-PROMPT" 0
  jq -nc --argjson n 200 '
    range($n) as $i
    | {type:"assistant",uuid:("a\($i)"),isSidechain:false,timestamp:"2026-09-30T10:01:00.000Z",message:{role:"assistant",content:[{type:"text",text:("entry \($i) " + ("w" * 100))}]}}'
} > "$T3"
blk="$(run_claude "$T3" | ctx)"
n_entries="$(grep -c '^- ' <<<"$blk")"
ok "select: at most 60 entries" test "$n_entries" -le 60
ok "select: fills newest first (newest present, oldest absent)" bash -c "grep -q 'entry 199 ' <<<\"\$1\" && ! grep -q 'entry 0 ' <<<\"\$1\"" _ "$blk"
ok "select: omission line present" bash -c "grep -Eq '^\(\+[0-9]+ earlier entries omitted\)\$' <<<\"\$1\"" _ "$blk"
ok "select: chronological (entry 150 before entry 151)" bash -c "test \$(grep -n 'entry 150 ' <<<\"\$1\" | cut -d: -f1) -lt \$(grep -n 'entry 151 ' <<<\"\$1\" | cut -d: -f1)" _ "$blk"
ok "select: anchor first after the header, pointer last" bash -c "test \"\$(sed -n 2p <<<\"\$1\")\" = '> [prompt] TURN-OPENING-PROMPT' && tail -n 1 <<<\"\$1\" | grep -q '^Parent session sess-1'" _ "$blk"
ok "select: within the cap" test "$(chars "$blk")" -le 8000
blk="$(MEMOIRE_SUBAGENT_MAX_CHARS=2000 run_claude "$T3" | ctx)"
ok "select: MEMOIRE_SUBAGENT_MAX_CHARS is honoured" test "$(chars "$blk")" -le 2000
blk="$(MEMOIRE_SUBAGENT_MAX_ENTRIES=5 run_claude "$T3" | ctx)"
ok "select: MEMOIRE_SUBAGENT_MAX_ENTRIES is honoured" test "$(grep -c '^- ' <<<"$blk")" -le 5
# per-entry caps: prompt 1500, text 1200, input 200, result 220
T3c="$SCRATCH/t3c.jsonl"
{
  cl_user "$(head -c 3000 /dev/zero | tr '\0' 'P')" 0
  cl_text "$(head -c 3000 /dev/zero | tr '\0' 'T')" 1
  cl_use tc1 Bash "$(jq -nc --arg c "$(head -c 400 /dev/zero | tr '\0' 'I')" '{command:$c}')" 2
  cl_result tc1 "$(head -c 200 /dev/zero | tr '\0' 'H')$(head -c 500 /dev/zero | tr '\0' 'M')$(head -c 100 /dev/zero | tr '\0' 'E')" 3
} > "$T3c"
blk="$(run_claude "$T3c" | ctx)"
ok "caps: prompt <= 1500 chars + marker" test "$(grep '^> \[prompt\]' <<<"$blk" | tr -d '\n' | jq -Rs length)" -le 1520
ok "caps: text <= 1200 chars" test "$(grep -- '- 10:00:01 text: ' <<<"$blk" | sed 's/^- 10:00:01 text: //' | tr -d '\n' | jq -Rs length)" -le 1200
ok "caps: tool input <= 200 chars" bash -c "test \$(grep -- 'tool Bash ' <<<\"\$1\" | sed 's/.*tool Bash command=//; s/ → .*//' | tr -d '\n' | jq -Rs length) -le 200" _ "$blk"
res="$(grep -- 'tool Bash ' <<<"$blk" | sed 's/.* → ok //')"
ok "caps: result excerpt is head 140 + … + tail, <= 220" test "$(chars "$res")" -le 220
ok "caps: result excerpt keeps head and tail" bash -c "[[ \$1 == HHHH* && \$1 == *EEEE && \$1 == *…* ]]" _ "$res"

# long turn: the anchor is outside the 512 KiB fill window
T4="$SCRATCH/t4.jsonl"
{
  cl_user "EARLIER-TURN-PROMPT" 0
  cl_text "EARLIER-TURN-ANSWER" 1
  cl_user "LONG-TURN-OPENING-PROMPT" 2
  jq -nc --argjson n 400 '
    range($n) as $i
    | ({type:"assistant",uuid:("ta\($i)"),isSidechain:false,timestamp:"2026-09-30T10:02:00.000Z",message:{role:"assistant",content:[{type:"tool_use",id:("tl\($i)"),name:"Bash",input:{command:"work \($i)"}}]}},
       {type:"user",uuid:("tr\($i)"),isSidechain:false,timestamp:"2026-09-30T10:02:01.000Z",message:{role:"user",content:[{type:"tool_result",tool_use_id:("tl\($i)"),content:("out\($i) " + ("z" * 2000)),is_error:false}]}})'
} > "$T4"
ok "long turn: the fixture is larger than the fill window" test "$(stat -c %s "$T4")" -gt 524288
blk="$(run_claude "$T4" | ctx)"
ok "long turn: anchor present and whole" has "$blk" "> [prompt] LONG-TURN-OPENING-PROMPT"
ok "long turn: newest tool call kept, after the anchor" bash -c "test \$(grep -n 'work 399' <<<\"\$1\" | cut -d: -f1) -gt \$(grep -n 'LONG-TURN-OPENING-PROMPT' <<<\"\$1\" | cut -d: -f1)" _ "$blk"
ok "long turn: no pre-anchor entry, no boundary line (the anchor is older than the window)" bash -c "! grep -q 'EARLIER-TURN' <<<\"\$1\" && ! grep -q -- '--- current turn ---' <<<\"\$1\"" _ "$blk"
ok "long turn: omission line says older entries were not read" has "$blk" "or more earlier entries omitted)"
ok "long turn: within the cap" test "$(chars "$blk")" -le 8000

# earlier-turn entries fill the remaining budget, rendered ABOVE the boundary line
T5="$SCRATCH/t5.jsonl"
{
  cl_user "EARLIER-PROMPT" 0
  cl_text "EARLIER-ANSWER" 1
  cl_user "CURRENT-PROMPT" 2
  cl_use tp1 Bash '{"command":"current-work"}' 3
  cl_result tp1 "CURRENT-RESULT" 4
  cl_text "CURRENT-TEXT" 5
} > "$T5"
blk="$(run_claude "$T5" | ctx)"
ok "order: earlier prompt, earlier answer, boundary, anchor, current entries, pointer" bash -c "
  a=\$(grep -nF 'EARLIER-PROMPT' <<<\"\$1\" | head -1 | cut -d: -f1); b=\$(grep -nF 'EARLIER-ANSWER' <<<\"\$1\" | head -1 | cut -d: -f1)
  c=\$(grep -nF -- '--- current turn ---' <<<\"\$1\" | head -1 | cut -d: -f1); d=\$(grep -nF 'CURRENT-PROMPT' <<<\"\$1\" | head -1 | cut -d: -f1)
  e=\$(grep -nF 'current-work' <<<\"\$1\" | head -1 | cut -d: -f1); f=\$(grep -nF 'CURRENT-TEXT' <<<\"\$1\" | head -1 | cut -d: -f1)
  g=\$(grep -nF 'Parent session' <<<\"\$1\" | head -1 | cut -d: -f1)
  [[ -n \$a && \$a -lt \$b && \$b -lt \$c && \$c -lt \$d && \$d -lt \$e && \$e -lt \$f && \$f -lt \$g ]]" _ "$blk"
ok "order: the boundary line is exact and appears once" test "$(grep -cx -- '--- current turn ---' <<<"$blk")" -eq 1
ok "order: nothing omitted, no omission line" bash -c "! grep -q 'earlier entries omitted' <<<\"\$1\"" _ "$blk"
# pre-anchor entries only above the boundary: with a tight cap they are dropped, never moved below
T5b="$SCRATCH/t5b.jsonl"
{
  cl_user "EARLIER-PROMPT" 0
  cl_text "EARLIER-ANSWER $(head -c 900 /dev/zero | tr '\0' 'e')" 1
  cl_user "CURRENT-PROMPT" 2
  cl_text "CURRENT-TEXT" 3
} > "$T5b"
blk="$(MEMOIRE_SUBAGENT_MAX_CHARS=1000 run_claude "$T5b" | ctx)"
ok "order: under a tight cap current entries win over earlier turns" bash -c "grep -q 'CURRENT-TEXT' <<<\"\$1\" && ! grep -q 'EARLIER-ANSWER' <<<\"\$1\" && ! grep -qx -- '--- current turn ---' <<<\"\$1\"" _ "$blk"
ok "order: the dropped entry is counted in the omission line" has "$blk" "(+2 earlier entries omitted)"

# anchor beyond 16 MiB: the note replaces it
T6="$SCRATCH/t6.jsonl"
{
  cl_user "ANCIENT-PROMPT" 0
  jq -nc --argjson n 12 '
    range($n) as $i | {type:"assistant",uuid:("b\($i)"),isSidechain:false,timestamp:"2026-09-30T10:03:00.000Z",message:{role:"assistant",content:[{type:"text",text:("big \($i) " + ("q" * 1500000))}]}}'
  cl_text "TAIL-TEXT" 30
} > "$T6"
ok "no-anchor: the fixture is larger than 16 MiB" test "$(stat -c %s "$T6")" -gt 16777216
blk="$(run_claude "$T6" | ctx)"
ok "no-anchor: the note stands in for the anchor" has "$blk" "(no operator prompt in the recent window)"
ok "no-anchor: newest text kept" has "$blk" "TAIL-TEXT"
ok "no-anchor: the ancient prompt is not resurrected" lacks "$blk" "ANCIENT-PROMPT"

# tens of megabytes stay inside the injector's time bound (registered timeout 5 s)
T7="$SCRATCH/t7.jsonl"
jq -nc --argjson n 700 'range($n) as $i | {type:"assistant",uuid:("c\($i)"),isSidechain:false,timestamp:"2026-09-30T10:04:00.000Z",message:{role:"assistant",content:[{type:"text",text:("chunk \($i) " + ("k" * 1400))}]}}' > "$SCRATCH/chunk.jsonl"
: > "$T7"
for _ in $(seq 1 60); do cat "$SCRATCH/chunk.jsonl" >> "$T7"; done
{ cl_user "BIG-FILE-PROMPT" 40; cl_use tz1 Agent '{"prompt":"x"}' 41; } >> "$T7"
ok "big file: > 50 MB" test "$(stat -c %s "$T7")" -gt 50000000
blk="$(run_claude "$T7" | ctx)"
ok "big file: block built" has "$blk" "> [prompt] BIG-FILE-PROMPT"
bms=999999; for _ in 1 2 3; do s0="$(date +%s%N)"; run_claude "$T7" >/dev/null; ms=$(( ($(date +%s%N) - s0) / 1000000 )); (( ms < bms )) && bms="$ms"; done
ok "big file: best of 3 = ${bms} ms is inside the bound (registered timeout 5 s, limit $(lim 2000) ms)" test "$bms" -lt "$(lim 2000)"

# ---- A1: the block fits every accepted MAX_CHARS (round 3) --------------------------------------------------------
# The fixed part (header, anchor, pointer) used to be unconditional: a 1500-character anchor with
# MAX_CHARS=600 gave an 1800-character block. Now the anchor is cut to what the budget allows (never
# below 120 characters); when even that does not fit, the block is the header and the pointer only.
TA="$SCRATCH/a1.jsonl"
{
  cl_user "$(printf 'ANCHOR-A1 %s' "$(printf 'long anchor words %.0s' $(seq 1 200))")" 0
  for i in $(seq 1 30); do cl_text "entry $i $(printf 'w%.0s' $(seq 1 150))" $(( i + 1 )); done
  cl_use ta9 Agent '{"prompt":"x"}' 40
} > "$TA"
for mc in 600 700 800 1000 1500 3000 8000; do
  blk="$(MEMOIRE_SUBAGENT_MAX_CHARS=$mc run_claude "$TA" | ctx)"
  ok "A1: MAX_CHARS=$mc: the block fits ($(chars "$blk") chars)" test "$(chars "$blk")" -le "$mc"
  ok "A1: MAX_CHARS=$mc: header first, pointer last" bash -c "head -n 1 <<<\"\$1\" | grep -q '^# \[memoire\] parent context' && tail -n 1 <<<\"\$1\" | grep -q '^Parent session sess-1'" _ "$blk"
done
blk="$(MEMOIRE_SUBAGENT_MAX_CHARS=600 run_claude "$TA" | ctx)"
ok "A1: MAX_CHARS=600: the anchor is kept, cut with an ellipsis, at least 120 characters" bash -c "l=\$(grep '^> \[prompt\] ANCHOR-A1' <<<\"\$1\"); [[ -n \$l && \$l == *… && \${#l} -ge 120 ]]" _ "$blk"
DEEP="$SCRATCH/$(printf 'deep-directory-name-%.0s' $(seq 1 8))"; mkdir -p "$DEEP"; cp "$TA" "$DEEP/a1.jsonl"
blk="$(MEMOIRE_SUBAGENT_MAX_CHARS=600 run_claude "$DEEP/a1.jsonl" | ctx)"
ok "A1: a pointer so long that no anchor of 120 characters fits: header and pointer only" bash -c "[[ \$(wc -l <<<\"\$1\") -eq 2 ]] && head -n 1 <<<\"\$1\" | grep -q '^# \[memoire\]' && tail -n 1 <<<\"\$1\" | grep -q '^Parent session'" _ "$blk"
ok "A1: ... and that block still fits" test "$(chars "$blk")" -le 600
ok "A1: default bounds: a short anchor and the entries are untouched" bash -c "grep -q 'ANCHOR-A1' <<<\"\$1\" && grep -q 'entry 30 ' <<<\"\$1\"" _ "$(run_claude "$TA" | ctx)"

# ---- A2: hook input fields in the log are stripped of control characters and bounded ----------------------------------------
before="$(loglines)"
LONG="$(printf 'L%.0s' $(seq 1 400))"
jq -nc --arg t "$T1" --arg c "$PROJ" --arg sid $'sess\nFORGED status=injected' --arg ag $'agent\x1b[31m\u0085\u2028\x01x' --arg ty "type$LONG" \
  '{session_id:$sid,transcript_path:$t,cwd:$c,agent_id:$ag,agent_type:$ty,hook_event_name:"SubagentStart"}' > "$SCRATCH/a2.json"
out="$( ( cd "$PROJ" && "$CLAUDE" < "$SCRATCH/a2.json" ) )"
ok "A2: exactly one new log line, whatever the input carries" test "$(( $(loglines) - before ))" -eq 1
al="$(lastlog)"
ok "A2: no control character survives in the log line" bash -c "! LC_ALL=C grep -qP '[\\x00-\\x08\\x0b-\\x1f\\x7f]|\\xc2[\\x80-\\x9f]|\\xe2\\x80[\\xa8\\xa9]' <<<\"\$1\"" _ "$al"
fld() { sed -n "s/.* $1=\\(.*\\) $2=.*/\\1/p" <<<"$al"; }
ok "A2: session is bounded to 128 characters ($(chars "$(fld session agent)"))" test "$(chars "$(fld session agent)")" -le 128
ok "A2: agent is bounded to 128 characters" test "$(chars "$(fld agent type)")" -le 128
ok "A2: type is bounded to 128 characters ($(chars "$(fld type entries)"))" test "$(chars "$(fld type entries)")" -le 128
ok "A2: the fields keep their printable characters, the hook still injects" bash -c "grep -q 'status=injected session=sessFORGED status=injected agent=agent\[31mx type=typeLLL' <<<\"\$1\"" _ "$al"
ok "A2: the pointer line of the block is clean too" bash -c "! LC_ALL=C grep -qP '[\\x00-\\x09\\x0b-\\x1f\\x7f]|\\xc2[\\x80-\\x9f]|\\xe2\\x80[\\xa8\\xa9]' <<<\"\$(jq -r '.hookSpecificOutput.additionalContext' <<<\"\$1\" | tail -n 1)\"" _ "$out"
# controls without a newline (the newline case above already breaks the field split)
jq -nc --arg t "$T1" --arg c "$PROJ" --arg sid $'s2\x1b[2Jz\u0085' --arg ag $'a\x01\x7fb' --arg ty $'t\u2029c' \
  '{session_id:$sid,transcript_path:$t,cwd:$c,agent_id:$ag,agent_type:$ty,hook_event_name:"SubagentStart"}' > "$SCRATCH/a2b.json"
( cd "$PROJ" && "$CLAUDE" < "$SCRATCH/a2b.json" ) >/dev/null
al="$(lastlog)"
ok "A2: ESC, DEL, C0, U+0085 and U+2029 are stripped from session, agent and type" bash -c "! LC_ALL=C grep -qP '[\\x00-\\x08\\x0b-\\x1f\\x7f]|\\xc2[\\x80-\\x9f]|\\xe2\\x80[\\xa8\\xa9]' <<<\"\$1\" && grep -q 'session=s2\[2Jz agent=ab type=tc ' <<<\"\$1\"" _ "$al"

# ---- poll (B4) -----------------------------------------------------------------------
T8="$SCRATCH/t8.jsonl"
{ cl_user "POLL-PROMPT" 0; cl_text "before" 1; } > "$T8"
cat > "$SCRATCH/late.jsonl" <<EOF
$(cl_use tq1 Bash '{"command":"late-bash"}' 2)
$(cl_result tq1 "LATE-RESULT" 3)
$(cl_use tq2 Agent '{"prompt":"spawn"}' 4)
EOF
bash -c 'sleep 0.3; cat "$1" >> "$2"' _ "$SCRATCH/late.jsonl" "$T8" &
wpid=$!
out="$(run_claude "$T8")"; wait "$wpid" 2>/dev/null
blk="$(ctx <<<"$out")"
ok "poll: the entries that landed after the hook started are in the block" has "$blk" "tool Bash command=late-bash → ok LATE-RESULT"
ok "poll: ended on the unmatched Agent call" has "$(lastlog)" "poll_end=signal"
ok "poll: the bound is 1.5 s" bash -c "ms=\$(tail -n 1 '$LOGF' | sed 's/.*elapsed_ms=//'); [[ \$ms -lt $(lim 1500) ]]"
# growing file, no delegation call: the poll ends on its 1.5 s bound (not 3 s)
T8g="$SCRATCH/t8g.jsonl"
{ cl_user "GROW-PROMPT" 0; cl_text "before" 1; } > "$T8g"
cat > "$SCRATCH/grow-claude.sh" <<'GEOF'
#!/usr/bin/env bash
while :; do printf '{"type":"system","uuid":"g","subtype":"x","timestamp":"2026-09-30T10:00:05.000Z"}\n' >> "$1"; sleep 0.1; done
GEOF
chmod +x "$SCRATCH/grow-claude.sh"
( exec "$SCRATCH/grow-claude.sh" "$T8g" ) &
gpid=$!
run_claude "$T8g" >/dev/null
kill "$gpid" 2>/dev/null; wait "$gpid" 2>/dev/null
ok "poll: a growing file with no delegation call ends on the bound" has "$(lastlog)" "poll_end=bound"
ok "poll: the bound is 1.5 s, not more" bash -c "ms=\$(tail -n 1 '$LOGF' | sed 's/.*elapsed_ms=//'); [[ \$ms -ge 1400 && \$ms -lt $(lim 2300) ]]"
# an EARLIER Agent call with its result does not stop the poll: fallback on stable size
T8b="$SCRATCH/t8b.jsonl"
{ cl_user "P" 0; cl_use tq3 Agent '{"prompt":"old"}' 1; cl_result tq3 "launched" 2; cl_text "x" 3; } > "$T8b"
s0="$(date +%s%N)"; run_claude "$T8b" >/dev/null; ms=$(( ($(date +%s%N) - s0) / 1000000 ))
ok "poll: a matched earlier Agent call is not a signal (stable fallback, >= 500 ms)" bash -c "grep -q 'poll_end=stable' <<<\"\$1\" && [[ $ms -ge 500 ]]" _ "$(lastlog)"

# ---- fail-open (B9c) -------------------------------------------------------------------
one_log_line() { # one_log_line <cmd...>; expects exit 0, no stdout, exactly one new log line
  local b o rc; b="$(loglines)"
  o="$("$@" 2>/dev/null)"; rc=$?
  [[ "$rc" -eq 0 && -z "$o" && "$(( $(loglines) - b ))" -eq 1 ]]
}
NOJQ="$SCRATCH/nojq"; mkdir -p "$NOJQ"
for b in cat date mkdir stat tail head grep sed tr wc dirname basename rm mktemp sleep cut env bash; do
  p="$(type -P "$b")" && ln -sf "$p" "$NOJQ/$b"
done
run_nojq() { payload "$T1" | PATH="$NOJQ" "$CLAUDE"; }
run_nojq_codex() { payload "$T1" | PATH="$NOJQ" "$CODEX_HOOK"; }
ok "failopen: jq missing (claude): exit 0, no output, one log line" one_log_line run_nojq
ok "failopen: jq missing is named in the log" has "$(lastlog)" "status=error=jq-missing"
ok "failopen: jq missing (codex): exit 0, no output, one log line" one_log_line run_nojq_codex
run_unreadable() { payload "$SCRATCH/does-not-exist.jsonl" | "$CLAUDE"; }
ok "failopen: transcript unreadable: exit 0, no output, one log line" one_log_line run_unreadable
ok "failopen: unreadable is named in the log" has "$(lastlog)" "status=error=transcript-unreadable"
T9="$SCRATCH/t9.jsonl"
{ cl_user "P" 0; echo '{"type":"assistant", this is not json'; cl_text "x" 2; } > "$T9"
# Round 2 (coordinator ruling, changes B9c): a malformed line is SKIPPED and counted, the rest is injected.
before="$(loglines)"
out="$(payload "$T9" | "$CLAUDE")"; blk="$(ctx <<<"$out")"
ok "failopen: a malformed line is skipped: the rest is injected, exactly one log line" bash -c "grep -q '> \[prompt\] P' <<<\"\$1\" && grep -q 'text: x' <<<\"\$1\" && [[ \$(( \$(wc -l < '$LOGF') - $before )) -eq 1 ]]" _ "$blk"
ok "failopen: the skipped line is counted in the log" has "$(lastlog)" "bad_lines=1"
run_garbage_stdin() { printf 'not json at all' | "$CLAUDE"; }
ok "failopen: garbage on stdin: exit 0, no output, one log line" one_log_line run_garbage_stdin
run_garbage_codex() { printf 'not json at all' | "$CODEX"; }
ok "failopen: garbage on stdin (codex): exit 0, no output, one log line" one_log_line run_garbage_codex
# an unterminated last line (a write in flight) is ignored, not an error
T9b="$SCRATCH/t9b.jsonl"
{ cl_user "INFLIGHT-PROMPT" 0; cl_text "kept" 1; printf '{"type":"assistant","uuid":"zz","message":{"content":[{"type":"te'; } > "$T9b"
blk="$(run_claude "$T9b" | ctx)"
ok "failopen: an unterminated last line is dropped, the rest injected" bash -c "grep -q INFLIGHT-PROMPT <<<\"\$1\" && grep -q 'text: kept' <<<\"\$1\"" _ "$blk"

# ---- gates (B9d) -------------------------------------------------------------------------
PARENT_DIR="$SCRATCH/gate-parent"; CHILD_DIR="$SCRATCH/gate-child"; OK_DIR="$SCRATCH/gate-ok"
mkdir -p "$PARENT_DIR" "$CHILD_DIR" "$OK_DIR"
printf 'project: gp\ncapture: false\n' > "$PARENT_DIR/.memoire"
printf 'project: gc\ncapture: false\n' > "$CHILD_DIR/.memoire"
out="$(RUN_CWD="$OK_DIR" run_claude "$T1" "$PARENT_DIR")"
ok "gates: capture:false on the parent's directory: no injection" test -z "$out"
ok "gates: parent gate logged" has "$(lastlog)" "status=capture-off"
out="$(RUN_CWD="$CHILD_DIR" run_claude "$T1" "$OK_DIR")"
ok "gates: capture:false on the child's directory: no injection" test -z "$out"
ok "gates: child gate logged" has "$(lastlog)" "status=capture-off"
out="$(RUN_CWD="$OK_DIR" run_claude "$T1" "$OK_DIR")"
ok "gates: neither directory off: injection" test -n "$out"
out="$(MEMOIRE_CAPTURE=off RUN_CWD="$OK_DIR" run_claude "$T1" "$OK_DIR")"
ok "gates: MEMOIRE_CAPTURE=off (session kill switch) also stops it" test -z "$out"
out="$(RUN_CWD="$CHILD_DIR" run_codex "$T1" "$OK_DIR")"
ok "gates: the codex injector has the same gate (child's directory)" test -z "$out"
out="$(RUN_CWD="$OK_DIR" run_codex "$T1" "$PARENT_DIR")"
ok "gates: the codex injector's parent-directory gate" test -z "$out"
ok "gates: the codex parent gate is logged" has "$(lastlog)" "status=capture-off"

# ---- the log line is written BEFORE the block is emitted (B3) ------------------------------------------------
# A jq shim reports how many log lines exist at the moment the emit call (jq -cn) starts.
EMIT="$SCRATCH/emitshim"; mkdir -p "$EMIT"
REALJQ="$(command -v jq)"
cat > "$EMIT/jq" <<SHIMEOF
#!/usr/bin/env bash
if [[ "\$1" == "-cn" ]]; then wc -l < "$LOGF" > "$SCRATCH/lines-at-emit"; fi
exec "$REALJQ" "\$@"
SHIMEOF
chmod +x "$EMIT/jq"
switch_on
run_shim() { payload "$@" | ( cd "$PROJ" && PATH="$EMIT:$PATH" "$RUNNER" ); }
for RUNNER in "$CLAUDE" "$CODEX"; do
  rm -f "$SCRATCH/lines-at-emit"; before="$(loglines)"
  if [[ "$RUNNER" == "$CLAUDE" ]]; then src="$T1"; turn=""
  else
    src="$SCRATCH/rollout-emit.jsonl"; turn=te
    {
      echo '{"timestamp":"2026-09-30T10:00:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"te"}}'
      echo '{"timestamp":"2026-09-30T10:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"EMIT-PROMPT"}]},"metadata":{"client_authored":true}}'
      echo '{"timestamp":"2026-09-30T10:00:02.000Z","type":"response_item","payload":{"type":"function_call","name":"spawn_agent","arguments":"{}","call_id":"cs"}}'
    } > "$src"
  fi
  out="$(run_shim "$src" "$PROJ" "$turn")"
  ok "log-before-emit: $(basename "$RUNNER"): the block was emitted" test -n "$out"
  ok "log-before-emit: $(basename "$RUNNER"): the log line existed when the emit started (saw $(cat "$SCRATCH/lines-at-emit" 2>/dev/null) lines, before was $before)" test "$(cat "$SCRATCH/lines-at-emit" 2>/dev/null)" = "$(( before + 1 ))"
done

# ---- local time (B6): entries carry local HH:MM:SS, not UTC ------------------------------------------------------
# (TZ=UTC-2 is the POSIX spelling of UTC+2: no tzdata needed)
blk="$(TZ=UTC-2 run_claude "$T1" | ctx)"
ok "local time: claude entries at 10:00:02Z read 12:00:02 under UTC+2" has "$blk" "- 12:00:02 tool Bash"
ok "local time: and under TZ=UTC they read 10:00:02" has "$(TZ=UTC run_claude "$T1" | ctx)" "- 10:00:02 tool Bash"

# ---- codex (B9a): real 0.159.1 record shapes ---------------------------------------------
cx() { local extra="${4:-}"; [[ -n "$extra" ]] || extra='{}'; jq -nc --arg ts "$(iso "$1")" --arg top "$2" --argjson p "$3" --argjson extra "$extra" '{timestamp:$ts,type:$top,payload:$p} + $extra'; }
cx_user()  { local m="${3:-}"; [[ -n "$m" ]] || m='{"metadata":{"client_authored":true}}'; cx "$1" response_item "$(jq -nc --arg t "$2" '{type:"message",role:"user",content:[{type:"input_text",text:$t}]}')" "$m"; }
cx_asst()  { cx "$1" response_item "$(jq -nc --arg t "$2" '{type:"message",role:"assistant",content:[{type:"output_text",text:$t}],phase:"commentary"}')" "${3:-}"; }
cx_call()  { cx "$1" response_item "$(jq -nc --arg n "$2" --arg a "$3" --arg c "$4" '{type:"function_call",name:$n,arguments:$a,call_id:$c}')"; }
cx_out()   { cx "$1" response_item "$(jq -nc --arg c "$2" --arg o "$3" '{type:"function_call_output",call_id:$c,output:$o}')"; }
cx_ccall() { cx "$1" response_item "$(jq -nc --arg n "$2" --arg i "$3" --arg c "$4" '{type:"custom_tool_call",name:$n,input:$i,call_id:$c,status:"completed"}')"; }
cx_cout()  { cx "$1" response_item "$(jq -nc --arg c "$2" --arg o "$3" '{type:"custom_tool_call_output",call_id:$c,output:$o}')"; }
cx_start() { cx "$1" event_msg "$(jq -nc --arg t "$2" '{type:"task_started",turn_id:$t}')" "${3:-}"; }

R1="$SCRATCH/rollout1.jsonl"
{
  cx 0 session_meta '{"id":"thread-1","cwd":"/x","cli_version":"0.159.1","source":"cli"}'
  cx_start 1 t0
  cx_user 2 "EARLIER-CODEX-PROMPT"
  cx_asst 3 "EARLIER-CODEX-ANSWER"
  cx 4 event_msg '{"type":"task_complete","turn_id":"t0"}'
  cx_start 5 t1
  cx 6 response_item '{"type":"message","role":"developer","content":[{"type":"input_text","text":"DEVELOPER-INSTRUCTIONS"}]}'
  cx_user 7 "# AGENTS.md instructions for /x

<INSTRUCTIONS>AGENTSMD-BODY</INSTRUCTIONS>" '{"metadata":{"client_authored":false}}'
  cx_user 8 "<environment_context>
  <cwd>/x</cwd>ENVCTX-BODY
</environment_context>"
  cx_user 9 "CODEX-CURRENT-PROMPT with émoji 😀"
  cx 10 event_msg '{"type":"user_message","message":"LEGACY-USER-MESSAGE"}'
  cx 11 event_msg '{"type":"agent_reasoning","text":"REASONING-SCRATCH"}'
  cx 12 event_msg '{"type":"token_count","info":{"total_token_usage":{"input_tokens":1}}}'
  cx_call 13 exec_command '{"cmd":"ls -la","workdir":"/x"}' c1
  cx_out 14 c1 "Exit code: 0
Wall time: 0.1 seconds
Output:
LISTING-OK"
  cx_call 15 exec_command '{"cmd":"false"}' c2
  cx_out 16 c2 "Process exited with code 1
FAILED-OUT"
  cx_call 17 exec_command '{"cmd":"echo hi"}' c3
  cx_out 18 c3 "hi"
  cx_ccall 19 apply_patch "*** Begin Patch PATCH-BODY" c4
  cx_cout 20 c4 "Exit code: 0 PATCH-RESULT"
  cx 21 response_item '{"type":"agent_message","author":"/root","recipient":"/root/sub","content":[{"type":"text","text":"INTER-AGENT-ENVELOPE"}]}'
  cx_asst 22 "CODEX-ASSISTANT-TEXT"
  cx_call 23 spawn_agent '{"message":"go","fork_context":false}' c5
} > "$R1"
blk="$(run_codex "$R1" "$PROJ" t1 | ctx)"
ok "codex: anchor is the client-authored user message after the newest task_started" has "$blk" "> [prompt] CODEX-CURRENT-PROMPT with émoji 😀"
for bad in DEVELOPER-INSTRUCTIONS AGENTSMD-BODY ENVCTX-BODY LEGACY-USER-MESSAGE REASONING-SCRATCH INTER-AGENT-ENVELOPE; do
  ok "codex: $bad must not appear" lacks "$blk" "$bad"
done
ok "codex: function_call paired with its output, exit code 0 -> ok" has "$blk" "tool exec_command cmd=ls -la workdir=/x → ok Exit code: 0 Wall time: 0.1 seconds Output: LISTING-OK"
ok "codex: nonzero exit code -> error" has "$blk" "tool exec_command cmd=false → error Process exited with code 1 FAILED-OUT"
ok "codex: no exit code in the text -> unknown" has "$blk" "tool exec_command cmd=echo hi → unknown hi"
ok "codex: custom_tool_call included, paired by call_id" has "$blk" "tool apply_patch *** Begin Patch PATCH-BODY → ok Exit code: 0 PATCH-RESULT"
ok "codex: the spawn call without output is running" has "$blk" "tool spawn_agent message=go fork_context=false → running"
ok "codex: assistant text kept" has "$blk" "text: CODEX-ASSISTANT-TEXT"
ok "codex: the earlier turn sits above the boundary line" bash -c "
  a=\$(grep -nF 'EARLIER-CODEX-PROMPT' <<<\"\$1\" | cut -d: -f1); c=\$(grep -nxF -- '--- current turn ---' <<<\"\$1\" | cut -d: -f1); d=\$(grep -nF 'CODEX-CURRENT-PROMPT' <<<\"\$1\" | cut -d: -f1)
  [[ -n \$a && \$a -lt \$c && \$c -lt \$d ]]" _ "$blk"
ok "codex: the poll ended on the unmatched spawn call" has "$(lastlog)" "poll_end=signal"
ok "codex: log names the harness" has "$(lastlog)" "memoire-subagent-context-codex"
# without turn_id the newest task_started is used
blk2="$(run_codex "$R1" | ctx)"
ok "codex: no turn_id falls back to the newest task_started" has "$blk2" "> [prompt] CODEX-CURRENT-PROMPT"
# Child turn IDs cannot anchor the parent; select its newest task_started.
R1b="$SCRATCH/rollout1b.jsonl"
{ cat "$R1"; cx_start 30 t2; cx_user 31 "NESTED-INNER-PROMPT"; } > "$R1b"
blk="$(run_codex "$R1b" "$PROJ" t1 | ctx)"
# The latest parent prompt is the anchor even when the child turn_id matches an older turn.
turn_order() { # turn_order <block>: prints "<boundary> <current> <nested>" line numbers
  local c d e
  c="$(grep -nxF -- '--- current turn ---' <<<"$1" | cut -d: -f1 | head -n 1)"
  d="$(grep -nF 'CODEX-CURRENT-PROMPT' <<<"$1" | cut -d: -f1 | head -n 1)"
  e="$(grep -nF 'NESTED-INNER-PROMPT' <<<"$1" | cut -d: -f1 | head -n 1)"
  echo "${c:-0} ${d:-0} ${e:-0}"
}
read -r bc bd be <<<"$(turn_order "$blk")"
ok "codex: child turn_id does not select parent anchor: newest parent turn is below boundary" test "$bc" -gt 0 -a "$bd" -lt "$bc" -a "$bc" -lt "$be"
blk="$(run_codex "$R1b" "$PROJ" | ctx)"
read -r bc bd be <<<"$(turn_order "$blk")"
ok "codex: without turn_id the newest task_started wins: CODEX-CURRENT-PROMPT ($bd) sits above the boundary ($bc), the anchor is the later prompt ($be)" test "$bc" -gt 0 -a "$bd" -lt "$bc" -a "$bc" -lt "$be"
# inherited records of a forked sub-agent thread
R2="$SCRATCH/rollout2.jsonl"
{
  cx 0 session_meta '{"id":"child","cwd":"/x","subagent_history_start_ordinal":3}' '{"ordinal":0}'
  cx_user 1 "INHERITED-PROMPT" '{"ordinal":1}'
  cx_asst 2 "INHERITED-ANSWER" '{"ordinal":2}'
  cx_start 3 tc '{"ordinal":3}'
  cx_user 4 "OWN-CHILD-PROMPT" '{"ordinal":4}'
  cx_asst 5 "OWN-CHILD-ANSWER" '{"ordinal":5}'
} > "$R2"
blk="$(run_codex "$R2" "$PROJ" tc | ctx)"
ok "codex: inherited records (ordinal < subagent_history_start_ordinal) are excluded" bash -c "! grep -q INHERITED <<<\"\$1\" && grep -q OWN-CHILD-ANSWER <<<\"\$1\"" _ "$blk"

blk="$(TZ=UTC-2 run_codex "$R1" "$PROJ" t1 | ctx)"
ok "local time: codex entries at 10:00:13Z read 12:00:13 under UTC+2" has "$blk" "- 12:00:13 tool exec_command"

# ---- codex time against the timeout (B9e) ---------------------------------------------------
CODEX_TIMEOUT_MS=$(( $(sed -n 's/^# registered-timeout-s: *//p' "$CODEX_HOOK" | head -n 1) * 1000 )); MARGIN_MS=1500
CODEX_LIMIT_MS="$(lim $(( CODEX_TIMEOUT_MS - MARGIN_MS )))"
ok "time: the Codex script declares its registered timeout" test "$CODEX_TIMEOUT_MS" -ge 3000
elapsed_of_last() { tail -n 1 "$LOGF" | sed 's/.*elapsed_ms=//'; }
# worst quiet case: no spawn call, size stable -> stops after ~500 ms
R3="$SCRATCH/rollout3.jsonl"
{ cx_start 1 t1; cx_user 2 "QUIET-PROMPT"; cx_asst 3 "quiet"; } > "$R3"
# (wall time on a shared machine: the best of three runs is the measure)
best_of_3() { local best=999999 m i; for i in 1 2 3; do "$@" >/dev/null; m="$(elapsed_of_last)"; (( m < best )) && best="$m"; done; echo "$best"; }
best="$(best_of_3 run_codex "$R3" "$PROJ" t1)"
ok "time: quiet worst case stops on stable size (best of 3: $best ms)" bash -c "grep -q 'poll_end=stable' <<<\"\$1\" && [[ $best -le $CODEX_LIMIT_MS ]]" _ "$(lastlog)"
# worst busy case: the rollout keeps growing without a spawn call -> the 1 s bound
R4="$SCRATCH/rollout4.jsonl"
cp "$R3" "$R4"
cat > "$SCRATCH/grower.sh" <<'EOF'
#!/usr/bin/env bash
# appends one record every 100 ms until signalled
while :; do printf '{"timestamp":"2026-09-30T10:00:00.000Z","type":"event_msg","payload":{"type":"exec_command_output_delta","chunk":"x"}}\n' >> "$1"; sleep 0.1; done
EOF
chmod +x "$SCRATCH/grower.sh"
( exec "$SCRATCH/grower.sh" "$R4" ) &
gpid=$!
ms="$(best_of_3 run_codex "$R4" "$PROJ" t1)"
kill "$gpid" 2>/dev/null; wait "$gpid" 2>/dev/null
ok "time: busy worst case ends at the 1 s bound (best of 3: $ms ms) with 1.5 s of margin under the 3 s timeout" bash -c "grep -q 'poll_end=bound' <<<\"\$1\" && [[ $ms -le $CODEX_LIMIT_MS ]]" _ "$(lastlog)"
# signal case is fast
ms="$(best_of_3 run_codex "$R1" "$PROJ" t1)"
ok "time: an unmatched spawn call ends the poll at once (best of 3: $ms ms, limit $(lim 800) ms)" test "$ms" -lt "$(lim 800)"
# big rollout
R5="$SCRATCH/rollout5.jsonl"
jq -nc --argjson n 700 'range($n) as $i | {timestamp:"2026-09-30T10:04:00.000Z",type:"response_item",payload:{type:"message",role:"assistant",content:[{type:"output_text",text:("chunk \($i) " + ("k" * 1400))}]}}' > "$SCRATCH/cchunk.jsonl"
: > "$R5"; for _ in $(seq 1 60); do cat "$SCRATCH/cchunk.jsonl" >> "$R5"; done
{ cx_start 40 tb; cx_user 41 "BIG-CODEX-PROMPT"; cx_call 42 spawn_agent '{"message":"x"}' cb; } >> "$R5"
ms="$(best_of_3 run_codex "$R5" "$PROJ" tb)"
ok "time: a >50 MB rollout stays inside the bound (best of 3: $ms ms, limit $CODEX_LIMIT_MS ms)" test "$ms" -le "$CODEX_LIMIT_MS"
ok "time: elapsed is logged" has "$(lastlog)" "elapsed_ms="

# ---- parity: bash Claude, bash Codex and the plugin build the same block structure ---------------
# (the plugin's own suite runs through scripts/tests/subagent-context-plugin-test.sh, in the public core)
PAR="$SCRATCH/parity"; mkdir -p "$PAR"
if [[ -n "$NODE" && -z "$NODE_WHY" ]]; then
  "$NODE" "$TESTDIR/subagent-context-plugin-test.mjs" gen "$PAR" || fail "parity: fixture generation"
  switch_on
  # The pointer counts against the budget; the plugin's location is "OpenCode
  # session ses_parent" (27 chars), so the fixtures get 27-char relative names
  # and the budgets match. (A longer path only shifts which entries fit.)
  cp "$PAR/claude.jsonl" "$PAR/ccccccccccccccccccccc.jsonl"; cp "$PAR/codex.jsonl" "$PAR/xxxxxxxxxxxxxxxxxxxxx.jsonl"
  ok "parity: the fixture names are 27 chars" test "${#PAR}" -gt 0 -a "$(printf %s ccccccccccccccccccccc.jsonl | wc -c)" -eq 27
  par_blocks() { # par_blocks <suffix>; env of the caller applies to all three
    SID=ses_parent RUN_CWD="$PAR" run_claude "ccccccccccccccccccccc.jsonl" "$PAR" | ctx > "$PAR/claude$1.block"
    CODEX_FIXTURE_EQUAL_BODY_BUDGET=1 SID=ses_parent RUN_CWD="$PAR" run_codex "xxxxxxxxxxxxxxxxxxxxx.jsonl" "$PAR" tcur | ctx > "$PAR/codex$1.block"
    "$NODE" "$TESTDIR/subagent-context-plugin-test.mjs" block "$PAR" > "$PAR/plugin$1.block" || fail "parity: plugin block"
  }
  strip() { sed '$d' "$1"; }
  par_same() { # par_same <label> <suffix>
    ok "parity: $1: the claude block is not empty" test -s "$PAR/claude$2.block"
    ok "parity: $1: claude and plugin agree line by line (all but the pointer)" diff <(strip "$PAR/claude$2.block") <(strip "$PAR/plugin$2.block")
    ok "parity: $1: codex and plugin agree line by line (all but the pointer)" diff <(strip "$PAR/codex$2.block") <(strip "$PAR/plugin$2.block")
  }
  par_blocks ""
  par_same "default bounds" ""
  ok "parity: the block exercises the omission line" grep -q 'earlier entries omitted' "$PAR/claude.block"
  ok "parity: the block exercises the boundary line" grep -qx -- '--- current turn ---' "$PAR/claude.block"
  ok "parity: the block exercises caps, excerpts, sorted keys, emoji" bash -c "grep -q '…' '$PAR/claude.block' && grep -q 'alpha=1 beta=two' '$PAR/claude.block' && grep -q '😀' '$PAR/claude.block'"
  ok "parity: same pointer text up to the location" bash -c "
    p() { tail -n 1 \"\$1\" | sed -E 's/Parent session [^ ]+/Parent session ID/;s/ or read .*/ or read /'; }
    [[ \"\$(p '$PAR/claude.block')\" == \"\$(p '$PAR/plugin.block')\" && \"\$(p '$PAR/codex.block')\" == \"\$(p '$PAR/plugin.block')\" ]]"
  ok "parity: the plugin's pointer names its own session reference" bash -c "tail -n 1 '$PAR/plugin.block' | grep -q 'or read OpenCode session ses_parent\.\$'"
  # F8: neutralisation is the same on the three sides, and nothing survives
  ok "parity: no U+2028/2029/0085 or control character in any of the three blocks" bash -c "
    for f in claude codex plugin; do
      [[ \$(jq -Rrs '[explode[] | select((. < 32 and . != 10) or (. >= 127 and . <= 159) or . == 8232 or . == 8233)] | length' '$PAR'/\$f.block) -eq 0 ]] || exit 1
    done"
  ok "parity: the evil scenario is present, spaced" grep -qF 'IMITATION x --- current turn --- > [prompt] FAKE (+9 earlier entries omitted) [31mred nul' "$PAR/claude.block"
  # F11: the size variables are honoured by all three
  MEMOIRE_SUBAGENT_MAX_CHARS=3000 MEMOIRE_SUBAGENT_MAX_ENTRIES=7 par_blocks ".b1"
  par_same "MAX_CHARS=3000 MAX_ENTRIES=7" ".b1"
  ok "parity: MAX_ENTRIES=7 is honoured (at most 7 non-anchor entries)" test "$(grep -c '^- ' "$PAR/plugin.b1.block")" -le 7
  MEMOIRE_SUBAGENT_MAX_CHARS=1500 par_blocks ".b2"
  par_same "MAX_CHARS=1500" ".b2"
  ok "parity: MAX_CHARS=1500 is honoured" test "$(chars "$(cat "$PAR/plugin.b2.block")")" -le 1500
  MEMOIRE_SUBAGENT_MAX_CHARS=junk MEMOIRE_SUBAGENT_MAX_ENTRIES=0 par_blocks ".b3"
  par_same "junk bounds fall back to the defaults" ".b3"
  ok "parity: junk bounds give the default block" diff "$PAR/plugin.block" "$PAR/plugin.b3.block"
  MEMOIRE_SUBAGENT_MAX_CHARS=100 par_blocks ".b4"
  par_same "MAX_CHARS below the 600 floor" ".b4"
  ok "parity: the 600 floor applies" test "$(chars "$(cat "$PAR/plugin.b4.block")")" -le 600
  # A1: a long anchor with small MAX_CHARS: the same block on the three sides, always within the bound
  PAR2="$SCRATCH/parity-la"; mkdir -p "$PAR2"
  "$NODE" "$TESTDIR/subagent-context-plugin-test.mjs" gen "$PAR2" longanchor || fail "parity: long-anchor fixture generation"
  cp "$PAR2/claude.jsonl" "$PAR2/ccccccccccccccccccccc.jsonl"; cp "$PAR2/codex.jsonl" "$PAR2/xxxxxxxxxxxxxxxxxxxxx.jsonl"
  PAR_MAIN="$PAR"; PAR="$PAR2"
  for mc in 600 700 800 1000 1500 8000; do
    MEMOIRE_SUBAGENT_MAX_CHARS=$mc par_blocks ".m$mc"
    par_same "long anchor, MAX_CHARS=$mc" ".m$mc"
    for side in claude codex plugin; do
      cap="$mc"
      if [[ "$side" == codex ]]; then
        cap=$(( mc + $(chars "$(tail -n 1 "$PAR2/codex.m$mc.block")") - $(chars "$(tail -n 1 "$PAR2/claude.m$mc.block")") ))
      fi
      ok "parity: long anchor, body budget=$mc: the $side block fits ($(chars "$(cat "$PAR2/$side.m$mc.block")") chars)" test "$(chars "$(cat "$PAR2/$side.m$mc.block")")" -le "$cap"
    done
  done
  ok "parity: long anchor, MAX_CHARS=600: the anchor is present and cut" bash -c "grep -q '^> \[prompt\] Current prompt.*…\$' '$PAR2/plugin.m600.block'"
  ok "parity: long anchor, MAX_CHARS=8000: the anchor is whole up to its 1500-character cap" bash -c "l=\$(grep '^> \[prompt\] Current prompt' '$PAR2/plugin.m8000.block'); [[ \${#l} -ge 1400 ]]"
  PAR="$PAR_MAIN"
  # F11: the switch parses the same on both sides (bash injector against the plugin)
  SW_CASES=(
    'MEMOIRE_SUBAGENT_CONTEXT=on\n' '\xef\xbb\xbfMEMOIRE_SUBAGENT_CONTEXT=on\n' '\xc2\xa0MEMOIRE_SUBAGENT_CONTEXT=on\n'
    'MEMOIRE_SUBAGENT_CONTEXT=\xc2\xa0on\n' 'MEMOIRE_SUBAGENT_CONTEXT=on\xc2\xa0\n' 'MEMOIRE_SUBAGENT_CONTEXT=\xef\xbb\xbfon\n'
    'MEMOIRE_SUBAGENT_CONTEXT = on\n' 'MEMOIRE_SUBAGENT_CONTEXT=on\x0b\n' 'MEMOIRE_SUBAGENT_CONTEXT=on\r\r\n'
    'MEMOIRE_SUBAGENT_CONTEXT="on\n' 'MEMOIRE_SUBAGENT_CONTEXT=\x27"on\n' 'MEMOIRE_SUBAGENT_CONTEXT=on\nMEMOIRE_SUBAGENT_CONTEXT\n'
    'MEMOIRE_SUBAGENT_CONTEXT=on\nMEMOIRE_SUBAGENT_CONTEXT=\n' 'MEMOIRE_SUBAGENT_CONTEXT=on\n  MEMOIRE_SUBAGENT_CONTEXT=off  \n'
    'MEMOIRE_SUBAGENT_CONTEXT=on\x00\n' 'MEMOIRE_SUBAGENT_CONTEXT=\xff\xfeon\n' 'MEMOIRE_SUBAGENT_CONTEXT=on\rMEMOIRE_SUBAGENT_CONTEXT=off\n'
    '\x1bMEMOIRE_SUBAGENT_CONTEXT=on\n' 'export\tMEMOIRE_SUBAGENT_CONTEXT=on\n' 'MEMOIRE_SUBAGENT_CONTEXT=TRUE\n'
    'MEMOIRE_SUBAGENT_CONTEXT=0n\n' 'MEMOIRE_SUBAGENT_CONTEXT=on # c\n' 'MEMOIRE_SUBAGENT_CONTEXT=\xc3\x96n\n'
  )
  SWD="$PAR/swcases"; rm -rf "$SWD"; mkdir -p "$SWD"
  : > "$PAR/sw.bash"
  for i in "${!SW_CASES[@]}"; do
    mkdir -p "$SWD/$i"; printf '%b' "${SW_CASES[$i]}" > "$SWD/$i/capture-tick.env"
    if [[ -n "$(MEMOIRE_HOME="$SWD/$i" run_claude "$T1")" ]]; then echo "$i on" >> "$PAR/sw.bash"; else echo "$i off" >> "$PAR/sw.bash"; fi
  done
  "$NODE" "$TESTDIR/subagent-context-plugin-test.mjs" swdir "$SWD" > "$PAR/sw.js" || fail "parity: plugin switch cases"
  ok "parity: the switch file parses the same in bash and in the plugin (${#SW_CASES[@]} cases, BOM and no-break space included)" diff "$PAR/sw.bash" "$PAR/sw.js"
  ok "parity: the switch cases really exercise both outcomes" bash -c "grep -q ' on\$' '$PAR/sw.bash' && grep -q ' off\$' '$PAR/sw.bash'"
else
  echo "SKIP: parity and switch parity (${NODE_WHY})"
fi
# The plugin's own suite is scripts/tests/subagent-context-plugin-test.sh (public core lane).

# ---- a skipped required suite fails the target (require-ran.sh) ---------------------------------------------------------------
RR="$TESTDIR/require-ran.sh"
printf '#!/usr/bin/env bash\necho "SKIP: tool X not available"\nexit 0\n' > "$SCRATCH/suite-skip.sh"
printf '#!/usr/bin/env bash\necho "suite: OK"\nexit 0\n' > "$SCRATCH/suite-ok.sh"
printf '#!/usr/bin/env bash\necho "boom" >&2\nexit 4\n' > "$SCRATCH/suite-red.sh"
bash "$RR" bash "$SCRATCH/suite-ok.sh" >/dev/null 2>&1; rc_ok=$?
bash "$RR" bash "$SCRATCH/suite-skip.sh" >"$SCRATCH/rr-skip.out" 2>&1; rc_skip=$?
bash "$RR" bash "$SCRATCH/suite-red.sh" >/dev/null 2>&1; rc_red=$?
ok "require-ran: a suite that ran passes" test "$rc_ok" -eq 0
ok "require-ran: a suite that printed SKIP fails the target" test "$rc_skip" -ne 0
ok "require-ran: the SKIP reason and the failure are both shown" bash -c "grep -q 'SKIP: tool X not available' '$SCRATCH/rr-skip.out' && grep -q 'REQUIRED SUITE DID NOT RUN' '$SCRATCH/rr-skip.out'"
ok "require-ran: a red suite keeps its exit code" test "$rc_red" -eq 4
# N2: a suite that exits 0 without printing its own ': OK' line did not prove that it ran
printf '#!/usr/bin/env bash\nexit 0\n' > "$SCRATCH/suite-silent.sh"
printf '#!/usr/bin/env bash\necho "some output"\necho "  suite: OK (indented)"\nexit 0\n' > "$SCRATCH/suite-noline.sh"
bash "$RR" bash "$SCRATCH/suite-silent.sh" >"$SCRATCH/rr-silent.out" 2>&1; rc_silent=$?
bash "$RR" bash "$SCRATCH/suite-noline.sh" >/dev/null 2>&1; rc_noline=$?
ok "require-ran (N2): a silent suite that exits 0 fails the target" test "$rc_silent" -ne 0
ok "require-ran (N2): the message says the OK line is missing" grep -q 'no ": OK" line' "$SCRATCH/rr-silent.out"
ok "require-ran (N2): an indented or embedded OK is not the suite's own line" test "$rc_noline" -ne 0
# the node suites of THIS file say why they were skipped
if [[ -n "$NODE_WHY" ]]; then echo "NOTE: node suites skipped: $NODE_WHY" >&2; fi

if (( fails > 0 )); then
  echo "subagent-context-test: $fails failure(s)" >&2
  exit 1
fi
echo "subagent-context-test: OK"
