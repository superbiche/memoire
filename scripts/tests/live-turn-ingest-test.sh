#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# live-turn-ingest-test.sh — regression tests for the live turn-ingest slice
# (2026-07-12) under the R-10.2 notify-only hook (memoire:D165): the
# session checkpoint hook ingests its own transcript TAIL then POSTs a derive
# intent — it never invokes a derive binary or opens the store. The checkpoint
# fires at PreCompact as well as SessionEnd (Fibery #342), and both are
# covered below. The sweep runs a
# codex/qwen capture pre-pass before session selection. All fail-open paths
# are pinned: a missing transcript, a hung adapter, or an unreachable API
# must never block session end (hook exit 0 always).

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AUTO="$ROOT/memoire-derive-auto.sh"
SWEEP="$ROOT/memoire-derive-sweep.sh"
command -v flock >/dev/null || { echo "SKIP: flock not available"; exit 0; }
command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/home/log" "$WORK/home/queue/pending" "$WORK/home/queue/inflight" "$WORK/bin"
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

# One shared exec log records every fake invocation in order, so ordering
# assertions (capture before derive) read straight off it. -list-targets
# serves the sweep's enumeration from a mutable fixture file
# (derive-seam-port: the sweep reads targets through the binary, not
# sqlite3) — adapters that "ingest" a session mid-pre-pass append to it.
TARGETS_FILE="$WORK/targets.txt"
printf 'proj\x1fsess-1\n' > "$TARGETS_FILE"
INTENTS_FILE="$WORK/intents.txt"
: > "$INTENTS_FILE"
cat > "$WORK/bin/fake-derive" <<FAKE
#!/usr/bin/env bash
if [[ " \$* " == *" -list-targets "* ]]; then
  cat "$TARGETS_FILE"
  exit 0
fi
if [[ " \$* " == *" -list-open-intents "* ]]; then
  [[ "\${FAKE_INTENTS_RC:-0}" != "0" ]] && { echo "intent store down" >&2; exit 1; }
  cat "$INTENTS_FILE" 2>/dev/null
  exit 0
fi
if [[ " \$* " == *" -settle-intents "* ]]; then
  echo "settled=0"
  exit 0
fi
echo "DERIVE \$*" >> "$EXEC_LOG"
printf '{\\n  "status": "completed"\\n}\\n'
FAKE
cat > "$WORK/bin/fake-capture" <<FAKE
#!/usr/bin/env bash
echo "CAPTURE \$*" >> "$EXEC_LOG"
FAKE
# The hook worker POSTs the derive intent with curl; a recording fake on
# PATH stands in for the API and answers 200 (the hook reads the -w
# http_code from stdout).
cat > "$WORK/bin/curl" <<FAKE
#!/usr/bin/env bash
echo "NOTIFY \$*" >> "$EXEC_LOG"
printf '200'
FAKE
chmod +x "$WORK/bin/fake-derive" "$WORK/bin/fake-capture" "$WORK/bin/curl"

TRANSCRIPT="$WORK/transcript.jsonl"
printf '{"type":"assistant"}\n' > "$TRANSCRIPT"

# wait_for_line LABEL PATTERN — the hook detaches its worker, so outcomes
# land in the log asynchronously.
wait_for_line() {
  local label="$1" pattern="$2" i
  for i in $(seq 1 30); do
    grep -q "$pattern" "$LOG" 2>/dev/null && return 0
    sleep 0.5
  done
  fail "$label: log line matching '$pattern' never appeared"
}

# MEMOIRE_AUTO_DERIVE=1 and MEMOIRE_DB= are pinned in every invocation so an
# ambient operator kill switch or live DSN can never leak into the harness
# (bit live 2026-07-22: the cutover-gap kill switch silently no-oped cases).
run_auto() { # hook-json extra-env...
  local hook_json="$1"; shift
  printf '%s' "$hook_json" | \
    env PATH="$WORK/bin:$PATH" \
    MEMOIRE_HOME="$WORK/home" \
    MEMOIRE_AUTO_DERIVE=1 \
    MEMOIRE_DERIVE_NOTIFY=1 \
    MEMOIRE_DB= \
    MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
    MEMOIRE_CAPTURE_CC_BIN="$WORK/bin/fake-capture" \
    MEMOIRE_DERIVE_KEY_ENV=HOME \
    "$@" bash "$AUTO" || fail "hook must exit 0"
}

