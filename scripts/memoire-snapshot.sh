#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# memoire-snapshot.sh — slice 2 (memoire:D150/D154/D155): age-encrypted,
# generation-aware snapshots of the SQLite store + raw/ custody, staged
# locally and (optionally) pushed into HL3's off-site pipeline landing zone.
#
# Artifact set (staging dir; the pushed tree is staging minus .state/):
#   db/memoire-db.db.zst.age          nightly full DB snapshot (VACUUM INTO),
#                                     STABLE name: each push overwrites, the
#                                     pipeline's --backup-dir archives priors.
#   raw/gen-<G>/baseline.tar.zst.age  full raw/ custody at generation G.
#   raw/gen-<G>/inc-<stamp>.tar.zst.age
#                                     raw files not yet captured by this
#                                     generation's baseline/incrementals.
#   MANIFEST.json                     plaintext: generation, artifact names,
#                                     sha256s. Names and hashes only — never
#                                     payload content. RESTORE AUTHORITY:
#                                     only manifest-listed artifacts count;
#                                     unlisted files in the landing zone are
#                                     push residue and must be ignored.
#   .state/                           local-only snapshot state (never
#                                     pushed): state.json (generation,
#                                     recipient, inc count) + the captured-
#                                     file set for the current generation.
#
# Membership discipline (slice-2 review round 1, finding 1): raw filenames
# carry QUEUE-write timestamps but land in raw/ at DRAIN time — under
# backlog or remote forwarding, filename order is NOT arrival order, so no
# timestamp cutoff is sound. Incrementals are computed as a SET DIFFERENCE:
# files present in raw/ and absent from the captured set. Correct under any
# arrival order; a file is never skipped, only captured late.
#
# Key rotation (finding 2): .state/state.json records the recipient an
# artifact set was encrypted to. A recipient change forces a full
# re-baseline, so every artifact in staging is always encrypted to the
# CURRENT recipient. Crypto-shred procedure: replace the recipient file,
# run snapshot (auto re-baselines) + push, VERIFY the push succeeded, and
# only then destroy the old key (docs/MIRROR-PROTOCOL.md rule 5).
#
# Push protocol (finding 3, tightened round 2): three phases so the landing
# zone always holds a manifest whose artifacts are all present — (1)
# artifact sync with --delay-updates (no deletions, manifest excluded;
# transfers stage remotely and rename only when ALL are complete, so an
# interrupted transfer leaves the OLD set fully intact; the residual
# mixed-set window is a crash during the final rename batch — sub-second,
# local-disk — detectable by rule-7 hash verification and recoverable from
# the off-site _archive), (2) manifest sync (supersedes), (3) prune pass
# (rsync --delete; an interruption leaves stale unlisted files, which the
# manifest authority rule already ignores).
#
# Generation discipline (docs/MIRROR-PROTOCOL.md): the custody generation
# (custody/MANIFEST.json, bumped by memoire-purge) namespaces raw
# artifacts; a bump drops the old gen from staging and re-baselines. The
# off-site pipeline is rclone copy (never deletes remotely, unbounded
# _archive), so remote deletion is NOT the mirror-purge lever — key
# rotation is (memoire:D155).
#
# Usage:
#   memoire-snapshot.sh [--push] [--raw-only] [--home DIR] [--staging DIR]
#
#   --raw-only   Postgres-era mode (HL3 hub): the DB leg is CNPG PITR's
#                job. Never requires the SQLite file, never stages a DB
#                artifact, and PURGES any stale staged DB artifact so the
#                manifest and push tree are raw-only too.
# Env:
#   MEMOIRE_HOME                store home (default ~/.memoire)
#   MEMOIRE_DB                  store of record (SQLite path or postgres://
#                               DSN); when unset, read from db.env exactly
#                               like the derive sweep. A Postgres DSN makes
#                               the default-mode DB leg REFUSE (use
#                               --raw-only): the local SQLite file is a
#                               frozen pre-cutover store and snapshotting
#                               it would publish a green, weeks-stale
#                               MANIFEST. A SQLite value overrides the
#                               default $MEMOIRE_HOME/memoire.db path.
#   MEMOIRE_DB_ENV_FILE         env-file fallback for MEMOIRE_DB (default
#                               $MEMOIRE_HOME/db.env)
#   MEMOIRE_SNAPSHOT_RECIPIENT  age recipient (public key). Required unless
#                               MEMOIRE_SNAPSHOT_RECIPIENT_FILE resolves.
#   MEMOIRE_SNAPSHOT_RECIPIENT_FILE
#                               file containing the recipient (default
#                               $MEMOIRE_HOME/snapshot-recipient.txt)
#   MEMOIRE_SNAPSHOT_STAGING    staging dir (default $MEMOIRE_HOME/snapshots)
#   MEMOIRE_SNAPSHOT_PUSH_DEST  rsync destination for --push, e.g.
#                               memoire-mirror-hl3:/
#   MEMOIRE_SNAPSHOT_MAX_INCS   re-baseline after this many incrementals
#                               (default 30)
#   MEMOIRE_SNAPSHOT_CHOWN      uid:gid ownership mapping applied to every
#                               push phase (--chown). For PRIVILEGED (root)
#                               receivers pushing into a zone owned by a
#                               non-root writer (the HL3 rrsync identity)
#                               — unset for ordinary remote pushes.

