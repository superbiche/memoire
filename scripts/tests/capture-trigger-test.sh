#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# capture-trigger-test.sh — the capture triggers of increment 2 (Claude Code
# Stop, Codex Stop, OpenCode session.idle wrapper): scripts/memoire-trigger-
# capture-{claude,codex,opencode}.sh. test-ops lane (`make test`, Makefile). Offline and
# hermetic: fixtures generated here, scratch HOME and MEMOIRE_HOME, stub readers
# (and the real Claude and Codex readers over a stub queue writer for the race),
# no harness, no network, never the operator's registry or home.
#
#   a  switches     each switch off or absent: the leg logs "disabled" (or
#                   "capture-off") and does nothing; last assignment wins; the
#                   env file is never sourced; ticks are unaffected
#   b  fail-open    jq missing, unwritable log dir, missing env file, unreadable
#                   transcript, junk input: exit 0, nothing on stdout, one log line
#   c  poll         Claude: a transcript grown around the real Stop payloads;
#                   final text late -> found (on decoded text); never there ->
#                   size-stable stop; busy transcript -> bound; needle only in a
#                   user line or an older assistant text -> not a signal
#   d  inline time  Claude worst case and normal path, Codex hook, under baseline-
#                   scaled limits capped 500 ms below the registered timeouts
#   e  codex        null transcript_path; closing record present / late / absent:
#                   the reader is invoked either way and the log says which
#   f  opencode     archive / missing live database skipped; live path runs with
#                   the reader's gate overrides; 5 s debounce; a pending marker
#                   gives exactly one extra run; the real reader over the census
#                   fixture (when sqlite3 >= 3.44 is there)
#   g  race         trigger against a tick-style reader on the same transcript /
#                   rollout: one captures, the other logs skipped=locked, ids unique
#   h  detachment   the reader completes after the hook process has exited
#
# A skipped suite prints SKIP with the reason; scripts/tests/require-ran.sh
# (the Makefile wraps this in it) fails the target.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CL="$ROOT/scripts/memoire-trigger-capture-claude.sh"
CX="$ROOT/scripts/memoire-trigger-capture-codex.sh"
OC="$ROOT/scripts/memoire-trigger-capture-opencode.sh"
CC_READER="$ROOT/scripts/memoire-capture-claude-transcripts.sh"
CX_READER="$ROOT/scripts/memoire-capture-codex-rollout.sh"
FIX="$ROOT/tests/foreign/fixtures"
for t in jq flock timeout setsid base64; do command -v "$t" >/dev/null || { echo "SKIP: $t not available"; exit 0; }; done

# Own session: the cleanup below kills processes; nothing here may reach the caller's group.
if [[ -z "${CAPTURE_TRIGGER_OWN_SESSION:-}" ]]; then
  CAPTURE_TRIGGER_OWN_SESSION=1 exec setsid -w bash "${BASH_SOURCE[0]}" "$@"
fi

fails=0
fail() { echo "FAIL: $*" >&2; fails=$(( fails + 1 )); }
ok() { local msg="$1"; shift; "$@" || fail "$msg"; }
has() { grep -qF -- "$2" <<<"$1"; }
now_ms() { echo $(( $(date +%s%N) / 1000000 )); }

SCRATCH="$(mktemp -d "${TMPDIR:-/var/tmp}/capture-trigger-test.XXXXXX")" && [[ -n "$SCRATCH" && -d "$SCRATCH" ]] \
  || { echo "FAIL: cannot create the scratch directory under ${TMPDIR:-/var/tmp}" >&2; exit 1; }
# kill_own: SIGKILL every process of this test's own session (the re-exec above made it a session of its
# own, sid == $$) and every process that carries this run's token in its environment (the detached runs
# start a session of their own with setsid and inherit the token). Never a command-line match: a process
# of the caller whose command line holds the scratch path is not touched.
export CAPTURE_TRIGGER_TEST_TOKEN="$SCRATCH"
kill_own() {
  [[ -n "$SCRATCH" && -d "$SCRATCH" ]] || return 0
  local pid sid p
  while read -r pid sid; do
    [[ "$sid" == "$$" && "$pid" != "$$" && "$pid" != "$BASHPID" ]] && kill -9 "$pid" 2>/dev/null
  done < <(ps -e -o pid=,sid= 2>/dev/null)
  # grep without -q: under pipefail, an early exit of grep -q lets tr die of
  # SIGPIPE and the pipeline reports failure for a process that DOES carry the
  # token (i6 missed one sentinel in about one run out of three).
  for p in /proc/[0-9]*; do
    pid="${p#/proc/}"; [[ "$pid" == "$$" || "$pid" == "$BASHPID" ]] && continue
    if { tr '\0' '\n' < "$p/environ"; } 2>/dev/null | grep -xF "CAPTURE_TRIGGER_TEST_TOKEN=$SCRATCH" >/dev/null; then kill -9 "$pid" 2>/dev/null; fi
  done
  return 0
}
cleanup() {
  [[ -n "$SCRATCH" && -d "$SCRATCH" ]] || return 0
  kill_own
  rm -rf "$SCRATCH"
}
trap cleanup EXIT
export HOME="$SCRATCH/home"; mkdir -p "$HOME"
export TMPDIR="$SCRATCH/tmp"; mkdir -p "$TMPDIR"
export MEMOIRE_PROJECTS_REGISTRY="$SCRATCH/no-registry.json"
export TZ=UTC
unset MEMOIRE_CAPTURE MEMOIRE_ATTRIB_FORK MEMOIRE_PROJECT MEMOIRE_HOOK_CAPTURE MEMOIRE_CAPTURE_CODEX MEMOIRE_CAPTURE_OPENCODE \
  MEMOIRE_OPENCODE_DB OPENCODE_DB MEMOIRE_CAPTURE_CC_BIN MEMOIRE_CAPTURE_CODEX_BIN MEMOIRE_CAPTURE_OPENCODE_BIN MEMOIRE_TRIGGER_LOG
ok "the test runs as its own session leader" test "$(ps -o sid= -p $$ | tr -d ' ')" = "$$"

# ---- timing scale (as the sub-agent perf test): baseline measured, limits scaled 1.0x..3.0x ----
BASELINE_MS=999999
for _i in 1 2 3; do
  _s="$(now_ms)"; jq -n 'reduce range(0;1000000) as $i (0; .+$i)' >/dev/null 2>&1; _m=$(( $(now_ms) - _s ))
  (( _m < BASELINE_MS )) && BASELINE_MS="$_m"
done
[[ "${CAPTURE_TRIGGER_BASELINE_MS:-}" =~ ^[0-9]+$ ]] && BASELINE_MS="$CAPTURE_TRIGGER_BASELINE_MS"
SCALE_PCT=$(( BASELINE_MS * 100 / 150 )); (( SCALE_PCT < 100 )) && SCALE_PCT=100; (( SCALE_PCT > 300 )) && SCALE_PCT=300
CL_TIMEOUT_S="$(sed -n 's/^# registered-timeout-s: *//p' "$CL" | head -n 1)"; : "${CL_TIMEOUT_S:=5}"
CX_TIMEOUT_S="$(sed -n 's/^# registered-timeout-s: *//p' "$CX" | head -n 1)"; : "${CX_TIMEOUT_S:=3}"
capped_lim() { # <base ms> <timeout s>: scaled, never above timeout - 500 ms
  local v=$(( $1 * SCALE_PCT / 100 )) cap=$(( $2 * 1000 - 500 ))
  (( v > cap )) && v="$cap"; echo "$v"
}
CL_WORST_LIMIT="$(capped_lim 3000 "$CL_TIMEOUT_S")"
CL_NORMAL_LIMIT="$(capped_lim 1000 "$CL_TIMEOUT_S")"
CX_LIMIT="$(capped_lim 300 "$CX_TIMEOUT_S")"
echo "capture-trigger-test baseline: jq ${BASELINE_MS} ms (ref 150), scale ${SCALE_PCT}%; limits: claude worst ${CL_WORST_LIMIT} ms (timeout ${CL_TIMEOUT_S} s), claude normal ${CL_NORMAL_LIMIT} ms, codex hook ${CX_LIMIT} ms (timeout ${CX_TIMEOUT_S} s)" >&2