# --- hook: transcript present → capture, then notify (order pinned) --------
run_auto "{\"session_id\":\"sess-a\",\"cwd\":\"/tmp/proj\",\"transcript_path\":\"$TRANSCRIPT\"}"
wait_for_line "hook happy path" "session=sess-a capture=rc=0 drain=ok notify_code=200"
# An absent hook_event_name defaults to SessionEnd (the hook's first life).
grep -q "event=SessionEnd notify project=proj session=sess-a" "$LOG" \
  || fail "a payload without hook_event_name must log event=SessionEnd"
grep -q "CAPTURE --tail $TRANSCRIPT" "$EXEC_LOG" || fail "capture adapter not invoked with --tail and the transcript path"
grep -q "NOTIFY .*/v1/derive/notify" "$EXEC_LOG" || fail "notify not posted after capture"
[[ "$(grep -n 'CAPTURE' "$EXEC_LOG" | head -1 | cut -d: -f1)" -lt \
   "$(grep -n 'NOTIFY' "$EXEC_LOG" | head -1 | cut -d: -f1)" ]] \
  || fail "capture must run before notify"
# The hook is notify-only: no derive binary, no store, and the bearer never
# enters argv (the header travels via process substitution).
! grep -q "DERIVE" "$EXEC_LOG" || fail "notify-only hook must never invoke a derive binary"
! grep -q "test-token" "$EXEC_LOG" || fail "bearer must never appear in curl argv"
grep -q 'NOTIFY .*"session_id":"sess-a"' "$EXEC_LOG" || fail "notify body must carry the session id"
grep -q 'NOTIFY .*"project":"proj"' "$EXEC_LOG" || fail "notify body must carry the resolved project"

# --- hook: PreCompact is a CHECKPOINT, not an ending (Fibery #342) ----------
# Same script, same two actions; the cause rides the log so a compaction burst
# is legible. Exit 0 is load-bearing here: exit 2 would BLOCK compaction.
: > "$LOG"; : > "$EXEC_LOG"
run_auto "{\"session_id\":\"sess-pc\",\"cwd\":\"/tmp/proj\",\"transcript_path\":\"$TRANSCRIPT\",\"hook_event_name\":\"PreCompact\",\"trigger\":\"manual\"}"
wait_for_line "hook precompact" \
  "event=PreCompact cause=manual notify project=proj session=sess-pc capture=rc=0 drain=ok notify_code=200"
grep -q "CAPTURE --tail $TRANSCRIPT" "$EXEC_LOG" || fail "PreCompact capture must run the adapter in --tail mode"
grep -q "NOTIFY .*/v1/derive/notify" "$EXEC_LOG" || fail "PreCompact must post the derive intent too"
[[ "$(grep -n 'CAPTURE' "$EXEC_LOG" | head -1 | cut -d: -f1)" -lt \
   "$(grep -n 'NOTIFY' "$EXEC_LOG" | head -1 | cut -d: -f1)" ]] \
  || fail "PreCompact capture must run before notify"

# --- hook: an explicit SessionEnd carries its reason as the cause -----------
: > "$LOG"; : > "$EXEC_LOG"
run_auto "{\"session_id\":\"sess-se\",\"cwd\":\"/tmp/proj\",\"transcript_path\":\"$TRANSCRIPT\",\"hook_event_name\":\"SessionEnd\",\"reason\":\"clear\"}"
wait_for_line "hook sessionend" \
  "event=SessionEnd cause=clear notify project=proj session=sess-se capture=rc=0 drain=ok notify_code=200"

