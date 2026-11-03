#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# muse-store-capture.sh - guarantee tests for the Muse Code store-sweep capture
# adapter (scripts/memoire-capture-muse.sh). The adapter walks the Muse session
# store (~/.local/share/muse/sessions/YYYY/MM/DD/<sid>/session.jsonl and
# <sid>/subagent/<cid>/session.jsonl), decodes each file with `muse export`
# and queues every logged record as one event, plus a turn projection and
# per-session facts. `make test-core` runs this script (no Memoire home, no
# store, no network, no Muse binary: a stub stands in for `muse export`), and
# so does `make muse-capture-test`.
#
#   basic        one ended root session: every line of the file is one event
#                carrying the record verbatim; mapping table; turn projection;
#                facts; cwd and project from the workspace
#   capture-all  tool calls and results, reasoning (encrypted_content kept),
#                a 20000-character multibyte reply: no truncation anywhere
#   subagent     a bound child (child_session_bound in the root) is captured
#                under its own session id with parent, subagent id and role;
#                its turn is projected
#   internal     an unbound child (a reminder child) is captured whole and
#                marked muse_internal, with no subagent id or role
#   open-turn    an open quiet session is captured up to now without a turn
#                projection; after the terminal and session.end are appended
#                the next run emits only the new records, the turn once and new
#                facts; nothing is emitted twice
#   quiet        a tree written less than --quiet-for ago is deferred without
#                an export call; once quiet it is captured
#   idempotent   two dry runs over the same store print identical lines
#   queue        a real run hands the dry-run lines to the queue writer through
#                -batch-file; a second run over the unchanged store neither
#                exports nor queues (skipped=1 in the stderr done line)
#   queue-failure  a failing queue write exits 1 and advances no cursor; the
#                next run with a working queue emits everything
#   state-failure  an unwritable cursor after a successful queue write exits 7
#   read-only    nothing under the store root is created, modified or removed
#   export-failure  a failing export fails its unit (exit 1); another unit of
#                the same run is captured
#   schema       an export_schema_version other than 1 refuses the unit
#                (exit 1, nothing emitted for it, one log line)
#   unparseable  an unparseable line becomes muse_unparseable_line with the raw
#                text; diagnostics land in the facts; exit 0
#   exclude      a root whose workspace is under an excluded prefix (direct or
#                symlinked, trailing slash) is never exported and emits
#                nothing; its log lines never name it; a sibling with a longer
#                name is captured
#   no-exclude-file  a missing exclusion file exits 3 before any export
#   capture-off  a workspace under a `.memoire` with `capture: false` emits
#                nothing; a neighbour is captured
#   kill-switch  MEMOIRE_CAPTURE=off exits 0 with no export call
#   no-muse-bin  a missing binary exits 1 when a unit is ready, 0 when nothing
#                is ready
#   window       --since leaves out a tree older than the cutoff; an explicit
#                session argument bypasses the window
#   invalid-id   a session dir whose name is not a UUID is refused (logged,
#                never exported), exit 0
#   snapshot-append  a record appended to a session file while its export runs
#                (after the exporter's snapshot) is queued by the next run: the
#                cursor fingerprint is taken before the export, never after
#   probe-bound  the pre-export workspace probe stops at 64 lines and 1 MiB: a
#                metadata record on line 64 is found, on line 65 or past the
#                first 1 MiB it is not (the unit is deferred as no_workspace,
#                never exported)
#   gap          export gap events (no id, sequence or time) become muse_gap
#                events named muse:<sid>:gap:<k>@<anchor>, the k-th gap after
#                the nearest preceding record (anchor "start" before any
#                record, a duplicate record line included; a re-appearing
#                anchor continues its count): identical markers stay distinct,
#                ids are stable across dry runs, count == diagnostics.gaps, a
#                duplicate record is not emitted twice, and a later run emits
#                only the new gaps
#   workdir      no run.* work directory survives a real or a dry run
#   contract     (only with MUSE_CONTRACT_BIN) the stub and the real binary
#                agree on the fields the adapter's behaviour depends on, over
#                the fixture store: schema version, session id, turn count,
#                session_end, every record event (kind, id, verbatim envelope,
#                derived session id, turn count and duplicate flag), every gap
#                event (verbatim), unparseable lines, and the gaps and
#                duplicate_records counts. Other diagnostic values,
#                accepted_spawns and exporter_version are
#                carried verbatim into the facts but drive no behaviour; they
#                are NOT compared (the real binary does not decode the
#                synthetic subagent records: unknown kinds, no spawns), and a
#                synthetic retained frame is rejected by the real binary.
#
# Environment (all optional):
#   MUSE_CAPTURE_ADAPTER  adapter under test (default: the repository's)
#   MUSE_CAPTURE_ONLY     space-separated test names to run (default: all)
#   MUSE_CAPTURE_KEEP     keep the scratch directory (debugging)
#   MUSE_CONTRACT_BIN     a real `muse` binary for the contract test

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ADAPTER="${MUSE_CAPTURE_ADAPTER:-$ROOT/scripts/memoire-capture-muse.sh}"
FIX="$ROOT/tests/foreign/muse-store-fixtures.py"
EXPECT="$ROOT/tests/foreign/muse-store-expect.py"
STUB="$ROOT/tests/foreign/muse-export-stub.py"
ONLY="${MUSE_CAPTURE_ONLY:-}"

command -v python3 >/dev/null || { echo "SKIP: python3 not available"; exit 0; }
[[ -f "$ADAPTER" ]] || { echo "FAIL: adapter not found: $ADAPTER"; exit 1; }