# ---- harness of the cases ---------------------------------------------------------------------
mkdir -p "$SCRATCH/bin"
# Stub reader: records its call (args, the STUCK_MS it saw, a start time) and acts as told.
cat > "$SCRATCH/bin/reader" <<'STUB'
#!/usr/bin/env bash
{ printf '%s start=%s stuck=%s needle-env=%s args:' "$(date +%s%3N)" "$$" "${MEMOIRE_OPENCODE_STUCK_MS-unset}" "$(tr '\0' '\n' < /proc/$$/environ | grep -q NEEDLEFRAG && echo yes || echo no)"; printf ' %s' "$@"; printf '\n'; } >> "$STUB_LOG"
[[ -n "${STUB_SLEEP:-}" ]] && sleep "$STUB_SLEEP"
[[ -n "${STUB_OUT:-}" ]] && printf '%b' "$STUB_OUT"
[[ -n "${STUB_ERR:-}" ]] && printf '%b' "$STUB_ERR" >&2
[[ -n "${STUB_DONE:-}" ]] && : > "$STUB_DONE"
exit "${STUB_RC:-0}"
STUB
chmod +x "$SCRATCH/bin/reader"
# Stub queue writer for the real readers: per call, one line "CALL <id>..." and one stdout line
# per batch item (the codex reader counts them); SLOWQ: seconds to sleep first.
cat > "$SCRATCH/bin/queue" <<'STUB'
#!/usr/bin/env bash
[[ -n "${SLOWQ:-}" ]] && sleep "$SLOWQ"
args=("$@"); batch=""; single=""
for ((i = 0; i < ${#args[@]}; i++)); do
  case "${args[i]}" in -batch-file) batch="${args[i+1]}" ;; -source-event-id) single="${args[i+1]}" ;; esac
done
if [[ -n "$batch" ]]; then
  while IFS= read -r l; do id="${l#*\"source_event_id\":\"}"; echo "ID ${id%%\"*}" >> "$QLOG"; echo "$batch"; done < "$batch"
else
  echo "ID $single" >> "$QLOG"
fi
exit 0
STUB
chmod +x "$SCRATCH/bin/queue"

CASE=0
newcase() { # sets MEMOIRE_HOME, STUB_LOG, QLOG, LOG dir; env file content from $1 (may be "-" for none)
  CASE=$(( CASE + 1 ))
  export MEMOIRE_HOME="$SCRATCH/mh$CASE"; mkdir -p "$MEMOIRE_HOME"
  export STUB_LOG="$SCRATCH/stub$CASE.log" QLOG="$SCRATCH/q$CASE.log"; : > "$STUB_LOG"; : > "$QLOG"
  [[ "${1:--}" == "-" ]] || printf '%b' "$1" > "$MEMOIRE_HOME/capture-tick.env"
  unset STUB_SLEEP STUB_OUT STUB_ERR STUB_DONE STUB_RC SLOWQ
  export MEMOIRE_CAPTURE_CC_BIN="$SCRATCH/bin/reader" MEMOIRE_CAPTURE_CODEX_BIN="$SCRATCH/bin/reader" MEMOIRE_CAPTURE_OPENCODE_BIN="$SCRATCH/bin/reader"
  export MEMOIRE_TRIGGER_STEP_MS=100
}
ON_ALL='MEMOIRE_HOOK_CAPTURE=on\nMEMOIRE_CAPTURE_CODEX=on\nMEMOIRE_CAPTURE_OPENCODE=on\n'
logof() { cat "$MEMOIRE_HOME/log/trigger-$1.log" 2>/dev/null; }
lastlog() { logof "$1" | tail -n 1; }
hooklog() { logof "$1" | grep "stage=hook" | tail -n 1; }
stubcalls() { grep -c 'args:' "$STUB_LOG" 2>/dev/null || true; }
# wait_for <file> <pattern> <seconds>: the detached run's log line
wait_for() { local i; for ((i = 0; i < $3 * 20; i++)); do grep -q -- "$2" "$1" 2>/dev/null && return 0; sleep 0.05; done; return 1; }
wait_run() { wait_for "$MEMOIRE_HOME/log/trigger-$1.log" 'stage=run' "${2:-6}"; }

iso() { printf '2026-09-30T10:%02d:%02d.000Z' $(( ($1 / 60) % 60 )) $(( $1 % 60 )); }
cc_user() { printf '{"type":"user","uuid":"%s","timestamp":"%s","sessionId":"%s","cwd":"/scratch/p","message":{"role":"user","content":"%s"}}\n' "$1" "$(iso "$2")" "$3" "$4"; }
cc_text() { # uuid sec session text(json string content, already escaped)
  printf '{"type":"assistant","uuid":"%s","timestamp":"%s","sessionId":"%s","cwd":"/scratch/p","message":{"role":"assistant","content":[{"type":"text","text":"%s"}]}}\n' "$1" "$(iso "$2")" "$3" "$4"
}
cc_tool() { printf '{"type":"assistant","uuid":"%s","timestamp":"%s","sessionId":"%s","cwd":"/scratch/p","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"true"}}]}}\n' "$1" "$(iso "$2")" "$3"; }
# stop_payload <fixture> <transcript> [extra jq filter]
stop_payload() { jq -c --arg t "$2" '.transcript_path = $t | .cwd = "/scratch/p"'"${3:+ | $3}" "$FIX/claude-stop/$1"; }
F10=run1-010-Stop-stdin.json
F18=run1-018-Stop-stdin.json
TEXT18="$(jq -r .last_assistant_message "$FIX/claude-stop/$F18")"
TEXT10="$(jq -r .last_assistant_message "$FIX/claude-stop/$F10")"
SID18="$(jq -r .session_id "$FIX/claude-stop/$F18")"

# Base transcript of a turn whose final text is NOT written yet.
mk_base() { # <file> <session>
  { cc_user u1 1 "$2" "hello"; cc_tool a1 2 "$2"; cc_text a0 3 "$2" "an older answer, not this turn's"; } > "$1"
}
json_str() { jq -Rs '.' | sed -e 's/^"//' -e 's/"$//'; }   # JSON-escape stdin, without the quotes

# =====================================================================================================
echo "--- a: switches"
TR="$SCRATCH/t-a.jsonl"; mk_base "$TR" "$SID18"
hook_claude() { stop_payload "$F18" "$TR" | bash "$CL" 2>"$SCRATCH/err"; }
for spec in \
  '-|disabled|no env file' \
  'MEMOIRE_HOOK_CAPTURE=off\n|disabled|HOOK off' \
  'MEMOIRE_CAPTURE_CODEX=on\nMEMOIRE_CAPTURE_OPENCODE=on\n|disabled|HOOK absent (ticks opt-ins alone)' \
  'MEMOIRE_HOOK_CAPTURE=on\nMEMOIRE_HOOK_CAPTURE=off\n|disabled|last assignment wins (off)' \
  'MEMOIRE_HOOK_CAPTURE=on; touch '"$SCRATCH"'/pwned\n|disabled|junk value is not on' ; do
  IFS='|' read -r content want label <<<"$spec"
  newcase "$content"; : > "$STUB_LOG"
  hook_claude; rc=$?
  ok "a claude [$label]: exit 0" test "$rc" -eq 0
  ok "a claude [$label]: logged status=$want" has "$(lastlog claude)" "status=$want"
  ok "a claude [$label]: reader not called" test "$(stubcalls)" -eq 0
done
ok "a: the env file is never sourced" test ! -e "$SCRATCH/pwned"
newcase 'export MEMOIRE_HOOK_CAPTURE="ON" # comment\n'
STUB_OUT='' hook_claude; wait_run claude
ok "a claude: export/quote/case/comment forms enable (reader called)" test "$(stubcalls)" -eq 1
newcase "$ON_ALL"; MEMOIRE_CAPTURE=off hook_claude
ok "a claude: MEMOIRE_CAPTURE=off kills" has "$(lastlog claude)" "status=capture-off"
ok "a claude: MEMOIRE_CAPTURE=off, reader not called" test "$(stubcalls)" -eq 0
newcase "$ON_ALL"; MEMOIRE_ATTRIB_FORK=1 hook_claude
ok "a claude: MEMOIRE_ATTRIB_FORK skips" has "$(lastlog claude)" "status=attrib-fork"
newcase "$ON_ALL"; stop_payload "$F18" "$TR" '.stop_hook_active = true' | bash "$CL" 2>/dev/null
ok "a claude: stop_hook_active skips" has "$(lastlog claude)" "status=stop-hook-active"
ok "a claude: stop_hook_active, reader not called" test "$(stubcalls)" -eq 0
# the detached run re-reads the switches itself
newcase 'MEMOIRE_HOOK_CAPTURE=off\n'; bash "$CL" --run "$TR" s1 2>/dev/null
ok "a claude: --run re-reads the switch" has "$(lastlog claude)" "stage=run status=disabled"
ok "a claude: --run disabled, reader not called" test "$(stubcalls)" -eq 0

RO="$SCRATCH/t-a-rollout.jsonl"
{ jq -nc '{type:"session_meta",payload:{id:"th1",cwd:"/scratch/p"}}'; jq -nc '{type:"event_msg",payload:{type:"task_started",turn_id:"t1"}}'; } > "$RO"
cx_payload() { jq -nc --arg t "${1:-$RO}" --arg turn "${2:-t1}" --arg m "${3:-done}" '{session_id:"th1",transcript_path:$t,cwd:"/scratch/p",turn_id:$turn,hook_event_name:"Stop",stop_hook_active:false,last_assistant_message:$m}'; }
for spec in \
  '-|disabled|no env file' \
  'MEMOIRE_HOOK_CAPTURE=on\n|disabled|HOOK on, CODEX leg absent' \
  'MEMOIRE_HOOK_CAPTURE=on\nMEMOIRE_CAPTURE_CODEX=off\n|disabled|CODEX leg off' \
  'MEMOIRE_CAPTURE_CODEX=on\n|disabled|CODEX on, HOOK absent' ; do
  IFS='|' read -r content want label <<<"$spec"
  newcase "$content"
  cx_payload | bash "$CX" 2>/dev/null; rc=$?
  ok "a codex [$label]: exit 0" test "$rc" -eq 0
  ok "a codex [$label]: logged status=$want" has "$(lastlog codex)" "status=$want"
  sleep 0.2; ok "a codex [$label]: reader not called" test "$(stubcalls)" -eq 0
done
newcase 'MEMOIRE_HOOK_CAPTURE=on\n'; cx_payload | MEMOIRE_CAPTURE_CODEX=on bash "$CX" 2>/dev/null
ok "a codex: the process environment is the fallback for the leg gate" wait_run codex
newcase "$ON_ALL"; cx_payload | MEMOIRE_CAPTURE=off bash "$CX" 2>/dev/null
ok "a codex: MEMOIRE_CAPTURE=off kills" has "$(lastlog codex)" "status=capture-off"
newcase 'MEMOIRE_HOOK_CAPTURE=off\nMEMOIRE_CAPTURE_CODEX=on\n'; bash "$CX" --run "$RO" t1 th1 2>/dev/null
ok "a codex: --run re-reads the switches" has "$(lastlog codex)" "stage=run status=disabled"

mkdir -p "$HOME/.local/share/opencode"; : > "$HOME/.local/share/opencode/opencode.db"
for spec in \
  '-|disabled|no env file' \
  'MEMOIRE_HOOK_CAPTURE=on\n|disabled|OPENCODE leg absent' \
  'MEMOIRE_HOOK_CAPTURE=on\nMEMOIRE_CAPTURE_OPENCODE=off\n|disabled|OPENCODE leg off' \
  'MEMOIRE_CAPTURE_OPENCODE=on\n|disabled|HOOK absent' ; do
  IFS='|' read -r content want label <<<"$spec"
  newcase "$content"
  bash "$OC" ses_abc 2>/dev/null; rc=$?
  ok "a opencode [$label]: exit 0" test "$rc" -eq 0
  ok "a opencode [$label]: logged status=$want" has "$(lastlog opencode)" "status=$want"
  ok "a opencode [$label]: reader not called" test "$(stubcalls)" -eq 0
done
newcase "$ON_ALL"; MEMOIRE_CAPTURE=off bash "$OC" ses_abc 2>/dev/null
ok "a opencode: MEMOIRE_CAPTURE=off kills" has "$(lastlog opencode)" "status=capture-off"

# Ticks are unaffected: HOOK off, the tick's own opt-in on -> the tick still runs (when the tick scripts are in this tree).
TICKS=0
for leg in codex opencode; do
  TICK="$ROOT/scripts/memoire-capture-$leg-tick.sh"; [[ -x "$TICK" ]] || continue
  TICKS=$(( TICKS + 1 ))
  ok "a: the $leg tick does not read MEMOIRE_HOOK_CAPTURE" bash -c '! grep -q MEMOIRE_HOOK_CAPTURE "$1"' _ "$TICK"
  newcase 'MEMOIRE_HOOK_CAPTURE=off\n'
  mkdir -p "$SCRATCH/sess" "$SCRATCH/arch"
  if [[ "$leg" == codex ]]; then
    env MEMOIRE_CAPTURE_CODEX=on MEMOIRE_CODEX_SESSIONS_DIR="$SCRATCH/sess" MEMOIRE_CODEX_ARCHIVED_SESSIONS_DIR="$SCRATCH/arch" bash "$TICK" 2>/dev/null
    ok "a: the codex tick still runs with HOOK off" has "$(cat "$MEMOIRE_HOME/log/capture-codex-tick.log" 2>/dev/null)" "tick_codex files="
  else
    env MEMOIRE_CAPTURE_OPENCODE=on bash "$TICK" 2>/dev/null
    ok "a: the opencode tick still runs with HOOK off" has "$(cat "$MEMOIRE_HOME/log/capture-opencode-tick.log" 2>/dev/null)" "tick_opencode "
  fi
done
echo "a: $TICKS tick script(s) present in this tree"

# =====================================================================================================
echo "--- b: fail-open"
# A PATH with the tools the legs need, without jq.
NOJQ="$SCRATCH/nojq"; mkdir -p "$NOJQ"
for t in bash cat date mkdir grep tr cut sed stat tail head dirname sleep base64 readlink env kill timeout setsid flock awk mktemp rm touch printf; do
  p="$(command -v "$t" 2>/dev/null)" && [[ -x "$p" ]] && ln -sf "$p" "$NOJQ/$t"
done
newcase "$ON_ALL"; mk_base "$TR" "$SID18"
stop_payload "$F18" "$TR" | PATH="$NOJQ" bash "$CL" >"$SCRATCH/out" 2>/dev/null; rc=$?
ok "b claude: jq missing -> exit 0" test "$rc" -eq 0
ok "b claude: jq missing -> error=jq-missing logged" has "$(lastlog claude)" "status=error=jq-missing"
ok "b claude: jq missing -> nothing on stdout" test ! -s "$SCRATCH/out"
newcase "$ON_ALL"; cx_payload | PATH="$NOJQ" bash "$CX" >"$SCRATCH/out" 2>/dev/null; rc=$?
ok "b codex: jq missing -> exit 0, logged" test "$rc" -eq 0 && has "$(lastlog codex)" "error=jq-missing"
# unwritable log directory: a regular file where the directory should be
for leg in claude codex opencode; do
  newcase "$ON_ALL"; : > "$MEMOIRE_HOME/log"
  case "$leg" in
    claude) stop_payload "$F18" "$TR" | bash "$CL" >"$SCRATCH/out" 2>/dev/null ;;
    codex) cx_payload | bash "$CX" >"$SCRATCH/out" 2>/dev/null ;;
    opencode) bash "$OC" ses_abc >"$SCRATCH/out" 2>/dev/null ;;
  esac; rc=$?
  ok "b $leg: unwritable log dir -> exit 0" test "$rc" -eq 0
  ok "b $leg: unwritable log dir -> nothing on stdout" test ! -s "$SCRATCH/out"