# --- hook: stuck queue → bounded wait, drain=timeout, notify STILL fires ----
# (Round-4 MED-1 hardening: custody-before-notify is best-effort — a stuck
# forwarder must never block the intent, only mark the drain honestly.)
: > "$LOG"; : > "$EXEC_LOG"
printf '{}' > "$WORK/home/queue/pending/stuck.json"
run_auto "{\"session_id\":\"sess-b\",\"cwd\":\"/tmp/proj\",\"transcript_path\":\"$TRANSCRIPT\"}" \
  MEMOIRE_DRAIN_TIMEOUT=1
wait_for_line "hook drain timeout" "session=sess-b capture=rc=0 drain=timeout pending_left=1 notify_code=200"
grep -q "NOTIFY .*/v1/derive/notify" "$EXEC_LOG" || fail "drain timeout must not suppress the notify"
rm -f "$WORK/home/queue/pending/stuck.json"

# --- hook: no transcript_path → capture skipped, notify still fires ---------
: > "$LOG"; : > "$EXEC_LOG"
run_auto '{"session_id":"sess-c","cwd":"/tmp/proj"}'
wait_for_line "hook without transcript" "session=sess-c capture=skip drain=skip notify_code=200"
! grep -q "CAPTURE" "$EXEC_LOG" || fail "capture must be skipped without a transcript_path"

# --- hook: unreadable transcript → fail-open, notify still fires ------------
: > "$LOG"; : > "$EXEC_LOG"
run_auto "{\"session_id\":\"sess-d\",\"cwd\":\"/tmp/proj\",\"transcript_path\":\"$WORK/missing.jsonl\"}"
wait_for_line "hook unreadable transcript" "session=sess-d capture=skip drain=skip notify_code=200"

# --- hook: hung capture adapter → killed at timeout, notify still fires -----
: > "$LOG"; : > "$EXEC_LOG"
cat > "$WORK/bin/fake-capture-hang" <<'FAKE'
#!/usr/bin/env bash
sleep 30
FAKE
chmod +x "$WORK/bin/fake-capture-hang"
run_auto "{\"session_id\":\"sess-e\",\"cwd\":\"/tmp/proj\",\"transcript_path\":\"$TRANSCRIPT\"}" \
  MEMOIRE_CAPTURE_CC_BIN="$WORK/bin/fake-capture-hang" MEMOIRE_CAPTURE_TIMEOUT=1
wait_for_line "hook hung capture" "session=sess-e capture=rc=124 drain=ok notify_code=200"

# --- hook: invalid MEMOIRE_CAPTURE_TIMEOUT → default, fail-open -------------
: > "$LOG"; : > "$EXEC_LOG"
run_auto "{\"session_id\":\"sess-f\",\"cwd\":\"/tmp/proj\",\"transcript_path\":\"$TRANSCRIPT\"}" \
  MEMOIRE_CAPTURE_TIMEOUT=abc
wait_for_line "hook invalid capture timeout" "session=sess-f capture=rc=0 drain=ok notify_code=200"

# --- hook: API unreachable → exit 0, honest failure marker, no retry loop ---
# (No session-existence probe anymore: the hub decides what an intent means;
# an unreachable API degrades to a logged failure, never a blocked session
# end.)
: > "$LOG"; : > "$EXEC_LOG"
mkdir -p "$WORK/bin-noapi"
printf '#!/usr/bin/env bash\nexit 7\n' > "$WORK/bin-noapi/curl"
chmod +x "$WORK/bin-noapi/curl"
run_auto "{\"session_id\":\"sess-g\",\"cwd\":\"/tmp/proj\",\"transcript_path\":\"$TRANSCRIPT\"}" \
  PATH="$WORK/bin-noapi:$WORK/bin:$PATH"
wait_for_line "hook API down" "session=sess-g capture=rc=0 drain=ok notify_code=curl_failed"

# --- hook: DEFAULT (L0010) → capture only, no derive intent, no drain wait ---
: > "$LOG"; : > "$EXEC_LOG"
run_auto "{\"session_id\":\"sess-l0\",\"cwd\":\"/tmp/proj\",\"transcript_path\":\"$TRANSCRIPT\"}" \
  MEMOIRE_DERIVE_NOTIFY=0
