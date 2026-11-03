#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# hl5-trigger-test.sh — regression tests for the nightly-HL5 early-fire gate
# and the nightly service's ExecCondition/ExecStartPost modes (memoire:D163;
# hardened per review rounds 1-2: exact threshold, duplicate tuples,
# wrapping quiet-hours boundaries, active/activating/timeout service
# states, file-based lane recency (success recorded only by
# --record-success), rolling 24h cooldown, anomaly-holds, and the
# --lane-fresh-check contract). R-10.3 (memoire:D165, round-2 ruling)
# additions: session-granular store confirmation via a fake
# `memoire-derive -list-outstanding` (stale markers filtered, empty list
# = 0 sessions, query failure and malformed output hold, unsafe
# identifiers skipped) and the --early-fire-check lane gate (cooldown,
# threshold, cooldown-record-on-pass, and the ruling-3a pin that the
# cooldown never gates --lane-fresh-check).

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TRIGGER="$ROOT/memoire-derive-hl5-trigger.sh"
command -v timeout >/dev/null || { echo "SKIP: timeout not available"; exit 0; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/home/log" "$WORK/bin"
LOG="$WORK/home/log/derive-auto.log"
CALLS="$WORK/systemctl-calls.log"
COOLDOWN="$WORK/home/log/.hl5-early-fire-epoch"
SUCCESS="$WORK/home/log/.hl5-lane-success-epoch"
touch "$CALLS"

fail() {
  echo "FAIL: $1" >&2
  echo "--- trigger log:" >&2; grep -E "hl5-trigger|nightly-skip" "$LOG" >&2 || true
  echo "--- systemctl calls:" >&2; cat "$CALLS" >&2 || true
  exit 1
}

# Fake systemctl driven by env: FAKE_STATE (is-active output; "timeout"
# simulates a hung client killed by the wrapper), FAKE_START_RC.
cat > "$WORK/bin/systemctl" <<FAKE
#!/usr/bin/env bash
args="\$*"
if [[ "\$args" == *"is-active"* ]]; then
  if [[ "\${FAKE_STATE:-inactive}" == "timeout" ]]; then sleep 30; fi
  echo "\${FAKE_STATE:-inactive}"
  [[ "\${FAKE_STATE:-inactive}" == "active" ]] && exit 0 || exit 3
fi
echo "SYSTEMCTL \$args" >> "$CALLS"
exit "\${FAKE_START_RC:-0}"
FAKE
chmod +x "$WORK/bin/systemctl"

# Fake memoire-derive for -list-outstanding (R-10.3 store confirmation).
# FAKE_OUTSTANDING_RC forces a query failure; FAKE_OUTSTANDING_FILE is
# printed verbatim when set; the default answer confirms EVERY marker in
# the sweep log as still-outstanding, which preserves the pre-R-10.3
# semantics of the legacy fixtures above it.
cat > "$WORK/bin/memoire-derive" <<FAKE
#!/usr/bin/env bash
[[ "\${FAKE_OUTSTANDING_RC:-0}" != "0" ]] && exit "\${FAKE_OUTSTANDING_RC}"
if [[ -n "\${FAKE_OUTSTANDING_FILE:-}" ]]; then cat "\${FAKE_OUTSTANDING_FILE}"; exit 0; fi
grep 'split_depth_exceeded' "\$MEMOIRE_HOME/log/derive-auto.log" 2>/dev/null \
  | awk '{p=""; s=""; for(i=1;i<=NF;i++){if(index(\$i,"project=")==1)p=substr(\$i,9); if(index(\$i,"session=")==1)s=substr(\$i,9)} if(p!=""&&s!="")printf "%s\037%s\n",p,s}' \
  | sort -u
FAKE
chmod +x "$WORK/bin/memoire-derive"

# seed_legs N [tuples] — N fresh split_depth_exceeded lines over `tuples`
# distinct (project, session, window) triples (default: all distinct).
seed_legs() {
  local n="$1" tuples="${2:-$1}" i t now
  now="$(date -Is)"
  : > "$LOG"
  for ((i = 0; i < n; i++)); do
    t=$((i % tuples))
    printf '%s sweep project=p%d session=s%d window=%d..%d rc=0 {"skipped":"split_depth_exceeded","prompt_bytes":120000,"depth":6}\n' \
      "$now" "$t" "$t" "$((1000 + t))" "$((2000 + t))" >> "$LOG"
  done
}

run_trigger() { # extra env...
  env PATH="$WORK/bin:$PATH" \
    MEMOIRE_HOME="$WORK/home" \
    MEMOIRE_AUTO_DERIVE=1 \
    MEMOIRE_HL5_TRIGGER_SESSIONS=50 \
    MEMOIRE_HL5_NOW_HOUR=23 \
    MEMOIRE_HL5_SYSTEMCTL_TIMEOUT=2 \
    MEMOIRE_DERIVE_BIN="$WORK/bin/memoire-derive" \
    "$@" bash "$TRIGGER"
}

early_check() { # expected-rc extra env... — runs --early-fire-check
  local want="$1"; shift
  set +e
  env PATH="$WORK/bin:$PATH" MEMOIRE_HOME="$WORK/home" MEMOIRE_AUTO_DERIVE=1 \
    MEMOIRE_HL5_TRIGGER_SESSIONS=50 MEMOIRE_HL5_SYSTEMCTL_TIMEOUT=2 \
    MEMOIRE_DERIVE_BIN="$WORK/bin/memoire-derive" \
    "$@" bash "$TRIGGER" --early-fire-check
  local rc=$?
  set -e
  [[ "$rc" == "$want" ]]
}

# --- below threshold: held, no start ----------------------------------------
seed_legs 10
run_trigger || fail "trigger must exit 0"
grep -q "hl5-trigger held sessions=10" "$LOG" || fail "held marker missing"
grep -q "SYSTEMCTL" "$CALLS" && fail "below-threshold must not start the service"

# --- exactly at threshold: held (strict >) ----------------------------------
seed_legs 50
run_trigger || fail "trigger must exit 0"
grep -q "hl5-trigger held sessions=50" "$LOG" || fail "exact threshold must hold (strict >)"
grep -q "SYSTEMCTL" "$CALLS" && fail "exact threshold must not fire"

# --- duplicate tuples count once --------------------------------------------
seed_legs 90 10   # 90 lines over 10 distinct tuples
run_trigger || fail "trigger must exit 0"
grep -q "hl5-trigger held sessions=10" "$LOG" || fail "duplicate tuples must dedupe to 10"

# --- 51 distinct legs in quiet hours: fires, cooldown epoch recorded --------
seed_legs 51
run_trigger || fail "trigger must exit 0"
grep -q "hl5-trigger fired sessions=51" "$LOG" || fail "fired marker missing"
grep -q "SYSTEMCTL --user start --no-block memoire-derive-sweep-hl5.service" "$CALLS" \
  || fail "service start missing"
[[ -s "$COOLDOWN" ]] || fail "cooldown epoch must be recorded after an accepted start"

# --- rolling 24h cooldown: one early fire per rolling day -------------------
: > "$CALLS"
seed_legs 60
run_trigger || fail "trigger must exit 0"
grep -q "SYSTEMCTL" "$CALLS" && fail "cooldown must block a second fire"
# 22h ago is still inside the 24h window (round-2 MED-NEW-1: the 00:05 ->
# 22:05 same-date double fire must be impossible).
echo "$(( $(date +%s) - 22 * 3600 ))" > "$COOLDOWN"
seed_legs 60
run_trigger || fail "trigger must exit 0"
grep -q "SYSTEMCTL" "$CALLS" && fail "22h-old cooldown must still block (24h rolling)"
# 25h ago no longer blocks.
echo "$(( $(date +%s) - 25 * 3600 ))" > "$COOLDOWN"
seed_legs 60
run_trigger || fail "trigger must exit 0"
grep -q "SYSTEMCTL --user start" "$CALLS" || fail "aged cooldown must allow a fire"
rm -f "$COOLDOWN"

# --- malformed cooldown state: anomaly holds --------------------------------
: > "$CALLS"; seed_legs 60
echo "not-an-epoch" > "$COOLDOWN"
run_trigger || fail "trigger must exit 0"
grep -q "held=cooldown_state_anomaly" "$LOG" || fail "cooldown anomaly marker missing"
grep -q "SYSTEMCTL" "$CALLS" && fail "malformed cooldown must hold"
rm -f "$COOLDOWN"

# --- quiet-hours boundaries for the real wrapping window 22 -> 7 ------------
for hour in 21 07; do
  : > "$CALLS"; seed_legs 60
  run_trigger MEMOIRE_HL5_NOW_HOUR="$hour" \
    MEMOIRE_HL5_QUIET_START=22 MEMOIRE_HL5_QUIET_END=7 || fail "trigger must exit 0"
  grep -q "SYSTEMCTL" "$CALLS" && fail "hour $hour must be outside quiet hours 22->7"
done
for hour in 22 06 00; do
  : > "$CALLS"; rm -f "$COOLDOWN"; seed_legs 60
  run_trigger MEMOIRE_HL5_NOW_HOUR="$hour" \
    MEMOIRE_HL5_QUIET_START=22 MEMOIRE_HL5_QUIET_END=7 || fail "trigger must exit 0"
  grep -q "SYSTEMCTL --user start" "$CALLS" || fail "hour $hour must be inside quiet hours 22->7"
done
rm -f "$COOLDOWN"

# --- active/activating service blocks; is-active timeout holds --------------
for st in active activating; do
  : > "$CALLS"; seed_legs 60
  run_trigger FAKE_STATE="$st" || fail "trigger must exit 0"
  grep -q "SYSTEMCTL" "$CALLS" && fail "state $st must block the fire"
done
: > "$CALLS"; seed_legs 60
run_trigger FAKE_STATE=timeout || fail "trigger must exit 0"
grep -q "held=is_active_timeout" "$LOG" || fail "is-active timeout must hold with a marker"
grep -q "SYSTEMCTL" "$CALLS" && fail "is-active timeout must not fire"

# --- file-based lane recency (round-2 HIGH-NEW-1) ---------------------------
# Recent recorded success holds; stale one does not; malformed holds.
: > "$CALLS"; seed_legs 60
echo "$(( $(date +%s) - 3600 ))" > "$SUCCESS"
run_trigger || fail "trigger must exit 0"
grep -q "SYSTEMCTL" "$CALLS" && fail "recent recorded lane success must hold"
: > "$CALLS"; seed_legs 60
echo "$(( $(date +%s) - 30 * 3600 ))" > "$SUCCESS"
run_trigger || fail "trigger must exit 0"
grep -q "SYSTEMCTL --user start" "$CALLS" || fail "stale recorded success must not hold"
rm -f "$COOLDOWN"
: > "$CALLS"; seed_legs 60
echo "garbage" > "$SUCCESS"
run_trigger || fail "trigger must exit 0"
grep -q "held=success_state_anomaly" "$LOG" || fail "success anomaly marker missing"
grep -q "SYSTEMCTL" "$CALLS" && fail "malformed success state must hold"
rm -f "$SUCCESS" "$COOLDOWN"

# --- start failure: no cooldown consumed, start_failed marker ---------------
: > "$CALLS"; seed_legs 60
run_trigger FAKE_START_RC=1 || fail "trigger must exit 0"
grep -q "hl5-trigger start_failed" "$LOG" || fail "start_failed marker missing"
[[ -e "$COOLDOWN" ]] && fail "failed start must not consume the cooldown"

# --- timestamp parse anomaly holds (round-2 LOW-NEW-2 guard) ----------------
: > "$CALLS"; seed_legs 60
printf 'BOGUS-TS sweep project=px session=sx window=1..2 rc=0 {"skipped":"split_depth_exceeded"}\n' >> "$LOG"
run_trigger || fail "trigger must exit 0"
grep -q "held=timestamp_parse_anomaly" "$LOG" || fail "parse anomaly marker missing"
grep -q "SYSTEMCTL" "$CALLS" && fail "parse anomaly must hold"

# --- kill switch ------------------------------------------------------------
: > "$CALLS"; seed_legs 200
run_trigger MEMOIRE_HL5_TRIGGER=0 || fail "trigger must exit 0"
grep -q "SYSTEMCTL" "$CALLS" && fail "disabled trigger must never fire"

# --- stale markers outside the window do not count --------------------------
: > "$CALLS"; : > "$LOG"
old="$(date -d '-3 hours' -Is)"
for i in $(seq 1 80); do
  printf '%s sweep project=old%d session=old%d window=1..2 rc=0 {"skipped":"split_depth_exceeded"}\n' \
    "$old" "$i" "$i" >> "$LOG"
done
run_trigger || fail "trigger must exit 0"
grep -q "SYSTEMCTL" "$CALLS" && fail "stale markers must not fire the trigger"

# --- --record-success + --lane-fresh-check contract -------------------------
rm -f "$SUCCESS"
env MEMOIRE_HOME="$WORK/home" MEMOIRE_AUTO_DERIVE=1 bash "$TRIGGER" --record-success \
  || fail "record-success must exit 0"
[[ -s "$SUCCESS" ]] || fail "record-success must write the epoch file"
grep -Eq '^[0-9]+$' "$SUCCESS" || fail "success file must hold a bare epoch"

check() { # expected-rc extra env...
  local want="$1"; shift
  set +e
  env PATH="$WORK/bin:$PATH" MEMOIRE_HOME="$WORK/home" MEMOIRE_AUTO_DERIVE=1 "$@" \
    bash "$TRIGGER" --lane-fresh-check
  local rc=$?
  set -e
  [[ "$rc" == "$want" ]]
}
# Fresh success recorded just above -> recency skip inside quiet hours.
check 1 MEMOIRE_HL5_NOW_HOUR=23 || fail "fresh-check right after a recorded success must skip"
echo "$(( $(date +%s) - 30 * 3600 ))" > "$SUCCESS"
check 0 MEMOIRE_HL5_NOW_HOUR=23 || fail "fresh-check with stale success must pass"
check 1 MEMOIRE_HL5_NOW_HOUR=12 || fail "fresh-check outside quiet hours must skip"
echo "garbage" > "$SUCCESS"
check 1 MEMOIRE_HL5_NOW_HOUR=23 || fail "fresh-check with malformed success state must skip"
rm -f "$SUCCESS"
check 0 MEMOIRE_HL5_NOW_HOUR=23 || fail "fresh-check with no recorded success must pass"

# --- global kill switch gates the whole lane (round-3 HIGH-NEW-1) -----------
: > "$CALLS"; seed_legs 200
run_trigger MEMOIRE_AUTO_DERIVE=0 || fail "trigger must exit 0"
grep -q "SYSTEMCTL" "$CALLS" && fail "MEMOIRE_AUTO_DERIVE=0 must gate the trigger"
check 1 MEMOIRE_HL5_NOW_HOUR=23 MEMOIRE_AUTO_DERIVE=0 \
  || fail "fresh-check under the global kill switch must skip"

# --- is-active anomaly (unrecognized state / query failure) holds -----------
: > "$CALLS"; seed_legs 60
run_trigger FAKE_STATE=garbage || fail "trigger must exit 0"
grep -q "held=is_active_anomaly" "$LOG" || fail "is-active anomaly marker missing"
grep -q "SYSTEMCTL" "$CALLS" && fail "unrecognized is-active state must hold"

# --- record-success persistence failure is loud (round-3 MED-NEW-1;
# round-5 LOW-NEW-1: the stderr diagnostic must be asserted — the file
# marker is unwritable in exactly this failure mode, so the journal line is
# the only surviving explanation) ---------------------------------------------
RO="$WORK/ro-home"; mkdir -p "$RO/log"; chmod 500 "$RO/log"
set +e
err_out="$(env MEMOIRE_HOME="$RO" MEMOIRE_AUTO_DERIVE=1 bash "$TRIGGER" --record-success 2>&1 >/dev/null)"
rc=$?
set -e
chmod 700 "$RO/log"
[[ "$rc" != "0" ]] || fail "record-success must fail loudly when it cannot persist"
[[ "$err_out" == *"could not be persisted"* ]] \
  || fail "record-success failure must reach stderr (journal) when the log dir is unwritable"

# --- scan-cap truncation can only under-count (safe direction) --------------
# Falsifying fixture (round-4 LOW-2): 60 distinct would-fire tuples first,
# then a 100-line suffix of only 10 repeated tuples. With the cap honored,
# only the suffix is counted (10 -> HELD); if a regression ignores the cap
# or counts the wrong portion, the 60 distinct legs fire and this fails.
: > "$CALLS"; : > "$LOG"
now="$(date -Is)"
for i in $(seq 1 60); do
  printf '%s sweep project=d%d session=d%d window=%d..%d rc=0 {"skipped":"split_depth_exceeded"}\n' \
    "$now" "$i" "$i" "$((3000 + i))" "$((4000 + i))" >> "$LOG"
done
for i in $(seq 1 100); do
  t=$((i % 10))
  printf '%s sweep project=r%d session=r%d window=%d..%d rc=0 {"skipped":"split_depth_exceeded"}\n' \
    "$now" "$t" "$t" "$((5000 + t))" "$((6000 + t))" >> "$LOG"
done
run_trigger MEMOIRE_HL5_SCAN_LINES=100 || fail "trigger must exit 0"
grep -q "hl5-trigger held sessions=10" "$LOG" || fail "cap must bound the counted suffix (held legs=10)"
grep -q "SYSTEMCTL" "$CALLS" && fail "truncated scan must not fire"

# --- R-10.3: store confirmation filters stale candidate legs ----------------
# 60 fresh markers, but the store confirms only 10 sessions -> held.
: > "$CALLS"; seed_legs 60
printf 'p%d\037s%d\n' 0 0 1 1 2 2 3 3 4 4 5 5 6 6 7 7 8 8 9 9 > "$WORK/partial-outstanding.txt"
run_trigger FAKE_OUTSTANDING_FILE="$WORK/partial-outstanding.txt" || fail "trigger must exit 0"
grep -q "hl5-trigger held sessions=10" "$LOG" || fail "store confirmation must filter to 10 legs"
grep -q "SYSTEMCTL" "$CALLS" && fail "store-filtered count must not fire"

# --- R-10.3: empty outstanding = all markers stale = legs 0 (MED-2 closed) --
: > "$CALLS"; seed_legs 200
: > "$WORK/empty-outstanding.txt"
run_trigger FAKE_OUTSTANDING_FILE="$WORK/empty-outstanding.txt" || fail "trigger must exit 0"
grep -q "hl5-trigger held sessions=0" "$LOG" || fail "empty outstanding must count 0 legs"
grep -q "SYSTEMCTL" "$CALLS" && fail "all-stale markers must never fire (MED-2)"

# --- R-10.3: outstanding query failure holds --------------------------------
: > "$CALLS"; seed_legs 60
run_trigger FAKE_OUTSTANDING_RC=1 || fail "trigger must exit 0"
grep -q "held=outstanding_query_failed" "$LOG" || fail "query-failure marker missing"
grep -q "SYSTEMCTL" "$CALLS" && fail "outstanding query failure must hold"

# --- R-10.3: malformed outstanding output holds -----------------------------
: > "$CALLS"; seed_legs 60
printf 'no-separator-line\n' > "$WORK/bad-outstanding.txt"
run_trigger FAKE_OUTSTANDING_FILE="$WORK/bad-outstanding.txt" || fail "trigger must exit 0"
grep -q "held=outstanding_parse_anomaly" "$LOG" || fail "parse-anomaly marker missing"
grep -q "SYSTEMCTL" "$CALLS" && fail "malformed outstanding must hold"

# --- R-10.3: spaced identifiers cannot alias a firing session (finding 3) ---
# 60 markers whose sessions embed spaces ("foo x1".."foo x60") plus one
# outstanding session "foo": with whitespace-tokenized matching every
# marker would falsely match "foo" and fire; injective matching finds no
# marker for "foo" -> held legs=0.
: > "$CALLS"; : > "$LOG"
now="$(date -Is)"
for i in $(seq 1 60); do
  printf '%s sweep project=p session=foo x%d window=%d..%d rc=0 {"skipped":"split_depth_exceeded"}\n' \
    "$now" "$i" "$((7000 + i))" "$((8000 + i))" >> "$LOG"
done
printf 'p\037foo\n' > "$WORK/alias-outstanding.txt"
run_trigger FAKE_OUTSTANDING_FILE="$WORK/alias-outstanding.txt" || fail "trigger must exit 0"
grep -q "hl5-trigger held sessions=0" "$LOG" || fail "spaced-session aliasing must confirm 0 legs"
grep -q "SYSTEMCTL" "$CALLS" && fail "aliased identifiers must not fire"

# --- R-10.3: unsafe store identifiers are skipped, safe direction -----------
# Outstanding rows with whitespace/backslash identifiers are skipped (an
# unconfirmed candidate under-counts); the skip leaves a marker.
: > "$CALLS"; seed_legs 60
{ printf 'p with space\037s0\n'; printf 'p1\037s\\1\n'; } > "$WORK/unsafe-outstanding.txt"
run_trigger FAKE_OUTSTANDING_FILE="$WORK/unsafe-outstanding.txt" || fail "trigger must exit 0"
grep -q "outstanding_unsafe_identifiers_skipped n=2" "$LOG" || fail "unsafe-skip marker missing"
grep -q "hl5-trigger held sessions=0" "$LOG" || fail "unsafe identifiers must not confirm legs"
grep -q "SYSTEMCTL" "$CALLS" && fail "unsafe identifiers must not fire"

# --- R-10.3 round-3 (F3 residual): a forged marker cannot match a SAFE
# outstanding pair. Markers whose session embeds another field's key
# ("x project=pN session=sN") would match outstanding pairs (pN, sN)
# under naive substring search; the ambiguity filter (unique
# project=/session=/window= keys per line) drops them -> sessions=0,
# with an observability marker. Threshold 3 so 4 forged matches WOULD
# fire if the filter regressed.
: > "$CALLS"; : > "$LOG"
now="$(date -Is)"
for i in 1 2 3 4; do
  printf '%s sweep project=px session=x project=p%d session=s%d window=1..2 rc=0 {"skipped":"split_depth_exceeded"}\n' \
    "$now" "$i" "$i" >> "$LOG"
done
printf 'p1\037s1\np2\037s2\np3\037s3\np4\037s4\n' > "$WORK/forge-outstanding.txt"
run_trigger MEMOIRE_HL5_TRIGGER_SESSIONS=3 FAKE_OUTSTANDING_FILE="$WORK/forge-outstanding.txt" \
  || fail "trigger must exit 0"
grep -q "ambiguous_markers_dropped n=4" "$LOG" || fail "ambiguity-drop marker missing"
grep -q "hl5-trigger held sessions=0" "$LOG" || fail "forged markers must confirm 0 sessions"
grep -q "SYSTEMCTL" "$CALLS" && fail "forged markers must not fire"

# --- R-10.3 round-2: session granularity closes the finding-2 overcount ----
# Session s0 has 3 distinct oversized windows and is the only outstanding
# session; s1..s59 are stale markers. Under the ratified session-granular
# metric this counts 1 (not 3 legs, not 60): covered-and-uncovered islands
# inside one session can no longer multiply the count.
: > "$CALLS"; : > "$LOG"
now="$(date -Is)"
for w in 1 2 3; do
  printf '%s sweep project=p0 session=s0 window=%d..%d rc=0 {"skipped":"split_depth_exceeded"}\n' \
    "$now" "$((9000 + w))" "$((9500 + w))" >> "$LOG"
done
for i in $(seq 1 59); do
  printf '%s sweep project=p%d session=s%d window=1..2 rc=0 {"skipped":"split_depth_exceeded"}\n' \
    "$now" "$i" "$i" >> "$LOG"
done
printf 'p0\037s0\n' > "$WORK/one-outstanding.txt"
run_trigger FAKE_OUTSTANDING_FILE="$WORK/one-outstanding.txt" || fail "trigger must exit 0"
grep -q "hl5-trigger held sessions=1" "$LOG" || fail "one outstanding session must count once (session granularity)"
grep -q "SYSTEMCTL" "$CALLS" && fail "1 session must not fire"

# --- R-10.3 round-2: --early-fire-check lane gate ---------------------------
# Threshold pass: 51 confirmed sessions, no cooldown -> exit 0, cooldown
# recorded, early-fire marker.
rm -f "$COOLDOWN"; seed_legs 51
early_check 0 || fail "51 sessions above threshold must pass the early gate"
grep -q "hl5-trigger early-fire sessions=51" "$LOG" || fail "early-fire marker missing"
[[ -s "$COOLDOWN" ]] || fail "early gate pass must record the cooldown"

# Fresh cooldown blocks the next tick (one fire per rolling day).
seed_legs 51
early_check 1 || fail "fresh cooldown must skip the early gate"

# Below/at threshold: clean skip with early-held marker.
rm -f "$COOLDOWN"; seed_legs 10
early_check 1 || fail "10 sessions must not pass threshold 50"
grep -q "hl5-trigger early-held sessions=10" "$LOG" || fail "early-held marker missing"

# PROVISIONAL default threshold 3 (round-2 ruling): 4 sessions pass, 3 hold.
# Empty env value falls through to the script default.
rm -f "$COOLDOWN"; seed_legs 4
early_check 0 MEMOIRE_HL5_TRIGGER_SESSIONS= || fail "4 sessions must pass the default threshold 3"
rm -f "$COOLDOWN"; seed_legs 3
early_check 1 MEMOIRE_HL5_TRIGGER_SESSIONS= || fail "3 sessions must hold at the default threshold 3"

# Anomalies and kill switches skip cleanly.
rm -f "$COOLDOWN"; seed_legs 51
early_check 1 FAKE_OUTSTANDING_RC=1 || fail "outstanding failure must skip the early gate"
grep -q "held=outstanding_query_failed" "$LOG" || fail "early-gate query-failure marker missing"
early_check 1 MEMOIRE_HL5_TRIGGER=0 || fail "narrow kill switch must skip the early gate"
early_check 1 MEMOIRE_AUTO_DERIVE=0 || fail "global kill switch must skip the early gate"
echo "not-an-epoch" > "$COOLDOWN"
early_check 1 || fail "malformed cooldown must skip the early gate"
grep -q "early-skip=cooldown_state_anomaly" "$LOG" || fail "early cooldown anomaly marker missing"
rm -f "$COOLDOWN"

# Ruling 3a pin: a consumed cooldown must NOT starve the nightly backstop —
# --lane-fresh-check never reads the cooldown file. Fresh cooldown, no
# recorded success: the nightly gate must still pass.
rm -f "$SUCCESS"
date +%s > "$COOLDOWN"
check 0 MEMOIRE_HL5_NOW_HOUR=23 || fail "lane-fresh-check must ignore the early-fire cooldown (ruling 3a)"
rm -f "$COOLDOWN"

# Unrecordable cooldown must NOT proceed (wake-churn guard): read-only log
# dir after seeding -> gate pass path fails closed with exit 1.
seed_legs 51
chmod 500 "$WORK/home/log"
early_check 1 || fail "unrecordable cooldown must skip the early gate"
chmod 700 "$WORK/home/log"

echo "hl5-trigger-test: OK"
