#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# Hermetic test for memoire-snapshot.sh (slice 2, memoire:D155): artifacts
# decrypt and restore; incremental membership is a set difference (late
# arrivals with OLD name prefixes are still captured — round-1 finding 1);
# a recipient change forces re-encryption of everything (finding 2); the
# push keeps the manifest authoritative (finding 3); a custody manifest
# with a non-numeric generation refuses before staging mutation (finding
# 4); a generation bump re-baselines; MAX_INCS bounds the chain; the
# manifest carries names+hashes only.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SNAP="$ROOT/scripts/memoire-snapshot.sh"
fail() { echo "snapshot-test FAIL: $*" >&2; exit 1; }

command -v age-keygen >/dev/null 2>&1 || { echo "snapshot-test: SKIP (age-keygen unavailable)"; exit 0; }

# Hermeticity: the script resolves MEMOIRE_DB from env then db.env (stale-
# SQLite guard, inbox 2026-08-03); the invoking machine's live values must
# not leak into the fixture runs.
unset MEMOIRE_DB MEMOIRE_DB_ENV_FILE

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
HOME_DIR="$WORK/home"
STAGING="$WORK/staging"
mkdir -p "$HOME_DIR/raw" "$HOME_DIR/custody"

KEY="$WORK/key.txt"
age-keygen -o "$KEY" 2>/dev/null
RECIPIENT="$(grep -oE 'age1[a-z0-9]+' "$KEY" | head -1)"
export MEMOIRE_SNAPSHOT_RECIPIENT="$RECIPIENT"

# Fixture store: real SQLite DB + timestamp-named raw files + generation 3.
sqlite3 "$HOME_DIR/memoire.db" "CREATE TABLE events(id INTEGER PRIMARY KEY, text TEXT); INSERT INTO events(text) VALUES ('snapshot needle one');"
echo '{"generation": 3}' > "$HOME_DIR/custody/MANIFEST.json"
printf '{"n":1}' > "$HOME_DIR/raw/1700000000001-adapter-aaa.json"
printf '{"n":2}' > "$HOME_DIR/raw/1700000000002-adapter-bbb.json"

run() { "$SNAP" --home "$HOME_DIR" --staging "$STAGING" "$@" >/dev/null; }
untar() { # $1 artifact, $2 dest dir, $3 key
  mkdir -p "$2"
  age -d -i "${3:-$KEY}" "$1" | zstd -q -d -c | tar -x -C "$2"
}

# --- run 1: baseline -------------------------------------------------------
run
[[ -f "$STAGING/db/memoire-db.db.zst.age" ]] || fail "missing DB artifact"
[[ -f "$STAGING/raw/gen-3/baseline.tar.zst.age" ]] || fail "missing baseline for generation 3"
[[ -f "$STAGING/MANIFEST.json" ]] || fail "missing manifest"

age -d -i "$KEY" "$STAGING/db/memoire-db.db.zst.age" | zstd -q -d -c > "$WORK/restored.db"
got="$(sqlite3 "$WORK/restored.db" "SELECT text FROM events;")"
[[ "$got" == "snapshot needle one" ]] || fail "restored DB content mismatch: $got"

untar "$STAGING/raw/gen-3/baseline.tar.zst.age" "$WORK/restore-raw"
[[ -f "$WORK/restore-raw/1700000000001-adapter-aaa.json" && -f "$WORK/restore-raw/1700000000002-adapter-bbb.json" ]] \
  || fail "baseline missing raw files"

jq -e '.generation == 3 and (.artifacts | length >= 2)' "$STAGING/MANIFEST.json" >/dev/null || fail "manifest shape"
name="$(jq -r '.artifacts[0].name' "$STAGING/MANIFEST.json")"
want="$(jq -r '.artifacts[0].sha256' "$STAGING/MANIFEST.json")"
gotsum="$(sha256sum "$STAGING/$name" | cut -d' ' -f1)"
[[ "$gotsum" == "$want" ]] || fail "manifest hash mismatch for $name"
grep -q "snapshot needle" "$STAGING/MANIFEST.json" && fail "manifest leaks content"

