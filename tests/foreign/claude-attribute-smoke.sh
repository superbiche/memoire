#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# Attribution-at-Stop smoke: the Stop hook against fake transcripts and a fake
# `claude` binary, isolated MEMOIRE_HOME + API. Each attribution runs as a
# transient systemd user unit, so this smoke NEEDS a user systemd manager and
# fails without one. Asserts:
#   1. one memory_usage event with the parsed verdicts and a stable source
#      event id; the fork called with --resume <sid> --fork-session and a
#      --session-id the worker chose and registered; MEMOIRE_ATTRIB_FORK=1 set
#      and no Anthropic token in its env (through the unit, and with the worker
#      run by hand under a polluted environment); the fork's hooks silent
#      (capture, inject and the SessionEnd checkpoint); its
#      transcript moved out of the projects tree; an id the turn was not
#      served marked unserved;
#   2. the once-per-turn governor on a second, sequential stop;
#   3. the stop_hook_active re-entry silent;
#   4. a turn with no memoire call skipped;
#   5. a FAILING fork: no event, its transcript still disposed of;
#   6. two CONCURRENT stops on one turn: one fork, one skip;
#   7. the transcript ingest skips a registered fork still in the tree, and the
#      next run sweeps a fork a killed worker left behind;
#   8. the bootstrap prints a plain Stop entry with a short timeout;
#   9. THE LIFECYCLE ORACLE, with a fake CLI that starts a child that starts a
#      grandchild which leaves the process group: (a) the unit stopped by the
#      supervisor, (b) the runtime cap expiring, (c) the worker killed
#      outright; after each, within 10 s, none of the three is alive, no event
#      was written and the fork's transcript is out of the tree or registered;
#  10. THE EXIT ORACLE: a broken setting, an unreadable worker, a broken
#      worker, a crashing CLI, no runtime directory: the hook exits 0 each time
#      and the failure is logged;
#  11. THE START TABLE, one case per cell of the table in the hook's header,
#      each asserting the hook's exit status, its elapsed time, whether the
#      unit runs AT THE MOMENT THE HOOK RETURNS, the label of any event, the
#      payload's fate and the log line: the start confirmed; refused (a client
#      that refuses, and no user manager); not answered, by a client that
#      never submits the unit; not answered, by a client that SUBMITS the unit
#      to the real manager and then stalls; and a MEMOIRE_ATTRIB_MODE value
#      other than off, which changes nothing;
#  12. the event drained, search-visible, payload intact.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="${MEMOIRE_BIN:-$ROOT/bin/memoire-api}"
PORT="${MEMOIRE_CLAUDE_ATTRIB_PORT:-37784}"
BASE="http://127.0.0.1:$PORT/v1"
HOME_DIR="$(mktemp -d "${TMPDIR:-/tmp}/memoire-claude-attrib.XXXXXX")"
HOOK="$ROOT/scripts/memoire-attribute-claude.sh"
LOG="$HOME_DIR/hooks/attrib.log"
RUNTIME="${XDG_RUNTIME_DIR:-}/memoire-attrib"
PID=""

cleanup() {
  if [[ -n "${PID:-}" ]] && kill -0 "$PID" 2>/dev/null; then
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  # no unit, no process and no payload of this smoke may outlive it
  if [[ -f "$LOG" ]]; then
    sed -n 's/.* unit \(memoire-attrib-[A-Za-z0-9]*\) starting .*/\1/p' "$LOG" | while IFS= read -r u; do
      systemctl --user stop "$u" 2>/dev/null || true
      rm -f "$RUNTIME/$u.json"
    done
  fi
  if [[ -f "$HOME_DIR/fake-tree" ]]; then
    while IFS= read -r p; do kill -9 "$p" 2>/dev/null || true; done < "$HOME_DIR/fake-tree"
  fi
  if [[ -n "${SMOKE_KEEP:-}" ]]; then echo "kept: $HOME_DIR" >&2; else rm -rf "$HOME_DIR"; fi
}
trap cleanup EXIT

fail() { echo "Memoire Claude attribution smoke failed: $*" >&2; cat "$LOG" 2>/dev/null >&2 || true; exit 1; }

wait_ready() {
  for _ in $(seq 1 50); do
    if curl -fsS "$BASE/health" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  echo "Memoire API did not become ready" >&2
  cat "$HOME_DIR/server.log" >&2 || true
  exit 1
}

[[ -x "$BIN" ]] || { echo "Binary not found or not executable: $BIN (run: make build)" >&2; exit 1; }
REAL_SYSTEMD_RUN="$(command -v systemd-run)" || { echo "systemd-run not found" >&2; exit 1; }
"$REAL_SYSTEMD_RUN" --user --collect --quiet --wait -- "$(command -v true)" 2>/dev/null \
  || { echo "Memoire Claude attribution smoke needs a user systemd manager (systemd-run --user): none answered" >&2; exit 1; }

MEMOIRE_ADDR="127.0.0.1:$PORT" MEMOIRE_HOME="$HOME_DIR" MEMOIRE_DB="$HOME_DIR/memoire.db" \
  MEMOIRE_SCHEMA_DIR="$ROOT/schema" "$BIN" > "$HOME_DIR/server.log" 2>&1 &
PID="$!"
wait_ready

# --- fixtures -----------------------------------------------------------------
PROJ_DIR="$HOME_DIR/projects/-fake-project"
mkdir -p "$PROJ_DIR" "$HOME_DIR/fakebin"
REPLY="The webhook arrived before the cron overwrote the status [123456]; the placeholder id is kept."
ATTRIB="$HOME_DIR/hooks/attrib"
printf '%s\n' "$ROOT" > "$HOME_DIR/fake-root"
printf '%s\n' "$PROJ_DIR" > "$HOME_DIR/fake-projdir"

