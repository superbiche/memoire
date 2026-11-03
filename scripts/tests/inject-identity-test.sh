#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# inject-identity-test.sh — regression test for the injector's liaison:D239 identity
# surfaces: ask-on-missing (.memoire-less git repo proposes the file with a
# convention-suggested key) and resolver-keyed attribution. Pure and offline:
# fixture repos and token under a tmp dir, MEMOIRE_ADDR points at a dead
# port so the timeline blocks fail open; only the identity block is asserted.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INJECT="$ROOT/scripts/memoire-inject-claude.sh"

command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }
command -v git >/dev/null || { echo "SKIP: git not available"; exit 0; }

fail() { echo "FAIL: $1" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Walk-up poison pre-check (same guard as resolve-project-test).
d="$TMP"
while [[ "$d" != "/" ]]; do
  d="$(dirname "$d")"
  [[ -f "$d/.memoire" ]] && fail "pre-check: real .memoire file at $d/.memoire poisons this test env"
done

mkdir -p "$TMP/home"
printf 'test-token\n' > "$TMP/home/token"
export MEMOIRE_HOME="$TMP/home"
export MEMOIRE_INJECT=on                 # injection is opt-in; this suite tests its content
export MEMOIRE_ADDR="127.0.0.1:1"        # dead port: timeline blocks fail open
export MEMOIRE_PROJECTS_REGISTRY="$TMP/registry.json"
export MEMOIRE_KEY_PREFIXES="$TMP/dev"
echo '{"schema":"liaison-memoire-projects/1","projects":{}}' > "$TMP/registry.json"
unset MEMOIRE_PROJECT MEMOIRE_CAPTURE 2>/dev/null || true

run_inject() { # cwd -> stdout
  printf '{"cwd":"%s","hook_event_name":"SessionStart","session_id":"s1"}' "$1" \
    | "$INJECT" 2>/dev/null
}

# 1. Git repo under the prefix, no .memoire: proposes the conventional key.
REPO="$TMP/dev/acme/stack"
mkdir -p "$REPO/sub"
git -C "$REPO" init -q
out="$(run_inject "$REPO")"
grep -q 'no .memoire file' <<<"$out" || fail "repo without .memoire must carry ask block, got: $out"
grep -q 'project: acme-stack' <<<"$out" || fail "suggested key must be acme-stack, got: $out"
grep -q "$REPO/.memoire" <<<"$out" || fail "proposal must name the git root .memoire path, got: $out"

# 2. From a subdir the git root (not the subdir) anchors the suggestion.
out="$(run_inject "$REPO/sub")"
grep -q 'project: acme-stack' <<<"$out" || fail "subdir must suggest the git-root key, got: $out"

# 3. Repo outside the prefixes: proposal without a conventional key.
OUT_REPO="$TMP/elsewhere/proj"
mkdir -p "$OUT_REPO"
git -C "$OUT_REPO" init -q
out="$(run_inject "$OUT_REPO")"
grep -q 'no .memoire file' <<<"$out" || fail "outside-prefix repo must still ask, got: $out"
grep -q 'an explicit `project:` key' <<<"$out" || fail "outside-prefix must not invent a key, got: $out"

# 4. Repo WITH .memoire: no ask block.
printf 'project: keyed-repo\n' > "$REPO/.memoire"
out="$(run_inject "$REPO")"
if grep -q 'no .memoire file' <<<"$out"; then
  fail "repo with .memoire must not ask, got: $out"
fi

# 5. Non-repo dir: no ask block (repos only).
PLAIN="$TMP/dev/plain"
mkdir -p "$PLAIN"
out="$(run_inject "$PLAIN")"
if grep -q 'no .memoire file' <<<"$out"; then
  fail "non-repo dir must not ask, got: $out"
fi

# 6. MEMOIRE_PROJECT env: no ask block (explicit override, nothing missing).
out="$(MEMOIRE_PROJECT=forced run_inject "$OUT_REPO")"
if grep -q 'no .memoire file' <<<"$out"; then
  fail "env override must not ask, got: $out"
fi

echo "inject-identity-test: OK"
