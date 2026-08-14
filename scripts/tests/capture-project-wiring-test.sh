#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# capture-project-wiring-test.sh — proves the LIVE capture adapters route
# project attribution through the shared resolver (liaison:D238 project-identity
# slice, 2026-07-18). Covers memoire-capture-text.sh, the codex/qwen live
# wrappers that exec it, and the CC hook adapter memoire-capture-claude.sh.
#
# Pure and offline: a stub queue bin captures the -project argument, a fixture
# registry drives resolution, nothing writes to Memoire or the live registry.
# The rollout backfills' wiring is covered by turn-ingest-adapter-test.sh and
# opencode-capture-test.sh; the resolver core by resolve-project-test.sh.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TEXT_SCRIPT="$ROOT/scripts/memoire-capture-text.sh"
CODEX_SCRIPT="$ROOT/scripts/memoire-capture-codex.sh"
QWEN_SCRIPT="$ROOT/scripts/memoire-capture-qwen.sh"
CLAUDE_SCRIPT="$ROOT/scripts/memoire-capture-claude.sh"

command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }

fail() { echo "FAIL: $1" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Stub queue bin: echo the -project value the adapter resolved.
STUB="$TMP/memoire-queue"
cat > "$STUB" <<'STUBEOF'
#!/usr/bin/env bash
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "-project" ]]; then printf 'PROJECT=%s\n' "${2:-}"; fi
  shift
done
exit 0
STUBEOF
chmod +x "$STUB"

REG="$TMP/registry.json"
cat > "$REG" <<'JSON'
{
  "schema": "liaison-memoire-projects/1",
  "projects": {
    "client-projfoo": { "display": "projfoo", "paths": ["/home/user/projfoo"] }
  }
}
JSON

export MEMOIRE_QUEUE_BIN="$STUB"
export MEMOIRE_PROJECTS_REGISTRY="$REG"
unset MEMOIRE_PROJECT

proj_of() { sed -n 's/^PROJECT=//p' <<<"$1" | head -1; }

# --- memoire-capture-text.sh --------------------------------------------------
# Registry hit: CWD_VALUE (=MEMOIRE_CWD) resolves through the registry.
out="$(MEMOIRE_CWD=/home/user/projfoo "$TEXT_SCRIPT" --text hi 2>/dev/null)"
[[ "$(proj_of "$out")" == "client-projfoo" ]] \
  || fail "text: registry hit expected client-projfoo, got '$(proj_of "$out")'"

# Basename fail-open for an unmapped dir.
out="$(MEMOIRE_CWD=/home/user/otherproj "$TEXT_SCRIPT" --text hi 2>/dev/null)"
[[ "$(proj_of "$out")" == "otherproj" ]] \
  || fail "text: unmapped dir must fail-open to basename, got '$(proj_of "$out")'"

# MEMOIRE_PROJECT env override wins and is not double-applied.
out="$(MEMOIRE_PROJECT=forced MEMOIRE_CWD=/home/user/projfoo "$TEXT_SCRIPT" --text hi 2>/dev/null)"
[[ "$(proj_of "$out")" == "forced" ]] \
  || fail "text: MEMOIRE_PROJECT override must win, got '$(proj_of "$out")'"

# --- codex / qwen live wrappers (exec memoire-capture-text.sh) ----------------
# The wrappers must NOT pre-set MEMOIRE_PROJECT to basename (that would defeat
# the registry). MEMOIRE_CWD is the launch/session dir here.
out="$(MEMOIRE_CWD=/home/user/projfoo "$CODEX_SCRIPT" --text hi 2>/dev/null)"
[[ "$(proj_of "$out")" == "client-projfoo" ]] \
  || fail "codex wrapper: registry hit expected client-projfoo, got '$(proj_of "$out")'"
out="$(MEMOIRE_CWD=/home/user/projfoo "$QWEN_SCRIPT" --text hi 2>/dev/null)"
[[ "$(proj_of "$out")" == "client-projfoo" ]] \
  || fail "qwen wrapper: registry hit expected client-projfoo, got '$(proj_of "$out")'"

# --- memoire-capture-claude.sh (CC hook) --------------------------------------
# The hook resolves from the payload .cwd (session home).
out="$(printf '{"cwd":"/home/user/projfoo","hook_event_name":"PostToolUse","session_id":"s1"}' \
  | "$CLAUDE_SCRIPT" 2>/dev/null)"
[[ "$(proj_of "$out")" == "client-projfoo" ]] \
  || fail "claude: registry hit expected client-projfoo, got '$(proj_of "$out")'"

