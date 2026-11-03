#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# sweep-split-test.sh — regression test for memoire-derive-sweep.sh's adaptive
# window split (operator brief 2026-07-10, amendment 4). No live provider, no
# real memoire-derive: a fake binary reports prompt sizes, serves the
# -list-targets/-median-epoch sweep helpers (derive-seam-port: the script no
# longer opens the store itself), and records execute argv.
#
# Covered: oversized window splits at the median into [lo,mid]/[mid+1,hi];
# right-sized windows execute whole (no pinned epochs); an always-oversized
# window terminates on markers with ZERO executes; ids containing a single
# quote survive the argv path; a missing key exits without touching anything;
# all-executed-legs-failed exits non-zero with a marker (single leg and
# across a split recursion) while partial failure stays green regardless of
# WHICH leg failed (D166 ruling 1); the run-STATUS layer feeds the same
# gate — a leg recording status="failed" at rc=0 counts as failed, all
# such legs make the lane red, one surviving success keeps it green, and
# rc-failures and status-failures aggregate across a split (L4(a),
# memoire:D198 — the 89-failures class); the derive binary sees the
# repo-anchored MEMOIRE_SCHEMA_DIR with explicit override winning (D166
# ruling 2a).

SWEEP="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/memoire-derive-sweep.sh"
command -v flock >/dev/null || { echo "SKIP: flock not available"; exit 0; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/home/log" "$WORK/bin"
EXEC_LOG="$WORK/exec.log"
touch "$EXEC_LOG"

fail() {
  echo "FAIL: $1" >&2
  echo "--- sweep log:" >&2
  cat "$WORK/home/log/derive-auto.log" >&2 || true
  echo "--- exec calls:" >&2
  cat "$EXEC_LOG" >&2 || true
  exit 1
}

# Fake memoire-derive: -dry-run reports prompt_bytes from the FAKE_MODE
# policy; -execute appends its window to EXEC_LOG; -list-targets and
# -median-epoch serve the sweep helpers from the fixture (one session,
# epochs 1000..1900 step 100), mirroring the real CLI's framing. Output
# shape mirrors the real CLI's MarshalIndent (two-space top-level indent).
cat > "$WORK/bin/fake-derive" <<'FAKE'
#!/usr/bin/env bash
mode="" lo=0 hi=0 prov=0 nosup=0
args=("$@")
for ((i=0; i<${#args[@]}; i++)); do
  case "${args[i]}" in
    -dry-run) mode=dry ;;
    -execute) mode=exec ;;
    -list-targets) mode=list ;;
    -list-open-intents) mode=intents ;;
    -settle-intents) mode=settle ;;
    -median-epoch) mode=median ;;
    -cut-points) mode=cuts ;;
    -start-epoch) lo="${args[i+1]}" ;;
    -end-epoch) hi="${args[i+1]}" ;;
    # Fibery #344 flags, recorded on the EXEC line below so the assertions can
    # pin exactly which legs carry them.
    -provisional) prov=1 ;;
    -no-supersede) nosup=1 ;;
  esac
done
if [[ "$mode" == cuts ]]; then
  # Rhythm cuts (Fibery #343). FAKE_CUTS is the canned newline-separated cut
  # list; unset means "no rhythm cut", which is what every pre-#343 case in
  # this file expects. Unrecognized flags must NEVER fall through to the
  # dry-run/execute branch below: that is how a new CLI helper silently forges
  # an EXEC line and breaks the leg-count assertions.
  [[ "${FAKE_CUTS:-}" == cuts-fail ]] && { echo "boom: cut detector unreachable" >&2; exit 1; }
  [[ -n "${FAKE_CUTS:-}" ]] && printf '%s\n' ${FAKE_CUTS}
  exit 0
