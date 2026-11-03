#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# memoire-relabel-observed.sh — memoire:D136 R-1 batch re-label.
#
# Non-raw observations linked to a derivation run whose provenance claims
# 'observed' were mislabeled by the model-derive path — a model infers, it
# never observes. The parser gate (slice C, same landing) downgrades new rows
# at parse time; this script corrects the historical corpus to 'inferred'.
#
# Scope is EXACTLY the ruled predicate — nothing else is touched:
#     kind != 'raw' AND provenance = 'observed'
#     AND derivation_run_id IS NOT NULL AND derivation_run_id != ''
#
# Safety properties (verified, not assumed):
# * content hashes are unaffected: provenance is not a CanonicalContentHash
#   input (rules.go — hash covers project/kind/normalized text only);
# * can_instruct cannot interact: the 0007 triggers ABORT any update leaving
#   can_instruct=1 without user_confirmed/imported provenance, so the script
#   pre-checks the scope for can_instruct=1 rows and refuses with a report
#   instead of failing mid-transaction;
# * single IMMEDIATE transaction; the exact changed-row count is reported
#   from changes() — the count is the completion artifact.
#
# DRY-RUN by default: prints exact counts, mutates nothing.
# Apply is double-gated (operator-only): --apply AND MEMOIRE_RELABEL_APPLY=yes.
#
# CUTOVER QUIESCE (round-1 review finding 2): an OLD memoire-derive binary
# already past its provider call can write mislabeled rows AFTER this
# script's convergence check. Apply order: stop the derive timer/hook, wait
# for in-flight memoire-derive processes to finish, deploy the gated binary,
# THEN apply. The apply path refuses while a memoire-derive process is
# alive, and the final convergence check re-verifies an empty scope.

DB="${MEMOIRE_DB:-$HOME/.memoire/memoire.db}"
APPLY=no
for arg in "$@"; do
  case "$arg" in
    --apply) APPLY=yes ;;
    --db=*) DB="${arg#--db=}" ;;
    -h|--help)
      sed -n '3,26p' "$0" | sed 's/^# \{0,1\}//'
      echo "usage: $(basename "$0") [--db=<path>] [--apply]   (apply also needs MEMOIRE_RELABEL_APPLY=yes)"
      exit 0
      ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

command -v sqlite3 >/dev/null || { echo "sqlite3 is required" >&2; exit 2; }
[[ -f "$DB" ]] || { echo "database not found: $DB" >&2; exit 2; }

SQ() { sqlite3 -cmd ".timeout 10000" "$DB" "$1"; }

# The provenance column arrived in 0007 — refuse to run against older DBs.
have="$(SQ "SELECT COUNT(*) FROM pragma_table_info('observations') WHERE name='provenance';")"
[[ "$have" == "1" ]] || { echo "observations.provenance missing (schema < 0007): refusing" >&2; exit 2; }

WHERE="kind != 'raw' AND provenance = 'observed' AND derivation_run_id IS NOT NULL AND derivation_run_id != ''"

scope="$(SQ "SELECT COUNT(*) FROM observations WHERE $WHERE;")"
guard="$(SQ "SELECT COUNT(*) FROM observations WHERE $WHERE AND can_instruct = 1;")"
raw_observed="$(SQ "SELECT COUNT(*) FROM observations WHERE kind = 'raw' AND provenance = 'observed';")"

echo "db:                    $DB"
echo "scope (to re-label):   $scope"
echo "raw observed (kept):   $raw_observed"

if [[ "$guard" != "0" ]]; then
  echo "REFUSING: $guard in-scope row(s) carry can_instruct=1 — updating them would trip the 0007 invariant trigger. Surface these to the operator first:" >&2
  SQ "SELECT id FROM observations WHERE $WHERE AND can_instruct = 1 LIMIT 20;" >&2
  exit 3
fi

if [[ "$APPLY" != "yes" ]]; then
  echo "dry-run: no changes made (use --apply with MEMOIRE_RELABEL_APPLY=yes to execute)"
  exit 0
fi
if [[ "${MEMOIRE_RELABEL_APPLY:-}" != "yes" ]]; then
  echo "REFUSING --apply: MEMOIRE_RELABEL_APPLY=yes not set (double gate)" >&2
  exit 3
fi

# Quiesce guard: an in-flight derive run (old binary, pre-gate) could write
# mislabeled rows after the convergence check below. Wait for it or stop the
# timer/hook first. Test fixtures bypass with MEMOIRE_RELABEL_SKIP_QUIESCE=yes
# (never set this against the live store).
if [[ "${MEMOIRE_RELABEL_SKIP_QUIESCE:-}" != "yes" ]] && pgrep -af 'memoire-derive' >/dev/null 2>&1; then
  echo "REFUSING --apply: a memoire-derive process is running — quiesce derivation first:" >&2
  pgrep -af 'memoire-derive' >&2 || true
  exit 3
fi

changed="$(sqlite3 -cmd ".timeout 10000" "$DB" <<SQL
BEGIN IMMEDIATE;
UPDATE observations SET provenance = 'inferred' WHERE $WHERE;
SELECT changes();
COMMIT;
SQL
)"

remaining="$(SQ "SELECT COUNT(*) FROM observations WHERE $WHERE;")"
echo "re-labeled rows:       $changed"
echo "remaining in scope:    $remaining"
[[ "$remaining" == "0" ]] || { echo "WARNING: scope not empty after apply (concurrent writer?) — rerun to converge" >&2; exit 4; }
echo "OK"
