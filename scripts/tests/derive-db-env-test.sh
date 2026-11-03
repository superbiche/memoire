#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# derive-db-env-test.sh — regression tests for the derive-lane MEMOIRE_DB
# sourcing added by derive-seam-port (round-1 MED-1/LOW-2 repairs): the
# SWEEP must (1) export a db.env-sourced DSN to the derive binary verbatim
# (no shell-eval of credential metacharacters), (2) let an explicit
# MEMOIRE_DB env win over the file, (3) log a list_targets_failed marker
# instead of exiting silently when the store is unreachable, and (4) keep
# the MEMOIRE_AUTO_DERIVE=0 instant-exit contract independent of the env
# file — even when the file is a FIFO that would block any reader.
# The SessionEnd HOOK is notify-only since R-10.2 (memoire:D165): it must
# never read db.env or invoke a derive binary at all — pinned here with the
# FIFO env file present on an ENABLED run.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SWEEP="$ROOT/memoire-derive-sweep.sh"
AUTO="$ROOT/memoire-derive-auto.sh"
command -v flock >/dev/null || { echo "SKIP: flock not available"; exit 0; }
command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/home/log" "$WORK/bin"
LOG="$WORK/home/log/derive-auto.log"
EXEC_LOG="$WORK/exec.log"
touch "$EXEC_LOG"
printf 'test-token\n' > "$WORK/home/token"

fail() {
  echo "FAIL: $1" >&2
  echo "--- log:" >&2
  cat "$LOG" >&2 || true
  echo "--- exec log:" >&2
  cat "$EXEC_LOG" >&2 || true
  exit 1
}

# The DSN fixture carries shell metacharacters that would break on any
# eval/source-style read: $, &, ;, spaces are all inert under grep|cut.
DSN='postgres://memoire:p$a&s;s wd@192.0.2.1:5433/memoire'
printf 'MEMOIRE_DB=%s\n' "$DSN" > "$WORK/home/db.env"

# fake-derive records the MEMOIRE_DB it observes on every -list-targets call
# and serves one target; -median-epoch is unused here; anything else logs
# DERIVE (the hook case asserts the env reached the actual derive call too).
cat > "$WORK/bin/fake-derive" <<FAKE
#!/usr/bin/env bash
if [[ " \$* " == *" -list-targets "* ]]; then
  echo "LISTDB=[\${MEMOIRE_DB:-}]" >> "$EXEC_LOG"
  printf 'proj\\x1fsess-1\\n'
  exit 0
fi
if [[ " \$* " == *" -list-open-intents "* ]]; then
  exit 0
fi
if [[ " \$* " == *" -settle-intents "* ]]; then
  echo "settled=0"
  exit 0
fi
echo "DERIVEDB=[\${MEMOIRE_DB:-}] \$*" >> "$EXEC_LOG"
printf '{\\n  "prompt_bytes": 100\\n}\\n'
FAKE
cat > "$WORK/bin/noop-capture" <<'FAKE'
#!/usr/bin/env bash
exit 0
FAKE
# The hook probes the API with curl before deriving; a fake stands in for a
# healthy server that knows the session.
cat > "$WORK/bin/curl" <<'FAKE'
#!/usr/bin/env bash
exit 0
FAKE
chmod +x "$WORK/bin/fake-derive" "$WORK/bin/noop-capture" "$WORK/bin/curl"

run_sweep() { # extra-env...
  env MEMOIRE_HOME="$WORK/home" \
    MEMOIRE_AUTO_DERIVE=1 \
    MEMOIRE_DERIVE_NOTIFY=1 \
    MEMOIRE_DB= \
    MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
    MEMOIRE_CAPTURE_CODEX_BIN="$WORK/bin/noop-capture" \
    MEMOIRE_CAPTURE_QWEN_BIN="$WORK/bin/noop-capture" \
    MEMOIRE_CAPTURE_OPENCODE_BIN="$WORK/bin/noop-capture" \
    MEMOIRE_DERIVE_KEY_ENV=HOME \
    MEMOIRE_SWEEP_HOURS=99999999 \
    "$@" bash "$SWEEP"
}

# --- sweep: db.env-sourced DSN reaches the binary verbatim ------------------
run_sweep || fail "sweep must exit 0"
grep -qF "LISTDB=[$DSN]" "$EXEC_LOG" || fail "file-sourced DSN must reach -list-targets verbatim"
grep -qF "DERIVEDB=[$DSN]" "$EXEC_LOG" || fail "file-sourced DSN must reach the derive call verbatim"

# --- sweep: explicit MEMOIRE_DB env wins over the file ----------------------
: > "$EXEC_LOG"; : > "$LOG"
run_sweep MEMOIRE_DB="$WORK/home/explicit.db" || fail "sweep must exit 0"
grep -qF "LISTDB=[$WORK/home/explicit.db]" "$EXEC_LOG" \
  || fail "explicit MEMOIRE_DB must win over db.env"