done
# unreadable transcript, junk input
newcase "$ON_ALL"
stop_payload "$F18" "$SCRATCH/nope.jsonl" | bash "$CL" >"$SCRATCH/out" 2>/dev/null; rc=$?
ok "b claude: unreadable transcript -> exit 0" test "$rc" -eq 0
ok "b claude: unreadable transcript -> one log line" test "$(logof claude | wc -l)" -eq 1 && has "$(lastlog claude)" "error=transcript-unreadable"
newcase "$ON_ALL"; printf 'not json {' | bash "$CL" >"$SCRATCH/out" 2>/dev/null; rc=$?
ok "b claude: junk input -> exit 0, logged" test "$rc" -eq 0 && has "$(lastlog claude)" "status=error=transcript-unreadable"
newcase "$ON_ALL"; printf '' | bash "$CL" >/dev/null 2>&1; ok "b claude: empty input -> exit 0" test "$?" -eq 0
newcase "$ON_ALL"; printf 'not json {' | bash "$CX" >"$SCRATCH/out" 2>/dev/null; rc=$?
ok "b codex: junk input -> exit 0, transcript_path=null logged" test "$rc" -eq 0 && has "$(lastlog codex)" "transcript_path=null"
newcase "$ON_ALL"; cx_payload "$SCRATCH/nope.jsonl" | bash "$CX" 2>/dev/null
wait_run codex; ok "b codex: unreadable rollout -> error logged by the run, reader not called" has "$(lastlog codex)" "error=rollout-unreadable"
ok "b codex: unreadable rollout -> reader not called" test "$(stubcalls)" -eq 0
newcase "$ON_ALL"; bash "$OC" 'bad id/../x' 2>/dev/null; rc=$?
ok "b opencode: bad session id -> exit 0, logged" test "$rc" -eq 0 && has "$(lastlog opencode)" "error=bad-session-id"
newcase "$ON_ALL"; bash "$OC" 2>/dev/null; ok "b opencode: no session id -> exit 0" test "$?" -eq 0
# a reader that fails or hangs past its timeout: still exit 0, logged
newcase "$ON_ALL"; STUB_RC=1 bash "$CL" --run "$TR" s1 2>/dev/null
ok "b claude: a failing reader -> error=reader logged" has "$(lastlog claude)" "status=error=reader"
newcase "$ON_ALL"; STUB_SLEEP=5 MEMOIRE_TRIGGER_READER_TIMEOUT_S=1 bash "$CL" --run "$TR" s1 2>/dev/null
ok "b claude: a reader past its timeout -> error=timeout logged" has "$(lastlog claude)" "status=error=timeout"
ok "b: a run leaves no work directory behind" test -z "$(ls -A "$MEMOIRE_HOME/tmp" 2>/dev/null)"

# =====================================================================================================
echo "--- c: Claude poll on the real Stop payloads"
# c1: the final text (payload 010, then 018 with quotes/newlines/backticks) appears ~400 ms after the hook starts
for F in "$F10" "$F18"; do
  newcase "$ON_ALL"; SID="$(jq -r .session_id "$FIX/claude-stop/$F")"; MSG="$(jq -r .last_assistant_message "$FIX/claude-stop/$F")"
  TR="$SCRATCH/t-c-$CASE.jsonl"; mk_base "$TR" "$SID"
  ( sleep 0.4; cc_text a9 9 "$SID" "$(printf '%s' "$MSG" | json_str)" >> "$TR" ) & LATE=$!
  stop_payload "$F" "$TR" | MEMOIRE_TRIGGER_STEP_MS=250 bash "$CL" 2>/dev/null; wait "$LATE"
  ok "c [$F]: the late final text is found (poll_end=signal)" has "$(hooklog claude)" "poll_end=signal"
  ok "c [$F]: found within the bound" test "$(hooklog claude | sed -n 's/.* poll_ms=\([0-9]*\).*/\1/p')" -lt 1500
  wait_run claude; ok "c [$F]: the reader was started on the transcript with --tail" has "$(cat "$STUB_LOG")" "--tail --emit-sessions $TR"
