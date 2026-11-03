#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# opencode-v2-capture.sh — guarantees of the native OpenCode adapter
# (scripts/memoire-capture-opencode.sh) on an OpenCode 2.0 store (session_v2 +
# session_message). The v1 suite (tests/foreign/opencode-capture.sh) keeps the
# shared guarantees (bounded reads, -safe, fallback, signals, scratch, ids);
# this one covers what the v2 schema changes. Everything runs under a scratch
# directory from mktemp -d (removed at exit); nothing reads the live store or
# ~/.memoire. The queue binary is built from this tree (a build failure FAILS).
#
#   V1   census on the synthetic v2 store (fixtures/opencode-v2-census.sql):
#        events per type equal the numbers written in the fixture; every text
#        of the store reaches exactly ONE event (nothing stored twice); the
#        never-read tables (credential, account*, session_pending,
#        session_inbox) and the refused session leave no trace; the schema is
#        detected (schema=v2 in the summary)
#   V2   the migration floor: the rows of a migrated session at or below
#        max(message.time_updated) emit nothing, the row edited after it emits
#        under the v2 mapping; MEMOIRE_OPENCODE_V2_FLOOR_MS=0 reads them all
#   V3   a v1 cursor carries over: a session read by the v1 mapping (cursor
#        k=m, done_sig of v1) resumes after its position, emits only what is
#        above the floor, and its cursor is now k=s, schema v2
#   V4   an unchanged store is skipped on the second run (done_sig of v2);
#        a session changed since is read alone
#   V5   the quiet and stuck gates on v2 rows: a tool item of the newest row
#        running for 10 minutes gates the session, one running for 2 hours
#        does not; a row changed 10 s ago gates its session
#   V6   spill files on v2 tool items: attached whole, missing -> spill_rejected,
#        truncated without a path -> truncated_by_harness
#   V7   a BLOB row and a non-JSON row produce the record event only and never
#        poison their session
#   V8   versioned re-emission: an edited row re-emits under a new version,
#        the old version stays, no second turn
#   V9   --schema v1 on a v2 store reads the frozen tables and warns about the
#        unmapped session_message rows; --schema v2 on a v1-only store fails
#        loudly (exit 1, one line)
#   V10  the mapping holds nothing twice: no `data` of a file in the prompt
#        event, no `content` in the record event, counts in their place
#   V11  the row is the unit of the spill batch cap: a row whose spills alone
#        exceed the cap is read alone and attached whole, the cursor moves on;
#        a row deferred because earlier rows filled the cap emits NOTHING in
#        that batch and is read whole in the next one; every id once
#   V12  a fallback to an archive copy of another schema (v1 archive behind a
#        v2 primary) is refused: one log line, exit 1, nothing queued, no
#        "no such table" crash
#   V13  a fallback DURING schema discovery (at the tables read, at the columns
#        read) restarts the discovery on the archive copy: a v2 archive
#        completes the run with the full census, a v1 archive is refused when
#        the primary's schema was already known and read as v1 when it was not;
#        the failed discovery statement is not retried on the copy: no
#        statement of one schema runs against the other
#   V14  no statement of a v2 run reads session_message, message or part whole
#        (EXPLAIN QUERY PLAN of every statement of a 134-session walk): no SCAN
#        of them, and every SEARCH of them names an index or the rowid (an
#        unindexed aggregate is a SEARCH in SQLite's words); every selection
#        page is a rowid-range SEARCH of the session table; every migration
#        floor read is a session_id index range of the frozen message table
#   V15  a group signal during initialization is a clean stop: at the initial
#        discovery (exit 0, ONE log line, stopped=1) and during the schema
#        refresh after a fallback (exit 0, the fallback line + the stop line,
#        no refusal, failed=0); the same when the signal lands on the ending
#        timer of the read after it printed its timestamp

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ADAPTER="${MEMOIRE_TEST_ADAPTER:-$ROOT/scripts/memoire-capture-opencode.sh}"
FIXTURE_SQL="$ROOT/tests/foreign/fixtures/opencode-v2-census.sql"
V1_FIXTURE_SQL="$ROOT/tests/foreign/fixtures/opencode-census.sql"
ONLY="${1:-}"

fail() { echo "FAIL: $1" >&2; exit 1; }
command -v jq >/dev/null || fail "jq not available"
command -v sqlite3 >/dev/null || fail "sqlite3 not available"
command -v go >/dev/null || fail "go not available (the suite needs the real queue binary)"
[[ -x "$ADAPTER" ]] || fail "adapter not executable: $ADAPTER"
[[ -r "$FIXTURE_SQL" ]] || fail "fixture missing: $FIXTURE_SQL"
case "${MEMOIRE_CAPTURE:-}" in off|0|false) fail "MEMOIRE_CAPTURE=$MEMOIRE_CAPTURE in this shell would kill the adapter";; esac

SCRATCH=""
for base in "${TMPDIR:-}" /var/tmp /tmp; do
  [[ -n "$base" && -d "$base" && -w "$base" ]] || continue
  SCRATCH="$(mktemp -d "$base/opencode-v2-capture.XXXXXX" 2>/dev/null)" && break
  SCRATCH=""
done
[[ -n "$SCRATCH" ]] || fail "cannot create a scratch directory"
cleanup() { rm -rf "${SCRATCH:?}"; }
trap cleanup EXIT
mkdir -p "$SCRATCH/home" "$SCRATCH/tmp" "$SCRATCH/bin" "$SCRATCH/tool-output"
SCRATCH="$(realpath "$SCRATCH")"
TOOLDIR="$SCRATCH/tool-output"

QBIN="$SCRATCH/bin/memoire-queue"
(cd "$ROOT/services/api/cmd/queue" && go build -o "$QBIN" .) >"$SCRATCH/build.log" 2>&1 \
  || { cat "$SCRATCH/build.log" >&2; fail "go build of memoire-queue failed"; }

# The census store, with the spill placeholder replaced; one scratch copy per test.
CENSUS_DB="$SCRATCH/census.db"
sed "s|__TOOL_OUT__|$TOOLDIR|g" "$FIXTURE_SQL" | sqlite3 "$CENSUS_DB" || fail "cannot load the v2 fixture"
printf 'spill line 1 é日本\nspill line 2\n' >"$TOOLDIR/tool_ok"
copy_db() { local d="$SCRATCH/$1.db"; rm -f "$d"; cp "$CENSUS_DB" "$d"; printf '%s' "$d"; }

MH=""; QDIR=""; CURS=""; OCENV=()
new_home() { MH="$SCRATCH/mh-$1"; rm -rf "$MH"; mkdir -p "$MH"; QDIR="$MH/queue/pending"; CURS="$MH/capture-cursors/opencode"; OCENV=(); }
OC_PREFIX=()   # e.g. (setsid): the adapter in its own process group, for the signal tests
oc() { # oc <db> [adapter args...]
  local db="$1"; shift
  ${OC_PREFIX[@]+"${OC_PREFIX[@]}"} env HOME="$SCRATCH/home" MEMOIRE_HOME="$MH" TMPDIR="$SCRATCH/tmp" \
    MEMOIRE_OPENCODE_DB="$db" MEMOIRE_OPENCODE_DB_FALLBACK="$SCRATCH/no-such-archive.db" \
    MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR="$TOOLDIR" MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR_FALLBACK="$SCRATCH/no-such-tool-fallback" \
    MEMOIRE_PROJECTS_REGISTRY="$SCRATCH/no-such-registry.json" MEMOIRE_QUEUE_BIN="$QBIN" MEMOIRE_REDACT=0 \
    MEMOIRE_RESOLVE_PROJECT="$ROOT/scripts/memoire-resolve-project.sh" \
    ${OCENV[@]+"${OCENV[@]}"} "$ADAPTER" "$@"
}
qfiles() { [[ -d "$QDIR" ]] && find "$QDIR" -maxdepth 1 -name '*.json' -print; }
qall() { qfiles | xargs -r cat; }
qcount() { qfiles | wc -l; }
qids() { qall | jq -r '.source_event_id' | LC_ALL=C sort; }
qevent() { qall | jq -c --arg id "$1" 'select(.source_event_id == $id)' | head -1; }
qtypes() { qall | jq -r '.event_type' | sort | uniq -c | awk '{print $2"="$1}' | sort; }
pass() { echo "   $1 ok${2:+ ($2)}"; }
run_test() { [[ -z "$ONLY" || "$ONLY" == "all" || "$ONLY" == "$1" ]]; }
summary_of() { grep -o 'done: .*' "$1" | head -1; }

