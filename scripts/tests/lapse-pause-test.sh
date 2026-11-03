#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# lapse-pause-test.sh — the durable pause spelling (triage lane v1, R-E).
#
# The pause in force when this landed was an enormous TTL set cluster-side
# (36500 days). That works and LIES: the lane reads as armed, silently never
# fires, and nothing in the repo says the timer is off. MEMOIRE_LAPSE_PAUSED=1
# says it out loud.
#
# Pinned here: with the pause set the lapse-expired call is not made AND the
# skip is logged; with it unset the call is made and the TTL pass-through
# still rides it — pausing and tuning are separate acts and must not collapse
# into one knob.

SWEEP="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/memoire-derive-sweep.sh"
command -v flock >/dev/null || { echo "SKIP: flock not available"; exit 0; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/home/log" "$WORK/bin"
LOG="$WORK/home/log/derive-auto.log"
CURL_LOG="$WORK/curl.log"
touch "$CURL_LOG"
echo "test-token" > "$WORK/home/token"

fail() {
  echo "FAIL: $1" >&2
  echo "--- sweep log:" >&2; cat "$LOG" >&2 || true
  echo "--- curl calls:" >&2; cat "$CURL_LOG" >&2 || true
  exit 1
}

# A fake memoire-derive that serves one small target and completes it: the
# sweep needs work to reach its tail, and the tail is what is under test.
cat > "$WORK/bin/fake-derive" <<'FAKE'
#!/usr/bin/env bash
mode=""
for a in "$@"; do
  case "$a" in
    -list-targets) mode=list ;;
    -settle-intents) mode=settle ;;
    -list-open-intents) mode=intents ;;
    -median-epoch) mode=median ;;
    -dry-run) mode=dry ;;
    -execute) mode=exec ;;
  esac
done
case "$mode" in
  list) printf 'sweepproj\x1fsess-lapse\n'; exit 0 ;;
  intents) exit 0 ;;
  settle) echo "settled=0"; exit 0 ;;
  median) echo 1500; exit 0 ;;
  exec) printf '{\n  "status": "completed",\n  "written_count": 1\n}\n'; exit 0 ;;
  dry) printf '{\n  "prompt_bytes": 100,\n  "window_start_epoch": 1000,\n  "window_end_epoch": 1900\n}\n'; exit 0 ;;
esac
exit 0
FAKE
chmod +x "$WORK/bin/fake-derive"

# A fake curl that records the request and answers like the admin endpoint.
cat > "$WORK/bin/curl" <<CURLFAKE
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$CURL_LOG"
printf '{"lapsed":0}'
exit 0
CURLFAKE
chmod +x "$WORK/bin/curl"

printf '#!/usr/bin/env bash\nexit 0\n' > "$WORK/bin/noop-capture"
chmod +x "$WORK/bin/noop-capture"

run_sweep() { # PAUSED  TTL
  : > "$LOG"
  : > "$CURL_LOG"
  PATH="$WORK/bin:$PATH" \
  MEMOIRE_HOME="$WORK/home" \
  MEMOIRE_AUTO_DERIVE=1 \
  MEMOIRE_DB= \
  MEMOIRE_LAPSE_PAUSED="$1" \
  MEMOIRE_LAPSE_TTL_DAYS="$2" \
  MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
  MEMOIRE_CAPTURE_CODEX_BIN="$WORK/bin/noop-capture" \
  MEMOIRE_CAPTURE_QWEN_BIN="$WORK/bin/noop-capture" \
  MEMOIRE_CAPTURE_OPENCODE_BIN="$WORK/bin/noop-capture" \
  MEMOIRE_DERIVE_KEY_ENV=HOME \
  MEMOIRE_SWEEP_HOURS=99999999 \
  bash "$SWEEP"
}

# Paused: no call, and the skip is VISIBLE. A silent skip would be the same
# lie the TTL=36500 interim tells.
run_sweep 1 21
if grep -q 'lapse-expired' "$CURL_LOG"; then
  fail "MEMOIRE_LAPSE_PAUSED=1 must skip the lapse-expired call"
fi
grep -q 'lapse-expired skipped=lapse_paused' "$LOG" \
  || fail "a paused lapse lane must log its skip"

# Unpaused: the call is made and the TTL pass-through rides it.
run_sweep 0 21
grep -q 'lapse-expired' "$CURL_LOG" \
  || fail "with the pause unset the lapse-expired call must be made"
grep -q '"ttl_days":21' "$CURL_LOG" \
  || fail "the MEMOIRE_LAPSE_TTL_DAYS pass-through must survive the pause guard"

# The default is UNPAUSED: a guard that defaults to off would silently
# disarm every deployment that never heard of it.
: > "$LOG"; : > "$CURL_LOG"
PATH="$WORK/bin:$PATH" \
MEMOIRE_HOME="$WORK/home" MEMOIRE_AUTO_DERIVE=1 MEMOIRE_DB= \
MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
MEMOIRE_CAPTURE_CODEX_BIN="$WORK/bin/noop-capture" \
MEMOIRE_CAPTURE_QWEN_BIN="$WORK/bin/noop-capture" \
MEMOIRE_CAPTURE_OPENCODE_BIN="$WORK/bin/noop-capture" \
MEMOIRE_DERIVE_KEY_ENV=HOME MEMOIRE_SWEEP_HOURS=99999999 \
bash "$SWEEP"
grep -q 'lapse-expired' "$CURL_LOG" \
  || fail "an unset MEMOIRE_LAPSE_PAUSED must leave the lane armed"

echo "PASS: lapse-pause-test.sh"