done
# c2: present at once -> no wait
newcase "$ON_ALL"; TR="$SCRATCH/t-c2.jsonl"; mk_base "$TR" "$SID18"; cc_text a9 9 "$SID18" "$(printf '%s' "$TEXT18" | json_str)" >> "$TR"
stop_payload "$F18" "$TR" | bash "$CL" 2>/dev/null
ok "c: a final text already present -> polls=0" has "$(hooklog claude)" "polls=0 poll_end=signal"
# c3: never appears, file static -> size-stable stop, well inside the bound
newcase "$ON_ALL"; TR="$SCRATCH/t-c3.jsonl"; mk_base "$TR" "$SID18"
stop_payload "$F18" "$TR" | bash "$CL" 2>/dev/null
ok "c: never appears, static file -> poll_end=stable" has "$(hooklog claude)" "poll_end=stable"
ok "c: ...and the reader still runs (latency, never loss)" test "$(wait_run claude; stubcalls)" -eq 1
# c4: never appears, transcript keeps growing -> the bound
newcase "$ON_ALL"; TR="$SCRATCH/t-c4.jsonl"; mk_base "$TR" "$SID18"
( for i in $(seq 1 26); do sleep 0.1; cc_tool "x$i" 10 "$SID18" >> "$TR"; done ) & BUSY=$!
stop_payload "$F18" "$TR" | bash "$CL" 2>/dev/null; wait "$BUSY"
ok "c: never appears, busy file -> poll_end=bound" has "$(hooklog claude)" "poll_end=bound"
PMS="$(hooklog claude | sed -n 's/.* poll_ms=\([0-9]*\).*/\1/p')"
ok "c: the poll stops at the 1.5 s bound (poll_ms=$PMS)" test "$PMS" -ge 1450 -a "$PMS" -le 1600
# c5: the needle only in a USER line or only in an OLDER assistant text is not a signal
newcase "$ON_ALL"; TR="$SCRATCH/t-c5.jsonl"; mk_base "$TR" "$SID18"
cc_user u9 9 "$SID18" "$(printf '%s' "$TEXT10" | json_str)" >> "$TR"
cc_text a8 8 "$SID18" "$(printf '%s' "$TEXT10" | json_str)" >> "$TR"; cc_text a7 9 "$SID18" "a newer, different text" >> "$TR"
stop_payload "$F10" "$TR" | bash "$CL" 2>/dev/null
ok "c: a needle in a user line / an older assistant text is no signal" has "$(hooklog claude)" "poll_end=stable"
# c6: raw escapes: the text decoded from JSON matches (a raw grep of the escaped line would not)
newcase "$ON_ALL"; TR="$SCRATCH/t-c6.jsonl"; mk_base "$TR" "$SID18"
MSG6=$'line one "quoted"\nsecond line é — \\backslash and a tab\there'
cc_text a9 9 "$SID18" "$(printf '%s' "$MSG6" | json_str)" >> "$TR"
stop_payload "$F18" "$TR" "| .last_assistant_message = \$m" 2>/dev/null >/dev/null
jq -c --arg t "$TR" --arg m "$MSG6" '.transcript_path=$t | .last_assistant_message=$m' "$FIX/claude-stop/$F18" | bash "$CL" 2>/dev/null
ok "c: escapes, newline, non-ASCII and a backslash match on decoded text" has "$(hooklog claude)" "poll_end=signal"
# c7: a sidechain (sub-agent) text is not the turn's final text
newcase "$ON_ALL"; TR="$SCRATCH/t-c7.jsonl"; mk_base "$TR" "$SID18"
cc_text a9 9 "$SID18" "$(printf '%s' "$TEXT18" | json_str)" | jq -c '. + {isSidechain:true}' >> "$TR"
stop_payload "$F18" "$TR" | bash "$CL" 2>/dev/null
ok "c: a sidechain line is no signal" has "$(hooklog claude)" "poll_end=stable"

# =====================================================================================================
echo "--- d: inline time under scaled limits (Claude worst case = busy file + slow reader)"
worst=0
for i in 1 2 3; do
  newcase "$ON_ALL"; TR="$SCRATCH/t-d$i.jsonl"; mk_base "$TR" "$SID18"
  ( for j in $(seq 1 35); do sleep 0.1; cc_tool "x$j" 10 "$SID18" >> "$TR"; done ) & BUSY=$!
  export STUB_SLEEP=6   # the reader outlives the hook's 1 s wait
  s="$(now_ms)"; stop_payload "$F18" "$TR" | bash "$CL" 2>/dev/null; m=$(( $(now_ms) - s ))
  (( m > worst )) && worst="$m"
  wait "$BUSY"
done
echo "d: claude worst case ${worst} ms (limit ${CL_WORST_LIMIT})" >&2
ok "d claude: worst case (poll bound + 1 s wait) = $worst ms, limit $CL_WORST_LIMIT ms (timeout ${CL_TIMEOUT_S} s)" test "$worst" -lt "$CL_WORST_LIMIT"
ok "d claude: the hook did wait for the run (run=detached logged)" has "$(hooklog claude)" "run=detached"
worst=0
for i in 1 2 3; do
  newcase "$ON_ALL"; TR="$SCRATCH/t-d$i.jsonl"; mk_base "$TR" "$SID18"; cc_text a9 9 "$SID18" "$(printf '%s' "$TEXT18" | json_str)" >> "$TR"
  s="$(now_ms)"; stop_payload "$F18" "$TR" | bash "$CL" 2>/dev/null; m=$(( $(now_ms) - s ))
  (( m > worst )) && worst="$m"
done
echo "d: claude normal path ${worst} ms (limit ${CL_NORMAL_LIMIT})" >&2
ok "d claude: normal path (text present, quick reader) = $worst ms, limit $CL_NORMAL_LIMIT ms" test "$worst" -lt "$CL_NORMAL_LIMIT"
ok "d claude: a quick reader is waited for (run=done)" has "$(hooklog claude)" "run=done"
worst=0
for i in 1 2 3; do
  newcase "$ON_ALL"; export STUB_SLEEP=3
  s="$(now_ms)"; cx_payload | bash "$CX" 2>/dev/null; m=$(( $(now_ms) - s ))
  (( m > worst )) && worst="$m"
done
echo "d: codex hook ${worst} ms (limit ${CX_LIMIT})" >&2
ok "d codex: the hook detaches first: worst of 3 = $worst ms, limit $CX_LIMIT ms (timeout ${CX_TIMEOUT_S} s)" test "$worst" -lt "$CX_LIMIT"


