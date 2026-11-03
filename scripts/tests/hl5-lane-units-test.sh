#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# hl5-lane-units-test.sh — coverage for the two D163 surfaces the trigger
# harness cannot see (review rounds 1-2, MED-4 residual): the Conductor
# prewarm script (deadline, key transport out of argv, fail-open paths) and
# the derive-timer.sh generated units (ExecCondition/ExecStartPost wiring,
# no timer persistence, env overrides).

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PREWARM="$ROOT/memoire-conductor-prewarm.sh"
TIMERSH="$ROOT/derive-timer.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/home/log" "$WORK/bin"
LOG="$WORK/home/log/derive-auto.log"
ARGV_LOG="$WORK/curl-argv.log"
touch "$ARGV_LOG"

fail() {
  echo "FAIL: $1" >&2
  echo "--- log:" >&2; cat "$LOG" >&2 || true
  echo "--- curl argv:" >&2; cat "$ARGV_LOG" >&2 || true
  exit 1
}

# Fake curl: records its argv, answers per FAKE_CURL_CODE.
cat > "$WORK/bin/curl" <<FAKE
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$ARGV_LOG"
printf '%s' "\${FAKE_CURL_CODE:-200}"
exit 0
FAKE
chmod +x "$WORK/bin/curl"

run_prewarm() { # extra env...
  env PATH="$WORK/bin:$PATH" \
    MEMOIRE_HOME="$WORK/home" \
    REVEILLE_API_KEY="sekret-value-123" \
    MEMOIRE_PREWARM_DEADLINE_S=3 \
    MEMOIRE_PREWARM_ATTEMPT_TIMEOUT_S=2 \
    "$@" bash "$PREWARM"
}

# --- prewarm: 200 -> ok marker, single attempt ------------------------------
run_prewarm || fail "prewarm must exit 0"
grep -q "prewarm ok model=.* attempts=1" "$LOG" || fail "ok marker missing"

# --- prewarm: key never in curl argv (round-1 HIGH-2) -----------------------
grep -q "sekret-value-123" "$ARGV_LOG" && fail "bearer value must never appear in curl argv"
grep -q -- "-H @" "$ARGV_LOG" || fail "header must be passed via @file"

# --- prewarm: persistent 503 -> hard deadline, fail-open --------------------
: > "$LOG"; : > "$ARGV_LOG"
start=$(date +%s)
run_prewarm FAKE_CURL_CODE=503 || fail "prewarm must exit 0 on deadline"
elapsed=$(( $(date +%s) - start ))
grep -q "prewarm deadline" "$LOG" || fail "deadline marker missing"
(( elapsed <= 8 )) || fail "deadline must be hard (took ${elapsed}s for a 3s budget)"

# --- prewarm: missing key -> skip marker, no curl call ----------------------
: > "$LOG"; : > "$ARGV_LOG"
env PATH="$WORK/bin:$PATH" MEMOIRE_HOME="$WORK/home" \
  MEMOIRE_DERIVE_KEY_ENV=MEMOIRE_TEST_UNSET_KEY bash "$PREWARM" \
  || fail "prewarm without key must exit 0"
grep -q "prewarm skipped=key_missing" "$LOG" || fail "key_missing marker absent"
[[ -s "$ARGV_LOG" ]] && fail "missing key must not call curl"

# --- generated units --------------------------------------------------------
# Fake systemctl so enable/daemon-reload are no-ops; fake credstore files.
cat > "$WORK/bin/systemctl" <<'FAKE'
#!/usr/bin/env bash
exit 0
FAKE
chmod +x "$WORK/bin/systemctl"
export XDG_CONFIG_HOME="$WORK/xdg"
mkdir -p "$XDG_CONFIG_HOME/credstore.encrypted"
touch "$XDG_CONFIG_HOME/credstore.encrypted/hl3_qwen_api_key" \
      "$XDG_CONFIG_HOME/credstore.encrypted/reveille_api_key"