# =============================================================================
if run_test V1; then
echo "== V1 census on the synthetic v2 store"
new_home v1
DB="$(copy_db v1)"
oc "$DB" --full >/dev/null 2>"$SCRATCH/v1.err" || fail "V1: census run failed: $(tail -3 "$SCRATCH/v1.err")"
grep -q 'schema=v2' "$SCRATCH/v1.err" || fail "V1: the summary must say schema=v2"
declare -A CEXP=( [opencode_session_facts]=2 [opencode_todo]=1 [opencode_permission]=2 [opencode_message]=18
  [opencode_user_prompt]=2 [opencode_file]=1 [opencode_item]=3 [opencode_reasoning]=2 [opencode_assistant_message]=7
  [opencode_tool_call]=5 [opencode_tool_result]=4 [opencode_tool_failure]=1 [opencode_compaction]=1 [opencode_assistant_turn]=5 )
declare -A CGOT=()
while read -r t; do CGOT[$t]=$(( ${CGOT[$t]:-0} + 1 )); done < <(qall | jq -r '.event_type')
for t in "${!CEXP[@]}"; do [[ "${CGOT[$t]:-0}" == "${CEXP[$t]}" ]] || fail "V1: $t events=${CGOT[$t]:-0}, fixture says ${CEXP[$t]}"; done
for t in "${!CGOT[@]}"; do [[ -n "${CEXP[$t]:-}" ]] || fail "V1: unexpected event type $t x${CGOT[$t]}"; done
[[ "$(qcount)" == "54" ]] || fail "V1: $(qcount) events, fixture says 54"
# every text of the store reaches exactly one event (the prompt, the items, the summary)
for s in 'Hola é日本 😀 — run ls' 'Thinking in français…' 'fichier-un\nfichier-deux 日本' 'Two files — voilà 😀.' 'Continue from where you left — synthétique' \
         'no such file — introuvable' 'The file is missing — introuvable.' 'Still streaming — en cours' 'first lines only — tronqué' 'Only thinking — réflexion' \
         'résumé 日本' 'Second prompt — deuxième' 'Only this — seulement ceci 😀' 'inconnu é' 'Instructions updated — règles é' 'Edited after the migration — réémis 日本' 'QUJDREVG'; do
  n="$(qall | jq -c 'del(.text)' | grep -c -F -- "$(printf '%b' "$s" | jq -Rs . | sed 's/^"//; s/"$//')")"
  [[ "$n" == "1" ]] || fail "V1: the text '$s' is in $n events' payloads (must be exactly one)"
done
# the turn text rides the text column, not the payload
[[ "$(qevent opencode:turn:msg_v02a2 | jq -r '.text')" == "Two files — voilà 😀." ]] || fail "V1: the turn text of msg_v02a2"
[[ "$(qevent opencode:turn:msg_v02a13 | jq -r '.text')" == "Only this — seulement ceci 😀" ]] || fail "V1: the turn skips the empty text item"
qevent opencode:turn:msg_v02a5 | grep -q . && fail "V1: a message not completed has no turn"
qevent opencode:turn:msg_v02a6 | grep -q . && fail "V1: a message with a running tool has no turn"
qevent opencode:turn:msg_v02a9 | grep -q . && fail "V1: a message without text has no turn"
# never-read tables and the refused session
qall | grep -q 'CANARY' && fail "V1: a value from a never-read table or the refused session surfaced"
grep -q "refusing a session whose id" "$SCRATCH/v1.err" || fail "V1: the bad session id must be refused with a log line"
# the fixture's project_directory hint rides the facts; the schema rides every payload
qevent 'opencode:session:ses_v02:facts:u0001700001020000' | jq -e '.payload.project_directories[0].directory == "/tmp/oc-v2-census/alpha-extra" and .payload.schema == "v2"' >/dev/null || fail "V1: session facts of ses_v02"
qall | jq -e 'select(.payload.schema != "v2")' | grep -q . && fail "V1: every payload carries schema: v2"
# todo and permission as in v1
qevent 'opencode:session:ses_v02:todo:u0001700001001000n2' | jq -e '.payload.count == 2 and .payload.todos[1].content == "read it back 日本"' >/dev/null || fail "V1: the todo snapshot"
qevent 'opencode:permission:perm_v1:ses_v02:u0001700000001000' | jq -e '.payload.permission.resource == "ls é"' >/dev/null || fail "V1: the permission of ses_v02"
pass V1 "54 events, every text once"
fi

# =============================================================================
if run_test V2; then
echo "== V2 the migration floor"
new_home v2
DB="$(copy_db v2)"
oc "$DB" --full >/dev/null 2>"$SCRATCH/v2.err" || fail "V2: run failed"
qids | grep -q 'opencode:message:msg_v01u:' && fail "V2: a migrated row at or below the floor must not be emitted (msg_v01u)"
qids | grep -q 'opencode:message:msg_v01a:' && fail "V2: a migrated row AT the floor must not be emitted (msg_v01a)"
qids | grep -q 'opencode:message:msg_v01i:' && fail "V2: msg_v01i"
qevent 'opencode:message:msg_v01b:u0001700000105000' | jq -e '.payload.message_type == "assistant" and .payload.message.content_items == 1 and (.payload.message | has("content") | not)' >/dev/null || fail "V2: the row edited after the migration must be emitted under the v2 mapping"
qevent opencode:turn:msg_v01b | jq -e '.text == "Edited after the migration — réémis 日本"' >/dev/null || fail "V2: its turn"
new_home v2b; OCENV=(MEMOIRE_OPENCODE_V2_FLOOR_MS=0)
oc "$DB" --full >/dev/null 2>&1 || fail "V2: floor-0 run failed"
for m in msg_v01u msg_v01a msg_v01i msg_v01b; do qids | grep -q "opencode:message:$m:" || fail "V2: with the floor at 0 every migrated row is read ($m)"; done
[[ "$(qcount)" == "60" ]] || fail "V2: floor 0 must add the three migrated rows' 6 events (got $(qcount), want 60)"
pass V2
fi

# =============================================================================
if run_test V3; then
echo "== V3 a v1 cursor carries over"
new_home v3
DB="$(copy_db v3)"
mkdir -p "$CURS"; chmod 700 "$CURS"
# the cursor the v1 mapping left for ses_v01: position at the frozen message, k=m
jq -n --arg ix "$(printf '%s' msg_v01a | xxd -p | tr -d '\n' | tr 'a-f' 'A-F')" \
  '{v: 3, session_id: "ses_v01", p: {mu: 1700000100000, k: "m", idhex: $ix, facts_mu: 1700000100000, todo: "", inputs_mu: 0, epoch_id: "", perm_mu: 1700000001000, done_sig: "1700000100000.1700000100000.1700000002500.2.2.///"}, f: {}, turns: ["msg_v01a"], updated_at: "2026-10-06T00:00:00+02:00"}' >"$CURS/ses_v01.json"
