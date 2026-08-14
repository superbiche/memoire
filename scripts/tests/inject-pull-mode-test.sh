#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# inject-pull-mode-test.sh — regression test for pull-based retrieval v0:
# `inject_content: false` in the project's .memoire (or session-level
# MEMOIRE_INJECT_CONTENT=off) suppresses the recent-context CONTENT block and
# emits the search-on-subject rule instead. STATE surfaces are unaffected:
# operating notes keep riding, and the rule block must appear even when the
# API is unreachable (static text, fail-open discipline).
#
# Live-content cases use `python3 -m http.server` as a static mock: the http
# path /v1/timeline maps to the file v1/timeline regardless of query string,
# so one fixture file serves both the recent-context and operating-notes
# reads. SKIP-guarded like every optional tool in this suite.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INJECT="$ROOT/scripts/memoire-inject-claude.sh"

command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }
command -v curl >/dev/null || { echo "SKIP: curl not available"; exit 0; }

fail() { echo "FAIL: $1" >&2; exit 1; }

TMP="$(mktemp -d)"
MOCK_PID=""
BH_PID=""
LIM_PID=""
cleanup() {
  local p
  for p in "$MOCK_PID" "$BH_PID" "$LIM_PID"; do
    if [[ -n "$p" ]]; then
      kill "$p" 2>/dev/null || true
      wait "$p" 2>/dev/null || true
    fi
  done
  rm -rf "$TMP"
}
trap cleanup EXIT

# Walk-up poison pre-check (same guard as resolve-project-test).
d="$TMP"
while [[ "$d" != "/" ]]; do
  d="$(dirname "$d")"
  [[ -f "$d/.memoire" ]] && fail "pre-check: real .memoire file at $d/.memoire poisons this test env"
done

mkdir -p "$TMP/home"
printf 'test-token\n' > "$TMP/home/token"
export MEMOIRE_HOME="$TMP/home"
export MEMOIRE_PROJECTS_REGISTRY="$TMP/registry.json"
echo '{"schema":"liaison-memoire-projects/1","projects":{}}' > "$TMP/registry.json"
unset MEMOIRE_PROJECT MEMOIRE_CAPTURE MEMOIRE_INJECT_CONTENT 2>/dev/null || true

FLAGGED="$TMP/flagged"
PLAIN="$TMP/plain"
mkdir -p "$FLAGGED" "$PLAIN"
printf 'project: pull-proj\ninject_content: false\n' > "$FLAGGED/.memoire"
printf 'project: push-proj\n' > "$PLAIN/.memoire"

run_inject() { # cwd -> stdout (MEMOIRE_BASE_URL from caller env)
  printf '{"cwd":"%s","hook_event_name":"SessionStart","session_id":"s1"}' "$1" \
    | "$INJECT" 2>/dev/null
}

DEAD_URL="http://127.0.0.1:1"

## ---- offline (dead API): the rule is static text and must still ride ----

out="$(MEMOIRE_BASE_URL="$DEAD_URL" run_inject "$FLAGGED")"
grep -q 'pull mode' <<<"$out" || fail "flagged+dead API must emit pull-mode block, got: $out"
grep -q 'once the work subject is clear' <<<"$out" || fail "pull block must carry the deferred-search rule, got: $out"
if grep -q 'recent context' <<<"$out"; then
  fail "flagged project must not emit recent-context block, got: $out"
fi
if grep -q 'review backlog' <<<"$out"; then
  fail "dead API must not emit a review-backlog header (fail-open), got: $out"
fi

out="$(MEMOIRE_BASE_URL="$DEAD_URL" run_inject "$PLAIN")"
if grep -q 'pull mode' <<<"$out"; then
  fail "unflagged project must not emit pull-mode block, got: $out"
fi

# Session-level override gates like the repo-level flag.
out="$(MEMOIRE_INJECT_CONTENT=off MEMOIRE_BASE_URL="$DEAD_URL" run_inject "$PLAIN")"
grep -q 'pull mode' <<<"$out" || fail "MEMOIRE_INJECT_CONTENT=off must emit pull-mode block, got: $out"

