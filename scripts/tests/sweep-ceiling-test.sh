#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# sweep-ceiling-test.sh — regression test for memoire-derive-sweep.sh's
# prompt-byte ceiling resolution (review r1 F1, docs/DERIVE-PROFILE-DESIGN.md
# §3). No live provider, no real memoire-derive: a fake binary records every
# invocation's argv and serves canned dry-run JSON.
#
# The bug this pins: the sweep always passed its 70000 fallback as
# -max-prompt-bytes, which the CLI marks as an EXPLICIT flag, so a declared
# endpoint profile could never govern the scheduled path.
#
# Covered: an operator env override still outranks the profile and rides every
# argv; with the override unset the FIRST dry-run omits the flag so the profile
# can be reported, and every later leg carries the profile's number; the
# no-profile endpoint keeps the 70000 fallback (without it, the CLI's built-in
# default of 0 = no cap would silently disable the adaptive split); and each
# unreadable-JSON failure mode — no effective_limits block, non-numeric value,
# source=flag — falls back to 70000 rather than to no cap.

SWEEP="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/memoire-derive-sweep.sh"
command -v flock >/dev/null || { echo "SKIP: flock not available"; exit 0; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/home/log" "$WORK/bin"
ARGV_LOG="$WORK/argv.log"
touch "$ARGV_LOG"
LOG="$WORK/home/log/derive-auto.log"

fail() {
  echo "FAIL: $1" >&2
  echo "--- argv calls:" >&2
  cat "$ARGV_LOG" >&2 || true
  echo "--- sweep log:" >&2
  cat "$LOG" >&2 || true
  exit 1
}

# Fake memoire-derive. FAKE_LIMITS selects the effective_limits block the
# dry-run reports; FAKE_SPLIT makes the whole-session window oversized so the
# split recursion runs and its legs' argv can be asserted too. Output framing
# mirrors the real CLI's MarshalIndent: two-space top level, four-space
# members, and prompt_meta carries its own max_prompt_bytes so a reader that
# matches the wrong block is caught here.
cat > "$WORK/bin/fake-derive" <<'FAKE'
#!/usr/bin/env bash
mode="" lo=0 hi=0
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
  esac
done
case "$mode" in
  list)
    printf 'sweepproj\x1fsess-ceiling\n'
    exit 0 ;;
  intents) exit 0 ;;
  settle) echo "settled=0"; exit 0 ;;
  cuts)
    # Rhythm cuts (Fibery #343). This file is about ceiling RESOLUTION, so the
    # detector reports no cut by default and every case keeps its single
    # whole-session window; FAKE_CUTS turns it on where a case wants segment
    # argv asserted. Answering here is load-bearing regardless: an unhandled
    # helper flag would fall through to the argv log and the dry-run JSON
    # below, corrupting every ceiling assertion.
    [[ -n "${FAKE_CUTS:-}" ]] && printf '%s\n' ${FAKE_CUTS}
    exit 0 ;;
  median)
    eligible=()
    for ep in 1000 1100 1200 1300 1400 1500 1600 1700 1800 1900; do
      (( ep >= lo && ep <= hi )) && eligible+=("$ep")
    done
    n=${#eligible[@]}
    (( n > 0 )) && printf '%s\n' "${eligible[$(( (n - 1) / 2 ))]}"
    exit 0 ;;
esac

printf '%s %s\n' "$mode" "$*" >> "$ARGV_LOG"

if [[ "$mode" == exec ]]; then
  printf '{\n  "status": "completed",\n  "written_count": 1\n}\n'
  exit 0
fi

(( lo == 0 && hi == 0 )) && { lo=1000; hi=1900; }
# 40000 fits under every ceiling this test exercises (43929 profile, 70000
# fallback), so a window is oversized only where a case wants it to be.
pb=40000
if [[ "${FAKE_SPLIT:-0}" == 1 ]] && (( hi - lo >= 900 )); then pb=200000; fi

case "${FAKE_LIMITS:-profile}" in
  profile) limits='    "max_completion_tokens": 12288,\n    "max_prompt_bytes": 43929,\n    "source": "profile"' ;;
  default) limits='    "max_completion_tokens": 0,\n    "max_prompt_bytes": 0,\n    "source": "default"' ;;
  flag)    limits='    "max_completion_tokens": 0,\n    "max_prompt_bytes": 55555,\n    "source": "flag"' ;;
  garbage) limits='    "max_prompt_bytes": "not-a-number",\n    "source": "profile"' ;;
  split)   limits='    "max_completion_tokens": 12288,\n    "max_completion_tokens_source": "profile",\n    "max_prompt_bytes": 43929,\n    "max_prompt_bytes_source": "profile"' ;;
  none)    limits='' ;;
esac

{
  printf '{\n'
  if [[ -n "$limits" ]]; then
    printf '  "effective_limits": {\n'
    printf "$limits\n"
    printf '  },\n'
  fi
  printf '  "prompt_bytes": %s,\n' "$pb"
  printf '  "prompt_meta": {\n    "limits": {\n      "max_prompt_bytes": 99999999\n    }\n  },\n'
  printf '  "window_start_epoch": %s,\n  "window_end_epoch": %s\n}\n' "$lo" "$hi"
}
exit 0
FAKE
chmod +x "$WORK/bin/fake-derive"

printf '#!/usr/bin/env bash\nexit 0\n' > "$WORK/bin/noop-capture"
chmod +x "$WORK/bin/noop-capture"