fi
if [[ "$mode" == list ]]; then
  [[ "${FAKE_MODE:-}" == list-fail ]] && { echo "boom: store unreachable" >&2; exit 1; }
  # Record the schema env once per sweep (list runs exactly once): the
  # repo-anchored MEMOIRE_SCHEMA_DIR export is load-bearing (D166 ruling
  # 2a — the relative default silently no-ops migration under timer cwd).
  printf 'SCHEMA %s\n' "${MEMOIRE_SCHEMA_DIR:-unset}" >> "$EXEC_LOG"
  printf 'bigproj\x1f%s\n' "sess'big"
  exit 0
fi
if [[ "$mode" == intents ]]; then
  exit 0
fi
if [[ "$mode" == settle ]]; then
  echo "settled=0"
  exit 0
fi
if [[ "$mode" == median ]]; then
  eligible=()
  for ep in 1000 1100 1200 1300 1400 1500 1600 1700 1800 1900; do
    (( ep >= lo && ep <= hi )) && eligible+=("$ep")
  done
  n=${#eligible[@]}
  (( n > 0 )) && printf '%s\n' "${eligible[$(( (n - 1) / 2 ))]}"
  exit 0
fi
if [[ "$mode" == dry ]]; then
  # Resolve each unpinned edge from the fixture independently, the way the
  # real CLI resolves window_start/window_end from the fetched event range.
  # Half-pinned windows are legal with -session-id and are exactly what a
  # rhythm pass produces for its first and last segments.
  (( lo == 0 )) && lo=1000
  (( hi == 0 )) && hi=1900
  case "$FAKE_MODE" in
    huge) pb=200000 ;;
    split-once|exec-fail-left|exec-fail-right|exec-fail-split|status-fail-left|status-mixed)
      if (( hi - lo >= 900 )); then pb=200000; else pb=50000; fi ;;
    rhythm-oversize)
      if (( hi - lo >= 400 )); then pb=200000; else pb=50000; fi ;;
    *) pb=50000 ;;
  esac
  printf '{\n  "prompt_bytes": %s,\n  "window_start_epoch": %s,\n  "window_end_epoch": %s\n}\n' "$pb" "$lo" "$hi"
else
  echo "EXEC lo=$lo hi=$hi prov=$prov nosup=$nosup" >> "$EXEC_LOG"
  # Run-status layer (L4(a)): a recorded failed run prints its status and
  # exits 0 by documented policy — the fake mirrors that exact shape.
  # duplicate:true + failed is the retry-CAS-loser shape (review round-1
  # finding 3): stale failed status while another caller retries.
  if [[ "$FAKE_MODE" == status-dup-fail ]]; then
    printf '{\n  "status": "failed",\n  "duplicate": true,\n  "written_count": 0\n}\n'
    exit 0
  fi
  st=completed
  case "$FAKE_MODE" in
    status-fail) st=failed ;;
    status-fail-left) if (( lo == 1000 )); then st=failed; fi ;;
    status-mixed) if (( lo == 1401 )); then st=failed; fi ;;
  esac
  printf '{\n  "status": "%s",\n  "written_count": 1\n}\n' "$st"
  case "$FAKE_MODE" in
    exec-fail|exec-fail-split) exit 1 ;;
    exec-fail-left) if (( lo == 1000 )); then exit 1; fi ;;
    exec-fail-right) if (( lo == 1401 )); then exit 1; fi ;;
    status-mixed) if (( lo == 1000 )); then exit 1; fi ;;
  esac
  exit 0
fi
FAKE
chmod +x "$WORK/bin/fake-derive"

# No-op capture adapter: this test is about the window split, not the
# foreign-harness pre-pass (live-turn-ingest-test.sh covers that) — the real
# adapters must not scan the machine's actual rollout dirs from a test.
printf '#!/usr/bin/env bash\nexit 0\n' > "$WORK/bin/noop-capture"
chmod +x "$WORK/bin/noop-capture"