# --- run 2: no new raw files => no incremental -----------------------------
run
incs="$(find "$STAGING/raw/gen-3" -name 'inc-*.tar.zst.age' | wc -l)"
[[ "$incs" == 0 ]] || fail "expected no incremental without new files, got $incs"

# --- run 3: LATE ARRIVAL with an OLD name prefix must still be captured ----
# (round-1 finding 1: raw names carry queue-time stamps; a backlogged item
# can land in raw/ with a prefix OLDER than files already captured. A
# cutoff design skips it forever; set-difference membership must not.)
printf '{"n":0}' > "$HOME_DIR/raw/1600000000000-adapter-late.json"
printf '{"n":3}' > "$HOME_DIR/raw/1700000000005-adapter-ccc.json"
run
inc="$(find "$STAGING/raw/gen-3" -name 'inc-*.tar.zst.age')"
[[ -n "$inc" ]] || fail "expected an incremental after new raw files"
untar "$inc" "$WORK/restore-inc"
[[ -f "$WORK/restore-inc/1600000000000-adapter-late.json" ]] || fail "late arrival with old prefix was skipped"
[[ -f "$WORK/restore-inc/1700000000005-adapter-ccc.json" ]] || fail "incremental missing the new file"
[[ ! -f "$WORK/restore-inc/1700000000001-adapter-aaa.json" ]] || fail "incremental re-tars captured files"

# --- run 4: recipient change forces full re-encryption ---------------------
# (round-1 finding 2: after rotation, EVERY staged artifact must decrypt
# with the new key alone, or destroying the old key kills the restore chain.)
KEY2="$WORK/key2.txt"
age-keygen -o "$KEY2" 2>/dev/null
RECIPIENT2="$(grep -oE 'age1[a-z0-9]+' "$KEY2" | head -1)"
MEMOIRE_SNAPSHOT_RECIPIENT="$RECIPIENT2" run
[[ -z "$(find "$STAGING/raw/gen-3" -name 'inc-*.tar.zst.age')" ]] || fail "rotation must re-baseline (incs cleared)"
untar "$STAGING/raw/gen-3/baseline.tar.zst.age" "$WORK/restore-rot" "$KEY2"
[[ -f "$WORK/restore-rot/1600000000000-adapter-late.json" ]] || fail "post-rotation baseline incomplete"
if age -d -i "$KEY" "$STAGING/raw/gen-3/baseline.tar.zst.age" >/dev/null 2>&1; then
  fail "post-rotation baseline still decrypts with the OLD key"
fi
if age -d -i "$KEY" "$STAGING/db/memoire-db.db.zst.age" >/dev/null 2>&1; then
  fail "post-rotation DB artifact still decrypts with the OLD key"
fi
export MEMOIRE_SNAPSHOT_RECIPIENT="$RECIPIENT2" KEY="$KEY2"

# --- run 5: generation bump => old gen dropped, fresh baseline -------------
echo '{"generation": 4}' > "$HOME_DIR/custody/MANIFEST.json"
run
[[ ! -d "$STAGING/raw/gen-3" ]] || fail "old generation must leave staging on bump"
[[ -f "$STAGING/raw/gen-4/baseline.tar.zst.age" ]] || fail "missing re-baseline for generation 4"
[[ ! -f "$STAGING/.state/captured-gen-3.txt" ]] || fail "old generation captured set must be dropped"

# --- MAX_INCS retention: next run re-baselines and clears incrementals -----
printf '{"n":4}' > "$HOME_DIR/raw/1700000000009-adapter-ddd.json"
MEMOIRE_SNAPSHOT_MAX_INCS=1 run
[[ -n "$(find "$STAGING/raw/gen-4" -name 'inc-*.tar.zst.age')" ]] || fail "expected incremental before rollup"
printf '{"n":5}' > "$HOME_DIR/raw/1700000000010-adapter-eee.json"
MEMOIRE_SNAPSHOT_MAX_INCS=1 run
[[ -z "$(find "$STAGING/raw/gen-4" -name 'inc-*.tar.zst.age')" ]] || fail "rollup must clear incrementals"
[[ -f "$STAGING/raw/gen-4/baseline.tar.zst.age" ]] || fail "rollup must produce a baseline"

