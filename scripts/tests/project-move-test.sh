#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# project-move-test.sh — regression test for memoire-project-move.sh
# (liaison:D238 project-identity slice + liaison:D239 identity v2, 2026-07-18). Builds a tiny
# fixture DB from the real schema migrations (through 0010), exercises
# --from-path and --plan in dry-run, asserts the apply double-gate refuses,
# then runs ONE double-gated --apply against a throwaway COPY of the fixture
# to assert the identity-table re-key end-state. It NEVER touches the live
# store — everything lives under mktemp.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$ROOT/scripts/memoire-project-move.sh"

command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }
command -v sqlite3 >/dev/null || { echo "SKIP: sqlite3 not available"; exit 0; }

fail() { echo "FAIL: $1" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP" 2>/dev/null || true' EXIT
DB="$TMP/fixture.db"

for m in 0001_initial 0004_derivation 0005_observation_review_log 0006_derivation_model 0007_governance_v1 \
         0008_review_queue_indexes 0009_derivation_run_created_ns 0010_project_identity; do
  sqlite3 "$DB" < "$ROOT/schema/${m}.sql" >/dev/null || fail "schema $m failed to apply"
done

sqlite3 "$DB" <<'SQL'
INSERT INTO sessions(session_id,project,client,started_at_epoch) VALUES
 ('s_pinf','infra','cc',1000),('s_sinf','infra','cc',1001),
 ('s_pherm','app-one','cc',1002),('s_hermie','app-one-alt','cc',1003),
 ('s_sib','infrastructure','cc',1005);
INSERT INTO events(idempotency_key,session_id,project,source_adapter,event_type,cwd,payload_json,occurred_at_epoch,received_at_epoch) VALUES
 ('e1','s_pinf','infra','cc','x','/home/ops/dev/alpha/infra','{}',1000,1000),
 ('e2','s_pinf','infra','cc','x','/home/ops/dev/alpha/infra/servers/srv-one','{}',1000,1000),
 ('e3','s_sinf','infra','cc','x','/home/ops/dev/client-a/infra','{}',1001,1001),
 ('e4','s_pherm','app-one','cc','x','/home/ops/dev/alpha/app-one','{}',1002,1002),
 ('e5','s_hermie','app-one-alt','cc','x','/home/ops/dev/alpha/app-one','{}',1003,1003),
 ('e7','s_sib','infrastructure','cc','x','/home/ops/dev/alpha/infrastructure','{}',1005,1005);
INSERT INTO observations(event_id,project,session_id,kind,text,occurred_at_epoch,created_at_epoch,content_hash) VALUES
 (1,'infra','s_pinf','typed','a',1000,1000,'sha256:1'),
 (4,'app-one','s_pherm','typed','b',1002,1002,'sha256:2'),
 (5,'app-one-alt','s_hermie','typed','c',1003,1003,'sha256:3');
INSERT INTO derivation_runs(id,project,session_id,provider,data_boundary,derivation_version,input_hash,window_start_epoch,window_end_epoch,status,created_at_epoch) VALUES
 ('dr4','app-one','s_pherm','rules','local','1','h4',1002,1002,'done',1002),
 ('dr5','app-one-alt','s_hermie','rules','local','1','h5',1003,1003,'done',1003);
-- Schema 0010 identity rows for the app-one->personal-app-one move:
-- one root that merges cleanly, one root the target ALREADY knows (PK
-- collision -> old row dropped, not duplicated), one stale alert to drop.
INSERT INTO project_roots(project,root,source,first_seen_epoch,last_seen_epoch) VALUES
 ('app-one','/home/ops/dev/alpha/app-one','basename',1002,1002),
 ('app-one','/home/ops/old/app-one','basename',1002,1002),
 ('personal-app-one','/home/ops/old/app-one','registry',900,900);
INSERT INTO project_identity_alerts(project,root,known_root,source,first_seen_epoch,last_seen_epoch,event_count) VALUES
 ('app-one','/tmp/scratch/app-one','/home/ops/dev/alpha/app-one','basename',1002,1002,3);
SQL

REG="$TMP/reg.json"
cat > "$REG" <<'JSON'
{
  "schema": "liaison-memoire-projects/1",
  "projects": {
    "personal-infra":  { "display": "infra",  "paths": ["/home/ops/dev/alpha/infra"], "aliases": [] },
    "client-a-infra": { "display": "infra",  "paths": ["/home/ops/dev/client-a/infra"], "aliases": [] },
    "personal-app-one": { "display": "app-one", "paths": ["/home/ops/dev/alpha/app-one"], "aliases": ["app-one-alt"] }
  }
}
JSON

snapshot() { sqlite3 "$DB" "SELECT project,count(*) FROM events GROUP BY project ORDER BY project;" | tr '\n' ';'; }
BEFORE="$(snapshot)"

# 1. from-path component boundary: /home/.../infra matches infra + its srv-one
#    descendant (2 events) but NOT the sibling /home/.../infrastructure.
out="$(bash "$SCRIPT" --db "$DB" --from-path /home/ops/dev/alpha/infra --to personal-infra)"
grep -q 'events        (cwd component-boundary match) : 2' <<<"$out" \
  || fail "from-path infra: expected 2 boundary-matched events"
grep -q "SET project='personal-infra'" <<<"$out" || fail "from-path: missing events UPDATE"
# infra derivation value is ambiguous (also under client-a) -> refused
grep -q "value 'infra' .* REFUSED" <<<"$out" || fail "from-path: infra derivation should be refused as ambiguous"

# 2. from-path app-one fold: app-one + app-one-alt sessions, both derivation values
#    unambiguous here -> re-keyed together.
out="$(bash "$SCRIPT" --db "$DB" --from-path /home/ops/dev/alpha/app-one --to personal-app-one)"
grep -q 'observations  (via session_id join)          : 2' <<<"$out" || fail "app-one fold: expected 2 observations"
grep -q "UPDATE derivation_runs SET project='personal-app-one' WHERE project IN ('app-one','app-one-alt')" <<<"$out" \
  || fail "app-one fold: expected app-one+app-one-alt derivation re-key"

# 3. plan mode: shared display 'infra' is refused across entries; unique alias
#    'app-one-alt' IS re-keyable by value.
out="$(bash "$SCRIPT" --db "$DB" --plan "$REG")"
grep -q 'NOT re-keyable by value: .*infra' <<<"$out" || fail "plan: infra should be flagged cross-entry ambiguous"
grep -qE "derivation_runs \(by project value[^)]*'app-one-alt'" <<<"$out" || fail "plan: app-one-alt alias should be re-keyable by value"

# 3b. plan mode longest-prefix-wins (review FIX 1 regression): with a nested
#     prefix whose entry id sorts alphabetically BEFORE the broad entry
#     (aa-srv-one-apps < zz-personal-infra), the emitted SQL must still order the
#     broad UPDATE first and the nested UPDATE last, so the specific prefix
#     re-claims its rows instead of being clobbered by the broad one.
NESTREG="$TMP/nested-registry.json"
cat > "$NESTREG" <<'JSON'
{
  "schema": "liaison-memoire-projects/1",
  "projects": {
    "aa-srv-one-apps": { "display": "apps", "paths": ["/home/ops/dev/alpha/infra/servers/srv-one"] },
    "zz-personal-infra": { "display": "infra", "paths": ["/home/ops/dev/alpha/infra"] }
  }
}
JSON
out="$(bash "$SCRIPT" --db "$DB" --plan "$NESTREG")"
broad_line="$(grep -n "UPDATE events SET project='zz-personal-infra'" <<<"$out" | head -1 | cut -d: -f1)"
nested_line="$(grep -n "UPDATE events SET project='aa-srv-one-apps'" <<<"$out" | head -1 | cut -d: -f1)"
[[ -n "$broad_line" && -n "$nested_line" ]] || fail "nested plan: expected both UPDATE previews"
[[ "$broad_line" -lt "$nested_line" ]] \
  || fail "nested plan: broad prefix UPDATE must precede nested prefix UPDATE (longest prefix wins)"

# 4. apply double-gate: --apply without the env var must refuse, exit non-zero,
#    and write nothing.
if bash "$SCRIPT" --db "$DB" --from-path /home/ops/dev/alpha/infra --to personal-infra --apply 2>/dev/null; then
  fail "apply without MEMOIRE_PROJECT_MOVE_APPLY=yes should have failed"
fi

# 5. dry-runs must not have mutated the DB.
[[ "$(snapshot)" == "$BEFORE" ]] || fail "dry-runs mutated the DB (must be read-only)"

# 6. Schema 0010 identity tables ride the value re-key (review 2026-07-18):
#    the app-one fold plan must preview the identity SQL...
out="$(bash "$SCRIPT" --db "$DB" --from-path /home/ops/dev/alpha/app-one --to personal-app-one)"
grep -q "UPDATE OR IGNORE project_roots SET project='personal-app-one'" <<<"$out" \
  || fail "identity plan: expected project_roots merge preview"
grep -q "DELETE FROM project_identity_alerts WHERE project IN" <<<"$out" \
  || fail "identity plan: expected stale alert deletion preview"

#    ...and a gated apply on a throwaway copy must merge roots (PK collision
#    dropped, not duplicated) and drop the old key's alerts.
APPLY_DB="$TMP/apply.db"
cp "$DB" "$APPLY_DB"
MEMOIRE_PROJECT_MOVE_APPLY=yes bash "$SCRIPT" --db "$APPLY_DB" \
  --from-path /home/ops/dev/alpha/app-one --to personal-app-one --apply >/dev/null \
  || fail "gated apply failed"
roots="$(sqlite3 "$APPLY_DB" "SELECT project||'|'||root FROM project_roots ORDER BY root;" | tr '\n' ';')"
[[ "$roots" == "personal-app-one|/home/ops/dev/alpha/app-one;personal-app-one|/home/ops/old/app-one;" ]] \
  || fail "identity apply: roots wrong after move: $roots"
n_old="$(sqlite3 "$APPLY_DB" "SELECT count(*) FROM project_roots WHERE project='app-one';")"
[[ "$n_old" == "0" ]] || fail "identity apply: old-key roots must be gone, got $n_old"
n_alerts="$(sqlite3 "$APPLY_DB" "SELECT count(*) FROM project_identity_alerts;")"
[[ "$n_alerts" == "0" ]] || fail "identity apply: stale alerts must be dropped, got $n_alerts"

echo "PASS: memoire-project-move dry-run, fold, plan-ambiguity, apply-gate, read-only invariant, identity re-key"