# Fixture lives in fake-derive (epochs 1000..1900, median 1400); the session
# id carries a single quote to exercise the raw-argv contract end to end.
# Ambient-immunity: MEMOIRE_AUTO_DERIVE=1 and MEMOIRE_DB= are pinned so an
# operator kill switch or a live DSN in the invoking shell can never leak
# into the harness (bit live 2026-07-22: the cutover-gap kill switch in the
# session env silently no-oped every case).
run_sweep() { # FAKE_MODE  (SCHEMA_OVERRIDE env, if set, becomes the
              # explicit MEMOIRE_SCHEMA_DIR — empty pin means "unset" so
              # an ambient value in the invoking shell can never leak in;
              # FAKE_CUTS, if set, is the fake's canned rhythm cut list)
  : > "$WORK/home/log/derive-auto.log"
  : > "$EXEC_LOG"
  FAKE_MODE="$1" EXEC_LOG="$EXEC_LOG" \
  FAKE_CUTS="${FAKE_CUTS:-}" \
  MEMOIRE_HOME="$WORK/home" \
  MEMOIRE_AUTO_DERIVE=1 \
  MEMOIRE_DB= \
  MEMOIRE_SCHEMA_DIR="${SCHEMA_OVERRIDE:-}" \
  MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
  MEMOIRE_CAPTURE_CODEX_BIN="$WORK/bin/noop-capture" \
  MEMOIRE_CAPTURE_QWEN_BIN="$WORK/bin/noop-capture" \
  MEMOIRE_CAPTURE_OPENCODE_BIN="$WORK/bin/noop-capture" \
  MEMOIRE_DERIVE_KEY_ENV=HOME \
  MEMOIRE_SWEEP_HOURS=99999999 \
  MEMOIRE_SWEEP_RHYTHM_CUTS="${RHYTHM_OFF:-1}" \
  MEMOIRE_SWEEP_MAX_SPLIT_DEPTH="${SPLIT_DEPTH:-6}" \
  MEMOIRE_SWEEP_SUPERSEDE="${SUPERSEDE:-1}" \
  bash "$SWEEP"
}
LOG="$WORK/home/log/derive-auto.log"

# Case 1: right-sized window executes whole, without pinned epochs. The
# derive binary must also see the repo-anchored schema dir (D166 ruling
# 2a): without the export, timer-cwd invocations silently no-op migration.
EXPECTED_SCHEMA="$(cd "$(dirname "$SWEEP")/.." && pwd)/schema"
run_sweep small
grep -q "EXEC lo=0 hi=0" "$EXEC_LOG" || fail "right-sized window must execute whole (unpinned)"
grep -qxF "SCHEMA $EXPECTED_SCHEMA" "$EXEC_LOG" \
  || fail "derive binary must see the repo-anchored MEMOIRE_SCHEMA_DIR"
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 1 ]] || fail "right-sized window must execute exactly once"

# Case 1b: an explicit MEMOIRE_SCHEMA_DIR override wins over the anchor.
SCHEMA_OVERRIDE="$WORK/custom-schema" run_sweep small
grep -qxF "SCHEMA $WORK/custom-schema" "$EXEC_LOG" \
  || fail "explicit MEMOIRE_SCHEMA_DIR must win over the repo anchor"

# Case 2: oversized window splits at the median (1400) into two executed legs.
run_sweep split-once
grep -q '"split":true' "$LOG" || fail "split marker missing"
grep -q '"mid":1400' "$LOG" || fail "median split point must be 1400"
grep -q "EXEC lo=1000 hi=1400" "$EXEC_LOG" || fail "left leg [1000,1400] not executed"
grep -q "EXEC lo=1401 hi=1900" "$EXEC_LOG" || fail "right leg [1401,1900] not executed"
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 2 ]] || fail "split must execute exactly two legs"
grep -q "session=sess'big" "$LOG" || fail "quoted session id must survive into the log"

