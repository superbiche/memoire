#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# acpx-journal-capture.sh - guarantee tests for the acpx journal capture
# adapter (scripts/memoire-capture-acpx.sh). It reads acpx session records and
# their journal streams (live ~/.acpx/sessions or the synced archive layout)
# and queues one boundary event per prompt, the turn result carrying the
# worker's final message, and, for agents without a native Memoire adapter,
# the intermediate assistant messages. `make test-core` runs this script (no
# Memoire home, no store, no network: everything lives in a scratch directory),
# and so does `make acpx-capture-test`.
#
#   basic        codex-acp (native adapter exists): prompt + result per turn,
#                no intermediate messages; final message text exact
#   full-agent   an agent without a native adapter also gets its
#                intermediate messages
#   full-flag    --full forces intermediate messages for any agent
#   noids        chunks without messageId: tool calls separate messages
#   reconnect    JSON-RPC ids restart per connection; the journal request_id
#                keys the turn; a session/load replay outside a turn is ignored
#   thoughts     agent_thought_chunk text never reaches an event
#   incomplete   a turn without turn_result emits nothing while the stream is
#                fresh; past --quiet-for it emits its prompt (and messages in
#                full mode) but never a result event
#   cancelled    a cancelled turn still gets its result event, with
#                status and stop_reason as recorded
#   long         texts over 6000 characters split into a first part
#                (text_truncated) and a :rest continuation; parts concatenate
#                to the original, multibyte included
#   segmented    archive layout: rotated segments named by content hash are
#                ordered by their segment meta sequence; a turn spanning a
#                boundary is reassembled
#   live-wins    a record present live and in an archive is read once, live
#   source-isolation  the live copy's streams only, even when an archive-only
#                neighbour brings the archive directory into the run and the
#                archive copy is a stale snapshot; also with two archives
#   rpconly      a stream without journal meta lines emits nothing, exit 0
#   exclude      a record whose cwd is under an excluded prefix (path boundary,
#                symlinks resolved) emits nothing and its stream is never
#                read (witnessed by access time, calibrated first); a
#                trailing-slash or symlinked prefix excludes the same root; a
#                sibling with a longer name is not excluded
#   no-exclude-file  a missing or unreadable exclusion file stops the run
#                (exit 3) before any record is read
#   capture-off  a cwd under a `.memoire` with `capture: false` emits nothing
#   idempotent   two runs over the same input emit identical lines
#   queue        without --dry-run the queue writer receives the same lines
#                through -batch-file; a second run over unchanged input does
#                not call it again (state file)
#   state-failure  an unwritable cursor after a successful queue write exits
#                non-zero
#   quiet-cursor an open turn deferred by a real run is queued by a later
#                run once quiet, even though no file changed
#   mode-cursor  the capture mode is part of the cursor: a --full run after a
#                boundaries-only run over unchanged files queues the
#                intermediate messages; a full capture satisfies a later
#                boundaries run; a legacy cursor entry without a mode is
#                re-captured; the cursor value records the mode
#   mode-migration  a cursor of legacy entries (no mode) where only one record
#                is inside the --since window: the run persists its cursor
#                (exit 0), and the next unchanged run queues nothing
#
# Environment (all optional):
#   ACPX_CAPTURE_ADAPTER  adapter under test (default: the repository's)
#   ACPX_CAPTURE_ONLY     space-separated test names to run (default: all)
#   ACPX_CAPTURE_KEEP     keep the scratch directory (debugging)

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ADAPTER="${ACPX_CAPTURE_ADAPTER:-$ROOT/scripts/memoire-capture-acpx.sh}"
FIX="$ROOT/tests/foreign/acpx-journal-fixtures.py"
EXPECT="$ROOT/tests/foreign/acpx-journal-expect.py"
ONLY="${ACPX_CAPTURE_ONLY:-}"