WORK="$(mktemp -d "${TMPDIR:-/var/tmp}/muse-capture-test.XXXXXX")" || { echo "FAIL: mktemp -d"; exit 1; }
trap '[[ -n "${MUSE_CAPTURE_KEEP:-}" ]] && echo "kept: $WORK" || { chmod -R u+rwx "$WORK" 2>/dev/null; rm -rf "$WORK"; }' EXIT

FAILURES=0
fail() { echo "FAIL: $1" >&2; FAILURES=$((FAILURES + 1)); }
check() { echo "ok: $1"; }
want() { [[ -z "$ONLY" || " $ONLY " == *" $1 "* ]]; }

PROJ="$WORK/muse-cap-proj"
mkdir -p "$PROJ"
cat > "$WORK/registry.json" <<JSON
{
  "schema": "liaison-memoire-projects/1",
  "projects": {
    "muse-cap-test": { "display": "muse-cap-test", "paths": ["$PROJ"] }
  }
}
JSON

mkdir -p "$WORK/bin"
# Stub queue writer: records each -batch-file it receives; QSTUB_FAIL=1 fails.
cat > "$WORK/bin/memoire-queue" <<'STUB'
#!/usr/bin/env bash
[[ -n "${QSTUB_FAIL:-}" ]] && { echo "stub: queue write failed" >&2; exit 1; }
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  if [[ "${args[i]}" == "-batch-file" || "${args[i]}" == "--batch-file" ]]; then
    n=$(ls "$QSTUB_DIR" | wc -l)
    cp "${args[i+1]}" "$QSTUB_DIR/batch-$n.jsonl"
    exit 0
  fi
done
echo "stub: expected -batch-file" >&2
exit 1
STUB
chmod +x "$WORK/bin/memoire-queue"
# Stub muse binary: `muse export` only (tests/foreign/muse-export-stub.py).
cat > "$WORK/bin/muse" <<STUB
#!/usr/bin/env bash
exec python3 -I "$STUB" "\$@"
STUB
chmod +x "$WORK/bin/muse"

R1=01a14aa4-0000-7000-8000-000000000001
R2=01a14aa4-0000-7000-8000-0000000000a2
C1=f64903bf-0000-7000-8000-000000000002
A1=01a14aa4-4802-7000-8000-000000000003
C2=19bfbe78-0000-7000-8000-000000000004

# mkenv NAME -> scratch env with sessions/, home/, mh/ (MEMOIRE_HOME), q/
# (queue stub drop) and an empty exclusion file.
mkenv() {
  E="$WORK/$1"
  mkdir -p "$E/sessions" "$E/home" "$E/mh" "$E/q" "$E/cfg/memoire"
  : > "$E/cfg/memoire/acpx-exclude"
  : > "$E/exports.log"
}
fxo() { local op="$1"; shift; python3 -I "$FIX" "$op" "$E/sessions" "$@" || fail "fixture $op $*"; }
age() { fxo touch --id "$1" --age "${2:-600}" >/dev/null; }

# run [adapter args...] -> stdout in $E/out.jsonl, stderr in $E/err, rc in RC
run() {
  HOME="$E/home" MEMOIRE_HOME="$E/mh" XDG_CONFIG_HOME="$E/cfg" \
    MEMOIRE_PROJECTS_REGISTRY="$WORK/registry.json" \
    MEMOIRE_MUSE_BIN="${MUSE_BIN_OVERRIDE:-$WORK/bin/muse}" MUSE_STUB_LOG="$E/exports.log" \
    MEMOIRE_MUSE_SESSIONS_DIR="$E/sessions" \
    MEMOIRE_QUEUE_BIN="$WORK/bin/memoire-queue" QSTUB_DIR="$E/q" \
    bash "$ADAPTER" "$@" >"$E/out.jsonl" 2>"$E/err"
  RC=$?
}

# expect NAME CHECK [ARGS...] over $E/out.jsonl
expect() {
  local name="$1"; shift
  local c="$1"; shift
  if python3 -I "$EXPECT" "$c" "$E/out.jsonl" "$@" >"$E/cmp" 2>&1; then
    check "$name"
  else
    fail "$name"; sed -n '1,8p' "$E/cmp" >&2; sed -n '1,5p' "$E/err" >&2
  fi
}
expect_file() {
  local name="$1" file="$2" c="$3"; shift 3
  if python3 -I "$EXPECT" "$c" "$file" "$@" >"$E/cmp" 2>&1; then check "$name"; else fail "$name"; sed -n '1,8p' "$E/cmp" >&2; fi
}

# basic_root SID [WORKSPACE] -> an ended root session with two turns, aged
basic_root() {
  local sid="$1" ws="${2:-$PROJ}"
  fxo new --id "$sid" --workspace "$ws" >/dev/null
  fxo turn --id "$sid" --prompt "Throwaway: name a colour" --reply "Blue." --run "run-$sid-1" >/dev/null
  fxo turn --id "$sid" --prompt "And another?" --reply "Green." --reply "Or red." --run "run-$sid-2" >/dev/null
  fxo end --id "$sid" >/dev/null
  age "$sid"
}

