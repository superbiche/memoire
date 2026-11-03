#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# kill-switch-test.sh — regression test for MEMOIRE_AUTO_DERIVE=0 (turn-ingest
# campaign Phase 1, slice 5). Both auto-derivation entry points — the
# SessionEnd hook and the sweep — must short-circuit to an instant clean exit
# with one log marker and ZERO derive invocations, even when everything they
# need for a real run is present. Phase 3 mass-closes sessions and must not
# trigger a derivation burst.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AUTO="$ROOT/memoire-derive-auto.sh"
SWEEP="$ROOT/memoire-derive-sweep.sh"
command -v flock >/dev/null || { echo "SKIP: flock not available"; exit 0; }
command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/home/log" "$WORK/bin"
LOG="$WORK/home/log/derive-auto.log"
EXEC_LOG="$WORK/exec.log"
touch "$EXEC_LOG"

fail() {
  echo "FAIL: $1" >&2
  echo "--- log:" >&2
  cat "$LOG" >&2 || true
  exit 1
}

# Fake derive binary records every invocation; a kill-switched run must never
# reach it. Same for the pre-pass capture adapters and the CC transcript
# adapter (live turn ingest, 2026-07-12): the switch must short-circuit
# before any capture too.
cat > "$WORK/bin/fake-derive" <<FAKE
#!/usr/bin/env bash
# -list-targets serves the enumeration fixture (derive-seam-port: the sweep
# reads targets through the binary, not sqlite3). It logs under its own LIST
# marker: a kill-switched run must not reach the store through ANY binary
# mode, so the switched-case empty-EXEC_LOG assertion covers a leaked
# enumeration too (round-1 LOW-1).
if [[ " \$* " == *" -list-targets "* ]]; then
  echo "LIST \$*" >> "$EXEC_LOG"
  printf 'proj\\x1fsess-1\\n'
  exit 0
fi
if [[ " \$* " == *" -list-open-intents "* ]]; then
  echo "LIST \$*" >> "$EXEC_LOG"
  exit 0
fi
if [[ " \$* " == *" -settle-intents "* ]]; then
  echo "LIST \$*" >> "$EXEC_LOG"
  echo "settled=0"
  exit 0
fi
echo "CALLED \$*" >> "$EXEC_LOG"
printf '{\\n  "prompt_bytes": 100\\n}\\n'
FAKE
cat > "$WORK/bin/fake-capture" <<FAKE
#!/usr/bin/env bash
echo "CAPTURE \$*" >> "$EXEC_LOG"
FAKE
chmod +x "$WORK/bin/fake-derive" "$WORK/bin/fake-capture"

# The derivable-session fixture lives in fake-derive's -list-targets, so an
# un-switched sweep WOULD run.

# --- sweep: switch on → marker, exit 0, zero derive/capture calls ------------
MEMOIRE_AUTO_DERIVE=0 \
MEMOIRE_HOME="$WORK/home" \
MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
MEMOIRE_CAPTURE_CODEX_BIN="$WORK/bin/fake-capture" \
MEMOIRE_CAPTURE_QWEN_BIN="$WORK/bin/fake-capture" \
MEMOIRE_DERIVE_KEY_ENV=HOME \
bash "$SWEEP" || fail "kill-switched sweep must exit 0"
grep -q "sweep skipped=auto_derive_disabled" "$LOG" || fail "sweep marker line missing"
[[ ! -s "$EXEC_LOG" ]] || fail "kill-switched sweep must never invoke derive or capture"

# Control: with the switch explicitly OFF the same environment runs the
# pre-pass and derives (=1 pinned so an ambient operator kill switch cannot
# leak in — bit live 2026-07-22 during the cutover gap).
: > "$LOG"
MEMOIRE_AUTO_DERIVE=1 \
MEMOIRE_DB= \
MEMOIRE_HOME="$WORK/home" \
MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
MEMOIRE_CAPTURE_CODEX_BIN="$WORK/bin/fake-capture" \
MEMOIRE_CAPTURE_QWEN_BIN="$WORK/bin/fake-capture" \
MEMOIRE_DERIVE_KEY_ENV=HOME \
bash "$SWEEP" || fail "control sweep must exit 0"
grep -q "CALLED" "$EXEC_LOG" || fail "control sweep must invoke derive (fixture broken?)"
grep -q "CAPTURE" "$EXEC_LOG" || fail "control sweep must run the capture pre-pass"

# --- SessionEnd hook: switch on → marker, exit 0, zero derive/capture calls --
: > "$LOG"; : > "$EXEC_LOG"
printf '{"session_id":"sess-1","cwd":"/tmp/proj","transcript_path":"/tmp/nonexistent.jsonl"}' | \
MEMOIRE_AUTO_DERIVE=0 \
MEMOIRE_HOME="$WORK/home" \
MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
MEMOIRE_CAPTURE_CC_BIN="$WORK/bin/fake-capture" \
bash "$AUTO" || fail "kill-switched hook must exit 0"
grep -q "auto skipped=auto_derive_disabled" "$LOG" || fail "hook marker line missing"
[[ ! -s "$EXEC_LOG" ]] || fail "kill-switched hook must never invoke derive or capture"

# --- SessionEnd hook inside the attribution fork: silent, exit 0, no worker ---
# The switch is pinned OFF (=1) so only the fork guard can stop the hook. The
# foreground creates its log directory before it detaches the worker, so a
# home that stays absent proves the hook went no further than the guard.
: > "$EXEC_LOG"
printf '{"session_id":"sess-fork","cwd":"/tmp/proj","transcript_path":"/tmp/nonexistent.jsonl","hook_event_name":"SessionEnd"}' | \
MEMOIRE_ATTRIB_FORK=1 \
MEMOIRE_AUTO_DERIVE=1 \
MEMOIRE_HOME="$WORK/fork-home" \
MEMOIRE_CAPTURE_CC_BIN="$WORK/bin/fake-capture" \
bash "$AUTO" || fail "hook inside the attribution fork must exit 0"
[[ ! -e "$WORK/fork-home" ]] || fail "hook inside the attribution fork must do nothing"
[[ ! -s "$EXEC_LOG" ]] || fail "hook inside the attribution fork must never invoke capture"
# Control: the same call without the fork marker goes on to its log directory.
printf '{"session_id":"sess-fork","cwd":"/tmp/proj","transcript_path":"/tmp/nonexistent.jsonl","hook_event_name":"SessionEnd"}' | \
MEMOIRE_ATTRIB_FORK= \
MEMOIRE_AUTO_DERIVE=1 \
MEMOIRE_HOME="$WORK/fork-home" \
MEMOIRE_CAPTURE_CC_BIN="$WORK/bin/fake-capture" \
bash "$AUTO" || fail "control hook must exit 0"
[[ -d "$WORK/fork-home/log" ]] || fail "control hook must reach its log directory (fixture broken?)"

echo "kill-switch-test: OK"