# Case 3: always-oversized terminates on markers with zero provider calls.
run_sweep huge
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 0 ]] || fail "always-oversized must never execute"
grep -q '"skipped":"unsplittable_window"\|"skipped":"split_depth_exceeded"' "$LOG" \
  || fail "always-oversized must end on skip markers"

# Case 5 (D166 ruling 1): a sweep whose EVERY executed leg fails exits
# non-zero with the all-legs-failed marker, so the HL5 lane's
# ExecStartPost success-epoch record is skipped (the 42P01 window
# recorded lane success over 100% leg failure).
if run_sweep exec-fail; then fail "all-legs-failed sweep must exit non-zero"; fi
grep -q 'all-legs-failed attempted=1' "$LOG" || fail "all-legs-failed marker missing"

# Case 5b: a split where BOTH legs fail aggregates across the recursion —
# exit non-zero with attempted=2 (pins counter persistence through
# derive_window recursion, not just the single-leg case).
if run_sweep exec-fail-split; then fail "both-legs-failed split must exit non-zero"; fi
grep -q 'all-legs-failed attempted=2' "$LOG" || fail "split all-fail must report attempted=2"

# Case 6: partial failure still exits zero — progress was made, stragglers
# retry next tick; the all-fail marker must not appear.
run_sweep exec-fail-left || fail "partial-failure sweep must exit zero"
grep -q "EXEC lo=1401 hi=1900" "$EXEC_LOG" || fail "surviving leg must still execute"
! grep -q 'all-legs-failed' "$LOG" || fail "partial failure must not emit the all-fail marker"

# Case 6b: the FAILING leg being the FINAL one must still exit zero — kills
# any implementation that gates on the last leg's status instead of the
# aggregate counters (a last-status gate passes Cases 5 and 6 but marks
# this real-progress sweep all-failed).
run_sweep exec-fail-right || fail "final-leg failure with earlier success must exit zero"
grep -q "EXEC lo=1000 hi=1400" "$EXEC_LOG" || fail "successful earlier leg must have executed"
! grep -q 'all-legs-failed' "$LOG" || fail "final-leg failure must not emit the all-fail marker"

# Case 8 (L4(a), memoire:D198 — run-status honesty): a single leg that
# records status="failed" at rc=0 (the 89-failures shape: provider down,
# exit 0 by documented policy) is a failed lane — exit non-zero with the
# marker carrying the layer split. Falsified against the pre-L4 sweep:
# rc=0 legs were unconditionally successes and this sweep exited 0.
if run_sweep status-fail; then fail "all-status-failed sweep must exit non-zero"; fi
grep -q 'all-legs-failed attempted=1 rc_failed=0 status_failed=1' "$LOG" \
  || fail "status-layer all-fail marker missing or miscounted"

# Case 8b: one leg status-failed, the other completed — partial success
# stays green by design (ruled scope), no all-fail marker.
run_sweep status-fail-left || fail "partial status-failure must exit zero"
grep -q "EXEC lo=1401 hi=1900" "$EXEC_LOG" || fail "surviving leg must still execute"
! grep -q 'all-legs-failed' "$LOG" || fail "partial status-failure must not emit the all-fail marker"

# Case 8c: failures aggregate ACROSS layers — left leg fails at rc=1,
# right leg records status="failed" at rc=0: every executed leg failed
# somewhere, the lane must be red with both counters populated.
if run_sweep status-mixed; then fail "mixed-layer all-fail sweep must exit non-zero"; fi
grep -q 'all-legs-failed attempted=2 rc_failed=1 status_failed=1' "$LOG" \
  || fail "mixed-layer all-fail marker missing or miscounted"

# Case 8d (review round-1 finding 3): a duplicate:true leg carrying the
# STALE failed status (the failed->running retry-CAS loser — another
# caller is live-retrying that run) is NOT this lane's failure: the lane
# stays green and the all-fail marker must not appear.
run_sweep status-dup-fail || fail "duplicate-failed leg must not red the lane"
! grep -q 'all-legs-failed' "$LOG" || fail "duplicate-failed leg must not emit the all-fail marker"