wait_for_line "hook default notify off" "session=sess-l0 capture=rc=0 drain=skip notify_code=off_derivation_retired"
grep -q "CAPTURE --tail $TRANSCRIPT" "$EXEC_LOG" || fail "capture must run with the derive intent off"
! grep -q "NOTIFY" "$EXEC_LOG" || fail "no derive intent may be posted by default (L0010)"

# --- hook: MISSING TOKEN → capture still runs, honest notify skip -----------
# (Round-1 MED-5: curl/token are notify-only prerequisites; their absence
# must never suppress the local transcript capture — custody is local. The
# skipped_no_curl branch shares this exact code path.)
: > "$LOG"; : > "$EXEC_LOG"
run_auto "{\"session_id\":\"sess-h\",\"cwd\":\"/tmp/proj\",\"transcript_path\":\"$TRANSCRIPT\"}" \
  MEMOIRE_TOKEN_FILE="$WORK/no-such-token"
wait_for_line "hook missing token" "session=sess-h capture=rc=0 drain=ok notify_code=skipped_no_token"
grep -q "CAPTURE --tail $TRANSCRIPT" "$EXEC_LOG" || fail "capture must run even without a token"
! grep -q "NOTIFY" "$EXEC_LOG" || fail "notify must be skipped without a token"

# --- sweep: pre-pass invokes both adapters, drain marker, then derives ------
: > "$LOG"; : > "$EXEC_LOG"
cat > "$WORK/bin/fake-capture-fail" <<FAKE
#!/usr/bin/env bash
echo "CAPTUREQWEN \$*" >> "$EXEC_LOG"
echo "boom" >&2
exit 1
FAKE
chmod +x "$WORK/bin/fake-capture-fail"
cat > "$WORK/bin/fake-capture-opencode" <<FAKE
#!/usr/bin/env bash
echo "CAPTUREOPENCODE \$*" >> "$EXEC_LOG"
exit 0
FAKE
chmod +x "$WORK/bin/fake-capture-opencode"
MEMOIRE_HOME="$WORK/home" \
MEMOIRE_AUTO_DERIVE=1 \
MEMOIRE_DERIVE_NOTIFY=1 \
MEMOIRE_DB= \
MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
MEMOIRE_CAPTURE_CODEX_BIN="$WORK/bin/fake-capture" \
MEMOIRE_CAPTURE_QWEN_BIN="$WORK/bin/fake-capture-fail" \
MEMOIRE_CAPTURE_OPENCODE_BIN="$WORK/bin/fake-capture-opencode" \
MEMOIRE_DERIVE_KEY_ENV=HOME \
bash "$SWEEP" || fail "sweep must exit 0"
grep -q "CAPTURE --scan --since 2 hours ago" "$EXEC_LOG" || fail "codex pre-pass not invoked with --scan --since"
grep -q "CAPTUREQWEN --scan --since 2 hours ago" "$EXEC_LOG" || fail "qwen pre-pass not invoked with --scan --since"
grep -q "CAPTUREOPENCODE --scan --since 2 hours ago" "$EXEC_LOG" || fail "opencode pre-pass not invoked with --scan --since"
grep -q "sweep prepass adapter=codex rc=0" "$LOG" || fail "codex pre-pass line missing"
grep -q "sweep prepass adapter=qwen rc=1 boom" "$LOG" || fail "qwen pre-pass failure must be logged, not fatal"
grep -q "sweep prepass adapter=opencode rc=0" "$LOG" || fail "opencode pre-pass line missing"
grep -q "sweep prepass drain=ok" "$LOG" || fail "drain marker missing"
grep -q "DERIVE .*-session-id sess-1" "$EXEC_LOG" || fail "failing adapter must not block the sweep derive"
# Pre-pass must complete before any derive (fresh events must be selectable).
[[ "$(grep -n 'CAPTUREQWEN' "$EXEC_LOG" | head -1 | cut -d: -f1)" -lt \
   "$(grep -n 'DERIVE' "$EXEC_LOG" | head -1 | cut -d: -f1)" ]] \
  || fail "pre-pass must run before session derivation"