if want basic; then
  mkenv basic
  basic_root "$R1"
  run --scan --dry-run
  [[ $RC == 0 ]] && check "basic: rc 0" || fail "basic: rc=$RC"
  expect "basic: well-formed" common
  expect "basic: every line captured verbatim" records "$E/sessions"
  expect "basic: mapping table" typed
  expect "basic: turn 1" turns "$R1" "run-$R1-1" "Blue." "Throwaway: name a colour"
  expect "basic: turn 2 joins its messages" turns "$R1" "run-$R1-2" $'Green.\n\nOr red.' "And another?"
  expect "basic: prompt event" text "$R1" muse_user_prompt "Throwaway: name a colour"
  expect "basic: facts" facts "$R1" "workspace_root=\"$PROJ\"" 'model_id="muse-spark-1.2"' \
    'client_info_name="muse_code_acp"' 'turn_count=2' 'session_end={"exit_reason":"clean"}' \
    'muse_internal=false' 'parent_session_id=null' 'export_schema_version=1'
  expect "basic: one facts event" factscount "$R1" 1
  expect "basic: root link" link "$R1" null null null false
  expect "basic: cwd" cwd "$PROJ"
  expect "basic: project" project muse-cap-test
  grep -q "$R1/session.jsonl" "$E/exports.log" && check "basic: exported by session.jsonl path" \
    || fail "basic: export not called with the session.jsonl path"
fi

if want capture-all; then
  mkenv capture-all
  LONG="$(python3 -c 'print(("é漢🙂 lorem ipsum " * 1200)[:20000], end="")')"
  fxo new --id "$R1" --workspace "$PROJ" >/dev/null
  fxo turn --id "$R1" --prompt "Throwaway: read a file" --reasoning "Planning the read." \
    --tool 'read_file={"path":"notes.txt"}:line one' --tool 'grep={"q":"x"}:no match' \
    --reply "$LONG" --run run-ca >/dev/null
  fxo end --id "$R1" >/dev/null
  age "$R1"
  run --scan --dry-run
  expect "capture-all: every line captured verbatim" records "$E/sessions"
  expect "capture-all: mapping table (tool, reasoning texts)" typed
  expect "capture-all: tool calls text" text "$R1" muse_tool_calls $'read_file {"path":"notes.txt"}\ngrep {"q":"x"}'
  expect "capture-all: tool results text" text "$R1" muse_tool_results $'line one\n\nno match'
  expect "capture-all: reasoning text" text "$R1" muse_reasoning "Planning the read."
  expect "capture-all: long multibyte reply whole" text "$R1" muse_assistant_message "$LONG"
  expect "capture-all: turn text whole" turns "$R1" run-ca "$LONG" "Throwaway: read a file"
  grep -q 'ZmFrZS1lbmNyeXB0ZWQtcmVhc29uaW5n' "$E/out.jsonl" && check "capture-all: encrypted_content kept" \
    || fail "capture-all: encrypted_content dropped"
fi

if want subagent || want internal; then
  mkenv subagent
  fxo new --id "$R1" --workspace "$PROJ" >/dev/null
  fxo turn --id "$R1" --prompt "Throwaway: delegate a fruit list" --terminal none --run run-p1 >/dev/null
  fxo spawn --id "$R1" --child "$C1" --subagent "$A1" --role workflow-agent --reply "Peach, Pear, Plum" >/dev/null
  fxo internal --id "$R1" --child "$C2" --reply "no reminder needed" >/dev/null
  fxo turn --id "$R1" --prompt "" --reply "Subagent result: Peach, Pear, Plum" --run run-p2 >/dev/null
  fxo end --id "$R1" >/dev/null
  age "$R1"
  run --scan --dry-run
  [[ $RC == 0 ]] || fail "subagent: rc=$RC"
  expect "subagent: every line of root and children captured" records "$E/sessions"
  expect "subagent: well-formed" common
  if want subagent; then
    expect "subagent: bound child link" link "$C1" "$R1" "$A1" workflow-agent false
    expect "subagent: child turn" turns "$C1" - "Peach, Pear, Plum" "Child task: workflow-agent"
    expect "subagent: child facts" facts "$C1" "root_session_id=\"$R1\"" "parent_session_id=\"$R1\"" \
      "subagent_id=\"$A1\"" 'subagent_role="workflow-agent"' 'muse_internal=false'
    expect "subagent: child cwd is the root workspace" cwd "$PROJ"
    expect "subagent: empty-prompt run still projected" turns "$R1" run-p2 "Subagent result: Peach, Pear, Plum" ""
    expect "subagent: run without terminal not projected" noturn "$R1" run-p1
  fi
  if want internal; then
    expect "internal: unbound child marked internal" link "$C2" "$R1" null null true
    expect "internal: unbound child captured" present "$C2"
  fi
fi

if want open-turn; then
  mkenv open-turn
  fxo new --id "$R1" --workspace "$PROJ" >/dev/null
  fxo turn --id "$R1" --prompt "Throwaway: count to three" --reply "One, two," --terminal none --run run-o >/dev/null
  age "$R1"
  run --scan
  [[ $RC == 0 ]] || fail "open-turn: first run rc=$RC"
  cat "$E"/q/batch-*.jsonl > "$E/first.jsonl" 2>/dev/null || : > "$E/first.jsonl"
  expect_file "open-turn: no projection before the terminal" "$E/first.jsonl" noturn "$R1"
  expect_file "open-turn: open session captured" "$E/first.jsonl" present "$R1"
  # Append the terminal and the end: the same run id, new records.
  python3 -I - "$E/sessions" "$R1" <<'PY'
import json, os, sys, uuid
root, sid = sys.argv[1], sys.argv[2]
for d, _, f in os.walk(root):
    if os.path.basename(d) == sid:
        path = os.path.join(d, "session.jsonl")