# Case 7 (preflight audit finding 3): mandatory enumeration failure must
# exit NON-ZERO after its marker — a zero exit here records HL5 lane
# success over zero enumerated work (the D166 honesty class).
if run_sweep list-fail; then fail "list_targets failure must exit non-zero"; fi
grep -q 'skipped=list_targets_failed' "$LOG" || fail "list_targets_failed marker missing"
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 0 ]] || fail "failed enumeration must not execute legs"

# ---------------------------------------------------------------------------
# Rhythm cuts (Fibery #343). The pass runs BEFORE the ceiling check, so these
# cases pin that cuts are the first splitter and halving stays the fallback.
# ---------------------------------------------------------------------------

# Case R1: one rhythm cut yields two segments, with the OUTER edges left
# unpinned — the first segment has no lower bound and the last no upper one, so
# events committed between the cut-points read and the last execute are still
# picked up instead of waiting a tick.
FAKE_CUTS=1400 run_sweep small
grep -q '"rhythm":true,"segments":2,"cuts":\[1400\]' "$LOG" || fail "rhythm marker missing or wrong"
grep -q "EXEC lo=0 hi=1400" "$EXEC_LOG" || fail "first segment must run with an unpinned lower bound"
grep -q "EXEC lo=1401 hi=0" "$EXEC_LOG" || fail "last segment must run with an unpinned upper bound"
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 2 ]] || fail "one cut must execute exactly two segments"
! grep -q '"split":true' "$LOG" || fail "a right-sized rhythm segment must not halve"

# Case R2: several cuts partition the window contiguously — segment k+1 starts
# at segment k's end + 1, so no event can fall between two segments and lose
# its chance at a manifest.
FAKE_CUTS=$'1200\n1500' run_sweep small
grep -q '"segments":3,"cuts":\[1200,1500\]' "$LOG" || fail "multi-cut marker missing"
grep -q "EXEC lo=0 hi=1200" "$EXEC_LOG" || fail "segment 1 missing"
grep -q "EXEC lo=1201 hi=1500" "$EXEC_LOG" || fail "segment 2 missing"
grep -q "EXEC lo=1501 hi=0" "$EXEC_LOG" || fail "segment 3 missing"
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 3 ]] || fail "two cuts must execute exactly three segments"

# Case R3: RHYTHM FIRST, HALVING FALLBACK. The cut sits at 1100, deliberately
# NOT on the median: a right-sized first segment executes whole while the
# oversized second segment halves on ITS OWN median, inside its own bounds.
#
# This is the falsifier for the reverse order. Halving-first on the same mode
# would recurse on the whole session and produce
# [1000,1200] [1201,1400] [1401,1700] [1701,1900] — four legs, none of them
# starting at 1101. Rhythm-first produces three, and the boundary at 1101 is
# the proof that the story cut, not the byte cut, went first.
FAKE_CUTS=1100 run_sweep rhythm-oversize
grep -q '"rhythm":true,"segments":2,"cuts":\[1100\]' "$LOG" || fail "rhythm pass must run before the ceiling check"
grep -q '"split":true' "$LOG" || fail "an oversized rhythm segment must fall back to halving"
grep -q "EXEC lo=0 hi=1100" "$EXEC_LOG" || fail "right-sized rhythm segment must execute whole"
grep -q "EXEC lo=1101 hi=1500" "$EXEC_LOG" || fail "halving inside the oversized segment missing"
grep -q "EXEC lo=1501 hi=1900" "$EXEC_LOG" || fail "halving inside the oversized segment (right) missing"
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 3 ]] || fail "rhythm-then-halving must yield exactly three legs"
! grep -q "EXEC lo=1000 hi=1200" "$EXEC_LOG" || fail "halving must not run on the whole session ahead of the cuts"

