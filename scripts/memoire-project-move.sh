#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-project-move.sh — the standing project re-key tool for the Memoire
# store (liaison:D238 ruling 4: registry maps paths -> project ids; `memoire project-move`
# is the standing re-key). Re-keys the `project` column of historical rows so a
# collision-family basename (infra, apps, app-one, vacation, ...) splits into its
# disambiguated ids, and old-home paths fold into their current project.
#
# MODES
#   --from-path <abs-prefix> --to <id>
#       Re-key every row attributed to <abs-prefix> (component-boundary cwd
#       match on events; sessions/observations via session join) to <id>.
#   --plan <registry.json>
#       Compute the full re-key for EVERY entry of a projects registry
#       (schema liaison-memoire-projects/1) against the current DB.
#
# DEFAULT IS DRY-RUN. Dry-run opens the DB READ-ONLY (file:...?mode=ro, plus the
# sqlite3 -readonly flag), prints per-table counts and the EXACT SQL it WOULD run.
# It never writes, never touches the WAL.
#
# APPLY IS DOUBLE-GATED AND OPERATOR-ONLY. `--apply` alone is not enough: the
# env var MEMOIRE_PROJECT_MOVE_APPLY=yes must ALSO be set. Apply runs all UPDATEs
# in a single BEGIN IMMEDIATE transaction and re-checks counts afterward. This is
# a deliberate morning-ruling operation — never run it from an autonomous session.
#
# ---------------------------------------------------------------------------
# SCHEMA FACTS THIS TOOL RELIES ON (verified against schema/ in this branch):
#
# * FTS follows automatically. observations_fts mirrors observations.project via
#   the AFTER UPDATE trigger `observations_au` (schema/0001_initial.sql:139-144):
#   it deletes the old fts row and re-inserts new.project. So re-keying
#   observations.project keeps the FTS `project` column in sync with NO extra
#   work here. (events and sessions have no FTS mirror.)
#
# * No UNIQUE index can collide on a project re-key. The only index over
#   content_hash is `idx_observations_project_kind_content_hash`
#   (schema/0004_derivation.sql:43-45) and it is NON-UNIQUE. No other UNIQUE
#   index references `project`+`content_hash`. The UNIQUE indexes that exist —
#   events.idempotency_key, idx_obs_event_kind(event_id,kind),
#   derivation_runs idempotency (provider,model,version,project,session,window,
#   input_hash) — either omit project or keep it disambiguating within a single
#   target, so a project UPDATE cannot raise a UNIQUE constraint violation.
#   (derivation_runs idempotency DOES include project: two runs that differ ONLY
#   by project and become identical after re-key WOULD collide — the plan mode
#   detects and refuses ambiguous derivation_runs re-keys, see below.)
#
# * The governance trigger does not fire destructively. `governance_
#   observations_can_instruct_update` (schema/0007_governance_v1.sql:24) re-runs
#   on every UPDATE but only RAISE(ABORT)s when can_instruct=1 with a provenance
#   not in (user_confirmed,imported). A row that already carries can_instruct=1
#   passed that gate at insert, so a project-only UPDATE leaves it valid and the
#   trigger stays quiet.
#
# * CONTENT-HASH / FUTURE-DEDUPE CAVEAT. CanonicalContentHash includes `project`
#   (services/api/internal/derive/rules.go:287 hashes {project,kind,
#   normalized_text}). This tool re-keys the `project` COLUMN only; it does NOT
#   recompute or rewrite any stored content_hash. Consequences:
#     - Existing rows keep their stored hash, computed under the OLD project id.
#     - FUTURE derivations under the NEW id compute a DIFFERENT hash for the same
#       text, so they will NOT dedupe against these moved rows — a re-derivation
#       of identical content can create a second observation. This is the price
#       of re-keying and is a MORNING RULING input, surfaced in the caveats block.
#     - Because content_hash embeds the old project, two rows that came from
#       DIFFERENT old projects keep DIFFERENT hashes even after landing in the
#       same target, so the move itself does not manufacture a (project,kind,
#       content_hash) collision. Dry-run still runs a duplicate probe per target
#       (informational; the index is non-unique so a hit does not block apply).
#
# * ALIASES ARE NEVER WRITTEN TO THE DB. An old basename key (e.g. `hermie`) stays
#   retrievable because it lives in the registry entry's `aliases`, which the
#   READ path consults. This tool re-keys stored rows; it does not write aliases
#   into any table. Alias values are used ONLY as extra current-project values to
#   match derivation_runs (whose rows carry no cwd/session link to disambiguate).
# ---------------------------------------------------------------------------
#
# ENV
#   MEMOIRE_DB                  DB path (default ~/.memoire/memoire.db)
#   MEMOIRE_PROJECT_MOVE_APPLY  must equal "yes" (with --apply) to actually write
#
# EXIT: 0 on a clean dry-run/apply; non-zero on bad args or an apply gate failure.