out="$(printf '{"cwd":"/home/user/otherproj","hook_event_name":"PostToolUse","session_id":"s1"}' \
  | "$CLAUDE_SCRIPT" 2>/dev/null)"
[[ "$(proj_of "$out")" == "otherproj" ]] \
  || fail "claude: unmapped dir must fail-open to basename, got '$(proj_of "$out")'"

out="$(printf '{"cwd":"/home/user/projfoo","hook_event_name":"PostToolUse","session_id":"s1"}' \
  | MEMOIRE_PROJECT=forced "$CLAUDE_SCRIPT" 2>/dev/null)"
[[ "$(proj_of "$out")" == "forced" ]] \
  || fail "claude: MEMOIRE_PROJECT override must win, got '$(proj_of "$out")'"

# Fail-open when the resolver script is missing: capture still gets basename.
out="$(MEMOIRE_RESOLVE_PROJECT=/no/such/resolver.sh MEMOIRE_CWD=/home/user/projfoo \
  "$TEXT_SCRIPT" --text hi 2>/dev/null)"
[[ "$(proj_of "$out")" == "projfoo" ]] \
  || fail "text: missing resolver must fail-open to basename, got '$(proj_of "$out")'"

# --- liaison:D239 kill switches -------------------------------------------------------
# Session-level env gates the whole adapter run — even with a broken resolver
# (the env check is adapter-local by design).
out="$(MEMOIRE_CAPTURE=off MEMOIRE_CWD=/home/user/projfoo "$TEXT_SCRIPT" --text hi 2>/dev/null)"
[[ -z "$out" ]] || fail "text: MEMOIRE_CAPTURE=off must capture nothing, got '$out'"
out="$(MEMOIRE_CAPTURE=off MEMOIRE_RESOLVE_PROJECT=/no/such/resolver.sh \
  MEMOIRE_CWD=/home/user/projfoo "$TEXT_SCRIPT" --text hi 2>/dev/null)"
[[ -z "$out" ]] || fail "text: MEMOIRE_CAPTURE=off must gate even without resolver, got '$out'"
out="$(printf '{"cwd":"/home/user/projfoo","hook_event_name":"PostToolUse","session_id":"s1"}' \
  | MEMOIRE_CAPTURE=off MEMOIRE_CLAUDE_HOOK_LOG="$TMP/hook.log" "$CLAUDE_SCRIPT" 2>/dev/null)"
[[ -z "$out" ]] || fail "claude: MEMOIRE_CAPTURE=off must capture nothing, got '$out'"

# Repo-level: capture: false in the nearest .memoire gates via the resolver,
# and MEMOIRE_PROJECT does not bypass it (override and toggle are orthogonal).
KOFF="$TMP/killrepo"
mkdir -p "$KOFF/sub"
printf 'project: killrepo-key\ncapture: false\n' > "$KOFF/.memoire"
out="$(MEMOIRE_CWD="$KOFF" "$TEXT_SCRIPT" --text hi 2>/dev/null)"
[[ -z "$out" ]] || fail "text: .memoire capture:false must capture nothing, got '$out'"
out="$(MEMOIRE_CWD="$KOFF/sub" "$TEXT_SCRIPT" --text hi 2>/dev/null)"
[[ -z "$out" ]] || fail "text: .memoire capture:false must gate from subdir, got '$out'"
out="$(MEMOIRE_PROJECT=forced MEMOIRE_CWD="$KOFF" "$TEXT_SCRIPT" --text hi 2>/dev/null)"
[[ -z "$out" ]] || fail "text: MEMOIRE_PROJECT must not bypass capture:false, got '$out'"
out="$(printf '{"cwd":"%s","hook_event_name":"PostToolUse","session_id":"s1"}' "$KOFF" \
  | MEMOIRE_CLAUDE_HOOK_LOG="$TMP/hook.log" "$CLAUDE_SCRIPT" 2>/dev/null)"
[[ -z "$out" ]] || fail "claude: .memoire capture:false must capture nothing, got '$out'"

# A .memoire WITHOUT capture: false must not gate — and its project: key wins.
MON="$TMP/memrepo"
mkdir -p "$MON"
printf 'project: memrepo-key\n' > "$MON/.memoire"
out="$(MEMOIRE_CWD="$MON" "$TEXT_SCRIPT" --text hi 2>/dev/null)"
[[ "$(proj_of "$out")" == "memrepo-key" ]] \
  || fail "text: .memoire project key must win, got '$(proj_of "$out")'"

echo "capture-project-wiring-test: OK"