# Case R4: MEMOIRE_SWEEP_RHYTHM_CUTS=0 restores the pure time-slice behavior —
# one unpinned whole-session window, no cut marker — even with cuts available.
FAKE_CUTS=1400 RHYTHM_OFF=0 run_sweep small
! grep -q '"rhythm":true' "$LOG" || fail "the kill switch must skip the rhythm pass"
grep -q "EXEC lo=0 hi=0" "$EXEC_LOG" || fail "kill switch must derive the whole session"
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 1 ]] || fail "kill switch must execute exactly one leg"

# Case R5: a cut-detector failure is FAIL-OPEN BUT LOUD. Silently reverting the
# lane to time slicing is the class this marker exists to prevent.
FAKE_CUTS=cuts-fail run_sweep small
grep -q 'cuts=failed' "$LOG" || fail "cut-detector failure must be logged"
grep -q "EXEC lo=0 hi=0" "$EXEC_LOG" || fail "cut failure must fall back to the whole window"
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 1 ]] || fail "cut failure must execute exactly one leg"

# Case R6: the cut list is ALL-OR-NOTHING (review r1 F1). One unusable line
# condemns the whole output — a helper that emitted garbage has not told us the
# surviving lines are meaningful — so the SESSION degrades to the unpinned
# whole-session window and says so. Falsified against the previous behavior,
# which dropped the bad lines, kept `cuts:[1400]` and derived two segments.
FAKE_CUTS=$'not-an-epoch\n1400\n1200\n1400\n-5' run_sweep small
grep -q 'cuts=failed reason=malformed_output' "$LOG" || fail "malformed cut output must be logged as a failure"
! grep -q '"rhythm":true' "$LOG" || fail "malformed cut output must not produce a rhythm segmentation"
grep -q "EXEC lo=0 hi=0" "$EXEC_LOG" || fail "malformed cut output must derive the whole session"
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 1 ]] || fail "malformed cut output must execute exactly one leg"

# Case R6b: a non-increasing list is malformed too, even when every line is a
# well-formed epoch — the partition depends on strict ordering.
FAKE_CUTS=$'1400\n1200' run_sweep small
grep -q 'cuts=failed reason=malformed_output' "$LOG" || fail "a non-increasing cut list must fail open"
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 1 ]] || fail "non-increasing cut list must execute exactly one leg"

# Case R6c: an EMPTY helper output is the ordinary "no cut needed" answer and
# must NOT be reported as a failure — otherwise every quiet session would log
# one, and the marker would stop meaning anything.
FAKE_CUTS= run_sweep small
! grep -q 'cuts=failed' "$LOG" || fail "empty cut output is not a failure"
! grep -q '"rhythm":true' "$LOG" || fail "empty cut output must not segment"
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 1 ]] || fail "empty cut output must execute exactly one whole-session leg"

# Case R7: the per-session leg budget. Rhythm cuts multiply legs, so the
# halving depth's 2^depth is also a per-SESSION ceiling: at depth 1 the budget
# is 2 legs, so segments 3 and 4 are marked, not derived.
FAKE_CUTS=$'1100\n1300\n1500' SPLIT_DEPTH=1 run_sweep small
grep -q '"segments":4' "$LOG" || fail "four segments expected before the budget bites"
grep -q '"skipped":"session_leg_budget_exhausted","budget":2' "$LOG" || fail "leg-budget marker missing"
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 2 ]] || fail "leg budget must cap executed legs at 2"

# Case R8 (review r1 F2): rhythm segments enter derive_window at DEPTH 1, not
# 0 — a rhythm cut IS a split, so it consumes one level of the halving budget
# instead of handing every segment a fresh one. Nothing else in this harness
# observes that number: the segment argv and leg-budget cases above stay green
# with depth 0, which is exactly why this case exists.
#
# The oversized second segment's own split line carries the depth it was
# entered at, so assert the whole line. With 0 it reads "depth":0 and this
# fails — falsified.
FAKE_CUTS=1100 run_sweep rhythm-oversize
grep -q '{"split":true,"prompt_bytes":200000,"mid":1500,"depth":1}' "$LOG" \
  || fail "an oversized rhythm segment must split at depth 1"