lines = [json.loads(l) for l in open(path) if l.strip()]
seq = max(l.get("sequence", 0) for l in lines)
t0 = int(open(os.path.join(os.path.dirname(path), ".fixture-t0")).read())
def rec(n, pt, payload):
    return {"schema_version": 1, "id": str(uuid.uuid5(uuid.NAMESPACE_URL, f"{sid}:{n}")),
            "stream": {"kind": "session", "id": sid}, "sequence": n, "recorded_at": t0 + n * 1000,
            "record_type": "event", "durability": "durable", "causation_id": None,
            "payload_type": pt, "payload_schema_version": 1, "payload": payload}
new = [
    rec(seq + 1, "runtime.session", {"event": {"kind": "assistant_message_committed", "message_id": "m2", "text": "three."}, "kind": "run", "run_id": "run-o"}),
    rec(seq + 2, "runtime.session", {"event": {"kind": "terminal", "terminal": "completed", "reason": None}, "kind": "run", "run_id": "run-o"}),
    rec(seq + 3, "session.end", {"kind": "session_end", "record": {"exit_reason": "clean", "session_id": sid, "uptime_ms": 1}}),
]
with open(path, "a") as fh:
    for r in new:
        fh.write(json.dumps(r, separators=(",", ":")) + "\n")
PY
  age "$R1"
  rm -f "$E"/q/batch-*.jsonl
  run --scan
  [[ $RC == 0 ]] || fail "open-turn: second run rc=$RC"
  cat "$E"/q/batch-*.jsonl > "$E/second.jsonl" 2>/dev/null || : > "$E/second.jsonl"
  expect_file "open-turn: turn projected from the whole run" "$E/second.jsonl" turns "$R1" run-o $'One, two,\n\nthree.' "Throwaway: count to three"
  expect_file "open-turn: nothing emitted twice" "$E/first.jsonl" disjoint "$E/second.jsonl"
  n_rec=$(python3 -I -c 'import json,sys; print(sum(1 for l in open(sys.argv[1]) if json.loads(l)["event_type"] not in ("muse_assistant_turn","muse_session")))' "$E/second.jsonl")
  [[ "$n_rec" == 3 ]] && check "open-turn: only the 3 new records" || fail "open-turn: $n_rec record events in the second run (want 3)"
  cat "$E/first.jsonl" "$E/second.jsonl" > "$E/out.jsonl"
  expect "open-turn: union covers every line" records "$E/sessions"
  expect "open-turn: facts re-emitted once session.end appears" factscount "$R1" 2
  expect "open-turn: newest facts carry the end" facts "$R1" 'session_end={"exit_reason":"clean"}'
fi

if want quiet; then
  mkenv quiet
  fxo new --id "$R1" --workspace "$PROJ" >/dev/null
  fxo turn --id "$R1" --prompt "Throwaway" --reply "ok" >/dev/null
  run --scan --dry-run --quiet-for 300
  if [[ $RC == 0 && ! -s "$E/out.jsonl" && ! -s "$E/exports.log" ]]; then check "quiet: fresh tree deferred, not exported"
  else fail "quiet: rc=$RC, $(wc -l < "$E/out.jsonl") lines, $(wc -l < "$E/exports.log") exports"; fi
  age "$R1" 400
  run --scan --dry-run --quiet-for 300
  expect "quiet: captured once quiet" records "$E/sessions"
  # A fresh child keeps the whole tree deferred.
  fxo internal --id "$R1" --child "$C2" >/dev/null
  fxo touch --id "$R1" --age 400 >/dev/null; touch "$(find "$E/sessions" -path "*/$C2/session.jsonl")"
  : > "$E/exports.log"
  run --scan --dry-run --quiet-for 300
  [[ ! -s "$E/out.jsonl" && ! -s "$E/exports.log" ]] && check "quiet: a fresh child defers its root" \
    || fail "quiet: fresh child did not defer the unit"
fi

if want idempotent; then
  mkenv idempotent
  basic_root "$R1"
  run --scan --dry-run; cp "$E/out.jsonl" "$E/a.jsonl"
  run --scan --dry-run
  cmp -s "$E/a.jsonl" "$E/out.jsonl" && check "idempotent: identical dry runs" || fail "idempotent: dry runs differ"
  [[ ! -e "$E/mh/cursors/muse-capture.json" ]] && check "idempotent: dry run writes no cursor" \
    || fail "idempotent: dry run wrote a cursor"
fi

if want queue; then
  mkenv queue
  basic_root "$R1"
  run --scan --dry-run; cp "$E/out.jsonl" "$E/dry.jsonl"
  run --scan
  [[ $RC == 0 ]] || fail "queue: rc=$RC"
  cat "$E"/q/batch-*.jsonl > "$E/queued.jsonl" 2>/dev/null || : > "$E/queued.jsonl"
  if python3 -I -c '