command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }
command -v python3 >/dev/null || { echo "SKIP: python3 not available"; exit 0; }
[[ -f "$ADAPTER" ]] || { echo "FAIL: adapter not found: $ADAPTER"; exit 1; }

WORK="$(mktemp -d "${TMPDIR:-/var/tmp}/acpx-capture-test.XXXXXX")" || { echo "FAIL: mktemp -d"; exit 1; }
trap '[[ -n "${ACPX_CAPTURE_KEEP:-}" ]] && echo "kept: $WORK" || { chmod -R u+rwx "$WORK" 2>/dev/null; rm -rf "$WORK"; }' EXIT

FAILURES=0
fail() { echo "FAIL: $1" >&2; FAILURES=$((FAILURES + 1)); }
check() { echo "ok: $1"; }
want() { [[ -z "$ONLY" || " $ONLY " == *" $1 "* ]]; }

PROJ=/tmp/acpx-cap-proj
cat > "$WORK/registry.json" <<JSON
{
  "schema": "liaison-memoire-projects/1",
  "projects": {
    "acpx-cap-test": { "display": "acpx-cap-test", "paths": ["$PROJ"] }
  }
}
JSON

# Stub queue writer: records each -batch-file it receives.
mkdir -p "$WORK/bin"
cat > "$WORK/bin/memoire-queue" <<'STUB'
#!/usr/bin/env bash
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

# mkenv NAME -> scratch env dir with sessions/, archive/, home/, mh/, q/ and an
# empty exclusion file; sets globals for run().
mkenv() {
  E="$WORK/$1"
  mkdir -p "$E/sessions" "$E/archive" "$E/home" "$E/mh" "$E/q" "$E/cfg/memoire"
  : > "$E/cfg/memoire/acpx-exclude"
}

# Fixture registries for the native-lane derivation (harness parity): codex-acp
# runs the codex harness whose acpx lane is `boundaries`; antigravity-acp.py
# runs antigravity whose lane is `journal`. Written once, pointed at by run().
cat > "$WORK/acpx.json" <<'JSON'
{"schema": "liaison-acpx-registry/1", "agents": {
  "codex": {"command": "~/.local/share/mise/shims/codex-acp"},
  "antigravity": {"command": "~/.agents/bin/antigravity-acp.py", "args": ["--profile", "x"]}}}
JSON
cat > "$WORK/harnesses.json" <<'JSON'
{"schema": "liaison-harness-registry/1", "harnesses": {
  "codex": {"status": "active", "added": "2026-05-16", "aliases": [], "acpx_agents": ["codex"], "launch_surfaces": ["codex"], "native_cli": "codex", "config_home": "~/.codex"},
  "antigravity": {"status": "trial", "added": "2026-10-13", "aliases": [], "acpx_agents": ["antigravity"], "launch_surfaces": ["antigravity"], "native_cli": null, "config_home": null, "exercised": {"state": "no", "date": null, "session": null, "note": null}}}}
JSON
cat > "$WORK/lanes.json" <<'JSON'
{"schema": "memoire-capture-lanes/1", "harnesses": {
  "codex": {"native": {"lane": "rollout-sweep", "enable_env": "MEMOIRE_CAPTURE_CODEX"}, "acpx": "boundaries"},
  "antigravity": {"native": {"lane": "none", "reason": "acpx only in the fleet"}, "acpx": "journal"}},
  "acpx_lane": {"script": "scripts/memoire-capture-acpx.sh", "exclusion_file": "~/.config/memoire/acpx-exclude", "cursor": "~/.memoire/cursors/acpx-capture.json", "timer": null}}
JSON