# Case R8b: the same fixture at MEMOIRE_SWEEP_MAX_SPLIT_DEPTH=1. The oversized
# rhythm segment is already AT the depth limit, so it is MARKED rather than
# halved — which can only happen if the segment entered at depth 1. At depth 0
# it would halve normally and no depth marker would appear, so this is the
# second, independent half of the falsifier.
FAKE_CUTS=1100 SPLIT_DEPTH=1 run_sweep rhythm-oversize
grep -q '"skipped":"split_depth_exceeded","prompt_bytes":200000,"depth":1' "$LOG" \
  || fail "at MAX_SPLIT_DEPTH=1 an oversized rhythm segment must be depth-capped, not halved"
! grep -q '"split":true' "$LOG" || fail "a depth-capped rhythm segment must not halve"
grep -q "EXEC lo=0 hi=1100" "$EXEC_LOG" || fail "the right-sized segment must still execute"
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 1 ]] || fail "only the right-sized segment may execute"

# Case R8c (review r2 residual on F2): R8/R8b above only ever make the
# TRAILING segment oversized, so they observe the depth argument at the
# post-loop call site and leave the in-LOOP one — the call that derives every
# non-trailing segment — unobserved. Replacing only that argument with 0 left
# both of them green.
#
# A LATE cut inverts the fixture: segment 1 [0,1800] resolves to 1000..1800 and
# is oversized, while the trailing segment [1801,0] resolves to 1801..1900 and
# fits. So the depth asserted here can only have come from the in-loop call.
# Segment 1 halves on its own median (1400) at depth 1, and its left leg is
# still oversized so it halves again at depth 2 — which also pins that the
# halving budget keeps counting up from 1 rather than restarting.
FAKE_CUTS=1800 run_sweep rhythm-oversize
grep -q '{"split":true,"prompt_bytes":200000,"mid":1400,"depth":1}' "$LOG" \
  || fail "an oversized NON-trailing rhythm segment must split at depth 1"
grep -q '{"split":true,"prompt_bytes":200000,"mid":1200,"depth":2}' "$LOG" \
  || fail "halving inside a rhythm segment must continue from depth 1, not restart"
grep -q "EXEC lo=1801 hi=0" "$EXEC_LOG" || fail "the right-sized trailing segment must execute whole"
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 4 ]] || fail "expected three halved legs plus the trailing segment"

# Case R8d: the same late-cut fixture at MEMOIRE_SWEEP_MAX_SPLIT_DEPTH=1. The
# oversized FIRST segment is already at the limit, so it is marked and never
# halved, and the trailing segment is the only executed leg.
#
# The depth marker alone is not a falsifier here — entered at depth 0 the
# segment would halve and its own left leg would then hit the cap at depth 1,
# producing a marker that looks identical. What separates the two is that a
# depth-capped segment emits NO split line and executes NO halved leg, so both
# are asserted.
FAKE_CUTS=1800 SPLIT_DEPTH=1 run_sweep rhythm-oversize
grep -q '"skipped":"split_depth_exceeded","prompt_bytes":200000,"depth":1' "$LOG" \
  || fail "at MAX_SPLIT_DEPTH=1 an oversized non-trailing segment must be depth-capped"
! grep -q '"split":true' "$LOG" || fail "a depth-capped non-trailing segment must not halve at all"
grep -q "EXEC lo=1801 hi=0" "$EXEC_LOG" || fail "the trailing segment must still execute"
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 1 ]] || fail "only the trailing segment may execute"

