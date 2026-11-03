#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# capture-tick-test.sh — regression test for the quiet-timer capture body
# (streaming capture, Fibery #342). Pure and offline: a stub adapter stands in
# for the CC transcript capture and a recording fake curl for the API, both on
# a scratch PATH with a scratch MEMOIRE_HOME and a dummy token.
#
# Pinned here: the summary line's arithmetic over the adapter's TSV; notify OFF
# by default (the hourly sweep already enumerates outstanding work — the tick
# changes window CONTENT, not cadence); notify ON posting exactly one intent
# per (project, session) with the bearer in a header file and NEVER in argv;
# single-instance locking; and the fail-open exits a timer must not flap on.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TICK="$ROOT/memoire-capture-tick.sh"
command -v flock >/dev/null || { echo "SKIP: flock not available"; exit 0; }
command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/home/log" "$WORK/bin" "$WORK/projects"
LOG="$WORK/home/log/capture-tick.log"
EXEC_LOG="$WORK/exec.log"
touch "$EXEC_LOG"
printf 'tick-test-token\n' > "$WORK/home/token"

fail() {
  echo "FAIL: $1" >&2
  echo "--- log:" >&2
  cat "$LOG" >&2 || true
  echo "--- exec log:" >&2
  cat "$EXEC_LOG" >&2 || true
  exit 1
}

# Stub adapter: records argv, then emits the TSV contract on stdout and noise
# on stderr (the tick must drop that noise, keeping one line per tick).
cat > "$WORK/bin/fake-adapter" <<FAKE
#!/usr/bin/env bash
echo "ADAPTER \$*" >> "$EXEC_LOG"
echo "adapter chatter that must not reach the tick log" >&2
printf 'proj-a\tsess-1\t3\n'
printf 'proj-a\tsess-1\t1\n'
printf 'proj-b\tsess-2\t2\n'
FAKE
cat > "$WORK/bin/curl" <<FAKE
#!/usr/bin/env bash
echo "NOTIFY \$*" >> "$EXEC_LOG"
printf '200'
FAKE
chmod +x "$WORK/bin/fake-adapter" "$WORK/bin/curl"

run_tick() { # extra-env...
  env PATH="$WORK/bin:$PATH" \
    MEMOIRE_HOME="$WORK/home" \
    MEMOIRE_CC_PROJECTS_DIR="$WORK/projects" \
    MEMOIRE_CAPTURE_CC_BIN="$WORK/bin/fake-adapter" \
    MEMOIRE_CAPTURE= \
    MEMOIRE_AUTO_DERIVE=1 \
    "$@" bash "$TICK" || fail "tick must exit 0"
}

# --- default tick: adapter argv, summary arithmetic, notify OFF -------------
run_tick
grep -q "ADAPTER --tail --quiet-for 120 --since 172800 --emit-sessions $WORK/projects" "$EXEC_LOG" \
  || fail "tick must call the adapter with --tail, both windows and --emit-sessions"
grep -q "tick files_new=3 turns=6 sessions=2 notify=off rc=0" "$LOG" \
  || fail "tick summary line wrong or missing"
[[ "$(grep -c . "$LOG")" == "1" ]] || fail "a tick must log exactly one line when notify is off"
! grep -q "adapter chatter" "$LOG" || fail "adapter stderr must not reach the tick log"
! grep -q "NOTIFY" "$EXEC_LOG" || fail "notify must be OFF by default"

# --- knob overrides ride through to the adapter -----------------------------
: > "$LOG"; : > "$EXEC_LOG"
run_tick MEMOIRE_TICK_QUIET_S=30 MEMOIRE_TICK_SINCE_S=600
grep -q -- "--quiet-for 30 --since 600" "$EXEC_LOG" || fail "tick knobs must reach the adapter"

# --- MEMOIRE_TICK_NOTIFY=1: one POST per distinct (project, session) --------
: > "$LOG"; : > "$EXEC_LOG"
run_tick MEMOIRE_TICK_NOTIFY=1
[[ "$(grep -c 'NOTIFY' "$EXEC_LOG")" == "2" ]] \
  || fail "notify must fire once per DISTINCT (project, session), not once per TSV row"
