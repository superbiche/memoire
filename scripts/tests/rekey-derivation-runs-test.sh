#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# rekey-derivation-runs-test.sh — regression test for
# memoire-rekey-derivation-runs.sh (memoire:D140 residual fold). Fixture from
# the real schema; covers every scope boundary, dry-run purity, the apply
# double-gate, the quiesce guard, exact apply count, and idempotency.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$ROOT/scripts/memoire-rekey-derivation-runs.sh"

command -v sqlite3 >/dev/null || { echo "SKIP: sqlite3 not available"; exit 0; }

fail() { echo "FAIL: $1" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP" 2>/dev/null || true' EXIT
DB="$TMP/fixture.db"

for m in 0001_initial 0004_derivation 0005_observation_review_log 0006_derivation_model 0007_governance_v1 \
         0008_review_queue_indexes 0009_derivation_run_created_ns 0010_project_identity \
         0011_redaction_violation_index; do
  sqlite3 "$DB" < "$ROOT/schema/${m}.sql" >/dev/null || fail "schema $m failed to apply"
done

sqlite3 "$DB" <<'SQL'
INSERT INTO sessions(session_id,project,client,started_at_epoch) VALUES
 ('s1','client-a-infra','cc',100),   -- resolvable target
 ('s2','personal-infra','cc',101),   -- resolvable target
 ('s3','alpha-project','cc',102);            -- session STILL under the basename -> unresolved
INSERT INTO derivation_runs(id,project,session_id,provider,data_boundary,derivation_version,window_start_epoch,window_end_epoch,input_hash,status,created_at_epoch) VALUES
 ('drv_1','alpha-project','s1','rules','local','v1',100,200,'h1','completed',100),  -- -> client-a-infra
 ('drv_2','alpha-project','s2','rules','local','v1',100,200,'h2','completed',101),  -- -> personal-infra
 ('drv_3','alpha-project','s3','rules','local','v1',100,200,'h3','completed',102),  -- session same-key: left
 ('drv_4','alpha-project','s-missing','rules','local','v1',100,200,'h4','completed',103), -- no sessions row: left
 ('drv_5','beta-project','s2','rules','local','v1',100,200,'h5','completed',104),   -- -> personal-infra
 ('drv_6','liaison','s1','rules','local','v1',100,200,'h6','completed',105),-- out of values: untouched
 -- idempotency collision pair: drv_7 maps to personal-infra where drv_8
 -- already occupies the identical 0006 tuple -> drv_7 is excluded, reported
 ('drv_7','alpha-project','s2','rules','local','v1',300,400,'h7','completed',106),
 ('drv_8','personal-infra','s2','rules','local','v1',300,400,'h7','completed',107),
 -- sibling collision pair (round-2 finding 1): two fossils differing only by
 -- pre-fold project share session/window/hash -> both would fold to the SAME
 -- personal-infra tuple; both excluded and reported, neither folded
 ('drv_9','alpha-project','s2','rules','local','v1',500,600,'h9','completed',108),
 ('drv_10','beta-project','s2','rules','local','v1',500,600,'h9','completed',109);
SQL

snapshot() { sqlite3 "$DB" "SELECT id||':'||project FROM derivation_runs ORDER BY id;" | tr '\n' ';'; }
before="$(snapshot)"

# --- dry-run: exact counts, no mutation --------------------------------------
out="$("$SCRIPT" --db="$DB")"
grep -q "fossil runs (values alpha-project,beta-project): 8" <<<"$out" || fail "total must be 8: $out"
grep -q "resolvable via session:  6" <<<"$out" || fail "resolvable must be 6: $out"
grep -q "idempotency collisions (left): 3" <<<"$out" || fail "collisions must be 3: $out"
grep -q "foldable now:            3" <<<"$out" || fail "foldable must be 3: $out"
grep -q "no sessions row (left):  1" <<<"$out" || fail "no-session must be 1: $out"
grep -q "session same-key (left): 1" <<<"$out" || fail "same-key must be 1: $out"
grep -q "drv_7 (alpha-project)" <<<"$out" || fail "target-collision id must be reported: $out"
grep -q "drv_9 (alpha-project)" <<<"$out" || fail "sibling-collision drv_9 must be reported: $out"
grep -q "drv_10 (beta-project)" <<<"$out" || fail "sibling-collision drv_10 must be reported: $out"
[[ "$(snapshot)" == "$before" ]] || fail "dry-run mutated the database"

# --- values validation (round-1 finding 2) -----------------------------------
for bad in "--values=" "--values=," "--values=alpha-project," "--values=alpha-project,,beta-project"; do
  if "$SCRIPT" --db="$DB" "$bad" >/dev/null 2>&1; then
    fail "$bad must be rejected"
  fi
done

# --- gates -------------------------------------------------------------------
if MEMOIRE_RUNS_REKEY_APPLY="" "$SCRIPT" --db="$DB" --apply >/dev/null 2>&1; then
  fail "--apply without env gate must refuse"
fi
DECOY="$TMP/memoire-derive-decoy"
printf '#!/usr/bin/env bash\nsleep 30\n' > "$DECOY" && chmod +x "$DECOY"
"$DECOY" & DECOY_PID=$!
if MEMOIRE_RUNS_REKEY_APPLY=yes "$SCRIPT" --db="$DB" --apply >/dev/null 2>&1; then
  kill "$DECOY_PID" 2>/dev/null || true
  fail "apply must refuse while a memoire-derive process is running"
fi
kill "$DECOY_PID" 2>/dev/null || true
wait "$DECOY_PID" 2>/dev/null || true
[[ "$(snapshot)" == "$before" ]] || fail "refused applies mutated the database"

# --- double-gated apply: exactly the resolvable rows -------------------------
out="$(MEMOIRE_RUNS_REKEY_APPLY=yes MEMOIRE_RUNS_REKEY_SKIP_QUIESCE=yes "$SCRIPT" --db="$DB" --apply)"
grep -q "re-keyed runs:           3" <<<"$out" || fail "apply must re-key exactly 3: $out"
grep -q "^OK$" <<<"$out" || fail "apply must end OK"

expected="drv_1:client-a-infra;drv_10:beta-project;drv_2:personal-infra;drv_3:alpha-project;drv_4:alpha-project;drv_5:personal-infra;drv_6:liaison;drv_7:alpha-project;drv_8:personal-infra;drv_9:alpha-project;"
[[ "$(snapshot)" == "$expected" ]] || fail "post-apply state wrong: $(snapshot)"

# --- idempotent --------------------------------------------------------------
out="$(MEMOIRE_RUNS_REKEY_APPLY=yes MEMOIRE_RUNS_REKEY_SKIP_QUIESCE=yes "$SCRIPT" --db="$DB" --apply)"
grep -q "re-keyed runs:           0" <<<"$out" || fail "second apply must re-key 0"

echo "rekey-derivation-runs-test: OK"
