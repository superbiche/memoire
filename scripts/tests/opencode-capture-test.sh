#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# opencode-capture-test.sh — regression test for the opencode capture adapter
# (opencode parity slice, 2026-07-12). Pure --dry-run over a fixture SQLite DB
# built in-test: no API, no queue writes, no provider, never touches the live
# opencode store.
#
# The part.data.type literals below ('text', 'reasoning', 'tool') pin the
# values observed in opencode 1.17.18's drizzle schema. The adapter captures
# ONLY type='text' — if a drizzle migration renames these values, the
# positive-capture assertions fail loudly instead of the adapter silently
# capturing nothing.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$ROOT/scripts/memoire-capture-opencode.sh"

command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }
command -v sqlite3 >/dev/null || { echo "SKIP: sqlite3 not available"; exit 0; }

# Pin the project resolver to a controlled fixture so the basename-fallback
# assertions below are hermetic (never read the operator's live
# ~/.agents/memoire-projects.json). A registry-hit case at the end overrides it.
export MEMOIRE_PROJECTS_REGISTRY="/no/such/registry.json"
unset MEMOIRE_PROJECT

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

DB="$(mktemp --suffix=.db)"
trap 'rm -f "$DB"' EXIT

NOW_MS=$(( $(date +%s) * 1000 ))
# 1767225600000 ms = 2026-01-01T00:00:00Z — pins the occurred_at conversion.
sqlite3 "$DB" <<SQL
CREATE TABLE session (id text PRIMARY KEY, parent_id text, directory text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, data text NOT NULL);
CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL, time_created integer NOT NULL, data text NOT NULL);
INSERT INTO session VALUES ('ses_top', NULL, '/home/user/projfoo', $NOW_MS, $NOW_MS);
INSERT INTO session VALUES ('ses_sub', 'ses_top', '/home/user/projfoo', $NOW_MS, $NOW_MS);
INSERT INTO session VALUES ('ses_old', NULL, '/home/user/projold', 1000000000000, 1000000000000);
INSERT INTO message VALUES ('msg_a1', 'ses_top', 1767225600000, '{"role":"assistant","time":{"created":1767225600000,"completed":1767225600500}}');
INSERT INTO message VALUES ('msg_u1', 'ses_top', 1767225601000, '{"role":"user","time":{"created":1767225601000}}');
INSERT INTO message VALUES ('msg_a2', 'ses_top', 1767225602000, '{"role":"assistant","time":{"created":1767225602000,"completed":1767225602500}}');
INSERT INTO message VALUES ('msg_a3', 'ses_top', 1767225604000, '{"role":"assistant","time":{"created":1767225604000}}');
INSERT INTO message VALUES ('msg_s1', 'ses_sub', 1767225603000, '{"role":"assistant","time":{"created":1767225603000,"completed":1767225603500}}');
INSERT INTO message VALUES ('msg_o1', 'ses_old', 1000000000000, '{"role":"assistant","time":{"created":1000000000000,"completed":1000000000500}}');
INSERT INTO part VALUES ('prt_01', 'msg_a1', 'ses_top', 1767225600000, '{"type":"text","text":"openturnneedle first"}');
INSERT INTO part VALUES ('prt_02', 'msg_a1', 'ses_top', 1767225600001, '{"type":"reasoning","text":"reasoningneedle"}');
INSERT INTO part VALUES ('prt_03', 'msg_a1', 'ses_top', 1767225600002, '{"type":"text","text":"second part tail"}');
INSERT INTO part VALUES ('prt_04', 'msg_a1', 'ses_top', 1767225600003, '{"type":"tool","text":"toolneedle"}');
INSERT INTO part VALUES ('prt_05', 'msg_u1', 'ses_top', 1767225601000, '{"type":"text","text":"userpromptneedle"}');
INSERT INTO part VALUES ('prt_06', 'msg_a2', 'ses_top', 1767225602000, '{"type":"tool","text":"toolonlyneedle"}');
INSERT INTO part VALUES ('prt_07', 'msg_s1', 'ses_sub', 1767225603000, '{"type":"text","text":"subagentneedle"}');
INSERT INTO part VALUES ('prt_08', 'msg_o1', 'ses_old', 1000000000000, '{"type":"text","text":"oldneedle"}');
INSERT INTO part VALUES ('prt_09', 'msg_a3', 'ses_top', 1767225604000, '{"type":"text","text":"activeneedle"}');
SQL

# --- scan mode: only the fresh top-level assistant turn emits -----------------
out="$(OPENCODE_DB="$DB" "$SCRIPT" --scan --since "1 hour ago" --dry-run 2>/dev/null)"
[[ "$(grep -c . <<<"$out")" == 1 ]] || fail "scan must yield exactly 1 turn event"
turn="$(jq -c . <<<"$out")"
[[ "$(jq -r '.text' <<<"$turn")" == $'openturnneedle first\n\nsecond part tail' ]] \
  || fail "text parts must join with blank lines in part-id order"