HOME_DIR="${MEMOIRE_HOME:-$HOME/.memoire}"
STAGING="${MEMOIRE_SNAPSHOT_STAGING:-}"
PUSH=0
RAW_ONLY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --push) PUSH=1; shift ;;
    # Postgres-era mode (HL3 relocation, cdk8s review round-1 finding 2):
    # the DB leg is CNPG PITR's job; this script keeps ONLY raw/ custody.
    # Never requires or stages the SQLite file — a stale copy must not be
    # silently snapshotted as if it were the store.
    --raw-only) RAW_ONLY=1; shift ;;
    --home) HOME_DIR="${2:?}"; shift 2 ;;
    --staging) STAGING="${2:?}"; shift 2 ;;
    -h|--help) grep -E '^# ' "$0" | cut -c3-; exit 0 ;;
    *) echo "memoire-snapshot: unknown argument: $1" >&2; exit 2 ;;
  esac
done

STAGING="${STAGING:-$HOME_DIR/snapshots}"

# Store selection (dialect seam, memoire:D159 cutover): resolve MEMOIRE_DB
# exactly like the derive sweep — env first, then the canonical db.env —
# so the snapshot lane reads the same store-of-record as every other
# consumer. Read without shell-eval so credential characters in a DSN stay
# inert. Post-cutover the resolved value is a postgres:// DSN: the DB leg
# then REFUSES instead of falling through to whatever pre-cutover SQLite
# file still sits at $HOME_DIR/memoire.db — snapshotting that frozen store
# would publish a hash-verified MANIFEST vouching for weeks-stale data,
# and MIRROR-PROTOCOL's restore authority would faithfully restore it
# (inbox 2026-08-03; D166 hard-fail doctrine). --raw-only is the ruled
# Postgres-era mode and is unaffected: it never touches the DB leg.
DB_ENV_FILE="${MEMOIRE_DB_ENV_FILE:-$HOME_DIR/db.env}"
if [[ -z "${MEMOIRE_DB:-}" && -r "$DB_ENV_FILE" ]]; then
  # `|| true`: a readable db.env without a MEMOIRE_DB line is not an error
  # (the sweep runs without -e; this script would die under -e + pipefail
  # on grep's miss — review r1 F1).
  MEMOIRE_DB="$(grep -m1 '^MEMOIRE_DB=' "$DB_ENV_FILE" | cut -d= -f2- || true)"