import json,sys
a=[json.loads(l) for l in open(sys.argv[1])]; b=[json.loads(l) for l in open(sys.argv[2])]
sys.exit(0 if a==b else 1)' "$E/dry.jsonl" "$E/queued.jsonl"; then check "queue: queued lines equal the dry run"
  else fail "queue: queued lines differ from the dry run ($(wc -l < "$E/dry.jsonl") vs $(wc -l < "$E/queued.jsonl"))"; fi
  [[ -f "$E/mh/cursors/muse-capture.json" ]] && check "queue: cursor written" || fail "queue: no cursor"
  perm=$(stat -c %a "$E/mh/cursors/muse-capture.json" 2>/dev/null)
  [[ "$perm" == 600 ]] && check "queue: cursor mode 0600" || fail "queue: cursor mode $perm"
  grep -q -E '^memoire-capture-muse: done: units=1 events=[1-9][0-9]* failed=0 ' "$E/err" \
    && check "queue: done line" || fail "queue: done line '$(grep 'done:' "$E/err")'"
  nb=$(ls "$E/q" | wc -l); : > "$E/exports.log"
  run --scan
  [[ $(ls "$E/q" | wc -l) == "$nb" && ! -s "$E/exports.log" ]] && check "queue: unchanged store neither exported nor queued" \
    || fail "queue: second run exported or queued again"
  [[ "$(grep -c '^memoire-capture-muse: done:' "$E/err")" == 1 ]] && grep -q -E '^memoire-capture-muse: done: units=0 events=0 failed=0 rejected=0 excluded=0 deferred=0 skipped=1 capture_off=0 no_workspace=0$' "$E/err" \
    && check "queue: done line skipped=1" || fail "queue: done line '$(grep 'done:' "$E/err")'"
  [[ ! -s "$E/out.jsonl" ]] && check "queue: real run prints nothing on stdout" || fail "queue: stdout not empty"
fi

if want queue-failure; then
  mkenv queue-failure
  basic_root "$R1"
  QSTUB_FAIL=1 run --scan
  [[ $RC == 1 ]] && check "queue-failure: rc 1" || fail "queue-failure: rc=$RC"
  run --scan
  cat "$E"/q/batch-*.jsonl > "$E/out.jsonl" 2>/dev/null || : > "$E/out.jsonl"
  expect "queue-failure: next run emits everything" records "$E/sessions"
fi

if want state-failure; then
  mkenv state-failure
  basic_root "$R1"
  mkdir -p "$E/mh/cursors"; chmod 500 "$E/mh/cursors"
  if [[ -w "$E/mh/cursors" ]]; then echo "skip: state-failure needs a non-root user"
  else
    run --scan
    [[ $RC == 7 ]] && check "state-failure: rc 7" || fail "state-failure: rc=$RC (want 7)"
    [[ -n "$(ls "$E/q")" ]] && check "state-failure: the queue write happened" || fail "state-failure: nothing queued"
  fi
  chmod 700 "$E/mh/cursors"
fi

if want read-only; then
  mkenv read-only
  basic_root "$R1"
  fxo spawn --id "$R1" --child "$C1" --subagent "$A1" >/dev/null; age "$R1"
  snap() { find "$E/sessions" -printf '%P %s %T@ %m\n' | sort; }
  snap > "$E/before"
  run --scan; run --scan --full; run --scan --dry-run
  snap > "$E/after"
  cmp -s "$E/before" "$E/after" && check "read-only: store untouched" || { fail "read-only: store changed"; diff "$E/before" "$E/after" | head -5 >&2; }
fi

if want export-failure; then
  mkenv export-failure
  basic_root "$R1"; basic_root "$R2"
  MUSE_STUB_FAIL="$R1" run --scan
  [[ $RC == 1 ]] && check "export-failure: rc 1" || fail "export-failure: rc=$RC"
  cat "$E"/q/batch-*.jsonl > "$E/out.jsonl" 2>/dev/null || : > "$E/out.jsonl"
  expect "export-failure: other unit captured" records "$E/sessions" "$R2"
  expect "export-failure: failed unit emits nothing" absent "$R1"
fi

if want schema; then
  mkenv schema
  basic_root "$R1"
  MUSE_STUB_SCHEMA=2 run --scan --dry-run
  [[ $RC == 1 && ! -s "$E/out.jsonl" ]] && check "schema: refused, rc 1, nothing emitted" || fail "schema: rc=$RC, $(wc -l < "$E/out.jsonl") lines"
  grep -q 'export_schema_version' "$E/err" && grep -q '2' "$E/err" && check "schema: logged" || fail "schema: no log line naming the version"
fi

if want unparseable; then
  mkenv unparseable
  fxo new --id "$R1" --workspace "$PROJ" >/dev/null
  fxo turn --id "$R1" --prompt "Throwaway" --reply "ok" >/dev/null
  fxo garbage --id "$R1" >/dev/null
  fxo end --id "$R1" >/dev/null
  age "$R1"
  run --scan --dry-run
  [[ $RC == 0 ]] && check "unparseable: rc 0" || fail "unparseable: rc=$RC"
  expect "unparseable: raw line captured" records "$E/sessions"
  expect "unparseable: mapping table" typed
  expect "unparseable: diagnostics in facts" facts "$R1" 'diagnostics={"unparseable_lines":1}'
  grep -q -i 'unparseable' "$E/err" && check "unparseable: logged" || fail "unparseable: no log line"
fi

exclude_case() {
  local name="$1" line="$2"
  mkenv "exclude-$name"
  local X="$E/ex"
  mkdir -p "$X/private-proj/sub" "$X/private-proj-other"
  ln -s "$X/private-proj/sub" "$X/link-into-private"
  ln -s "$X/private-proj" "$X/link-to-root"
  printf '# comment\n\n%s\n' "${line//@X@/$X}" > "$E/cfg/memoire/acpx-exclude"
  basic_root "$R1" "$X/private-proj"
  basic_root "$R2" "$X/link-into-private"
  basic_root 01a14aa4-0000-7000-8000-0000000000a3 "$X/private-proj-other"
  run --scan --dry-run
  [[ $RC == 0 ]] && check "exclude ($name): rc 0" || fail "exclude ($name): rc=$RC"
  expect "exclude ($name): direct cwd emits nothing" absent "$R1"
  expect "exclude ($name): symlinked cwd emits nothing" absent "$R2"
  if grep -q -e "$R1" -e "$R2" "$E/exports.log"; then fail "exclude ($name): an excluded session was exported"
  else check "exclude ($name): excluded sessions never exported"; fi
  if grep -q -E 'private-proj($|[^-])|link-into-private|link-to-root' "$E/err"; then fail "exclude ($name): the log names an excluded workspace"
  else check "exclude ($name): log does not name the excluded workspace"; fi
  expect "exclude ($name): sibling kept" present 01a14aa4-0000-7000-8000-0000000000a3
}
if want exclude; then
  exclude_case canonical '@X@/private-proj'
  exclude_case trailing-slash '@X@/private-proj/'
  exclude_case symlink-prefix '@X@/link-to-root'