oc "$DB" --full ses_v01 >/dev/null 2>"$SCRATCH/v3.err" || fail "V3: run failed"
[[ "$(qcount)" == "4" ]] || fail "V3: a migrated session resumes after its v1 position: facts (session changed) + 3 events of the edited row, got $(qcount): $(qids | tr '\n' ' ')"
qevent 'opencode:session:ses_v01:facts:u0001700000105000' | grep -q . || fail "V3: the facts re-emit (session_v2.time_updated moved)"
qids | grep -q 'opencode:message:msg_v01a:' && fail "V3: the frozen row is behind the cursor"
jq -e '.p.k == "s" and .p.schema == "v2" and .p.mu == 1700000105000 and (.turns | index("msg_v01a")) != null and (.turns | index("msg_v01b")) != null' "$CURS/ses_v01.json" >/dev/null || fail "V3: the cursor is now a v2 position and keeps the turns: $(cat "$CURS/ses_v01.json")"
pass V3
fi

# =============================================================================
if run_test V4; then
echo "== V4 an unchanged store is skipped; a changed session is read alone"
new_home v4
DB="$(copy_db v4)"
oc "$DB" --full >/dev/null 2>&1 || fail "V4: first run failed"
n1="$(qcount)"
oc "$DB" --full >/dev/null 2>"$SCRATCH/v4.err" || fail "V4: second run failed"
[[ "$(qcount)" == "$n1" ]] || fail "V4: the second run must queue nothing"
grep -q 'skipped_unchanged=2' "$SCRATCH/v4.err" || fail "V4: both sessions must be skipped unchanged: $(summary_of "$SCRATCH/v4.err")"
sqlite3 "$DB" "INSERT INTO session_message VALUES ('msg_v02u17', 'ses_v02', 'user', 17, 1700001004000, 1700001004000, '{\"text\":\"Third prompt — troisième\",\"files\":[],\"time\":{\"created\":1700001004000}}');"
oc "$DB" --full >/dev/null 2>"$SCRATCH/v4b.err" || fail "V4: third run failed"
grep -q 'sessions=1 ' "$SCRATCH/v4b.err" && grep -q 'skipped_unchanged=1' "$SCRATCH/v4b.err" || fail "V4: the changed session alone is read: $(summary_of "$SCRATCH/v4b.err")"
[[ "$(( $(qcount) - n1 ))" == "2" ]] || fail "V4: the new user row adds its record and prompt events"
pass V4
fi

# =============================================================================
if run_test V5; then
echo "== V5 quiet and stuck gates on v2 rows"
new_home v5
DB="$SCRATCH/v5.db"; rm -f "$DB"; sqlite3 "$CENSUS_DB" .schema | sqlite3 "$DB"
NOW_MS="$(date +%s%3N)"
OLD=$(( NOW_MS - 3 * 3600 * 1000 ))
sess() { sqlite3 "$DB" "INSERT INTO session_v2 (id, project_id, slug, directory, version, time_created, time_updated) VALUES ('$1', 'p', 's', '/tmp/v5', '2.0.22', $OLD, $OLD);"; }
row() { sqlite3 "$DB" "INSERT INTO session_message VALUES ('$1', '$2', '$3', $4, $5, $6, '$7');"; }
RUN10=$(( NOW_MS - 600000 )); RUN2H=$(( NOW_MS - 2 * 3600 * 1000 )); FRESH=$(( NOW_MS - 10000 ))
sess stuckyoung; row m_sy stuckyoung assistant 0 $OLD $RUN10 "{\"content\":[{\"type\":\"tool\",\"id\":\"c\",\"name\":\"shell\",\"state\":{\"status\":\"running\",\"input\":{}},\"time\":{\"created\":$RUN10}}],\"time\":{\"created\":$OLD}}"
sess stuckold;   row m_so stuckold assistant 0 $OLD $RUN2H "{\"content\":[{\"type\":\"tool\",\"id\":\"c\",\"name\":\"shell\",\"state\":{\"status\":\"running\",\"input\":{}},\"time\":{\"created\":$RUN2H}}],\"time\":{\"created\":$OLD}}"
sess busy;       row m_b busy user 0 $OLD $FRESH "{\"text\":\"changed 10 s ago\",\"files\":[],\"time\":{\"created\":$OLD}}"
sess quiet;      row m_q quiet user 0 $OLD $OLD "{\"text\":\"quiet é\",\"files\":[],\"time\":{\"created\":$OLD}}"
oc "$DB" --scan >/dev/null 2>"$SCRATCH/v5.err" || fail "V5: run failed: $(tail -2 "$SCRATCH/v5.err")"
has() { qids | grep -q "opencode:session:$1:facts"; }
has stuckyoung && fail "V5: a session whose newest row has a tool running for 10 min must stay gated"
has stuckold || fail "V5: a tool running for 2 hours does not gate (stuckold must be captured)"
has busy && fail "V5: a row changed 10 s ago gates its session"
has quiet || fail "V5: a quiet session is captured"
qevent "opencode:message:m_so:u$(printf '%016d' "$RUN2H"):c0:outcome" | jq -e '.payload.status == "running"' >/dev/null || fail "V5: a stuck-late import keeps state.status verbatim"
grep -q 'skipped_gated=2' "$SCRATCH/v5.err" || fail "V5: two sessions gated: $(summary_of "$SCRATCH/v5.err")"
pass V5
fi

# =============================================================================
if run_test V6; then
echo "== V6 spill files on v2 tool items"
new_home v6
DB="$(copy_db v6)"
oc "$DB" --full ses_v02 >/dev/null 2>&1 || fail "V6: run failed"
outc() { qevent "opencode:message:$1:u$2:c0:outcome" | jq -c '.payload'; }
jq -e '.spill_attached == true and .spill_bytes == 35 and .spill_output == "spill line 1 é日本\nspill line 2\n" and .truncated_by_harness == null' <<<"$(outc msg_v02a7 0001700001002500)" >/dev/null || fail "V6: the spill file is attached whole: $(outc msg_v02a7 0001700001002500 | cut -c1-300)"
jq -e '.truncated_by_harness == true and .spill_rejected == "missing"' <<<"$(outc msg_v02a8 0001700001002700)" >/dev/null || fail "V6: a missing spill: truncated_by_harness + spill_rejected=missing"
outc2() { qevent "opencode:message:msg_v02a2:u0001700001000900:c1:outcome" | jq -c '.payload'; }
jq -e '.truncated_by_harness == null and .spill_rejected == null and .spill_attached == null and .state_out.content[0].text == "fichier-un\nfichier-deux 日本"' <<<"$(outc2)" >/dev/null || fail "V6: an untruncated output carries no flag and its content whole"
# a spill outside the tool-output directory is refused
sqlite3 "$DB" "UPDATE session_message SET data = replace(data, '$TOOLDIR/tool_gone', '/etc/passwd'), time_updated = 1700001005000 WHERE id = 'msg_v02a8'; UPDATE session_v2 SET time_updated = 1700001005000 WHERE id = 'ses_v02';"
oc "$DB" --full ses_v02 >/dev/null 2>&1 || fail "V6: second run failed"
jq -e '.truncated_by_harness == true and .spill_rejected == "outside_dir"' <<<"$(outc msg_v02a8 0001700001005000)" >/dev/null || fail "V6: a spill outside the tool-output directory is refused"
pass V6
fi

