#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# relabel-observed-test.sh — regression test for memoire-relabel-observed.sh
# (memoire:D136 R-1 slice C). Builds a fixture DB from the real schema
# migrations, seeds every scope boundary, asserts dry-run mutates nothing,
# the apply double-gate refuses, and one double-gated apply re-labels EXACTLY
# the ruled predicate. Never touches the live store.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$ROOT/scripts/memoire-relabel-observed.sh"

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
INSERT INTO observations(project,session_id,kind,text,content_hash,review_state,provenance,derivation_run_id,occurred_at_epoch,created_at_epoch) VALUES
 -- IN SCOPE: non-raw + observed + run-linked -> becomes inferred
 ('p','s1','fact','in-scope one','h1','proposed','observed','drv_a',100,100),
 ('p','s1','decision','in-scope two','h2','accepted','observed','drv_a',101,101),
 -- OUT: raw stays observed (capture path is legitimate)
 ('p','s1','raw','raw row','h3','accepted','observed',NULL,102,102),
 -- OUT: non-raw observed WITHOUT a derivation run link
 ('p','s1','fact','no run link','h4','accepted','observed',NULL,103,103),
 ('p','s1','fact','empty run link','h5','accepted','observed','',104,104),
 -- OUT: already inferred / other provenance
 ('p','s1','fact','already inferred','h6','accepted','inferred','drv_a',105,105),
 ('p','s1','fact','confirmed row','h7','accepted','user_confirmed','drv_a',106,106);
SQL

snapshot() { sqlite3 "$DB" "SELECT id||':'||provenance FROM observations ORDER BY id;" | tr '\n' ';'; }
before="$(snapshot)"

# --- dry-run mutates nothing and reports the exact scope ---------------------
out="$("$SCRIPT" --db="$DB")"
grep -q "scope (to re-label):   2" <<<"$out" || fail "dry-run scope must be 2, got: $out"
grep -q "dry-run: no changes" <<<"$out" || fail "dry-run must announce itself"
[[ "$(snapshot)" == "$before" ]] || fail "dry-run mutated the database"

# --- apply gate: --apply alone refuses ---------------------------------------
if MEMOIRE_RELABEL_APPLY="" "$SCRIPT" --db="$DB" --apply >/dev/null 2>&1; then
  fail "--apply without MEMOIRE_RELABEL_APPLY=yes must refuse"
fi
[[ "$(snapshot)" == "$before" ]] || fail "refused apply mutated the database"

# --- quiesce guard: a live memoire-derive process blocks apply ---------------
# Simulate an in-flight derive with a sleeping decoy whose cmdline matches.
DECOY="$TMP/memoire-derive-decoy"
printf '#!/usr/bin/env bash\nsleep 30\n' > "$DECOY" && chmod +x "$DECOY"
"$DECOY" & DECOY_PID=$!
if MEMOIRE_RELABEL_APPLY=yes "$SCRIPT" --db="$DB" --apply >/dev/null 2>&1; then
  kill "$DECOY_PID" 2>/dev/null || true
  fail "apply must refuse while a memoire-derive process is running"
fi
kill "$DECOY_PID" 2>/dev/null || true
wait "$DECOY_PID" 2>/dev/null || true
[[ "$(snapshot)" == "$before" ]] || fail "quiesce-refused apply mutated the database"

# --- double-gated apply relabels exactly the ruled scope ---------------------
out="$(MEMOIRE_RELABEL_APPLY=yes MEMOIRE_RELABEL_SKIP_QUIESCE=yes "$SCRIPT" --db="$DB" --apply)"
grep -q "re-labeled rows:       2" <<<"$out" || fail "apply must report exactly 2 rows, got: $out"
grep -q "^OK$" <<<"$out" || fail "apply must end OK"

after_expected="1:inferred;2:inferred;3:observed;4:observed;5:observed;6:inferred;7:user_confirmed;"
[[ "$(snapshot)" == "$after_expected" ]] || fail "post-apply state wrong: $(snapshot)"

# Idempotent: a second apply finds an empty scope.
out="$(MEMOIRE_RELABEL_APPLY=yes MEMOIRE_RELABEL_SKIP_QUIESCE=yes "$SCRIPT" --db="$DB" --apply)"
grep -q "re-labeled rows:       0" <<<"$out" || fail "second apply must re-label 0 rows"

echo "relabel-observed-test: OK"