# Missing read token (remote-capture client topology): the rule is local
# text and must ride anyway, clean exit (round-1 review finding).
mkdir -p "$TMP/home-notoken"
out="$(MEMOIRE_HOME="$TMP/home-notoken" MEMOIRE_BASE_URL="$DEAD_URL" run_inject "$FLAGGED")" \
  || fail "flagged+no-token must exit 0"
grep -q 'pull mode' <<<"$out" || fail "flagged+no-token must still emit pull-mode block, got: $out"

## ---- live mock: content suppressed, operating notes keep riding ----

command -v python3 >/dev/null || { echo "inject-pull-mode-test: OK (live-mock cases SKIPPED: python3 not available)"; exit 0; }

mkdir -p "$TMP/docroot/v1"
cat > "$TMP/docroot/v1/timeline" <<'JSON'
{"hits":[
  {"id":101,"kind":"fact","review_state":"accepted","captured_at_epoch":1785312000,"snippet":"sentinel-observation-body"},
  {"id":102,"kind":"decision","review_state":"accepted","captured_at_epoch":1785312000,"snippet":"second-sentinel-body"}
]}
JSON
cat > "$TMP/docroot/v1/sessions" <<'JSON'
{"sessions":[
  {"session_id":"s-a","project":"push-proj","client":"cc","started_at_epoch":1785312000,"observation_count":9,"proposed_count":3,"lapsed_count":0},
  {"session_id":"s-b","project":"push-proj","client":"cc","started_at_epoch":1785225600,"observation_count":5,"proposed_count":2,"lapsed_count":1}
]}
JSON

MOCK_PORT=""
for _ in 1 2 3 4 5; do
  cand=$(( (RANDOM % 20000) + 20000 ))
  ( cd "$TMP/docroot" && exec python3 -m http.server "$cand" --bind 127.0.0.1 ) >/dev/null 2>&1 &
  MOCK_PID=$!
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    if curl -sf -m 1 "http://127.0.0.1:$cand/v1/timeline" >/dev/null 2>&1; then
      MOCK_PORT="$cand"
      break
    fi
    kill -0 "$MOCK_PID" 2>/dev/null || break
    sleep 0.2
  done
  [[ -n "$MOCK_PORT" ]] && break
  kill "$MOCK_PID" 2>/dev/null || true
  wait "$MOCK_PID" 2>/dev/null || true
  MOCK_PID=""
done
[[ -n "$MOCK_PORT" ]] || { echo "inject-pull-mode-test: OK (live-mock cases SKIPPED: no free port for mock server)"; exit 0; }
MOCK_URL="http://127.0.0.1:$MOCK_PORT"

# Unflagged + live data: content block present with the hit lines, no rule.
out="$(MEMOIRE_BASE_URL="$MOCK_URL" run_inject "$PLAIN")"
grep -q 'recent context' <<<"$out" || fail "unflagged+live must emit recent context, got: $out"
grep -q 'sentinel-observation-body' <<<"$out" || fail "unflagged+live must carry hit content, got: $out"
grep -q 'operating notes' <<<"$out" || fail "unflagged+live must emit operating notes (mock serves can_instruct read too), got: $out"
if grep -q 'pull mode' <<<"$out"; then
  fail "unflagged+live must not emit pull-mode block, got: $out"
fi

# Review-backlog header (memoire:D192): push state, one line, counts
# summed from the sessions surface; rides in BOTH modes; killable.
out="$(MEMOIRE_BASE_URL="$MOCK_URL" run_inject "$PLAIN")"
grep -q 'review backlog' <<<"$out" || fail "unflagged+live must emit review-backlog header, got: $out"
grep -q '5 proposed observations across 2 sessions' <<<"$out" || fail "review backlog must sum proposed counts, got: $out"
out="$(MEMOIRE_INJECT_REVIEW=0 MEMOIRE_BASE_URL="$MOCK_URL" run_inject "$PLAIN")"
if grep -q 'review backlog' <<<"$out"; then
  fail "MEMOIRE_INJECT_REVIEW=0 must suppress the review-backlog header, got: $out"