# =============================================================================
if run_test V7; then
echo "== V7 a BLOB row and a non-JSON row: the record event only, no poison"
new_home v7
DB="$(copy_db v7)"
oc "$DB" --full ses_v02 --summary >"$SCRATCH/v7.out" 2>"$SCRATCH/v7.err" || fail "V7: run failed"
IFS=$'\t' read -r _ _ failed _ _ _ blobs _ <"$SCRATCH/v7.out"
[[ "$failed" == "0" && "$blobs" == "1" ]] || fail "V7: failed_sessions=$failed blobs=$blobs (want 0 and 1)"
qevent 'opencode:message:msg_v02b16:u0001700001003900' | jq -e '.payload.message.blob == true and .payload.message.raw_blob == "AP8Q" and .payload.message_type == "user"' >/dev/null || fail "V7: the BLOB row is captured as base64"
qids | grep -q 'opencode:message:msg_v02b16:u0001700001003900:' && fail "V7: a BLOB row has no projection events"
qevent 'opencode:message:msg_v02x15:u0001700001003800' | jq -e '.payload.message.raw == "not json at all — brut"' >/dev/null || fail "V7: the non-JSON row is captured raw"
qids | grep -q 'opencode:message:msg_v02x15:u0001700001003800:' && fail "V7: a non-JSON row has no projection events"
pass V7
fi

# =============================================================================
if run_test V8; then
echo "== V8 an edited row re-emits under a new version; the old stays; no second turn"
new_home v8
DB="$(copy_db v8)"
oc "$DB" --full ses_v02 >/dev/null 2>&1 || fail "V8: first run failed"
# an edit lands with a new time_updated (after every other row) and moves the session's own clock
sqlite3 "$DB" "UPDATE session_message SET data = replace(data, 'Two files — voilà 😀.', 'Two files — corrigé 😀.'), time_updated = 1700001005000 WHERE id = 'msg_v02a2'; UPDATE session_v2 SET time_updated = 1700001005000 WHERE id = 'ses_v02';"
oc "$DB" --full ses_v02 >/dev/null 2>&1 || fail "V8: second run failed"
qevent 'opencode:message:msg_v02a2:u0001700001000900:c2' | jq -e '.payload.text == "Two files — voilà 😀."' >/dev/null || fail "V8: the old version stays"
qevent 'opencode:message:msg_v02a2:u0001700001005000:c2' | jq -e '.payload.text == "Two files — corrigé 😀."' >/dev/null || fail "V8: the new version is emitted"
[[ "$(qall | jq -r 'select(.source_event_id == "opencode:turn:msg_v02a2") | .text' | wc -l)" == "1" ]] || fail "V8: exactly one turn for the edited message"
pass V8
fi

# =============================================================================
if run_test V9; then
echo "== V9 --schema v1 on a v2 store warns; --schema v2 on a v1-only store fails loudly"
new_home v9
DB="$(copy_db v9)"
oc "$DB" --full --schema v1 >/dev/null 2>"$SCRATCH/v9.err" || fail "V9: v1 run on the frozen tables failed: $(tail -2 "$SCRATCH/v9.err")"
grep -q 'schema=v1' "$SCRATCH/v9.err" || fail "V9: schema=v1 in the summary"
grep -q 'session_message holds unmapped' "$SCRATCH/v9.err" || fail "V9: the v1 mode must warn about the unmapped session_message rows"
qids | grep -q 'opencode:part:prt_v01a:' || fail "V9: the v1 mode reads the frozen part rows"
new_home v9b
V1DB="$SCRATCH/v9-v1only.db"; rm -f "$V1DB"
sed "s|__TOOL_OUT__|$TOOLDIR|g" "$V1_FIXTURE_SQL" | sqlite3 "$V1DB" || fail "V9: cannot load the v1 fixture"
sqlite3 "$V1DB" "DROP TABLE session_message;"
oc "$V1DB" --full --schema v2 >/dev/null 2>"$SCRATCH/v9b.err"; rc=$?
[[ "$rc" == "1" ]] || fail "V9: --schema v2 without the v2 tables must exit 1 (got $rc)"
[[ "$(wc -l <"$SCRATCH/v9b.err")" == "1" ]] && grep -q 'schema v2 requested' "$SCRATCH/v9b.err" || fail "V9: one log line: $(cat "$SCRATCH/v9b.err")"
new_home v9c
oc "$V1DB" --full >/dev/null 2>"$SCRATCH/v9c.err" || fail "V9: auto on a v1-only store"
grep -q 'schema=v1' "$SCRATCH/v9c.err" || fail "V9: auto detects v1 when session_v2 is absent"
new_home v9d; OCENV=(MEMOIRE_OPENCODE_SCHEMA=bogus)
oc "$DB" --full ses_v02 >/dev/null 2>"$SCRATCH/v9d.err" || fail "V9: a bogus schema knob falls back to auto"
grep -q "unrecognised schema" "$SCRATCH/v9d.err" && grep -q 'schema=v2' "$SCRATCH/v9d.err" || fail "V9: bogus knob -> one line, auto: $(cat "$SCRATCH/v9d.err")"
pass V9
fi

# =============================================================================
if run_test V10; then
echo "== V10 nothing twice: the record event holds the row minus its content, counts in their place"
new_home v10
DB="$(copy_db v10)"
oc "$DB" --full ses_v02 >/dev/null 2>&1 || fail "V10: run failed"
qevent 'opencode:message:msg_v02u1:u0001700001000200' | jq -e '(.payload.message | has("text") or has("files") | not) and .payload.message.files_count == 1 and .payload.message.time.created == 1700001000200' >/dev/null || fail "V10: the user record event"
qevent 'opencode:message:msg_v02u1:u0001700001000200:text' | jq -e '.payload.text == "Hola é日本 😀 — run ls" and (.payload.files[0] | has("data") | not) and .payload.files[0].data_bytes == 8 and .payload.files[0].name == "pasted.png"' >/dev/null || fail "V10: the prompt event carries the text and the files without their data"
qevent 'opencode:message:msg_v02u1:u0001700001000200:f0' | jq -e '.payload.file.data == "QUJDREVG" and .payload.file.mime == "image/png" and .payload.index == 0' >/dev/null || fail "V10: the file event carries the file whole"
qevent 'opencode:message:msg_v02a2:u0001700001000900' | jq -e '(.payload.message | has("content") | not) and .payload.message.content_items == 3 and .payload.message.finish == "stop" and .payload.message.tokens.input == 10 and .payload.seq == 2' >/dev/null || fail "V10: the assistant record event"
qevent 'opencode:message:msg_v02a2:u0001700001000900:c1' | jq -e '.event_type == "opencode_tool_call" and .payload.tool == "shell" and .payload.callID == "call_v02a2" and .payload.state.input.command == "ls é" and (.payload.part | has("state") | not) and .payload.index == 1' >/dev/null || fail "V10: the tool call event"
qevent 'opencode:message:msg_v02a4:u0001700001001500:c0:outcome' | jq -e '.event_type == "opencode_tool_failure" and .payload.status == "error" and .payload.state_out.error.message == "no such file — introuvable"' >/dev/null || fail "V10: the failed tool outcome"
qevent 'opencode:message:msg_v02c10:u0001700001003100' | jq -e '(.payload.message | has("summary") or has("recent") | not) and .payload.message.status == "completed"' >/dev/null || fail "V10: the compaction record event"
qevent 'opencode:message:msg_v02c10:u0001700001003100:compaction' | jq -e '.payload.part.summary == "## Objective\n- résumé 日本"' >/dev/null || fail "V10: the compaction event carries the summary"
qevent 'opencode:message:msg_v02s0:u0001700001000100:text' | jq -e '.event_type == "opencode_item" and .payload.system == true and .payload.description == "Instructions updated: core"' >/dev/null || fail "V10: the system item"
qevent 'opencode:message:msg_v02y3:u0001700001001000:text' | jq -e '.payload.synthetic == true' >/dev/null || fail "V10: the synthetic item"
qevent 'opencode:message:msg_v02i11:u0001700001003200' | jq -e '.payload.message.outcome == "succeeded"' >/dev/null || fail "V10: the idle record event, whole"
qids | grep -q 'opencode:message:msg_v02i11:u0001700001003200:' && fail "V10: an idle row has no projection"
pass V10
fi