# --- corrupt custody manifests are a loud stop BEFORE staging mutation -----
before="$(find "$STAGING" -type f | LC_ALL=C sort | xargs sha256sum | sha256sum)"
for bad in 'not json' '{"generation":"three"}' '{"generation":null}' '{"generation":-2}' '{"generation":3.7}'; do
  echo "$bad" > "$HOME_DIR/custody/MANIFEST.json"
  if "$SNAP" --home "$HOME_DIR" --staging "$STAGING" >/dev/null 2>&1; then
    fail "custody manifest '$bad' must refuse to snapshot"
  fi
done
after="$(find "$STAGING" -type f | LC_ALL=C sort | xargs sha256sum | sha256sum)"
[[ "$before" == "$after" ]] || fail "refused runs must not mutate staging"

echo '{"generation": 4}' > "$HOME_DIR/custody/MANIFEST.json"

# --- same-second sequential runs must produce DISTINCT incrementals --------
# (round-2 finding N1: second-precision names collided, silently replacing
# the earlier incremental while the captured set claimed both were mirrored.)
printf '{"n":6}' > "$HOME_DIR/raw/1700000000011-adapter-fff.json"
run
printf '{"n":7}' > "$HOME_DIR/raw/1700000000012-adapter-ggg.json"
run
incs_count="$(find "$STAGING/raw/gen-4" -name 'inc-*.tar.zst.age' | wc -l)"
[[ "$incs_count" == 2 ]] || fail "same-second sequential runs must produce distinct incrementals, got $incs_count"
mkdir -p "$WORK/restore-ss"
for f in "$STAGING/raw/gen-4"/inc-*.tar.zst.age; do untar "$f" "$WORK/restore-ss"; done
[[ -f "$WORK/restore-ss/1700000000011-adapter-fff.json" && -f "$WORK/restore-ss/1700000000012-adapter-ggg.json" ]] \
  || fail "same-second incrementals lost a file"

# --- concurrent runs are refused (flock) -----------------------------------
(
  exec 9>"$STAGING/.state/lock"
  flock 9
  ! "$SNAP" --home "$HOME_DIR" --staging "$STAGING" >/dev/null 2>&1 \
    || { echo "snapshot-test FAIL: concurrent run must be refused while the lock is held" >&2; exit 1; }
) || fail "flock refusal check failed"

# --- push: manifest authority + .state never leaves ------------------------
echo '{"generation": 4}' > "$HOME_DIR/custody/MANIFEST.json"
DEST="$WORK/landing/"
MEMOIRE_SNAPSHOT_PUSH_DEST="$DEST" run --push
[[ -f "$WORK/landing/MANIFEST.json" ]] || fail "push missing manifest"
[[ ! -e "$WORK/landing/.state" ]] || fail ".state must never be pushed"
# Manifest authority invariant: every listed artifact exists with the
# listed hash in the landing zone.
jq -r '.artifacts[] | "\(.sha256)  \(.name)"' "$WORK/landing/MANIFEST.json" \
  | (cd "$WORK/landing" && sha256sum -c --quiet -) || fail "landing zone violates manifest authority"

# --- raw-only mode (Postgres era, HL3 relocation) --------------------------
# No SQLite file: default mode must refuse; --raw-only must run the raw leg
# and must NOT stage a DB artifact even when a stale memoire.db exists.
RO_HOME="$WORK/ro-home"; RO_STAGING="$WORK/ro-staging"
mkdir -p "$RO_HOME/raw" "$RO_HOME/custody"
printf '{"n":1}' > "$RO_HOME/raw/1700000000020-adapter-hhh.json"
cp "$HOME_DIR/snapshot-recipient.txt" "$RO_HOME/snapshot-recipient.txt" 2>/dev/null \
  || printf '%s\n' "$RECIPIENT" > "$RO_HOME/snapshot-recipient.txt"