# =====================================================================================================
echo "--- e: Codex trigger"
# A rollout of one turn: meta, task_started, the assistant message; the closing record is appended (or not).
mk_rollout() { # <file> <turn> <message>
  { jq -nc '{type:"session_meta",payload:{id:"th1",cwd:"/scratch/p"}}'
    jq -nc --arg t "$2" '{type:"event_msg",payload:{type:"task_started",turn_id:$t}}'
    jq -nc --arg m "$3" '{type:"response_item",payload:{type:"message",role:"assistant",content:[{type:"output_text",text:$m}]}}'; } > "$1"
}
closing_rec() { jq -nc --arg t "$1" '{type:"event_msg",payload:{type:"task_complete",turn_id:$t}}'; }
MSG_CX='The answer is forty-two, with "quotes" and accents é.'
# e1: transcript_path null -> log, exit 0, no spawn
newcase "$ON_ALL"
jq -nc '{session_id:"th1",transcript_path:null,turn_id:"t1",hook_event_name:"Stop"}' | bash "$CX" 2>/dev/null; rc=$?
ok "e codex: transcript_path null -> exit 0" test "$rc" -eq 0
ok "e codex: transcript_path null -> logged" has "$(lastlog codex)" "transcript_path=null"
sleep 0.3; ok "e codex: transcript_path null -> reader not called" test "$(stubcalls)" -eq 0
# e2: closing record present (task_complete for this turn)
newcase "$ON_ALL"; RO="$SCRATCH/r-e2.jsonl"; mk_rollout "$RO" t1 "partial"; closing_rec t1 >> "$RO"
cx_payload "$RO" t1 "never matches this" | bash "$CX" 2>/dev/null; wait_run codex
ok "e codex: closing present -> reader invoked with --tail on the rollout" has "$(cat "$STUB_LOG")" "--tail --summary"
ok "e codex: closing present -> log says closing=present (the closing record, not the message tail)" has "$(lastlog codex)" "closing=present"
ok "e codex: closing present -> found at once (polls=0)" has "$(lastlog codex)" "polls=0"
# e3: closing record appears late (after ~400 ms) -> found by the poll
newcase "$ON_ALL"; RO="$SCRATCH/r-e3.jsonl"; mk_rollout "$RO" t1 "partial"; export MEMOIRE_TRIGGER_STEP_MS=300
( sleep 0.5; closing_rec t1 >> "$RO" ) & LATE=$!
cx_payload "$RO" t1 "never matches this" | bash "$CX" 2>/dev/null; wait_run codex; wait "$LATE"
ok "e codex: closing record late -> closing=present" has "$(lastlog codex)" "closing=present"
# e4: a closing record of ANOTHER turn is not this turn's
newcase "$ON_ALL"; RO="$SCRATCH/r-e4.jsonl"; mk_rollout "$RO" t1 "partial"; closing_rec t0 >> "$RO"
cx_payload "$RO" t1 "never matches this" | bash "$CX" 2>/dev/null; wait_run codex
ok "e codex: another turn's closing record -> not present (stable-absent), reader still invoked" has "$(lastlog codex)" "closing=stable-absent"
ok "e codex: ...reader invoked" test "$(stubcalls)" -eq 1
# e5: no closing record at all -> reader invoked anyway, log says absent
newcase "$ON_ALL"; RO="$SCRATCH/r-e5.jsonl"; mk_rollout "$RO" t1 "partial"
cx_payload "$RO" t1 "never matches this" | bash "$CX" 2>/dev/null; wait_run codex
ok "e codex: closing absent -> reader invoked anyway" test "$(stubcalls)" -eq 1
ok "e codex: closing absent -> log says so" has "$(lastlog codex)" "closing=stable-absent"
# e6: busy rollout, never closes -> timeout path
newcase "$ON_ALL"; RO="$SCRATCH/r-e6.jsonl"; mk_rollout "$RO" t1 "partial"; export MEMOIRE_TRIGGER_POLL_MS=1000
( for j in $(seq 1 14); do sleep 0.1; jq -nc '{type:"response_item",payload:{type:"function_call_output",output:"x"}}' >> "$RO"; done ) & BUSY=$!
cx_payload "$RO" t1 "never matches this" | bash "$CX" 2>/dev/null; wait_run codex; wait "$BUSY"
ok "e codex: busy rollout, no closing -> closing=timeout, reader invoked" has "$(lastlog codex)" "closing=timeout" && test "$(stubcalls)" -eq 1
unset MEMOIRE_TRIGGER_POLL_MS
# e7: the message tail in a line counts as the turn's end
newcase "$ON_ALL"; RO="$SCRATCH/r-e7.jsonl"; mk_rollout "$RO" t1 "$MSG_CX"
cx_payload "$RO" t1 "$MSG_CX" | bash "$CX" 2>/dev/null; wait_run codex
ok "e codex: the message tail found on a line -> closing=present" has "$(lastlog codex)" "closing=present"
# e8: no turn_id in the hook input: a closing record of an earlier turn does not count, a new one does
newcase "$ON_ALL"; RO="$SCRATCH/r-e8.jsonl"; mk_rollout "$RO" t0 "old"; closing_rec t0 >> "$RO"; export MEMOIRE_TRIGGER_STEP_MS=300
( sleep 0.4; closing_rec t1 >> "$RO" ) & LATE=$!
jq -nc --arg t "$RO" '{session_id:"th1",transcript_path:$t,hook_event_name:"Stop"}' | bash "$CX" 2>/dev/null; wait_run codex; wait "$LATE"
ok "e codex: no turn_id: waits for a NEW closing record" has "$(lastlog codex)" "closing=present"
unset MEMOIRE_TRIGGER_STEP_MS
# e9: the message content is not in the log or on the reader's command line
ok "e codex: no content in the log" bash -c '! grep -qF -e "forty-two" -e "quotes" "$1"' _ "$MEMOIRE_HOME/log/trigger-codex.log"
ok "e codex: no content in the reader's arguments" bash -c '! grep -q "forty-two" "$1"' _ "$STUB_LOG"

# =====================================================================================================
echo "--- f: OpenCode wrapper"
LIVE="$HOME/.local/share/opencode/opencode.db"
ocrun() { bash "$OC" "$@" 2>/dev/null; }
# f1: archive paths skipped
newcase "$ON_ALL"; mkdir -p "$SCRATCH/arch"; : > "$SCRATCH/arch/opencode.db"
MEMOIRE_OPENCODE_DB="$SCRATCH/arch/opencode.db" ocrun ses_f1
ok "f opencode: MEMOIRE_OPENCODE_DB on the archive -> source=archive skipped" has "$(lastlog opencode)" "status=skipped" && has "$(lastlog opencode)" "source=archive"
ok "f opencode: archive -> reader not called" test "$(stubcalls)" -eq 0
newcase "$ON_ALL"; printf 'MEMOIRE_OPENCODE_DB=%s\n' "$SCRATCH/arch/opencode.db" >> "$MEMOIRE_HOME/capture-tick.env"; ocrun ses_f1
ok "f opencode: the env file's MEMOIRE_OPENCODE_DB is applied like the tick's unit -> archive skipped" has "$(lastlog opencode)" "source=archive"
newcase "$ON_ALL"; mv -f "$LIVE" "$LIVE.away"; ocrun ses_f1
ok "f opencode: live database missing (fallback would read the archive) -> skipped" has "$(lastlog opencode)" "status=skipped" && test "$(stubcalls)" -eq 0
mv -f "$LIVE.away" "$LIVE"
newcase "$ON_ALL"; MEMOIRE_OPENCODE_DB="$HOME/.local/share/../share/opencode/opencode.db" MEMOIRE_OPENCODE_TRIGGER_SETTLE_S=0 ocrun ses_f1
ok "f opencode: MEMOIRE_OPENCODE_DB resolving to the live path runs" test "$(stubcalls)" -eq 1
# f2: live path: the reader gets the gate overrides
newcase "$ON_ALL"; export MEMOIRE_OPENCODE_TRIGGER_SETTLE_S=2 MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S=0
ocrun ses_f2
L="$(cat "$STUB_LOG")"
ok "f opencode: one reader run" test "$(stubcalls)" -eq 1
ok "f opencode: MEMOIRE_OPENCODE_STUCK_MS=0 reaches the reader" has "$L" "stuck=0"
ok "f opencode: --quiet-for is settle-1" has "$L" "--quiet-for 1"
ok "f opencode: the session id is the last argument" bash -c '[[ "$1" == *" ses_f2" ]]' _ "$(grep 'args:' "$STUB_LOG" | head -n 1)"
ok "f opencode: --max-seconds and --db (the live path) given" has "$L" "--max-seconds 30 ses_f2" && has "$L" "--db $LIVE"
ok "f opencode: logged captured/empty with source=live" has "$(lastlog opencode)" "source=live"
ok "f opencode: no pending marker left" test ! -e "$MEMOIRE_HOME/hooks/oc-ses_f2.pending"
# f3: 5 s debounce (2 s here): two idles in a row -> run starts at least DEBOUNCE apart, both run
newcase "$ON_ALL"; export MEMOIRE_OPENCODE_TRIGGER_SETTLE_S=0 MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S=2
ocrun ses_f3; ocrun ses_f3
T1="$(grep 'args:' "$STUB_LOG" | sed -n 1p | cut -d' ' -f1)"; T2="$(grep 'args:' "$STUB_LOG" | sed -n 2p | cut -d' ' -f1)"
ok "f opencode: debounce: both idles ran (none dropped)" test "$(stubcalls)" -eq 2
ok "f opencode: debounce: the second start is >= 2 s after the first ($(( T2 - T1 )) ms)" test $(( T2 - T1 )) -ge 1950
# f4: a storm: pending marker -> exactly one extra run
newcase "$ON_ALL"; export MEMOIRE_OPENCODE_TRIGGER_SETTLE_S=0 MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S=1; export STUB_SLEEP=1.5
bash "$OC" ses_f4 2>/dev/null & A=$!
sleep 0.5
bash "$OC" ses_f4 2>/dev/null; bash "$OC" ses_f4 2>/dev/null; bash "$OC" ses_f4 2>/dev/null
ok "f opencode: idles during a run leave a pending marker" test -e "$MEMOIRE_HOME/hooks/oc-ses_f4.pending"
ok "f opencode: ...and log pending-marked (3 times)" test "$(logof opencode | grep -c 'status=pending-marked')" -eq 3
wait "$A"
ok "f opencode: a storm of 4 idles -> exactly 2 reader runs" test "$(stubcalls)" -eq 2
ok "f opencode: the trailing run is logged" has "$(logof opencode | grep -v pending-marked | tail -n 1)" "trailing=1"
ok "f opencode: the pending marker is consumed" test ! -e "$MEMOIRE_HOME/hooks/oc-ses_f4.pending"
unset STUB_SLEEP
# f5: a different session is not blocked by the first one's lock
newcase "$ON_ALL"; export STUB_SLEEP=1; bash "$OC" ses_f5a 2>/dev/null & A=$!; sleep 0.3
bash "$OC" ses_f5b 2>/dev/null & B=$!; wait "$A" "$B"
ok "f opencode: two sessions run independently" test "$(stubcalls)" -eq 2 && test ! -e "$MEMOIRE_HOME/hooks/oc-ses_f5a.pending"
unset STUB_SLEEP
# f6: the switches are read again after the settle: a flip during the wait is honoured
newcase "$ON_ALL"; export MEMOIRE_OPENCODE_TRIGGER_SETTLE_S=1 MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S=0
bash "$OC" ses_f6 2>/dev/null & A=$!; sleep 0.3; printf 'MEMOIRE_HOOK_CAPTURE=off\n' >> "$MEMOIRE_HOME/capture-tick.env"; wait "$A"
ok "f opencode: a switch flipped during the settle stops the run" test "$(stubcalls)" -eq 0 && has "$(lastlog opencode)" "status=disabled"
unset MEMOIRE_OPENCODE_TRIGGER_SETTLE_S MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S