# write_transcript <session-id> : a turn that consulted memory
write_transcript() {
  local sid="$1" t="$PROJ_DIR/$1.jsonl"
  jq -nc --arg sid "$sid" '{type:"user",uuid:"u-prompt-1",sessionId:$sid,isMeta:false,origin:{kind:"human"},message:{role:"user",content:"why did the status flip?"}}' > "$t"
  jq -nc --arg sid "$sid" '{type:"assistant",uuid:"a-1",sessionId:$sid,message:{role:"assistant",model:"claude-opus-5-5",content:[{type:"tool_use",id:"toolu_1",name:"mcp__memoire__memoire_search",input:{q:"status flip"}}]}}' >> "$t"
  jq -nc --arg sid "$sid" '{type:"user",uuid:"u-2",sessionId:$sid,isMeta:false,message:{role:"user",content:[{type:"tool_result",tool_use_id:"toolu_1",content:"[123456] gotcha 2026-01-01: webhook arrived before the cron overwrite\n[123457] fact 2026-01-02: placeholder ids are kept on cancel"}]}}' >> "$t"
  jq -nc --arg sid "$sid" --arg reply "$REPLY" '{type:"assistant",uuid:"a-2",sessionId:$sid,message:{role:"assistant",model:"claude-opus-5-5",content:[{type:"text",text:$reply}]}}' >> "$t"
}

# Fake claude: records argv and env, counts its calls, writes the fork's
# transcript beside the parent under the --session-id it was given (as the real
# --fork-session does), and answers with a fenced block. It runs inside the
# unit, so it takes its mode from the file fake-mode, not from the environment:
#   fail      write the transcript, exit 1
#   crash     write the transcript, die on SIGSEGV
#   sleep N   answer after N seconds
#   tree      start a child that starts a grandchild in its own session, all
#             sleeping; pids in fake-tree (one per line: cli, child, grandchild)
cat > "$HOME_DIR/fakebin/claude" <<'FAKE'
#!/usr/bin/env bash
D="$(cd "$(dirname "$0")/.." && pwd)"
ROOT="$(cat "$D/fake-root")"
PROJ_DIR="$(cat "$D/fake-projdir")"
mode="$(cat "$D/fake-mode" 2>/dev/null || true)"
printf '%s\n' "$@" > "$D/fake-argv"
echo call >> "$D/fake-calls"
env | grep -E '^(MEMOIRE_ATTRIB_FORK|ANTHROPIC_API_KEY|ANTHROPIC_AUTH_TOKEN|CLAUDECODE)=' > "$D/fake-env" || true
fork_id=""
prev=""
for a in "$@"; do [[ "$prev" == "--session-id" ]] && fork_id="$a"; prev="$a"; done
[[ -n "$fork_id" ]] || { echo "no --session-id given" >&2; exit 8; }
printf '{"type":"mode","sessionId":"%s"}\n' "$fork_id" > "$PROJ_DIR/$fork_id.jsonl"
case "$mode" in
  fail) echo "simulated failure" >&2; exit 1 ;;
  crash) kill -SEGV $$ ;;
  sleep\ *) sleep "${mode#sleep }" ;;
  tree)
    D="$D" bash -c 'setsid sleep 300 & echo $! > "$D/fake-grandchild"; echo $$ > "$D/fake-child"; wait' &
    for _ in $(seq 1 50); do [[ -s "$D/fake-grandchild" && -s "$D/fake-child" ]] && break; sleep 0.1; done
    { echo $$; cat "$D/fake-child" "$D/fake-grandchild"; } > "$D/fake-tree"
    wait
    exit 0 ;;
esac
# The fork's own hooks must stay silent: run the capture and inject hooks as the fork would.
# The inject call sets MEMOIRE_INJECT=on so only the fork guard can keep it silent.
printf '{"session_id":"%s","cwd":"%s","hook_event_name":"UserPromptSubmit","prompt":"zzforkpromptleak"}' "$fork_id" "$ROOT" | MEMOIRE_HOME="$D" MEMOIRE_CLAUDE_STRICT=1 "$ROOT/scripts/memoire-capture-claude.sh" --project memoire-attrib-smoke > /dev/null
inject_out="$(printf '{"session_id":"%s","cwd":"%s","hook_event_name":"SessionStart"}' "$fork_id" "$ROOT" | MEMOIRE_HOME="$D" MEMOIRE_INJECT=on MEMOIRE_INJECT_CONTENT=off MEMOIRE_BASE_URL=http://127.0.0.1:9 "$ROOT/scripts/memoire-inject-claude.sh")"
[[ -z "$inject_out" ]] || { echo "inject hook spoke inside the fork" >&2; exit 9; }
# The fork's end is a checkpoint like any session's: run the checkpoint hook as the fork
# would. It gets a home of its own, which it creates before anything else it does.
printf '{"session_id":"%s","cwd":"%s","transcript_path":"%s","hook_event_name":"SessionEnd","reason":"other"}' "$fork_id" "$ROOT" "$PROJ_DIR/$fork_id.jsonl" | MEMOIRE_HOME="$D/fork-checkpoint-home" MEMOIRE_AUTO_DERIVE=1 MEMOIRE_BASE_URL=http://127.0.0.1:9 "$ROOT/scripts/memoire-derive-auto.sh" > /dev/null
jq -nc --arg sid "$fork_id" '{type:"result",is_error:false,session_id:$sid,total_cost_usd:0.12,duration_ms:1500,result:"Two corrections first.\n\nClaim 1 came from the runbook (docs/runbook.md). Claim 2 is a Memoire hit.\n\n```memoire\n[123456] used: the webhook arrived before the cron overwrote the status\n[123457] unused: placeholder ids kept on cancel, not relied on\n[555555] used: an id this turn was never served\n```\n\nClaims resting on Memoire alone:\n- memory-only: none\n- unverified:\n  - the webhook ordering, not checked against the cron code yet\n"}'
FAKE
chmod +x "$HOME_DIR/fakebin/claude"
fake_mode() { printf '%s\n' "${1:-}" > "$HOME_DIR/fake-mode"; }
fake_mode ""

