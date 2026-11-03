#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# inject-off-test.sh — session-start injection is opt-in. With MEMOIRE_INJECT
# unset (or anything but on|1|true) the injector prints nothing, calls nothing
# (a stub curl records every call and a FIFO stands in for the token file: a
# read would block), and exits 0; the same through the qwen envelope wrapper.
# MEMOIRE_INJECT=on restores the output. The switch is read like the other
# opt-in switches: $MEMOIRE_HOME/capture-tick.env wins when it carries the key,
# otherwise the process environment decides. The OpenCode plugin leg lives in
# opencode-plugin-test.mjs.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INJECT="$ROOT/scripts/memoire-inject-claude.sh"
ENVELOPE="$ROOT/scripts/memoire-inject-envelope.sh"
command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }
command -v timeout >/dev/null || { echo "SKIP: timeout not available"; exit 0; }

fail() { echo "FAIL: $1" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

d="$TMP"
while [[ "$d" != "/" ]]; do
  d="$(dirname "$d")"
  [[ -f "$d/.memoire" ]] && fail "pre-check: real .memoire file at $d/.memoire poisons this test env"
done

# Stub curl: records the call, answers like the API would.
mkdir -p "$TMP/bin" "$TMP/home/queue/pending" "$TMP/proj"
cat > "$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_LOG"
case "$*" in
  *"/v1/health"*) echo '{"clock_skew_future_events":0}' ;;
  *"/v1/sessions"*) echo '{"sessions":[{"proposed_count":2,"started_at_epoch":1}]}' ;;
  *"can_instruct=true"*) echo '{"hits":[{"id":9,"kind":"decision","captured_at_epoch":1700000000,"snippet":"standing order"}]}' ;;
  *) echo '{"hits":[{"id":7,"kind":"fact","review_state":"accepted","captured_at_epoch":1700000000,"snippet":"hello"}]}' ;;
esac
STUB
chmod +x "$TMP/bin/curl"
export STUB_LOG="$TMP/curl.log"
: > "$STUB_LOG"

# Token file = FIFO without a writer: any read of it blocks.
mkfifo "$TMP/home/token"
# Deep, old backlog: the health header would fire if the injector got that far.
for i in $(seq 1 30); do : > "$TMP/home/queue/pending/$i.json"; done
touch -d '3 days ago' "$TMP/home/queue/pending/"*.json
printf 'project: off-proj\ninject_content: false\n' > "$TMP/proj/.memoire"

run() { # run <injector> [env assignments...] -> stdout; rc in RC
  local script="$1"; shift
  set +e
  OUT="$(printf '{"cwd":"%s"}' "$TMP/proj" | env -i PATH="$TMP/bin:$PATH" HOME="$TMP" TMPDIR="$TMP" \
    MEMOIRE_HOME="$TMP/home" MEMOIRE_BASE_URL="http://127.0.0.1:9" \
    MEMOIRE_PROJECTS_REGISTRY="$TMP/registry-absent.json" STUB_LOG="$STUB_LOG" \
    "$@" timeout -k 1 8 bash "$script" 2>"$TMP/err")"
  RC=$?
  set -e
}
assert_silent() { # label
  [[ "$RC" == 0 ]] || fail "$1: exit $RC, want 0 (a blocked token read exits 124)"
  [[ -z "$OUT" ]] || fail "$1: printed output: $OUT"
  [[ ! -s "$TMP/err" ]] || fail "$1: stderr: $(cat "$TMP/err")"
  [[ ! -s "$STUB_LOG" ]] || fail "$1: curl was called: $(cat "$STUB_LOG")"
}

# --- 1. injector, switch unset / not on: nothing, no call, no token read ------
run "$INJECT";                          assert_silent "unset"
for v in "" off 0 false no junk; do
  run "$INJECT" "MEMOIRE_INJECT=$v";    assert_silent "MEMOIRE_INJECT='$v'"
done
# Existing knobs do not switch it on.
run "$INJECT" MEMOIRE_INJECT_NOTES=1 MEMOIRE_INJECT_REVIEW=1 MEMOIRE_INJECT_CONTENT=on MEMOIRE_INJECT_BACKLOG=1
assert_silent "legacy knobs only"