fi
DB_PATH="$HOME_DIR/memoire.db"
if [[ -n "${MEMOIRE_DB:-}" ]]; then
  if [[ "$MEMOIRE_DB" == postgres://* || "$MEMOIRE_DB" == postgresql://* ]]; then
    if [[ "$RAW_ONLY" != 1 ]]; then
      echo "memoire-snapshot: MEMOIRE_DB resolves to a Postgres DSN — the SQLite DB leg would snapshot a stale pre-cutover store; use --raw-only (CNPG PITR owns the DB)" >&2
      exit 1
    fi
  else
    DB_PATH="$MEMOIRE_DB"
  fi
fi
RAW_DIR="$HOME_DIR/raw"
CUSTODY_MANIFEST="$HOME_DIR/custody/MANIFEST.json"
MAX_INCS="${MEMOIRE_SNAPSHOT_MAX_INCS:-30}"

TOOLS=(age zstd jq sha256sum tar comm flock)
[[ "$RAW_ONLY" == 1 ]] || TOOLS+=(sqlite3)
for tool in "${TOOLS[@]}"; do
  command -v "$tool" >/dev/null 2>&1 || { echo "memoire-snapshot: $tool is required" >&2; exit 1; }
done
if [[ "$RAW_ONLY" != 1 ]]; then
  [[ -f "$DB_PATH" ]] || { echo "memoire-snapshot: no database at $DB_PATH" >&2; exit 1; }
fi
[[ -d "$RAW_DIR" ]] || { echo "memoire-snapshot: no raw dir at $RAW_DIR" >&2; exit 1; }

# raw/ custody lock (internal/rawstore LockPath: a dotfile BESIDE raw/, so
# it is never captured). The packer and purge rewrite raw/ — day packs
# replace flat files — and must not move a file between the inventory and
# the tar below. Packs are ordinary top-level files to this script: they
# are captured by name like any other raw/ file.
RAW_LOCK="$HOME_DIR/.raw.lock"   # == rawstore.LockPath("$HOME_DIR/raw")
exec 8>>"$RAW_LOCK"
flock -w "${MEMOIRE_SNAPSHOT_LOCK_WAIT_S:-1800}" 8 \
  || { echo "memoire-snapshot: raw custody lock busy: $RAW_LOCK" >&2; exit 1; }

RECIPIENT="${MEMOIRE_SNAPSHOT_RECIPIENT:-}"
if [[ -z "$RECIPIENT" ]]; then
  RECIPIENT_FILE="${MEMOIRE_SNAPSHOT_RECIPIENT_FILE:-$HOME_DIR/snapshot-recipient.txt}"
  [[ -r "$RECIPIENT_FILE" ]] || { echo "memoire-snapshot: no recipient (set MEMOIRE_SNAPSHOT_RECIPIENT or create $RECIPIENT_FILE)" >&2; exit 1; }
  RECIPIENT="$(grep -v '^#' "$RECIPIENT_FILE" | grep -m1 . || true)"
fi
[[ "$RECIPIENT" == age1* ]] || { echo "memoire-snapshot: recipient does not look like an age public key" >&2; exit 1; }

# Custody generation: missing manifest = fresh store = generation 0 (same
# semantics as memoire-purge readGeneration). Anything else — unreadable
# JSON, wrong type, negative — is a LOUD STOP before any staging mutation:
# snapshotting under a wrong generation namespace would poison the mirror's
# purge story (round-1 finding 4: the type check must gate up front).
GEN=0
if [[ -f "$CUSTODY_MANIFEST" ]]; then
  GEN="$(jq -er 'if (.generation|type) == "number" and .generation == (.generation|floor) and .generation >= 0 then (.generation|tostring) else error("generation is not a non-negative integer") end' "$CUSTODY_MANIFEST" 2>/dev/null)" \
    || { echo "memoire-snapshot: custody manifest $CUSTODY_MANIFEST is unreadable or its generation is not a non-negative integer; refusing to snapshot" >&2; exit 1; }
  [[ "$GEN" =~ ^[0-9]+$ ]] || { echo "memoire-snapshot: custody generation $GEN is not a non-negative integer; refusing to snapshot" >&2; exit 1; }
fi

mkdir -p "$STAGING/db" "$STAGING/raw" "$STAGING/.state"
STATE_FILE="$STAGING/.state/state.json"
CAPTURED="$STAGING/.state/captured-gen-$GEN.txt"
STATE_GEN=-1 STATE_RECIPIENT="" INCS=0
if [[ -f "$STATE_FILE" ]]; then
  STATE_GEN="$(jq -r '.generation // -1' "$STATE_FILE")"
  STATE_RECIPIENT="$(jq -r '.recipient // ""' "$STATE_FILE")"
  INCS="$(jq -r '.incrementals // 0' "$STATE_FILE")"
fi

# Exactly one run per staging dir (slice-2 round-2 finding N1): concurrent
# runs would race the captured set, the .current temp files, and the
# cleanup trap's glob. The lock lives under .state/ (never pushed).
exec 9>"$STAGING/.state/lock"
flock -n 9 || { echo "memoire-snapshot: another run holds $STAGING/.state/lock; refusing" >&2; exit 1; }

NOW_UTC="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# Nanosecond-resolution stamp: incremental artifact names must be unique
# and lexicographically ordered even for same-second runs (finding N1 —
# a colliding name silently overwrote the earlier incremental while the
# captured set claimed both were mirrored).
RUN_STAMP="$(date -u +%Y%m%dT%H%M%S%NZ)"
GEN_DIR="$STAGING/raw/gen-$GEN"

# --- DB snapshot (stable name; consistent under WAL via VACUUM INTO) -------
# Skipped entirely under --raw-only (Postgres era: CNPG PITR owns the DB).
if [[ "$RAW_ONLY" != 1 ]]; then
  DB_TMP="$(mktemp "$STAGING/.db-snapshot.XXXXXX")"
  rm -f "$DB_TMP" # VACUUM INTO refuses to overwrite; keep the reserved name
  cleanup() { rm -f "$DB_TMP" "$DB_TMP.zst.age.tmp" "$STAGING/.state/.current."* 2>/dev/null || true; }
  trap cleanup EXIT
  sqlite3 "file:$DB_PATH?mode=ro" "VACUUM INTO '$DB_TMP'"
  zstd -q -T0 -c "$DB_TMP" | age -r "$RECIPIENT" > "$DB_TMP.zst.age.tmp"
  mv "$DB_TMP.zst.age.tmp" "$STAGING/db/memoire-db.db.zst.age"
  rm -f "$DB_TMP"
else
  cleanup() { rm -f "$STAGING/.state/.current."* 2>/dev/null || true; }
  trap cleanup EXIT
  # Raw-only selects raw-only ARTIFACTS, not merely raw-only production
  # (cdk8s review round-2 MED-1): a DB artifact left by a pre-cutover
  # default-mode run must not stay authoritative — purge it so neither
  # the manifest walk nor the push tree can carry it.
  if compgen -G "$STAGING/db/*" >/dev/null 2>&1; then
    echo "memoire-snapshot: raw-only — purging stale staged DB artifact(s)" >&2
    rm -f "$STAGING/db/"*
  fi
fi

# --- raw custody: baseline or incremental, set-difference membership -------
rebaseline=0
if [[ "$STATE_GEN" != "$GEN" ]]; then
  # Generation bump (or first run): the old generation's artifacts and
  # captured sets leave staging NOW; the landing zone follows on the next
  # --push. Remote stragglers die by key rotation, not file deletion.
  find "$STAGING/raw" -mindepth 1 -maxdepth 1 -type d ! -name "gen-$GEN" -exec rm -rf {} +
  find "$STAGING/.state" -maxdepth 1 -type f -name 'captured-gen-*.txt' ! -name "captured-gen-$GEN.txt" -delete
  rebaseline=1
elif [[ -n "$STATE_RECIPIENT" && "$STATE_RECIPIENT" != "$RECIPIENT" ]]; then
  # Key rotation (round-1 finding 2): every artifact must be encrypted to
  # the CURRENT recipient before the old key may be destroyed — a matching
  # generation must not skip re-encryption.
  rebaseline=1
elif [[ "$INCS" -ge "$MAX_INCS" ]]; then
  rebaseline=1
elif [[ ! -d "$GEN_DIR" || ! -f "$CAPTURED" ]]; then
  rebaseline=1
fi

mkdir -p "$GEN_DIR"

# The current raw inventory, sorted. Captured membership is BY NAME (raw/
# is append-only custody; names are content-addressed queue names): a file
# is new iff its name is not in the captured set — correct under any
# arrival order (round-1 finding 1: queue-time name prefixes lag drain-time
# arrival, so no timestamp cutoff is sound).
CURRENT_LIST="$STAGING/.state/.current.$RUN_STAMP"
find "$RAW_DIR" -maxdepth 1 -type f -printf '%f\n' | LC_ALL=C sort > "$CURRENT_LIST"

tar_files_to() {
  # stdin: newline-separated file names relative to RAW_DIR; $1: output
  local out="$1"
  tar -C "$RAW_DIR" --verbatim-files-from -cf - -T - \
    | zstd -q -T0 -c \
    | age -r "$RECIPIENT" > "$out.tmp"
  mv "$out.tmp" "$out"
}

if [[ "$rebaseline" == 1 ]]; then
  rm -f "$GEN_DIR"/baseline.tar.zst.age "$GEN_DIR"/inc-*.tar.zst.age
  tar_files_to "$GEN_DIR/baseline.tar.zst.age" < "$CURRENT_LIST"
  command cp -f "$CURRENT_LIST" "$CAPTURED.tmp" && mv "$CAPTURED.tmp" "$CAPTURED"
  INCS=0
else
  NEW_LIST="$STAGING/.state/.current.$RUN_STAMP.new"
  LC_ALL=C comm -13 "$CAPTURED" "$CURRENT_LIST" > "$NEW_LIST"
  if [[ -s "$NEW_LIST" ]]; then
    tar_files_to "$GEN_DIR/inc-$RUN_STAMP.tar.zst.age" < "$NEW_LIST"
    # Captured set grows by exactly what was tarred; files deleted from
    # raw/ (purge) stay listed — harmless, and the next re-baseline resets.
    LC_ALL=C sort -u "$CAPTURED" "$NEW_LIST" > "$CAPTURED.tmp"
    mv "$CAPTURED.tmp" "$CAPTURED"
    INCS=$((INCS + 1))
  fi
  rm -f "$NEW_LIST"
fi
rm -f "$CURRENT_LIST"

# --- manifest + state ------------------------------------------------------
(
  cd "$STAGING"
  find db raw -type f -name '*.age' -printf '%p\n' | LC_ALL=C sort | while read -r f; do
    printf '%s\t%s\n' "$f" "$(sha256sum "$f" | cut -d' ' -f1)"
  done
) | jq -Rn --arg gen "$GEN" --arg at "$NOW_UTC" '
  {generation: ($gen|tonumber), created_at: $at,
   artifacts: [inputs | split("\t") | {name: .[0], sha256: .[1]}]}
' > "$STAGING/MANIFEST.json.tmp"
mv "$STAGING/MANIFEST.json.tmp" "$STAGING/MANIFEST.json"

jq -n --arg gen "$GEN" --arg recipient "$RECIPIENT" --arg incs "$INCS" --arg at "$NOW_UTC" \
  '{generation: ($gen|tonumber), recipient: $recipient, incrementals: ($incs|tonumber), last_run: $at}' \
  > "$STATE_FILE.tmp"
mv "$STATE_FILE.tmp" "$STATE_FILE"

echo "memoire-snapshot: generation=$GEN incrementals=$INCS rebaseline=$rebaseline staging=$STAGING"

# --- push (three phases; see header) ---------------------------------------
if [[ "$PUSH" == 1 ]]; then
  DEST="${MEMOIRE_SNAPSHOT_PUSH_DEST:-}"
  [[ -n "$DEST" ]] || { echo "memoire-snapshot: --push needs MEMOIRE_SNAPSHOT_PUSH_DEST" >&2; exit 1; }
  # Fail-closed guard (cdk8s review round-2 MED-2, kept as a general
  # defense): the historic prep placeholder is a valid LOCAL RELATIVE
  # rsync path — a "successful" push into an ephemeral filesystem.
  # Refuse loudly if any config surface still carries it.
  [[ "$DEST" != "SEAM-S5-UNSET" ]] || { echo "memoire-snapshot: push destination is the unresolved S5 seam placeholder — refusing (see cdk8s/main.ts seam S5)" >&2; exit 1; }
  # Local root push must not flip landing-zone ownership away from the
  # rrsync identity (seam-reconcile review MED-1): rsync -a run as root
  # preserves the root owner of staging files, which would lock the
  # non-root memoire-mirror writer out of its own zone and break the
  # hub host's rollback path. MEMOIRE_SNAPSHOT_CHOWN=uid:gid maps ownership
  # on receive (root-only rsync capability — exactly the local-CronJob
  # context that needs it). The sentinel refusal above is a GENERAL
  # fail-closed guard for any config surface that carries the historic
  # placeholder — the current cdk8s artifact ships a resolved dest.
  RSYNC_OWN=()
  [[ -n "${MEMOIRE_SNAPSHOT_CHOWN:-}" ]] && RSYNC_OWN=("--chown=$MEMOIRE_SNAPSHOT_CHOWN")
  # Phase 1: artifact sync — no deletions, manifest excluded. STABLE names
  # (db/, baseline) are updated IN PLACE here, so plain rsync would let an
  # interruption clobber bytes the old manifest still vouches for (round-2
  # F3). --delay-updates holds every transferred file in a temp area and
  # renames only after all transfers complete: a network drop leaves the
  # old set fully intact. Residual window: a crash DURING the final rename
  # batch (local-disk, sub-second) can mix sets — detectable by rule-7
  # hash verification, recoverable from the off-site pipeline's _archive
  # (the sweep archived the prior artifact bytes on its next pass).
  rsync -a "${RSYNC_OWN[@]}" --delay-updates --exclude=.state --exclude=MANIFEST.json "$STAGING"/ "$DEST"
  # Phase 2: the manifest flips authority to the new set (all its artifacts
  # are present after phase 1).
  rsync -a "${RSYNC_OWN[@]}" "$STAGING/MANIFEST.json" "$DEST"
  # Phase 3: prune stale files. An interruption leaves unlisted residue,
  # which the manifest authority rule ignores; the next push retries.
  rsync -a "${RSYNC_OWN[@]}" --delete --exclude=.state "$STAGING"/ "$DEST"
  echo "memoire-snapshot: pushed to $DEST"
fi