# Ambient-immunity mirrors sweep-split-test.sh: the kill switch, a live DSN,
# or an operator ceiling override in the invoking shell must never leak in.
run_sweep() { # FAKE_LIMITS  (CEILING_ENV / FAKE_SPLIT via env)
  : > "$LOG"
  : > "$ARGV_LOG"
  FAKE_LIMITS="$1" ARGV_LOG="$ARGV_LOG" FAKE_SPLIT="${FAKE_SPLIT:-0}" \
  FAKE_CUTS="${FAKE_CUTS:-}" \
  MEMOIRE_HOME="$WORK/home" \
  MEMOIRE_AUTO_DERIVE=1 \
  MEMOIRE_DB= \
  MEMOIRE_SWEEP_MAX_PROMPT_BYTES="${CEILING_ENV:-}" \
  MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
  MEMOIRE_CAPTURE_CODEX_BIN="$WORK/bin/noop-capture" \
  MEMOIRE_CAPTURE_QWEN_BIN="$WORK/bin/noop-capture" \
  MEMOIRE_CAPTURE_OPENCODE_BIN="$WORK/bin/noop-capture" \
  MEMOIRE_DERIVE_KEY_ENV=HOME \
  MEMOIRE_SWEEP_HOURS=99999999 \
  bash "$SWEEP"
}

first_dry() { grep -m1 '^dry ' "$ARGV_LOG"; }
exec_argv() { grep -m1 '^exec ' "$ARGV_LOG"; }

# Case (a): an operator env override outranks the profile and is passed on
# EVERY call, probe included — there is nothing to resolve.
CEILING_ENV=123456 run_sweep profile
first_dry | grep -q -- '-max-prompt-bytes 123456' \
  || fail "env override must ride the first dry-run argv"
exec_argv | grep -q -- '-max-prompt-bytes 123456' \
  || fail "env override must ride the execute argv"
if exec_argv | grep -q -- '-max-prompt-bytes 43929'; then
  fail "the profile must never outrank an explicit operator override"
fi

# Case (b): override unset + a profiled endpoint. The probe omits the flag so
# the CLI can apply the profile; the resolved number then rides every leg.
run_sweep profile
if first_dry | grep -q -- '-max-prompt-bytes'; then
  fail "the ceiling probe must omit -max-prompt-bytes so the profile can govern"
fi
exec_argv | grep -q -- '-max-prompt-bytes 43929' \
  || fail "the execute leg must carry the profile's resolved ceiling"
grep -q 'ceiling=43929 source=profile' "$LOG" \
  || fail "the resolved ceiling must be logged with its source"

# Case (b2): per-limit source keys (the CLI splits them when the two limits
# resolve from different sources) are read too.
run_sweep split
exec_argv | grep -q -- '-max-prompt-bytes 43929' \
  || fail "per-limit max_prompt_bytes_source=profile must resolve the ceiling"

# Case (b3): the split recursion's own legs carry the resolved ceiling — one
# resolution per sweep invocation, and only the very first dry-run probes.
FAKE_SPLIT=1 run_sweep profile
[[ "$(grep -c -- '^dry .*-max-prompt-bytes 43929' "$ARGV_LOG")" -ge 2 ]] \
  || fail "split legs must be sized against the resolved profile ceiling"
[[ "$(grep -c '^dry ' "$ARGV_LOG")" -ge 3 ]] || fail "expected a split into legs"
grep -q '"split":true' "$LOG" || fail "oversized window must still split"

# Case (b4): with rhythm cuts active (Fibery #343) the FIRST dry-run of the
# sweep is a rhythm segment's, not the whole session's — the ceiling still
# resolves exactly once, from that first probe, and every later segment leg
# carries the resolved number. Without this the probe would move behind the
# cut pass and the profile could stop governing.
FAKE_CUTS=1400 run_sweep profile
grep -q '"rhythm":true' "$LOG" || fail "the rhythm pass must run under the ceiling harness"
if first_dry | grep -q -- '-max-prompt-bytes'; then
  fail "the first rhythm segment's dry-run is the probe and must omit -max-prompt-bytes"
fi
[[ "$(grep -c -- '^dry .*-max-prompt-bytes 43929' "$ARGV_LOG")" -ge 1 ]] \
  || fail "later rhythm segments must be sized against the resolved profile ceiling"
[[ "$(grep -c -- '^exec ' "$ARGV_LOG")" == 2 ]] || fail "one cut must execute two segments"
[[ "$(grep -c -- '^exec .*-max-prompt-bytes 43929' "$ARGV_LOG")" == 2 ]] \
  || fail "every rhythm segment's execute must carry the resolved ceiling"
[[ "$(grep -c 'ceiling=43929 source=profile' "$LOG")" == 1 ]] \
  || fail "the ceiling must resolve exactly once per sweep invocation"

# Case (c): a profile-less endpoint (source=default, value 0) keeps the 70000
# script fallback. Falsifier for the tempting "just omit the flag" remedy:
# with 0 = no cap the sweep would never split again.
run_sweep default
exec_argv | grep -q -- '-max-prompt-bytes 70000' \
  || fail "a profile-less endpoint must fall back to the sweep's 70000"

# Case (d): failure modes all land on the same fallback, never on no cap.
for mode in none garbage flag; do
  run_sweep "$mode"
  exec_argv | grep -q -- '-max-prompt-bytes 70000' \
    || fail "unreadable/non-profile effective_limits ($mode) must fall back to 70000"
done

echo "sweep-ceiling-test: OK"