# --- sweep: missing adapter binary → skip marker, sweep continues -----------
: > "$LOG"; : > "$EXEC_LOG"
MEMOIRE_HOME="$WORK/home" \
MEMOIRE_AUTO_DERIVE=1 \
MEMOIRE_DERIVE_NOTIFY=1 \
MEMOIRE_DB= \
MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
MEMOIRE_CAPTURE_CODEX_BIN="$WORK/bin/does-not-exist" \
MEMOIRE_CAPTURE_QWEN_BIN="$WORK/bin/fake-capture" \
MEMOIRE_CAPTURE_OPENCODE_BIN="$WORK/bin/fake-capture" \
MEMOIRE_DERIVE_KEY_ENV=HOME \
bash "$SWEEP" || fail "sweep with missing adapter must exit 0"
grep -q "sweep prepass adapter=codex skipped=capture_bin_missing" "$LOG" || fail "missing adapter skip marker absent"
grep -q "DERIVE .*-session-id sess-1" "$EXEC_LOG" || fail "missing adapter must not block the sweep derive"

# --- sweep: hung adapter → killed at timeout (rc=124), sweep continues ------
: > "$LOG"; : > "$EXEC_LOG"
MEMOIRE_HOME="$WORK/home" \
MEMOIRE_AUTO_DERIVE=1 \
MEMOIRE_DERIVE_NOTIFY=1 \
MEMOIRE_DB= \
MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
MEMOIRE_CAPTURE_CODEX_BIN="$WORK/bin/fake-capture-hang" \
MEMOIRE_CAPTURE_QWEN_BIN="$WORK/bin/fake-capture" \
MEMOIRE_CAPTURE_OPENCODE_BIN="$WORK/bin/fake-capture" \
MEMOIRE_CAPTURE_TIMEOUT=1 \
MEMOIRE_DERIVE_KEY_ENV=HOME \
bash "$SWEEP" || fail "sweep with hung adapter must exit 0"
grep -q "sweep prepass adapter=codex rc=124" "$LOG" || fail "hung adapter must be killed at the timeout"
grep -q "DERIVE .*-session-id sess-1" "$EXEC_LOG" || fail "hung adapter must not block the sweep derive"

# --- sweep: invalid MEMOIRE_DRAIN_TIMEOUT → default, sweep continues --------
: > "$LOG"; : > "$EXEC_LOG"
MEMOIRE_HOME="$WORK/home" \
MEMOIRE_AUTO_DERIVE=1 \
MEMOIRE_DERIVE_NOTIFY=1 \
MEMOIRE_DB= \
MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
MEMOIRE_CAPTURE_CODEX_BIN="$WORK/bin/fake-capture" \
MEMOIRE_CAPTURE_QWEN_BIN="$WORK/bin/fake-capture" \
MEMOIRE_CAPTURE_OPENCODE_BIN="$WORK/bin/fake-capture" \
MEMOIRE_DRAIN_TIMEOUT=abc \
MEMOIRE_DERIVE_KEY_ENV=HOME \
bash "$SWEEP" || fail "sweep with invalid drain timeout must exit 0"
grep -q "sweep prepass drain=ok" "$LOG" || fail "invalid drain timeout must fall back, not abort"
grep -q "DERIVE .*-session-id sess-1" "$EXEC_LOG" || fail "invalid drain timeout must not block the sweep derive"