grep -q "reasoningneedle" <<<"$out" && fail "reasoning part must never emit"
grep -q "toolneedle" <<<"$out" && fail "tool part must never emit"
grep -q "userpromptneedle" <<<"$out" && fail "user-message text part must never emit"
grep -q "subagentneedle" <<<"$out" && fail "subagent (parent_id) session must never emit"
grep -q "oldneedle" <<<"$out" && fail "session older than --since cutoff must never emit"
grep -q "activeneedle" <<<"$out" && fail "in-progress message (no time.completed) must never emit — a partial turn would freeze under its deterministic source id"
[[ "$(jq -r '.source_event_id' <<<"$turn")" == "opencode-turn:ses_top:msg_a1" ]] \
  || fail "source_event_id must be opencode-turn:<session_id>:<message_id>"
[[ "$(jq -r '.event_type' <<<"$turn")" == "opencode_assistant_turn" ]] \
  || fail "event_type must be opencode_assistant_turn"
[[ "$(jq -r '.occurred_at' <<<"$turn")" == "2026-01-01T00:00:00Z" ]] \
  || fail "occurred_at must convert message.data.time.created ms epoch to RFC3339 UTC"
[[ "$(jq -r '.project' <<<"$turn")" == "projfoo" ]] \
  || fail "project must fail-open to basename(session.directory) with no registry match"
jq -e '.payload.backfill_generation | length > 0' <<<"$turn" >/dev/null \
  || fail "payload missing backfill_generation"
jq -e '.payload.text_truncated == false' <<<"$turn" >/dev/null \
  || fail "short turn must not be flagged truncated"

# Determinism: a second scan emits the identical source_event_id.
id2="$(OPENCODE_DB="$DB" "$SCRIPT" --scan --since "1 hour ago" --dry-run 2>/dev/null \
  | jq -r '.source_event_id')"
[[ "$id2" == "opencode-turn:ses_top:msg_a1" ]] || fail "source_event_id not deterministic"

# Truncation: with a tiny cap the text shrinks and the payload says so.
capped="$(MEMOIRE_BACKFILL_TEXT_MAX=14 OPENCODE_DB="$DB" "$SCRIPT" --scan --since "1 hour ago" --dry-run 2>/dev/null)"
[[ "$(jq -r '.text' <<<"$capped")" == "openturnneedle" ]] \
  || fail "turn text not capped at MEMOIRE_BACKFILL_TEXT_MAX"
jq -e '.payload.text_truncated == true' <<<"$capped" >/dev/null \
  || fail "capped turn must be flagged truncated"

# Explicit session-id mode bypasses the time cutoff.
old_out="$(OPENCODE_DB="$DB" "$SCRIPT" --dry-run ses_old 2>/dev/null)"
grep -q "oldneedle" <<<"$old_out" || fail "explicit session id must ingest regardless of age"
[[ "$(grep -c . <<<"$old_out")" == 1 ]] || fail "explicit session id must yield exactly its own turns"

# Value-taking options without an operand must be a fast usage error, not an
# infinite parse loop (a failed `shift 2` under set -u without set -e leaves
# the argument list unchanged).
for opt in --since --db --project; do
  rc=0
  timeout 5 env OPENCODE_DB="$DB" "$SCRIPT" --dry-run "$opt" >/dev/null 2>&1 || rc=$?
  [[ "$rc" == 2 ]] || fail "missing operand for $opt must exit 2 promptly (got $rc)"
done

# A nonsense text cap must fall back to the default, not silently capture
# nothing (invalid --argjson) or corrupt text (negative jq slice).
for cap in not-an-integer 0 -1; do
  cap_out="$(MEMOIRE_BACKFILL_TEXT_MAX="$cap" OPENCODE_DB="$DB" "$SCRIPT" --scan --since "1 hour ago" --dry-run 2>/dev/null)" \
    || fail "cap '$cap' must fall back, not fail the scan"
  [[ "$(jq -r '.text' <<<"$cap_out")" == $'openturnneedle first\n\nsecond part tail' ]] \
    || fail "cap '$cap' must fall back to the default and leave text intact"
  jq -e '.payload.text_truncated == false' <<<"$cap_out" >/dev/null \
    || fail "cap '$cap' fallback must not flag truncation"
done

# --- resolver wiring: a registry match disambiguates over the basename --------
# Proves the adapter runs the shared resolver on session.directory rather than
# taking a bare basename. session.directory is /home/user/projfoo; a registry
# that maps that exact prefix must win over the "projfoo" basename.
REG="$(mktemp --suffix=.json)"
trap 'rm -f "$DB" "$REG"' EXIT
cat > "$REG" <<'JSON'
{
  "schema": "liaison-memoire-projects/1",
  "projects": {
    "client-projfoo": { "display": "projfoo", "paths": ["/home/user/projfoo"] }
  }
}
JSON
mapped="$(MEMOIRE_PROJECTS_REGISTRY="$REG" OPENCODE_DB="$DB" "$SCRIPT" --scan --since "1 hour ago" --dry-run 2>/dev/null)"
[[ "$(jq -r '.project' <<<"$mapped")" == "client-projfoo" ]] \
  || fail "adapter must resolve session.directory through the registry (got '$(jq -r '.project' <<<"$mapped")')"

# --project override still beats the registry.
forced="$(MEMOIRE_PROJECTS_REGISTRY="$REG" OPENCODE_DB="$DB" "$SCRIPT" --scan --since "1 hour ago" --project forced --dry-run 2>/dev/null)"
[[ "$(jq -r '.project' <<<"$forced")" == "forced" ]] \
  || fail "--project override must beat the registry"

echo "opencode-capture-test: OK"
