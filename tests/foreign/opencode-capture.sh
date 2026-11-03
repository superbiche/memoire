#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# opencode-capture.sh — guarantees of the native OpenCode capture adapter
# (scripts/memoire-capture-opencode.sh). Everything runs under a scratch
# directory from mktemp -d (removed at exit); nothing here reads the operator's
# archive of real sessions, the live OpenCode database, or ~/.memoire: every
# adapter run names its database explicitly, pins the fallback to a path that
# does not exist, and runs with HOME and MEMOIRE_HOME inside the scratch. A
# build failure of the queue binary FAILS the suite (it never skips). Every
# content test carries non-ASCII text (accents, CJK, 4-byte characters).
#
# One test per guarantee, each failing when the guarantee breaks:
#   T1   scan mode over more sessions than one selection page
#   T2   the cursor is a position in the stream, written after every batch:
#        event cap, wall-time bound and SIGTERM stop cleanly and PROGRESS
#   T3   no `sqlite3 -json`; every sqlite3 call is -safe; a 5 MB text and a
#        20 MB tool output full of escapes pass exactly and fast; no read
#        overlaps a queue write; a concurrent WAL writer can checkpoint
#   T4   spill files: outside the tool-output dir, a link that leaves it, a
#        named pipe, a directory, a missing file, a relative path are all
#        refused with truncated_by_harness + spill_rejected, never an empty
#        payload; a good file and an archive-copy file are attached whole
#   T5   SQL in an id is never executed (session, message, part ids) and a row
#        with such an id is skipped and counted
#   T6   the exit code and the summary cover every session of the run
#   T7   every key of a part survives (text time, tool metadata, unknown)
#   T8   no id carries two contents: step-finish fold, turn edit, permission
#        across sessions, context epoch
#   T9   no file is created in the database directory; the fallback is taken
#        at any read, not only at the first probe; nothing to read exits 0
#   T10  the outcome id sorts after its call; session facts re-emit when the
#        session changes
#   T11  the default source is one constant
#   T12  the cursor never moves past a failed queue write; a healed rerun
#        completes the set
#   T13  identity is stable on re-scan and cursor loss; an updated row
#        re-emits under a new version, the old version stays
#   T14  nothing is truncated: 200 kB user prompt and assistant text, 50 kB
#        tool output, 300 kB reasoning
#   T15  occurred_at is the record time; scratch attribution through the
#        registry
#   T16  turn rules and text; only the turn is admitted by derivation; the
#        never-read tables stay unread
#   T17  quiet gate over children, stuck-running gate, window, explicit ids
#   T18  census on a SYNTHETIC store (tests/foreign/fixtures/
#        opencode-census.sql): events per type equal the numbers written in
#        the fixture, and a key-level comparison: every key of every source
#        row appears in its event
#   T19  an unchanged session is skipped: done_sig is read back, a second run
#        issues no chunk and no session-level statement, a changed session is
#        read alone
#   T20  a fallback in the middle of a run never advances the primary slot of a
#        cursor: the next run on the primary reads what the archive lacked; no
#        row is skipped and no id carries two contents
#   T21  a SIGTERM to the whole process group is a clean stop: exit 0, one log
#        line, the cursor at the last durable batch, no temporary file in the
#        queue directory, a rerun completes the set
#   T22  the selection reads no table whole: the query plan of every statement
#        of a run contains no full scan of part or message, and the selection
#        walks sessions in pages
#   T23  chunk reads are bounded: rows, bytes, and a slack after the quiet gate
#        (a row changed within the slack is left for the next run)
#   T24  spill content per batch is capped: past the cap the event carries
#        truncated_by_harness and spill_deferred and the part is read again
#   T25  error classes: a malformed database and a malformed row fall back like
#        busy or locked; a file: URI is refused; the parameter is a path
#   T26  ids are validated against [A-Za-z0-9_.-]+: a row that fails is logged,
#        skipped and counted
#   T27  no comment cites a document outside the repository
#   T28  sqlite3 (>= 3.44) and jq (>= 1.7) versions checked once there is a database to read (exit 1,
#        one log line); nothing to read exits 0 without any check
#   T29  a full scratch directory fails loudly: exit 1, one log line
#   T30  the work directory is private (0700/0600) under the Memoire home,
#        never in TMPDIR; kill -9 leaves one, the next run clears it
#   T32  a BLOB in message.data or part.data is captured as base64 (raw_blob,
#        blob: true) and counted; its session is not poisoned
#   T33  a part the Memoire plugin injected (id prefix prt_memoire_) is read,
#        advances the cursor and is never queued, nor is it in the turn text;
#        any other synthetic part is still captured
#
# Manual check on real personal data (NOT run by make test): build a scratch
# copy of the sessions you may read from the archive of real sessions (open the
# archive with immutable=1, copy the rows of a few sessions into a scratch
# database with the same schema), then run
#   MEMOIRE_OPENCODE_TEST_REAL_DB=<scratch copy> \
#     bash tests/foreign/opencode-capture.sh census-real
# which runs only the census comparison (ids, types, times, keys, texts, bytes
# of non-ASCII strings) against that database. It prints identifiers, types and
# counts only.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# Mutation testing points this at a faulty adapter copy.
ADAPTER="${MEMOIRE_TEST_ADAPTER:-$ROOT/scripts/memoire-capture-opencode.sh}"
FIXTURE_SQL="$ROOT/tests/foreign/fixtures/opencode-census.sql"
ONLY="${1:-}"

fail() { echo "FAIL: $1" >&2; exit 1; }
command -v jq >/dev/null || fail "jq not available"
command -v sqlite3 >/dev/null || fail "sqlite3 not available"
command -v go >/dev/null || fail "go not available (the suite needs the real queue binary)"
[[ -x "$ADAPTER" ]] || fail "adapter not executable: $ADAPTER"
[[ -r "$FIXTURE_SQL" ]] || fail "fixture missing: $FIXTURE_SQL"
case "${MEMOIRE_CAPTURE:-}" in off|0|false)
  fail "MEMOIRE_CAPTURE=$MEMOIRE_CAPTURE in this shell would kill the adapter"
esac
REAL_SQLITE3="$(command -v sqlite3)"

# Scratch: ${TMPDIR:-/var/tmp}, or /tmp when that is not writable (a sandbox).
SCRATCH=""
for base in "${TMPDIR:-}" /var/tmp /tmp; do
  [[ -n "$base" && -d "$base" && -w "$base" ]] || continue
  SCRATCH="$(mktemp -d "$base/opencode-capture.XXXXXX" 2>/dev/null)" && break
  SCRATCH=""
done
[[ -n "$SCRATCH" ]] || fail "cannot create a scratch directory under TMPDIR, /var/tmp or /tmp"
cleanup() { rm -rf "${SCRATCH:?}"; }
trap cleanup EXIT
mkdir -p "$SCRATCH/home" "$SCRATCH/tmp" "$SCRATCH/bin" "$SCRATCH/tool-output"
SCRATCH="$(realpath "$SCRATCH")"

# Real queue binary, built from this tree. A failed build is a FAILED suite.
QBIN="$SCRATCH/bin/memoire-queue"
(cd "$ROOT/services/api/cmd/queue" && go build -o "$QBIN" .) >"$SCRATCH/build.log" 2>&1 \
  || { cat "$SCRATCH/build.log" >&2; fail "go build of memoire-queue failed"; }

# --- environment of every adapter run -----------------------------------------
# MH: the scratch Memoire home of the current test. OCENV: extra env assignments.
MH="$SCRATCH/mh"
OCENV=()
TOOLDIR="$SCRATCH/tool-output"
FALLBACK_DB="$SCRATCH/no-such-archive.db"
SQLITE_BIN="$REAL_SQLITE3"
QUEUE="$QBIN"

oc_prep() { # oc_prep <db> -> OCC = the fenced adapter command (env-prefixed)
  local db="$1"
  OCC=(env HOME="$SCRATCH/home" MEMOIRE_HOME="$MH" TMPDIR="$SCRATCH/tmp"
    MEMOIRE_OPENCODE_DB="$db" MEMOIRE_OPENCODE_DB_FALLBACK="$FALLBACK_DB"
    MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR="$TOOLDIR"
    MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR_FALLBACK="$SCRATCH/no-such-tool-fallback"
    MEMOIRE_OPENCODE_SQLITE3="$SQLITE_BIN"
    MEMOIRE_PROJECTS_REGISTRY="$SCRATCH/no-such-registry.json"
    MEMOIRE_QUEUE_BIN="$QUEUE" MEMOIRE_REDACT=0
    MEMOIRE_RESOLVE_PROJECT="$ROOT/scripts/memoire-resolve-project.sh"
    ${OCENV[@]+"${OCENV[@]}"} "$ADAPTER")
}
oc() { # oc <db> [adapter args...] — the fenced adapter run
  local db="$1"; shift
  oc_prep "$db"
  "${OCC[@]}" "$@"
}

new_home() { # new_home <name> — fresh scratch Memoire home + recording state
  MH="$SCRATCH/mh-$1"
  rm -rf "$MH"; mkdir -p "$MH"
  REC="$SCRATCH/rec-$1"
  rm -rf "$REC"; mkdir -p "$REC"
  QDIR="$MH/queue/pending"
  CURS="$MH/capture-cursors/opencode"
  OCENV=(); QUEUE="$QBIN"; SQLITE_BIN="$REAL_SQLITE3"
}

qfiles() { [[ -d "$QDIR" ]] && find "$QDIR" -maxdepth 1 -name '*.json' -print; }
qids() { qfiles | xargs -r jq -r '.source_event_id' | LC_ALL=C sort -u; }
qcount() { qfiles | wc -l; }
qevent() { # qevent <source_event_id> -> the queue JSON of that id (first)
  qfiles | xargs -r jq -c --arg id "$1" 'select(.source_event_id == $id)' | head -1
}