! "$SNAP" --home "$RO_HOME" --staging "$RO_STAGING" >/dev/null 2>&1 \
  || fail "default mode must refuse when memoire.db is absent"
"$SNAP" --raw-only --home "$RO_HOME" --staging "$RO_STAGING" >/dev/null 2>&1 \
  || fail "--raw-only must succeed without a SQLite database"
find "$RO_STAGING/raw" -name 'baseline.tar.zst.age' | grep -q . \
  || fail "--raw-only must stage the raw baseline"
printf 'stale' > "$RO_HOME/memoire.db"
"$SNAP" --raw-only --home "$RO_HOME" --staging "$RO_STAGING" >/dev/null 2>&1 \
  || fail "--raw-only must succeed with a stale memoire.db present"
[[ ! -e "$RO_STAGING/db/memoire-db.db.zst.age" ]] \
  || fail "--raw-only must never stage a DB artifact (stale SQLite trap)"

# Round-2 MED-1: a DB artifact ALREADY IN STAGING (pre-cutover default-mode
# residue) must be purged — absent from the manifest AND the pushed tree.
printf 'stale-artifact' > "$RO_STAGING/db/memoire-db.db.zst.age"
RO_DEST="$WORK/ro-landing/"
MEMOIRE_SNAPSHOT_PUSH_DEST="$RO_DEST" "$SNAP" --raw-only --push \
  --home "$RO_HOME" --staging "$RO_STAGING" >/dev/null 2>&1 \
  || fail "--raw-only --push must succeed"
[[ ! -e "$RO_STAGING/db/memoire-db.db.zst.age" ]] \
  || fail "raw-only must purge a stale staged DB artifact"
! grep -q 'memoire-db' "$RO_STAGING/MANIFEST.json" \
  || fail "raw-only manifest must not list a DB artifact"
[[ ! -e "$WORK/ro-landing/db/memoire-db.db.zst.age" ]] \
  || fail "raw-only push tree must not carry a DB artifact"

# Round-2 MED-2: the cdk8s S5 placeholder is a valid local rsync dest —
# the script must refuse it loudly, never false-green into it.
! MEMOIRE_SNAPSHOT_PUSH_DEST="SEAM-S5-UNSET" "$SNAP" --raw-only --push \
    --home "$RO_HOME" --staging "$RO_STAGING" >/dev/null 2>&1 \
  || fail "S5 sentinel destination must be refused"

# --- stale-SQLite guard (inbox 2026-08-03, D166 hard-fail) -----------------
# Post-CNPG-cutover MEMOIRE_DB resolves to a Postgres DSN while the frozen
# pre-cutover memoire.db still exists (the D159 fallback left it in place).
# Default mode must REFUSE — never snapshot the stale file into a
# hash-verified green MANIFEST — and --raw-only must keep working.
PG_HOME="$WORK/pg-home"; PG_STAGING="$WORK/pg-staging"
mkdir -p "$PG_HOME/raw" "$PG_HOME/custody"
echo '{"generation": 0}' > "$PG_HOME/custody/MANIFEST.json"
printf '{"n":1}' > "$PG_HOME/raw/1700000000030-adapter-iii.json"
sqlite3 "$PG_HOME/memoire.db" "CREATE TABLE events(id INTEGER PRIMARY KEY, text TEXT); INSERT INTO events(text) VALUES ('frozen pre-cutover row');"
PG_DSN='postgres://memoire:secret@db.example:5433/memoire'

# (a) env DSN: default mode refuses, staging stays untouched.
! MEMOIRE_DB="$PG_DSN" "$SNAP" --home "$PG_HOME" --staging "$PG_STAGING" >/dev/null 2>&1 \
  || fail "default mode must refuse when MEMOIRE_DB (env) is a Postgres DSN"
