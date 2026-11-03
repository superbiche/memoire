#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# memoire-rekey-derivation-runs.sh — session-join re-key for derivation_runs
# fossils (memoire:D140 residual; trigger handoff "refusal families").
#
# The registry re-key refused derivation_runs rows under ambiguous basename
# values (one basename shared by several registries): runs carry no cwd to
# split by VALUE. They CAN
# be split by SESSION — sessions are re-keyed already, and each run's
# session_id resolves to exactly one corrected project. This tool folds each
# fossil run to its session's project.
#
# Scope: project IN (<values>) AND a sessions row exists AND the session's
# project differs from the run's (a session still keyed to the same basename
# is NOT resolvable — reported, left untouched). Runs without a sessions row
# are reported and left untouched.
#
# IDEMPOTENCY COLLISIONS (round-1 review finding 1): the 0006 unique index
# covers (provider, model, derivation_version, PROJECT, session_id, window,
# input_hash). A fossil whose post-fold tuple is already occupied — the
# window was re-derived under the corrected project after the session
# re-key — CANNOT be folded (the UPDATE would abort the transaction) and
# must NOT be deleted (its observations reference the run id). Such rows
# are EXCLUDED from the fold and reported with ids for an operator ruling.
#
# The stored run id is NOT recomputed (ids are stable identifiers; the
# project-in-the-hash concern is a dedupe-key property of FUTURE runs and
# exists since the session re-key, independent of this fold).
#
# DRY-RUN by default. Apply is double-gated: --apply AND
# MEMOIRE_RUNS_REKEY_APPLY=yes. Refuses while a memoire-derive process is
# alive (quiesce guard; MEMOIRE_RUNS_REKEY_SKIP_QUIESCE=yes for hermetic
# fixtures only). Single IMMEDIATE transaction; exact changes() count.

DB="${MEMOIRE_DB:-$HOME/.memoire/memoire.db}"
# Neutral example default; pass the store's actual basename values via
# --values or MEMOIRE_RUNS_REKEY_VALUES.
VALUES="${MEMOIRE_RUNS_REKEY_VALUES:-alpha-project,beta-project}"
APPLY=no
for arg in "$@"; do
  case "$arg" in
    --apply) APPLY=yes ;;
    --db=*) DB="${arg#--db=}" ;;
    --values=*) VALUES="${arg#--values=}" ;;
    -h|--help)
      sed -n '3,26p' "$0" | sed 's/^# \{0,1\}//'
      echo "usage: $(basename "$0") [--db=<path>] [--values=v1,v2] [--apply]   (apply also needs MEMOIRE_RUNS_REKEY_APPLY=yes)"
      exit 0
      ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

command -v sqlite3 >/dev/null || { echo "sqlite3 is required" >&2; exit 2; }
[[ -f "$DB" ]] || { echo "database not found: $DB" >&2; exit 2; }

# Values validation (round-1 review finding 2): the anchored shape forbids
# empty tokens outright (leading/trailing/double commas) — read -ra would
# silently drop a trailing empty field, so validate the RAW string, then
# split. An empty token would silently match project='' rows.
[[ "$VALUES" =~ ^[A-Za-z0-9_-]+(,[A-Za-z0-9_-]+)*$ ]] \
  || { echo "invalid --values: comma-separated non-empty tokens of [A-Za-z0-9_-] required, got '$VALUES'" >&2; exit 2; }
IN_LIST=""
IFS=',' read -ra TOKENS <<<"$VALUES"
for tok in "${TOKENS[@]}"; do
  IN_LIST+="${IN_LIST:+,}'$tok'"
done
SQ() { sqlite3 -cmd ".timeout 10000" "$DB" "$1"; }

# A fossil is excluded when its post-fold 0006 tuple is occupied — either by
# an EXISTING run already at the target project, or by a SIBLING fossil that
# would fold to the same tuple (two rows differing only by pre-fold project
# share a session, hence a target; round-2 review finding 1). COALESCE
# mirrors the index. Both members of a sibling pair exclude symmetrically.
COLLIDES="(EXISTS (SELECT 1 FROM derivation_runs x, sessions sx
  WHERE sx.session_id = derivation_runs.session_id
    AND x.project = sx.project
    AND x.provider = derivation_runs.provider
    AND COALESCE(x.model,'') = COALESCE(derivation_runs.model,'')
    AND x.derivation_version = derivation_runs.derivation_version
    AND COALESCE(x.session_id,'') = COALESCE(derivation_runs.session_id,'')
    AND x.window_start_epoch = derivation_runs.window_start_epoch
    AND x.window_end_epoch = derivation_runs.window_end_epoch
    AND x.input_hash = derivation_runs.input_hash)
 OR EXISTS (SELECT 1 FROM derivation_runs y, sessions sy
  WHERE y.id != derivation_runs.id
    AND sy.session_id = y.session_id
    AND y.project IN ($IN_LIST)
    AND sy.project IS NOT NULL AND sy.project != '' AND sy.project != y.project
    AND y.provider = derivation_runs.provider
    AND COALESCE(y.model,'') = COALESCE(derivation_runs.model,'')
    AND y.derivation_version = derivation_runs.derivation_version
    AND COALESCE(y.session_id,'') = COALESCE(derivation_runs.session_id,'')
    AND y.window_start_epoch = derivation_runs.window_start_epoch
    AND y.window_end_epoch = derivation_runs.window_end_epoch
    AND y.input_hash = derivation_runs.input_hash))"