# =============================================================================
if run_test V11; then
echo "== V11 the row is the unit of the spill batch cap"
new_home v11
DB="$SCRATCH/v11.db"; rm -f "$DB"; sqlite3 "$CENSUS_DB" .schema | sqlite3 "$DB"
T=1700002000000
mkdir -p "$TOOLDIR/v11"
for f in a b c d e; do printf 'spill %s é日本 — twelve\n' "$f" >"$TOOLDIR/v11/$f"; done   # 29 bytes each
SZ=$(stat -c %s "$TOOLDIR/v11/a")
sqlite3 "$DB" "INSERT INTO session_v2 (id, project_id, slug, directory, version, time_created, time_updated) VALUES ('s11', 'p', 's', '/tmp/v11', '2.0.22', $T, $((T + 9000)));"
tool() { printf '{"type":"tool","id":"%s","name":"shell","state":{"status":"completed","input":{"command":"cat %s"},"content":[{"type":"text","text":"head — %s"}],"metadata":{"truncated":true,"outputPath":"%s"}},"time":{"created":%s}}' "$1" "$1" "$1" "$2" "$3"; }
# row 1: one spill; row 2: TWO spills whose sum exceeds the cap on its own; row 3: one spill; row 4: a plain text row
sqlite3 "$DB" "INSERT INTO session_message VALUES ('m11a', 's11', 'assistant', 0, $((T + 1000)), $((T + 1000)), '{\"content\":[$(tool ca "$TOOLDIR/v11/a" $((T + 1000)))],\"time\":{\"created\":$((T + 1000)),\"completed\":$((T + 1000))}}');"
sqlite3 "$DB" "INSERT INTO session_message VALUES ('m11b', 's11', 'assistant', 1, $((T + 2000)), $((T + 2000)), '{\"content\":[$(tool cb "$TOOLDIR/v11/b" $((T + 2000))),{\"type\":\"text\",\"text\":\"between — entre\"},$(tool cc "$TOOLDIR/v11/c" $((T + 2000)))],\"time\":{\"created\":$((T + 2000)),\"completed\":$((T + 2000))}}');"
sqlite3 "$DB" "INSERT INTO session_message VALUES ('m11c', 's11', 'assistant', 2, $((T + 3000)), $((T + 3000)), '{\"content\":[$(tool cd "$TOOLDIR/v11/d" $((T + 3000)))],\"time\":{\"created\":$((T + 3000)),\"completed\":$((T + 3000))}}');"
sqlite3 "$DB" "INSERT INTO session_message VALUES ('m11d', 's11', 'user', 3, $((T + 4000)), $((T + 4000)), '{\"text\":\"after the spills — après\",\"files\":[],\"time\":{\"created\":$((T + 4000))}}');"
# the cap holds one file and a half: row 1 alone fits, row 2 (two files) must wait for the next batch, then exceeds the cap alone and is read alone
OCENV=(MEMOIRE_OPENCODE_SPILL_BATCH_BYTES=$(( SZ + SZ / 2 )))
oc "$DB" --full s11 --summary >"$SCRATCH/v11.out" 2>"$SCRATCH/v11.err" || fail "V11: run failed: $(tail -2 "$SCRATCH/v11.err")"
IFS=$'\t' read -r _ _ failed _ stopped _ _ _ <"$SCRATCH/v11.out"
[[ "$failed" == "0" && "$stopped" == "0" ]] || fail "V11: the run must complete (failed=$failed stopped=$stopped): $(tail -1 "$SCRATCH/v11.err")"
jq -e '.p.mu == '"$((T + 4000))"' and .p.k == "s"' "$CURS/s11.json" >/dev/null || fail "V11: the cursor must reach the last row: $(jq -c .p "$CURS/s11.json")"
u() { printf '%016d' "$1"; }
for spec in "m11a:$((T + 1000)):c0:a" "m11b:$((T + 2000)):c0:b" "m11b:$((T + 2000)):c2:c" "m11c:$((T + 3000)):c0:d"; do
  IFS=: read -r mid mu ci f <<<"$spec"
  qevent "opencode:message:$mid:u$(u "$mu"):$ci:outcome" | jq -e --arg f "$f" '.payload.spill_attached == true and .payload.spill_bytes == '"$SZ"' and (.payload.spill_output | startswith("spill " + $f + " "))' >/dev/null \
    || fail "V11: the spill of $mid $ci must be attached whole: $(qevent "opencode:message:$mid:u$(u "$mu"):$ci:outcome" | cut -c1-400)"
done
qids | grep -q ':deferred' && fail "V11: the v2 mapping never emits a :deferred marker"
dup="$(qids | uniq -d)"
[[ -z "$dup" ]] || fail "V11: an id was emitted twice: $dup"
bad="$(qall | jq -c '[.source_event_id, (.payload | tojson)]' | sort -u | jq -r '.[0]' | sort | uniq -d)"
[[ -z "$bad" ]] || fail "V11: an id carries two contents: $bad"
[[ "$(qevent 'opencode:turn:m11b' | jq -r .text)" == "between — entre" ]] || fail "V11: the turn of the deferred-then-read row"
# the batches: three queue batches at least (row 1 | row 2 alone | row 3 + row 4), and row 2's events all in one batch
grep -c . "$SCRATCH/v11.err" >/dev/null
# a second run queues nothing
oc "$DB" --full s11 >/dev/null 2>"$SCRATCH/v11b.err" || fail "V11: second run failed"
grep -q 'skipped_unchanged=1' "$SCRATCH/v11b.err" || fail "V11: the session is done after the first run: $(summary_of "$SCRATCH/v11b.err")"
pass V11 "$(qcount) events, 4 spills attached whole, cursor at the end"
fi

# =============================================================================
if run_test V12; then
echo "== V12 a fallback to an archive copy of another schema is refused"
new_home v12
PRIMARY="$(copy_db v12-primary)"
ARCHIVE="$SCRATCH/v12-archive.db"; rm -f "$ARCHIVE"
sed "s|__TOOL_OUT__|$TOOLDIR|g" "$V1_FIXTURE_SQL" | sqlite3 "$ARCHIVE" || fail "V12: cannot load the v1 archive fixture"
# a sqlite3 shim: the primary is locked from its 4th statement on (after the probe, the tables and the columns)
mkdir -p "$SCRATCH/v12bin"
cat >"$SCRATCH/v12bin/sqlite3" <<SHIM
#!/usr/bin/env bash
if [[ "\$*" == *"$PRIMARY"* ]]; then
  n=\$(( \$(cat "$SCRATCH/v12.count" 2>/dev/null || echo 0) + 1 )); echo "\$n" >"$SCRATCH/v12.count"
  if (( n > 3 )); then echo "Error: database is locked" >&2; exit 5; fi