payload() {
  jq -nc --arg sid "$1" --arg tp "$PROJ_DIR/$1.jsonl" --arg cwd "$ROOT" --arg reply "$REPLY" --argjson active "$2" \
    '{session_id:$sid,transcript_path:$tp,cwd:$cwd,hook_event_name:"Stop",stop_hook_active:$active,last_assistant_message:$reply}'
}
# run_hook <session-id> <stop_hook_active> [env assignments...]: the hook alone; it
# returns once the unit is started. HOOK_RC and HOOK_MS hold its exit status and
# its elapsed time. The Anthropic key and the harness marker are in the HOOK's
# environment: neither may reach the unit.
HOOK_RC=0 HOOK_MS=0
run_hook() {
  local sid="$1" active="$2" t0; shift 2
  HOOK_RC=0
  t0="$(date +%s%N)"
  payload "$sid" "$active" |
  env MEMOIRE_HOME="$HOME_DIR" MEMOIRE_PROJECT=memoire-attrib-smoke \
    MEMOIRE_CLAUDE_BIN="$HOME_DIR/fakebin/claude" \
    ANTHROPIC_API_KEY=smoke-dummy-must-be-stripped CLAUDECODE=1 "$@" \
    "$HOOK" || HOOK_RC=$?
  HOOK_MS=$(( ($(date +%s%N) - t0) / 1000000 ))
}
calls() { [[ -f "$HOME_DIR/fake-calls" ]] && wc -l < "$HOME_DIR/fake-calls" || echo 0; }
# forks_in_tree: transcripts in the projects tree that are not one of ours
forks_in_tree() { find "$PROJ_DIR" -name '*.jsonl' ! -name 'claude-attrib-smoke-*' | wc -l; }
# units_of <session-id>: every unit the hook logged for that session, oldest first
units_of() { [[ -f "$LOG" ]] && sed -n "s/.* unit \(memoire-attrib-[A-Za-z0-9]*\) starting .* session=$1\$/\1/p" "$LOG" || true; }
unit_of() { units_of "$1" | tail -1; }
unit_running() { case "$(systemctl --user is-active "$1" 2>/dev/null || true)" in active|activating|deactivating) return 0 ;; *) return 1 ;; esac; }
wait_unit_gone() { for _ in $(seq 1 150); do unit_running "$1" || return 0; sleep 0.1; done; return 1; }
payload_of() { [[ -e "$RUNTIME/$1.json" ]]; }
# attribute <session-id> <stop_hook_active> [env...]: the hook, then the end of every
# unit it started for that session
attribute() {
  local sid="$1" u
  run_hook "$@"
  [[ "$HOOK_RC" == 0 ]] || fail "the hook exited $HOOK_RC for $sid"
  while IFS= read -r u; do
    [[ -z "$u" ]] || wait_unit_gone "$u" || fail "unit $u of $sid did not end"
  done < <(units_of "$sid")
}