refusal="$(MEMOIRE_DB="$PG_DSN" "$SNAP" --home "$PG_HOME" --staging "$PG_STAGING" 2>&1 >/dev/null || true)"
grep -qi 'postgres' <<<"$refusal" || fail "DSN refusal must name the Postgres cause"
[[ ! -e "$PG_STAGING/db/memoire-db.db.zst.age" ]] \
  || fail "refused DSN run must not stage a DB artifact"

# (b) db.env DSN (env unset): same refusal — the sweep's fallback source
# is honored, not just the env var. postgresql:// prefix covered here.
printf 'MEMOIRE_DB=postgresql://memoire:secret@db.example:5433/memoire\n' > "$PG_HOME/db.env"
! "$SNAP" --home "$PG_HOME" --staging "$PG_STAGING" >/dev/null 2>&1 \
  || fail "default mode must refuse when db.env carries a Postgres DSN"

# (c) --raw-only with a DSN resolved: the ruled Postgres-era mode still runs.
MEMOIRE_DB="$PG_DSN" "$SNAP" --raw-only --home "$PG_HOME" --staging "$PG_STAGING" >/dev/null 2>&1 \
  || fail "--raw-only must succeed when MEMOIRE_DB is a Postgres DSN"
find "$PG_STAGING/raw" -name 'baseline.tar.zst.age' | grep -q . \
  || fail "--raw-only under a DSN must stage the raw baseline"

# (d) a SQLite MEMOIRE_DB value overrides the default path: the resolved
# store-of-record is snapshotted, not whatever sits at memoire.db.
rm -f "$PG_HOME/db.env"; rm -rf "$PG_STAGING"
ALT_DB="$WORK/alt-store.db"
sqlite3 "$ALT_DB" "CREATE TABLE events(id INTEGER PRIMARY KEY, text TEXT); INSERT INTO events(text) VALUES ('resolved store row');"
MEMOIRE_DB="$ALT_DB" "$SNAP" --home "$PG_HOME" --staging "$PG_STAGING" >/dev/null 2>&1 \
  || fail "default mode must accept a SQLite MEMOIRE_DB value"
age -d -i "$KEY" "$PG_STAGING/db/memoire-db.db.zst.age" | zstd -q -d -c > "$WORK/restored-alt.db"
got="$(sqlite3 "$WORK/restored-alt.db" "SELECT text FROM events;")"
[[ "$got" == "resolved store row" ]] \
  || fail "DB leg must snapshot the resolved MEMOIRE_DB store, got: $got"

# (d2) a readable db.env WITHOUT a MEMOIRE_DB line is not an error (r1 F1:
# grep's miss killed the script under -e + pipefail — in every mode).
printf 'OTHER=value\n' > "$PG_HOME/db.env"
sqlite3 "$PG_HOME/memoire.db" "SELECT 1;" >/dev/null # frozen file still present
"$SNAP" --home "$PG_HOME" --staging "$PG_STAGING" >/dev/null 2>&1 \
  || fail "default mode must run when db.env has no MEMOIRE_DB line"
"$SNAP" --raw-only --home "$PG_HOME" --staging "$PG_STAGING" >/dev/null 2>&1 \
  || fail "--raw-only must run when db.env has no MEMOIRE_DB line"

# (e) env wins over db.env (sweep precedence): a stale SQLite path in
# db.env must not shadow the env DSN's refusal.
printf 'MEMOIRE_DB=%s\n' "$ALT_DB" > "$PG_HOME/db.env"
! MEMOIRE_DB="$PG_DSN" "$SNAP" --home "$PG_HOME" --staging "$PG_STAGING" >/dev/null 2>&1 \
  || fail "env DSN must take precedence over a SQLite path in db.env"
rm -f "$PG_HOME/db.env"

# Round-2 LOW-NEW-1: the help contract must advertise the mode.
# No -q: early grep exit would close the pipe and, under pipefail, turn
# the help printer's SIGPIPE (141) into a timing-dependent flake.
"$SNAP" -h | grep -- '--raw-only' >/dev/null || fail "-h must document --raw-only"