fi

if want no-exclude-file; then
  mkenv no-exclude-file
  basic_root "$R1"
  rm -f "$E/cfg/memoire/acpx-exclude"
  run --scan --dry-run
  [[ $RC == 3 && ! -s "$E/out.jsonl" && ! -s "$E/exports.log" ]] && check "no-exclude-file: rc 3, nothing read" \
    || fail "no-exclude-file: rc=$RC, $(wc -l < "$E/out.jsonl") lines, $(wc -l < "$E/exports.log") exports"
fi

if want capture-off; then
  mkenv capture-off
  mkdir -p "$WORK/off-proj"
  printf 'project: off-proj\ncapture: false\n' > "$WORK/off-proj/.memoire"
  basic_root "$R1" "$WORK/off-proj"
  basic_root "$R2"
  run --scan --dry-run
  [[ $RC == 0 ]] || fail "capture-off: rc=$RC"
  expect "capture-off: nothing emitted" absent "$R1"
  expect "capture-off: neighbour captured" records "$E/sessions" "$R2"
fi

if want kill-switch; then
  mkenv kill-switch
  basic_root "$R1"
  MEMOIRE_CAPTURE=off run --scan --dry-run
  [[ $RC == 0 && ! -s "$E/out.jsonl" && ! -s "$E/exports.log" ]] && check "kill-switch: off" || fail "kill-switch: rc=$RC or work done"
fi

if want no-muse-bin; then
  mkenv no-muse-bin
  MUSE_BIN_OVERRIDE="$WORK/absent/muse" run --scan --dry-run
  [[ $RC == 0 ]] && check "no-muse-bin: nothing ready, rc 0" || fail "no-muse-bin: empty store rc=$RC"
  basic_root "$R1"
  MUSE_BIN_OVERRIDE="$WORK/absent/muse" run --scan --dry-run
  [[ $RC == 1 && ! -s "$E/out.jsonl" ]] && check "no-muse-bin: unit ready, rc 1" || fail "no-muse-bin: rc=$RC"
fi

if want window; then
  mkenv window
  basic_root "$R1"; age "$R1" $((3 * 86400))
  basic_root "$R2"; age "$R2" 600
  run --scan --dry-run --since "1 hour ago"
  expect "window: old tree left out" absent "$R1"
  expect "window: recent tree captured" present "$R2"
  run --scan --dry-run --full
  expect "window: --full takes everything" present "$R1"
  d="$(find "$E/sessions" -maxdepth 4 -type d -name "$R1")"
  run --dry-run "$d"
  expect "window: explicit dir bypasses the window" records "$E/sessions" "$R1"
  run --dry-run "$d/session.jsonl"
  expect "window: explicit session.jsonl bypasses the window" records "$E/sessions" "$R1"
fi

if want invalid-id; then
  mkenv invalid-id
  basic_root "$R1"
  bad="$E/sessions/2026/10/17/not-a-uuid"
  mkdir -p "$bad"; cp "$(find "$E/sessions" -path "*/$R1/session.jsonl")" "$bad/session.jsonl"
  touch -d '10 minutes ago' "$bad/session.jsonl"
  run --scan --dry-run
  [[ $RC == 0 ]] && check "invalid-id: rc 0" || fail "invalid-id: rc=$RC"
  grep -q 'not-a-uuid' "$E/exports.log" && fail "invalid-id: invalid dir exported" || check "invalid-id: never exported"
  grep -q -i 'refus\|reject\|invalid' "$E/err" && check "invalid-id: logged" || fail "invalid-id: no log line"
  expect "invalid-id: valid neighbour captured" records "$E/sessions" "$R1"
fi

if want snapshot-append; then
  mkenv snapshot-append
  basic_root "$R1"
  python3 -I - "$E/sessions" "$R1" > "$E/append.jsonl" <<'PY'
import json, os, sys, uuid
root, sid = sys.argv[1], sys.argv[2]
for d, _, f in os.walk(root):
    if os.path.basename(d) == sid:
        path = os.path.join(d, "session.jsonl")
seq = max(json.loads(l).get("sequence", 0) for l in open(path) if l.strip())
t0 = int(open(os.path.join(os.path.dirname(path), ".fixture-t0")).read())
rec = {"schema_version": 1, "id": str(uuid.uuid5(uuid.NAMESPACE_URL, f"{sid}:late")),
       "stream": {"kind": "session", "id": sid}, "sequence": seq + 1, "recorded_at": t0 + (seq + 1) * 1000,
       "record_type": "event", "durability": "durable", "causation_id": None,
       "payload_type": "runtime.session.task_source_committed", "payload_schema_version": 1,
       "payload": {"note": "appended while the export ran"}}