# --- 1. the attribution ---------------------------------------------------------
SID="claude-attrib-smoke-session"
write_transcript "$SID"
attribute "$SID" false
JOURNAL="$ATTRIB/$SID.jsonl"
[[ -f "$JOURNAL" ]] || fail "no journal written"
grep -q '"event": *"ask"' "$JOURNAL" || fail "no ask journaled"
grep -q '"event": *"record"' "$JOURNAL" || fail "no record journaled"
grep -q '"queue_exit": *0' "$JOURNAL" || fail "queue write did not exit 0: $(tail -1 "$JOURNAL")"
UNIT1="$(unit_of "$SID")"
[[ -n "$UNIT1" ]] || fail "the hook did not log the unit it started"
grep -q "\"unit\": *\"$UNIT1\"" "$JOURNAL" || fail "the ask does not name its unit"
grep -q '"mode": *"background"' "$JOURNAL" || fail "background label not recorded"
payload_of "$UNIT1" && fail "the payload file outlived its unit"
grep -qx -- '--fork-session' "$HOME_DIR/fake-argv" || fail "fork not called with --fork-session"
grep -A1 -x -- '--resume' "$HOME_DIR/fake-argv" | grep -qx "$SID" || fail "fork not resumed on the session id"
grep -A1 -x -- '--model' "$HOME_DIR/fake-argv" | grep -qx 'claude-opus-5-5' || fail "fork model not taken from the transcript"
FORK_ID="$(grep -A1 -x -- '--session-id' "$HOME_DIR/fake-argv" | tail -1)"
[[ "$FORK_ID" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] || fail "fork session id is not a uuid: $FORK_ID"
grep -qx "$FORK_ID" "$ATTRIB/fork-ids" || fail "fork id not registered"
grep -q '^MEMOIRE_ATTRIB_FORK=1$' "$HOME_DIR/fake-env" || fail "MEMOIRE_ATTRIB_FORK not set in the fork env"
grep -q '^ANTHROPIC_API_KEY=' "$HOME_DIR/fake-env" && fail "ANTHROPIC_API_KEY reached the fork env"
grep -q '^CLAUDECODE=' "$HOME_DIR/fake-env" && fail "CLAUDECODE reached the fork env"
[[ -f "$ATTRIB/forks/$FORK_ID.jsonl" ]] || fail "fork transcript not moved out of the projects tree"
[[ "$(forks_in_tree)" == 0 ]] || fail "a fork transcript is still beside the parent"
[[ "$(calls)" == 1 ]] || fail "expected one fork call, got $(calls)"
[[ ! -e "$HOME_DIR/fork-checkpoint-home" ]] || fail "the checkpoint hook ran inside the fork (capture or derive intent for a session the store never receives)"

# --- 1b. the worker strips the token on its own (run by hand, polluted env) ------
SID1B="claude-attrib-smoke-direct"
write_transcript "$SID1B"
payload "$SID1B" false > "$HOME_DIR/direct-payload.json"
env MEMOIRE_HOME="$HOME_DIR" MEMOIRE_PROJECT=memoire-attrib-smoke MEMOIRE_CLAUDE_BIN="$HOME_DIR/fakebin/claude" \
  ANTHROPIC_API_KEY=smoke-dummy-must-be-stripped ANTHROPIC_AUTH_TOKEN=smoke-dummy-must-be-stripped CLAUDECODE=1 \
  python3 "$ROOT/scripts/memoire-attribute-claude.py" "$HOME_DIR/direct-payload.json" >> "$LOG" 2>&1 || fail "the worker run by hand failed"
[[ "$(calls)" == 2 ]] || fail "the worker run by hand did not fork"
grep -q '^MEMOIRE_ATTRIB_FORK=1$' "$HOME_DIR/fake-env" || fail "MEMOIRE_ATTRIB_FORK not set by the worker"
grep -qE '^(ANTHROPIC_API_KEY|ANTHROPIC_AUTH_TOKEN|CLAUDECODE)=' "$HOME_DIR/fake-env" && fail "the worker let a token or the harness marker through"
grep -q '"mode": *"direct"' "$ATTRIB/$SID1B.jsonl" || fail "a worker run by hand is not labelled direct"

# --- 2. governor: a second stop in the same turn asks nothing --------------------
attribute "$SID" false
[[ "$(calls)" == 2 ]] || fail "fork called twice in the same turn"
grep -q 'already asked this turn' "$JOURNAL" || fail "governor skip not journaled"

# --- 3. re-entry after another hook blocked: silent ----------------------------
before="$(wc -l < "$JOURNAL")"
units_before="$(units_of "$SID" | wc -l)"
attribute "$SID" true
[[ "$(wc -l < "$JOURNAL")" == "$before" ]] || fail "stop_hook_active re-entry touched the journal"
[[ "$(units_of "$SID" | wc -l)" == "$units_before" ]] || fail "stop_hook_active re-entry started a unit"

# --- 4. a turn with no memoire call: skip, no fork -------------------------------
SID2="claude-attrib-smoke-nocall"
jq -nc --arg sid "$SID2" '{type:"user",uuid:"u-x",sessionId:$sid,origin:{kind:"human"},message:{role:"user",content:"hi"}}' > "$PROJ_DIR/$SID2.jsonl"
jq -nc --arg sid "$SID2" '{type:"assistant",uuid:"a-x",sessionId:$sid,message:{role:"assistant",model:"claude-opus-5-5",content:[{type:"text",text:"hello"}]}}' >> "$PROJ_DIR/$SID2.jsonl"
attribute "$SID2" false
grep -q 'no memoire call' "$ATTRIB/$SID2.jsonl" || fail "no-memoire-call skip not journaled"
[[ "$(calls)" == 2 ]] || fail "fork called on a turn with no memoire call"

# --- 5. a failing fork: no event, its transcript still disposed of ---------------
SID3="claude-attrib-smoke-failing"
write_transcript "$SID3"
fake_mode fail
attribute "$SID3" false
[[ "$(calls)" == 3 ]] || fail "failing fork was not attempted"
grep -q '"fork_error"' "$ATTRIB/$SID3.jsonl" || fail "fork failure not journaled"
grep -q '"queue_exit"' "$ATTRIB/$SID3.jsonl" && fail "a failed fork wrote an event"
[[ "$(forks_in_tree)" == 0 ]] || fail "the failed fork's transcript is still in the projects tree"
FAIL_FORK_ID="$(grep -A1 -x -- '--session-id' "$HOME_DIR/fake-argv" | tail -1)"
[[ -f "$ATTRIB/forks/$FAIL_FORK_ID.jsonl" ]] || fail "the failed fork's transcript was not moved"
# a failed turn stays claimed: no second billable attempt
fake_mode ""
attribute "$SID3" false
[[ "$(calls)" == 3 ]] || fail "a failed turn was retried"

# --- 6. two concurrent stops on one turn: one fork, one skip ---------------------
SID4="claude-attrib-smoke-concurrent"
write_transcript "$SID4"
fake_mode "sleep 1"
( run_hook "$SID4" false; exit "$HOOK_RC" ) &
h1=$!
( run_hook "$SID4" false; exit "$HOOK_RC" ) &
h2=$!
wait "$h1" || fail "a concurrent hook exited non-zero"
wait "$h2" || fail "a concurrent hook exited non-zero"
[[ "$(units_of "$SID4" | wc -l)" == 2 ]] || fail "concurrent stops did not start two units"
while IFS= read -r u; do wait_unit_gone "$u" || fail "concurrent unit $u did not end"; done < <(units_of "$SID4")
fake_mode ""
[[ "$(calls)" == 4 ]] || fail "concurrent stops made $(( $(calls) - 3 )) fork calls, want 1"
[[ "$(grep -c '"event": *"ask"' "$ATTRIB/$SID4.jsonl")" == 1 ]] || fail "concurrent stops journaled more than one ask"
grep -q 'already asked this turn' "$ATTRIB/$SID4.jsonl" || fail "the losing concurrent stop did not skip"

# --- 7. the transcript ingest skips a registered fork still in the tree ----------
INFLIGHT="99999999-9999-4999-8999-999999999999"
printf '{"type":"mode","sessionId":"%s"}\n' "$INFLIGHT" > "$PROJ_DIR/$INFLIGHT.jsonl"
echo "$INFLIGHT" >> "$ATTRIB/fork-ids"
ingest_err="$(MEMOIRE_HOME="$HOME_DIR" "$ROOT/scripts/memoire-capture-claude-transcripts.sh" --dry-run "$PROJ_DIR/$INFLIGHT.jsonl" 2>&1 >/dev/null || true)"
grep -q 'skipped 1 attribution fork transcript' <<<"$ingest_err" || fail "ingest did not skip a registered fork given by path: $ingest_err"
ingest_err="$(MEMOIRE_HOME="$HOME_DIR" "$ROOT/scripts/memoire-capture-claude-transcripts.sh" --dry-run "$PROJ_DIR" 2>&1 >/dev/null || true)"
grep -q 'skipped 1 attribution fork transcript' <<<"$ingest_err" || fail "ingest did not skip a registered fork in a scanned tree: $ingest_err"
rm -f "$PROJ_DIR/$INFLIGHT.jsonl"

# --- 7b. a fork left behind by a killed worker is swept by the next run -----------
STALE="88888888-8888-4888-8888-888888888888"
printf '{"type":"mode","sessionId":"%s"}\n' "$STALE" > "$PROJ_DIR/$STALE.jsonl"
touch -d '1 hour ago' "$PROJ_DIR/$STALE.jsonl"
echo "$STALE" >> "$ATTRIB/fork-ids"
attribute "$SID2" false
[[ -f "$ATTRIB/forks/$STALE.jsonl" && ! -e "$PROJ_DIR/$STALE.jsonl" ]] || fail "stale registered fork transcript not swept"

# --- 8. the bootstrap prints a plain Stop entry ------------------------------------
"$ROOT/scripts/memoire-bootstrap.sh" hooks-claude | sed -n '/^{/,/^}/p' |
  jq -e '.hooks.Stop[0].hooks[0] | (has("async") | not) and .timeout <= 30 and (.command | test("^/.*memoire-attribute-claude.sh$"))' >/dev/null \
  || fail "bootstrap does not print a plain Stop entry with a short timeout"
[[ "$(calls)" == 4 ]] || fail "expected four fork calls before the lifecycle cases, got $(calls)"

# --- 9. the lifecycle oracle ------------------------------------------------------
tree_alive() {
  local n=0 p
  while IFS= read -r p; do kill -0 "$p" 2>/dev/null && n=$((n + 1)); done < "$HOME_DIR/fake-tree"
  echo "$n"
}
# start_tree <session-id> [env assignments...]: an attribution whose fork holds a
# child and a grandchild (own session); returns once the three are running
start_tree() {
  local sid="$1"; shift
  write_transcript "$sid"
  rm -f "$HOME_DIR/fake-tree" "$HOME_DIR/fake-child" "$HOME_DIR/fake-grandchild"
  fake_mode tree
  run_hook "$sid" false "$@"
  [[ "$HOOK_RC" == 0 ]] || fail "the hook exited $HOOK_RC ($sid)"
  for _ in $(seq 1 100); do [[ -s "$HOME_DIR/fake-tree" ]] && break; sleep 0.1; done
  [[ -s "$HOME_DIR/fake-tree" ]] || fail "the fork's tree did not start ($sid)"
  [[ "$(wc -l < "$HOME_DIR/fake-tree")" == 3 && "$(tree_alive)" == 3 ]] || fail "the fork's tree is not three live processes ($sid)"
  TREE_UNIT="$(unit_of "$sid")"
  [[ -n "$TREE_UNIT" ]] && unit_running "$TREE_UNIT" || fail "the tree's unit is not running ($sid)"
  # every process of the tree is inside the unit's control group
  local cg p
  cg="$(systemctl --user show -p ControlGroup --value "$TREE_UNIT")"
  while IFS= read -r p; do
    grep -q "$TREE_UNIT.service" "/proc/$p/cgroup" || fail "process $p of the tree is outside the unit's control group $cg"
  done < "$HOME_DIR/fake-tree"
}
# tree_gone <case>: within 10 s
tree_gone() {
  local i
  for i in $(seq 1 100); do [[ "$(tree_alive)" == 0 ]] && return 0; sleep 0.1; done
  fail "$1: $(tree_alive) process(es) of the fork's tree alive after 10 s"
}
# after_stop <session-id> <case>: no event, transcript out of the tree or registered
after_stop() {
  local sid="$1" fid
  wait_unit_gone "$TREE_UNIT" || fail "$2: unit $TREE_UNIT still there"
  grep -q '"queue_exit"' "$ATTRIB/$sid.jsonl" && fail "$2: a stopped attribution wrote an event"
  fid="$(sed -n 's/.*"fork_session_id": *"\([0-9a-f-]*\)".*/\1/p' "$ATTRIB/$sid.jsonl" | head -1)"
  [[ -n "$fid" ]] || fail "$2: no fork id journaled"
  [[ ! -e "$PROJ_DIR/$fid.jsonl" ]] || grep -qx "$fid" "$ATTRIB/fork-ids" || fail "$2: the fork's transcript is in the tree and not registered"
  payload_of "$TREE_UNIT" && fail "$2: the payload file outlived its unit"
  fake_mode ""
}

# (a) the supervisor stops the unit
start_tree "claude-attrib-smoke-stop"
[[ "$(systemctl --user show -p KillMode --value "$TREE_UNIT")" == "control-group" ]] || fail "unit not in control-group kill mode"
[[ "$(systemctl --user show -p RuntimeMaxUSec --value "$TREE_UNIT")" == "3min 20s" ]] || fail "unit does not carry the default 200 s cap: $(systemctl --user show -p RuntimeMaxUSec --value "$TREE_UNIT")"
systemctl --user stop "$TREE_UNIT"
tree_gone "unit stopped"
after_stop "claude-attrib-smoke-stop" "unit stopped"
grep -q 'worker stopped by signal' "$ATTRIB/claude-attrib-smoke-stop.jsonl" || fail "unit stopped: the worker did not journal its stop"
[[ "$(forks_in_tree)" == 0 ]] || fail "unit stopped: the worker did not dispose of the fork's transcript"

# (b) the runtime cap expires
start_tree "claude-attrib-smoke-cap" MEMOIRE_ATTRIB_TIMEOUT=3
tree_gone "cap expired"
after_stop "claude-attrib-smoke-cap" "cap expired"
grep -q 'worker stopped by signal' "$ATTRIB/claude-attrib-smoke-cap.jsonl" || fail "cap expired: the worker did not journal its stop"

# (c) the worker is killed outright
start_tree "claude-attrib-smoke-kill"
worker_pid="$(systemctl --user show -p MainPID --value "$TREE_UNIT")"
grep -q 'memoire-attribute-claude.py' "/proc/$worker_pid/cmdline" || fail "the unit's main process is not the worker"
kill -KILL "$worker_pid"
tree_gone "worker killed"
after_stop "claude-attrib-smoke-kill" "worker killed"
# its transcript stays registered (skipped by the ingest) until swept; remove it for the counts below
find "$PROJ_DIR" -name '*.jsonl' ! -name 'claude-attrib-smoke-*' -delete

# --- 10. the exit oracle: the hook exits 0 and says why ---------------------------
# exit_zero <session-id> [env assignments...]
exit_zero() {
  local sid="$1"; shift
  write_transcript "$sid"
  attribute "$sid" false "$@"
}
exit_zero "claude-attrib-smoke-badcap" MEMOIRE_ATTRIB_TIMEOUT=not-a-number
grep -q 'invalid MEMOIRE_ATTRIB_TIMEOUT (not-a-number)' "$LOG" || fail "broken setting not logged"
grep -q '"queue_exit": *0' "$ATTRIB/claude-attrib-smoke-badcap.jsonl" || fail "broken setting: the attribution did not run on the default cap"

printf 'raise SystemExit(3)\n' > "$HOME_DIR/unreadable-worker.py"
chmod 000 "$HOME_DIR/unreadable-worker.py"
n="$(calls)"
exit_zero "claude-attrib-smoke-noworker" MEMOIRE_ATTRIB_WORKER="$HOME_DIR/unreadable-worker.py"
grep -q 'skip: worker not found or not readable' "$LOG" || fail "unreadable worker not logged"
[[ -z "$(unit_of claude-attrib-smoke-noworker)" ]] || fail "unreadable worker: a unit was started"
[[ "$(calls)" == "$n" ]] || fail "unreadable worker: a fork ran"

printf 'import sys\nsys.exit(int("not-a-number"))\n' > "$HOME_DIR/broken-worker.py"
exit_zero "claude-attrib-smoke-brokenworker" MEMOIRE_ATTRIB_WORKER="$HOME_DIR/broken-worker.py"
grep -q 'ValueError' "$LOG" || fail "broken worker: its error is not in the log"
payload_of "$(unit_of claude-attrib-smoke-brokenworker)" && fail "broken worker: the unit did not remove the payload the worker never read"

fake_mode crash
exit_zero "claude-attrib-smoke-crash"
fake_mode ""
grep -q '"fork_error"' "$ATTRIB/claude-attrib-smoke-crash.jsonl" || fail "crashing CLI: fork failure not journaled"
grep -q '"queue_exit"' "$ATTRIB/claude-attrib-smoke-crash.jsonl" && fail "crashing CLI: an event was written"
[[ "$(forks_in_tree)" == 0 ]] || fail "crashing CLI: the fork's transcript is still in the projects tree"

n="$(calls)"
exit_zero "claude-attrib-smoke-noxdg" XDG_RUNTIME_DIR=
grep -q 'skip: no usable XDG_RUNTIME_DIR' "$LOG" || fail "missing runtime directory not logged"
[[ "$(calls)" == "$n" ]] || fail "a fork ran without a runtime directory"

# --- 11. the start table: one case per cell ---------------------------------------
# Each fake client is first on PATH for the hook only.
mkdir -p "$HOME_DIR/refuse" "$HOME_DIR/stall" "$HOME_DIR/submit-stall"
printf '#!/usr/bin/env bash\necho "fake manager: refused" >&2\nexit 1\n' > "$HOME_DIR/refuse/systemd-run"
# a hung client, with a child left behind holding its output
printf '#!/usr/bin/env bash\nsleep 60 &\nwait\n' > "$HOME_DIR/stall/systemd-run"
# a client that SUBMITS the unit to the real manager, then hangs before returning
printf '#!/usr/bin/env bash\n"%s" "$@"\nsleep 60 &\nwait\n' "$REAL_SYSTEMD_RUN" > "$HOME_DIR/submit-stall/systemd-run"
chmod +x "$HOME_DIR/refuse/systemd-run" "$HOME_DIR/stall/systemd-run" "$HOME_DIR/submit-stall/systemd-run"

# cell 1, the start confirmed: the hook returns while the unit runs; the event is background
SID11="claude-attrib-smoke-confirmed"
write_transcript "$SID11"
fake_mode "sleep 3"
run_hook "$SID11" false
[[ "$HOOK_RC" == 0 ]] || fail "start confirmed: the hook exited $HOOK_RC"
[[ "$HOOK_MS" -lt 1500 ]] || fail "start confirmed: the hook took ${HOOK_MS} ms, want under 1500 ms"
U11="$(unit_of "$SID11")"
[[ -n "$U11" ]] && unit_running "$U11" || fail "start confirmed: the unit is not running when the hook returns"
if [[ -e "$ATTRIB/$SID11.jsonl" ]] && grep -q '"event": *"record"' "$ATTRIB/$SID11.jsonl"; then
  fail "start confirmed: the attribution was over when the hook returned"
fi
wait_unit_gone "$U11" || fail "start confirmed: the unit did not end"
grep -q '"queue_exit": *0' "$ATTRIB/$SID11.jsonl" || fail "start confirmed: no event"
grep -q '"mode": *"background"' "$ATTRIB/$SID11.jsonl" || fail "start confirmed: the event is not labelled background"
payload_of "$U11" && fail "start confirmed: the payload outlived the unit"
echo "start confirmed: hook returned in ${HOOK_MS} ms, unit running" >&2
fake_mode ""

# cell 2, the start refused: by a client that refuses, and where no user manager answers
refused() { # <case> <session-id> [env...]
  local name="$1" sid="$2" n u; shift 2
  write_transcript "$sid"
  n="$(calls)"
  run_hook "$sid" false "$@"
  [[ "$HOOK_RC" == 0 ]] || fail "$name: the hook exited $HOOK_RC"
  [[ "$HOOK_MS" -lt 1500 ]] || fail "$name: the hook took ${HOOK_MS} ms, want under 1500 ms"
  u="$(unit_of "$sid")"
  [[ -n "$u" ]] || fail "$name: the hook did not log the unit it tried to start"
  grep -q "skip: unit $u not started (systemd-run exit [1-9]" "$LOG" || fail "$name: the refusal is not logged"
  unit_running "$u" && fail "$name: a unit runs though its start was refused"
  [[ "$(calls)" == "$n" && ! -e "$ATTRIB/$sid.jsonl" ]] || fail "$name: the worker ran though the start was refused"
  echo "$name: hook returned in ${HOOK_MS} ms" >&2
  REFUSED_UNIT="$u"
}
refused "start refused" "claude-attrib-smoke-refused" PATH="$HOME_DIR/refuse:$PATH"
grep -q 'fake manager: refused' "$LOG" || fail "start refused: the client's own message is not in the log"
payload_of "$REFUSED_UNIT" && fail "start refused: the payload was not removed"
mkdir -p "$HOME_DIR/empty-runtime"
refused "no user manager" "claude-attrib-smoke-nomanager" XDG_RUNTIME_DIR="$HOME_DIR/empty-runtime" DBUS_SESSION_BUS_ADDRESS=
grep -q 'Failed to connect' "$LOG" || fail "no user manager: the manager's own error is not in the log"
[[ -z "$(ls -A "$HOME_DIR/empty-runtime/memoire-attrib" 2>/dev/null)" ]] || fail "no user manager: a payload file was left"

# cell 3, the start not answered; first by a client that never submits the unit
SID13="claude-attrib-smoke-stall"
write_transcript "$SID13"
n="$(calls)"
run_hook "$SID13" false PATH="$HOME_DIR/stall:$PATH"
[[ "$HOOK_RC" == 0 ]] || fail "start not answered: the hook exited $HOOK_RC"
[[ "$HOOK_MS" -ge 4500 && "$HOOK_MS" -lt 7000 ]] || fail "start not answered: the hook took ${HOOK_MS} ms, want 5 s to under 7 s"
U13="$(unit_of "$SID13")"
stall_ms="$HOOK_MS"
grep -q "unit $U13 start not confirmed within 5s: not followed" "$LOG" || fail "start not answered: not logged"
unit_running "$U13" && fail "start not answered: a unit runs though none was submitted"
[[ "$(calls)" == "$n" && ! -e "$ATTRIB/$SID13.jsonl" ]] || fail "start not answered: the worker ran though no unit was submitted"
payload_of "$U13" || fail "start not answered: the payload is gone, the table says it stays for a later sweep"
# ... swept by a later run once it is an hour old
touch -d '2 hours ago' "$RUNTIME/$U13.json"
attribute "$SID2" false
payload_of "$U13" && fail "start not answered: the hour-old payload was not swept by the next run"
echo "start not answered (nothing submitted): hook returned in ${stall_ms} ms" >&2

# cell 3 again, by a client that submits the unit and then hangs: the unit is real,
# runs supervised, and its event is a background event like any other
SID14="claude-attrib-smoke-late"
write_transcript "$SID14"
fake_mode "sleep 8"
run_hook "$SID14" false PATH="$HOME_DIR/submit-stall:$PATH"
[[ "$HOOK_RC" == 0 ]] || fail "start submitted, not answered: the hook exited $HOOK_RC"
[[ "$HOOK_MS" -ge 4500 && "$HOOK_MS" -lt 7000 ]] || fail "start submitted, not answered: the hook took ${HOOK_MS} ms, want 5 s to under 7 s"
U14="$(unit_of "$SID14")"
grep -q "unit $U14 start not confirmed within 5s: not followed" "$LOG" || fail "start submitted, not answered: not logged"
unit_running "$U14" || fail "start submitted, not answered: the submitted unit is not running when the hook returns"
[[ "$(systemctl --user show -p KillMode --value "$U14")" == "control-group" ]] || fail "start submitted, not answered: the unit is not under its boundary"
[[ "$(systemctl --user show -p RuntimeMaxUSec --value "$U14")" == "3min 20s" ]] || fail "start submitted, not answered: the unit is not under its cap"
wait_unit_gone "$U14" || fail "start submitted, not answered: the unit did not end"
fake_mode ""
grep -q '"queue_exit": *0' "$ATTRIB/$SID14.jsonl" || fail "start submitted, not answered: the supervised attribution did not complete"
grep -q '"mode": *"background"' "$ATTRIB/$SID14.jsonl" || fail "start submitted, not answered: the event is not labelled background"
payload_of "$U14" && fail "start submitted, not answered: the payload outlived the unit"
echo "start not answered (unit submitted): hook returned in ${HOOK_MS} ms, unit running" >&2

# a mode value other than off changes nothing: background, and the hook does not wait
SID15="claude-attrib-smoke-modevalue"
write_transcript "$SID15"
fake_mode "sleep 3"
run_hook "$SID15" false MEMOIRE_ATTRIB_MODE=sync
[[ "$HOOK_RC" == 0 && "$HOOK_MS" -lt 1500 ]] || fail "mode value: exit $HOOK_RC in ${HOOK_MS} ms, want 0 under 1500 ms"
U15="$(unit_of "$SID15")"
unit_running "$U15" || fail "mode value: the unit is not running when the hook returns"
grep -q 'MEMOIRE_ATTRIB_MODE=sync has no effect' "$LOG" || fail "mode value: not logged"
wait_unit_gone "$U15" || fail "mode value: the unit did not end"
fake_mode ""
grep -q '"mode": *"background"' "$ATTRIB/$SID15.jsonl" || fail "mode value: the event is not labelled background"
n="$(calls)"
write_transcript "claude-attrib-smoke-modeoff"
run_hook "claude-attrib-smoke-modeoff" false MEMOIRE_ATTRIB_MODE=off
[[ "$HOOK_RC" == 0 && -z "$(unit_of claude-attrib-smoke-modeoff)" && "$(calls)" == "$n" ]] || fail "MEMOIRE_ATTRIB_MODE=off did not disable the hook"
# nothing of this smoke is left in the runtime directory
while IFS= read -r u; do
  payload_of "$u" && fail "payload of $u left in the runtime directory"
done < <(sed -n 's/.* unit \(memoire-attrib-[A-Za-z0-9]*\) starting .*/\1/p' "$LOG")

# --- 12. the event is drained and its summary line is search-visible --------------
TOKEN="$(tr -d '\r\n' < "$HOME_DIR/token")"
drained=0
for _ in $(seq 1 50); do
  if curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE/search?q=attribution&project=memoire-attrib-smoke&limit=5" | grep -q 'memory_usage'; then
    drained=1; break
  fi
  sleep 0.2
done
[[ "$drained" == 1 ]] || { ls -R "$HOME_DIR/queue" "$HOME_DIR/raw" 2>/dev/null >&2; fail "memory_usage event not drained or not search-visible"; }
grep -R -q "zzforkpromptleak" "$HOME_DIR/raw" 2>/dev/null && fail "the fork's UserPromptSubmit was captured"
# Several memory_usage events exist by now: pick the first session's by content.
raw_file=""
while IFS= read -r f; do
  if jq -e --arg sid "$SID" '.session_id == $sid' "$f" >/dev/null 2>&1; then raw_file="$f"; break; fi
done < <(grep -R -l 'claude-memory-usage-' "$HOME_DIR/raw")
[[ -n "$raw_file" ]] || fail "no memory_usage event with a stable source event id under raw/ for the first session"
grep -R -l 'claude-memory-usage-' "$HOME_DIR/raw" | while IFS= read -r f; do
  jq -e --arg sid "$SID3" '.session_id != $sid' "$f" >/dev/null || fail "the failed fork's turn has an event"
  jq -e '.session_id | test("smoke-(stop|cap|kill|crash|refused|nomanager|stall)$") | not' "$f" >/dev/null || fail "a stopped, crashed or never started attribution has an event"
  jq -e '.payload.mode == "background" or .payload.mode == "direct"' "$f" >/dev/null || fail "an event carries a mode that does not exist: $(jq -r .payload.mode "$f")"
done
jq -e '.event_type == "memory_usage"' "$raw_file" >/dev/null || fail "event type wrong"
jq -e '.payload.mode == "background" and .payload.hook_version == "4"' "$raw_file" >/dev/null || fail "mode label or hook version wrong in the event"
jq -e '.payload.parse_ok == true' "$raw_file" >/dev/null || fail "block not parsed"
jq -e '.payload.served_ids == [123456, 123457]' "$raw_file" >/dev/null || fail "served ids not recorded"
jq -e '.payload.cited_ids == [123456]' "$raw_file" >/dev/null || fail "cited ids not recorded"
jq -e '[.payload.verdicts[] | select(.verdict == "used" and .served) | .id] == [123456]' "$raw_file" >/dev/null || fail "served used verdict not parsed"
jq -e '[.payload.verdicts[] | select(.verdict == "unused") | .id] == [123457]' "$raw_file" >/dev/null || fail "unused verdict not parsed"
jq -e '.payload.ids_subset_served == false and .payload.unserved_ids == [555555]' "$raw_file" >/dev/null || fail "unserved id not flagged"
jq -e '[.payload.verdicts[] | select(.id == 555555) | .served] == [false]' "$raw_file" >/dev/null || fail "unserved verdict not marked"
jq -e '.payload.memory_only == [] and (.payload.unverified | length) == 1' "$raw_file" >/dev/null || fail "Memoire-alone groups not parsed"
jq -e --arg f "$FORK_ID" '.payload.fork.transcript == "moved" and .payload.fork.session_id == $f' "$raw_file" >/dev/null || fail "fork metadata not recorded"
jq -e '.text | test("used=1 unused=1 .*unserved=1")' "$raw_file" >/dev/null || fail "summary text wrong"
echo "memoire claude attribution smoke passed"
exit 0