# Ownership-mapping contract (seam-reconcile MED-1 falsifier, no root
# needed): a PATH-first rsync wrapper records argv and delegates; with
# MEMOIRE_SNAPSHOT_CHOWN set every push phase must carry --chown, and
# with it unset none may.
RSYNC_LOG="$WORK/rsync-argv.log"
mkdir -p "$WORK/shim"
REAL_RSYNC="$(command -v rsync)"
cat > "$WORK/shim/rsync" <<SHIM
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$RSYNC_LOG"
exec "$REAL_RSYNC" "\$@"
SHIM
chmod +x "$WORK/shim/rsync"
: > "$RSYNC_LOG"
MEMOIRE_SNAPSHOT_CHOWN="$(id -u):$(id -g)" MEMOIRE_SNAPSHOT_PUSH_DEST="$RO_DEST"   PATH="$WORK/shim:$PATH" "$SNAP" --raw-only --push   --home "$RO_HOME" --staging "$RO_STAGING" >/dev/null 2>&1   || fail "chown-mapped push must succeed (self uid:gid is a no-op chown)"
[[ "$(wc -l < "$RSYNC_LOG")" == 3 ]] || fail "expected exactly 3 rsync phases, got $(wc -l < "$RSYNC_LOG")"
[[ "$(grep -c -- '--chown=' "$RSYNC_LOG")" == 3 ]] \
  || fail "every push phase must carry --chown when MEMOIRE_SNAPSHOT_CHOWN is set"
: > "$RSYNC_LOG"
MEMOIRE_SNAPSHOT_CHOWN= MEMOIRE_SNAPSHOT_PUSH_DEST="$RO_DEST" PATH="$WORK/shim:$PATH" "$SNAP" --raw-only --push   --home "$RO_HOME" --staging "$RO_STAGING" >/dev/null 2>&1   || fail "unmapped push must still succeed"
! grep -q -- '--chown=' "$RSYNC_LOG"   || fail "no push phase may carry --chown when MEMOIRE_SNAPSHOT_CHOWN is unset"

# --- raw cold archive: parts are immutable custody files --------------------
# (internal/rawstore; cold-archive review R1) The reachable sequence: a day
# is packed and snapshotted; a late drain lands in that day; the packer packs
# it into a NEW part and deletes the flat file before any snapshot saw it.
# The next snapshot must capture the new part, and restoring every artifact
# must reproduce the live store exactly (memoire-raw-pack --verify digest).
# The custody lock (beside raw/) is never captured and is waited on.
PK_HOME="$WORK/pk-home"; PK_STAGING="$WORK/pk-staging"; PK_BIN="$WORK/memoire-raw-pack"
mkdir -p "$PK_HOME/raw" "$PK_HOME/custody"
(cd "$ROOT" && go build -o "$PK_BIN" ./services/api/cmd/rawpack) || fail "build memoire-raw-pack"
echo '{"generation": 1}' > "$PK_HOME/custody/MANIFEST.json"
printf '{"n":1}' > "$PK_HOME/raw/1700000000001-http-aaa.json"
printf '{"n":3}' > "$PK_HOME/raw/1700000000003-http-ccc.json"
"$PK_BIN" --home "$PK_HOME" --apply >/dev/null || fail "pack day 2023-11-14"
[[ -f "$PK_HOME/raw/pack-2023-11-14.000.tar.zst" ]] || fail "part .000 missing"
"$SNAP" --raw-only --home "$PK_HOME" --staging "$PK_STAGING" >/dev/null || fail "snapshot after first pack"
printf '{"n":2,"late":true}' > "$PK_HOME/raw/1700000000002-http-bbb.json"
"$PK_BIN" --home "$PK_HOME" --apply >/dev/null || fail "pack the late drain"
[[ -f "$PK_HOME/raw/pack-2023-11-14.001.tar.zst" ]] || fail "late drain must land in a NEW part .001"
[[ ! -f "$PK_HOME/raw/1700000000002-http-bbb.json" ]] || fail "late flat file should be packed and deleted"
"$SNAP" --raw-only --home "$PK_HOME" --staging "$PK_STAGING" >/dev/null || fail "snapshot after late pack"
PK_RESTORE="$WORK/pk-restore"; mkdir -p "$PK_RESTORE/raw"
for art in "$PK_STAGING"/raw/gen-1/baseline.tar.zst.age "$PK_STAGING"/raw/gen-1/inc-*.tar.zst.age; do
  [[ -f "$art" ]] && untar "$art" "$PK_RESTORE/raw"