grep -q 'NOTIFY .*"project":"proj-a","session_id":"sess-1"' "$EXEC_LOG" || fail "missing intent for proj-a/sess-1"
grep -q 'NOTIFY .*"project":"proj-b","session_id":"sess-2"' "$EXEC_LOG" || fail "missing intent for proj-b/sess-2"
grep -q 'NOTIFY .*/v1/derive/notify' "$EXEC_LOG" || fail "notify must POST /v1/derive/notify"
! grep -q "tick-test-token" "$EXEC_LOG" || fail "the bearer must never appear in curl argv"
grep -q "tick notify project=proj-a session=sess-1 code=200" "$LOG" || fail "per-session notify line missing"
grep -q "notify=2/2 rc=0" "$LOG" || fail "notify summary must count the successes"

# --- notify prerequisites degrade honestly, never fatally -------------------
: > "$LOG"; : > "$EXEC_LOG"
run_tick MEMOIRE_TICK_NOTIFY=1 MEMOIRE_TOKEN_FILE="$WORK/home/no-such-token"
grep -q "notify=off_no_token" "$LOG" || fail "a missing token must degrade notify to a marker"
! grep -q "NOTIFY" "$EXEC_LOG" || fail "no token must mean no POST"
: > "$LOG"; : > "$EXEC_LOG"
run_tick MEMOIRE_TICK_NOTIFY=1 MEMOIRE_AUTO_DERIVE=0
grep -q "notify=off_auto_derive_disabled" "$LOG" || fail "MEMOIRE_AUTO_DERIVE=0 must disable notify only"
grep -q "files_new=3" "$LOG" || fail "MEMOIRE_AUTO_DERIVE=0 must NOT disable the capture"

# --- single instance: a second concurrent tick skips on the lock ------------
: > "$LOG"; : > "$EXEC_LOG"
cat > "$WORK/bin/fake-adapter-slow" <<FAKE
#!/usr/bin/env bash
echo "ADAPTERSLOW \$*" >> "$EXEC_LOG"
sleep 3
printf 'proj-a\tsess-1\t1\n'
FAKE
chmod +x "$WORK/bin/fake-adapter-slow"
env PATH="$WORK/bin:$PATH" MEMOIRE_HOME="$WORK/home" \
  MEMOIRE_CC_PROJECTS_DIR="$WORK/projects" \
  MEMOIRE_CAPTURE_CC_BIN="$WORK/bin/fake-adapter-slow" \
  bash "$TICK" &
SLOW_PID=$!
for _ in $(seq 1 20); do
  grep -q "ADAPTERSLOW" "$EXEC_LOG" && break
  sleep 0.2
done
grep -q "ADAPTERSLOW" "$EXEC_LOG" || fail "the slow tick never started"
run_tick
grep -q "tick skipped=locked" "$LOG" || fail "a second concurrent tick must skip on the lock"
wait "$SLOW_PID" || fail "the slow tick must still exit 0"

# --- kill switch and missing adapter: marker + exit 0 -----------------------
: > "$LOG"; : > "$EXEC_LOG"
env PATH="$WORK/bin:$PATH" MEMOIRE_HOME="$WORK/home" \
  MEMOIRE_CC_PROJECTS_DIR="$WORK/projects" \
  MEMOIRE_CAPTURE_CC_BIN="$WORK/bin/fake-adapter" \
  MEMOIRE_CAPTURE=off \
  bash "$TICK" || fail "kill-switched tick must exit 0"
grep -q "tick skipped=capture_disabled" "$LOG" || fail "kill switch marker missing"
! grep -q "ADAPTER" "$EXEC_LOG" || fail "kill switch must short-circuit before any capture"

: > "$LOG"; : > "$EXEC_LOG"
run_tick MEMOIRE_CAPTURE_CC_BIN="$WORK/bin/does-not-exist"
grep -q "tick skipped=capture_bin_missing" "$LOG" || fail "missing adapter marker absent"

: > "$LOG"; : > "$EXEC_LOG"
run_tick MEMOIRE_CC_PROJECTS_DIR="$WORK/no-such-projects-dir"
grep -q "tick skipped=no_projects_dir" "$LOG" || fail "missing projects dir marker absent"

echo "capture-tick-test: OK"