fi
# Zero proposed on a SUCCESSFUL response must stay silent (round-1 review
# finding 4: only the non-zero path was asserted). The static server
# re-reads fixtures per request, so swap the file and restore it.
cp "$TMP/docroot/v1/sessions" "$TMP/sessions.bak"
printf '{"sessions":[]}' > "$TMP/docroot/v1/sessions"
out="$(MEMOIRE_BASE_URL="$MOCK_URL" run_inject "$PLAIN")"
if grep -q 'review backlog' <<<"$out"; then
  fail "empty sessions response must not emit a review-backlog header, got: $out"
fi
printf '{"sessions":[{"session_id":"s-z","project":"push-proj","client":"cc","started_at_epoch":1785312000,"observation_count":4,"proposed_count":0,"lapsed_count":0}]}' > "$TMP/docroot/v1/sessions"
out="$(MEMOIRE_BASE_URL="$MOCK_URL" run_inject "$PLAIN")"
if grep -q 'review backlog' <<<"$out"; then
  fail "all-zero proposed counts must not emit a review-backlog header, got: $out"
fi
cp "$TMP/sessions.bak" "$TMP/docroot/v1/sessions"

# Flagged + live data: NO observation content, rule present, notes UNTOUCHED.
out="$(MEMOIRE_BASE_URL="$MOCK_URL" run_inject "$FLAGGED")"
grep -q 'pull mode' <<<"$out" || fail "flagged+live must emit pull-mode block, got: $out"
if grep -q 'recent context' <<<"$out"; then
  fail "flagged+live must not emit recent-context block, got: $out"
fi
grep -q 'operating notes' <<<"$out" || fail "flagged+live must keep operating notes riding (state stays push), got: $out"
grep -q 'review backlog' <<<"$out" || fail "review backlog is STATE and must ride in pull mode too, got: $out"
# The notes block legitimately carries observation text (operator-elevated);
# the RECENT-CONTEXT content must not leak outside it. Strip the notes block
# and assert no sentinel remains.
stripped="$(awk '/^# \[memoire\] operating notes/{skip=1} skip==0{print}' <<<"$out")"
if grep -q 'sentinel-observation-body' <<<"$stripped"; then
  fail "flagged+live leaked recent-context content outside the notes block, got: $out"
fi

# Notes overflow guard (memoire:D191): with the cap below the served hit
# count the header must warn loudly and trim to the cap — a silently
# evicted standing order is the failure mode this guards. The mock serves
# 2 hits; cap 1 → overflow warning + only the first hit shown.
out="$(MEMOIRE_INJECT_NOTES_LIMIT=1 MEMOIRE_BASE_URL="$MOCK_URL" run_inject "$PLAIN")"
grep -q 'OVERFLOW: more than 1 elevated' <<<"$out" || fail "notes overflow must warn in the header, got: $out"
notes_block="$(awk '/^# \[memoire\] operating notes/{skip=1} skip==1{print}' <<<"$out")"
grep -q 'sentinel-observation-body' <<<"$notes_block" || fail "overflow must still show the newest notes, got: $out"
if grep -q 'second-sentinel-body' <<<"$notes_block"; then
  fail "overflow must trim notes to the cap, got: $out"
fi
# No overflow at the default cap: normal header, no warning.
out="$(MEMOIRE_BASE_URL="$MOCK_URL" run_inject "$PLAIN")"
if grep -q 'OVERFLOW' <<<"$out"; then
  fail "default cap must not warn with 2 elevated hits, got: $out"
fi