fi
exec "$(command -v sqlite3)" "\$@"
SHIM
chmod +x "$SCRATCH/v12bin/sqlite3"
OCENV=(MEMOIRE_OPENCODE_DB_FALLBACK="$ARCHIVE" MEMOIRE_OPENCODE_SQLITE3="$SCRATCH/v12bin/sqlite3")
oc "$PRIMARY" --full --summary >"$SCRATCH/v12.out" 2>"$SCRATCH/v12.err"; rc=$?
[[ "$rc" == "1" ]] || fail "V12: a refused fallback exits 1 (got $rc): $(cat "$SCRATCH/v12.err")"
grep -q 'falling back to the archive copy' "$SCRATCH/v12.err" || fail "V12: the fallback must have been attempted: $(cat "$SCRATCH/v12.err")"
grep -q 'has schema v1 but the primary had v2; refusing the fallback' "$SCRATCH/v12.err" || fail "V12: the refusal line: $(cat "$SCRATCH/v12.err")"
grep -q 'no such table' "$SCRATCH/v12.err" && fail "V12: no statement may run against the wrong schema"
[[ "$(qcount)" == "0" ]] || fail "V12: nothing is queued"
[[ "$(grep -c . "$SCRATCH/v12.err")" == "2" ]] || fail "V12: two log lines (the fallback, the refusal): $(cat "$SCRATCH/v12.err")"
IFS=$'\t' read -r _ _ failed _ _ _ _ src <"$SCRATCH/v12.out"
[[ "$failed" == "1" && "$src" == "archive" ]] || fail "V12: the summary says failed=1 source=archive (got failed=$failed source=$src)"
pass V12
fi

# =============================================================================
if run_test V13; then
echo "== V13 a fallback during schema discovery restarts it on the archive copy: v2 archive completes, v1 archive refused or read as v1"
V1ARCH="$SCRATCH/v13-v1-archive.db"; rm -f "$V1ARCH"
sed "s|__TOOL_OUT__|$TOOLDIR|g" "$V1_FIXTURE_SQL" | sqlite3 "$V1ARCH" || fail "V13: cannot load the v1 archive fixture"
# a sqlite3 shim: the primary is locked from statement V13_LOCK_AT on (1 = the probe, 2 = the tables, 3 = the columns)
mkdir -p "$SCRATCH/v13bin"
cat >"$SCRATCH/v13bin/sqlite3" <<'SHIM'
#!/usr/bin/env bash
if [[ "$*" == *"$V13_PRIMARY"* ]]; then
  n=$(( $(cat "$V13_COUNT" 2>/dev/null || echo 0) + 1 )); echo "$n" >"$V13_COUNT"
  if (( n >= V13_LOCK_AT )); then echo "Error: database is locked" >&2; exit 5; fi
elif [[ "$*" == *"file:"* ]]; then   # a statement against the archive (the version and capability probes open no store)
  printf '%s\n@@STMT@@\n' "${!#}" >>"$V13_ARCHIVE_LOG"
fi
exec "$(command -v sqlite3)" "$@"
SHIM
chmod +x "$SCRATCH/v13bin/sqlite3"
v13_run() { # lock_at archive  -> runs the adapter on a fresh home and a fresh primary copy
  new_home "v13-$1-$(basename "$2" .db)"
  PRIMARY="$(copy_db "v13-primary-$1")"
  rm -f "$SCRATCH/v13.count"; : >"$SCRATCH/v13.archive.log"
  OCENV=(MEMOIRE_OPENCODE_DB_FALLBACK="$2" MEMOIRE_OPENCODE_SQLITE3="$SCRATCH/v13bin/sqlite3" V13_PRIMARY="$PRIMARY" V13_COUNT="$SCRATCH/v13.count" V13_LOCK_AT="$1" V13_ARCHIVE_LOG="$SCRATCH/v13.archive.log")
  oc "$PRIMARY" --full --summary >"$SCRATCH/v13.out" 2>"$SCRATCH/v13.err"; V13RC=$?
  grep -q 'falling back to the archive copy' "$SCRATCH/v13.err" || fail "V13($1,$(basename "$2")): the fallback must have been attempted: $(cat "$SCRATCH/v13.err")"
  grep -q 'no such table' "$SCRATCH/v13.err" && fail "V13($1,$(basename "$2")): a statement ran against the wrong schema: $(cat "$SCRATCH/v13.err")"
  IFS=$'\t' read -r _ _ V13FAILED _ _ _ _ V13SRC <"$SCRATCH/v13.out"
  [[ "$V13SRC" == "archive" ]] || fail "V13($1,$(basename "$2")): the summary says source=archive (got $V13SRC)"
  [[ "$(cat "$SCRATCH/v13.count")" -ge "$1" ]] || fail "V13($1): the lock was never hit"
}
V2ARCH="$(copy_db v13-v2-archive)"
for at in 2 3; do
  # a v2 archive behind the locked v2 primary: the whole census, from the archive
  v13_run "$at" "$V2ARCH"
  [[ "$V13RC" == "0" ]] || fail "V13($at,v2 archive): exit 0 expected (got $V13RC): $(cat "$SCRATCH/v13.err")"
  [[ "$(qcount)" == "54" ]] || fail "V13($at,v2 archive): 54 events expected from the archive, got $(qcount)"
  qall | jq -e 'select(.payload.schema != "v2")' | grep -q . && fail "V13($at,v2 archive): every payload carries schema v2"
  [[ "$V13FAILED" == "0" ]] || fail "V13($at,v2 archive): failed=$V13FAILED"
  [[ "$(grep -c . "$SCRATCH/v13.err")" == "3" ]] || fail "V13($at,v2 archive): three log lines (the fallback, the refused session id, the summary): $(cat "$SCRATCH/v13.err")"
done
# a v1 archive behind the locked v2 primary, lock at the columns read: the primary's schema (v2) was known -> refused
v13_run 3 "$V1ARCH"
[[ "$V13RC" == "1" ]] || fail "V13(3,v1 archive): a refused fallback exits 1 (got $V13RC): $(cat "$SCRATCH/v13.err")"
grep -q 'has schema v1 but the primary had v2; refusing the fallback' "$SCRATCH/v13.err" || fail "V13(3,v1 archive): the refusal line: $(cat "$SCRATCH/v13.err")"
[[ "$(qcount)" == "0" ]] || fail "V13(3,v1 archive): nothing is queued"
[[ "$(grep -c . "$SCRATCH/v13.err")" == "2" ]] || fail "V13(3,v1 archive): two log lines (the fallback, the refusal): $(cat "$SCRATCH/v13.err")"
[[ "$V13FAILED" == "1" ]] || fail "V13(3,v1 archive): failed=1 expected (got $V13FAILED)"
# the failed v2 column statement was NOT retried on the v1 archive: the archive saw only schema-free discovery statements
grep -q "pragma_table_info('session_v2')" "$SCRATCH/v13.archive.log" && fail "V13(3,v1 archive): the failed v2 column statement was retried on the v1 archive"
[[ "$(grep -c '^@@STMT@@$' "$SCRATCH/v13.archive.log")" == "2" ]] || fail "V13(3,v1 archive): the archive receives exactly the restarted discovery (tables, columns), saw $(grep -c '^@@STMT@@$' "$SCRATCH/v13.archive.log") statement(s)"
# lock at the tables read: nothing of the primary's schema was read; the archive is the source and is read as v1
v13_run 2 "$V1ARCH"
[[ "$V13RC" == "0" ]] || fail "V13(2,v1 archive): exit 0 expected (got $V13RC): $(cat "$SCRATCH/v13.err")"
grep -q 'refusing the fallback' "$SCRATCH/v13.err" && fail "V13(2,v1 archive): no refusal when the primary's schema was never read"
(( $(qcount) > 0 )) || fail "V13(2,v1 archive): the v1 archive emits its census"
qall | jq -e 'select(.payload.schema != "v1")' | grep -q . && fail "V13(2,v1 archive): every payload carries schema v1"
pass V13 "discovery restarted at the tables read and at the columns read without retrying the failed statement; v2 archive: 54 events; v1 archive: refused (schema known) or read as v1 (schema unknown)"
fi