MEMOIRE_DB="${MEMOIRE_DB:-$HOME/.memoire/memoire.db}"

DB="$MEMOIRE_DB"
MODE=""
FROM_PATH=""
TO_ID=""
PLAN_FILE=""
WANT_APPLY=0

# ---- helpers --------------------------------------------------------------

die() { printf 'memoire-project-move: %s\n' "$1" >&2; exit 2; }

usage() {
  sed -n '3,42p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

# Escape a value for a single-quoted SQL string literal.
sqlit() { local s=${1//\'/\'\'}; printf '%s' "$s"; }

# Component-boundary cwd predicate for an already-chosen path.
# Matches the exact dir or a descendant across a "/" boundary; never a sibling
# like /home/a/bc for prefix /home/a/b. Uses SQL length()/substr so char counts
# stay consistent with the stored text.
events_where() {
  local p; p="$(sqlit "$1")"
  printf "cwd IS NOT NULL AND (cwd = '%s' OR substr(cwd,1,length('%s')+1) = '%s' || '/')" "$p" "$p" "$p"
}

# Read-only query (dry-run + verification reads).
ro() { sqlite3 -readonly -batch -noheader "file:${DB}?mode=ro" "$@"; }

# ---- arg parsing ----------------------------------------------------------

[[ $# -eq 0 ]] && usage 0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --from-path) FROM_PATH="${2:-}"; shift 2 || die "--from-path needs a value" ;;
    --to)        TO_ID="${2:-}"; shift 2 || die "--to needs a value" ;;
    --plan)      PLAN_FILE="${2:-}"; shift 2 || die "--plan needs a file" ;;
    --db)        DB="${2:-}"; shift 2 || die "--db needs a path" ;;
    --apply)     WANT_APPLY=1; shift ;;
    -h|--help)   usage 0 ;;
    *)           die "unknown argument: $1" ;;
  esac
done

if [[ -n "$FROM_PATH" || -n "$TO_ID" ]]; then
  MODE="from-path"
  [[ -n "$FROM_PATH" && -n "$TO_ID" ]] || die "--from-path requires --to"
  [[ -n "$PLAN_FILE" ]] && die "--from-path and --plan are mutually exclusive"
  [[ "$FROM_PATH" == /* ]] || die "--from-path must be an absolute path"
  while [[ "$FROM_PATH" == */ && "$FROM_PATH" != "/" ]]; do FROM_PATH="${FROM_PATH%/}"; done
elif [[ -n "$PLAN_FILE" ]]; then
  MODE="plan"
  [[ -f "$PLAN_FILE" ]] || die "registry not found: $PLAN_FILE"
  command -v jq >/dev/null 2>&1 || die "jq required for --plan"
  jq -e '.projects | type == "object"' "$PLAN_FILE" >/dev/null 2>&1 \
    || die "registry has no .projects object: $PLAN_FILE"
else
  die "need --from-path <p> --to <id>, or --plan <registry.json>"
fi

[[ -f "$DB" ]] || die "database not found: $DB (set MEMOIRE_DB or --db)"

# Apply double-gate.
APPLY=0
if [[ "$WANT_APPLY" -eq 1 ]]; then
  if [[ "${MEMOIRE_PROJECT_MOVE_APPLY:-}" == "yes" ]]; then
    APPLY=1
  else
    die "--apply refused: set MEMOIRE_PROJECT_MOVE_APPLY=yes to actually write (operator-only). Refusing."
  fi
fi

# ---- accumulators ---------------------------------------------------------