# Boundary: the cap must stay within what the backend can actually
# return for cap+1. The timeline's normalizeLimit FALLS BACK to 50 on an
# over-max (>500) request, so an unclamped MEMOIRE_INJECT_NOTES_LIMIT=500
# would fetch 501, receive 50, and silently lose notes with a normal
# header. This mock honors ?limit= with the server's exact fallback
# semantics and holds 60 hits: with the 499 clamp the injector must
# render all 60 under a normal header.
python3 - "$TMP/lim-port" >/dev/null 2>&1 <<'PY' &
import http.server, json, sys, urllib.parse
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        q = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        try:
            limit = int(q.get("limit", ["20"])[0])
        except ValueError:
            limit = 20
        if limit <= 0 or limit > 500:
            limit = 50  # normalizeLimit(., 50, 500) fallback semantics
        hits = [{"id": i, "kind": "fact", "review_state": "accepted",
                 "captured_at_epoch": 1785312000, "snippet": "note-%d" % i}
                for i in range(1, min(limit, 60) + 1)]
        body = json.dumps({"hits": hits}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a):
        pass
srv = http.server.HTTPServer(("127.0.0.1", 0), H)
with open(sys.argv[1], "w") as f:
    f.write(str(srv.server_address[1]))
srv.serve_forever()
PY
LIM_PID=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do
  [[ -s "$TMP/lim-port" ]] && break
  sleep 0.2
done
if [[ -s "$TMP/lim-port" ]]; then
  LIM_URL="http://127.0.0.1:$(cat "$TMP/lim-port")"
  out="$(MEMOIRE_INJECT_NOTES_LIMIT=500 MEMOIRE_BASE_URL="$LIM_URL" run_inject "$PLAIN")"
  grep -q '60 elevated observations' <<<"$out" \
    || fail "cap 500 must clamp to 499 and render all 60 notes (fallback-to-50 lost notes silently), got: $out"
  if grep -q 'OVERFLOW' <<<"$out"; then
    fail "60 notes under a 499 cap must not warn, got: $out"
  fi
  # Overflow at the boundary neighborhood: cap 59 against 60 hits.
  out="$(MEMOIRE_INJECT_NOTES_LIMIT=59 MEMOIRE_BASE_URL="$LIM_URL" run_inject "$PLAIN")"
  grep -q 'OVERFLOW: more than 59 elevated' <<<"$out" || fail "cap 59 against 60 hits must warn, got: $out"
  kill "$LIM_PID" 2>/dev/null || true
  wait "$LIM_PID" 2>/dev/null || true
  LIM_PID=""
else
  kill "$LIM_PID" 2>/dev/null || true
  LIM_PID=""
  echo "note: limit-honoring mock did not start; boundary cases skipped"
fi

# Notes kill switch still works in pull mode (independent toggles).
out="$(MEMOIRE_INJECT_NOTES=0 MEMOIRE_BASE_URL="$MOCK_URL" run_inject "$FLAGGED")"
if grep -q 'operating notes' <<<"$out"; then
  fail "MEMOIRE_INJECT_NOTES=0 must suppress notes in pull mode too, got: $out"
fi
grep -q 'pull mode' <<<"$out" || fail "pull block must survive notes-off, got: $out"

## ---- blackholed hub: rule survives, aggregate deadline held ----

# A listener that completes the TCP handshake (kernel backlog) but never
# responds: each curl hangs until its -m bound. Without the aggregate
# budget three 2s hangs overrun a 5s hook deadline (round-1 review
# finding); with it the script must finish under `timeout 5` and still
# carry the rule.
command -v timeout >/dev/null || { echo "inject-pull-mode-test: OK (blackhole case SKIPPED: timeout not available)"; exit 0; }
python3 - "$TMP/bh-port" >/dev/null 2>&1 <<'PY' &
import socket, sys, time
s = socket.socket()
s.bind(("127.0.0.1", 0))
s.listen(8)
with open(sys.argv[1], "w") as f:
    f.write(str(s.getsockname()[1]))
time.sleep(60)
PY
BH_PID=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do
  [[ -s "$TMP/bh-port" ]] && break
  sleep 0.2