# f7: the real reader over the census fixture (scratch copy at the live path): the wrapper's argument contract is accepted,
# the session's events reach the stub queue under their ids, a second run adds none (shared cursor)
SQV="$(sqlite3 --version 2>/dev/null | sed -n 's/^\([0-9]*\)\.\([0-9]*\).*/\1 \2/p')"
if [[ -n "$SQV" ]] && { set -- $SQV; (( $1 > 3 || ( $1 == 3 && $2 >= 44 ) )); }; then
  newcase "$ON_ALL"; unset MEMOIRE_CAPTURE_OPENCODE_BIN
  mv -f "$LIVE" "$LIVE.keep"; sqlite3 "$LIVE" < "$FIX/opencode-census.sql" 2>/dev/null
  export MEMOIRE_OPENCODE_TRIGGER_SETTLE_S=1 MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S=0
  oc_real() { env MEMOIRE_QUEUE_BIN="$SCRATCH/bin/queue" MEMOIRE_CAPTURE_CURSOR_DIR="$MEMOIRE_HOME/cur" bash "$OC" ses_c01 2>/dev/null; }
  oc_real
  ok "f opencode: the real reader ran through the wrapper (captured, source=live)" has "$(lastlog opencode)" "status=captured" && has "$(lastlog opencode)" "failed=0 rc=0"
  ok "f opencode: the session's events were queued under opencode: ids" test "$(grep -c '^ID opencode:' "$QLOG")" -gt 0
  N1="$(grep -c '^ID ' "$QLOG")"; oc_real
  ok "f opencode: a second run adds nothing (shared cursor)" test "$(grep -c '^ID ' "$QLOG")" -eq "$N1" && has "$(lastlog opencode)" "status=empty"
  rm -f "$LIVE"; mv -f "$LIVE.keep" "$LIVE"
  unset MEMOIRE_OPENCODE_TRIGGER_SETTLE_S MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S
else
  echo "f: sqlite3 >= 3.44 not available, the real-reader OpenCode case is not run"
fi

# =====================================================================================================
echo "--- g: race with the tick (real readers, stub queue writer)"
# Claude: a slow reader run (the tick's role) holds the cursor lock; the trigger arrives meanwhile.
newcase "$ON_ALL"; TR="$SCRATCH/t-g1.jsonl"; SG=sess-g1
{ cc_user u0 1 "$SG" "hi"; cc_text a1 2 "$SG" "first answer"; cc_text a2 3 "$SG" "second answer"; } > "$TR"
unset MEMOIRE_CAPTURE_CC_BIN
race_env() { env MEMOIRE_QUEUE_BIN="$SCRATCH/bin/queue" MEMOIRE_CAPTURE_CURSOR_DIR="$MEMOIRE_HOME/cur" "$@"; }
SLOWQ=1.2 race_env bash "$CC_READER" --tail --emit-sessions "$TR" >/dev/null 2>&1 & TICK=$!
sleep 0.5
race_env bash "$CL" --run "$TR" "$SG" 2>/dev/null
ok "g claude: the trigger against a running reader logs skipped=locked" has "$(lastlog claude)" "status=skipped=locked"
wait "$TICK"
ok "g claude: the tick-side reader captured both turns, once" test "$(sort "$QLOG" | uniq -c | awk '$1 != 1' | wc -l)" -eq 0 && test "$(grep -c '^ID cc-turn:sess-g1:a[12]$' "$QLOG")" -eq 2
race_env bash "$CL" --run "$TR" "$SG" 2>/dev/null
ok "g claude: a later trigger finds nothing more (cursor shared, no duplicate ids)" has "$(lastlog claude)" "status=empty" && test "$(grep -c '^ID ' "$QLOG")" -eq 2
# ... the other way round: the trigger holds the lock, a tick-style run skips
newcase "$ON_ALL"; TR="$SCRATCH/t-g1b.jsonl"; { cc_user u0 1 "$SG" "hi"; cc_text a1 2 "$SG" "first answer"; } > "$TR"
( SLOWQ=1.2 race_env env MEMOIRE_CAPTURE_CC_BIN="$CC_READER" bash "$CL" --run "$TR" "$SG" 2>/dev/null ) & TRG=$!
sleep 0.5
race_env bash "$CC_READER" --tail --emit-sessions "$TR" >/dev/null 2>"$SCRATCH/tickerr"
ok "g claude: the tick-style run against a running trigger logs skipped=locked" has "$(cat "$SCRATCH/tickerr")" "skipped=locked"
wait "$TRG"
ok "g claude: the trigger captured the turn under its cc-turn id" test "$(grep -c '^ID cc-turn:sess-g1:a1$' "$QLOG")" -eq 1 && has "$(lastlog claude)" "status=captured"
# Codex: the census rollout (real record shapes), same shape of race.
newcase "$ON_ALL"; RO="$SCRATCH/rollout-g2.jsonl"; command cp "$FIX/codex-census/rollout-2026-05-01T09-00-00-census-alpha.jsonl" "$RO"
SLOWQ=1.2 race_env bash "$CX_READER" --tail "$RO" >/dev/null 2>&1 & TICK=$!
sleep 0.5
race_env env MEMOIRE_CAPTURE_CODEX_BIN="$CX_READER" MEMOIRE_TRIGGER_STEP_MS=100 bash "$CX" --run "$RO" u1 cx-alpha-0001 2>/dev/null
ok "g codex: the trigger against a running reader logs skipped=locked" has "$(lastlog codex)" "status=skipped=locked"
wait "$TICK"
ok "g codex: events were queued, every id once" test "$(grep -c '^ID codex:' "$QLOG")" -gt 0 && test "$(grep '^ID ' "$QLOG" | sort | uniq -d | wc -l)" -eq 0
BEFORE="$(grep -c '^ID ' "$QLOG")"
race_env env MEMOIRE_CAPTURE_CODEX_BIN="$CX_READER" MEMOIRE_TRIGGER_STEP_MS=100 bash "$CX" --run "$RO" u1 cx-alpha-0001 2>/dev/null
ok "g codex: a later trigger adds no duplicate (shared cursor)" test "$(grep -c '^ID ' "$QLOG")" -eq "$BEFORE" && has "$(lastlog codex)" "status=empty"
# OpenCode: the wrapper's own lock (pending marker) stands for the reader's session lock, tested above (f4);
# the reader's flock -n on the session cursor is covered by tests/foreign/opencode-capture.sh.

# =====================================================================================================
echo "--- h: detachment"
# The reader child completes after the hook process has exited (bash 5.2-safe: plain background exec).
newcase "$ON_ALL"; TR="$SCRATCH/t-h1.jsonl"; mk_base "$TR" "$SID18"; cc_text a9 9 "$SID18" "$(printf '%s' "$TEXT18" | json_str)" >> "$TR"
export STUB_SLEEP=2 STUB_DONE="$SCRATCH/done-h1" MEMOIRE_TRIGGER_WAIT_MS=300
stop_payload "$F18" "$TR" | bash "$CL" 2>/dev/null
ok "h claude: the hook has returned while the reader still runs" test ! -e "$STUB_DONE"
for i in $(seq 1 120); do [[ -e "$STUB_DONE" ]] && break; sleep 0.05; done
ok "h claude: the reader completed after the hook exited" test -e "$STUB_DONE"
ok "h claude: the run logged its own line after the hook's" wait_run claude 6
newcase "$ON_ALL"; RO="$SCRATCH/r-h2.jsonl"; mk_rollout "$RO" t1 "partial"; closing_rec t1 >> "$RO"
export STUB_SLEEP=1.5 STUB_DONE="$SCRATCH/done-h2"
cx_payload "$RO" t1 "partial" | bash "$CX" 2>/dev/null
ok "h codex: the hook has returned before the reader ran" test ! -e "$STUB_DONE"
for i in $(seq 1 160); do [[ -e "$STUB_DONE" ]] && break; sleep 0.05; done
ok "h codex: the reader completes after the hook exited" test -e "$STUB_DONE"
ok "h codex: the run logged its line" wait_run codex 6
# the detached process holds none of the hook's descriptors: a reader that keeps running does not keep the hook's pipes open
newcase "$ON_ALL"; TR="$SCRATCH/t-h3.jsonl"; mk_base "$TR" "$SID18"; cc_text a9 9 "$SID18" "$(printf '%s' "$TEXT18" | json_str)" >> "$TR"
export STUB_SLEEP=2 STUB_DONE="$SCRATCH/done-h3" MEMOIRE_TRIGGER_WAIT_MS=200
s="$(now_ms)"; { stop_payload "$F18" "$TR" | bash "$CL" 2>&1; } | cat >/dev/null; m=$(( $(now_ms) - s ))
ok "h claude: the hook's stdout/stderr pipe closes at hook exit, not at reader exit (${m} ms < 1500)" test "$m" -lt 1500
unset STUB_SLEEP STUB_DONE MEMOIRE_TRIGGER_WAIT_MS
for i in $(seq 1 100); do [[ -e "$SCRATCH/done-h3" ]] && break; sleep 0.05; done

