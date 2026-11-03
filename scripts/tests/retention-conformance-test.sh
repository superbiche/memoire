#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# retention-conformance-test.sh — memoire:D136 slice D4.
#
# Drift guard for docs/RETENTION-REDACTION-CONFORMANCE.md (the conformance
# table over liaison:D97). Every decision/ruling row gets its status cell
# asserted; rows whose status names a mechanical artifact additionally get
# a reality probe (posture-only rows 6 and R4 are table-side only).
# Implementing a decision without flipping its row — or flipping a row
# without implementing — fails this test. Probes are intentionally narrow:
# they detect the ARTIFACT a status names, not every conceivable
# implementation shape; when a probe goes stale, update probe + row together.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
DOC=docs/RETENTION-REDACTION-CONFORMANCE.md

fail() { echo "FAIL: $1" >&2; exit 1; }

[[ -f "$DOC" ]] || fail "$DOC missing"

# row <n> — the decision-table line for decision n.
row() { grep -E "^\| $1 \|" "$DOC" || fail "decision row $1 missing from $DOC"; }
# rrow <n> — the ruling-table line for ruling Rn.
rrow() { grep -E "^\| R$1 \|" "$DOC" || fail "ruling row R$1 missing from $DOC"; }

# Decision 1: retro-sweep tool exists (manual-tooling).
[[ -f services/api/cmd/sweep/main.go ]] || fail "decision 1: cmd/sweep missing but table says manual-tooling"
grep -q '"memoire-retro-sweep"' services/api/internal/sweep/sweep.go || fail "decision 1: memoire-retro-sweep tool name gone"
row 1 | grep -q 'manual-tooling' || fail "decision 1 row must say manual-tooling"

# Decision 2: keep-all stands as policy posture (no probe can prove absence
# of deletion in general). The cold layer is memoire-raw-pack over
# internal/rawstore (day packs that REPLACE flat files); every raw/ reader
# must go through rawstore so packed custody stays replayable and purgeable.
# The snapshot (D155) remains an additive MIRROR surface that never deletes
# from the live store. Probes are NON-AUTHORITATIVE drift smokes (slice-2
# review round 1, finding 5): the authoritative guard is review of any diff
# touching retention surfaces.
[[ -d services/api/cmd/rawpack ]] || fail "decision 2: cmd/rawpack missing but row 2 says the cold archive is implemented"
grep -q 'func PackClosedDays' services/api/internal/rawstore/rawstore.go || fail "decision 2: rawstore packer gone"
for reader in services/api/internal/pgshadow/pgshadow.go services/api/internal/purge/purge.go services/api/internal/sweep/sweep.go; do
  grep -q 'rawstore\.Walk' "$reader" || fail "decision 2: $reader no longer reads raw/ through rawstore (packed custody would be invisible to it)"
done
if grep -rlE --exclude-dir=tests --exclude-dir=rawstore --exclude-dir=rawpack --exclude='*_test.go' 'tar\.zst' services/ scripts/ Makefile | grep -v 'memoire-snapshot.sh'; then
  fail "decision 2/R3: a second cold-archive producer appeared — route it through internal/rawstore and update rows 2 and R3"
fi
if grep -E 'rm |unlink|DELETE' scripts/memoire-snapshot.sh | grep -vE '\$STAGING|\$DB_TMP|\$GEN_DIR|state\.json|snapshot' | grep -qE 'RAW_DIR|DB_PATH'; then
  fail "decision 2: snapshot tooling must never delete live-store data"
fi
row 2 | grep -q 'implemented' || fail "decision 2 row must say implemented (keep-all and cold archives)"
row 2 | grep -q 'L0006' || fail "decision 2 row must reference the L0006 record for its deviations"

# Decision 3: compaction lever lives in memoire-purge; soft-delete filters live.
grep -qE '\bVACUUM\b' services/api/internal/purge/purge.go || fail "decision 3: purge VACUUM lever gone — update the conformance table row 3"
grep -q 'wal_checkpoint(TRUNCATE)' services/api/internal/purge/purge.go || fail "decision 3: purge WAL truncate gone"
grep -q 'deleted_at_epoch IS NULL' services/api/internal/db/store.go || fail "decision 3: soft-delete default filter gone"
row 3 | grep -q 'manual-tooling' || fail "decision 3 row must say manual-tooling (compaction lever)"

# Decision 4: memoire-purge exists, double-gated, with intent journal.
[[ -d services/api/cmd/purge ]] || fail "decision 4: cmd/purge missing — update the conformance table row 4"
grep -q 'MEMOIRE_PURGE_APPLY' services/api/cmd/purge/main.go || fail "decision 4: purge double gate gone"
grep -q 'resume-intent' services/api/cmd/purge/main.go || fail "decision 4: purge intent-journal resume gone"
row 4 | grep -q 'manual-tooling' || fail "decision 4 row must say manual-tooling"

# Decision 5: 0700 home dirs still enforced.
grep -q '0o700' services/api/internal/config/config.go || fail "decision 5: 0700 dir creation gone"
row 5 | grep -q 'implemented' || fail "decision 5 row must say implemented (permissions)"

# Decision 6: pure policy posture (no sync exists to probe); status cell +
# the (iii) mirror contract doc must exist and be referenced.
row 6 | grep -q 'absent' || fail "decision 6 row must say absent (no enforcement mechanism)"
[[ -f docs/MIRROR-PROTOCOL.md ]] || fail "decision 6: docs/MIRROR-PROTOCOL.md missing (condition (iii) contract)"
row 6 | grep -q 'MIRROR-PROTOCOL' || fail "decision 6 row must reference the mirror protocol doc"

# Decision 7: transcript ingest exists; default-deny projection structure
# holds (raw events have no FTS surface).
[[ -f scripts/memoire-capture-claude-transcripts.sh ]] || fail "decision 7: transcript ingest script missing"
grep -q 'assistant_turn' services/api/internal/modelderive/guard.go || fail "decision 7: assistant_turn event types gone"
if grep -rq 'events_fts' schema/; then
  fail "decision 7: an events FTS surface appeared — default-deny projection claim in row 7 no longer structural"
fi
row 7 | grep -q 'implemented' || fail "decision 7 row must say implemented"

# Decision 8: the drainer failed-queue reaper is live with its config knob.
grep -q 'func (d Drainer) ReapFailed' services/api/internal/drainer/drainer.go || fail "decision 8: drainer ReapFailed gone — update the conformance table row 8"
grep -q 'MEMOIRE_FAILED_TTL_DAYS' services/api/internal/config/config.go || fail "decision 8: failed-TTL config knob gone"
row 8 | grep -q 'implemented' || fail "decision 8 row must say implemented"

# Ruling R1: quarantine-generation sweep is real tooling.
grep -q 'quarantine' services/api/internal/sweep/sweep.go || fail "R1: sweep quarantine mechanics gone"
rrow 1 | grep -q 'manual-tooling' || fail "ruling R1 row must say manual-tooling"

# Ruling R2: the sequencing drift was retroactively accepted (memoire:D144);
# the acceptance record must stay on the row.
rrow 2 | grep -q 'retroactively accepted' || fail "R2 acceptance record (memoire:D144) removed"

# Ruling R3: cold layer implemented with recorded deviations (artifact
# probes shared with decision 2 above).
rrow 3 | grep -q 'implemented' || fail "ruling R3 row must say implemented"
rrow 3 | grep -q 'L0006' || fail "ruling R3 row must reference the L0006 record for its deviations"

# Ruling R4: pure policy posture; status cell only.
rrow 4 | grep -q 'held' || fail "ruling R4 row must say held"

echo "retention-conformance-test: OK"