# --- 2. envelope: nothing on stdout either (no additionalContext) -------------
run "$ENVELOPE";                        assert_silent "envelope, unset"
run "$ENVELOPE" MEMOIRE_INJECT=off;     assert_silent "envelope, off"

# --- 3. env file wins when it carries the key ---------------------------------
printf '# comment\nexport MEMOIRE_INJECT = "off"  # note\n' > "$TMP/home/capture-tick.env"
run "$INJECT" MEMOIRE_INJECT=on;        assert_silent "env file off beats env on"
printf 'MEMOIRE_INJECT=on\nMEMOIRE_INJECT=off\n' > "$TMP/home/capture-tick.env"
run "$INJECT" MEMOIRE_INJECT=on;        assert_silent "env file last assignment wins"
printf 'MEMOIRE_INJECT_NOTES=on\n' > "$TMP/home/capture-tick.env"
run "$INJECT";                          assert_silent "env file with a sibling key only"
rm -f "$TMP/home/capture-tick.env"

# --- 4. on: the output is back (fixtures swapped for a readable token) --------
rm -f "$TMP/home/token"; printf 'tok\n' > "$TMP/home/token"
printf 'project: on-proj\n' > "$TMP/proj/.memoire"
EXPECT_PROJ='# [memoire] TRANSPORT DEAD — tell the operator in your FIRST reply'
for v in on 1 TRUE; do
  : > "$STUB_LOG"
  run "$INJECT" "MEMOIRE_INJECT=$v"
  [[ "$RC" == 0 ]] || fail "on ($v): exit $RC"
  grep -qF "$EXPECT_PROJ" <<<"$OUT" || fail "on ($v): TRANSPORT DEAD header missing: $OUT"
  grep -qF '# [memoire] review backlog — on-proj' <<<"$OUT" || fail "on ($v): review header missing: $OUT"
  grep -qF -- '- [7] fact 2023-11-14: hello' <<<"$OUT" || fail "on ($v): recent context missing: $OUT"
  grep -qF '# [memoire] operating notes — on-proj' <<<"$OUT" || fail "on ($v): operating notes missing: $OUT"
  [[ -s "$STUB_LOG" ]] || fail "on ($v): the stub was never called"
done
# on through the env file with the process environment unset.
printf 'MEMOIRE_INJECT=on\n' > "$TMP/home/capture-tick.env"
run "$INJECT"
grep -qF -- '- [7] fact' <<<"$OUT" || fail "env file on: recent context missing: $OUT"
rm -f "$TMP/home/capture-tick.env"
# Existing knobs keep working when on.
run "$INJECT" MEMOIRE_INJECT=on MEMOIRE_INJECT_NOTES=0 MEMOIRE_INJECT_REVIEW=0 MEMOIRE_INJECT_BACKLOG=0
grep -qF 'operating notes' <<<"$OUT" && fail "on + NOTES=0: notes printed"
grep -qF 'review backlog' <<<"$OUT" && fail "on + REVIEW=0: review header printed"
grep -qF 'TRANSPORT DEAD' <<<"$OUT" && fail "on + BACKLOG=0: backlog header printed"
grep -qF -- '- [7] fact' <<<"$OUT" || fail "on + knobs: recent context missing"

# --- 5. envelope on: the JSON envelope --------------------------------------
mkdir -p "$TMP/wrap"; cp "$ENVELOPE" "$INJECT" "$TMP/wrap/"
cp "$ROOT/scripts/memoire-resolve-project.sh" "$TMP/wrap/" 2>/dev/null || true
run "$TMP/wrap/memoire-inject-envelope.sh" MEMOIRE_INJECT=on
jq -e '.hookSpecificOutput.hookEventName == "SessionStart" and (.hookSpecificOutput.additionalContext | contains("recent context"))' <<<"$OUT" >/dev/null \
  || fail "envelope on: wrong envelope: $OUT"

echo "inject-off-test: OK"