# =====================================================================================================
echo "--- i: round 2 (review fixes F1-F6, F9)"
# i1 (F1): MEMOIRE_CAPTURE=off written in the env file stops the three legs, as it stops the ticks.
for off in off 0 false OFF; do
  newcase "${ON_ALL}MEMOIRE_CAPTURE=$off\n"; TR="$SCRATCH/t-i1.jsonl"; mk_base "$TR" "$SID18"
  stop_payload "$F18" "$TR" | bash "$CL" 2>/dev/null
  ok "i1 claude hook [file MEMOIRE_CAPTURE=$off]: disabled" has "$(lastlog claude)" "status=disabled"
  bash "$CL" --run "$TR" s1 2>/dev/null
  ok "i1 claude --run [file MEMOIRE_CAPTURE=$off]: disabled" has "$(lastlog claude)" "stage=run status=disabled"
  RO="$SCRATCH/r-i1.jsonl"; mk_rollout "$RO" t1 "partial"; closing_rec t1 >> "$RO"
  cx_payload "$RO" t1 x | bash "$CX" 2>/dev/null
  ok "i1 codex hook [file MEMOIRE_CAPTURE=$off]: disabled" has "$(lastlog codex)" "status=disabled"
  bash "$CX" --run "$RO" t1 th1 2>/dev/null
  ok "i1 codex --run [file MEMOIRE_CAPTURE=$off]: disabled" has "$(lastlog codex)" "stage=run status=disabled"
  bash "$OC" ses_i1 2>/dev/null
  ok "i1 opencode [file MEMOIRE_CAPTURE=$off]: disabled" has "$(lastlog opencode)" "status=disabled"
  sleep 0.2; ok "i1 [file MEMOIRE_CAPTURE=$off]: the reader was never called" test "$(stubcalls)" -eq 0
done
# last assignment wins: a later MEMOIRE_CAPTURE=on in the file lifts it
newcase "${ON_ALL}MEMOIRE_CAPTURE=off\nMEMOIRE_CAPTURE=on\n"; TR="$SCRATCH/t-i1b.jsonl"; mk_base "$TR" "$SID18"
bash "$CL" --run "$TR" s1 2>/dev/null
ok "i1: a later MEMOIRE_CAPTURE=on in the file lifts the off (last wins)" test "$(stubcalls)" -eq 1
# OpenCode: the file flips to off during the settle
newcase "$ON_ALL"; export MEMOIRE_OPENCODE_TRIGGER_SETTLE_S=1 MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S=0
bash "$OC" ses_i1c 2>/dev/null & A=$!; sleep 0.3; printf 'MEMOIRE_CAPTURE=off\n' >> "$MEMOIRE_HOME/capture-tick.env"; wait "$A"
ok "i1 opencode: MEMOIRE_CAPTURE=off written in the file during the settle stops the read" test "$(stubcalls)" -eq 0
ok "i1 opencode: ...logged disabled" has "$(lastlog opencode)" "status=disabled"
unset MEMOIRE_OPENCODE_TRIGGER_SETTLE_S MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S

# i2 (F2): the needle is in no process's argv: a jq shim and the reader stub log their argv.
SPY="$SCRATCH/spy"; mkdir -p "$SPY"; REALJQ="$(command -v jq)"
cat > "$SPY/jq" <<SPYEOF
#!/usr/bin/env bash
{ printf 'JQ'; printf ' [%s]' "\$@"; printf '\n'; } >> "\$JQ_SPY_LOG"
exec "$REALJQ" "\$@"
SPYEOF
chmod +x "$SPY/jq"
export JQ_SPY_LOG="$SCRATCH/jqspy.log"
NEEDLE_MSG='zzNEEDLEFRAGzz distinctive closing words of the turn, with enough text to be a real tail'
# Claude: final text absent for a while, then present (several polls, then a signal)
newcase "$ON_ALL"; : > "$JQ_SPY_LOG"; TR="$SCRATCH/t-i2.jsonl"; mk_base "$TR" "$SID18"
( sleep 0.3; cc_text a9 9 "$SID18" "$NEEDLE_MSG" >> "$TR" ) & LATE=$!
jq -c --arg t "$TR" --arg m "$NEEDLE_MSG" '.transcript_path=$t | .last_assistant_message=$m' "$FIX/claude-stop/$F18" | PATH="$SPY:$PATH" MEMOIRE_TRIGGER_STEP_MS=250 bash "$CL" 2>/dev/null; wait "$LATE"
wait_run claude
ok "i2 claude: the poll ran jq scans (spy sees calls)" test "$(grep -c '^JQ' "$JQ_SPY_LOG")" -ge 3
ok "i2 claude: ...and found the final text (signal)" has "$(hooklog claude)" "poll_end=signal"
ok "i2 claude: no needle fragment in any jq argv" bash -c '! grep -q "NEEDLEFRAG" "$1"' _ "$JQ_SPY_LOG"
ok "i2 claude: no needle fragment in the reader's argv" bash -c '! grep -q "NEEDLEFRAG" "$1"' _ "$STUB_LOG"
ok "i2 claude: no needle in the reader's environment" bash -c '! grep -q "needle-env=yes" "$1"' _ "$STUB_LOG"
# Codex run: initial scan and polls
newcase "$ON_ALL"; : > "$JQ_SPY_LOG"; RO="$SCRATCH/r-i2.jsonl"; mk_rollout "$RO" t1 "partial"; export MEMOIRE_TRIGGER_STEP_MS=300
( sleep 0.5; mk_rollout "$RO.x" t1 "$NEEDLE_MSG"; tail -n 1 "$RO.x" >> "$RO" ) & LATE=$!
cx_payload "$RO" t1 "$NEEDLE_MSG" | PATH="$SPY:$PATH" bash "$CX" 2>/dev/null; wait_run codex; wait "$LATE"
ok "i2 codex: the scans ran jq (spy sees calls)" test "$(grep -c '^JQ' "$JQ_SPY_LOG")" -ge 3
ok "i2 codex: ...and found the tail (closing=present)" has "$(lastlog codex)" "closing=present"
ok "i2 codex: no needle fragment in any jq argv" bash -c '! grep -q "NEEDLEFRAG" "$1"' _ "$JQ_SPY_LOG"
ok "i2 codex: no needle fragment in the reader's argv" bash -c '! grep -q "NEEDLEFRAG" "$1"' _ "$STUB_LOG"
ok "i2 codex: no needle in the reader's environment (M2 of the acceptance review)" bash -c '! grep -q "needle-env=yes" "$1"' _ "$STUB_LOG"
ok "i2 codex: no needle in the log" bash -c '! grep -q "NEEDLEFRAG" "$1"' _ "$MEMOIRE_HOME/log/trigger-codex.log"
unset MEMOIRE_TRIGGER_STEP_MS

# i3 (F3): a reader that fell back to the archive inside a run: the log says source=archive.
mkdir -p "$HOME/.local/share/opencode"; [[ -f "$LIVE" ]] || : > "$LIVE"
newcase "$ON_ALL"; export MEMOIRE_OPENCODE_TRIGGER_SETTLE_S=0 MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S=0
STUB_OUT='1\t4\t0\t0\t0\t0\t0\tarchive\n' bash "$OC" ses_i3 2>/dev/null
ok "i3 opencode: reader summary source=archive -> log says source=archive" has "$(lastlog opencode)" "source=archive"
ok "i3 opencode: ...and not source=live" bash -c '! grep -q "source=live" <<<"$1"' _ "$(lastlog opencode)"
newcase "$ON_ALL"; STUB_OUT='1\t4\t0\t0\t0\t0\t0\tprimary\n' bash "$OC" ses_i3b 2>/dev/null
ok "i3 opencode: reader summary source=primary -> source=live" has "$(lastlog opencode)" "source=live"
unset MEMOIRE_OPENCODE_TRIGGER_SETTLE_S MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S

# i4 (F4): an idle that arrives after the trailing run's marker check (the window between the last check and
# the lock release, which a marker touched during the trailing run shares) gets its run.
newcase "$ON_ALL"; export MEMOIRE_OPENCODE_TRIGGER_SETTLE_S=0 MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S=1
cat > "$SCRATCH/bin/reader-late" <<'STUB'
#!/usr/bin/env bash
# runs 1 and 2: an idle arrives (marker); run 3 must follow the release
{ printf '%s start=%s args:' "$(date +%s%3N)" "$$"; printf ' %s' "$@"; printf '\n'; } >> "$STUB_LOG"
n="$(grep -c 'args:' "$STUB_LOG")"
(( n <= 2 )) && touch "$MARKER"
exit 0
STUB
chmod +x "$SCRATCH/bin/reader-late"
export MARKER="$MEMOIRE_HOME/hooks/oc-ses_i4.pending" MEMOIRE_CAPTURE_OPENCODE_BIN="$SCRATCH/bin/reader-late"
bash "$OC" ses_i4 2>/dev/null
ok "i4 opencode: an idle after the last marker check gets a run (3 runs, got $(stubcalls))" test "$(stubcalls)" -eq 3
ok "i4 opencode: the marker is consumed" test ! -e "$MARKER"
ok "i4 opencode: the log says the wrapper looped back" has "$(lastlog opencode)" "relock=1"
# bounded: a reader that re-marks on every run does not keep the wrapper for ever
newcase "$ON_ALL"; export MEMOIRE_OPENCODE_TRIGGER_SETTLE_S=0 MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S=0
cat > "$SCRATCH/bin/reader-storm" <<'STUB'
#!/usr/bin/env bash
{ printf 'args:'; printf ' %s' "$@"; printf '\n'; } >> "$STUB_LOG"
touch "$MARKER"
STUB
chmod +x "$SCRATCH/bin/reader-storm"
export MARKER="$MEMOIRE_HOME/hooks/oc-ses_i4b.pending" MEMOIRE_CAPTURE_OPENCODE_BIN="$SCRATCH/bin/reader-storm"
timeout 30 bash "$OC" ses_i4b 2>/dev/null; rc=$?
ok "i4 opencode: a perpetual storm is bounded (exit $rc, $(stubcalls) runs, at most 4)" test "$rc" -eq 0 -a "$(stubcalls)" -le 4 -a "$(stubcalls)" -ge 1
ok "i4 opencode: ...and leaves the marker for the next idle or the tick" has "$(lastlog opencode)" "pending-left=1"
unset MARKER MEMOIRE_OPENCODE_TRIGGER_SETTLE_S MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S