APPLY_SQL=""            # newline-joined UPDATE statements (printed in dry-run, run on apply)
declare -a WARNINGS=()  # ambiguity / caveat lines to surface at the end

emit_sql() { APPLY_SQL+="$1"$'\n'; }
warn()     { WARNINGS+=("$1"); }

banner() {
  printf '======================================================================\n'
  printf 'memoire-project-move  (%s)\n' "$1"
  printf '  db:   %s\n' "$DB"
  printf '  mode: %s%s\n' "$MODE" "$([[ $APPLY -eq 1 ]] && echo '  [APPLY]' || echo '  [DRY-RUN]')"
  printf '======================================================================\n'
}

# Re-key one path prefix -> id. Prints counts + SQL for events/sessions/observations.
process_path() {
  local path="$1" id="$2"
  local w; w="$(events_where "$path")"
  local idl; idl="$(sqlit "$id")"
  local sset="SELECT DISTINCT session_id FROM events WHERE ${w}"

  # counts (single read-only session; TEMP table is allowed on a readonly main DB
  # because temp objects live outside the main file).
  local counts
  counts="$(ro <<SQL
CREATE TEMP TABLE _mp AS ${sset};
SELECT (SELECT count(*) FROM events WHERE ${w});
SELECT (SELECT count(*) FROM sessions WHERE session_id IN (SELECT session_id FROM _mp));
SELECT (SELECT count(*) FROM observations WHERE session_id IN (SELECT session_id FROM _mp));
SQL
)"
  local n_ev n_se n_ob
  n_ev="$(sed -n '1p' <<<"$counts")"
  n_se="$(sed -n '2p' <<<"$counts")"
  n_ob="$(sed -n '3p' <<<"$counts")"

  printf '\n-- path %s  ->  %s\n' "$path" "$id"
  printf '   events        (cwd component-boundary match) : %s\n' "${n_ev:-0}"
  printf '   sessions      (via session_id join)          : %s\n' "${n_se:-0}"
  printf '   observations  (via session_id join)          : %s\n' "${n_ob:-0}"

  emit_sql "UPDATE events SET project='${idl}' WHERE ${w};"
  emit_sql "UPDATE sessions SET project='${idl}' WHERE session_id IN (${sset});"
  emit_sql "UPDATE observations SET project='${idl}' WHERE session_id IN (${sset});"

  # Duplicate probe: would any (kind,content_hash) group hold >1 row under the
  # target after the move? Informational only (index is non-unique).
  local dup
  dup="$(ro <<SQL
WITH moved AS (
  SELECT kind, content_hash FROM observations
  WHERE content_hash IS NOT NULL
    AND ( project='${idl}' OR session_id IN (${sset}) )
)
SELECT count(*) FROM (SELECT 1 FROM moved GROUP BY kind, content_hash HAVING count(*)>1);
SQL
)"
  if [[ "${dup:-0}" -gt 0 ]]; then
    warn "target '${id}': ${dup} (kind,content_hash) group(s) would hold >1 observation after move (informational; index is non-unique — morning review)."
  fi
}