# --- recording stub queue (understands -batch-file) ---------------------------
# Records every batch in $REC, writes nothing to any queue. Failure injection:
#   STUB_FAIL_FROM=k   the k+1-th item of the whole run (and all later) fails
#   STUB_FAIL_SESSION=s any batch that contains session s fails at its first item
#   STUB_SLEEP=x       seconds slept per call
make_stub() {
  cat >"$SCRATCH/bin/stub-queue" <<'STUB'
#!/usr/bin/env bash
set -u
: "${REC:?}"
if [[ "$1" == "-batch-file" ]]; then
  f="$2"
else
  # one event given as flags: a one-item batch
  declare -A a
  while [[ $# -gt 0 ]]; do case "$1" in -*) a[${1#-}]="${2:-}"; shift 2 ;; *) shift ;; esac; done
  f="$(mktemp)"
  jq -nc --arg project "${a[project]:-}" --arg sid "${a[session-id]:-}" --arg ev "${a[event-type]:-}" \
    --arg id "${a[source-event-id]:-}" --arg at "${a[occurred-at]:-}" --arg cwd "${a[cwd]:-}" \
    --slurpfile payload "${a[payload-file]:-/dev/null}" --rawfile text "${a[text-file]:-/dev/null}" \
    '{project: $project, session_id: $sid, event_type: $ev, source_event_id: $id, occurred_at: $at, cwd: $cwd,
      payload: ($payload[0] // {})} + (if $text != "" then {text: $text} else {} end)' >"$f"
fi
s=$(date +%s%3N)
n=$(( $(cat "$REC/ncalls" 2>/dev/null || echo 0) + 1 )); echo "$n" >"$REC/ncalls"
[[ -n "${STUB_SLEEP:-}" ]] && sleep "$STUB_SLEEP"
total=$(wc -l <"$f")
done_so_far=$(cat "$REC/nitems" 2>/dev/null || echo 0)
write=$total
if [[ -n "${STUB_FAIL_SESSION:-}" ]] && grep -q "\"session_id\":\"$STUB_FAIL_SESSION\"" "$f"; then
  write=0
elif [[ -n "${STUB_FAIL_FROM:-}" ]] && (( done_so_far + total > STUB_FAIL_FROM )); then
  write=$(( STUB_FAIL_FROM - done_so_far )); (( write < 0 )) && write=0
fi
head -n "$write" "$f" >>"$REC/items.jsonl"
echo $(( done_so_far + write )) >"$REC/nitems"
i=0; while (( i < write )); do echo "stub-path-$i"; i=$((i + 1)); done
printf '%s %s %s\n' "$s" "$(date +%s%3N)" "$n" >>"$REC/calls.log"
(( write < total )) && { echo "stub: injected failure at item $((write + 1))" >&2; exit 1; }
exit 0
STUB
  chmod +x "$SCRATCH/bin/stub-queue"
}
make_stub
use_stub() { QUEUE="$SCRATCH/bin/stub-queue"; OCENV+=(REC="$REC"); }
rec_items() { [[ -s "$REC/items.jsonl" ]] && cat "$REC/items.jsonl"; return 0; }
rec_ids() { rec_items | jq -r '.source_event_id' | LC_ALL=C sort -u; }
rec_count() { rec_items | wc -l; }

# --- sqlite3 shim: timings, argv, failure injection ---------------------------
make_shim() {
  cat >"$SCRATCH/bin/sqlite3-shim" <<SHIM
#!/usr/bin/env bash
real="$REAL_SQLITE3"
log="\${SHIM_LOG:-/dev/null}"
s=\$(date +%s%3N)
[[ -n "\${SHIM_SQL_LOG:-}" ]] && printf '%s\\n@@STMT@@\\n' "\${!#}" >>"\$SHIM_SQL_LOG"
if [[ -n "\${SHIM_FAIL_MATCH:-}" && "\$*" == *"\$SHIM_FAIL_MATCH"* ]]; then
  n=\$(( \$(cat "\$SHIM_COUNT" 2>/dev/null || echo 0) + 1 )); echo "\$n" >"\$SHIM_COUNT"
  if (( n > \${SHIM_FAIL_AFTER:-0} )); then
    echo "Error: \${SHIM_FAIL_TEXT:-database is locked}" >&2
    printf '%s %s 5 %s\n' "\$s" "\$(date +%s%3N)" "\${*:1:\$(( \$# - 1 ))}" >>"\$log"
    exit 5
  fi
fi
"\$real" "\$@"; rc=\$?
printf '%s %s %s %s\n' "\$s" "\$(date +%s%3N)" "\$rc" "\${*:1:\$(( \$# - 1 ))}" >>"\$log"
exit \$rc
SHIM
  chmod +x "$SCRATCH/bin/sqlite3-shim"
}
make_shim
# The shim is found through PATH under the name sqlite3, so ANY adapter that runs
# sqlite3 (this round's or an older one) goes through it.
mkdir -p "$SCRATCH/shimpath"
ln -sf "$SCRATCH/bin/sqlite3-shim" "$SCRATCH/shimpath/sqlite3"
use_shim() { SQLITE_BIN=sqlite3; SHIM_LOG="$REC/sqlite.log"; : >"$SHIM_LOG"; OCENV+=(PATH="$SCRATCH/shimpath:$PATH" SHIM_LOG="$SHIM_LOG" SHIM_COUNT="$REC/shim.count"); }
shim_max_ms() { awk '{d=$2-$1; if (d>m) m=d} END {print m+0}' "$SHIM_LOG"; }

# --- fixture builders ---------------------------------------------------------
CENSUS_DB="$SCRATCH/census-schema.db"
sed "s|__TOOL_OUT__|$TOOLDIR|g" "$FIXTURE_SQL" | sqlite3 "$CENSUS_DB" || fail "fixture SQL does not load"
SCHEMA_SQL="$(sqlite3 "$CENSUS_DB" .schema)"

mkdb() { sqlite3 "$1" <<<"$SCHEMA_SQL"; }
sqlq() { printf '%s' "${1//\'/\'\'}"; }
NOW_MS="$(date +%s%3N)"
OLD=1767225600000   # 2026-01-01T00:00:00Z: past every gate and window

ins_session() { # db id dir [tc] [tu] [project_id]
  sqlite3 "$1" "INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES ('$(sqlq "$2")', '${6:-prj_t}', 's', '$(sqlq "$3")', 't $(sqlq "$2")', '1', ${4:-$OLD}, ${5:-$OLD});"
}
ins_message() { # db id sid role tc mu [completed|0] [error_json]
  local comp="" err=""
  [[ -n "${7:-}" && "$7" != "0" ]] && comp=",\"completed\":$7"
  [[ -n "${8:-}" ]] && err=",\"error\":$8"
  sqlite3 "$1" "INSERT INTO message VALUES ('$(sqlq "$2")', '$(sqlq "$3")', ${5:-$OLD}, ${6:-$OLD}, '{\"role\":\"$4\",\"time\":{\"created\":${5:-$OLD}${comp}}${err}}');"
}
ins_part() { # db id mid sid tc mu data-json
  sqlite3 "$1" "INSERT INTO part VALUES ('$(sqlq "$2")', '$(sqlq "$3")', '$(sqlq "$4")', ${5:-$OLD}, ${6:-$OLD}, '$(sqlq "$7")');"
}

# --- census comparison (T18): independent of the adapter's SQL ------------------
# Reads the source rows with `sqlite3 -json` (fixtures are small) and compares
# every part and message row with the events that carry it, KEY BY KEY.
read -r -d '' CENSUS_JQ <<'JQ' || true
def iso: . as $ms | (($ms / 1000) | floor | todate | .[0:19]) + "." + (("000" + (($ms % 1000) | tostring))[-3:]) + "Z";
def src: . as $raw | (try fromjson catch "\u0000invalid") as $x
  | if $x == "\u0000invalid" then {raw: $raw} elif ($x | type) == "object" then $x else {raw: $x} end;
$ev[0] as $E
| $parts[0] as $P
| $msgs[0] as $M
| (reduce $E[] as $e ({};
    ($e.id | split(":")) as $k
    | if $k[0] == "opencode" and $k[1] == "part" then .parts[$k[2]] += [$e]
      elif $k[0] == "opencode" and $k[1] == "message" then .msgs[$k[2]] += [$e]
      elif $k[0] == "opencode" and $k[1] == "turn" then .turns[$k[2]] += [$e]
      else . end)) as $ix
| (reduce $P[] as $p ({}; .[$p.message_id] += [$p])) as $pbm
| [
    ($P[] as $r
     | ($r.data | src) as $d
     | ($d.type // "") as $ty
     | ($ix.parts[$r.id] // []) as $evs
     | if ($ty == "step-start" or $ty == "step-finish") then
         (if ($evs | length) != 0 then "part \($r.id): a framing part produced an event" else empty end)
       elif ($evs | length) == 0 then "part \($r.id): no event"
       else
         ($evs[] | select(.at != ($r.tc | iso)) | "part \($r.id): occurred_at differs"),
         (if $ty == "tool" then
            (($evs | map(select(.id | endswith(":outcome"))) | .[0]) as $o
             | ($evs | map(select(.id | endswith(":outcome") | not)) | .[0]) as $c
             | (if $o == null or $c == null then "part \($r.id): call/outcome pair incomplete"
                else
                  (if $o.p.state_out != $d.state then "part \($r.id): state_out differs from the source state" else empty end),
                  (if $o.p.part != ($d | del(.state)) then "part \($r.id): outcome lost a part-level key" else empty end),
                  (if $c.p.part != ($d | del(.state)) then "part \($r.id): call lost a part-level key" else empty end),
                  (if $c.p.state.input != $d.state.input then "part \($r.id): call input differs" else empty end)
                end))
          else
            ($evs[] | (if .p.part != $d then "part \($r.id): a source key is absent or altered in the event" else empty end),
                      (if ($d | has("text")) and (.p | has("text")) and .p.text != $d.text then "part \($r.id): text differs" else empty end))
          end)
       end),
    ($M[] as $r
     | (($ix.turns[$r.id] // []) | .[0]) as $turn
     | (if $turn == null then empty
        else
          (($pbm[$r.id] // [])
              | map(select((.data | try fromjson catch {}) | (type == "object") and .type == "text")
                    | {tc: .tc, id: .id, t: ((.data | fromjson).text // "")})
              | map(select(.t != "")) | sort_by([.tc, .id]) | map(.t) | join("\n\n")) as $want
          | (if $turn.text != $want then "message \($r.id): the turn text is not the text parts joined in (time_created, id) order" else empty end)
        end)),
    ($M[] as $r
     | ($r.data | src) as $d
     | ($ix.msgs[$r.id] // []) as $evs
     | (($pbm[$r.id] // []) | map(select(((.data | try fromjson catch {}) | .type) == "step-finish"))
            | sort_by([.tc, .id]) | map((.data | fromjson) | del(.type))) as $sf
     | if ($evs | length) == 0 then "message \($r.id): no event"
       else
         ($evs[] | (if .p.message != $d then "message \($r.id): payload differs from the source row" else empty end),
                   (if .p.step_finish != $sf then "message \($r.id): step_finish fold differs from its step-finish parts" else empty end),
                   (if .at != ($r.tc | iso) then "message \($r.id): occurred_at differs" else empty end))
       end)
  ] | .[]
JQ

census_compare() { # <source db> <queue dir> -> prints mismatches, returns their count
  local db="$1" qd="$2" out
  find "$qd" -maxdepth 1 -name '*.json' -print0 | xargs -0 -r jq -c '{id: .source_event_id, t: .event_type, at: .occurred_at, text: .text, p: .payload}' | jq -s '.' >"$SCRATCH/census-ev.json"
  # one JSON object per row, built by SQL (linear in the row size; `sqlite3 -json` is superlinear on escapes)
  sqlite3 "$db" "SELECT json_object('id', id, 'message_id', message_id, 'tc', time_created, 'mu', time_updated, 'data', data) FROM part;" | jq -s '.' >"$SCRATCH/census-parts.json"
  sqlite3 "$db" "SELECT json_object('id', id, 'tc', time_created, 'mu', time_updated, 'data', data) FROM message;" | jq -s '.' >"$SCRATCH/census-msgs.json"
  out="$(jq -n -r --slurpfile ev "$SCRATCH/census-ev.json" --slurpfile parts "$SCRATCH/census-parts.json" \
        --slurpfile msgs "$SCRATCH/census-msgs.json" "$CENSUS_JQ" | sed 's/^/   census: /')"
  [[ -z "$out" ]] || printf '%s\n' "$out" >&2
  [[ -z "$out" ]] && return 0
  return 1
}


bytes_compare() { # <source db> <queue dir>: counts and bytes of the text and tool-output strings, store against events
  local db="$1" qd="$2" sq eq st et
  sq="$(sqlite3 "$db" "SELECT count(*) || ' ' || COALESCE(sum(octet_length(json_extract(data, '\$.text'))), 0) FROM part WHERE json_valid(data) AND json_extract(data, '\$.type') IN ('text', 'reasoning') AND json_extract(data, '\$.text') IS NOT NULL;")"
  eq="$(find "$qd" -maxdepth 1 -name '*.json' -print0 | xargs -0 -r jq -r 'select((.payload.part.type == "text" or .payload.part.type == "reasoning") and (.payload.text != null)) | (.payload.text | utf8bytelength)' | awk '{n++; b += $1} END {print n+0, b+0}')"
  st="$(sqlite3 "$db" "SELECT count(*) || ' ' || COALESCE(sum(octet_length(json_extract(data, '\$.state.output'))), 0) FROM part WHERE json_valid(data) AND json_extract(data, '\$.type') = 'tool' AND json_extract(data, '\$.state.output') IS NOT NULL;")"
  et="$(find "$qd" -maxdepth 1 -name '*.json' -print0 | xargs -0 -r jq -r 'select((.event_type == "opencode_tool_result" or .event_type == "opencode_tool_failure") and (.payload.state_out.output != null)) | (.payload.state_out.output | utf8bytelength)' | awk '{n++; b += $1} END {print n+0, b+0}')"
  echo "   text and reasoning strings (count bytes): store $sq, events $eq"
  echo "   tool outputs (count bytes): store $st, events $et"
  [[ "$sq" == "$eq" && "$st" == "$et" ]]
}
pass() { echo "   $1 ok${2:+ ($2)}"; }
count_type() { rec_items | jq -r 'select(.event_type == "'"$1"'") | .source_event_id' | wc -l; }

run_test() { [[ -z "$ONLY" || "$ONLY" == "all" || "$ONLY" == "$1" ]]; }

if [[ "$ONLY" != "census-real" ]]; then

# =============================================================================
if run_test T1; then
echo "== T1 scan mode over more sessions than one selection page"
new_home t1; use_stub
DB="$SCRATCH/t1.db"; mkdb "$DB"
RECENT=$(( NOW_MS - 3 * 3600 * 1000 ))
{
  echo "BEGIN;"
  for i in $(seq 0 59); do
    id="$(printf 's%03d' "$i")"
    if (( i < 30 )); then t=$OLD; else t=$RECENT; fi
    echo "INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES ('$id', 'prj_t', 's', '/tmp/t1proj', 't', '1', $t, $t);"
    echo "INSERT INTO message VALUES ('m$id', '$id', $t, $t, '{\"role\":\"user\",\"time\":{\"created\":$t}}');"
    echo "INSERT INTO part VALUES ('p$id', 'm$id', '$id', $t, $t, '{\"type\":\"text\",\"text\":\"hello $id\"}');"
  done
  echo "COMMIT;"
} | sqlite3 "$DB"
oc "$DB" --scan >"$SCRATCH/t1.out" 2>"$SCRATCH/t1.err"; rc=$?
[[ "$rc" == "0" ]] || fail "T1: scan run over 60 sessions exited $rc: $(tail -3 "$SCRATCH/t1.err")"
facts="$(rec_ids | grep -c '^opencode:session:s[0-9]*:facts:')"
[[ "$facts" == "30" ]] || fail "T1: expected the 30 active sessions (all placed after the first 25) captured, got $facts: $(tail -2 "$SCRATCH/t1.err")"
rec_ids | grep -q '^opencode:session:s000:facts' && fail "T1: a session outside the window was captured"
rec_ids | grep -q '^opencode:session:s059:facts' || fail "T1: the last session was not captured"
[[ "$(rec_count)" == "90" ]] || fail "T1: expected 30 sessions x 3 events, got $(rec_count)"
# --full drops the window: all 60, still more than one selection batch
new_home t1f; use_stub
oc "$DB" --full >/dev/null 2>"$SCRATCH/t1.err" || fail "T1: --full run failed: $(tail -3 "$SCRATCH/t1.err")"
[[ "$(rec_ids | grep -c '^opencode:session:s[0-9]*:facts:')" == "60" ]] || fail "T1: --full must capture all 60 sessions"
pass T1 "30 of 60 in scan, 60 in full, rc=0"
fi

# =============================================================================
if run_test T2; then
echo "== T2 the cursor is written after every batch; caps and bounds stop cleanly and progress"
DB="$SCRATCH/t2.db"; mkdb "$DB"
{
  echo "BEGIN;"
  echo "INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES ('sA', 'prj_t', 's', '/tmp/t2proj', 't', '1', $OLD, $OLD);"
  echo "INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES ('sB', 'prj_t', 's', '/tmp/t2proj', 't', '1', $((OLD + 900000)), $((OLD + 900000)));"
  for i in $(seq 1 300); do
    t=$(( OLD + i * 1000 ))
    echo "INSERT INTO message VALUES ('mA$i', 'sA', $t, $((t + 500)), '{\"role\":\"assistant\",\"time\":{\"created\":$t,\"completed\":$((t + 400))}}');"
    echo "INSERT INTO part VALUES ('pAt$i', 'mA$i', 'sA', $((t + 10)), $((t + 10)), '{\"type\":\"text\",\"text\":\"answer $i é日本\"}');"
    echo "INSERT INTO part VALUES ('pAx$i', 'mA$i', 'sA', $((t + 20)), $((t + 30)), '{\"type\":\"tool\",\"tool\":\"bash\",\"callID\":\"c$i\",\"state\":{\"status\":\"completed\",\"input\":{},\"output\":\"o$i\",\"time\":{\"start\":1,\"end\":2}}}');"
  done
  echo "INSERT INTO message VALUES ('mB1', 'sB', $((OLD + 900000)), $((OLD + 900000)), '{\"role\":\"user\",\"time\":{\"created\":$((OLD + 900000))}}');"
  echo "INSERT INTO part VALUES ('pB1', 'mB1', 'sB', $((OLD + 900000)), $((OLD + 900000)), '{\"type\":\"text\",\"text\":\"small session\"}');"
  echo "COMMIT;"
} | sqlite3 "$DB"
new_home t2full; use_stub
oc "$DB" --full >/dev/null 2>&1 || fail "T2: the unbounded reference run failed"
FULLIDS="$(rec_ids)"
NFULL="$(grep -c . <<<"$FULLIDS")"
[[ "$NFULL" -gt 1500 ]] || fail "T2: reference run too small ($NFULL ids)"
# (a) event cap: repeated capped runs must complete the set, no id twice
new_home t2cap; use_stub
OCENV+=(MEMOIRE_OPENCODE_MAX_EVENTS=200)
prev=0; runs=0
while :; do
  runs=$((runs + 1)); (( runs <= 40 )) || fail "T2: 40 capped runs did not finish the session (livelock)"
  oc "$DB" --full >/dev/null 2>"$SCRATCH/t2.err"; rc=$?
  [[ "$rc" == "0" ]] || fail "T2: a capped run exited $rc: $(tail -2 "$SCRATCH/t2.err")"
  now="$(rec_count)"
  if grep -q 'stopped cleanly on cap' "$SCRATCH/t2.err"; then
    (( now > prev )) || fail "T2: capped run $runs made no progress ($now items, cursor stuck)"
    [[ -s "$CURS/sA.json" ]] || fail "T2: no cursor after a capped run"
  else
    break
  fi
  prev="$now"
done
[[ "$(rec_ids)" == "$FULLIDS" ]] || fail "T2: the capped runs did not complete the same id set ($(rec_ids | wc -l) vs $NFULL)"
[[ "$(rec_count)" == "$NFULL" ]] || fail "T2: ids were queued more than once across capped runs ($(rec_count) items for $NFULL ids)"
(( runs >= 3 )) || fail "T2: the cap did not bound the runs (only $runs)"
# (b) wall-time bound with a slow queue: stops cleanly, progress, exit 0
new_home t2time; use_stub
OCENV+=(MEMOIRE_OPENCODE_RUN_BUDGET_S=1 MEMOIRE_OPENCODE_ROW_CHUNK=50 STUB_SLEEP=0.4)
oc "$DB" --full >/dev/null 2>"$SCRATCH/t2.err"; rc=$?
[[ "$rc" == "0" ]] || fail "T2: a run stopped by its time bound exited $rc"
grep -q 'stopped cleanly on time' "$SCRATCH/t2.err" || fail "T2: no clean-stop log on the time bound: $(tail -2 "$SCRATCH/t2.err")"
grep -q 'session(s) left' "$SCRATCH/t2.err" || fail "T2: the log does not say what is left"
n1="$(rec_count)"; [[ "$n1" -gt 0 && "$n1" -lt "$NFULL" ]] || fail "T2: time-bounded run queued $n1 of $NFULL"
[[ -s "$CURS/sA.json" ]] || fail "T2: no cursor after the time-bounded run"
oc "$DB" --full >/dev/null 2>&1
n2="$(rec_count)"; (( n2 > n1 )) || fail "T2: second time-bounded run made no progress ($n1 -> $n2)"
[[ "$(rec_count)" == "$(rec_ids | wc -l)" ]] || fail "T2: an id was re-queued after a time-bounded stop"
# (c) SIGTERM: a clean stop at the next batch boundary, cursor kept, exit 0
new_home t2term; use_stub
OCENV+=(STUB_SLEEP=1 MEMOIRE_OPENCODE_ROW_CHUNK=50)
oc_prep "$DB"; "${OCC[@]}" --full >/dev/null 2>"$SCRATCH/t2.err" &
apid=$!
sleep 3.5; kill -TERM "$apid" 2>/dev/null
wait "$apid"; rc=$?
[[ "$rc" == "0" ]] || fail "T2: SIGTERM stop exited $rc (want a clean 0): $(tail -2 "$SCRATCH/t2.err")"
grep -q 'stopped cleanly on signal' "$SCRATCH/t2.err" || fail "T2: no clean-stop log after SIGTERM"
[[ -s "$CURS/sA.json" ]] || fail "T2: no cursor after SIGTERM"
pass T2 "cap: $runs runs, no id twice; time bound and SIGTERM stop cleanly with progress"
fi

# =============================================================================
if run_test T3; then
echo "== T3 reading: no sqlite3 -json, -safe, linear cost on huge escaped rows, short reads, a concurrent WAL writer can checkpoint"
# (a) argv of every invocation
new_home t3a; use_stub; use_shim
DB="$SCRATCH/t3a.db"; mkdb "$DB"
ins_session "$DB" s3a /tmp/t3aproj
ins_message "$DB" m3a s3a assistant $OLD $((OLD + 2000)) $((OLD + 1900))
ins_part "$DB" p3a m3a s3a $((OLD + 100)) $((OLD + 100)) '{"type":"text","text":"answer"}'
ins_part "$DB" p3b m3a s3a $((OLD + 200)) $((OLD + 200)) '{"type":"tool","tool":"bash","callID":"c","state":{"status":"completed","input":{},"output":"o","time":{"start":1,"end":2}}}'
oc "$DB" --full >/dev/null 2>&1
grep -q ' -json' "$SHIM_LOG" && fail "T3: sqlite3 -json is used"
[[ "$(awk 'NF > 3' "$SHIM_LOG" | grep -vc -- ' -safe ')" == "0" ]] || fail "T3: an sqlite3 call is not -safe"
[[ "$(wc -l <"$SHIM_LOG")" -ge 5 ]] || fail "T3: shim saw too few calls"
# (b) huge rows full of escapes
DB="$SCRATCH/t3b.db"; mkdb "$DB"
ins_session "$DB" big /tmp/t3proj
ins_message "$DB" mb1 big user $OLD $OLD
ins_message "$DB" mb2 big assistant $((OLD + 1000)) $((OLD + 2000)) $((OLD + 2000))
LINE='aé日😀 "q" \ tab z'
NL="$(printf '%s\n' "$LINE" | LC_ALL=C wc -c)"
yes "$LINE" | head -n $(( 5000000 / NL )) >"$SCRATCH/big.txt"
jq -Rs '{type: "text", text: .}' "$SCRATCH/big.txt" >"$SCRATCH/big-text.json"
yes "$LINE" | head -n $(( 20000000 / NL )) >"$SCRATCH/huge.txt"
jq -Rs '{type: "tool", tool: "bash", callID: "cbig", state: {status: "completed", input: {}, title: "t", time: {start: 1, end: 2}, output: .}}' "$SCRATCH/huge.txt" >"$SCRATCH/huge-tool.json"
sqlite3 "$DB" "INSERT INTO part VALUES ('pbt', 'mb1', 'big', $OLD, $OLD, CAST(readfile('$SCRATCH/big-text.json') AS TEXT));
INSERT INTO part VALUES ('pbx', 'mb2', 'big', $((OLD + 1100)), $((OLD + 1100)), CAST(readfile('$SCRATCH/huge-tool.json') AS TEXT));"
SRC_TXT="$(sqlite3 "$DB" "SELECT length(json_extract(data, '\$.text')) FROM part WHERE id = 'pbt';")"
SRC_OUT="$(sqlite3 "$DB" "SELECT length(json_extract(data, '\$.state.output')) FROM part WHERE id = 'pbx';")"
new_home t3b; use_shim
OCENV+=(MEMOIRE_OPENCODE_READ_TIMEOUT_S=100)
t0="$(date +%s)"
oc_prep "$DB"; timeout 100 "${OCC[@]}" --full >/dev/null 2>"$SCRATCH/t3b.err"
rc=$?
t1="$(date +%s)"
[[ "$rc" == "0" ]] || fail "T3: the huge-row run exited $rc after $((t1 - t0)) s: $(tail -2 "$SCRATCH/t3b.err")"
(( t1 - t0 <= 60 )) || fail "T3: the huge-row run took $((t1 - t0)) s"
[[ "$(qevent "opencode:part:pbt:u$(printf '%016d' $OLD)" | jq -r '.payload.text | length')" == "$SRC_TXT" ]] || fail "T3: the 5 MB text is not exact"
[[ "$(qevent "opencode:part:pbt:u$(printf '%016d' $OLD)" | jq -j '.payload.text' | sha256sum)" == "$(sqlite3 "$DB" "SELECT json_extract(data, '\$.text') FROM part WHERE id = 'pbt';" | head -c -1 | sha256sum)" ]] || fail "T3: the 5 MB multibyte text is not byte-identical"
grep -qiE $'\xef\xbf\xbd|\\\\ufffd' "$QDIR"/*.json && fail "T3: a replacement character (U+FFFD) appeared in an event"
[[ "$(qevent "opencode:part:pbx:u$(printf '%016d' $((OLD + 1100))):outcome" | jq -r '.payload.state_out.output | length')" == "$SRC_OUT" ]] || fail "T3: the 20 MB tool output is not exact"
MAXMS="$(shim_max_ms)"
(( MAXMS < 1000 )) || fail "T3: the longest read transaction was $MAXMS ms (want < 1000)"
echo "   huge rows: 5 MB + 20 MB, run $((t1 - t0)) s, longest sqlite3 process $MAXMS ms"
# (c) no read overlaps a queue write (the stub logs its own intervals)
new_home t3c; use_stub; use_shim
oc "$SCRATCH/t3a.db" --full >/dev/null 2>&1 || fail "T3: overlap run failed"
overl="$(awk 'NR==FNR {qs[NR]=$1; qe[NR]=$2; n=NR; next} {for (i=1;i<=n;i++) if ($1 < qe[i] && $2 > qs[i]) c++} END {print c+0}' "$REC/calls.log" "$SHIM_LOG")"
[[ "$overl" == "0" ]] || fail "T3: $overl read(s) overlapped a queue write"
# (d) a concurrent WAL writer: the log must be checkpointable while the adapter runs.
# A control comes first: the same noise writer and checkpointer with NO adapter, for a window as long as a
# quiet adapter run. A slow disk can starve the checkpointer by itself; a window in which even the control
# completes no checkpoint cannot show the property, so the attempt is void and repeated (3 attempts), then
# part (d) is SKIPPED. Only when the control succeeds does an adapter window without a successful
# checkpoint fail. MEMOIRE_TEST_T3D_ADAPTER (a fault-injection copy of the adapter) and
# MEMOIRE_TEST_T3D_STARVE=1 (a reader held by the test itself, to exercise the void path) only serve the
# verification of this logic.
T3D_ADAPTER="${MEMOIRE_TEST_T3D_ADAPTER:-$ADAPTER}"
new_home t3dcal; use_stub
CALDB="$SCRATCH/wal-cal.db"; cp "$DB" "$CALDB"; sqlite3 "$CALDB" "PRAGMA journal_mode=WAL;" >/dev/null
OCENV+=(MEMOIRE_OPENCODE_READ_TIMEOUT_S=60)
c0="$(date +%s%3N)"; oc "$CALDB" --full >/dev/null 2>&1; c1="$(date +%s%3N)"
CTRL_MS=$(( c1 - c0 )); (( CTRL_MS < 2000 )) && CTRL_MS=2000; (( CTRL_MS > 15000 )) && CTRL_MS=15000
WDB="$SCRATCH/wal.db"; WALM="$SCRATCH/wal-measures.txt"
t3d_noise_start() {
  cp "$DB" "$WDB"; rm -f "$WDB-wal" "$WDB-shm"
  sqlite3 "$WDB" "PRAGMA journal_mode=WAL; CREATE TABLE noise (x blob);" >/dev/null
  rm -f "$SCRATCH/wal.sql"; mkfifo "$SCRATCH/wal.sql"
  touch "$SCRATCH/wal.run"
  ( while [[ -e "$SCRATCH/wal.run" ]]; do echo "INSERT INTO noise VALUES (randomblob(2000));"; sleep 0.01; done ) >"$SCRATCH/wal.sql" &
  wpid1=$!
  sqlite3 "$WDB" <"$SCRATCH/wal.sql" >/dev/null 2>&1 &
  wpid2=$!
  wpid4=""
  sleep 0.5
  if [[ "${MEMOIRE_TEST_T3D_STARVE:-}" == "1" ]]; then   # once the log holds frames: a reader at mark 0 would not block
    sqlite3 "$WDB" "BEGIN; SELECT count(*) FROM noise;" ".shell sleep 20" >/dev/null 2>&1 &
    wpid4=$!
    sleep 0.2
  fi
  : >"$WALM"
  ( while [[ -e "$SCRATCH/wal.run" ]]; do
      wb="$(stat -c %s "$WDB-wal" 2>/dev/null || echo 0)"
      r="$(sqlite3 "$WDB" ".timeout 50" "PRAGMA wal_checkpoint(TRUNCATE);" 2>/dev/null | tr '|' ' ')"
      printf '%s %s %s\n' "$(date +%s%3N)" "${r:-9 0 0}" "$wb" >>"$WALM"
      sleep 0.2
    done ) &
  wpid3=$!
  sleep 0.4
}
t3d_noise_stop() {
  rm -f "$SCRATCH/wal.run"; sleep 0.5
  [[ -n "$wpid4" ]] && pkill -P "$wpid4" 2>/dev/null   # the holder's sleeper, by parent pid (never by pattern)
  kill "$wpid1" "$wpid2" "$wpid3" ${wpid4:+"$wpid4"} 2>/dev/null
  wait 2>/dev/null
}
t3d_window() { # t3d_window <from ms> <to ms> -> "attempted succeeded"
  awk -v a="$1" -v b="$2" '$1 >= a && $1 <= b {n++; if ($2 == 0) k++} END {print n+0, k+0}' "$WALM"
}
T3D_SKIP=""; T3D_DONE=""
for attempt in 1 2 3; do
  new_home t3d; use_stub; use_shim
  t3d_noise_start
  k0="$(date +%s%3N)"; sleep "$(awk -v m="$CTRL_MS" 'BEGIN {printf "%.3f", m / 1000}')"; k1="$(date +%s%3N)"
  read -r cck cok <<<"$(t3d_window "$k0" "$k1")"
  if (( cok == 0 )); then
    t3d_noise_stop
    echo "   WAL control attempt $attempt VOID: noise writer alone, no adapter, $((k1 - k0)) ms window: $cck checkpoints, $cok succeeded; the environment cannot show the property"
    T3D_SKIP="attempt $attempt: control $cck checkpoints, $cok succeeded in $((k1 - k0)) ms"
    continue
  fi
  OCENV+=(MEMOIRE_OPENCODE_READ_TIMEOUT_S=60)
  a0="$(date +%s%3N)"
  ADAPTER_SAVE="$ADAPTER"; ADAPTER="$T3D_ADAPTER"
  oc_prep "$WDB"; ADAPTER="$ADAPTER_SAVE"
  timeout 100 "${OCC[@]}" --full >/dev/null 2>"$SCRATCH/t3d.err"
  rc=$?
  a1="$(date +%s%3N)"
  t3d_noise_stop
  T3D_DONE=1; break
done
if [[ -z "$T3D_DONE" ]]; then
  echo "   SKIP T3 (d): three void attempts, the noise writer alone left the log uncheckpointable in every control window ($T3D_SKIP); the other parts of T3 decide"
  pass T3 "longest read $MAXMS ms on 5 MB + 20 MB rows; part (d) SKIPPED (control window could not checkpoint)"
else
  [[ "$rc" == "0" ]] || fail "T3: adapter under a concurrent writer exited $rc: $(tail -2 "$SCRATCH/t3d.err")"
  grep -q 'source=primary-ro' "$SCRATCH/t3d.err" || fail "T3: a database with a live -wal/-shm must be opened mode=ro (got: $(grep -o 'source=[a-z-]*' "$SCRATCH/t3d.err"))"
  read -r nck nok <<<"$(t3d_window "$a0" "$a1")"
  maxwal="$(awk '{if ($5>m) m=$5} END {print m+0}' "$WALM")"
  noise="$(sqlite3 "$WDB" "SELECT count(*) FROM noise;")"
  (( noise >= 50 )) || fail "T3: the concurrent writer was not writing ($noise rows)"
  NUMS="control window (noise only, $((k1 - k0)) ms): $cck checkpoints, $cok succeeded; adapter window ($((a1 - a0)) ms): $nck checkpoints, $nok succeeded (TRUNCATE busy=0); writer $noise rows; largest wal seen $maxwal bytes; attempt $attempt"
  echo "   WAL: $NUMS"
  (( nck >= 2 )) || fail "T3: too few checkpoints attempted during the adapter run ($NUMS)"
  (( nok >= 1 )) || fail "T3: no checkpoint could complete while the adapter ran although the control succeeded: the reader stays open ($NUMS)"
  (( maxwal < 16777216 )) || fail "T3: the write-ahead log grew to $maxwal bytes while the adapter ran ($NUMS)"
  pass T3 "longest read $MAXMS ms on 5 MB + 20 MB rows; WAL checkpointed $nok/$nck times during the run (control $cok/$cck)"
fi
fi

# =============================================================================
if run_test T4; then
echo "== T4 spill paths from rows are inputs: contained, regular, non-blocking, never an empty payload"
new_home t4; use_stub
DB="$SCRATCH/t4.db"; mkdb "$DB"
ins_session "$DB" s4 /tmp/t4proj
ins_message "$DB" m4 s4 assistant $OLD $((OLD + 5000)) $((OLD + 4000))
mkdir -p "$SCRATCH/outside" "$TOOLDIR/adir" "$SCRATCH/toolfb"
printf 'OUTSIDE-SECRET line\n' >"$SCRATCH/outside/secret.txt"
printf 'good spill é日本\nsecond line\n' >"$TOOLDIR/tool_good"
printf 'archive copy spill ç\n' >"$SCRATCH/toolfb/tool_arch"
ln -sf "$SCRATCH/outside/secret.txt" "$TOOLDIR/link_out"
ln -sf "$TOOLDIR/tool_good" "$TOOLDIR/link_in"
mkfifo "$TOOLDIR/fifo"
n=0
spillpart() { # pid path — a truncated tool part whose spill path is $2
  n=$((n + 1))
  local out="in-row tail for $1"
  local data
  data="$(jq -nc --arg o "$out" --arg p "$2" '{type: "tool", tool: "bash", callID: "c", state: {status: "completed", input: {}, output: $o, title: "t", metadata: {truncated: true, outputPath: $p}, time: {start: 1, end: 2}}}')"
  ins_part "$DB" "$1" m4 s4 $((OLD + n * 10)) $((OLD + n * 10)) "$data"
}
spillpart p_out "$SCRATCH/outside/secret.txt"
spillpart p_linkout "$TOOLDIR/link_out"
spillpart p_fifo "$TOOLDIR/fifo"
spillpart p_dir "$TOOLDIR/adir"
spillpart p_none "$TOOLDIR/tool_absent"
spillpart p_rel "tool_good"
spillpart p_trav "$TOOLDIR/../outside/secret.txt"
spillpart p_good "$TOOLDIR/tool_good"
spillpart p_linkin "$TOOLDIR/link_in"
spillpart p_arch "$TOOLDIR/tool_arch"
spillpart p_elsewhere "$SCRATCH/outside/tool_arch"
OCENV+=(MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR_FALLBACK="$SCRATCH/toolfb")
t0="$(date +%s)"
oc_prep "$DB"; timeout 60 "${OCC[@]}" --full >/dev/null 2>"$SCRATCH/t4.err"; rc=$?
[[ "$rc" == "0" ]] || fail "T4: run exited $rc (124 = a named pipe blocked it): $(tail -2 "$SCRATCH/t4.err")"
(( $(date +%s) - t0 < 30 )) || fail "T4: the run took too long (a pipe?)"
outcome() { rec_items | jq -c --arg p "opencode:part:$1:u" 'select((.source_event_id | startswith($p)) and (.source_event_id | endswith(":outcome")))' | head -1; }
rejected() { # pid reason
  local o; o="$(outcome "$1")"
  [[ -n "$o" ]] || fail "T4: $1: no outcome event"
  jq -e --arg r "$2" --arg out "in-row tail for $1" '.payload.truncated_by_harness == true and .payload.spill_rejected == $r
      and (.payload.spill_attached == null) and (.payload.spill_output == null)
      and .payload.state_out.output == $out' <<<"$o" >/dev/null \
    || fail "T4: $1 must be emitted with truncated_by_harness, spill_rejected=$2 and the row output; got $(jq -c '.payload | {truncated_by_harness, spill_rejected, spill_attached, out: .state_out.output}' <<<"$o")"
}
rejected p_out outside_dir
rejected p_linkout outside_dir
rejected p_fifo not_regular_file
rejected p_dir not_regular_file
rejected p_none missing
rejected p_rel not_absolute
rejected p_trav outside_dir
rejected p_elsewhere missing
attached() { # pid expected-bytes
  local o; o="$(outcome "$1")"
  jq -e --argjson n "$2" '.payload.spill_attached == true and .payload.spill_bytes == $n and (.payload.spill_output | length) > 0
      and (.payload.spill_rejected == null)' <<<"$o" >/dev/null \
    || fail "T4: $1 must attach the spill file ($2 bytes); got $(jq -c '.payload | {spill_attached, spill_bytes, spill_rejected}' <<<"$o")"
}
attached p_good "$(stat -c %s "$TOOLDIR/tool_good")"
attached p_linkin "$(stat -c %s "$TOOLDIR/tool_good")"
attached p_arch "$(stat -c %s "$SCRATCH/toolfb/tool_arch")"
[[ "$(outcome p_good | jq -j '.payload.spill_output')" == "$(cat "$TOOLDIR/tool_good")" ]] || fail "T4: the attached spill differs from the file"
rec_items | grep -q 'OUTSIDE-SECRET' && fail "T4: content of a file outside the tool-output directory reached an event"
pass T4 "9 refusals with reasons (pipe did not block), 3 attachments incl. archive copy"
fi

# =============================================================================
if run_test T5; then
echo "== T5 no value from a row is executed as SQL; a row whose id carries SQL is skipped and counted; the CLI runs -safe"
new_home t5; use_stub; use_shim
DB="$SCRATCH/t5.db"; mkdb "$DB"
PWN1="$SCRATCH/PWNED-session"; PWN2="$SCRATCH/PWNED-message"; PWN3="$SCRATCH/PWNED-part"
EVIL_S="evil'; SELECT writefile('$PWN1','x'); SELECT '"
EVIL_M="mev'; SELECT writefile('$PWN2','x'); --"
EVIL_P="pev'; SELECT writefile('$PWN3','x'); --"
ins_session "$DB" "$EVIL_S" /tmp/t5proj
ins_message "$DB" mx "$EVIL_S" user $OLD $OLD
ins_part "$DB" px mx "$EVIL_S" $OLD $OLD '{"type":"text","text":"in the evil session — é日本😀"}'
ins_session "$DB" good5 /tmp/t5proj $((OLD + 1000)) $((OLD + 1000))
ins_message "$DB" mgood good5 user $((OLD + 1000)) $((OLD + 1500))
ins_part "$DB" pgood mgood good5 $((OLD + 1100)) $((OLD + 1100)) '{"type":"text","text":"a good row — é日本😀"}'
ins_message "$DB" "$EVIL_M" good5 assistant $((OLD + 1200)) $((OLD + 2000)) $((OLD + 2000))
ins_part "$DB" "$EVIL_P" "$EVIL_M" good5 $((OLD + 1300)) $((OLD + 1300)) '{"type":"text","text":"in a message with an evil id"}'
ins_part "$DB" pev2 mgood good5 $((OLD + 1400)) $((OLD + 1400)) '{"type":"text","text":"a good message, a good part id"}'
EVIL_P2="pev2'; SELECT writefile('$PWN3','x'); --"
sqlite3 "$DB" "INSERT INTO part VALUES ('$(sqlq "$EVIL_P2")', 'mgood', 'good5', $((OLD + 1450)), $((OLD + 1450)), '{\"type\":\"text\",\"text\":\"evil part id in a good message\"}');"
oc "$DB" --full --summary >"$SCRATCH/t5.out" 2>"$SCRATCH/t5.err"; rc=$?
[[ "$rc" == "0" ]] || fail "T5: run exited $rc: $(tail -2 "$SCRATCH/t5.err")"
[[ ! -e "$PWN1" ]] || fail "T5: SQL carried by a session id ran (writefile created a file)"
[[ ! -e "$PWN2" ]] || fail "T5: SQL carried by a message id ran"
[[ ! -e "$PWN3" ]] || fail "T5: SQL carried by a part id ran"
grep -q 'refusing a session whose id' "$SCRATCH/t5.err" || fail "T5: the refused session is not logged"
rec_ids | grep -q "good5:facts" || fail "T5: the good session was not captured"
rec_ids | grep -q "part:pgood:u" && rec_ids | grep -q "part:pev2:u" || fail "T5: the good rows of the session must be captured"
rec_items | grep -q 'writefile' && fail "T5: a value with SQL reached an event"
grep -q 'skipping a row whose id' "$SCRATCH/t5.err" || fail "T5: the skipped rows are not logged"
grep -q 'rejected=1 rejected_rows=3' "$SCRATCH/t5.err" || fail "T5: the summary must count 1 refused session and 3 skipped rows (got: $(grep -o 'rejected=[0-9]* rejected_rows=[0-9]*' "$SCRATCH/t5.err"))"
[[ "$(cut -f4 "$SCRATCH/t5.out")" == "4" ]] || fail "T5: the summary line must carry rejected=4 (got: $(cat "$SCRATCH/t5.out"))"
[[ "$(awk 'NF > 3' "$SHIM_LOG" | grep -vc -- ' -safe ')" == "0" ]] || fail "T5: an sqlite3 call is not -safe"
pass T5 "three injected ids, no file written; 1 session refused, 3 rows skipped and counted; good data captured"
fi

# =============================================================================
if run_test T6; then
echo "== T6 exit code and summary cover every session of the run"
DB="$SCRATCH/t6.db"; mkdb "$DB"
for s in s1 s2; do
  if [[ "$s" == "s1" ]]; then t=$OLD; else t=$((OLD + 60000)); fi
  ins_session "$DB" "$s" /tmp/t6proj $t $t
  ins_message "$DB" "m$s" "$s" user $t $t
  ins_part "$DB" "p$s" "m$s" "$s" $t $t '{"type":"text","text":"x"}'
done
for failing in s1 s2; do
  new_home "t6$failing"; use_stub
  OCENV+=(STUB_FAIL_SESSION="$failing")
  oc "$DB" --full >/dev/null 2>"$SCRATCH/t6.err"; rc=$?
  [[ "$rc" == "1" ]] || fail "T6: with session $failing failing (the other clean) the exit code is $rc, want 1"
  grep -q 'failed_sessions=1' "$SCRATCH/t6.err" || fail "T6: summary must say failed_sessions=1 (got: $(grep -o 'failed_sessions=[0-9]*' "$SCRATCH/t6.err"))"
  other=s1; [[ "$failing" == "s1" ]] && other=s2
  rec_ids | grep -q "session:$other:facts" || fail "T6: the healthy session $other was not captured"
  [[ ! -s "$CURS/$failing.json" ]] || jq -e '.p.done_sig == ""' "$CURS/$failing.json" >/dev/null || fail "T6: the failed session $failing must not be marked done"
done
pass T6 "failure of the first and of the last session both give rc=1, failed_sessions=1"
fi

# =============================================================================
if run_test T7; then
echo "== T7 every key of a part survives"
new_home t7; use_stub
DB="$SCRATCH/t7.db"; mkdb "$DB"
ins_session "$DB" s7 /tmp/t7proj
ins_message "$DB" m7u s7 user $OLD $OLD
ins_message "$DB" m7a s7 assistant $((OLD + 1000)) $((OLD + 2000)) $((OLD + 2000))
ins_part "$DB" u1 m7u s7 $OLD $OLD '{"type":"text","text":"prompt é日本😀","time":{"start":11,"end":12},"extra_user":{"k":1}}'
ins_part "$DB" a1 m7a s7 $((OLD + 1100)) $((OLD + 1100)) '{"type":"text","text":"answer","time":{"start":21,"end":22},"metadata":{"openrouter":{"x":1}},"extra_assistant":[1,2]}'
ins_part "$DB" r1 m7a s7 $((OLD + 1200)) $((OLD + 1200)) '{"type":"reasoning","text":"why","time":{"start":31,"end":32},"metadata":{"openrouter":{"y":2}}}'
ins_part "$DB" t1 m7a s7 $((OLD + 1300)) $((OLD + 1300)) '{"type":"tool","tool":"bash","callID":"c7","state":{"status":"completed","input":{"a":1},"output":"o","title":"t","metadata":{"exit":0},"time":{"start":1,"end":2},"attachments":[{"n":1}],"raw":"r"},"metadata":{"openrouter":{"z":3}},"extra_tool":true}'
oc "$DB" --full >/dev/null 2>&1 || fail "T7: run failed"
ev() { rec_items | jq -c --arg p "opencode:part:$1:u" 'select((.source_event_id | startswith($p)) and (.source_event_id | endswith(":outcome") | not))' | head -1; }
jq -e '.payload.part.time == {"start":11,"end":12} and .payload.part.extra_user == {"k":1} and .payload.text == "prompt é日本😀"' <<<"$(ev u1)" >/dev/null || fail "T7: user text part lost its time or a key"
jq -e '.payload.part.time == {"start":21,"end":22} and .payload.part.metadata == {"openrouter":{"x":1}} and .payload.part.extra_assistant == [1,2]' <<<"$(ev a1)" >/dev/null || fail "T7: assistant text part lost its time/metadata/key"
jq -e '.payload.part.time == {"start":31,"end":32} and .payload.part.metadata == {"openrouter":{"y":2}}' <<<"$(ev r1)" >/dev/null || fail "T7: reasoning part lost a key"
oc_out="$(rec_items | jq -c --arg p "opencode:part:t1:u" 'select(.source_event_id | startswith($p) and endswith(":outcome"))' | head -1)"
jq -e '.payload.part.metadata == {"openrouter":{"z":3}} and .payload.part.extra_tool == true
       and .payload.state_out.metadata == {"exit":0} and .payload.state_out.attachments == [{"n":1}] and .payload.state_out.raw == "r"' <<<"$oc_out" >/dev/null \
  || fail "T7: tool outcome lost part-level metadata or a state key"
jq -e '.payload.part.metadata == {"openrouter":{"z":3}} and .payload.part.extra_tool == true' <<<"$(ev t1)" >/dev/null || fail "T7: tool call lost the part-level keys"
pass T7 "text time, part metadata, state metadata/attachments/raw and unknown keys kept"
fi

# =============================================================================
if run_test T8; then
echo "== T8 no id carries two contents"
# (a) step-finish fold: same id, same content with a cursor pass and without
new_home t8a; use_stub
DB="$SCRATCH/t8a.db"; mkdb "$DB"
ins_session "$DB" s8 /tmp/t8proj
ins_message "$DB" m8 s8 assistant $OLD $((OLD + 900000)) $((OLD + 900000))
ins_part "$DB" sf1 m8 s8 $((OLD + 100)) $((OLD + 500000)) '{"type":"step-finish","reason":"tool-calls","cost":1}'
oc "$DB" --full >/dev/null 2>&1 || fail "T8a: first run failed"
sqlite3 "$DB" "INSERT INTO part VALUES ('sf2', 'm8', 's8', $((OLD + 200)), $((OLD + 901000)), '{\"type\":\"step-finish\",\"reason\":\"stop\",\"cost\":2}'); UPDATE message SET time_updated = $((OLD + 901000)) WHERE id = 'm8';"
oc "$DB" --full >/dev/null 2>&1 || fail "T8a: second run failed"
CUR_RUN="$(rec_items)"
new_home t8a2; use_stub; rm -rf "$CURS"
oc "$DB" --full >/dev/null 2>&1 || fail "T8a: cursorless run failed"
CLEAN_RUN="$(rec_items)"
bad="$(jq -rn --slurpfile a <(printf '%s\n' "$CUR_RUN") --slurpfile b <(printf '%s\n' "$CLEAN_RUN") '
  ($b | map({(.source_event_id): .payload}) | add) as $clean
  | [$a[] | select($clean[.source_event_id] != null and .payload != $clean[.source_event_id]) | .source_event_id] | .[]')"
[[ -z "$bad" ]] || fail "T8a: the same id carries different content with and without a cursor: $bad"
jq -e -s '[.[] | select(.event_type == "opencode_message" and (.source_event_id | contains("m8")))] | map(.payload.step_finish | length) | max == 2' <<<"$CLEAN_RUN" >/dev/null \
  || fail "T8a: the fold must carry ALL step-finish parts of the message"
# (b) a text part edited after the turn was queued: one turn id, one content
new_home t8b; use_stub
DB="$SCRATCH/t8b.db"; mkdb "$DB"
ins_session "$DB" s8b /tmp/t8bproj
ins_message "$DB" m8b s8b assistant $OLD $((OLD + 2000)) $((OLD + 2000))
ins_part "$DB" tp m8b s8b $((OLD + 1000)) $((OLD + 1000)) '{"type":"text","text":"answer v1 é日本"}'
oc "$DB" --full >/dev/null 2>&1 || fail "T8b: first run failed"
sqlite3 "$DB" "UPDATE part SET time_updated = $((OLD + 900000)), data = '{\"type\":\"text\",\"text\":\"answer v2 edited 😀\"}' WHERE id = 'tp';"
oc "$DB" --full >/dev/null 2>&1 || fail "T8b: second run failed"
turns="$(rec_items | jq -c 'select(.event_type == "opencode_assistant_turn") | [.source_event_id, .text]')"
[[ "$(sort -u <<<"$turns" | wc -l)" == "1" ]] || fail "T8b: one turn id carries two contents: $turns"
rec_ids | grep -q 'part:tp:u0001767225601000$\|part:tp:u0001767226500000$' || fail "T8b: the edited part must re-emit under a new version"
[[ "$(rec_ids | grep -c 'part:tp:u')" == "2" ]] || fail "T8b: both versions of the part must exist"
# (c) permission rows are project-level: one id never carries two sessions
new_home t8c; use_stub
DB="$SCRATCH/t8c.db"; mkdb "$DB"
sqlite3 "$DB" "INSERT INTO project (id, worktree, time_created, time_updated, sandboxes) VALUES ('prj_p', '/tmp/t8cproj', 1, 1, '[]'); INSERT INTO permission VALUES ('perm1', 'prj_p', 'edit', '*', $OLD, $((OLD + 5000)));"
for s in sp1 sp2; do
  ins_session "$DB" "$s" /tmp/t8cproj $OLD $OLD prj_p
  ins_message "$DB" "m$s" "$s" user $OLD $OLD
  ins_part "$DB" "p$s" "m$s" "$s" $OLD $OLD '{"type":"text","text":"x"}'
done
oc "$DB" --full >/dev/null 2>&1 || fail "T8c: run failed"
dupe="$(rec_items | jq -r 'select(.event_type == "opencode_permission") | [.source_event_id, .session_id] | @tsv' | sort | uniq | cut -f1 | uniq -d)"
[[ -z "$dupe" ]] || fail "T8c: a permission id is emitted for several sessions: $dupe"
[[ "$(rec_items | jq -r 'select(.event_type == "opencode_permission") | .source_event_id' | sort -u | wc -l)" == "2" ]] || fail "T8c: one permission event per session expected"
# (d) context epoch: versioned by the epoch row, not by the session's activity
new_home t8d; use_stub
DB="$SCRATCH/t8d.db"; mkdb "$DB"
ins_session "$DB" s8d /tmp/t8dproj
ins_message "$DB" m8d s8d user $OLD $OLD
ins_part "$DB" p8d m8d s8d $OLD $OLD '{"type":"text","text":"x"}'
sqlite3 "$DB" "INSERT INTO session_context_epoch VALUES ('s8d', 'base', 'snap', 4);"
oc "$DB" --full >/dev/null 2>&1 || fail "T8d: first run failed"
ins_message "$DB" m8d2 s8d user $((OLD + 50000)) $((OLD + 50000))
ins_part "$DB" p8d2 m8d2 s8d $((OLD + 50000)) $((OLD + 50000)) '{"type":"text","text":"later"}'
oc "$DB" --full >/dev/null 2>&1 || fail "T8d: second run failed"
[[ "$(rec_ids | grep -c '^opencode:context_epoch:')" == "1" ]] || fail "T8d: an unchanged epoch was re-emitted under a new id: $(rec_ids | grep context_epoch | tr '\n' ' ')"
sqlite3 "$DB" "UPDATE session_context_epoch SET snapshot = 'snap2', baseline_seq = 5 WHERE session_id = 's8d';"
oc "$DB" --full >/dev/null 2>&1 || fail "T8d: third run failed"
[[ "$(rec_ids | grep -c '^opencode:context_epoch:')" == "2" ]] || fail "T8d: a changed epoch must re-emit"
pass T8 "step-finish fold, turn edit, permission, epoch: no id with two contents"
fi

# =============================================================================
if run_test T9; then
echo "== T9 nothing is created in the database directory; the fallback is taken at any read; nothing to read exits 0"
new_home t9a; use_stub
D9="$SCRATCH/t9a-dir"; mkdir -p "$D9"
mkdb "$D9/opencode.db"
sqlite3 "$D9/opencode.db" "PRAGMA journal_mode=WAL;" >/dev/null
ins_session "$D9/opencode.db" s9 /tmp/t9proj
ins_message "$D9/opencode.db" m9 s9 user $OLD $OLD
ins_part "$D9/opencode.db" p9 m9 s9 $OLD $OLD '{"type":"text","text":"x"}'
[[ ! -e "$D9/opencode.db-wal" && ! -e "$D9/opencode.db-shm" ]] || fail "T9: fixture still has -wal/-shm (the writer must be closed)"
before="$(ls -A "$D9" | sort | tr '\n' ' ')"
oc "$D9/opencode.db" --full >/dev/null 2>"$SCRATCH/t9.err"; rc=$?
after="$(ls -A "$D9" | sort | tr '\n' ' ')"
[[ "$rc" == "0" ]] || fail "T9: run exited $rc: $(tail -2 "$SCRATCH/t9.err")"
[[ "$before" == "$after" ]] || fail "T9: the adapter created files in the database directory (before: $before / after: $after)"
grep -q 'source=primary-immutable' "$SCRATCH/t9.err" || fail "T9: a database with no -wal/-shm must be opened immutable=1"
rec_ids | grep -q 'part:p9:' || fail "T9: nothing captured from the immutable open"
# the fallback triggers mid-run, at a read that is not the first probe
new_home t9c; use_stub; use_shim
D9C="$SCRATCH/t9c"; mkdir -p "$D9C"
mkdb "$D9C/primary.db"
{
  echo "BEGIN;"
  echo "INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES ('s9c', 'prj_t', 's', '/tmp/t9cproj', 't', '1', $OLD, $OLD);"
  for i in $(seq 1 120); do
    echo "INSERT INTO message VALUES ('m$i', 's9c', $((OLD + i)), $((OLD + i)), '{\"role\":\"user\",\"time\":{\"created\":$((OLD + i))}}');"
    echo "INSERT INTO part VALUES ('p$i', 'm$i', 's9c', $((OLD + i)), $((OLD + i)), '{\"type\":\"text\",\"text\":\"t$i\"}');"
  done
  echo "COMMIT;"
} | sqlite3 "$D9C/primary.db"
cp "$D9C/primary.db" "$D9C/archive.db"
FALLBACK_DB="$D9C/archive.db"
OCENV+=(SHIM_FAIL_MATCH="$D9C/primary.db" SHIM_FAIL_AFTER=6 MEMOIRE_OPENCODE_ROW_CHUNK=50)
oc "$D9C/primary.db" --full >/dev/null 2>"$SCRATCH/t9.err"; rc=$?
FALLBACK_DB="$SCRATCH/no-such-archive.db"
[[ "$rc" == "0" ]] || fail "T9: a lock at a later read must fall back, not fail the run (rc=$rc): $(tail -3 "$SCRATCH/t9.err")"
grep -q 'falling back to the archive copy' "$SCRATCH/t9.err" || fail "T9: the fallback is not logged"
grep -q 'source=fallback-archive' "$SCRATCH/t9.err" || fail "T9: the summary must say source=fallback-archive"
[[ "$(rec_ids | grep -c 'part:p')" == "120" ]] || fail "T9: the run did not complete through the fallback ($(rec_ids | grep -c 'part:p') of 120 parts)"
[[ "$(grep -c "primary.db" "$SHIM_LOG")" -ge 7 ]] || fail "T9: the failure was not injected after the first reads"
# a primary path that does not exist is an open failure: fallback, and no empty file is created
new_home t9d; use_stub
D9D="$SCRATCH/t9d"; mkdir -p "$D9D"
FALLBACK_DB="$D9C/archive.db"
oc "$D9D/absent.db" --full >/dev/null 2>"$SCRATCH/t9.err"; rc=$?
FALLBACK_DB="$SCRATCH/no-such-archive.db"
[[ "$rc" == "0" ]] || fail "T9: a missing primary must fall back (rc=$rc): $(tail -2 "$SCRATCH/t9.err")"
[[ -z "$(ls -A "$D9D")" ]] || fail "T9: the adapter created a file for a database that does not exist: $(ls -A "$D9D")"
grep -q 'source=fallback-archive' "$SCRATCH/t9.err" || fail "T9: a missing primary must be read from the fallback"
oc "$D9D/absent.db" --full >/dev/null 2>"$SCRATCH/t9.err"; rc=$?
[[ "$rc" == "0" && -z "$(ls -A "$D9D")" ]] || fail "T9: with neither database there is nothing to read: exit 0 and no file created (rc=$rc, dir: $(ls -A "$D9D"))"
grep -q 'no database to read' "$SCRATCH/t9.err" || fail "T9: nothing to read must be said in one log line: $(cat "$SCRATCH/t9.err")"
[[ "$(grep -c . "$SCRATCH/t9.err")" == "1" ]] || fail "T9: nothing to read must log exactly one line"
# a database that exists but is not one: an open failure of the primary, then of the fallback: exit 1
printf 'this is not a database\n' >"$D9D/garbage.db"
oc "$D9D/garbage.db" --full >/dev/null 2>"$SCRATCH/t9.err"; rc=$?
[[ "$rc" == "1" ]] || fail "T9: a file that is not a database with no fallback must exit 1 (rc=$rc)"
pass T9 "no file created beside the database (also for a missing one); lock at read 7 -> fallback, run completed; nothing to read exits 0"
fi

# =============================================================================
if run_test T10; then
echo "== T10 the outcome sorts after its call; session facts re-emit when the session changes"
new_home t10; use_stub
DB="$SCRATCH/t10a.db"; mkdb "$DB"
ins_session "$DB" s10a /tmp/t10proj
ins_message "$DB" m10a s10a assistant $OLD $((OLD + 2000)) $((OLD + 1900))
ins_part "$DB" prt_c13 m10a s10a $((OLD + 100)) $((OLD + 100)) '{"type":"tool","tool":"bash","callID":"c","state":{"status":"completed","input":{},"output":"o","time":{"start":1,"end":2}}}'
oc "$DB" --full >/dev/null 2>&1 || fail "T10: run failed"
sorted="$(rec_ids | grep '^opencode:part:prt_c13:' | LC_ALL=C sort)"
[[ "$(tail -1 <<<"$sorted")" == *":outcome" ]] || fail "T10: the greatest id of a tool part must be its outcome (final state), got: $(tail -1 <<<"$sorted")"
[[ "$(head -1 <<<"$sorted")" != *":outcome" ]] || fail "T10: the call must sort before the outcome"
new_home t10b; use_stub
DB="$SCRATCH/t10b.db"; mkdb "$DB"
ins_session "$DB" s10 /tmp/t10proj
ins_message "$DB" m10 s10 user $OLD $OLD
ins_part "$DB" p10 m10 s10 $OLD $OLD '{"type":"text","text":"x"}'
oc "$DB" --full >/dev/null 2>&1
[[ "$(rec_ids | grep -c 'session:s10:facts')" == "1" ]] || fail "T10: first run must emit one facts event"
oc "$DB" --full >/dev/null 2>&1
[[ "$(rec_ids | grep -c 'session:s10:facts')" == "1" ]] || fail "T10: an unchanged session must not re-emit its facts"
sqlite3 "$DB" "UPDATE session SET title = 'renamed title', time_updated = $((OLD + 777000)) WHERE id = 's10';"
oc "$DB" --full >/dev/null 2>&1
[[ "$(rec_ids | grep -c 'session:s10:facts')" == "2" ]] || fail "T10: a changed session must re-emit its facts under a new version"
rec_items | jq -e 'select(.event_type == "opencode_session_facts" and (.source_event_id | endswith("u0001767226377000"))) | .payload.session.title == "renamed title"' >/dev/null || fail "T10: the new facts do not carry the new title"
pass T10 "outcome after call; facts versioned by session.time_updated"
fi

# =============================================================================
if run_test T11; then
echo "== T11 the default source is one constant"
D11="$SCRATCH/home"
mkdir -p "$D11/.local/share/opencode" "$D11/Sync/archives/opencode-history"
for w in live arch; do
  case "$w" in live) f="$D11/.local/share/opencode/opencode.db" ;; arch) f="$D11/Sync/archives/opencode-history/opencode.db" ;; esac
  rm -f "$f"; mkdb "$f"
  ins_session "$f" "${w}_s" /tmp/t11proj
  ins_message "$f" "${w}_m" "${w}_s" user $OLD $OLD
  ins_part "$f" "${w}_p" "${w}_m" "${w}_s" $OLD $OLD '{"type":"text","text":"x"}'
done
default_run() { # adapter -> facts session id captured with NO database configured
  new_home t11d; use_stub
  env -i PATH="$PATH" HOME="$D11" MEMOIRE_HOME="$MH" TMPDIR="$SCRATCH/tmp" REC="$REC" \
    MEMOIRE_OPENCODE_DB_FALLBACK="$SCRATCH/no-such-archive.db" MEMOIRE_PROJECTS_REGISTRY="$SCRATCH/none" \
    MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR_FALLBACK="$SCRATCH/none" MEMOIRE_QUEUE_BIN="$SCRATCH/bin/stub-queue" \
    MEMOIRE_RESOLVE_PROJECT="$ROOT/scripts/memoire-resolve-project.sh" \
    "$1" --full >/dev/null 2>"$SCRATCH/t11.err"
  rec_ids | grep -o 'session:[a-z]*_s' | sort -u | tr '\n' ' '
}
got="$(default_run "$ADAPTER")"
grep -q '^OPENCODE_DEFAULT_SOURCE="live"$' "$ADAPTER" || fail "T11: the default-source constant is not at the top of the adapter"
[[ "$got" == "session:live_s " ]] || fail "T11: with the constant at live, the default read must be the live path under HOME (got '$got')"
mkdir -p "$SCRATCH/copy/scripts"
sed 's/^OPENCODE_DEFAULT_SOURCE="live"$/OPENCODE_DEFAULT_SOURCE="archive"/' "$ADAPTER" >"$SCRATCH/copy/scripts/memoire-capture-opencode.sh"
chmod +x "$SCRATCH/copy/scripts/memoire-capture-opencode.sh"
got="$(default_run "$SCRATCH/copy/scripts/memoire-capture-opencode.sh")"
[[ "$got" == "session:arch_s " ]] || fail "T11: flipping the ONE constant must flip the default source (got '$got')"
pass T11 "constant flips the default source"
fi

# =============================================================================
if run_test T12; then
echo "== T12 the cursor never moves past a failed queue write; a healed rerun queues exactly what is missing"
DB="$SCRATCH/t12.db"; mkdb "$DB"
ins_session "$DB" s12 /tmp/t12proj
for i in 1 2 3; do
  ins_message "$DB" "m$i" s12 user $((OLD + i * 1000)) $((OLD + i * 1000))
  ins_part "$DB" "p$i" "m$i" s12 $((OLD + i * 1000)) $((OLD + i * 1000)) "{\"type\":\"text\",\"text\":\"t$i\"}"
done
new_home t12full; use_stub; oc "$DB" --full >/dev/null 2>&1; T12FULL="$(rec_ids)"; T12N="$(rec_count)"
for k in 0 1 3 5 6; do
  new_home "t12k$k"; use_stub
  OCENV+=(STUB_FAIL_FROM="$k")
  oc "$DB" --full >/dev/null 2>"$SCRATCH/t12.err"; rc=$?
  [[ "$rc" == "1" ]] || fail "T12: a failed write must fail the run (k=$k, rc=$rc)"
  written="$(rec_count)"
  [[ "$written" == "$k" ]] || fail "T12: stub wrote $written items, expected $k"
  OCENV=(); use_stub
  oc "$DB" --full >/dev/null 2>"$SCRATCH/t12.err" || fail "T12: healed rerun failed (k=$k): $(tail -2 "$SCRATCH/t12.err")"
  [[ "$(rec_ids)" == "$T12FULL" ]] || fail "T12: healed rerun did not complete the id set (k=$k)"
  uniq_pairs="$(rec_items | jq -c '[.source_event_id, (.payload | tojson)]' | sort -u | wc -l)"
  (( uniq_pairs == $(rec_ids | wc -l) )) || fail "T12: an id was queued with two different contents (k=$k)"
  (( $(rec_count) <= T12N + 3 )) || fail "T12: healed rerun re-queued far more than the missing set (k=$k: $(rec_count) items for $T12N ids)"
done
# a failure at the TURN write: the turn is not remembered, the heal queues it
DB="$SCRATCH/t12t.db"; mkdb "$DB"
ins_session "$DB" s12t /tmp/t12proj
ins_message "$DB" m1 s12t assistant $OLD $((OLD + 2000)) $((OLD + 2000))
ins_part "$DB" p1 m1 s12t $((OLD + 1000)) $((OLD + 1000)) '{"type":"text","text":"the answer — é日本"}'
new_home t12t; use_stub
OCENV+=(STUB_FAIL_FROM=3)
oc "$DB" --full >/dev/null 2>&1 && fail "T12: a failure at the turn write must fail the run"
rec_ids | grep -q '^opencode:turn:' && fail "T12: the failing write was the turn"
jq -e '.turns | index("m1") | not' "$CURS/s12t.json" >/dev/null || fail "T12: the cursor remembers a turn that was never queued"
OCENV=(); use_stub; oc "$DB" --full >/dev/null 2>&1 || fail "T12: heal of the turn failed"
[[ "$(rec_ids | grep -c '^opencode:turn:m1$')" == "1" ]] || fail "T12: the healed rerun must queue the turn"
pass T12 "prefix semantics at k=0,1,3,5,6 and at the turn; heals complete the set"
fi

# =============================================================================
if run_test T13; then
echo "== T13 identity is stable on re-scan and cursor loss; an updated row re-emits under a NEW version"
new_home t13; use_stub
DB="$SCRATCH/t13.db"; mkdb "$DB"
ins_session "$DB" s13 /tmp/t13proj
ins_message "$DB" mu13 s13 user $OLD $OLD
ins_message "$DB" ma13 s13 assistant $((OLD + 1000)) $((OLD + 2000)) $((OLD + 1500))
ins_part "$DB" pu13 mu13 s13 $OLD $OLD '{"type":"text","text":"prompt one é"}'
ins_part "$DB" pa13 ma13 s13 $((OLD + 1100)) $((OLD + 1500)) '{"type":"text","text":"draft text é日本"}'
snap() { rec_items | jq -c '[.source_event_id, (.payload | tojson)]' | LC_ALL=C sort -u; }
oc "$DB" --full >/dev/null 2>&1 || fail "T13: first run failed"
A="$(snap)"
grep -q 'opencode:part:pa13:u0001767225601500' <<<"$A" || fail "T13: the FIRST emission must already carry a u-version"
n_before="$(rec_count)"
oc "$DB" --full >/dev/null 2>&1
[[ "$(rec_count)" == "$n_before" ]] || fail "T13: a cursor pass re-queued events ($n_before -> $(rec_count))"
rm -rf "$CURS"; new_home t13b; use_stub
oc "$DB" --full >/dev/null 2>&1
[[ "$(snap)" == "$A" ]] || fail "T13: a cursorless re-scan does not reproduce the same ids and payloads"
# real queue: update the part and the message, the old version stays in the queue
new_home t13r
oc "$DB" --full >/dev/null 2>&1 || fail "T13: real-queue run failed"
before="$(qids)"
sqlite3 "$DB" "UPDATE part SET time_updated = $((OLD + 9000)), data = '{\"type\":\"text\",\"text\":\"final text after edit 😀\"}' WHERE id = 'pa13'; UPDATE message SET time_updated = $((OLD + 10000)) WHERE id = 'ma13';"
oc "$DB" --full >/dev/null 2>&1 || fail "T13: second real-queue run failed"
new="$(comm -13 <(printf '%s\n' "$before") <(qids))"
[[ "$(grep -c . <<<"$new")" == "2" ]] || fail "T13: expected exactly 2 new ids (part + message), got: $new"
grep -q "^opencode:part:pa13:u$(printf '%016d' $((OLD + 9000)))$" <<<"$new" || fail "T13: the updated part must re-emit under its new version"
grep -q "^opencode:message:ma13:u$(printf '%016d' $((OLD + 10000)))$" <<<"$new" || fail "T13: the updated message must re-emit under its new version"
[[ "$(qevent "opencode:part:pa13:u$(printf '%016d' $((OLD + 1500)))" | jq -r '.payload.text')" == "draft text é日本" ]] || fail "T13: the previous version is gone or was rewritten"
[[ "$(qevent "opencode:part:pa13:u$(printf '%016d' $((OLD + 9000)))" | jq -r '.payload.text')" == "final text after edit 😀" ]] || fail "T13: the new version lost the edit"
pass T13
fi

# =============================================================================
if run_test T14; then
echo "== T14 nothing is truncated: 200 kB prompt and answer, 50 kB tool output, 300 kB reasoning"
new_home t14
DB="$SCRATCH/t14.db"; mkdb "$DB"
ins_session "$DB" s14 /tmp/t14proj
ins_message "$DB" mu14 s14 user $OLD $OLD
ins_message "$DB" ma14 s14 assistant $((OLD + 1000)) $((OLD + 2000)) $((OLD + 1900))
bigpart() { # id message type-json-key length role-filename
  jq -nc --arg u "${5}é日😀" --argjson n "$(( $4 / 4 ))" --arg ty "$3" '{type: $ty, text: ($u * $n)}' >"$SCRATCH/bp.json"
  sqlite3 "$DB" "INSERT INTO part VALUES ('$1', '$2', 's14', $((OLD + RANDOM % 900 + 10)), $((OLD + 950)), CAST(readfile('$SCRATCH/bp.json') AS TEXT));"
}
bigpart pu14 mu14 text 200000 U
bigpart pa14 ma14 text 200000 A
bigpart pr14 ma14 reasoning 300000 R
jq -nc --arg u "Té日😀" '{type: "tool", tool: "bash", callID: "c14", state: {status: "completed", input: {}, title: "t", output: ($u * 12500), time: {start: 1, end: 2}}}' >"$SCRATCH/bp.json"
sqlite3 "$DB" "INSERT INTO part VALUES ('px14', 'ma14', 's14', $((OLD + 1100)), $((OLD + 1100)), CAST(readfile('$SCRATCH/bp.json') AS TEXT));"
ins_part "$DB" pc14 ma14 s14 $((OLD + 1200)) $((OLD + 1200)) '{"type":"compaction","auto":true}'
ins_part "$DB" pp14 ma14 s14 $((OLD + 1300)) $((OLD + 1300)) '{"type":"patch","hash":"h","files":["a"]}'
ins_part "$DB" pf14 ma14 s14 $((OLD + 1400)) $((OLD + 1400)) '{"type":"file","mime":"image/png","filename":"f.png","url":"u"}'
ins_part "$DB" ps14 mu14 s14 $((OLD + 1500)) $((OLD + 1500)) '{"type":"text","text":"synthetic tail","synthetic":true}'
oc "$DB" --full >/dev/null 2>"$SCRATCH/t14.err" || fail "T14: run failed: $(tail -2 "$SCRATCH/t14.err")"
plen() { qfiles | xargs -r jq -r --arg p "$1" 'select(.source_event_id | startswith($p)) | '"$2" | head -1; }
[[ "$(plen opencode:part:pu14: '.payload.text | length')" == "200000" ]] || fail "T14: the 200000-char user prompt was cut"
[[ "$(plen opencode:part:pa14: '.payload.text | length')" == "200000" ]] || fail "T14: the 200000-char assistant text was cut"
[[ "$(plen opencode:part:pr14: '.payload.text | length')" == "300000" ]] || fail "T14: the 300000-char reasoning was cut"
[[ "$(qfiles | xargs -r jq -r 'select(.source_event_id | startswith("opencode:part:px14:") and endswith(":outcome")) | .payload.state_out.output | length')" == "50000" ]] || fail "T14: the 50000-char tool output was cut"
[[ "$(qfiles | xargs -r jq -r 'select(.event_type == "opencode_assistant_turn") | .text | length')" == "200000" ]] || fail "T14: the turn text must be the full assistant text"
qfiles | xargs -r grep -l 'text_truncated' >/dev/null 2>&1 && fail "T14: a text_truncated marker is present"
declare -A WANT=( [opencode_session_facts]=1 [opencode_message]=2 [opencode_assistant_turn]=1 [opencode_user_prompt]=1
  [opencode_assistant_message]=1 [opencode_reasoning]=1 [opencode_tool_call]=1 [opencode_tool_result]=1
  [opencode_compaction]=1 [opencode_patch]=1 [opencode_file]=1 [opencode_item]=1 )
declare -A GOT=()
while read -r t; do GOT[$t]=$(( ${GOT[$t]:-0} + 1 )); done < <(qfiles | xargs -r jq -r '.event_type')
for t in "${!WANT[@]}"; do [[ "${GOT[$t]:-0}" == "${WANT[$t]}" ]] || fail "T14: $t x${GOT[$t]:-0}, want ${WANT[$t]}"; done
for t in "${!GOT[@]}"; do [[ -n "${WANT[$t]:-}" ]] || fail "T14: unexpected type $t"; done
pass T14
fi

# =============================================================================
if run_test T15; then
echo "== T15 occurred_at is the record time; scratch attribution through the registry"
new_home t15
DB="$SCRATCH/t15.db"; mkdb "$DB"
T15DIR="/tmp/oc-t15-scratch-$$"
ins_session "$DB" s15 "$T15DIR"
ins_message "$DB" m15 s15 user $((OLD + 123)) $((OLD + 123))
ins_part "$DB" p15 m15 s15 $((OLD + 123)) $((OLD + 5000)) '{"type":"text","text":"scratchy é日本"}'
REG="$SCRATCH/registry.json"
cat >"$REG" <<'JSON'
{ "projects": { "scratch-tmp": { "display": "scratch-tmp", "scratch": true, "paths": ["/tmp"] } } }
JSON
OCENV+=(MEMOIRE_PROJECTS_REGISTRY="$REG")
oc "$DB" --full >/dev/null 2>&1 || fail "T15: run failed"
e="$(qevent "opencode:part:p15:u$(printf '%016d' $((OLD + 5000)))")"
[[ "$(jq -r '.occurred_at' <<<"$e")" == "2026-01-01T00:00:00.123Z" ]] || fail "T15: occurred_at must be the part's time_created (got $(jq -r '.occurred_at' <<<"$e"))"
[[ "$(jq -r '.project' <<<"$e")" == "scratch-tmp" ]] || fail "T15: project must resolve to scratch-tmp (got $(jq -r '.project' <<<"$e"))"
[[ "$(jq -r '.payload.memoire_project_source' <<<"$e")" == "registry" ]] || fail "T15: project source must be registry"
[[ "$(jq -r '.payload.memoire_project_root' <<<"$e")" == "/tmp" ]] || fail "T15: project root must be the registry prefix"
[[ "$(jq -r '.cwd' <<<"$e")" == "$T15DIR" ]] || fail "T15: the event must carry the session directory as cwd"
[[ "$(jq -r '.session_id' <<<"$e")" == "s15" ]] || fail "T15: session id lost"
[[ "$(qfiles | xargs -r jq -r '.occurred_at' | grep -vc '^2026-01-01T')" == "0" ]] || fail "T15: an event carries the run time as occurred_at"
pass T15
fi

# =============================================================================
if run_test T16; then
echo "== T16 turn rules and text; only the turn is derivation-admitted; the never-read tables stay unread"
new_home t16; use_shim
SHIM_SQL_LOG="$SCRATCH/t16-sql.log"; : >"$SHIM_SQL_LOG"; OCENV+=(SHIM_SQL_LOG="$SHIM_SQL_LOG")
printf 'spill line 1 é日本\nspill line 2\n' >"$TOOLDIR/tool_ok"
oc "$CENSUS_DB" --full >"$SCRATCH/t16.out" 2>"$SCRATCH/t16.err" || fail "T16: census run failed: $(tail -2 "$SCRATCH/t16.err")"
turns="$(qfiles | xargs -r jq -r 'select(.event_type == "opencode_assistant_turn") | .source_event_id' | sort | tr '\n' ' ')"
[[ "$turns" == "opencode:turn:msg_c02 opencode:turn:msg_c11 opencode:turn:msg_c12 " ]] || fail "T16: turns must exist for msg_c02, msg_c11 and msg_c12 only (not for the uncompleted, running, empty or user messages); got: $turns"
[[ "$(qevent opencode:turn:msg_c12 | jq -r '.text')" == "$(printf 'Alpha comes first by time.\n\nBeta comes second by time.')" ]] || fail "T16: turn text must be joined in (time_created, id) order, not id or insertion order"
[[ "$(qevent opencode:turn:msg_c02 | jq -r '.text')" == "$(printf 'I will run ls now — voilà 日本.\n\nDone: two files — é😀.')" ]] || fail "T16: turn text is not the joined text parts"
[[ "$(qevent opencode:turn:msg_c11 | jq -r '.text')" == "$(printf 'First invented paragraph — é日本.\n\nSecond invented paragraph 😀.')" ]] || fail "T16: turn text must skip the empty part and join with a blank line"
admitted='claude_UserPromptSubmit claude_PostToolUse claude_assistant_turn codex_assistant_turn qwen_assistant_turn opencode_assistant_turn pi_assistant_turn'
seen_admitted="$(qfiles | xargs -r jq -r '.event_type' | sort -u | grep -Fx -f <(tr ' ' '\n' <<<"$admitted") | tr '\n' ' ')"
[[ "$seen_admitted" == "opencode_assistant_turn " ]] || fail "T16: the only derivation-admitted type emitted must be opencode_assistant_turn (got: $seen_admitted)"
qfiles | xargs -r grep -l 'CANARY' >/dev/null 2>&1 && fail "T16: a value from a never-read table surfaced in an event"
grep -qiE 'credential|account|control_account' "$SHIM_SQL_LOG" && fail "T16: a query names a table that must never be read"
grep -q 'session_input holds rows' "$SCRATCH/t16.err" || fail "T16: session_input rows must raise a warning"
grep -q 'session_message holds unmapped' "$SCRATCH/t16.err" || fail "T16: session_message rows must raise a warning"
pass T16
fi

# =============================================================================
if run_test T17; then
echo "== T17 quiet gate over children, stuck-running gate, window and explicit ids"
new_home t17; use_stub
NOW_MS="$(date +%s%3N)"   # the gates compare with the clock: take it now, not at the start of the suite
DB="$SCRATCH/t17.db"; mkdb "$DB"
FRESH=$(( NOW_MS - 10000 ))
ins_session "$DB" quiet /tmp/t17a $OLD $OLD
ins_message "$DB" mq quiet user $OLD $OLD
ins_part "$DB" pq mq quiet $OLD $OLD '{"type":"text","text":"q"}'
ins_session "$DB" busy /tmp/t17b $OLD $OLD
ins_message "$DB" mb busy user $OLD $OLD
ins_part "$DB" pb mb busy $OLD $FRESH '{"type":"text","text":"changed 10 s ago; the session row is old"}'
RECENT=$(( NOW_MS - 3 * 3600 * 1000 ))
ins_session "$DB" parent /tmp/t17c $OLD $OLD
ins_session "$DB" child /tmp/t17c $OLD $OLD
ins_message "$DB" mpar parent user $RECENT $RECENT
ins_part "$DB" ppar mpar parent $RECENT $RECENT '{"type":"text","text":"parent quiet"}'
ins_message "$DB" mchi child user $OLD $FRESH
ins_part "$DB" pchi mchi child $OLD $FRESH '{"type":"text","text":"child busy"}'
RUN10=$(( NOW_MS - 600000 ))   # a tool running for 10 minutes
ins_session "$DB" stuckyoung /tmp/t17d $OLD $OLD
ins_message "$DB" msy stuckyoung assistant $OLD $RUN10
ins_part "$DB" psy msy stuckyoung $OLD $RUN10 "{\"type\":\"tool\",\"tool\":\"bash\",\"callID\":\"c\",\"state\":{\"status\":\"running\",\"input\":{},\"time\":{\"start\":$RUN10}}}"
RUN2H=$(( NOW_MS - 2 * 3600 * 1000 ))
ins_session "$DB" stuckold /tmp/t17e $OLD $OLD
ins_message "$DB" mso stuckold assistant $OLD $RUN2H
ins_part "$DB" pso mso stuckold $OLD $RUN2H "{\"type\":\"tool\",\"tool\":\"bash\",\"callID\":\"c\",\"state\":{\"status\":\"running\",\"input\":{},\"time\":{\"start\":$RUN2H}}}"
oc "$DB" --scan >/dev/null 2>"$SCRATCH/t17.err" || fail "T17: run failed"
has() { rec_ids | grep -q "session:$1:facts"; }
has busy && fail "T17: a session with a child changed 10 s ago must be gated"
has stuckyoung && fail "T17: a session with a tool running for 10 min must stay gated"
has parent && has stuckold || fail "T17: quiet sessions must be captured (parent/stuckold)"
has child && fail "T17: a child session that changed 10 s ago must be gated"
has quiet && fail "T17: a session older than the window must not be captured in scan mode"
rec_items | jq -e 'select(.source_event_id | startswith("opencode:part:pso:") and endswith(":outcome")) | .payload.status == "running"' >/dev/null || fail "T17: a stuck-late import must keep state.status verbatim"
# explicit ids bypass the window, not the gates
new_home t17e; use_stub
oc "$DB" --full quiet busy >/dev/null 2>&1 || fail "T17: explicit run failed"
has quiet || fail "T17: an explicit id must bypass the window"
has busy && fail "T17: an explicit id must NOT bypass the quiet gate"
OCENV+=(MEMOIRE_OPENCODE_QUIET_MS=0)
oc "$DB" --full busy >/dev/null 2>&1
has busy || fail "T17: MEMOIRE_OPENCODE_QUIET_MS=0 must drop the gate"
pass T17
fi

# =============================================================================
if run_test T18; then
echo "== T18 census on a synthetic store: events per type == the numbers in the fixture, key by key"
new_home t18
printf 'spill line 1 é日本\nspill line 2\n' >"$TOOLDIR/tool_ok"
oc "$CENSUS_DB" --full >/dev/null 2>"$SCRATCH/t18.err" || fail "T18: census run failed: $(tail -2 "$SCRATCH/t18.err")"
declare -A CEXP=( [opencode_session_facts]=4 [opencode_todo]=1 [opencode_permission]=4 [opencode_session_input]=1
  [opencode_context_epoch]=1 [opencode_message]=13 [opencode_user_prompt]=3 [opencode_assistant_message]=9
  [opencode_reasoning]=1 [opencode_item]=3 [opencode_file]=1 [opencode_tool_call]=6 [opencode_tool_result]=5
  [opencode_tool_failure]=1 [opencode_patch]=1 [opencode_compaction]=1 [opencode_assistant_turn]=3 )
declare -A CGOT=()
while read -r t; do CGOT[$t]=$(( ${CGOT[$t]:-0} + 1 )); done < <(qfiles | xargs -r jq -r '.event_type')
for t in "${!CEXP[@]}"; do [[ "${CGOT[$t]:-0}" == "${CEXP[$t]}" ]] || fail "T18: $t events=${CGOT[$t]:-0}, fixture says ${CEXP[$t]}"; done
for t in "${!CGOT[@]}"; do [[ -n "${CEXP[$t]:-}" ]] || fail "T18: unexpected event type $t x${CGOT[$t]}"; done
[[ "$(qcount)" == "58" ]] || fail "T18: $(qcount) events, fixture says 58"
census_compare "$CENSUS_DB" "$QDIR" || fail "T18: key-level comparison against the source rows failed"
bytes_compare "$CENSUS_DB" "$QDIR" || fail "T18: the counts and bytes of the strings differ between the store and the events"
# spill of the fixture: the attached file, whole
qfiles | xargs -r jq -e 'select(.source_event_id | startswith("opencode:part:prt_c50:") and endswith(":outcome")) | .payload.spill_attached == true and .payload.spill_output == "spill line 1 é日本\nspill line 2\n"' >/dev/null || fail "T18: the fixture spill file is not attached whole"
# the project_directory hints ride the session facts
qfiles | xargs -r jq -e 'select(.source_event_id | startswith("opencode:session:ses_c01:facts:")) | (.payload.project_directories | length) == 1 and .payload.project_directories[0].directory == "/tmp/oc-census/alpha-extra"' >/dev/null || fail "T18: the facts must carry the project_directory hints"
# harness truncation: flagged with or without a spill, never silently
outc() { qfiles | xargs -r jq -c --arg p "opencode:part:$1:u" 'select((.source_event_id | startswith($p)) and (.source_event_id | endswith(":outcome"))) | .payload' | head -1; }
jq -e '.truncated_by_harness == true and .spill_rejected == null and .spill_attached == null' <<<"$(outc prt_c70)" >/dev/null || fail "T18: a truncated tool output with no spill path must carry truncated_by_harness: true"
jq -e '.truncated_by_harness == true and .spill_rejected == "missing"' <<<"$(outc prt_c60)" >/dev/null || fail "T18: a truncated tool output whose spill is gone must carry truncated_by_harness and spill_rejected=missing"
jq -e '.truncated_by_harness == null and .spill_attached == true' <<<"$(outc prt_c50)" >/dev/null || fail "T18: an attached spill is not a truncation"
jq -e '.truncated_by_harness == null and .spill_rejected == null' <<<"$(outc prt_c13)" >/dev/null || fail "T18: an untruncated output must carry no truncation flag"
pass T18 "58 events, every source key and every turn text present"
fi


# =============================================================================
if run_test T19; then
echo "== T19 an unchanged session is skipped: done_sig is read back, a second run issues no chunk and no session-level statement, a changed session is read alone"
new_home t19; use_stub; use_shim
SHIM_SQL_LOG="$REC/sql.log"; : >"$SHIM_SQL_LOG"; OCENV+=(SHIM_SQL_LOG="$SHIM_SQL_LOG")
DB="$SCRATCH/t19.db"; mkdb "$DB"
T0=$(( NOW_MS - 3 * 3600 * 1000 ))
for s in a b c; do
  ins_session "$DB" "s19$s" /tmp/t19proj $T0 $T0
  ins_message "$DB" "m19$s" "s19$s" assistant $T0 $((T0 + 2000)) $((T0 + 1900))
  ins_part "$DB" "p19$s" "m19$s" "s19$s" $((T0 + 100)) $((T0 + 100)) '{"type":"text","text":"héllo 日本語 😀 session '"$s"'"}'
done
oc "$DB" --scan --summary >"$SCRATCH/t19.out1" 2>"$SCRATCH/t19.err1"; rc=$?
[[ "$rc" == "0" ]] || fail "T19: first run exited $rc: $(tail -2 "$SCRATCH/t19.err1")"
grep -q 'sessions=3 ' "$SCRATCH/t19.err1" || fail "T19: the first run must process the 3 sessions: $(tail -1 "$SCRATCH/t19.err1")"
for s in a b c; do
  jq -e '.p.done_sig | type == "string" and length > 0' "$CURS/s19$s.json" >/dev/null || fail "T19: session $s has no done_sig in its cursor after a complete read"
done
N1="$(rec_count)"
: >"$SHIM_SQL_LOG"
oc "$DB" --scan --summary >"$SCRATCH/t19.out2" 2>"$SCRATCH/t19.err2"; rc=$?
[[ "$rc" == "0" ]] || fail "T19: second run exited $rc"
grep -q 'skipped_unchanged=3 ' "$SCRATCH/t19.err2" || fail "T19: the second run over an unchanged database must skip the 3 sessions (got: $(tail -1 "$SCRATCH/t19.err2"))"
grep -q ' sessions=0 ' "$SCRATCH/t19.err2" || fail "T19: no session may be processed on the second run"
[[ "$(rec_count)" == "$N1" ]] || fail "T19: the second run queued events ($N1 -> $(rec_count))"
NSTMT="$(grep -c '@@STMT@@' "$SHIM_SQL_LOG")"
NCHUNK="$(grep -c "json_object('n'" "$SHIM_SQL_LOG")"
NAUX="$(grep -c "json_object('a'" "$SHIM_SQL_LOG")"
[[ "$NCHUNK" == "0" ]] || fail "T19: the second run issued $NCHUNK chunk read(s) (a session must be skipped without reading a row)"
[[ "$NAUX" == "0" ]] || fail "T19: the second run issued $NAUX session-level read(s)"
echo "   second run: $NSTMT statements (selection pages and per-session probes), 0 chunk reads, 0 session reads"
# a change in one session: that session alone is read
ins_part "$DB" p19b2 m19b s19b $((T0 + 5000)) $((T0 + 5000)) '{"type":"text","text":"a later part — é日本"}'
: >"$SHIM_SQL_LOG"
oc "$DB" --scan --summary >"$SCRATCH/t19.out3" 2>"$SCRATCH/t19.err3" || fail "T19: third run failed"
grep -q ' sessions=1 ' "$SCRATCH/t19.err3" && grep -q 'skipped_unchanged=2 ' "$SCRATCH/t19.err3" || fail "T19: a change in one session must process that session and skip the other two (got: $(tail -1 "$SCRATCH/t19.err3"))"
rec_ids | grep -q '^opencode:part:p19b2:u' || fail "T19: the new part was not captured"
[[ "$(grep -c "json_object('a'" "$SHIM_SQL_LOG")" == "1" ]] || fail "T19: exactly one session-level read expected on the third run"
# a verify run re-reads every session from its cursor, and queues nothing new
oc "$DB" --verify --summary >/dev/null 2>"$SCRATCH/t19.err4" || fail "T19: verify run failed"
grep -q 'skipped_unchanged=0 ' "$SCRATCH/t19.err4" && grep -q ' sessions=3 ' "$SCRATCH/t19.err4" || fail "T19: --verify must not skip any session (got: $(tail -1 "$SCRATCH/t19.err4"))"
pass T19 "3 sessions skipped by signature ($NSTMT statements, 0 chunk reads); one change read alone"
fi

# =============================================================================
if run_test T20; then
echo "== T20 a fallback in the middle of a run never advances the primary slot: the next run on the primary reads what the archive lacked"
new_home t20; use_stub; use_shim
D20="$SCRATCH/t20"; mkdir -p "$D20"
T0=$(( NOW_MS - 3 * 3600 * 1000 ))
mkdb "$D20/primary.db"
ins_session "$D20/primary.db" s20 /tmp/t20proj $T0 $T0
ins_message "$D20/primary.db" m20a s20 user $T0 $((T0 + 1000))
for i in 1 2 3; do
  ins_part "$D20/primary.db" "p20-$i" m20a s20 $((T0 + i * 100)) $((T0 + i * 100)) '{"type":"text","text":"in both copies '"$i"' é日本"}'
done
cp "$D20/primary.db" "$D20/archive.db"
# the newest row exists ONLY in the primary
ins_part "$D20/primary.db" p20-4 m20a s20 $((T0 + 5000)) $((T0 + 5000)) '{"type":"text","text":"only in the primary é日本😀"}'
FALLBACK_DB="$D20/archive.db"
OCENV+=(SHIM_FAIL_MATCH="$D20/primary.db" SHIM_FAIL_AFTER=6)
oc "$D20/primary.db" --scan --summary >"$SCRATCH/t20.out1" 2>"$SCRATCH/t20.err1"; rc=$?
[[ "$rc" == "0" ]] || fail "T20: run 1 (lock at the first chunk read) exited $rc: $(tail -2 "$SCRATCH/t20.err1")"
grep -q 'source=fallback-archive' "$SCRATCH/t20.err1" || fail "T20: run 1 must have read the archive copy"
rec_ids | grep -q '^opencode:part:p20-3:u' || fail "T20: run 1 must capture the rows the archive holds"
rec_ids | grep -q '^opencode:part:p20-4:u' && fail "T20: the row that only the primary holds cannot have been captured by run 1"
jq -e '(.p.mu == 0) and (.p.done_sig == "") and (.f.mu > 0)' "$CURS/s20.json" >/dev/null \
  || fail "T20: rows read from the fallback must advance the fallback slot only (cursor: $(jq -c '{p: .p.mu, pdone: .p.done_sig, f: .f.mu}' "$CURS/s20.json"))"
# run 2: the primary is healthy again
OCENV=(); use_stub; use_shim
FALLBACK_DB="$D20/archive.db"
oc "$D20/primary.db" --scan --summary >"$SCRATCH/t20.out2" 2>"$SCRATCH/t20.err2" || fail "T20: run 2 failed: $(tail -2 "$SCRATCH/t20.err2")"
grep -q 'source=primary' "$SCRATCH/t20.err2" || fail "T20: run 2 must read the primary"
grep -q 'skipped_unchanged=1' "$SCRATCH/t20.err2" && fail "T20: run 2 skipped the session although the primary was never read to its end"
rec_ids | grep -q '^opencode:part:p20-4:u' || fail "T20: the row that only the primary holds was never captured (skipped after the fallback)"
jq -e '(.p.mu > 0) and (.p.done_sig != "")' "$CURS/s20.json" >/dev/null || fail "T20: run 2 must advance the primary slot and mark it done"
bad="$(rec_items | jq -c '[.source_event_id, (.payload | tojson)]' | sort -u | jq -r '.[0]' | sort | uniq -d)"
[[ -z "$bad" ]] || fail "T20: an id carries two contents: $bad"
# run 3: the primary was read to its end, an unchanged session is now skipped
oc "$D20/primary.db" --scan --summary >"$SCRATCH/t20.out3" 2>"$SCRATCH/t20.err3"
grep -q 'skipped_unchanged=1' "$SCRATCH/t20.err3" || fail "T20: an unchanged primary must be skipped once it was read to its end (got: $(tail -1 "$SCRATCH/t20.err3"))"
GOT="$(rec_ids)"
# the two runs together hold exactly what one run on the primary alone holds
new_home t20ref; use_stub
FALLBACK_DB="$SCRATCH/no-such-archive.db"
oc "$D20/primary.db" --scan >/dev/null 2>&1 || fail "T20: reference run failed"
[[ -n "$(rec_ids)" && "$GOT" == "$(rec_ids)" ]] || fail "T20: the fallback run plus the primary run do not equal a straight run on the primary"
pass T20 "fallback slot advanced alone; primary re-read from its own position; p20-4 captured; no id with two contents"
fi

# =============================================================================
if run_test T21; then
echo "== T21 a SIGTERM to the whole process group is a clean stop: exit 0, one log line, cursor at the last durable batch, no temporary file in the queue directory"
DB="$SCRATCH/t21.db"; mkdb "$DB"
T0=$(( NOW_MS - 3 * 3600 * 1000 ))
ins_session "$DB" s21 /tmp/t21proj $T0 $T0
{
  echo "BEGIN;"
  for i in $(seq 1 3500); do
    t=$(( T0 + i * 100 ))
    echo "INSERT INTO message VALUES ('m21-$i', 's21', $t, $((t + 50)), '{\"role\":\"assistant\",\"time\":{\"created\":$t,\"completed\":$((t + 40))}}');"
    echo "INSERT INTO part VALUES ('p21-$i', 'm21-$i', 's21', $((t + 10)), $((t + 10)), '{\"type\":\"text\",\"text\":\"réponse $i 日本語 😀\"}');"
    echo "INSERT INTO part VALUES ('q21-$i', 'm21-$i', 's21', $((t + 20)), $((t + 30)), '{\"type\":\"tool\",\"tool\":\"bash\",\"callID\":\"c$i\",\"state\":{\"status\":\"completed\",\"input\":{},\"output\":\"sortie é $i\",\"time\":{\"start\":1,\"end\":2}}}');"
  done
  echo "COMMIT;"
} | sqlite3 "$DB"
new_home t21ref; use_stub
oc "$DB" --scan >/dev/null 2>&1 || fail "T21: reference run failed"
T21IDS="$(rec_ids)"
NREF="$(grep -c . <<<"$T21IDS")"
new_home t21
nvoid=0; ntrial=0; nwithcursor=0
for delay in 0.4 0.7 1.0 1.3 2.2 3.1; do
  oc_prep "$DB"
  ( exec setsid "${OCC[@]}" --scan --summary >"$SCRATCH/t21.out" 2>"$SCRATCH/t21.err" ) &
  gpid=$!
  sleep "$delay"
  ntrial=$((ntrial + 1))
  if ! kill -TERM -- "-$gpid" 2>/dev/null; then wait "$gpid"; nvoid=$((nvoid + 1)); continue; fi   # it ended before the signal
  wait "$gpid"; rc=$?
  [[ "$rc" == "0" ]] || fail "T21: a group SIGTERM at $delay s must end with exit 0 (rc=$rc): $(tail -2 "$SCRATCH/t21.err")"
  [[ "$(grep -c . "$SCRATCH/t21.err")" == "1" ]] || fail "T21: a group SIGTERM must leave exactly one log line, got: $(cat "$SCRATCH/t21.err")"
  grep -q 'stopped cleanly on signal' "$SCRATCH/t21.err" || fail "T21: the log line must say it stopped cleanly on the signal: $(cat "$SCRATCH/t21.err")"
  [[ "$(find "$QDIR" -maxdepth 1 -type f ! -name '*.json' | wc -l)" == "0" ]] || fail "T21: temporary file(s) left in the queue directory after a group SIGTERM: $(find "$QDIR" -maxdepth 1 -type f ! -name '*.json' | head -3 | tr '\n' ' ')"
  # (an early stop may come before the first cursor exists: nothing to check then)
  if [[ -s "$CURS/s21.json" ]]; then
    # the cursor is never ahead of what is durable: every part before it is queued
    cmu="$(jq -r '.p.mu' "$CURS/s21.json")"
    missing="$(comm -13 <(qids) <(sqlite3 "$DB" "SELECT 'opencode:part:' || id || ':u' || substr('0000000000000000' || time_updated, -16, 16) FROM part WHERE time_updated < $cmu AND id LIKE 'p21-%';" | LC_ALL=C sort -u) | wc -l)"
    [[ "$missing" == "0" ]] || fail "T21: the cursor is ahead of the durable events ($missing part(s) before it are not queued) after a stop at $delay s"
    nwithcursor=$((nwithcursor + 1))
  fi
done
(( nwithcursor >= 1 )) || fail "T21: no trial left a cursor to check"
(( ntrial - nvoid >= 1 )) || fail "T21: every trial finished before the signal (increase the session)"
# the heal: one more run completes the set, no id twice with two contents
oc "$DB" --scan >/dev/null 2>"$SCRATCH/t21.err" || fail "T21: the rerun after the stops failed: $(tail -2 "$SCRATCH/t21.err")"
[[ "$(qids)" == "$T21IDS" ]] || fail "T21: the rerun did not complete the id set ($(qids | wc -l) of $NREF)"
dupe="$(qfiles | xargs -r jq -c '[.source_event_id, (.payload | tojson), .text]' | sort -u | jq -r '.[0]' | sort | uniq -d | head -3)"
[[ -z "$dupe" ]] || fail "T21: an id carries two contents after group stops: $dupe"
[[ "$(find "$QDIR" -maxdepth 1 -type f ! -name '*.json' | wc -l)" == "0" ]] || fail "T21: temporary file(s) left after the rerun"
pass T21 "$((ntrial - nvoid)) group stop(s): exit 0, one log line, no temp file, cursor not ahead; rerun complete ($NREF ids)"
fi

# =============================================================================
if run_test T22; then
echo "== T22 the selection reads no table whole: no full scan of part or message in any statement; the walk is in pages of sessions"
new_home t22; use_stub; use_shim
SHIM_SQL_LOG="$REC/sql.log"; : >"$SHIM_SQL_LOG"; OCENV+=(SHIM_SQL_LOG="$SHIM_SQL_LOG")
DB="$SCRATCH/t22.db"; mkdb "$DB"
T0=$(( NOW_MS - 3 * 3600 * 1000 ))
{
  echo "BEGIN;"
  for i in $(seq 0 129); do
    id="$(printf 's%03d' "$i")"
    if (( i % 5 == 0 )); then t=$T0; else t=$OLD; fi
    echo "INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES ('$id', 'prj_t', 's', '/tmp/t22proj', 't', '1', $t, $t);"
    echo "INSERT INTO message VALUES ('m$id', '$id', $t, $t, '{\"role\":\"user\",\"time\":{\"created\":$t}}');"
    echo "INSERT INTO part VALUES ('p$id', 'm$id', '$id', $t, $t, '{\"type\":\"text\",\"text\":\"hello é $id\"}');"
  done
  echo "COMMIT;"
} | sqlite3 "$DB"
oc "$DB" --scan --summary >/dev/null 2>"$SCRATCH/t22.err" || fail "T22: run failed: $(tail -2 "$SCRATCH/t22.err")"
grep -q 'candidates=26 ' "$SCRATCH/t22.err" || fail "T22: 26 of the 130 sessions are inside the window (got: $(grep -o 'candidates=[0-9]*' "$SCRATCH/t22.err"))"
mkdir -p "$SCRATCH/t22stmts"; rm -f "$SCRATCH/t22stmts"/*
awk -v d="$SCRATCH/t22stmts" 'BEGIN {n = 0; f = d "/s0"} /^@@STMT@@$/ {close(f); n++; f = d "/s" n; next} {print >> f}' "$SHIM_SQL_LOG"
nplan=0; nfull=0; pages=0
for f in "$SCRATCH"/t22stmts/s*; do
  [[ -s "$f" ]] || continue
  sed -i "/^SELECT '@@END@@';\$/d" "$f"
  grep -q '^--version' "$f" && continue
  grep -q 'octet_length' "$f" && grep -q 'group_concat(a' "$f" && continue
  plan="$(sqlite3 -safe "file:$DB?immutable=1" "EXPLAIN QUERY PLAN $(cat "$f")" 2>&1)" || { echo "$plan" >&2; fail "T22: cannot explain a statement of the run"; }
  nplan=$((nplan + 1))
  if grep -qE 'SCAN (part|message|p|m)( |$)' <<<"$plan"; then
    nfull=$((nfull + 1)); echo "   full scan in: $(head -c 120 "$f" | tr '\n' ' ')" >&2; grep -E 'SCAN (part|message|p|m)( |$)' <<<"$plan" >&2
  fi
  if grep -q 'WITH b AS MATERIALIZED' "$f"; then
    pages=$((pages + 1))
    grep -q 'FROM part' "$f" && fail "T22: a selection page statement reads the part table"
  fi
done
[[ "$nfull" == "0" ]] || fail "T22: $nfull statement(s) scan part or message whole"
[[ "$pages" == "3" ]] || fail "T22: 130 sessions must be walked in 3 pages of 50, saw $pages selection statement(s)"
(( nplan >= 8 )) || fail "T22: too few statements were checked ($nplan)"
pass T22 "$nplan statements explained, 0 full scans of part or message, 3 selection pages for 130 sessions"
fi

# =============================================================================
if run_test T23; then
echo "== T23 chunk reads are bounded in rows, bytes and time: a row changed within the quiet slack is left for the next run"
new_home t23; use_stub; use_shim
NOW_MS="$(date +%s%3N)"
SHIM_SQL_LOG="$REC/sql.log"; : >"$SHIM_SQL_LOG"; OCENV+=(SHIM_SQL_LOG="$SHIM_SQL_LOG")
DB="$SCRATCH/t23.db"; mkdb "$DB"
T0=$(( NOW_MS - 3 * 3600 * 1000 ))
RECENT=$(( NOW_MS - 20000 ))   # 20 s ago: inside the default 120 s slack
ins_session "$DB" s23 /tmp/t23proj $T0 $T0
ins_message "$DB" m23a s23 assistant $T0 $((T0 + 1000)) $((T0 + 900))
ins_message "$DB" m23b s23 assistant $((T0 + 5000)) $((T0 + 6000)) $((T0 + 5900))
ins_part "$DB" p23a1 m23a s23 $((T0 + 100)) $((T0 + 100)) '{"type":"text","text":"old part — é日本"}'
ins_part "$DB" p23a2 m23a s23 $((T0 + 200)) $RECENT '{"type":"text","text":"changed 20 s ago — é日本😀"}'
ins_part "$DB" p23b1 m23b s23 $((T0 + 5100)) $((T0 + 5100)) '{"type":"text","text":"newest message, quiet — é日本"}'
oc "$DB" --scan --summary >"$SCRATCH/t23.out1" 2>"$SCRATCH/t23.err1"; rc=$?
[[ "$rc" == "0" ]] || fail "T23: run 1 exited $rc: $(tail -2 "$SCRATCH/t23.err1")"
rec_ids | grep -q '^opencode:part:p23a1:u' && rec_ids | grep -q '^opencode:part:p23b1:u' || fail "T23: the quiet rows of the session must be read in run 1"
rec_ids | grep -q '^opencode:part:p23a2:u' && fail "T23: a row changed 20 s ago (inside the slack) was read"
rec_ids | grep -q '^opencode:message:m23a:u' && fail "T23: a message whose part changed inside the slack was read before that part was quiet"
jq -e '.p.done_sig == ""' "$CURS/s23.json" >/dev/null || fail "T23: a session with a row inside the slack must not be marked done"
# bounds are in the statement itself
grep -q 'LIMIT 500' "$SHIM_SQL_LOG" || fail "T23: a chunk read must carry the row bound LIMIT 500"
grep -qE 'before < [0-9]+' "$SHIM_SQL_LOG" || fail "T23: a chunk read must carry the byte bound"
[[ "$(grep -cE 'time_updated <= [0-9]{13}' "$SHIM_SQL_LOG")" -ge 1 ]] || fail "T23: a chunk read must carry the time bound (the quiet slack)"
# run 2: the slack shrinks below the row's age: it is read now, and the session is done
oc "$DB" --scan --quiet-for 5 --summary >"$SCRATCH/t23.out2" 2>"$SCRATCH/t23.err2" || fail "T23: run 2 failed"
rec_ids | grep -q '^opencode:part:p23a2:u' && rec_ids | grep -q '^opencode:message:m23a:u' || fail "T23: the deferred row and its message must be read once they are older than the slack"
jq -e '.p.done_sig != ""' "$CURS/s23.json" >/dev/null || fail "T23: the session must be done once everything is older than the slack"
# a row bound of 7 shows in the statement, and the run still completes
new_home t23b; use_stub; use_shim
SHIM_SQL_LOG="$REC/sql.log"; : >"$SHIM_SQL_LOG"; OCENV+=(SHIM_SQL_LOG="$SHIM_SQL_LOG" MEMOIRE_OPENCODE_ROW_CHUNK=7)
oc "$DB" --scan --quiet-for 5 >/dev/null 2>&1 || fail "T23: chunk-7 run failed"
grep -q 'LIMIT 7' "$SHIM_SQL_LOG" || fail "T23: MEMOIRE_OPENCODE_ROW_CHUNK=7 must bound the read to 7 rows"
pass T23 "row, byte and time bounds in every chunk statement; a row inside the slack waits for the next run"
fi

# =============================================================================
if run_test T24; then
echo "== T24 spill content per batch is capped: past the cap the event is flagged and the part is read again"
new_home t24; use_stub
DB="$SCRATCH/t24.db"; mkdb "$DB"
T0=$(( NOW_MS - 3 * 3600 * 1000 ))
ins_session "$DB" s24 /tmp/t24proj $T0 $T0
ins_message "$DB" m24 s24 assistant $T0 $((T0 + 9000)) $((T0 + 8000))
mkdir -p "$TOOLDIR"
for i in 1 2 3 4; do
  { for k in $(seq 1 250); do printf 'é%d' $((k % 10)); done; } >"$TOOLDIR/tool_cap$i"   # ~750 bytes of multibyte text
  data="$(jq -nc --arg o "in-row tail $i é" --arg p "$TOOLDIR/tool_cap$i" '{type: "tool", tool: "bash", callID: "c", state: {status: "completed", input: {}, output: $o, title: "t", metadata: {truncated: true, outputPath: $p}, time: {start: 1, end: 2}}}')"
  ins_part "$DB" "pc$i" m24 s24 $((T0 + i * 100)) $((T0 + i * 100)) "$data"
done
SZ="$(stat -c %s "$TOOLDIR/tool_cap1")"
OCENV+=(MEMOIRE_OPENCODE_SPILL_MAX_BYTES=$((SZ * 2)) MEMOIRE_OPENCODE_SPILL_BATCH_BYTES=$((SZ * 2 + SZ / 2)))
oc "$DB" --scan --summary >"$SCRATCH/t24.out" 2>"$SCRATCH/t24.err"; rc=$?
[[ "$rc" == "0" ]] || fail "T24: run exited $rc: $(tail -2 "$SCRATCH/t24.err")"
dfr="$(rec_items | jq -c 'select(.source_event_id | endswith(":deferred"))')"
[[ "$(grep -c . <<<"$dfr")" == "1" ]] || fail "T24: exactly one deferred outcome expected (got $(grep -c . <<<"$dfr")): $(rec_ids | grep -c deferred)"
jq -e '.source_event_id | startswith("opencode:part:pc3:u")' <<<"$dfr" >/dev/null || fail "T24: the third spill (2 x $SZ + $SZ > the cap) is the one to defer: $(jq -r .source_event_id <<<"$dfr")"
jq -e '.payload.truncated_by_harness == true and .payload.spill_deferred == true and .payload.spill_attached == null and .payload.state_out.output == "in-row tail 3 é"' <<<"$dfr" >/dev/null \
  || fail "T24: the deferred event must carry truncated_by_harness, spill_deferred and the output of the row: $(jq -c '.payload | {truncated_by_harness, spill_deferred, spill_attached}' <<<"$dfr")"
for i in 1 2 3 4; do
  o="$(rec_items | jq -c --arg p "opencode:part:pc$i:u" 'select((.source_event_id | startswith($p)) and (.source_event_id | endswith(":outcome")))')"
  jq -e --argjson n "$SZ" '.payload.spill_attached == true and .payload.spill_bytes == $n' <<<"$o" >/dev/null || fail "T24: part pc$i must end up with its spill attached (got: $(jq -c '.payload | {spill_attached, spill_bytes, spill_deferred}' <<<"$o"))"
  [[ "$(jq -j '.payload.spill_output' <<<"$o")" == "$(cat "$TOOLDIR/tool_cap$i")" ]] || fail "T24: the spill of pc$i is not byte-identical"
done
[[ "$(rec_ids | grep -c ':deferred$')" == "1" ]] || fail "T24: only one part is deferred"
sorted="$(rec_ids | grep '^opencode:part:pc3:' | LC_ALL=C sort | tr '\n' ' ')"
[[ "$sorted" == *":deferred "*":outcome "* ]] || fail "T24: the deferred id must sort before the final outcome: $sorted"
bad="$(rec_items | jq -c '[.source_event_id, (.payload | tojson)]' | sort -u | jq -r '.[0]' | sort | uniq -d)"
[[ -z "$bad" ]] || fail "T24: an id carries two contents: $bad"
grep -q 'SPILL_BATCH_BYTES:-67108864' "$ADAPTER" && grep -q 'SPILL_MAX_BYTES:-67108864' "$ADAPTER" || fail "T24: the default caps must be 64 MiB"
pass T24 "cap of 2.5 files per batch: pc3 flagged spill_deferred and read again; all four spills end up attached, byte-identical"
fi

# =============================================================================
if run_test T25; then
echo "== T25 error classes: a malformed database or row falls back like busy/locked; a file: URI is refused; the parameter is a path"
T0=$(( NOW_MS - 3 * 3600 * 1000 ))
D25="$SCRATCH/t25"; rm -rf "$D25"; mkdir -p "$D25"
mkdb "$D25/good.db"
ins_session "$D25/good.db" s25 /tmp/t25proj $T0 $T0
ins_message "$D25/good.db" m25 s25 user $T0 $T0
for i in 1 2 3 4 5 6; do ins_part "$D25/good.db" "p25-$i" m25 s25 $((T0 + i)) $((T0 + i)) '{"type":"text","text":"contenu '"$i"' é日本"}'; done
# (a) a malformed database file: page 1 (the schema) is scribbled over
cp "$D25/good.db" "$D25/broken.db"
head -c 200 /dev/zero | tr '\0' '\377' | dd of="$D25/broken.db" bs=1 seek=100 conv=notrunc status=none
new_home t25a; use_stub
FALLBACK_DB="$D25/good.db"
oc "$D25/broken.db" --scan --summary >/dev/null 2>"$SCRATCH/t25.err"; rc=$?
FALLBACK_DB="$SCRATCH/no-such-archive.db"
[[ "$rc" == "0" ]] || fail "T25: a malformed primary with a good archive must fall back (rc=$rc): $(tail -2 "$SCRATCH/t25.err")"
grep -q 'primary database corrupt error' "$SCRATCH/t25.err" && grep -q 'falling back to the archive copy' "$SCRATCH/t25.err" || fail "T25: the malformed primary must be classified and the fallback logged: $(head -3 "$SCRATCH/t25.err")"
rec_ids | grep -q '^opencode:part:p25-6:u' || fail "T25: the fallback run did not capture the archive's rows"
# (b) a malformed row in the middle of a run (injected at the first chunk read): fallback, run completes
new_home t25b; use_stub; use_shim
FALLBACK_DB="$D25/good.db"
OCENV+=(SHIM_FAIL_MATCH="$D25/good.db" SHIM_FAIL_AFTER=6 SHIM_FAIL_TEXT="database disk image is malformed")
mkdir -p "$D25/prim"; cp "$D25/good.db" "$D25/prim/primary.db"
OCENV+=(SHIM_FAIL_MATCH="$D25/prim/primary.db")
oc "$D25/prim/primary.db" --scan --summary >/dev/null 2>"$SCRATCH/t25.err"; rc=$?
FALLBACK_DB="$SCRATCH/no-such-archive.db"
[[ "$rc" == "0" ]] || fail "T25: a malformed row at a later read must fall back (rc=$rc): $(tail -2 "$SCRATCH/t25.err")"
grep -q 'primary database corrupt error' "$SCRATCH/t25.err" || fail "T25: the malformed class must be named: $(head -3 "$SCRATCH/t25.err")"
rec_ids | grep -q '^opencode:part:p25-6:u' || fail "T25: the run did not complete through the fallback"
# (c) a file: URI is refused, for the source and for the fallback; nothing is opened or created
for form in env cli fb; do
  new_home t25c; use_stub
  case "$form" in
    env) OCENV+=(MEMOIRE_OPENCODE_DB="file:$D25/good.db?mode=rw"); oc "$D25/good.db" --scan >/dev/null 2>"$SCRATCH/t25.err"; rc=$? ;;
    cli) oc "file:$D25/good.db?mode=rw" --scan >/dev/null 2>"$SCRATCH/t25.err"; rc=$? ;;
    fb) FALLBACK_DB="file:$D25/good.db?mode=rw"; oc "$D25/good.db" --scan >/dev/null 2>"$SCRATCH/t25.err"; rc=$?; FALLBACK_DB="$SCRATCH/no-such-archive.db" ;;
  esac
  [[ "$rc" == "2" ]] || fail "T25: a file: URI ($form) must be refused with exit 2 (rc=$rc)"
  grep -q 'refused the source' "$SCRATCH/t25.err" && grep -q 'not a file: URI' "$SCRATCH/t25.err" || fail "T25: the refusal ($form) must be logged: $(cat "$SCRATCH/t25.err")"
  [[ "$(qcount)" == "0" ]] || fail "T25: something was queued after a refused source ($form)"
done
[[ ! -e "$D25/good.db-wal" && ! -e "$D25/good.db-shm" ]] || fail "T25: a file was created beside the database"
# (d) the parameter is a path: characters that are syntax in a URI stay literal
mkdir -p "$D25/odd?dir#x%y"; cp "$D25/good.db" "$D25/odd?dir#x%y/open code.db"
new_home t25d; use_stub
oc "$D25/odd?dir#x%y/open code.db" --scan >/dev/null 2>"$SCRATCH/t25.err" || fail "T25: a path with ? # % and a space failed: $(tail -2 "$SCRATCH/t25.err")"
rec_ids | grep -q '^opencode:part:p25-6:u' || fail "T25: a path with URI syntax characters was not read as a path"
pass T25 "malformed db and malformed row -> fallback; file: URI refused (env, --db, fallback); odd path read"
fi

# =============================================================================
if run_test T26; then
echo "== T26 ids are validated against [A-Za-z0-9_.-]+: a row that fails is logged, skipped and counted"
new_home t26; use_stub
DB="$SCRATCH/t26.db"; mkdb "$DB"
T0=$(( NOW_MS - 3 * 3600 * 1000 ))
LONG="$(head -c 101 /dev/zero | tr '\0' 'L')"
ins_session "$DB" "ses.A-b_1" /tmp/t26proj $T0 $T0
ins_message "$DB" "msg.1" "ses.A-b_1" assistant $T0 $((T0 + 500)) $((T0 + 400))
ins_part "$DB" "prt.2-x_y" "msg.1" "ses.A-b_1" $((T0 + 100)) $((T0 + 100)) '{"type":"text","text":"id valide — é日本😀"}'
ins_message "$DB" "m v" "ses.A-b_1" user $((T0 + 600)) $((T0 + 600))                                  # a space
ins_part "$DB" "pvalid" "m v" "ses.A-b_1" $((T0 + 610)) $((T0 + 610)) '{"type":"text","text":"valid part id, invalid message id"}'
ins_part "$DB" "p é" "msg.1" "ses.A-b_1" $((T0 + 620)) $((T0 + 620)) '{"type":"text","text":"invalid part id (accent, space)"}'
ins_part "$DB" "pnl" "msg.1" "ses.A-b_1" $((T0 + 630)) $((T0 + 630)) '{"type":"text","text":"ok"}'
# a session id with an embedded NUL (the text stops at the NUL for GLOB and length) and a 101-byte one
sqlite3 "$DB" "INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES (CAST(X'7300 31' AS TEXT), 'prj_t', 's', '/tmp/t26proj', 't', '1', $T0, $T0);" 2>/dev/null \
  || sqlite3 "$DB" "INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES (CAST(X'730031' AS TEXT), 'prj_t', 's', '/tmp/t26proj', 't', '1', $T0, $T0);"
ins_session "$DB" "$LONG" /tmp/t26proj $T0 $T0
oc "$DB" --scan --summary >"$SCRATCH/t26.out" 2>"$SCRATCH/t26.err"; rc=$?
[[ "$rc" == "0" ]] || fail "T26: run exited $rc: $(tail -2 "$SCRATCH/t26.err")"
rec_ids | grep -q '^opencode:part:prt.2-x_y:u' && rec_ids | grep -q '^opencode:message:msg.1:u' && rec_ids | grep -q '^opencode:session:ses.A-b_1:facts:' \
  || fail "T26: ids with . - _ are valid and must be captured"
rec_ids | grep -q '^opencode:part:pnl:u' || fail "T26: a valid row next to invalid ones must be captured"
rec_ids | grep -q 'pvalid\|p é\|m v' && fail "T26: a row whose id (or whose message id) is invalid produced an event"
grep -q 'rejected=2 rejected_rows=3' "$SCRATCH/t26.err" || fail "T26: want rejected=2 (NUL id, 101-byte id) rejected_rows=3 in the summary, got: $(grep -o 'rejected=[0-9]* rejected_rows=[0-9]*' "$SCRATCH/t26.err")"
[[ "$(grep -c 'skipping a row whose id' "$SCRATCH/t26.err")" == "3" ]] || fail "T26: each skipped row must be logged: $(grep -c 'skipping a row' "$SCRATCH/t26.err")"
[[ "$(cut -f4 "$SCRATCH/t26.out")" == "5" ]] || fail "T26: the summary line must carry rejected=5 (got: $(cat "$SCRATCH/t26.out"))"
[[ "$(grep -c 'refusing a session whose id' "$SCRATCH/t26.err")" == "2" ]] || fail "T26: both refused sessions must be logged"
pass T26 "valid dotted ids captured; 2 sessions refused, 3 rows skipped and counted (rejected=5)"
fi

# =============================================================================
if run_test T27; then
echo "== T27 no comment cites a document outside the repository"
P1='design r'; P2='amend'; P3='ments'
PAT="${P1}[0-9]|${P2}${P3}|AMEND""MENTS|FORM""ATS\\.md|DES""IGN\\.md|REV""IEW-|/tmp/memory-audit|t1-lot|[^a-z]lanes?/|round [0-9]|the review""er|reviewer's|Codex G[0-9]|census-set|/home/""michel"
hits=""
for f in scripts/memoire-capture-opencode.sh scripts/memoire-capture-opencode-tick.sh scripts/capture-opencode-tick-timer.sh \
         tests/foreign/opencode-capture.sh scripts/tests/opencode-capture-tick-test.sh tests/foreign/fixtures/opencode-census.sql; do
  [[ -r "$ROOT/$f" ]] || fail "T27: $f is missing"
  h="$(grep -nEi -- "$PAT" "$ROOT/$f" | grep -v 'PAT=' | head -3)"
  [[ -z "$h" ]] || hits+="$f: $h"$'\n'
  # short labels used to cite a review or an amendment
  h="$(grep -nE '(^|[ (,])(F1[0-5]?|F[2-9]|H1[0-4]?|H[2-9]|A(8|9|10|11)|D[1-9])( |,|\)|:|/|$)' "$ROOT/$f" | grep -E '^[0-9]+:\s*(#|--)' | head -3)"
  [[ -z "$h" ]] || hits+="$f (label): $h"$'\n'
done
[[ -z "$hits" ]] || fail "T27: comments cite documents outside the repository:
$hits"
pass T27 "adapter, tick, timer, tests and fixture carry no external citation"
fi

# =============================================================================
if run_test T28; then
echo "== T28 sqlite3 is checked once there is a database to read (exit 1, one log line); nothing to read exits 0 without any check"
new_home t28; use_stub
DB="$SCRATCH/t28.db"; mkdb "$DB"
ins_session "$DB" s28 /tmp/t28proj $OLD $OLD
fake() { # version -> a sqlite3 that reports it and otherwise is the real one
  mkdir -p "$SCRATCH/fake-$1"
  printf '#!/usr/bin/env bash\n[[ "${1:-}" == "--version" ]] && { echo "%s 2024-01-01 00:00:00 abcdef (64-bit)"; exit 0; }\nexec %s "$@"\n' "$1" "$REAL_SQLITE3" >"$SCRATCH/fake-$1/sqlite3"
  chmod +x "$SCRATCH/fake-$1/sqlite3"
}
for v in 3.40.1 3.43.9 2.9.9; do
  fake "$v"; new_home "t28$v"; use_stub
  OCENV+=(PATH="$SCRATCH/fake-$v:$PATH")
  SQLITE_BIN=sqlite3
  oc "$DB" --full >/dev/null 2>"$SCRATCH/t28.err"; rc=$?
  [[ "$rc" == "1" ]] || fail "T28: sqlite3 $v must give exit 1 (rc=$rc)"
  [[ "$(grep -c . "$SCRATCH/t28.err")" == "1" ]] && grep -q "sqlite3 >= 3.44 is required, found '$v'" "$SCRATCH/t28.err" || fail "T28: sqlite3 $v must log one line naming the version: $(cat "$SCRATCH/t28.err")"
  [[ "$(qcount)" == "0" && "$(rec_count)" == "0" ]] || fail "T28: something was captured with sqlite3 $v"
done
for v in 3.44.0 3.51.2 4.0.0; do
  fake "$v"; new_home "t28$v"; use_stub
  OCENV+=(PATH="$SCRATCH/fake-$v:$PATH")
  SQLITE_BIN=sqlite3
  oc "$DB" --full >/dev/null 2>"$SCRATCH/t28.err" || fail "T28: sqlite3 $v must be accepted: $(tail -2 "$SCRATCH/t28.err")"
done
# jq >= 1.7, same rule and exit as the Codex adapter
REAL_JQ="$(command -v jq)"
fakejq() { # version -> a jq that reports it and otherwise is the real one
  mkdir -p "$SCRATCH/fakejq-$1"
  printf '#!/usr/bin/env bash\n[[ "${1:-}" == "--version" ]] && { echo "jq-%s"; exit 0; }\nexec %s "$@"\n' "$1" "$REAL_JQ" >"$SCRATCH/fakejq-$1/jq"
  chmod +x "$SCRATCH/fakejq-$1/jq"
}
for v in 1.6 1.5 0.9; do
  fakejq "$v"; new_home "t28j$v"; use_stub
  OCENV+=(PATH="$SCRATCH/fakejq-$v:$PATH")
  oc "$DB" --full >/dev/null 2>"$SCRATCH/t28.err"; rc=$?
  [[ "$rc" == "1" ]] || fail "T28: jq-$v must give exit 1 (rc=$rc)"
  [[ "$(grep -c . "$SCRATCH/t28.err")" == "1" ]] && grep -q "jq is required (>= 1.7), found 'jq-$v'" "$SCRATCH/t28.err" || fail "T28: jq-$v must log one line naming the version: $(cat "$SCRATCH/t28.err")"
  [[ "$(qcount)" == "0" && "$(rec_count)" == "0" ]] || fail "T28: something was captured with jq-$v"
done
for v in 1.7 1.7.1 1.8.1 2.0; do
  fakejq "$v"; new_home "t28j$v"; use_stub
  OCENV+=(PATH="$SCRATCH/fakejq-$v:$PATH")
  oc "$DB" --full >/dev/null 2>"$SCRATCH/t28.err" || fail "T28: jq-$v must be accepted: $(tail -2 "$SCRATCH/t28.err")"
done
new_home t28jn; use_stub; OCENV+=(PATH="$SCRATCH/fakejq-1.6:$PATH")
oc "$SCRATCH/absent-28.db" --full >/dev/null 2>"$SCRATCH/t28.err"; rc=$?
[[ "$rc" == "0" ]] && grep -q 'no database to read' "$SCRATCH/t28.err" || fail "T28: nothing to read must exit 0 whatever the jq version (rc=$rc): $(cat "$SCRATCH/t28.err")"
# a missing sqlite3 with a database to read: exit 1, one line
new_home t28m; use_stub; SQLITE_BIN="$SCRATCH/no/such/sqlite3"
oc "$DB" --full >/dev/null 2>"$SCRATCH/t28.err"; rc=$?
[[ "$rc" == "1" && "$(grep -c . "$SCRATCH/t28.err")" == "1" ]] && grep -q 'sqlite3 is required' "$SCRATCH/t28.err" || fail "T28: a missing sqlite3 must give exit 1 and one line (rc=$rc): $(cat "$SCRATCH/t28.err")"
# nothing to read: no check at all, exit 0
new_home t28n; use_stub; SQLITE_BIN="$SCRATCH/no/such/sqlite3"
FALLBACK_DB="$SCRATCH/no-such-archive.db"
oc "$SCRATCH/absent-28.db" --full >/dev/null 2>"$SCRATCH/t28.err"; rc=$?
[[ "$rc" == "0" ]] && grep -q 'no database to read' "$SCRATCH/t28.err" || fail "T28: nothing to read must exit 0 before any tool check (rc=$rc): $(cat "$SCRATCH/t28.err")"
pass T28 "sqlite3 3.40.1, 3.43.9, 2.9.9 and jq 1.6, 1.5, 0.9 refused (exit 1, one line); sqlite3 3.44.0, 3.51.2, 4.0.0 and jq 1.7, 1.7.1, 1.8.1, 2.0 accepted; missing tool exit 1; nothing to read exit 0"
fi

# =============================================================================
if run_test T29; then
echo "== T29 a full scratch directory fails loudly: exit 1, one log line, nothing half-captured"
new_home t29; use_stub
DB="$SCRATCH/t29.db"; mkdb "$DB"
T0=$(( NOW_MS - 3 * 3600 * 1000 ))
{
  echo "BEGIN;"
  for i in $(seq 0 59); do
    id="$(printf 's%03d' "$i")"
    echo "INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES ('$id', 'prj_t', 's', '/tmp/t29proj', 't', '1', $T0, $T0);"
    echo "INSERT INTO message VALUES ('m$id', '$id', $T0, $T0, '{\"role\":\"user\",\"time\":{\"created\":$T0}}');"
    echo "INSERT INTO part VALUES ('p$id', 'm$id', '$id', $T0, $T0, '{\"type\":\"text\",\"text\":\"bonjour é $id\"}');"
  done
  echo "COMMIT;"
} | sqlite3 "$DB"
# A file-size limit of 1 KiB stands for a full disk: sqlite3 truncates its output
# silently and exits 0, printf and jq fail. (SIGXFSZ is ignored so writes fail
# with an error instead of killing the process.)
oc_prep "$DB"
bash -c 'ulimit -f 1; trap "" XFSZ; exec "$@"' _ "${OCC[@]}" --scan --summary >"$SCRATCH/t29.out" 2>"$SCRATCH/t29.err"; rc=$?
[[ "$rc" == "1" ]] || fail "T29: a full scratch must exit 1, not $rc (candidates: $(grep -o 'candidates=[0-9]*' "$SCRATCH/t29.err")): $(cat "$SCRATCH/t29.err")"
[[ "$(grep -c . "$SCRATCH/t29.err")" == "1" ]] && grep -q 'scratch write failed' "$SCRATCH/t29.err" || fail "T29: one log line 'scratch write failed' expected: $(cat "$SCRATCH/t29.err")"
[[ "$(rec_count)" == "0" ]] || fail "T29: events were queued although the selection could not be read ($(rec_count))"
[[ -z "$(ls -A "$MH/tmp/opencode-capture" 2>/dev/null | grep '^run\.')" ]] || fail "T29: the work directory was not removed after the failure"
# healed: the next run with room captures everything
oc "$DB" --scan --summary >/dev/null 2>"$SCRATCH/t29.err" || fail "T29: the healed run failed: $(tail -2 "$SCRATCH/t29.err")"
[[ "$(rec_ids | grep -c ':facts:')" == "60" ]] || fail "T29: the healed run must capture the 60 sessions (got $(rec_ids | grep -c ':facts:'))"
pass T29 "1 KiB file-size limit: exit 1, one log line, nothing queued; healed run captures 60 sessions"
fi

# =============================================================================
if run_test T30; then
echo "== T30 the work directory is private, under the Memoire home, never in TMPDIR; kill -9 leaves one, the next run clears it"
new_home t30; use_stub
DB="$SCRATCH/t30.db"; mkdb "$DB"
T0=$(( NOW_MS - 3 * 3600 * 1000 ))
ins_session "$DB" s30 /tmp/t30proj $T0 $T0
for i in $(seq 1 40); do
  ins_message "$DB" "m30-$i" s30 user $((T0 + i)) $((T0 + i))
  ins_part "$DB" "p30-$i" "m30-$i" s30 $((T0 + i)) $((T0 + i)) "{\"type\":\"text\",\"text\":\"contenu privé $i é日本\"}"
done
OCENV+=(STUB_SLEEP=1.5 MEMOIRE_OPENCODE_ROW_CHUNK=10)
oc_prep "$DB"
( exec setsid "${OCC[@]}" --scan >/dev/null 2>&1 ) &
gpid=$!
sleep 1.8
WD="$(ls -d "$MH"/tmp/opencode-capture/run.* 2>/dev/null | head -1)"
[[ -n "$WD" && -d "$WD" ]] || { kill -9 -- "-$gpid" 2>/dev/null; fail "T30: no work directory under the Memoire home during a run"; }
[[ "$(stat -c %a "$WD")" == "700" && "$(stat -c %a "$MH/tmp")" == "700" && "$(stat -c %a "$MH/tmp/opencode-capture")" == "700" ]] || fail "T30: the work directories must be mode 0700"
badmode="$(find "$WD" -type f ! -perm 600 | head -3)"
[[ -z "$badmode" ]] || fail "T30: files in the work directory must be mode 0600: $badmode"
[[ -z "$(ls -A "$SCRATCH/tmp")" ]] || fail "T30: a run put files in TMPDIR: $(ls -A "$SCRATCH/tmp" | head -3 | tr '\n' ' ')"
kill -9 -- "-$gpid" 2>/dev/null; wait "$gpid" 2>/dev/null
[[ "$(ls -d "$MH"/tmp/opencode-capture/run.* 2>/dev/null | wc -l)" == "1" ]] || fail "T30: after kill -9 one work directory must be left"
OCENV=(); use_stub
oc "$DB" --scan >/dev/null 2>&1 || fail "T30: the run after the kill failed"
[[ "$(ls -d "$MH"/tmp/opencode-capture/run.* 2>/dev/null | wc -l)" == "0" ]] || fail "T30: the next run must clear the directory a killed run left"
# an old directory of a LIVE run (its lock is held) is kept
mkdir -p "$MH/tmp/opencode-capture/run.LIVE1"; : >"$MH/tmp/opencode-capture/run.LIVE1/lock"; touch -d '3 days ago' "$MH/tmp/opencode-capture/run.LIVE1"
( exec 8>"$MH/tmp/opencode-capture/run.LIVE1/lock"; flock -n 8 && sleep 3 ) &
holder=$!; sleep 0.4
oc "$DB" --scan >/dev/null 2>&1
[[ -d "$MH/tmp/opencode-capture/run.LIVE1" ]] || fail "T30: the directory of a live run (lock held) must be kept"
wait "$holder" 2>/dev/null
oc "$DB" --scan >/dev/null 2>&1
[[ ! -d "$MH/tmp/opencode-capture/run.LIVE1" ]] || fail "T30: once its lock is free the directory must go"
pass T30 "0700/0600 under the Memoire home, TMPDIR untouched; kill -9 leftover cleared by the next run through the lock"
fi

# =============================================================================
if run_test T32; then
echo "== T32 a BLOB in message.data or part.data does not poison its session: captured as base64 (raw_blob, blob: true), counted, rows after it captured, a second run works"
new_home t32; use_stub
DB="$SCRATCH/t32.db"; mkdb "$DB"
T0=$(( NOW_MS - 3 * 3600 * 1000 ))
ins_session "$DB" s32 /tmp/t32proj $T0 $T0
ins_message "$DB" m32a s32 user $T0 $T0
ins_message "$DB" m32b s32 assistant $((T0 + 1000)) $((T0 + 1000)) $((T0 + 1900))
ins_part "$DB" p32a m32a s32 $((T0 + 10)) $((T0 + 10)) '{"type":"text","text":"avant le blob é日本"}'
# blobs of 1, 2, 3 and 300 bytes (every padding case), and one with the bytes of valid JSON text
B300="$(head -c 300 /dev/urandom | od -An -v -tx1 | tr -d ' \n')"
i=0
for hx in 41 4142 414243 "$B300" 7B7D; do
  i=$((i + 1))
  sqlite3 "$DB" "INSERT INTO part VALUES ('p32blob$i', 'm32a', 's32', $((T0 + 100 + i)), $((T0 + 100 + i)), X'$hx');"
done
sqlite3 "$DB" "INSERT INTO message VALUES ('m32blob', 's32', $((T0 + 500)), $((T0 + 500)), X'00FF10');"
ins_part "$DB" p32z m32b s32 $((T0 + 2000)) $((T0 + 2000)) '{"type":"text","text":"après le blob 😀"}'
oc "$DB" --scan --summary >"$SCRATCH/t32.out" 2>"$SCRATCH/t32.err"; rc=$?
[[ "$rc" == "0" ]] || fail "T32: a blob row must not fail the run (rc=$rc): $(tail -2 "$SCRATCH/t32.err")"
grep -q 'failed_sessions=0 ' "$SCRATCH/t32.err" || fail "T32: no session may fail: $(tail -1 "$SCRATCH/t32.err")"
grep -q ' blobs=6 ' "$SCRATCH/t32.err" && [[ "$(tail -n 1 "$SCRATCH/t32.out" | cut -f7)" == "6" ]] || fail "T32: six blob rows must be counted in the log line and the summary: $(tail -1 "$SCRATCH/t32.err") / $(tail -n 1 "$SCRATCH/t32.out")"
rec_ids | grep -q '^opencode:part:p32a:u' && rec_ids | grep -q '^opencode:part:p32z:u' || fail "T32: the rows before and after the blobs must be captured"
b64() { printf "$(sed 's/../\\x&/g' <<<"$1")" | base64 -w0; }
i=0; bad=0
for hx in 41 4142 414243 "$B300" 7B7D; do
  i=$((i + 1)); want="$(b64 "$hx")"
  got="$(rec_items | jq -r --arg id "p32blob$i" 'select(.payload.part_id == $id) | select(.payload.part.blob == true) | .payload.part.raw_blob')"
  [[ "$got" == "$want" ]] || { bad=$((bad + 1)); echo "   blob $i: want $want got $got" >&2; }
done
[[ "$bad" == "0" ]] || fail "T32: $bad part blob(s) not captured as the base64 of their bytes"
got="$(rec_items | jq -c 'select(.event_type == "opencode_message" and .payload.message_id == "m32blob") | .payload.message')"
[[ "$got" == '{"blob":true,"raw_blob":"AP8Q"}' ]] || fail "T32: a blob message.data must be captured as {blob:true, raw_blob} (got $got)"
n1="$(rec_count)"
# a second run, same cursor, then a fresh cursor: no error, the same set
oc "$DB" --scan --summary --verify >/dev/null 2>"$SCRATCH/t32.err2" || fail "T32: a second run over the same session failed: $(tail -2 "$SCRATCH/t32.err2")"
grep -q 'failed_sessions=0 ' "$SCRATCH/t32.err2" || fail "T32: the second run failed a session: $(tail -1 "$SCRATCH/t32.err2")"
ids1="$(rec_ids)"
new_home t32b; use_stub
oc "$DB" --scan >/dev/null 2>"$SCRATCH/t32.err3" || fail "T32: a run with a fresh cursor failed: $(tail -2 "$SCRATCH/t32.err3")"
[[ "$(rec_ids)" == "$ids1" && "$(rec_count)" == "$n1" ]] || fail "T32: a fresh run must capture the same set ($(rec_count) vs $n1 events)"
pass T32 "6 blob rows (5 part, 1 message; 1, 2, 3, 300 bytes, JSON-looking) captured as base64 and counted; rows around them captured; rerun and fresh run clean"
fi

# =============================================================================
if run_test T33; then
echo "== T33 a part the Memoire plugin injected (id prefix prt_memoire_) is read and never queued; the cursor passes it; any other synthetic part is still captured"
# The reader's prefix and the plugin's id construction must stay in step.
T33_PFX="$(sed -n 's/^MEMOIRE_PART_PREFIX="\(.*\)"$/\1/p' "$ROOT/scripts/memoire-capture-opencode.sh")"
[[ -n "$T33_PFX" ]] && grep -qF "id: \`${T33_PFX}\${crypto.randomUUID()}\`" "$ROOT/scripts/opencode-plugin/memoire.js" \
  || fail "T33: the plugin's part id (scripts/opencode-plugin/memoire.js) does not start with the reader's MEMOIRE_PART_PREFIX ('$T33_PFX')"
new_home t33
DB="$SCRATCH/t33.db"; mkdb "$DB"
INJ='{"type":"text","text":"MEMOIRE-INJECTED-CONTEXT timeline é日本","synthetic":true}'
# (a) first user message: operator text part + injected part (newest row), then a completed assistant turn
ins_session "$DB" s33a /tmp/t33proj
ins_message "$DB" m33a1 s33a user $OLD $((OLD + 300))
ins_part "$DB" p33a-op m33a1 s33a $((OLD + 100)) $((OLD + 100)) '{"type":"text","text":"Operator prompt é日本😀"}'
ins_part "$DB" prt_memoire_aaaa1111 m33a1 s33a $((OLD + 150)) $((OLD + 300)) "$INJ"
ins_message "$DB" m33a2 s33a assistant $((OLD + 400)) $((OLD + 900)) $((OLD + 800))
ins_part "$DB" p33a-as m33a2 s33a $((OLD + 500)) $((OLD + 500)) '{"type":"text","text":"Assistant answer é日本"}'
# defensive: a plugin part on an assistant message stays out of the turn text
ins_part "$DB" prt_memoire_aaaa2222 m33a2 s33a $((OLD + 600)) $((OLD + 600)) "$INJ"
# a user message whose ONLY part is the injected one, and an injected part of another type
ins_message "$DB" m33a3 s33a user $((OLD + 1000)) $((OLD + 1100))
ins_part "$DB" prt_memoire_aaaa3333 m33a3 s33a $((OLD + 1000)) $((OLD + 1100)) "$INJ"
# (b) other synthetic parts are captured as today (also an id that only resembles the prefix)
ins_session "$DB" s33b /tmp/t33proj
ins_message "$DB" m33b s33b user $OLD $((OLD + 300))
ins_part "$DB" p33b-op m33b s33b $((OLD + 100)) $((OLD + 100)) '{"type":"text","text":"Operator B é日本"}'
ins_part "$DB" prt_harness_cont m33b s33b $((OLD + 150)) $((OLD + 150)) '{"type":"text","text":"Continue if you have next steps","synthetic":true}'
ins_part "$DB" prt_memoirex_look m33b s33b $((OLD + 160)) $((OLD + 160)) '{"type":"text","text":"lookalike prefix","synthetic":true}'
# (c) sub-agent session (parent_id set): the parent-context part has the same prefix
ins_session "$DB" s33c /tmp/t33proj
sqlite3 "$DB" "UPDATE session SET parent_id = 's33a' WHERE id = 's33c';"
ins_message "$DB" m33c s33c user $OLD $((OLD + 300))
ins_part "$DB" p33c-op m33c s33c $((OLD + 100)) $((OLD + 100)) '{"type":"text","text":"Sub-agent task é日本"}'
ins_part "$DB" prt_memoire_cccc1111 m33c s33c $((OLD + 150)) $((OLD + 150)) '{"type":"text","text":"MEMOIRE-INJECTED-CONTEXT parent activity","synthetic":true}'
ins_part "$DB" prt_memoire_cccc2222 m33c s33c $((OLD + 200)) $((OLD + 300)) '{"type":"text","text":"MEMOIRE-INJECTED-CONTEXT parent block 2","synthetic":true}'
# (d) one row per batch: a skipped part alone in a batch must not stall the session
ins_session "$DB" s33d /tmp/t33proj
ins_message "$DB" m33d s33d user $OLD $((OLD + 300))
ins_part "$DB" p33d-op m33d s33d $((OLD + 100)) $((OLD + 100)) '{"type":"text","text":"Operator D é日本"}'
ins_part "$DB" prt_memoire_dddd1111 m33d s33d $((OLD + 150)) $((OLD + 150)) "$INJ"
ins_part "$DB" prt_memoire_dddd2222 m33d s33d $((OLD + 200)) $((OLD + 200)) "$INJ"
ins_part "$DB" p33d-after m33d s33d $((OLD + 250)) $((OLD + 250)) '{"type":"text","text":"after the skipped rows é日本"}'
OCENV+=(MEMOIRE_OPENCODE_ROW_CHUNK=1)
oc "$DB" --full --summary >"$SCRATCH/t33.out1" 2>"$SCRATCH/t33.err1" || fail "T33: run failed: $(tail -2 "$SCRATCH/t33.err1")"
qids | grep -q 'prt_memoire_' && fail "T33: an event carries a prt_memoire_ part id: $(qids | grep prt_memoire_ | head -2)"
qfiles | xargs -r grep -l 'MEMOIRE-INJECTED-CONTEXT' >/dev/null 2>&1 && fail "T33: the injected text leaked into an event"
# (a) operator prompt with its own text only; message events and the turn
jq -e '.event_type == "opencode_user_prompt" and .payload.text == "Operator prompt é日本😀" and .payload.part.synthetic == null' <<<"$(qfiles | xargs -r jq -c 'select(.source_event_id | startswith("opencode:part:p33a-op:"))')" >/dev/null \
  || fail "T33a: the operator prompt must be captured with its own text only"
[[ "$(qfiles | xargs -r jq -r 'select(.event_type == "opencode_user_prompt" and .session_id == "s33a") | .payload.text')" == "Operator prompt é日本😀" ]] \
  || fail "T33a: session s33a must hold exactly one user prompt event"
[[ "$(qfiles | xargs -r jq -r 'select(.event_type == "opencode_assistant_turn" and .session_id == "s33a") | .text')" == "Assistant answer é日本" ]] \
  || fail "T33a: the turn text must be the assistant text only"
for m in m33a1 m33a2 m33a3; do
  qids | grep -q "^opencode:message:$m:u" || fail "T33a: the message event of $m is missing"
done
[[ "$(qfiles | xargs -r jq -r 'select(.event_type == "opencode_assistant_turn" and .session_id == "s33a") | .source_event_id')" == "opencode:turn:m33a2" ]] || fail "T33a: exactly the one assistant turn expected"
# (b)
jq -e '.event_type == "opencode_item" and .payload.synthetic == true and .payload.text == "Continue if you have next steps"' <<<"$(qfiles | xargs -r jq -c 'select(.source_event_id | startswith("opencode:part:prt_harness_cont:"))')" >/dev/null \
  || fail "T33b: a synthetic part with another prefix must still be captured as opencode_item with synthetic: true"
jq -e '.event_type == "opencode_item" and .payload.synthetic == true' <<<"$(qfiles | xargs -r jq -c 'select(.source_event_id | startswith("opencode:part:prt_memoirex_look:"))')" >/dev/null \
  || fail "T33b: only the exact prefix prt_memoire_ is skipped"
# (c)
jq -e '.event_type == "opencode_user_prompt" and .payload.text == "Sub-agent task é日本"' <<<"$(qfiles | xargs -r jq -c 'select(.source_event_id | startswith("opencode:part:p33c-op:"))')" >/dev/null \
  || fail "T33c: the sub-agent prompt must be captured"
qids | grep -q "^opencode:message:m33c:u" || fail "T33c: the sub-agent message event is missing"
# (d) every row was passed
qids | grep -q '^opencode:part:p33d-after:u' || fail "T33d: the part after the skipped rows was not captured (stalled at a batch of one skipped row)"
for s in s33a s33b s33c s33d; do
  jq -e '.p.done_sig | type == "string" and length > 0' "$CURS/$s.json" >/dev/null || fail "T33: $s has no done_sig after a complete read"
done
jq -e '.p.mu >= '$((OLD + 1100)) "$CURS/s33a.json" >/dev/null || fail "T33a: the cursor did not pass the injected parts"
jq -e '.p.mu >= '$((OLD + 300)) "$CURS/s33c.json" >/dev/null || fail "T33c: the cursor did not pass the injected parts"
# a second run, and a verify run (re-read from the cursor), queue nothing new
N1="$(qcount)"; IDS1="$(qids)"
oc "$DB" --full >/dev/null 2>"$SCRATCH/t33.err2" || fail "T33: second run failed"
oc "$DB" --verify >/dev/null 2>"$SCRATCH/t33.err3" || fail "T33: verify run failed"
[[ "$(qcount)" == "$N1" && "$(qids)" == "$IDS1" ]] || fail "T33: a rerun queued something new ($N1 -> $(qcount))"
pass T33 "$N1 events; 7 injected parts read, none queued; operator prompts and turn text clean; cursors past them; lookalike and harness synthetic parts kept"
fi

# =============================================================================
if run_test T31; then
echo "== T31 every new file carries the SPDX header in its first three lines"
for f in scripts/memoire-capture-opencode.sh scripts/memoire-capture-opencode-tick.sh scripts/capture-opencode-tick-timer.sh \
         tests/foreign/opencode-capture.sh scripts/tests/opencode-capture-tick-test.sh tests/foreign/fixtures/opencode-census.sql; do
  head -3 "$ROOT/$f" | grep -q 'SPDX-License-Identifier: AGPL-3.0-only' || fail "T31: missing SPDX header: $f"
done
pass T31 "6 files"
fi

fi   # not census-real

if [[ "$ONLY" == "census-real" ]]; then
  RDB="${MEMOIRE_OPENCODE_TEST_REAL_DB:-}"
  [[ -n "$RDB" && -r "$RDB" ]] || fail "census-real: set MEMOIRE_OPENCODE_TEST_REAL_DB to a scratch copy of the sessions you may read"
  new_home real
  oc "$RDB" --full >/dev/null 2>"$SCRATCH/real.err" || fail "census-real: adapter run failed: $(tail -2 "$SCRATCH/real.err")"
  echo "   events: $(qcount)"
  qfiles | xargs -r jq -r '.event_type' | sort | uniq -c
  census_compare "$RDB" "$QDIR" || fail "census-real: comparison failed"
  bytes_compare "$RDB" "$QDIR" || fail "census-real: counts or bytes differ"
  echo "   census-real ok"
fi

echo "opencode-capture: OK"