# i5 (F5): the Claude poll bound is on the clock, and no knob can push the hook past the limit.
newcase "$ON_ALL"; TR="$SCRATCH/t-i5.jsonl"; mk_base "$TR" "$SID18"
( for i in $(seq 1 40); do sleep 0.1; cc_tool "x$i" 10 "$SID18" >> "$TR"; done ) & BUSY=$!
stop_payload "$F18" "$TR" | MEMOIRE_TRIGGER_STEP_MS=400 bash "$CL" 2>/dev/null; wait "$BUSY"
PMS="$(hooklog claude | sed -n 's/.* poll_ms=\([0-9]*\).*/\1/p')"
ok "i5 claude: busy file, step 400 ms: poll_ms=$PMS within 1450..1600 (the last step is cut to the bound)" test "$PMS" -le 1600 -a "$PMS" -ge 1450
# absurd knobs, busy file and a slow reader
newcase "$ON_ALL"; TR="$SCRATCH/t-i5b.jsonl"; mk_base "$TR" "$SID18"
( for i in $(seq 1 70); do sleep 0.1; cc_tool "x$i" 10 "$SID18" >> "$TR"; done ) & BUSY=$!
export STUB_SLEEP=8
s="$(now_ms)"; stop_payload "$F18" "$TR" | MEMOIRE_TRIGGER_POLL_MS=999999 MEMOIRE_TRIGGER_WAIT_MS=999999 MEMOIRE_TRIGGER_STEP_MS=1 bash "$CL" 2>/dev/null; m=$(( $(now_ms) - s ))
wait "$BUSY"; unset STUB_SLEEP
echo "i5: absurd knobs, claude hook ${m} ms (limit ${CL_WORST_LIMIT})" >&2
ok "i5 claude: POLL/WAIT at 999999 ms, STEP 1 ms: the hook ends in $m ms, limit $CL_WORST_LIMIT ms" test "$m" -lt "$CL_WORST_LIMIT"
ok "i5 claude: the poll stayed at its cap" test "$(hooklog claude | sed -n 's/.* poll_ms=\([0-9]*\).*/\1/p')" -le 1600
ok "i5 claude: step floor (polls at most 15 at 100 ms)" test "$(hooklog claude | sed -n 's/.* polls=\([0-9]*\).*/\1/p')" -le 15

# i6 (F6): the cleanup kills this test's own processes (session, or token in the environment) and not a
# process of the caller whose command line holds the scratch path.
env -u CAPTURE_TRIGGER_TEST_TOKEN setsid bash -c 'echo $$ > "$1"; sleep 61; true' _ "$SCRATCH/pid-a" "$SCRATCH/sentinel-a" </dev/null >/dev/null 2>&1 &
env -u CAPTURE_TRIGGER_TEST_TOKEN setsid bash -c 'echo $$ > "$1"; exec -a "sentinel-$2" sleep 62' _ "$SCRATCH/pid-b" "$SCRATCH" </dev/null >/dev/null 2>&1 &
setsid bash -c 'echo $$ > "$1"; exec sleep 63' _ "$SCRATCH/pid-t" </dev/null >/dev/null 2>&1 &   # detached run: own session, token inherited
sleep 64 & OWN=$!
sleep 0.5
SENT_A="$(cat "$SCRATCH/pid-a" 2>/dev/null)"; SENT_B="$(cat "$SCRATCH/pid-b" 2>/dev/null)"; TOK="$(cat "$SCRATCH/pid-t" 2>/dev/null)"
ok "i6: the sentinels of the caller are running before the cleanup" test -n "$SENT_A" -a -n "$SENT_B" -a -n "$TOK"
ok "i6: sentinel A is alive" kill -0 "${SENT_A:-0}"
ok "i6: sentinel B is alive" kill -0 "${SENT_B:-0}"
ok "i6: sentinel B's command line holds the scratch path" bash -c 'tr "\0" " " < /proc/$1/cmdline | grep -qF "$2"' _ "${SENT_B:-0}" "$SCRATCH"
ok "i6: the sentinel's command line holds the scratch path" bash -c 'tr "\0" " " < /proc/$1/cmdline | grep -qF "$2"' _ "$SENT_A" "$SCRATCH"
( SCRATCH=""; kill_own )
ok "i6: an empty scratch guard kills nothing" kill -0 "$OWN"
disown -a
kill_own
# A killed process may take a moment to go, or sit as a zombie until reaped (G1 of the round 2 review): wait up to
# 2 s, count State Z as dead, and say what the process looks like when it survives.
dead_within() { # dead_within <pid> <label> -> 0 when gone or zombie within 2 s
  local p="$1" label="$2" i st
  for i in $(seq 1 20); do
    kill -0 "$p" 2>/dev/null || return 0
    st="$(awk '/^State:/ {print $2}' "/proc/$p/status" 2>/dev/null)"; [[ "$st" == "Z" ]] && return 0
    sleep 0.1
  done
  echo "i6 diag ($label): pid $p state=${st:-?} token-in-env=$(tr '\0' '\n' < "/proc/$p/environ" 2>/dev/null | grep -xF "CAPTURE_TRIGGER_TEST_TOKEN=$SCRATCH" >/dev/null && echo yes || echo no) cmd=$(tr '\0' ' ' < "/proc/$p/cmdline" 2>/dev/null | cut -c1-80)" >&2
  return 1
}
ok "i6: a process of this test's own session is killed" dead_within "$OWN" own-session
ok "i6: a detached process carrying the run's token is killed" dead_within "$TOK" token
ok "i6: a caller's process whose command line holds the scratch path survives" kill -0 "$SENT_A"
ok "i6: a caller's process renamed with the scratch path survives" kill -0 "$SENT_B"
pkill -9 -P "$SENT_A" 2>/dev/null; kill -9 "$SENT_A" "$SENT_B" 2>/dev/null; wait 2>/dev/null

# i7 (F9): state files of finished sessions older than 7 days are removed at the wrapper's start, at most 50 a run.
newcase "$ON_ALL"; export MEMOIRE_OPENCODE_TRIGGER_SETTLE_S=0 MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S=0
mkdir -p "$MEMOIRE_HOME/hooks"
for i in $(seq 1 60); do for e in lock last pending; do : > "$MEMOIRE_HOME/hooks/oc-old$i.$e"; touch -d '10 days ago' "$MEMOIRE_HOME/hooks/oc-old$i.$e"; done; done
: > "$MEMOIRE_HOME/hooks/oc-young.last"; touch -d '2 days ago' "$MEMOIRE_HOME/hooks/oc-young.last"
: > "$MEMOIRE_HOME/hooks/subagent.log"; touch -d '10 days ago' "$MEMOIRE_HOME/hooks/subagent.log"
: > "$MEMOIRE_HOME/hooks/other-session.last"; touch -d '10 days ago' "$MEMOIRE_HOME/hooks/other-session.last"
bash "$OC" ses_i7 2>/dev/null
LEFT_OLD="$(find "$MEMOIRE_HOME/hooks" -name 'oc-old*' | wc -l)"
ok "i7: at most 50 old oc-* files removed in one run (left $LEFT_OLD of 180)" test "$LEFT_OLD" -eq 130
ok "i7: a file younger than 7 days is kept" test -e "$MEMOIRE_HOME/hooks/oc-young.last"
ok "i7: a file that is not oc-<sid>.* is never touched" test -e "$MEMOIRE_HOME/hooks/subagent.log" -a -e "$MEMOIRE_HOME/hooks/other-session.last"
ok "i7: the running session's own files are kept" test -e "$MEMOIRE_HOME/hooks/oc-ses_i7.last"
bash "$OC" ses_i7 2>/dev/null; bash "$OC" ses_i7 2>/dev/null; bash "$OC" ses_i7 2>/dev/null
ok "i7: later runs keep sweeping (130 -> 0 within three runs)" test "$(find "$MEMOIRE_HOME/hooks" -name 'oc-old*' | wc -l)" -eq 0
unset MEMOIRE_OPENCODE_TRIGGER_SETTLE_S MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S

wait
if (( fails > 0 )); then echo "capture-trigger-test: $fails FAILED" >&2; exit 1; fi
echo "capture-trigger-test: OK"