# =============================================================================
if run_test V14; then
echo "== V14 no statement of a v2 run scans session_message, message or part whole; the selection walks the sessions in pages"
new_home v14
DB="$(copy_db v14)"
V14_NOW="$(date +%s%3N)"; T0=$(( V14_NOW - 3 * 3600 * 1000 )); OLD=$(( V14_NOW - 30 * 24 * 3600 * 1000 ))
{
  echo "BEGIN;"
  for i in $(seq 0 129); do
    id="$(printf 'ses_p%03d' "$i")"
    if (( i % 5 == 0 )); then t=$T0; else t=$OLD; fi
    echo "INSERT INTO session_v2 VALUES ('$id', 'prj_v', NULL, NULL, NULL, NULL, 'plan-$i', '/tmp/oc-v2-census/plan', NULL, 'Plan $i', '2.0.22', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, 'build', '{\"id\":\"coder\",\"providerID\":\"conductor\"}', $t, $t, NULL, NULL, NULL, 0, $t, NULL, 'succeeded');"
    echo "INSERT INTO session_message VALUES ('msg_${id}u', '$id', 'user', 0, $t, $t, '{\"text\":\"plan é $i\",\"files\":[],\"time\":{\"created\":$t}}');"
    echo "INSERT INTO session_message VALUES ('msg_${id}a', '$id', 'assistant', 1, $((t + 1)), $((t + 2)), '{\"agent\":\"build\",\"model\":{\"id\":\"coder\",\"providerID\":\"conductor\"},\"content\":[{\"type\":\"text\",\"text\":\"réponse $i\"}],\"finish\":\"stop\",\"time\":{\"created\":$((t + 1)),\"completed\":$((t + 2))}}');"
    # every other session was migrated: a frozen v1 row below the v2 rows gives it a floor
    if (( i % 2 == 0 )); then echo "INSERT INTO message VALUES ('msg_${id}m', '$id', $((t - 1000)), $((t - 1000)), '{\"role\":\"user\",\"time\":{\"created\":$((t - 1000))}}');"; fi
  done
  echo "COMMIT;"
} | sqlite3 "$DB" || fail "V14: cannot build the 134-session store"
mkdir -p "$SCRATCH/v14bin"
cat >"$SCRATCH/v14bin/sqlite3" <<'SHIM'
#!/usr/bin/env bash
printf '%s\n@@STMT@@\n' "${!#}" >>"$V14_SQL_LOG"
exec "$(command -v sqlite3)" "$@"
SHIM
chmod +x "$SCRATCH/v14bin/sqlite3"
: >"$SCRATCH/v14.sql.log"
OCENV=(MEMOIRE_OPENCODE_SQLITE3="$SCRATCH/v14bin/sqlite3" V14_SQL_LOG="$SCRATCH/v14.sql.log")
oc "$DB" --scan --summary >"$SCRATCH/v14.out" 2>"$SCRATCH/v14.err" || fail "V14: run failed: $(tail -2 "$SCRATCH/v14.err")"
grep -q 'schema=v2' "$SCRATCH/v14.err" || fail "V14: the summary must say schema=v2"
grep -q 'candidates=26 ' "$SCRATCH/v14.err" || fail "V14: 26 of the 134 sessions are inside the window (got: $(grep -o 'candidates=[0-9]*' "$SCRATCH/v14.err"))"
mkdir -p "$SCRATCH/v14stmts"; rm -f "$SCRATCH/v14stmts"/*
awk -v d="$SCRATCH/v14stmts" 'BEGIN {n = 0; f = d "/s0"} /^@@STMT@@$/ {close(f); n++; f = d "/s" n; next} {print >> f}' "$SCRATCH/v14.sql.log"
nplan=0; nfull=0; pages=0; nmsg=0; nfloor=0
for f in "$SCRATCH"/v14stmts/s*; do
  [[ -s "$f" ]] || continue
  sed -i "/^SELECT '@@END@@';\$/d" "$f"
  grep -q '^--version' "$f" && continue
  grep -q 'octet_length' "$f" && grep -q 'group_concat(a' "$f" && continue
  plan="$(sqlite3 -safe "file:$DB?immutable=1" "EXPLAIN QUERY PLAN $(cat "$f")" 2>&1)" || { echo "$plan" >&2; fail "V14: cannot explain a statement of the run: $(head -c 200 "$f")"; }
  nplan=$((nplan + 1))
  grep -q 'session_message' "$f" && nmsg=$((nmsg + 1))
  if grep -qE 'SCAN (session_message|message|part|m|p)( |$)' <<<"$plan"; then
    nfull=$((nfull + 1)); echo "   full scan in: $(head -c 160 "$f" | tr '\n' ' ')" >&2; grep -E 'SCAN (session_message|message|part|m|p)( |$)' <<<"$plan" >&2
  fi
  # SQLite says SEARCH for an unindexed aggregate too: a SEARCH of these tables must name an index or the rowid
  if grep -E 'SEARCH (session_message|message|part|m|p)( |$)' <<<"$plan" | grep -qvE 'USING (COVERING )?INDEX|USING INTEGER PRIMARY KEY'; then
    nfull=$((nfull + 1)); echo "   unindexed search in: $(head -c 160 "$f" | tr '\n' ' ')" >&2; grep -E 'SEARCH (session_message|message|part|m|p)( |$)' <<<"$plan" >&2
  fi
  # the migration floor: max(time_updated) of the session's rows in the frozen message table, one range of its session_id index
  if grep -q 'max(time_updated) FROM message' "$f"; then
    nfloor=$((nfloor + 1))
    grep -qE 'SEARCH message USING (COVERING )?INDEX message_session_time_created_id_idx \(session_id=\?\)' <<<"$plan" || { echo "$plan" >&2; fail "V14: a migration floor read is not a session_id index range of message"; }
  fi
  if grep -q 'WITH b AS MATERIALIZED' "$f"; then
    pages=$((pages + 1))
    grep -qE 'SEARCH s USING INTEGER PRIMARY KEY \(rowid>\?\)' <<<"$plan" || { echo "$plan" >&2; fail "V14: a selection page does not walk the session table by a rowid range"; }
    grep -qE 'SCAN s( |$)' <<<"$plan" && { echo "$plan" >&2; fail "V14: a selection page scans the session table whole"; }
  fi
done
[[ "$nfull" == "0" ]] || fail "V14: $nfull statement(s) read session_message, message or part whole"
(( nfloor >= 1 )) || fail "V14: no migration floor read was exercised (the generated sessions carry frozen v1 rows)"
[[ "$pages" == "3" ]] || fail "V14: 134 sessions must be walked in 3 pages of 50, saw $pages selection statement(s)"
(( nplan >= 8 )) || fail "V14: too few statements were checked ($nplan)"
(( nmsg >= 3 )) || fail "V14: too few statements read session_message ($nmsg): the v2 route was not exercised"
pass V14 "$nplan statements explained ($nmsg over session_message, $nfloor floor reads by index), 0 whole-table reads, 3 selection pages for 134 sessions, each a rowid-range SEARCH"
fi

# =============================================================================
if run_test V15; then
echo "== V15 a group signal during initialization is a clean stop: at the initial discovery, and during the schema refresh after a fallback"
# a sqlite3 shim: busy on the primary from statement V15_BUSY_AT (0 = never); at statement V15_KILL_AT (counted over both
# sources) it signals the adapter's whole process group (the adapter runs under setsid) and dies as the signal would kill it
mkdir -p "$SCRATCH/v15bin"
cat >"$SCRATCH/v15bin/sqlite3" <<'SHIM'
#!/usr/bin/env bash
[[ "$*" == *"file:"* ]] || exec "$(command -v sqlite3)" "$@"   # the version and capability probes open no store: not counted
n=$(( $(cat "$V15_COUNT" 2>/dev/null || echo 0) + 1 )); echo "$n" >"$V15_COUNT"
# V15_DATE_AT: the signal lands on the ENDING TIMER of this statement instead (the date shim is armed; the statement runs normally)
if (( V15_DATE_AT > 0 && n == V15_DATE_AT )); then : >"$V15_DATE_ARM"; exec "$(command -v sqlite3)" "$@"; fi
# the adapter is the session leader (setsid): its process group id is the session id (`timeout` may put sqlite3 in a group of its own)
if (( n == V15_KILL_AT )); then kill -TERM -- -"$(ps -o sid= -p $$ | tr -d ' ')"; sleep 1; exit 143; fi
if (( V15_BUSY_AT > 0 )) && [[ "$*" == *"$V15_PRIMARY"* ]] && (( n >= V15_BUSY_AT )); then echo "Error: database is locked" >&2; exit 5; fi
exec "$(command -v sqlite3)" "$@"
SHIM
chmod +x "$SCRATCH/v15bin/sqlite3"
# a date shim on PATH: once armed, the next timestamp read prints a REAL timestamp, then signals the adapter's session group and dies
mkdir -p "$SCRATCH/v15path"
cat >"$SCRATCH/v15path/date" <<'SHIM'
#!/usr/bin/env bash
if [[ "$1" == "+%s%3N" && -e "$V15_DATE_ARM" ]]; then
  rm -f "$V15_DATE_ARM"; /usr/bin/date "$@"; kill -TERM -- -"$(ps -o sid= -p $$ | tr -d ' ')"; sleep 1; exit 143
fi
exec /usr/bin/date "$@"
SHIM
chmod +x "$SCRATCH/v15path/date"
V15_DATE_AT=0
v15_run() { # busy_at kill_at   (V15_DATE_AT > 0: kill_at is ignored, the signal lands on the ending timer of statement V15_DATE_AT)
  new_home "v15-$1-$2"
  PRIMARY="$(copy_db "v15-primary-$1-$2")"; ARCH="$(copy_db "v15-archive-$1-$2")"
  rm -f "$SCRATCH/v15.count" "$SCRATCH/v15.arm"
  OCENV=(MEMOIRE_OPENCODE_DB_FALLBACK="$ARCH" MEMOIRE_OPENCODE_SQLITE3="$SCRATCH/v15bin/sqlite3" V15_PRIMARY="$PRIMARY" V15_COUNT="$SCRATCH/v15.count" V15_BUSY_AT="$1" V15_KILL_AT="$2" V15_DATE_AT="$V15_DATE_AT" V15_DATE_ARM="$SCRATCH/v15.arm" PATH="$SCRATCH/v15path:$PATH")
  OC_PREFIX=(setsid)
  oc "$PRIMARY" --full --summary >"$SCRATCH/v15.out" 2>"$SCRATCH/v15.err"; V15RC=$?
  OC_PREFIX=()
  local last="$2"; (( V15_DATE_AT > 0 )) && last="$V15_DATE_AT"
  [[ "$(cat "$SCRATCH/v15.count")" == "$last" ]] || fail "V15($1,$2,date=$V15_DATE_AT): the signal must have been the last statement (count $(cat "$SCRATCH/v15.count"))"
  (( V15_DATE_AT > 0 )) && [[ -e "$SCRATCH/v15.arm" ]] && fail "V15($1,$2,date=$V15_DATE_AT): the date shim never fired"
  [[ "$V15RC" == "0" ]] || fail "V15($1,$2): a group signal is a clean stop, exit 0 (got $V15RC): $(cat "$SCRATCH/v15.err")"
  grep -q 'stopped cleanly on signal' "$SCRATCH/v15.err" || fail "V15($1,$2): the clean-stop line is missing: $(cat "$SCRATCH/v15.err")"
  grep -q -E 'refusing|cannot read|read failed|query failed|internal error' "$SCRATCH/v15.err" && fail "V15($1,$2): a stop is not a failure: $(cat "$SCRATCH/v15.err")"
  IFS=$'\t' read -r _ _ V15FAILED _ V15STOPPED _ _ V15SRC <"$SCRATCH/v15.out"
  [[ "$V15FAILED" == "0" && "$V15STOPPED" == "1" ]] || fail "V15($1,$2): summary failed=0 stopped=1 expected (got failed=$V15FAILED stopped=$V15STOPPED)"
  [[ "$(qcount)" == "0" ]] || fail "V15($1,$2): nothing is queued"
}
# the signal at the initial tables read (statement 2), no fallback: one line
v15_run 0 2
[[ "$(grep -c . "$SCRATCH/v15.err")" == "1" ]] || fail "V15(0,2): exactly one log line: $(cat "$SCRATCH/v15.err")"
[[ "$V15SRC" == "primary" ]] || fail "V15(0,2): source=primary (got $V15SRC)"
# the primary goes busy at its 4th statement (the selection); the schema refresh on the archive is statement 5: the signal there
v15_run 4 5
grep -q 'falling back to the archive copy' "$SCRATCH/v15.err" || fail "V15(4,5): the fallback must have been taken: $(cat "$SCRATCH/v15.err")"
[[ "$(grep -c . "$SCRATCH/v15.err")" == "2" ]] || fail "V15(4,5): two log lines (the fallback, the stop): $(cat "$SCRATCH/v15.err")"
[[ "$V15SRC" == "archive" ]] || fail "V15(4,5): source=archive (got $V15SRC)"
# the same two points, the signal landing on the ending timer of the read after it printed a valid timestamp
V15_DATE_AT=2; v15_run 0 0
[[ "$(grep -c . "$SCRATCH/v15.err")" == "1" ]] || fail "V15(date=2): exactly one log line: $(cat "$SCRATCH/v15.err")"
V15_DATE_AT=5; v15_run 4 0
grep -q 'falling back to the archive copy' "$SCRATCH/v15.err" || fail "V15(4,date=5): the fallback must have been taken: $(cat "$SCRATCH/v15.err")"
[[ "$(grep -c . "$SCRATCH/v15.err")" == "2" ]] || fail "V15(4,date=5): two log lines (the fallback, the stop): $(cat "$SCRATCH/v15.err")"
V15_DATE_AT=0
pass V15 "a group signal at the initial discovery and during the schema refresh after a fallback, on the statement and on its ending timer: exit 0, the stop line, stopped=1, failed=0"
fi

echo "opencode-v2-capture: all selected tests passed"