print(json.dumps(rec, separators=(",", ":")))
PY
  MUSE_STUB_APPEND="$E/append.jsonl" run --scan --quiet-for 0
  [[ $RC == 0 && ! -e "$E/append.jsonl" ]] && check "snapshot-append: first run, producer appended during export" \
    || fail "snapshot-append: first run rc=$RC or the append did not happen"
  run --scan --quiet-for 0
  [[ $RC == 0 ]] || fail "snapshot-append: second run rc=$RC"
  cat "$E"/q/batch-*.jsonl > "$E/out.jsonl" 2>/dev/null || : > "$E/out.jsonl"
  expect "snapshot-append: the appended record is queued by the next run" records "$E/sessions"
  expect "snapshot-append: nothing queued twice" common
fi

# probe_case NAME FILLERS BIG EXPECT(found|deferred): FILLERS filler records
# between the capture-selection record and the metadata record; BIG=1 makes the
# first filler larger than 1 MiB.
probe_case() {
  local name="$1" fillers="$2" big="$3" want_found="$4"
  mkenv "probe-bound-$name"
  basic_root "$R1"
  python3 -I - "$E/sessions" "$R1" "$fillers" "$big" <<'PY'
import json, os, sys, uuid
root, sid, n, big = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4] == "1"
for d, _, f in os.walk(root):
    if os.path.basename(d) == sid:
        path = os.path.join(d, "session.jsonl")
lines = open(path).read().splitlines(True)
meta = next(i for i, l in enumerate(lines) if '"runtime.session.metadata"' in l)
fill = []
for i in range(n):
    rec = {"schema_version": 1, "id": str(uuid.uuid5(uuid.NAMESPACE_URL, f"{sid}:fill:{i}")),
           "stream": {"kind": "session", "id": "filler-stream"}, "sequence": i + 1, "recorded_at": 1792253635000000 + i,
           "record_type": "event", "durability": "durable", "causation_id": None,
           "payload_type": "runtime.session.task_source_committed", "payload_schema_version": 1,
           "payload": {"pad": ("x" * (1100000 if big and i == 0 else 8))}}
    fill.append(json.dumps(rec, separators=(",", ":")) + "\n")
lines[meta:meta] = fill
open(path, "w").write("".join(lines))
PY
  age "$R1"
  local mline
  mline=$(grep -n '"runtime.session.metadata"' "$(find "$E/sessions" -path "*/$R1/session.jsonl")" | cut -d: -f1)
  run --scan --dry-run
  [[ $RC == 0 ]] || fail "probe-bound ($name): rc=$RC"
  if [[ "$want_found" == found ]]; then
    expect "probe-bound ($name): metadata on line $mline found, unit captured" records "$E/sessions"
  else
    if [[ ! -s "$E/out.jsonl" && ! -s "$E/exports.log" ]] && grep -q -E '^memoire-capture-muse: done: .* no_workspace=1$' "$E/err"; then
      check "probe-bound ($name): metadata on line $mline outside the bound, deferred, never exported"
    else
      fail "probe-bound ($name): $(wc -l < "$E/out.jsonl") lines, $(wc -l < "$E/exports.log") exports, done: $(grep 'done:' "$E/err")"
    fi
  fi
}
if want probe-bound; then
  probe_case line64 61 0 found
  probe_case line65 62 0 deferred
  probe_case bytes 1 1 deferred
fi

if want workdir; then
  mkenv workdir
  basic_root "$R1"
  run --scan --dry-run
  run --scan
  left=$(find "$E/mh/tmp/muse-capture" -mindepth 1 -maxdepth 1 -name 'run.*' 2>/dev/null | wc -l)
  [[ "$left" == 0 ]] && check "workdir: no run.* directory left" || fail "workdir: $left run.* directories left"
fi

if want gap; then
  # `muse export` gap events carry no id, sequence or time; identical markers
  # must still land as distinct events, named by position (k-th gap after the
  # nearest preceding record, "start" before any record), each emitted once.
  mkenv gap
  fxo new --id "$R1" --workspace "$PROJ" >/dev/null
  f="$(find "$E/sessions" -path "*/$R1/session.jsonl")"
  python3 -I - "$f" "$R1" "$FIX" <<'PY'