# --- sweep: unreachable store logs list_targets_failed, exits 0 -------------
: > "$EXEC_LOG"; : > "$LOG"
cat > "$WORK/bin/fake-derive-fail" <<'FAKE'
#!/usr/bin/env bash
if [[ " $* " == *" -list-targets "* ]]; then
  echo "store unreachable: connection refused" >&2
  exit 1
fi
echo "DERIVE-MUST-NOT-RUN" >> "${EXEC_LOG_PATH:?}"
FAKE
chmod +x "$WORK/bin/fake-derive-fail"
# Mandatory-enumeration honesty (preflight audit finding 3, extends
# D166): a sweep that cannot enumerate must exit NON-ZERO after its
# marker — the old exit-0 pin let HL5 record lane success over zero
# enumerated work.
if EXEC_LOG_PATH="$EXEC_LOG" run_sweep MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive-fail"; then
  fail "sweep with failing enumeration must exit non-zero"
fi
grep -q "sweep skipped=list_targets_failed" "$LOG" || fail "list_targets_failed marker missing"
grep -q "connection refused" "$LOG" || fail "failure detail must be logged (sanitized)"
grep -q "DERIVE-MUST-NOT-RUN" "$EXEC_LOG" && fail "failed enumeration must not derive"

# --- sweep: kill switch exits instantly even when the env file is a FIFO ----
: > "$EXEC_LOG"; : > "$LOG"
mkfifo "$WORK/fifo.env"
timeout 5 env MEMOIRE_HOME="$WORK/home" \
  MEMOIRE_AUTO_DERIVE=0 \
  MEMOIRE_DB= \
  MEMOIRE_DB_ENV_FILE="$WORK/fifo.env" \
  MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
  MEMOIRE_DERIVE_KEY_ENV=HOME \
  bash "$SWEEP" || fail "kill-switched sweep must exit 0 instantly with a FIFO env file"
grep -q "sweep skipped=auto_derive_disabled" "$LOG" || fail "sweep kill marker missing"
[[ ! -s "$EXEC_LOG" ]] || fail "kill-switched sweep must not invoke the binary"

# --- hook: notify-only — never reads db.env, never invokes the binary -------
# (R-10.2, memoire:D165.) The FIFO env file is present on an ENABLED run: a
# hook that still opened db.env would block on it; timeout 5 turns that into
# a loud failure. The recording curl proves the intent POST replaced the
# derive call.
: > "$EXEC_LOG"; : > "$LOG"
TRANSCRIPT="$WORK/transcript.jsonl"
printf '{"type":"assistant"}\n' > "$TRANSCRIPT"
cat > "$WORK/bin/curl" <<FAKE
#!/usr/bin/env bash
echo "NOTIFY \$*" >> "$EXEC_LOG"
printf '200'
FAKE
chmod +x "$WORK/bin/curl"
printf '{"session_id":"sess-h","cwd":"/tmp/proj","transcript_path":"%s"}' "$TRANSCRIPT" | \
  timeout 5 env PATH="$WORK/bin:$PATH" \
  MEMOIRE_HOME="$WORK/home" \
  MEMOIRE_AUTO_DERIVE=1 \
  MEMOIRE_DERIVE_NOTIFY=1 \
  MEMOIRE_DB= \
  MEMOIRE_DB_ENV_FILE="$WORK/fifo.env" \
  MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
  MEMOIRE_CAPTURE_CC_BIN="$WORK/bin/noop-capture" \
  bash "$AUTO" || fail "enabled hook must exit 0 without touching the FIFO env file"
for i in $(seq 1 30); do
  grep -q "NOTIFY .*/v1/derive/notify" "$EXEC_LOG" 2>/dev/null && break
  sleep 0.5
done
grep -q "NOTIFY .*/v1/derive/notify" "$EXEC_LOG" || fail "hook must post the derive intent"
grep -q "DERIVEDB\|DERIVE " "$EXEC_LOG" && fail "notify-only hook must never invoke the derive binary"

# --- hook: kill switch exits instantly even when the env file is a FIFO -----
: > "$EXEC_LOG"; : > "$LOG"
printf '{"session_id":"sess-i","cwd":"/tmp/proj","transcript_path":"%s"}' "$TRANSCRIPT" | \
  timeout 5 env PATH="$WORK/bin:$PATH" \
  MEMOIRE_HOME="$WORK/home" \
  MEMOIRE_AUTO_DERIVE=0 \
  MEMOIRE_DB= \
  MEMOIRE_DB_ENV_FILE="$WORK/fifo.env" \
  MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
  MEMOIRE_CAPTURE_CC_BIN="$WORK/bin/noop-capture" \
  bash "$AUTO" || fail "kill-switched hook must exit 0 instantly with a FIFO env file"
grep -q "auto skipped=auto_derive_disabled" "$LOG" || fail "hook kill marker missing"
[[ ! -s "$EXEC_LOG" ]] || fail "kill-switched hook must not capture or notify"

echo "derive-db-env-test: OK"
