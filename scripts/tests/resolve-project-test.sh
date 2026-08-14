#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# resolve-project-test.sh — regression test for memoire-resolve-project.sh
# (liaison:D238 registry slice + liaison:D239 `.memoire` v2 slice). Pure and offline: builds
# fixture registry, repos, and `.memoire` files under a tmp dir and exercises
# the resolver over collision families, walk-up precedence, kill switches,
# and fail-open paths. Never reads the live registry or DB.
#
# Hermeticity: every queried path lives under $TMP so the `.memoire` walk-up
# only ever sees fixture files (v1 used real-looking /home paths; the v2
# walk-up made that filesystem-sensitive).

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$ROOT/scripts/memoire-resolve-project.sh"

command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }

fail() { echo "FAIL: $1" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# The walk-up climbs to /; a stray .memoire FILE above $TMP would poison
# expectations. Fail loudly rather than mysteriously.
d="$TMP"
while [[ "$d" != "/" ]]; do
  d="$(dirname "$d")"
  [[ -f "$d/.memoire" ]] && fail "pre-check: real .memoire file at $d/.memoire poisons this test env"
done

DEV="$TMP/dev"
REG="$TMP/registry.json"

cat > "$REG" <<JSON
{
  "schema": "liaison-memoire-projects/1",
  "projects": {
    "personal-infra":  { "display": "infra", "paths": ["$DEV/personal/infra"] },
    "otherorg-infra": { "display": "infra", "paths": ["$DEV/otherorg/infra"] },
    "vps9-apps":     { "display": "apps",  "paths": ["$DEV/personal/infra/servers/vps9/apps"] },
    "vps9":          { "display": "vps9","paths": ["$DEV/personal/infra/servers/vps9"] },
    "cctime":          { "display": "cctime","paths": ["$DEV/personal/cctime", "$TMP/tools/cctime"] },
    "scratch-workspace": { "display": "workspace", "paths": ["$DEV/agentrh/results/workspace"], "scratch": true }
  }
}
JSON

mkdir -p "$DEV/personal/infra/servers/vps9/apps" \
         "$DEV/personal/infra/servers/vps9/monitoring" \
         "$DEV/otherorg/infra" "$DEV/personal/infrastructure" \
         "$DEV/personal/cctime" "$TMP/tools/cctime" \
         "$DEV/agentrh/results/workspace/sub" "$DEV/personal/someproj"

export MEMOIRE_PROJECTS_REGISTRY="$REG"
unset MEMOIRE_PROJECT MEMOIRE_CAPTURE 2>/dev/null || true

expect() {
  local got want desc
  desc="$1"; want="$2"; shift 2
  got="$("$SCRIPT" "$@")"
  [[ "$got" == "$want" ]] || fail "$desc: expected '$want', got '$got'"
  echo "ok: $desc -> $got"
}

## ---- liaison:D238 registry behavior (rebased under $TMP, semantics unchanged) ----

expect "personal infra"        personal-infra  "$DEV/personal/infra"
expect "otherorg infra"       otherorg-infra "$DEV/otherorg/infra"
expect "vps9 apps (nested)"  vps9-apps     "$DEV/personal/infra/servers/vps9/apps"
expect "vps9 host"           vps9          "$DEV/personal/infra/servers/vps9"
expect "vps9 descendant"     vps9          "$DEV/personal/infra/servers/vps9/monitoring"
expect "no false prefix"       infrastructure  "$DEV/personal/infrastructure"
expect "cctime dev home"       cctime          "$DEV/personal/cctime"
expect "cctime tool home"      cctime          "$TMP/tools/cctime"
expect "trailing slash"        personal-infra  "$DEV/personal/infra/"
expect "session-home preferred" otherorg-infra "$DEV/personal/infra" "$DEV/otherorg/infra"
expect "empty arg2 -> arg1"     personal-infra  "$DEV/personal/infra" ""

GOT="$(MEMOIRE_PROJECT=forced "$SCRIPT" "$DEV/personal/infra")"
[[ "$GOT" == "forced" ]] || fail "env override: got '$GOT'"
echo "ok: env override -> $GOT"

expect "unmapped -> basename" someproj "$DEV/personal/someproj"
expect "scratch entry resolves to id" scratch-workspace "$DEV/agentrh/results/workspace"
expect "scratch descendant resolves to id" scratch-workspace "$DEV/agentrh/results/workspace/sub"

GOT="$(MEMOIRE_PROJECTS_REGISTRY=/no/such/file.json "$SCRIPT" "$DEV/personal/infra")"
[[ "$GOT" == "infra" ]] || fail "missing registry fail-open: got '$GOT'"
echo "ok: missing registry -> $GOT"

echo 'not json{' > "$TMP/bad.json"
GOT="$(MEMOIRE_PROJECTS_REGISTRY="$TMP/bad.json" "$SCRIPT" "$DEV/otherorg/infra")"
[[ "$GOT" == "infra" ]] || fail "invalid registry fail-open: got '$GOT'"
echo "ok: invalid registry -> $GOT"

echo '{"projects":[]}' > "$TMP/wrongshape.json"
GOT="$(MEMOIRE_PROJECTS_REGISTRY="$TMP/wrongshape.json" "$SCRIPT" "$DEV/otherorg/infra")"
[[ "$GOT" == "infra" ]] || fail "wrong-shape fail-open: got '$GOT'"
echo "ok: wrong-shape registry -> $GOT"

GOT="$(bash -c 'source "$1"; memoire_resolve_project "$2"' _ "$SCRIPT" "$DEV/personal/cctime")"
[[ "$GOT" == "cctime" ]] || fail "sourceable: got '$GOT'"
echo "ok: sourceable -> $GOT"

## ---- liaison:D239 `.memoire` walk-up ----

# Repo with a .memoire whose key differs from both registry and basename.
MREPO="$DEV/personal/infra"                 # registry says personal-infra
printf 'project: dotfile-wins\n' > "$MREPO/.memoire"
expect ".memoire beats registry"      dotfile-wins "$MREPO"
expect ".memoire from nested subdir"  dotfile-wins "$MREPO/servers/vps9/monitoring"

# Wait — nested subdir under a DEEPER registry entry: .memoire still wins
# (walk-up precedence is absolute over registry, per liaison:D239 precedence order).
expect ".memoire beats deeper registry entry" dotfile-wins "$MREPO/servers/vps9/apps"

# Env override still beats .memoire.
GOT="$(MEMOIRE_PROJECT=forced "$SCRIPT" "$MREPO")"
[[ "$GOT" == "forced" ]] || fail "env beats .memoire: got '$GOT'"
echo "ok: env beats .memoire -> $GOT"

# Session-home attribution: walk-up runs from the home dir (arg2), not the
# roaming event cwd (arg1).
expect ".memoire via session-home" dotfile-wins "$DEV/otherorg/infra" "$MREPO"

# Whitespace-tolerant parsing.
printf '  project:   spaced-key  \n' > "$DEV/personal/someproj/.memoire"
expect ".memoire whitespace tolerated" spaced-key "$DEV/personal/someproj"

# .memoire without a project: line falls through (registry, then basename).
printf 'capture: false\n' > "$DEV/otherorg/infra/.memoire"
expect ".memoire without project: falls through to registry" otherorg-infra "$DEV/otherorg/infra"

# Garbled .memoire falls through, never errors.
printf '\x00\x01garbage' > "$DEV/personal/cctime/.memoire"
expect "garbled .memoire falls through" cctime "$DEV/personal/cctime"
rm -f "$DEV/personal/cctime/.memoire"

# A DIRECTORY named .memoire (like the live service home ~/.memoire) must
# not match the walk-up.
mkdir -p "$DEV/agentrh/results/workspace/.memoire"
expect "dir named .memoire ignored" scratch-workspace "$DEV/agentrh/results/workspace"

## ---- liaison:D239 capture kill switches ----

# Default: enabled.
GOT="$("$SCRIPT" --capture-enabled "$DEV/personal/cctime")"
[[ "$GOT" == "on" ]] || fail "capture default on: got '$GOT'"
echo "ok: capture default -> $GOT"

# Repo-level: capture: false in nearest .memoire (otherorg/infra fixture).
if "$SCRIPT" --capture-enabled "$DEV/otherorg/infra" >/dev/null; then
  fail "capture: false must gate (exit 1)"
fi
echo "ok: .memoire capture:false -> off"

# ...including from a subdir via walk-up.
mkdir -p "$DEV/otherorg/infra/deep"
if "$SCRIPT" --capture-enabled "$DEV/otherorg/infra/deep" >/dev/null; then
  fail "capture: false must gate from subdir"
fi
echo "ok: .memoire capture:false from subdir -> off"

# Session-level env, all accepted spellings.
for v in off 0 false; do
  if MEMOIRE_CAPTURE="$v" "$SCRIPT" --capture-enabled "$DEV/personal/cctime" >/dev/null; then
    fail "MEMOIRE_CAPTURE=$v must gate"
  fi
done
echo "ok: MEMOIRE_CAPTURE=off|0|false -> off"

# MEMOIRE_PROJECT does NOT bypass capture-off (orthogonal switches).
if MEMOIRE_PROJECT=forced "$SCRIPT" --capture-enabled "$DEV/otherorg/infra" >/dev/null; then
  fail "MEMOIRE_PROJECT must not bypass capture: false"
fi
echo "ok: MEMOIRE_PROJECT does not bypass capture-off"

# A .memoire with project but no capture line: enabled.
GOT="$("$SCRIPT" --capture-enabled "$MREPO")"
[[ "$GOT" == "on" ]] || fail "project-only .memoire capture on: got '$GOT'"
echo "ok: project-only .memoire -> capture on"

# Sourceable gate.
if ! bash -c 'source "$1"; memoire_capture_enabled "$2"' _ "$SCRIPT" "$DEV/personal/cctime"; then
  fail "sourceable memoire_capture_enabled must return 0 when enabled"
fi
echo "ok: sourceable capture gate"

## ---- pull-based retrieval v0: inject_content gate ----

# Default: content injection enabled.
GOT="$("$SCRIPT" --inject-content-enabled "$DEV/personal/cctime")"
[[ "$GOT" == "on" ]] || fail "inject-content default on: got '$GOT'"
echo "ok: inject-content default -> $GOT"

# Repo-level flag, including walk-up from a subdir.
PULLREPO="$DEV/personal/someproj"
printf 'project: pulled\ninject_content: false\n' > "$PULLREPO/.memoire"
mkdir -p "$PULLREPO/deep"
if "$SCRIPT" --inject-content-enabled "$PULLREPO" >/dev/null; then
  fail "inject_content: false must gate (exit 1)"
fi
if "$SCRIPT" --inject-content-enabled "$PULLREPO/deep" >/dev/null; then
  fail "inject_content: false must gate from subdir"
fi
echo "ok: .memoire inject_content:false -> off (root and subdir)"

# Session-level env, all accepted spellings.
for v in off 0 false; do
  if MEMOIRE_INJECT_CONTENT="$v" "$SCRIPT" --inject-content-enabled "$DEV/personal/cctime" >/dev/null; then
    fail "MEMOIRE_INJECT_CONTENT=$v must gate"
  fi
done
echo "ok: MEMOIRE_INJECT_CONTENT=off|0|false -> off"

# Orthogonality: inject_content does not touch capture, and capture: false
# does not touch content injection (otherorg/infra fixture has capture:
# false only).
GOT="$("$SCRIPT" --capture-enabled "$PULLREPO")"
[[ "$GOT" == "on" ]] || fail "inject_content flag must not disable capture: got '$GOT'"
GOT="$("$SCRIPT" --inject-content-enabled "$DEV/otherorg/infra")"
[[ "$GOT" == "on" ]] || fail "capture: false must not disable content injection: got '$GOT'"
echo "ok: capture and inject_content gates are orthogonal"

# MEMOIRE_PROJECT does NOT bypass the gate (same contract as capture).
if MEMOIRE_PROJECT=forced "$SCRIPT" --inject-content-enabled "$PULLREPO" >/dev/null; then
  fail "MEMOIRE_PROJECT must not bypass inject_content: false"
fi
echo "ok: MEMOIRE_PROJECT does not bypass inject-content-off"

# Sourceable gate.
if ! bash -c 'source "$1"; memoire_inject_content_enabled "$2"' _ "$SCRIPT" "$DEV/personal/cctime"; then
  fail "sourceable memoire_inject_content_enabled must return 0 when enabled"
fi
echo "ok: sourceable inject-content gate"

# Restore the fixture for downstream sections.
printf '  project:   spaced-key  \n' > "$PULLREPO/.memoire"

## ---- D164 worktree canonicalization (operator ruling (a), 2026-07-22) ----
# A session in a linked git worktree attributes as if it ran in the main
# checkout: key AND root fold into the parent project, so the server's
# (project, root) conflict detection never fires for worktrees.

if command -v git >/dev/null 2>&1; then
  WT="$TMP/wt"
  mkdir -p "$WT"
  gq() { git -C "$1" -c user.email=t@t -c user.name=t "${@:2}" >/dev/null 2>&1; }

  # Repo with a committed .memoire; worktree checkout carries it too.
  MAIN="$WT/mainproj"
  mkdir -p "$MAIN"
  gq "$MAIN" init
  printf 'project: wt-main-key\n' > "$MAIN/.memoire"
  gq "$MAIN" add .memoire
  gq "$MAIN" commit -m fixture
  gq "$MAIN" worktree add "$WT/mainproj-topic" -b topic
  mkdir -p "$WT/mainproj-topic/services/x"

  expect "worktree inherits .memoire key" wt-main-key "$WT/mainproj-topic"
  GOT="$("$SCRIPT" --verbose "$WT/mainproj-topic")"
  [[ "$GOT" == "$(printf 'wt-main-key\t%s\tmemoire-file' "$MAIN")" ]] \
    || fail "worktree root must be the MAIN checkout: got '$GOT'"
  echo "ok: worktree root canonicalizes to main -> $GOT"
  expect "worktree subdir inherits key" wt-main-key "$WT/mainproj-topic/services/x"
  expect "worktree as session-home canonicalizes" wt-main-key "$TMP" "$WT/mainproj-topic"

  # Plain repo (no .memoire, no registry entry): key = MAIN basename, not
  # the worktree dir name.
  PLAIN="$WT/plainproj"
  mkdir -p "$PLAIN"
  gq "$PLAIN" init
  touch "$PLAIN/f"
  gq "$PLAIN" add f
  gq "$PLAIN" commit -m fixture
  gq "$PLAIN" worktree add "$WT/plainproj-drift" -b drift
  expect "plain worktree -> main basename" plainproj "$WT/plainproj-drift"
  GOT="$("$SCRIPT" --verbose "$WT/plainproj-drift")"
  [[ "$GOT" == "$(printf 'plainproj\t%s\tbasename' "$PLAIN")" ]] \
    || fail "plain worktree verbose: got '$GOT'"
  echo "ok: plain worktree verbose -> $GOT"

  # Round-1 MED-1: the caller's spelling must not defeat canonicalization —
  # git reports canonical absolute paths; relative, symlinked, and
  # dot-component spellings of the same worktree dir must still fold, with
  # the ROOT pinned to main (the server conflicts key on root).
  GOT="$(cd "$WT" && "$SCRIPT" --verbose ./mainproj-topic)"
  [[ "$GOT" == "$(printf 'wt-main-key\t%s\tmemoire-file' "$MAIN")" ]] \
    || fail "relative worktree path must canonicalize: got '$GOT'"
  echo "ok: relative worktree path -> $GOT"
  ln -s "$WT/mainproj-topic" "$WT/topic-alias"
  GOT="$("$SCRIPT" --verbose "$WT/topic-alias")"
  [[ "$GOT" == "$(printf 'wt-main-key\t%s\tmemoire-file' "$MAIN")" ]] \
    || fail "symlinked worktree path must canonicalize: got '$GOT'"
  echo "ok: symlinked worktree path -> $GOT"
  GOT="$("$SCRIPT" --verbose "$WT/mainproj-topic/services/../services/x")"
  [[ "$GOT" == "$(printf 'wt-main-key\t%s\tmemoire-file' "$MAIN")" ]] \
    || fail "dot-component worktree path must canonicalize: got '$GOT'"
  echo "ok: dot-component worktree path -> $GOT"

  # Main checkout itself is untouched by canonicalization.
  expect "main checkout unchanged" wt-main-key "$MAIN"

  # Env override still beats worktree inheritance (the (a) escape hatch).
  GOT="$(MEMOIRE_PROJECT=forced "$SCRIPT" "$WT/mainproj-topic")"
  [[ "$GOT" == "forced" ]] || fail "env beats worktree inheritance: got '$GOT'"
  echo "ok: env override from worktree -> $GOT"

  # capture: false committed in main gates worktree sessions too.
  CAPOFF="$WT/capoff"
  mkdir -p "$CAPOFF"
  gq "$CAPOFF" init
  printf 'project: capoff\ncapture: false\n' > "$CAPOFF/.memoire"
  gq "$CAPOFF" add .memoire
  gq "$CAPOFF" commit -m fixture
  gq "$CAPOFF" worktree add "$WT/capoff-topic" -b topic
  if "$SCRIPT" --capture-enabled "$WT/capoff-topic" >/dev/null; then
    fail "capture: false must gate worktree sessions"
  fi
  echo "ok: capture:false gates worktree -> off"
else
  echo "skip: git not available, worktree canonicalization cases skipped"
fi

echo "PASS: all resolve-project cases"