# ---------------------------------------------------------------------------
# Open tail and the supersede knob (Fibery #344).
# ---------------------------------------------------------------------------

# Case P1: the whole-session window IS the open tail — it ends at the last
# event so far — so the single leg carries -provisional.
run_sweep small
grep -q "EXEC lo=0 hi=0 prov=1" "$EXEC_LOG" || fail "the whole-session window must be marked provisional"

# Case P2: with rhythm cuts, ONLY the trailing segment carries the marker. An
# inner segment is closed at a cut the detector already saw, so its arc ended.
# Falsified against marking every leg (the inner assertions) and against
# marking none (the trailing one).
FAKE_CUTS=$'1200\n1500' run_sweep small
grep -q "EXEC lo=0 hi=1200 prov=0" "$EXEC_LOG" || fail "the first segment must not be provisional"
grep -q "EXEC lo=1201 hi=1500 prov=0" "$EXEC_LOG" || fail "an inner segment must not be provisional"
grep -q "EXEC lo=1501 hi=0 prov=1" "$EXEC_LOG" || fail "the trailing segment must be provisional"
[[ "$(grep -c 'prov=1' "$EXEC_LOG")" == 1 ]] || fail "exactly one leg per session may be provisional"

# Case P3: halving an oversized OPEN TAIL keeps the marker on the right leg
# only — the left leg is a closed interval whose events are all in the past.
FAKE_CUTS=1100 run_sweep rhythm-oversize
grep -q "EXEC lo=0 hi=1100 prov=0" "$EXEC_LOG" || fail "a closed rhythm segment must not be provisional"
grep -q "EXEC lo=1101 hi=1500 prov=0" "$EXEC_LOG" || fail "the LEFT half of a halved open tail must not be provisional"
grep -q "EXEC lo=1501 hi=1900 prov=1" "$EXEC_LOG" || fail "the RIGHT half of a halved open tail must stay provisional"
[[ "$(grep -c 'prov=1' "$EXEC_LOG")" == 1 ]] || fail "halving must not multiply the provisional marker"

# Case P4: the fail-open paths still mark the tail — a cut-detector failure
# derives the whole session, which is exactly the open-tail window.
FAKE_CUTS=cuts-fail run_sweep small
grep -q 'cuts=failed' "$LOG" || fail "cut failure must still be logged"
grep -q "EXEC lo=0 hi=0 prov=1" "$EXEC_LOG" || fail "the fail-open whole-session leg must be provisional"

# Case P5: MEMOIRE_SWEEP_SUPERSEDE=0 adds -no-supersede to EVERY execute leg
# and changes nothing else; the default adds it to none.
FAKE_CUTS=$'1200\n1500' SUPERSEDE=0 run_sweep small
[[ "$(grep -c '^EXEC ' "$EXEC_LOG")" == 3 ]] || fail "the supersede knob must not change segmentation"
[[ "$(grep -c 'nosup=1' "$EXEC_LOG")" == 3 ]] || fail "MEMOIRE_SWEEP_SUPERSEDE=0 must pass -no-supersede on every execute leg"
FAKE_CUTS=$'1200\n1500' run_sweep small
[[ "$(grep -c 'nosup=0' "$EXEC_LOG")" == 3 ]] || fail "supersede must be enabled by default on every leg"

# Case 4: missing key exits silently without touching the log or executing.
: > "$LOG"; : > "$EXEC_LOG"
FAKE_MODE=small EXEC_LOG="$EXEC_LOG" \
MEMOIRE_HOME="$WORK/home" \
MEMOIRE_AUTO_DERIVE=1 \
MEMOIRE_DB= \
MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
MEMOIRE_DERIVE_KEY_ENV=MEMOIRE_TEST_UNSET_KEY_VAR \
bash "$SWEEP"
[[ ! -s "$LOG" && ! -s "$EXEC_LOG" ]] || fail "missing key must be a silent no-op"

echo "sweep-split-test: OK"