done
[[ -f "$PK_RESTORE/raw/pack-2023-11-14.001.tar.zst" ]] || fail "the new part was not captured"
live="$("$PK_BIN" --home "$PK_HOME" --verify | jq -r '.digest + " " + (.envelopes|tostring)')"
restored="$("$PK_BIN" --home "$PK_RESTORE" --verify | jq -r '.digest + " " + (.envelopes|tostring)')"
[[ "$live" == "$restored" && "$live" == *" 3" ]] || fail "restored custody differs from live: $restored vs $live"
# Review N1: the documented rollback (memoire-raw-pack --unpack) advances the
# custody generation BEFORE the part name can be reused. Sequence: unpack the
# snapshotted part .000 → late drain into that day → repack (the name .000 is
# reused) → snapshot → restore must equal live.
"$PK_BIN" --home "$PK_HOME" --unpack pack-2023-11-14.001.tar.zst >/dev/null || fail "unpack part .001"
"$PK_BIN" --home "$PK_HOME" --unpack pack-2023-11-14.000.tar.zst >/dev/null || fail "unpack part .000"
printf '{"n":4,"late":true}' > "$PK_HOME/raw/1700000000004-http-ddd.json"
"$PK_BIN" --home "$PK_HOME" --apply >/dev/null || fail "repack after rollback"
[[ -f "$PK_HOME/raw/pack-2023-11-14.000.tar.zst" ]] || fail "the repack should reuse part name .000 (the case under test)"
"$SNAP" --raw-only --home "$PK_HOME" --staging "$PK_STAGING" >/dev/null || fail "snapshot after rollback + repack"
PK_RESTORE2="$WORK/pk-restore2"; mkdir -p "$PK_RESTORE2/raw"
PK_GEN="$(jq -r .generation "$PK_HOME/custody/MANIFEST.json")"
for art in "$PK_STAGING/raw/gen-$PK_GEN"/baseline.tar.zst.age "$PK_STAGING/raw/gen-$PK_GEN"/inc-*.tar.zst.age; do
  [[ -f "$art" ]] && untar "$art" "$PK_RESTORE2/raw"
done
live="$("$PK_BIN" --home "$PK_HOME" --verify | jq -r '.digest + " " + (.envelopes|tostring)')"
restored="$("$PK_BIN" --home "$PK_RESTORE2" --verify | jq -r '.digest + " " + (.envelopes|tostring)')"
[[ "$live" == "$restored" && "$live" == *" 4" ]] || fail "restore after rollback + name reuse differs from live: $restored vs $live"
[[ "$PK_GEN" == 3 ]] || fail "each unpack must advance the custody generation (got $PK_GEN)"
[[ -f "$PK_HOME/.raw.lock" ]] || fail "snapshot must take the raw custody lock beside raw/"
find "$PK_RESTORE/raw" "$PK_RESTORE2/raw" -name "*.lock" | grep -q . && fail "the custody lock must never be captured"
(
  exec 7>>"$PK_HOME/.raw.lock"
  flock 7
  ! MEMOIRE_SNAPSHOT_LOCK_WAIT_S=1 "$SNAP" --raw-only --home "$PK_HOME" --staging "$PK_STAGING" >/dev/null 2>"$WORK/pk-lock.err" \
    || { echo "snapshot-test FAIL: snapshot must wait on, then refuse, a held raw custody lock" >&2; exit 1; }
  grep -q "raw custody lock busy" "$WORK/pk-lock.err" \
    || { echo "snapshot-test FAIL: refusal must name the raw custody lock" >&2; exit 1; }
) || fail "raw custody lock check failed"

echo "snapshot-test: OK"