# Re-key derivation_runs by project VALUE (task-specified). `values` are the
# current project strings for this target. Ambiguous values (that map to more
# than one target across the plan, or that also tag events OUTSIDE this prefix in
# from-path mode) are REFUSED here and surfaced as warnings, because
# derivation_runs carries no cwd to split by path.
process_derivation() {
  local id="$1"; shift
  local idl; idl="$(sqlit "$id")"
  local -a vals=("$@")
  [[ ${#vals[@]} -gt 0 ]] || return 0

  local safe=() unsafe=()
  local v
  for v in "${vals[@]}"; do
    [[ -n "$v" ]] || continue
    if [[ " ${AMBIGUOUS_VALUES[*]-} " == *" ${v} "* ]]; then
      unsafe+=("$v")
    else
      safe+=("$v")
    fi
  done

  # count of derivation_runs matching the SAFE values
  local n_dr=0
  if [[ ${#safe[@]} -gt 0 ]]; then
    local inlist="" first=1 vv
    for vv in "${safe[@]}"; do
      [[ $first -eq 1 ]] || inlist+=","
      inlist+="'$(sqlit "$vv")'"; first=0
    done
    n_dr="$(ro "SELECT count(*) FROM derivation_runs WHERE project IN (${inlist});")"
    printf '   derivation_runs (by project value %s) : %s\n' "${inlist}" "${n_dr:-0}"
    emit_sql "UPDATE derivation_runs SET project='${idl}' WHERE project IN (${inlist});"

    # Schema 0010 identity tables (also keyed by project VALUE, no cwd).
    # Roots merge into the target (UPDATE OR IGNORE skips (project,root) PK
    # collisions with roots the target already knows; leftovers are dropped);
    # alerts for the old key are deleted outright — their known_root context
    # is stale after a re-key, and live events under the new key re-learn or
    # re-alert honestly. Guarded on table existence so the tool still runs
    # against pre-0010 stores.
    local has_identity
    has_identity="$(ro "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='project_roots';")"
    if [[ "${has_identity:-0}" -gt 0 ]]; then
      local n_pr n_al
      n_pr="$(ro "SELECT count(*) FROM project_roots WHERE project IN (${inlist});")"
      n_al="$(ro "SELECT count(*) FROM project_identity_alerts WHERE project IN (${inlist});")"
      printf '   project_roots  (merge to target)             : %s\n' "${n_pr:-0}"
      printf '   identity_alerts (dropped, stale after move)  : %s\n' "${n_al:-0}"
      emit_sql "UPDATE OR IGNORE project_roots SET project='${idl}' WHERE project IN (${inlist});"
      emit_sql "DELETE FROM project_roots WHERE project IN (${inlist});"
      emit_sql "DELETE FROM project_identity_alerts WHERE project IN (${inlist});"
    fi
  fi
  if [[ ${#unsafe[@]} -gt 0 ]]; then
    local u; for u in "${unsafe[@]}"; do
      local n; n="$(ro "SELECT count(*) FROM derivation_runs WHERE project='$(sqlit "$u")';")"
      warn "derivation_runs value '${u}' -> '${id}' REFUSED: value is ambiguous (maps to multiple targets / spans other paths). ${n} run(s) left untouched — derivation_runs has no cwd to split by; needs session-level re-key or a morning ruling."
    done
  fi
}

# ---- main -----------------------------------------------------------------

declare -a AMBIGUOUS_VALUES=()

if [[ "$MODE" == "from-path" ]]; then
  banner "$MODE"
  process_path "$FROM_PATH" "$TO_ID"

  # derivation values = distinct current project of matched events. A value is
  # ambiguous if events with that project ALSO exist outside this prefix.
  local_w="$(events_where "$FROM_PATH")"
  mapfile -t dvals < <(ro "SELECT DISTINCT project FROM events WHERE ${local_w};")
  AMBIGUOUS_VALUES=()
  for v in "${dvals[@]}"; do
    [[ -n "$v" ]] || continue
    outside="$(ro "SELECT count(*) FROM events WHERE project='$(sqlit "$v")' AND NOT (${local_w});")"
    [[ "${outside:-0}" -gt 0 ]] && AMBIGUOUS_VALUES+=("$v")
  done
  process_derivation "$TO_ID" "${dvals[@]}"

elif [[ "$MODE" == "plan" ]]; then
  banner "$MODE"
  printf 'registry: %s\n' "$PLAN_FILE"

  # Cross-entry value-collision map: any (display|alias) value that appears in
  # more than one entry cannot be re-keyed by value alone.
  mapfile -t AMBIGUOUS_VALUES < <(
    jq -r '.projects | to_entries[] | .key as $k
           | ([.value.display] + (.value.aliases // []))[] | select(. != null) | "\(.)\t\($k)"' "$PLAN_FILE" \
    | awk -F'\t' '{ if (!seen[$1 SUBSEP $2]++) cnt[$1]++ } END { for (v in cnt) if (cnt[v]>1) print v }'
  )
  if [[ ${#AMBIGUOUS_VALUES[@]} -gt 0 ]]; then
    warn "derivation_runs: value(s) shared by multiple entries and NOT re-keyable by value: ${AMBIGUOUS_VALUES[*]}"
  fi

  mapfile -t KEYS < <(jq -r '.projects | keys[]' "$PLAN_FILE")

  # Longest-prefix-wins (review FIX 1): emit path updates ordered by prefix
  # length ASCENDING, so a broader prefix runs first and a more-specific nested
  # prefix runs later and re-claims its rows. Alphabetical entry order would let
  # a later broad entry clobber an earlier nested one (e.g. srv-one-apps under
  # personal-infra). Per-path counts are computed pre-apply, so a nested row is
  # counted under BOTH its broad and specific entries; final attribution is the
  # most specific prefix.
  printf '\n[plan] path updates ordered broad -> specific (longest prefix wins)\n'
  PAIRS=()
  for key in "${KEYS[@]}"; do
    mapfile -t paths < <(jq -r --arg k "$key" '.projects[$k].paths[]? // empty' "$PLAN_FILE")
    for p in "${paths[@]}"; do
      [[ -n "$p" ]] && PAIRS+=("${#p}"$'\t'"$p"$'\t'"$key")
    done
  done
  while IFS=$'\t' read -r _len p key; do
    [[ -n "$p" ]] && process_path "$p" "$key"
  done < <(printf '%s\n' "${PAIRS[@]}" | LC_ALL=C sort -n -t$'\t' -k1,1 -k2,2)

  for key in "${KEYS[@]}"; do
    display="$(jq -r --arg k "$key" '.projects[$k].display // $k' "$PLAN_FILE")"
    mapfile -t aliases < <(jq -r --arg k "$key" '.projects[$k].aliases[]? // empty' "$PLAN_FILE")
    printf '\n### entry: %s\n' "$key"
    process_derivation "$key" "$display" "${aliases[@]}"
  done
fi

# ---- SQL preview / apply --------------------------------------------------

printf '\n----------------------------------------------------------------------\n'
printf 'SQL that WOULD run (%s statements):\n' "$(grep -c ';' <<<"$APPLY_SQL")"
printf '%s\n' '----------------------------------------------------------------------'
printf '%s' "$APPLY_SQL"

# Caveats block — always printed so the morning ruling sees it.
printf '\n----------------------------------------------------------------------\n'
printf 'CAVEATS (morning-ruling input):\n'
printf '  * content_hash embeds project (rules.go:287). Stored hashes are NOT\n'
printf '    recomputed; moved rows keep old-project hashes. FUTURE derivations\n'
printf '    under the new id hash differently and will NOT dedupe against these\n'
printf '    moved rows — re-derivation can create a second observation.\n'
printf '  * idx_observations_project_kind_content_hash is NON-UNIQUE, so a\n'
printf '    project UPDATE cannot raise a UNIQUE violation on observations.\n'
printf '  * derivation_runs idempotency index DOES include project; ambiguous\n'
printf '    values were refused above rather than risk a merge collision.\n'
printf '  * FTS observations_fts.project follows via the observations_au trigger.\n'
printf '  * A roaming session that touched multiple prefixes is counted under\n'
printf '    each — session-home is the primary attribution; per-event cwd stays.\n'
if [[ ${#WARNINGS[@]} -gt 0 ]]; then
  printf '\nWARNINGS:\n'
  for w in "${WARNINGS[@]}"; do printf '  ! %s\n' "$w"; done
else
  printf '\nWARNINGS: none\n'
fi

if [[ "$APPLY" -eq 0 ]]; then
  printf '\n[DRY-RUN] no rows written. To apply: --apply AND MEMOIRE_PROJECT_MOVE_APPLY=yes.\n'
  exit 0
fi

# APPLY: single immediate transaction, then re-check.
printf '\n[APPLY] writing in one immediate transaction...\n'
{
  printf 'BEGIN IMMEDIATE;\n'
  printf '%s' "$APPLY_SQL"
  printf 'COMMIT;\n'
} | sqlite3 -bail "$DB" || die "apply transaction failed mid-statement; -bail stopped before COMMIT, so no partial write was committed — inspect the DB before retrying"

printf '[APPLY] transaction committed. Re-checking...\n'
if [[ "$MODE" == "from-path" ]]; then
  w="$(events_where "$FROM_PATH")"
  residual="$(sqlite3 -readonly "file:${DB}?mode=ro" "SELECT count(*) FROM events WHERE ${w} AND project<>'$(sqlit "$TO_ID")';")"
  printf '  residual mis-keyed events under prefix: %s (expect 0)\n' "${residual:-?}"
fi
printf '[APPLY] done.\n'
exit 0