done
if [[ ! -s "$TMP/bh-port" ]]; then
  kill "$BH_PID" 2>/dev/null || true
  echo "inject-pull-mode-test: OK (blackhole case SKIPPED: listener did not start)"
  exit 0
fi
BH_URL="http://127.0.0.1:$(cat "$TMP/bh-port")"
out="$(printf '{"cwd":"%s","hook_event_name":"SessionStart","session_id":"s1"}' "$FLAGGED" \
  | timeout 5 env MEMOIRE_BASE_URL="$BH_URL" "$INJECT" 2>/dev/null)" \
  || fail "flagged+blackholed hub must finish inside the 5s hook deadline with exit 0"
grep -q 'pull mode' <<<"$out" || fail "flagged+blackholed hub must still emit pull-mode block, got: $out"

# TIMEOUT_S >= BUDGET_S (round-2 finding): a raised per-call timeout must
# be capped to the remaining aggregate budget, not honored as-is — with a
# start-only guard MEMOIRE_INJECT_TIMEOUT=10 hangs the first request 10s
# and the hook deadline discards everything. `timeout 5` IS the production
# SessionStart deadline (round-3 finding: a looser 6s here masked
# boundary overruns).
out="$(printf '{"cwd":"%s","hook_event_name":"SessionStart","session_id":"s1"}' "$FLAGGED" \
  | timeout 5 env MEMOIRE_BASE_URL="$BH_URL" MEMOIRE_INJECT_TIMEOUT=10 "$INJECT" 2>/dev/null)" \
  || fail "TIMEOUT_S>=BUDGET_S must be capped to the aggregate budget (exit 0 under timeout 5)"
grep -q 'pull mode' <<<"$out" || fail "capped-timeout case must still emit pull-mode block, got: $out"

# Delayed local work (round-3 finding): a slow prelude (cold filesystem,
# busy process table — emulated by a resolver wrapper sleeping 1s per
# call, and the injector calls it twice) must draw down the budget so the
# first network request is already capped; the whole run still fits the
# production deadline.
cat > "$TMP/slow-resolver.sh" <<SH
#!/usr/bin/env bash
sleep 1
exec "$ROOT/scripts/memoire-resolve-project.sh" "\$@"
SH
chmod +x "$TMP/slow-resolver.sh"
out="$(printf '{"cwd":"%s","hook_event_name":"SessionStart","session_id":"s1"}' "$FLAGGED" \
  | timeout 5 env MEMOIRE_BASE_URL="$BH_URL" MEMOIRE_INJECT_TIMEOUT=10 \
      MEMOIRE_RESOLVE_PROJECT="$TMP/slow-resolver.sh" "$INJECT" 2>/dev/null)" \
  || fail "slow local prelude + blackhole must stay inside the 5s deadline (exit 0)"
grep -q 'pull mode' <<<"$out" || fail "slow-prelude case must still emit pull-mode block, got: $out"

# Mid-budget start (round-2 finding): a request that STARTS inside the
# budget must not be allowed its full per-call timeout past the budget
# end. TIMEOUT_S=3 with the default 4s budget: first call runs 3s, the
# second starts at ~3s and must be capped to ~1s — start-only gating
# would run it 3 more seconds and blow a 5s deadline.
out="$(printf '{"cwd":"%s","hook_event_name":"SessionStart","session_id":"s1"}' "$FLAGGED" \
  | timeout 5 env MEMOIRE_BASE_URL="$BH_URL" MEMOIRE_INJECT_TIMEOUT=3 "$INJECT" 2>/dev/null)" \
  || fail "mid-budget request must be capped to the remaining budget (exit 0 under timeout 5)"
grep -q 'pull mode' <<<"$out" || fail "mid-budget case must still emit pull-mode block, got: $out"

kill "$BH_PID" 2>/dev/null || true
wait "$BH_PID" 2>/dev/null || true

echo "inject-pull-mode-test: OK"