RESOLVABLE="derivation_runs.project IN ($IN_LIST)
  AND EXISTS (SELECT 1 FROM sessions s WHERE s.session_id = derivation_runs.session_id
              AND s.project IS NOT NULL AND s.project != '' AND s.project != derivation_runs.project)"
SCOPE="$RESOLVABLE AND NOT $COLLIDES"

total="$(SQ "SELECT COUNT(*) FROM derivation_runs WHERE project IN ($IN_LIST);")"
resolvable="$(SQ "SELECT COUNT(*) FROM derivation_runs WHERE $RESOLVABLE;")"
collisions="$(SQ "SELECT COUNT(*) FROM derivation_runs WHERE $RESOLVABLE AND $COLLIDES;")"
foldable="$(SQ "SELECT COUNT(*) FROM derivation_runs WHERE $SCOPE;")"
no_session="$(SQ "SELECT COUNT(*) FROM derivation_runs WHERE project IN ($IN_LIST)
  AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.session_id = derivation_runs.session_id);")"
unresolved="$((total - resolvable - no_session))"

echo "db:                      $DB"
echo "fossil runs (values $VALUES): $total"
echo "resolvable via session:  $resolvable"
echo "idempotency collisions (left): $collisions"
echo "foldable now:            $foldable"
echo "no sessions row (left):  $no_session"
echo "session same-key (left): $unresolved"
if [[ "$collisions" != "0" ]]; then
  echo "colliding fossil run ids, first 30 of $collisions (tuple occupied at target — operator ruling):"
  SQ "SELECT '  ' || id || ' (' || project || ')' FROM derivation_runs WHERE $RESOLVABLE AND $COLLIDES ORDER BY id LIMIT 30;"
  if [[ "$collisions" -gt 30 ]]; then
    echo "  ... truncated; full list: SELECT id, project FROM derivation_runs WHERE <resolvable AND collides> (see this script's SQL)"
  fi
fi
echo "per-target split (foldable):"
SQ "SELECT '  ' || derivation_runs.project || ' -> ' || s.project || ' : ' || COUNT(*)
    FROM derivation_runs JOIN sessions s ON s.session_id = derivation_runs.session_id
    WHERE $SCOPE
    GROUP BY derivation_runs.project, s.project
    ORDER BY derivation_runs.project, COUNT(*) DESC;"

if [[ "$APPLY" != "yes" ]]; then
  echo "dry-run: no changes made (use --apply with MEMOIRE_RUNS_REKEY_APPLY=yes to execute)"
  exit 0
fi
if [[ "${MEMOIRE_RUNS_REKEY_APPLY:-}" != "yes" ]]; then
  echo "REFUSING --apply: MEMOIRE_RUNS_REKEY_APPLY=yes not set (double gate)" >&2
  exit 3
fi
if [[ "${MEMOIRE_RUNS_REKEY_SKIP_QUIESCE:-}" != "yes" ]] && pgrep -af 'memoire-derive' >/dev/null 2>&1; then
  echo "REFUSING --apply: a memoire-derive process is running — quiesce derivation first:" >&2
  pgrep -af 'memoire-derive' >&2 || true
  exit 3
fi

changed="$(sqlite3 -cmd ".timeout 10000" "$DB" <<SQL
BEGIN IMMEDIATE;
UPDATE derivation_runs
SET project = (SELECT s.project FROM sessions s WHERE s.session_id = derivation_runs.session_id)
WHERE $SCOPE;
SELECT changes();
COMMIT;
SQL
)"

remaining="$(SQ "SELECT COUNT(*) FROM derivation_runs WHERE $SCOPE;")"
echo "re-keyed runs:           $changed"
echo "remaining resolvable:    $remaining"
[[ "$remaining" == "0" ]] || { echo "WARNING: resolvable scope not empty after apply — rerun to converge" >&2; exit 4; }
echo "OK"