# --- sweep: pre-pass runs before SESSION SELECTION, not merely before derive -
# An adapter that ingests a brand-new session mid-pre-pass must see that
# session derived in the SAME tick (the fake inserts straight into the store,
# standing in for capture -> queue -> drainer).
: > "$LOG"; : > "$EXEC_LOG"
cat > "$WORK/bin/fake-capture-insert" <<FAKE
#!/usr/bin/env bash
echo "CAPTUREINS \$*" >> "$EXEC_LOG"
printf 'proj\\x1fsess-new\\n' >> "$TARGETS_FILE"
FAKE
chmod +x "$WORK/bin/fake-capture-insert"
MEMOIRE_HOME="$WORK/home" \
MEMOIRE_AUTO_DERIVE=1 \
MEMOIRE_DERIVE_NOTIFY=1 \
MEMOIRE_DB= \
MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
MEMOIRE_CAPTURE_CODEX_BIN="$WORK/bin/fake-capture-insert" \
MEMOIRE_CAPTURE_QWEN_BIN="$WORK/bin/fake-capture" \
MEMOIRE_CAPTURE_OPENCODE_BIN="$WORK/bin/fake-capture" \
MEMOIRE_DERIVE_KEY_ENV=HOME \
bash "$SWEEP" || fail "sweep must exit 0"
grep -q "DERIVE .*-session-id sess-new" "$EXEC_LOG" || fail "session ingested by the pre-pass must be derived in the same tick"
# Every sweep invocation above must override ALL THREE adapter bins — a missing
# override silently falls back to the real adapter against live user state
# (ortho review 2026-07-13, MAJOR). Sentinel: the real adapters never run here.
grep -q "memoire-capture-opencode.sh" "$LOG" && fail "real opencode adapter must never run inside this test"

# --- sweep: intents are SCHEDULING INPUT (round-2 MED-2 harness) ------------
# An intent-only session outside the enumeration derives FIRST; a session in
# both lists derives exactly once; an intent-listing failure logs its marker
# and degrades to plain enumeration.
: > "$LOG"; : > "$EXEC_LOG"
printf 'proj2\x1fsess-i\nproj\x1fsess-1\n' > "$INTENTS_FILE"
env MEMOIRE_HOME="$WORK/home" \
  MEMOIRE_AUTO_DERIVE=1 \
  MEMOIRE_DERIVE_NOTIFY=1 \
  MEMOIRE_DB= \
  MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
  MEMOIRE_CAPTURE_CODEX_BIN="$WORK/bin/fake-capture" \
  MEMOIRE_CAPTURE_QWEN_BIN="$WORK/bin/fake-capture" \
  MEMOIRE_CAPTURE_OPENCODE_BIN="$WORK/bin/fake-capture" \
  MEMOIRE_DERIVE_KEY_ENV=HOME \
  bash "$SWEEP" || fail "sweep with intents must exit 0"
grep -q "DERIVE .*-session-id sess-i" "$EXEC_LOG" || fail "intent-only session must be derived"
[[ "$(grep -n 'DERIVE .*-session-id sess-i' "$EXEC_LOG" | head -1 | cut -d: -f1)" -lt \
   "$(grep -n 'DERIVE .*-session-id sess-1' "$EXEC_LOG" | head -1 | cut -d: -f1)" ]] \
  || fail "intent must be derived BEFORE the enumerated target"
[[ "$(grep -c 'DERIVE .*-execute .*-session-id sess-1' "$EXEC_LOG")" -eq 1 ]] \
  || fail "a session in both lists must execute exactly once"

: > "$LOG"; : > "$EXEC_LOG"
env MEMOIRE_HOME="$WORK/home" \
  MEMOIRE_AUTO_DERIVE=1 \
  MEMOIRE_DERIVE_NOTIFY=1 \
  MEMOIRE_DB= \
  FAKE_INTENTS_RC=1 \
  MEMOIRE_DERIVE_BIN="$WORK/bin/fake-derive" \
  MEMOIRE_CAPTURE_CODEX_BIN="$WORK/bin/fake-capture" \
  MEMOIRE_CAPTURE_QWEN_BIN="$WORK/bin/fake-capture" \
  MEMOIRE_CAPTURE_OPENCODE_BIN="$WORK/bin/fake-capture" \
  MEMOIRE_DERIVE_KEY_ENV=HOME \
  bash "$SWEEP" || fail "sweep with failing intent list must exit 0"
grep -q "sweep intents=list_failed" "$LOG" || fail "intent-list failure marker missing"
grep -q "DERIVE .*-session-id sess-1" "$EXEC_LOG" || fail "intent-list failure must degrade to enumeration, not skip"

echo "live-turn-ingest-test: OK"