# run [adapter args...] -> stdout of the adapter in $E/out.jsonl, stderr in $E/err, rc in RC
run() {
  HOME="$E/home" MEMOIRE_HOME="$E/mh" XDG_CONFIG_HOME="$E/cfg" \
    MEMOIRE_PROJECTS_REGISTRY="$WORK/registry.json" \
    MEMOIRE_ACPX_REGISTRY="${ACPX_REGISTRY_OVERRIDE:-$WORK/acpx.json}" MEMOIRE_CAPTURE_CODEX="${CODEX_FLAG_OVERRIDE:-on}" \
    MEMOIRE_HARNESS_INVENTORY="$WORK/harnesses.json" MEMOIRE_CAPTURE_LANES="$WORK/lanes.json" \
    MEMOIRE_QUEUE_BIN="$WORK/bin/memoire-queue" QSTUB_DIR="$E/q" \
    bash "$ADAPTER" "$@" >"$E/out.jsonl" 2>"$E/err"
  RC=$?
}

# compare NAME EXPECT_JSON MODE QUIET SOURCE [PROJECT]
compare() {
  if python3 "$EXPECT" "$2" "$E/out.jsonl" "$3" "$4" "$5" "${6:-acpx-cap-test}" >"$E/cmp" 2>&1; then
    check "$1"
  else
    fail "$1"; cat "$E/cmp" >&2; sed -n '1,5p' "$E/err" >&2
  fi
}

CODEX=/home/u/.local/share/mise/shims/codex-acp
AGY=/home/u/.agents/bin/antigravity-acp.py

if want basic; then
  mkenv basic
  python3 "$FIX" basic "$E/sessions" agent="$CODEX" > "$E/x.json"
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  [[ $RC == 0 ]] || fail "basic: rc=$RC"
  compare basic "$E/x.json" boundaries 0 live
fi

if want full-agent; then
  mkenv full-agent
  python3 "$FIX" basic "$E/sessions" agent="$AGY" seed=fa > "$E/x.json"
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  compare full-agent "$E/x.json" full 0 live
fi

if want no-registry; then
  # The derivation fails SAFE: without the acpx registry even codex-acp gets
  # full journal capture (nothing is lost), and the adapter says why once.
  mkenv no-registry
  python3 "$FIX" basic "$E/sessions" agent="$CODEX" seed=nr > "$E/x.json"
  ACPX_REGISTRY_OVERRIDE="$WORK/absent-acpx.json" run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  compare no-registry "$E/x.json" full 0 live
  grep -q 'capture lanes unresolved' "$E/err" || fail "no-registry: no unresolved marker in stderr"
fi

if want native-disabled; then
  # The native sweep that would capture the turns is OFF: boundaries-only
  # would lose the conversation, so the journal keeps full capture.
  mkenv native-disabled
  python3 "$FIX" basic "$E/sessions" agent="$CODEX" seed=nd > "$E/x.json"
  CODEX_FLAG_OVERRIDE=off run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  compare native-disabled "$E/x.json" full 0 live
fi

if want full-flag; then
  mkenv full-flag
  python3 "$FIX" basic "$E/sessions" agent="$CODEX" seed=ff > "$E/x.json"
  run --scan --sessions-dir "$E/sessions" --dry-run --full --since "100 years ago"
  compare full-flag "$E/x.json" full 0 live
fi

if want noids; then
  mkenv noids
  python3 "$FIX" noids "$E/sessions" agent="$AGY" > "$E/x.json"
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  compare noids "$E/x.json" full 0 live
fi

if want reconnect || want thoughts; then
  mkenv reconnect
  python3 "$FIX" reconnect "$E/sessions" agent="$AGY" > "$E/x.json"
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  compare "reconnect+thoughts" "$E/x.json" full 0 live
fi