import importlib.util, json, sys
path, sid, fix = sys.argv[1], sys.argv[2], sys.argv[3]
spec = importlib.util.spec_from_file_location("fx", fix)
fx = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fx)
lines = open(path).read().splitlines(True)
marker = fx.gap_marker(sid, "omitted_live_only", 1, "33333333-3333-4333-8333-333333333333")
lines.insert(1, json.dumps(marker, separators=(",", ":")) + "\n")  # after the retained frame, before any record
open(path, "w").writelines(lines)
PY
  # A repeated record id (muse export keeps it as a duplicate record event)
  # between two gaps: the anchor re-appears and its count continues (1@A, 2@A).
  fxo gap --id "$R1" >/dev/null
  fxo dup --id "$R1" >/dev/null
  fxo gap --id "$R1" >/dev/null
  fxo turn --id "$R1" --prompt "Throwaway: name a colour" --reply "Blue." --run run-g1 >/dev/null
  fxo gap --id "$R1" --count 3 >/dev/null
  fxo turn --id "$R1" --prompt "And another?" --reply "Green." --run run-g2 >/dev/null
  fxo gap --id "$R1" >/dev/null
  fxo end --id "$R1" >/dev/null
  age "$R1"
  run --scan --dry-run; cp "$E/out.jsonl" "$E/dry1.jsonl"
  run --scan --dry-run
  cmp -s "$E/dry1.jsonl" "$E/out.jsonl" && check "gap: identical dry runs (stable gap ids)" || fail "gap: dry runs differ"
  run --scan
  [[ $RC == 0 ]] && check "gap: rc 0" || fail "gap: rc=$RC"
  cat "$E"/q/batch-*.jsonl > "$E/first.jsonl" 2>/dev/null || : > "$E/first.jsonl"
  cp "$E/first.jsonl" "$E/out.jsonl"
  expect "gap: well-formed, no duplicate id" common
  expect "gap: every line captured, gaps by position" records "$E/sessions"
  expect "gap: mapping table (muse_gap, no text)" typed
  expect "gap: 7 distinct gap events, matching diagnostics.gaps" gaps "$R1" 7
  grep -q 'unhandled export event kind' "$E/err" && fail "gap: gap still handled by the unknown-kind fallback" \
    || check "gap: no unknown-kind fallback"
  # Append a turn and two more identical gaps: only the new ones are emitted.
  fxo turn --id "$R1" --prompt "A third?" --reply "Red." --run run-g3 >/dev/null
  fxo gap --id "$R1" --count 2 >/dev/null
  age "$R1"
  rm -f "$E"/q/batch-*.jsonl
  run --scan
  [[ $RC == 0 ]] || fail "gap: second run rc=$RC"
  cat "$E"/q/batch-*.jsonl > "$E/second.jsonl" 2>/dev/null || : > "$E/second.jsonl"
  expect_file "gap: nothing emitted twice" "$E/first.jsonl" disjoint "$E/second.jsonl"
  n_gap=$(python3 -I -c 'import json,sys; print(sum(1 for l in open(sys.argv[1]) if json.loads(l)["event_type"] == "muse_gap"))' "$E/second.jsonl")
  [[ "$n_gap" == 2 ]] && check "gap: only the 2 new gaps in the second run" || fail "gap: $n_gap gap events in the second run (want 2)"
  cat "$E/first.jsonl" "$E/second.jsonl" > "$E/out.jsonl"
  expect "gap: union covers every line" records "$E/sessions"
  expect "gap: union has 9 gaps, matching the newest diagnostics.gaps" gaps "$R1" 9
fi

if want contract; then
  if [[ -z "${MUSE_CONTRACT_BIN:-}" ]]; then
    echo "skip: contract (set MUSE_CONTRACT_BIN to a real muse binary)"
  else
    mkenv contract
    fxo new --id "$R1" --workspace "$PROJ" >/dev/null
    fxo turn --id "$R1" --prompt "Throwaway" --reply "ok" --reasoning "r" --tool 'read_file={"p":1}:x' >/dev/null
    fxo spawn --id "$R1" --child "$C1" --subagent "$A1" >/dev/null
    fxo internal --id "$R1" --child "$C2" >/dev/null
    fxo gap --id "$R1" --count 2 >/dev/null
    fxo dup --id "$R1" >/dev/null
    fxo gap --id "$R1" >/dev/null
    fxo garbage --id "$R1" >/dev/null
    fxo end --id "$R1" >/dev/null
    mkdir -p "$E/x"
    while IFS= read -r f; do
      n="$(basename "$(dirname "$f")")"
      python3 -I "$STUB" export --session "$f" --out "$E/x/stub-$n.json" >/dev/null
      (cd "$E/x" && "$MUSE_CONTRACT_BIN" export --session "$f" --out "$E/x/real-$n.json" >/dev/null 2>&1) \
        || fail "contract: real export failed for $n"
    done < <(find "$E/sessions" -name session.jsonl)
    if python3 -I - "$E/x" <<'PY'
import glob, json, os, sys
d = sys.argv[1]
def proj(e):
    s = e["sessions"][0]
    evs = []
    for x in e["events"]:
        if x.get("kind") == "retained_frame" or (x.get("kind") == "unparseable_line"
                                                 and x.get("raw_text", "").startswith('{"retained_frame"')):
            continue  # known divergence: real rejects a synthetic frame
        env = x.get("envelope") or {}
        der = x.get("derived") or {}
        gap = {k: v for k, v in x.items() if k != "kind"} if x.get("kind") == "gap" else None
        evs.append((x.get("kind"), env.get("id"), x.get("raw_text"), der.get("session_id"),
                    der.get("turn_count"), der.get("duplicate", False), json.dumps(env, sort_keys=True),
                    json.dumps(gap, sort_keys=True)))
    return {"schema": e.get("export_schema_version"), "sid": s.get("session_id"),
            "turns": s.get("turn_count"), "end": s.get("session_end"),
            "diag_keys": sorted(e.get("diagnostics", {})),
            "gaps": e.get("diagnostics", {}).get("gaps"),
            "duplicates": e.get("diagnostics", {}).get("duplicate_records"), "events": evs}
bad = 0
for real in sorted(glob.glob(f"{d}/real-*.json")):
    n = os.path.basename(real)[5:-5]
    a, b = proj(json.load(open(real))), proj(json.load(open(f"{d}/stub-{n}.json")))
    for k in a:
        if a[k] != b[k]:
            bad += 1
            print(f"{n}: {k} differs", file=sys.stderr)
sys.exit(1 if bad else 0)
PY
    then check "contract: stub agrees with the real binary on the behaviour-driving fields (other diagnostic values, accepted_spawns, exporter_version not compared)"; else fail "contract: stub and real binary disagree"; fi
  fi
fi

if (( FAILURES )); then
  echo "muse-store-capture: $FAILURES failure(s)" >&2
  exit 1
fi
echo "muse-store-capture: all passed"