UNITS="$XDG_CONFIG_HOME/systemd/user"

env PATH="$WORK/bin:$PATH" MEMOIRE_DERIVE_KEY_ENV=HL3_QWEN_API_KEY \
  bash "$TIMERSH" install >/dev/null || fail "install must succeed"
HOURLY="$UNITS/memoire-derive-sweep.service"
grep -q "^ExecStartPost=-/usr/bin/timeout 60 .*memoire-derive-hl5-trigger.sh$" "$HOURLY" \
  || fail "hourly unit must run the trigger under timeout 60, failure-ignored"
grep -q "^LoadCredentialEncrypted=hl3_qwen_api_key$" "$HOURLY" \
  || fail "hourly unit must load the hl3 credential"
grep -q "^Environment=MEMOIRE_HL5_TRIGGER=0$" "$HOURLY" \
  || fail "hourly unit must ship the early-fire trigger DISABLED (operator ruling, D163)"
# The low-effort ruling covers the HL5 lanes ONLY: the hourly unit must not
# carry the env, or its endpoint silently loses the server-default behavior.
grep -q "MEMOIRE_DERIVE_REASONING_EFFORT" "$HOURLY" \
  && fail "hourly unit must NOT set a reasoning effort (HL5-lane ruling only)"

env PATH="$WORK/bin:$PATH" bash "$TIMERSH" install-hl5 >/dev/null \
  || fail "install-hl5 must succeed"
SVC="$UNITS/memoire-derive-sweep-hl5.service"
TMR="$UNITS/memoire-derive-sweep-hl5.timer"
grep -q "^ExecCondition=.*memoire-derive-hl5-trigger.sh --lane-fresh-check$" "$SVC" \
  || fail "nightly service must gate on --lane-fresh-check"
grep -q "^ExecStartPost=.*memoire-derive-hl5-trigger.sh --record-success$" "$SVC" \
  || fail "nightly service must record success after ExecStart"
grep -q "^ExecStartPre=-.*memoire-conductor-prewarm.sh$" "$SVC" \
  || fail "nightly service must prewarm, failure-ignored"
grep -q "^LoadCredentialEncrypted=reveille_api_key$" "$SVC" \
  || fail "nightly service must load the reveille credential"
grep -q "^Environment=MEMOIRE_DERIVE_ENDPOINT=hl5-conductor$" "$SVC" \
  || fail "nightly service must target the hl5-conductor endpoint"
# conductor-proxy-v1 (2026-08-03): the unit must name a model the
# Conductor serves — the retired paperclip-brain alias 404s and fails
# the sweep's allowlist/served-model checks. Since 2026-08-26 that name
# is the stable Reveille routing alias 'brain' (homelabs brain-swap
# ruling): it survives brain model swaps, profile keys do not.
grep -q "^Environment=MEMOIRE_DERIVE_MODEL=brain$" "$SVC" \
  || fail "nightly service must name the stable Conductor brain alias"
grep -q "paperclip-brain" "$SVC" \
  && fail "nightly service must not carry the retired paperclip-brain alias"
grep -q "^Environment=MEMOIRE_SWEEP_MAX_PROMPT_BYTES=110000$" "$SVC" \
  || fail "nightly service must raise the split ceiling for HL5"
# Effort ruling (operator, 2026-08-26): the derive lane runs the brain at low
# chat-template reasoning effort. Unset would silently fall back to the
# server default (medium) for every nightly run.
grep -q "^Environment=MEMOIRE_DERIVE_REASONING_EFFORT=low$" "$SVC" \
  || fail "nightly service must pin the ruled low reasoning effort"
grep -q "^OnCalendar=\*-\*-\* 04:00:00$" "$TMR" || fail "timer must fire at 04:00 local"
grep -q "^Persistent" "$TMR" && fail "timer must NOT be persistent (round-1 HIGH-1)"

echo "hl5-lane-units-test: OK"