if want incomplete; then
  mkenv incomplete
  python3 "$FIX" incomplete "$E/sessions" agent="$AGY" > "$E/x.json"
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago" --quiet-for 3600
  compare "incomplete (fresh)" "$E/x.json" full 0 live
  touch -d '2 hours ago' "$E/sessions"/*
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago" --quiet-for 3600
  compare "incomplete (quiet)" "$E/x.json" full 1 live
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago" --quiet-for 3600 --boundaries-only
  compare "incomplete (quiet, boundaries-only)" "$E/x.json" boundaries 1 live
fi

if want cancelled; then
  mkenv cancelled
  python3 "$FIX" cancelled "$E/sessions" agent="$CODEX" > "$E/x.json"
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  compare cancelled "$E/x.json" boundaries 0 live
fi

if want long; then
  mkenv long
  python3 "$FIX" long "$E/sessions" agent="$AGY" > "$E/x.json"
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  compare long "$E/x.json" full 0 live
fi

if want segmented; then
  mkenv segmented
  python3 "$FIX" segmented "$E/archive" agent="$AGY" layout=archive > "$E/x.json"
  run --scan --sessions-dir "$E/sessions" --archive-dir "$E/archive" --dry-run --since "100 years ago"
  compare "segmented (archive)" "$E/x.json" full 0 archive
  mkenv segmented-live
  python3 "$FIX" segmented "$E/sessions" agent="$AGY" seed=sl > "$E/x.json"
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  compare "segmented (live)" "$E/x.json" full 0 live
fi

if want live-wins; then
  mkenv live-wins
  python3 "$FIX" basic "$E/sessions" agent="$CODEX" seed=lw > "$E/x.json"
  python3 "$FIX" basic "$E/archive" agent="$CODEX" seed=lw layout=archive > /dev/null
  run --scan --sessions-dir "$E/sessions" --archive-dir "$E/archive" --dry-run --since "100 years ago"
  compare live-wins "$E/x.json" boundaries 0 live
fi

if want source-isolation; then
  # Review r2 F6: a record present live and archived must be read from its
  # selected copy only, also when an archive-only neighbour makes the archive
  # directory part of the run. The archive copy is a stale snapshot: its active
  # segment stops before the last turn's result, so mixing the copies would
  # duplicate chunks (corrupt final text) and event ids.
  mkenv source-isolation
  python3 "$FIX" segmented "$E/sessions" agent="$AGY" seed=si > "$E/x.json"
  python3 "$FIX" segmented "$E/archive" agent="$AGY" seed=si layout=archive > /dev/null
  python3 "$FIX" basic "$E/archive" agent="$AGY" seed=si-neighbour layout=archive > "$E/n.json"
  r="$(jq -r .record_id "$E/x.json")"
  head -n -4 "$E/archive/$r.stream.ndjson" > "$E/stale" && cat "$E/stale" > "$E/archive/$r.stream.ndjson"
  run --scan --sessions-dir "$E/sessions" --archive-dir "$E/archive" --dry-run --since "100 years ago"
  compare "source-isolation: live copy only" "$E/x.json" full 0 live
  compare "source-isolation: archive-only neighbour read" "$E/n.json" full 0 archive
  # Same with the archive listed first and a second archive holding another copy.
  mkdir -p "$E/archive2"
  python3 "$FIX" segmented "$E/archive2" agent="$AGY" seed=si layout=archive > /dev/null
  run --scan --archive-dir "$E/archive2" --archive-dir "$E/archive" --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  compare "source-isolation: two archives, live wins" "$E/x.json" full 0 live
fi

if want rpconly; then
  mkenv rpconly
  python3 "$FIX" rpconly "$E/sessions" agent="$CODEX" > "$E/r.json"
  python3 "$FIX" basic "$E/sessions" agent="$CODEX" seed=rc > "$E/x.json"
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  s="$(jq -r .session_id "$E/r.json")"
  if [[ $RC == 0 ]] && ! grep -q "$s" "$E/out.jsonl"; then check "rpconly: nothing emitted"; else fail "rpconly: rc=$RC or events emitted"; fi
  compare "rpconly (neighbour still read)" "$E/x.json" boundaries 0 live
fi

# Read observation (review r1 F1): a read is witnessed by the file's access
# time, independent of the adapter's stderr. arm sets atime far before mtime,
# so under relatime or strictatime any successful read moves it; calibrate()
# proves this holds on the scratch filesystem, and a filesystem where it does
# not (noatime) fails the run instead of passing it vacuously.
ARMED_AT=946684800  # 2000-01-01T00:00:00Z
arm() { touch -a -d "@$ARMED_AT" "$@"; }
was_read() { [[ "$(stat -c %X "$1")" != "$ARMED_AT" ]]; }
calibrate() {
  local c="$WORK/atime-canary"
  printf 'x\n' > "$c"; touch -m -d '@1700000000' "$c"; arm "$c"
  was_read "$c" && return 1
  head -c 1 "$c" > /dev/null
  was_read "$c"
}
READS_OBSERVABLE=1
if ! calibrate; then
  READS_OBSERVABLE=0
  fail "read observation: reads do not update atime under ${TMPDIR:-/var/tmp}; run with TMPDIR on a relatime filesystem"
fi

# exclude_case NAME PREFIX_LINE: two excluded records (direct cwd, symlinked
# cwd) and a sibling whose name only extends the excluded root.
exclude_case() {
  local name="$1" line="$2"
  mkenv "exclude-$name"
  local X="$E/ex"
  mkdir -p "$X/private-proj/sub" "$X/private-proj-other"
  ln -s "$X/private-proj/sub" "$X/link-into-private"
  ln -s "$X/private-proj" "$X/link-to-root"
  printf '# comment\n\n%s\n' "${line//@X@/$X}" > "$E/cfg/memoire/acpx-exclude"
  python3 "$FIX" basic "$E/sessions" agent="$AGY" seed="ex1-$name" cwd="$X/private-proj" > "$E/x1.json"
  python3 "$FIX" basic "$E/sessions" agent="$AGY" seed="ex2-$name" cwd="$X/link-into-private" > "$E/x2.json"
  python3 "$FIX" basic "$E/sessions" agent="$AGY" seed="ex3-$name" cwd="$X/private-proj-other" > "$E/x3.json"
  local x r st
  for x in x1 x2; do
    r="$(jq -r .record_id "$E/$x.json")"
    arm "$E/sessions/$r".stream*.ndjson
  done
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  for x in x1 x2; do
    s="$(jq -r .session_id "$E/$x.json")"
    if grep -q "$s" "$E/out.jsonl"; then fail "exclude ($name): $x emitted events"; fi
    r="$(jq -r .record_id "$E/$x.json")"
    if (( READS_OBSERVABLE )); then
      for st in "$E/sessions/$r".stream*.ndjson; do
        was_read "$st" && fail "exclude ($name): excluded stream $(basename "$st") was read"
      done
    fi
  done
  if grep -q -E 'private-proj($|[^-])|link-into-private|link-to-root' "$E/err"; then fail "exclude ($name): the log names an excluded cwd"; fi
  compare "exclude ($name): sibling kept" "$E/x3.json" full 0 live private-proj-other
  [[ $RC == 0 ]] && check "exclude ($name): rc 0" || fail "exclude ($name): rc=$RC"
}

if want exclude; then
  exclude_case canonical '@X@/private-proj'
  exclude_case trailing-slash '@X@/private-proj/'
  exclude_case symlink-prefix '@X@/link-to-root'
fi

# no_exclusion_case NAME: the exclusion file is missing or unreadable; the run
# stops with exit 3 before any record or stream is read.
no_exclusion_case() {
  local name="$1"
  mkenv "no-exclude-file-$name"
  python3 "$FIX" basic "$E/sessions" agent="$AGY" seed="nx-$name" > /dev/null
  if [[ "$name" == missing ]]; then
    rm -f "$E/cfg/memoire/acpx-exclude"
  else
    printf '%s\n' "$E/some/prefix" > "$E/cfg/memoire/acpx-exclude"
    chmod 000 "$E/cfg/memoire/acpx-exclude"
    if [[ -r "$E/cfg/memoire/acpx-exclude" ]]; then
      echo "skip: no-exclude-file ($name) needs a non-root user"; return 0
    fi
  fi
  arm "$E/sessions"/*
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  local f read_any=0
  if (( READS_OBSERVABLE )); then
    for f in "$E/sessions"/*; do was_read "$f" && read_any=1; done
  fi
  if [[ $RC == 3 && ! -s "$E/out.jsonl" && $read_any == 0 ]]; then
    check "no-exclude-file ($name)"
  else
    fail "no-exclude-file ($name): rc=$RC (want 3), $(wc -l < "$E/out.jsonl") lines, record or stream read: $read_any"
  fi
  chmod 600 "$E/cfg/memoire/acpx-exclude" 2>/dev/null || true
}

if want no-exclude-file; then
  no_exclusion_case missing
  no_exclusion_case unreadable
fi

if want capture-off; then
  mkenv capture-off
  mkdir -p "$WORK/off-proj"
  printf 'project: off-proj\ncapture: false\n' > "$WORK/off-proj/.memoire"
  python3 "$FIX" basic "$E/sessions" agent="$AGY" seed=co cwd="$WORK/off-proj" > "$E/o.json"
  python3 "$FIX" basic "$E/sessions" agent="$AGY" seed=con > "$E/x.json"
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  s="$(jq -r .session_id "$E/o.json")"
  if [[ $RC == 0 ]] && ! grep -q "$s" "$E/out.jsonl"; then check "capture-off: nothing emitted"; else fail "capture-off: rc=$RC or events emitted"; fi
  compare "capture-off (neighbour still read)" "$E/x.json" full 0 live
fi

if want idempotent; then
  mkenv idempotent
  python3 "$FIX" segmented "$E/sessions" agent="$AGY" seed=id > /dev/null
  python3 "$FIX" long "$E/sessions" agent="$CODEX" seed=id2 > /dev/null
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  cp "$E/out.jsonl" "$E/first.jsonl"
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  if [[ -s "$E/first.jsonl" ]] && cmp -s "$E/first.jsonl" "$E/out.jsonl"; then check idempotent; else fail idempotent; fi
fi

if want queue; then
  mkenv queue
  python3 "$FIX" basic "$E/sessions" agent="$AGY" seed=q > "$E/x.json"
  run --scan --sessions-dir "$E/sessions" --dry-run --since "100 years ago"
  cp "$E/out.jsonl" "$E/dry.jsonl"
  run --scan --sessions-dir "$E/sessions" --since "100 years ago"
  if [[ $RC == 0 && -s "$E/dry.jsonl" ]] && cmp -s <(cat "$E/q"/batch-*.jsonl 2>/dev/null) "$E/dry.jsonl"; then
    check "queue: batch equals dry-run"
  else
    fail "queue: rc=$RC, batch differs from dry-run"
  fi
  before="$(ls "$E/q" | wc -l)"
  run --scan --sessions-dir "$E/sessions" --since "100 years ago"
  after="$(ls "$E/q" | wc -l)"
  if [[ $RC == 0 && "$before" -gt 0 && "$before" == "$after" ]]; then check "queue: unchanged input not re-queued"; else fail "queue: rc=$RC, re-queued ($before -> $after)"; fi
fi

if want quiet-cursor; then
  # A record holding a completed turn and a fresh open turn: the first real
  # run queues the completed turn; once the open turn passes the quiet window,
  # a later run over the UNCHANGED files must still queue its prompt and
  # messages (the state file must not mark deferred output as captured).
  mkenv quiet-cursor
  python3 "$FIX" incomplete "$E/sessions" agent="$AGY" seed=qc > "$E/x.json"
  run --scan --sessions-dir "$E/sessions" --since "100 years ago" --quiet-for 3
  first="$(cat "$E/q"/batch-*.jsonl 2>/dev/null | wc -l)"
  sleep 4
  run --scan --sessions-dir "$E/sessions" --since "100 years ago" --quiet-for 3
  # Re-queuing the completed turn is allowed (ids are idempotent and the hub
  # keeps the first write): compare the first occurrence of each id.
  cat "$E/q"/batch-*.jsonl 2>/dev/null | python3 -c '
import json, sys
seen = set()
for l in sys.stdin:
    i = json.loads(l)["source_event_id"]
    if i not in seen:
        seen.add(i); sys.stdout.write(l)' > "$E/out.jsonl"
  if [[ $RC == 0 && "$first" -gt 0 ]]; then
    compare "quiet-cursor: deferred turn queued once quiet" "$E/x.json" full 1 live
  else
    fail "quiet-cursor: rc=$RC, first run queued $first lines"
  fi
fi

if want state-failure; then
  # Review r2 F7: a cursor that cannot be persisted after a successful queue
  # write is a failed run (non-zero exit, logged), never a silent success.
  mkenv state-failure
  python3 "$FIX" basic "$E/sessions" agent="$AGY" seed=sf > /dev/null
  mkdir -p "$E/mh/cursors"; chmod 500 "$E/mh/cursors"
  if [[ -w "$E/mh/cursors" ]]; then
    echo "skip: state-failure needs a non-root user"
  else
    run --scan --sessions-dir "$E/sessions" --since "100 years ago"
    if [[ $RC != 0 && -s "$E/err" ]]; then check "state-failure: non-zero exit"; else fail "state-failure: rc=$RC with an unwritable cursor directory"; fi
  fi
  chmod 700 "$E/mh/cursors"
fi

if want mode-cursor; then
  # Review residual of the adapter slice: the cursor keyed on file sizes and
  # mtimes only, so a --full run after a boundaries run was a no-op. The mode
  # joins the cursor value as "mode": "full" | "boundaries"; a stored full
  # capture covers a later boundaries run; an entry without a mode (written
  # before this change) matches nothing.
  mkenv mode-cursor
  nbatch() { find "$E/q" -maxdepth 1 -name 'batch-*.jsonl' | wc -l; }
  python3 "$FIX" basic "$E/sessions" agent="$CODEX" seed=mc > "$E/x.json"
  run --scan --sessions-dir "$E/sessions" --since "100 years ago"
  r1=$RC; n1="$(nbatch)"
  m1="$(jq -r '[.[].mode] | unique | join(",")' "$E/mh/cursors/acpx-capture.json" 2>/dev/null)"
  if [[ $r1 == 0 && $n1 == 1 && "$m1" == "boundaries" ]]; then
    check "mode-cursor: auto run on a native agent records mode boundaries"
  else
    fail "mode-cursor: rc=$r1 batches=$n1 cursor modes=$m1"
  fi
  run --scan --sessions-dir "$E/sessions" --since "100 years ago" --full
  n2="$(nbatch)"
  last="batch-$((n2 - 1)).jsonl"
  msgs="$(jq -r 'select(.event_type=="acpx_assistant_message") | .source_event_id' "$E/q/$last" 2>/dev/null | wc -l)"
  m2="$(jq -r '[.[].mode] | unique | join(",")' "$E/mh/cursors/acpx-capture.json" 2>/dev/null)"
  if [[ $RC == 0 && $n2 == 2 && $msgs -gt 0 && "$m2" == "full" ]]; then
    check "mode-cursor: --full after boundaries queues intermediate messages"
  else
    fail "mode-cursor: rc=$RC batches=$n2 messages=$msgs cursor modes=$m2"
  fi
  run --scan --sessions-dir "$E/sessions" --since "100 years ago" --full
  run --scan --sessions-dir "$E/sessions" --since "100 years ago"
  run --scan --sessions-dir "$E/sessions" --since "100 years ago" --boundaries-only
  n3="$(nbatch)"
  if [[ $RC == 0 && $n3 == 2 ]]; then
    check "mode-cursor: a full capture covers later full and boundaries runs"
  else
    fail "mode-cursor: rc=$RC, re-queued after a full capture ($n2 -> $n3)"
  fi
  jq 'map_values(del(.mode))' "$E/mh/cursors/acpx-capture.json" > "$E/legacy.json" \
    && cp "$E/legacy.json" "$E/mh/cursors/acpx-capture.json"
  run --scan --sessions-dir "$E/sessions" --since "100 years ago"
  n4="$(nbatch)"
  m4="$(jq -r '[.[].mode] | unique | join(",")' "$E/mh/cursors/acpx-capture.json" 2>/dev/null)"
  if [[ $RC == 0 && $n4 == 3 && "$m4" == "boundaries" ]]; then
    check "mode-cursor: a legacy entry without a mode is re-captured"
  else
    fail "mode-cursor: rc=$RC batches=$n4 cursor modes=$m4 after a legacy entry"
  fi
fi

if want mode-migration; then
  # Review r1 F1 / coordinator C1: the real cursor holds entries written before
  # the mode joined it. A tick scans a short window, so most legacy entries are
  # outside it and are carried through the run untouched. They must not break
  # persistence: the run exits 0, the cursor is written, and the next unchanged
  # run queues nothing.
  mkenv mode-migration
  python3 "$FIX" basic "$E/sessions" agent="$CODEX" seed=mm1 > /dev/null
  python3 "$FIX" basic "$E/sessions" agent="$AGY" seed=mm2 > /dev/null
  run --scan --sessions-dir "$E/sessions" --since "100 years ago"
  jq 'map_values(del(.mode))' "$E/mh/cursors/acpx-capture.json" > "$E/legacy.json" \
    && cp "$E/legacy.json" "$E/mh/cursors/acpx-capture.json"
  n0="$(find "$E/q" -maxdepth 1 -name 'batch-*.jsonl' | wc -l)"
  aged=0
  for f in "$E/sessions"/*; do
    if grep -q antigravity "$f" 2>/dev/null; then
      base="${f##*/}"; base="${base%%.*}"
      for g in "$E/sessions/$base".*; do touch -d "10 days ago" "$g"; aged=$((aged + 1)); done
      break
    fi
  done
  run --scan --sessions-dir "$E/sessions" --since "2 days ago"
  r1=$RC
  n1="$(find "$E/q" -maxdepth 1 -name 'batch-*.jsonl' | wc -l)"
  valid="$(jq '[.[] | select((.files | type) == "object" and (.files | length) > 0)] | length' "$E/mh/cursors/acpx-capture.json" 2>/dev/null)"
  modes="$(jq -r '[.[] | .mode // empty] | unique | join(",")' "$E/mh/cursors/acpx-capture.json" 2>/dev/null)"
  if [[ $aged -gt 0 && $r1 == 0 && $n1 == $((n0 + 1)) && "$modes" == "boundaries" && "$valid" -ge 1 ]]; then
    check "mode-migration: a mixed legacy cursor persists after a short-window run"
  else
    fail "mode-migration: aged=$aged rc=$r1 batches $n0 -> $n1 modes=$modes valid_entries=$valid"
  fi
  if jq -e 'all(.[]; (.files | type) == "object" and (.files | length) > 0)' "$E/mh/cursors/acpx-capture.json" >/dev/null 2>&1; then
    check "mode-migration: every persisted entry keeps a valid fingerprint"
  else
    fail "mode-migration: a persisted entry lost its fingerprint"
  fi
  run --scan --sessions-dir "$E/sessions" --since "2 days ago"
  n2="$(find "$E/q" -maxdepth 1 -name 'batch-*.jsonl' | wc -l)"
  if [[ $RC == 0 && $n2 == "$n1" ]]; then
    check "mode-migration: the next unchanged run queues nothing"
  else
    fail "mode-migration: rc=$RC, re-queued ($n1 -> $n2)"
  fi
fi

if (( FAILURES > 0 )); then
  echo "acpx-journal-capture: $FAILURES failure(s)"
  exit 1
fi
echo "acpx-journal-capture: all passed"
