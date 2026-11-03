#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# inject-transport-backlog-test.sh — regression test for the L4(b)
# dead-transport escalation (memoire:D198): a transport backlog whose OLDEST
# pending age crosses the dead-transport threshold must surface as the
# distinct, imperative TRANSPORT DEAD block (replacing the operational
# CAPTURE BACKLOG line), while younger backlogs keep the pre-existing
# operational behavior and a healthy queue stays quiet. All blocks under
# test are local-only (no token, no network — they precede the curl/token
# preflights by contract), so no mock server is needed.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INJECT="$ROOT/scripts/memoire-inject-claude.sh"

command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }

fail() { echo "FAIL: $1" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Walk-up poison pre-check (same guard as resolve-project-test): a real
# .memoire above the tmp dir would change attribution, not these blocks,
# but keep the harness hermetic anyway.
d="$TMP"
while [[ "$d" != "/" ]]; do
  d="$(dirname "$d")"
  [[ -f "$d/.memoire" ]] && fail "pre-check: real .memoire file at $d/.memoire poisons this test env"
done

mkdir -p "$TMP/home/queue/pending" "$TMP/home/queue/failed" "$TMP/proj"
# NO token file: the script must emit the local-only blocks and fail open
# before any network read — exactly the remote-thin-client shape the
# backlog surface exists for (D153 slice-3 round-1 finding 2).

run_inject() { # extra env assignments as "K=V" args
  env -i PATH="$PATH" HOME="$TMP" \
    MEMOIRE_HOME="$TMP/home" \
    MEMOIRE_INJECT=on \
    MEMOIRE_PROJECTS_REGISTRY="$TMP/registry-absent.json" \
    "$@" \
    bash "$INJECT" <<<"{\"cwd\":\"$TMP/proj\"}"
}

# Case 1: oldest pending age beyond the dead-transport threshold (7h old,
# default threshold 6h) — the TRANSPORT DEAD block fires and REPLACES the
# operational CAPTURE BACKLOG line. Falsified against the pre-L4 injector:
# it emitted only the operational line for this fixture.
touch -d '7 hours ago' "$TMP/home/queue/pending/old-event.json"
out="$(run_inject)" || fail "inject must exit 0 (case 1), got rc=$?"
grep -q 'TRANSPORT DEAD' <<<"$out" || fail "7h-old backlog must escalate to TRANSPORT DEAD, got: $out"
grep -q 'tell the operator in your FIRST reply' <<<"$out" || fail "escalation must be imperative, got: $out"
grep -q '1 events stuck local->hub for 7h' <<<"$out" || fail "escalation must carry count and human age, got: $out"
grep -q 'CAPTURE BACKLOG' <<<"$out" && fail "escalation must replace the operational line, got: $out"

# Case 2: age above the operational threshold but below dead-transport
# (20 min) — pre-existing behavior unchanged: operational line, no
# escalation.
touch -d '20 minutes ago' "$TMP/home/queue/pending/old-event.json"
out="$(run_inject)" || fail "inject must exit 0 (case 2)"
grep -q 'CAPTURE BACKLOG' <<<"$out" || fail "20min backlog must keep the operational line, got: $out"
grep -q 'TRANSPORT DEAD' <<<"$out" && fail "20min backlog must not escalate, got: $out"

# Case 3: fresh pending file — quiet steady state, neither block.
touch "$TMP/home/queue/pending/old-event.json"
out="$(run_inject)" || fail "inject must exit 0 (case 3)"
grep -q 'CAPTURE BACKLOG\|TRANSPORT DEAD' <<<"$out" && fail "fresh backlog must stay quiet, got: $out"

# Case 4: an invalid threshold knob falls back to the default rather than
# breaking the arithmetic — 7h-old file still escalates.
touch -d '7 hours ago' "$TMP/home/queue/pending/old-event.json"
out="$(run_inject MEMOIRE_INJECT_TRANSPORT_DEAD_S=banana)" || fail "inject must exit 0 (case 4)"
grep -q 'TRANSPORT DEAD' <<<"$out" || fail "invalid knob must fall back to the 21600s default, got: $out"

# Case 5: an explicit threshold override wins — 20min-old backlog
# escalates under a 60s threshold (escalation takes precedence over the
# operational line for the same fixture).
touch -d '20 minutes ago' "$TMP/home/queue/pending/old-event.json"
out="$(run_inject MEMOIRE_INJECT_TRANSPORT_DEAD_S=60)" || fail "inject must exit 0 (case 5)"
grep -q 'TRANSPORT DEAD' <<<"$out" || fail "explicit threshold must win, got: $out"
grep -q 'CAPTURE BACKLOG' <<<"$out" && fail "escalation must replace the operational line (case 5), got: $out"

# Case 6: the block kill switch silences both tiers.
touch -d '7 hours ago' "$TMP/home/queue/pending/old-event.json"
out="$(run_inject MEMOIRE_INJECT_BACKLOG=0)" || fail "inject must exit 0 (case 6)"
grep -q 'CAPTURE BACKLOG\|TRANSPORT DEAD' <<<"$out" && fail "kill switch must silence both tiers, got: $out"

# Case 7 (round-1 finding 4): an invalid OPERATIONAL knob must fail open —
# rc=0 and the block still functions on the fallback default (20min-old
# file: age >= 600s fires the operational line). The pre-fix injector
# aborted the block under set -u here.
touch -d '20 minutes ago' "$TMP/home/queue/pending/old-event.json"
out="$(run_inject MEMOIRE_INJECT_BACKLOG_MIN=banana)" || fail "invalid operational knob must not break inject (rc)"
grep -q 'CAPTURE BACKLOG' <<<"$out" || fail "invalid knob must fall back and still warn, got: $out"
out="$(run_inject MEMOIRE_INJECT_BACKLOG_AGE_S=banana)" || fail "invalid age knob must not break inject (rc)"
grep -q 'CAPTURE BACKLOG' <<<"$out" || fail "invalid age knob must fall back and still warn, got: $out"

# Case 8 (round-1 finding 4): an int64-overflowing threshold must fall
# back to the default, not wrap to zero and cry TRANSPORT DEAD over a
# fresh queue.
touch "$TMP/home/queue/pending/old-event.json"
out="$(run_inject MEMOIRE_INJECT_TRANSPORT_DEAD_S=18446744073709551616)" || fail "overflow knob must not break inject (rc)"
grep -q 'TRANSPORT DEAD' <<<"$out" && fail "overflow knob must not wrap to zero, got: $out"

echo "inject-transport-backlog-test: OK"
