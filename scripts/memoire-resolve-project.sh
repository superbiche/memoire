#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-resolve-project.sh — resolve a Memoire project id from a directory,
# via the paths->id registry ruled in liaison:D238 (ruling 4). Basename stays the
# display name and the fail-open fallback; the registry only DISAMBIGUATES
# same-basename collisions (apps, infra, vacation, docs, ...) and folds
# old-home paths into their current project.
#
# Sourceable AND executable:
#   - executed:  memoire-resolve-project.sh <dir> [session-home-dir]  -> prints id
#   - executed:  memoire-resolve-project.sh --capture-enabled <dir> [session-home-dir]
#                -> prints on|off, exit 0 when capture is enabled, 1 when disabled
#   - executed:  memoire-resolve-project.sh --inject-content-enabled <dir> [session-home-dir]
#                -> prints on|off, exit 0 when content injection is enabled
#   - sourced:   memoire_resolve_project <dir> [session-home-dir]     -> echoes id
#   - sourced:   memoire_capture_enabled <dir> [session-home-dir]     -> 0/1
#   - sourced:   memoire_inject_content_enabled <dir> [session-home-dir] -> 0/1
#
# Resolution precedence (liaison:D239, refining liaison:D238):
#   1. MEMOIRE_PROJECT env  (hard override, unchanged from the adapters)
#   2. nearest `.memoire` FILE walking UP from the attribution dir — its
#      `project:` value wins. Presence is what counts at resolve time; being
#      committed to the repo is the usage convention (worktrees inherit it),
#      not a runtime git check — a freshly created, not-yet-committed
#      `.memoire` must take effect immediately (the ask-flow depends on it).
#   3. longest real path-prefix match over every entry's `paths` (registry,
#      demoted by liaison:D239 to history + scratch patterns)
#   4. basename fallback  (house convention: capture must NEVER break)
#
# `.memoire` file format (small, hand-editable, YAML-shaped lines):
#   project: <key>
#   capture: false        # optional repo-level kill switch
#   inject_content: false # optional: SessionStart injects state headers only
#                         # (pull mode — the session searches on demand)
#
# Capture kill switches (liaison:D239 item 6) — memoire_capture_enabled:
#   - MEMOIRE_CAPTURE=off|0|false  (session-level, e.g. bench runs)
#   - `capture: false` in the nearest `.memoire`  (repo-level)
#   MEMOIRE_PROJECT does NOT bypass capture-off: project override and capture
#   toggle are orthogonal. Fail-open: an unreadable/garbled `.memoire` never
#   disables capture and never breaks resolution.
#
# Session-home attribution (liaison:D238): the caller may pass the session LAUNCH dir
# as arg 2; when present it is PREFERRED over the per-event cwd (arg 1) so a
# session that roams across repos still attributes to its home project. The
# per-event cwd stays stored on the event; secondary attribution is derivable
# at read time — no schema change here.
#
# Registry: $MEMOIRE_PROJECTS_REGISTRY (default ~/.agents/memoire-projects.json).
# Fail-open: a missing/invalid registry, or a missing jq, degrades to basename —
# resolution never errors out and never blocks a capture. jq is the house JSON
# dependency (same as the capture/inject adapters); there is no pure-bash JSON
# parser here by design — absent jq we simply fall through to basename.

MEMOIRE_PROJECTS_REGISTRY="${MEMOIRE_PROJECTS_REGISTRY:-$HOME/.agents/memoire-projects.json}"

# Strip trailing slashes from a path (keep a lone "/").
_mrp_trim_slashes() {
  local p="$1"
  while [[ "$p" == */ && "$p" != "/" ]]; do p="${p%/}"; done
  printf '%s' "$p"
}

# Linked-worktree canonicalization (memoire:D164, operator ruling (a),
# 2026-07-22): a session in a git worktree attributes AS IF it ran in the
# main checkout — the worktree-toplevel prefix is rewritten to the main
# toplevel before resolution, so key and root fold into the parent project
# and no identity conflict is raised. MEMOIRE_PROJECT stays the escape
# hatch for a worktree that wants its own identity (step 1 precedes any
# dir logic). Fail-open: missing git, non-repo dirs, bare/odd layouts, or
# a dir outside the reported toplevel all keep the dir unchanged.
_mrp_canonical_dir() {
  local dir="$1"
  command -v git >/dev/null 2>&1 || { printf '%s' "$dir"; return 0; }
  local lines gitdir common wt_top
  lines="$(env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR \
    git -C "$dir" rev-parse --path-format=absolute \
      --git-dir --git-common-dir --show-toplevel 2>/dev/null)" \
    || { printf '%s' "$dir"; return 0; }
  { read -r gitdir; read -r common; read -r wt_top; } <<<"$lines"
  # A linked worktree is the ONLY shape rewritten: its --git-dir differs
  # from --git-common-dir and the common dir is a main checkout's .git.
  # Main checkouts (gitdir == common) and submodules (common == gitdir,
  # under .git/modules/) fall through untouched.
  if [[ -z "$gitdir" || -z "$common" || -z "$wt_top" \
        || "$gitdir" == "$common" || "$common" != */.git ]]; then
    printf '%s' "$dir"
    return 0
  fi
  # Compare the caller's dir PHYSICALLY: git reports canonical absolute
  # paths, while the caller may spell the same dir relative, through a
  # symlink, or with ./.. components (round-1 MED-1 — a string-prefix
  # miss would silently keep the worktree identity the policy folds).
  local phys
  phys="$(unset CDPATH; cd -- "$dir" 2>/dev/null && pwd -P)" || phys=""
  if [[ -z "$phys" ]]; then
    printf '%s' "$dir"
    return 0
  fi
  local main="${common%/.git}"
  if [[ "$phys" == "$wt_top" ]]; then
    printf '%s' "$main"
  elif [[ "$phys" == "$wt_top"/* ]]; then
    printf '%s' "$main${phys#"$wt_top"}"
  else
    printf '%s' "$dir"
  fi
}

# Attribution dir shared by resolution and the capture gate: session launch
# dir when provided, else the per-event cwd, else $PWD (liaison:D238 session-home
# attribution — a session that roams across repos keeps its home project).
# Worktree dirs canonicalize to their main checkout (policy (a) above).
_mrp_attribution_dir() {
  local dir="${2:-}"
  [[ -n "$dir" ]] || dir="${1:-}"
  [[ -n "$dir" ]] || dir="$PWD"
  _mrp_canonical_dir "$(_mrp_trim_slashes "$dir")"
}

# Nearest `.memoire` FILE walking up from $1 to /. Prints its path, or
# nothing. -f guards against directories named .memoire (notably the
# service home ~/.memoire, which is a directory and must never match).
_mrp_find_memoire_file() {
  local d="$1"
  [[ "$d" == /* ]] || return 0
  while :; do
    if [[ -f "$d/.memoire" ]]; then
      printf '%s' "$d/.memoire"
      return 0
    fi
    [[ "$d" == "/" ]] && return 0
    local parent="${d%/*}"
    [[ -n "$parent" ]] || parent="/"
    d="$parent"
  done
}

# First `<key>:` line's trimmed value from a .memoire file; empty on any
# problem (fail-open).
_mrp_memoire_get() {
  local file="$1" key="$2" line
  [[ -r "$file" ]] || return 0
  line="$(grep -m1 -E "^[[:space:]]*${key}[[:space:]]*:" "$file" 2>/dev/null)" || return 0
  line="${line#*:}"
  line="${line#"${line%%[![:space:]]*}"}"
  line="${line%"${line##*[![:space:]]}"}"
  printf '%s' "$line"
}

# Capture gate (liaison:D239 kill switches). 0 = capture enabled, 1 = disabled.
memoire_capture_enabled() {
  case "${MEMOIRE_CAPTURE:-}" in
    off|0|false) return 1 ;;
  esac
  local dir memoire_file
  dir="$(_mrp_attribution_dir "${1:-}" "${2:-}")"
  memoire_file="$(_mrp_find_memoire_file "$dir")"
  if [[ -n "$memoire_file" ]]; then
    [[ "$(_mrp_memoire_get "$memoire_file" capture)" == "false" ]] && return 1
  fi
  return 0
}

# Content-injection gate (pull-based retrieval v0). 0 = content injection
# enabled (default), 1 = disabled: the SessionStart inject emits state
# headers only and the session searches once the work subject is clear.
# Switches mirror the capture gate:
#   - MEMOIRE_INJECT_CONTENT=off|0|false  (session-level)
#   - `inject_content: false` in the nearest `.memoire`  (repo-level)
# Fail-open toward current behavior: an unreadable/garbled `.memoire`
# never disables injection.
memoire_inject_content_enabled() {
  case "${MEMOIRE_INJECT_CONTENT:-}" in
    off|0|false) return 1 ;;
  esac
  local dir memoire_file
  dir="$(_mrp_attribution_dir "${1:-}" "${2:-}")"
  memoire_file="$(_mrp_find_memoire_file "$dir")"
  if [[ -n "$memoire_file" ]]; then
    [[ "$(_mrp_memoire_get "$memoire_file" inject_content)" == "false" ]] && return 1
  fi
  return 0
}

# Core resolution: sets MRP_KEY, MRP_ROOT, MRP_SOURCE.
#   MRP_SOURCE: env | memoire-file | registry | basename
#   MRP_ROOT:   the dir the key is anchored at — the .memoire file's dir, the
#               matched registry prefix, the attribution dir for basename, or
#               "" for env (an override has no root). Conflict detection
#               (liaison:D239 item 4, mechanic ratified 2026-07-18: adapters pass the
#               resolved root) keys on it server-side.
_mrp_resolve_core() {
  local event_dir="${1:-}"
  local home_dir="${2:-}"
  MRP_KEY="" MRP_ROOT="" MRP_SOURCE=""

  # 1. Hard env override — matches every capture/inject adapter.
  if [[ -n "${MEMOIRE_PROJECT:-}" ]]; then
    MRP_KEY="$MEMOIRE_PROJECT" MRP_SOURCE="env"
    return 0
  fi

  local dir
  dir="$(_mrp_attribution_dir "$event_dir" "$home_dir")"

  # 2. Nearest `.memoire` file wins (liaison:D239). Empty/absent project: line falls
  # through — fail-open, never an error.
  local memoire_file mproj
  memoire_file="$(_mrp_find_memoire_file "$dir")"
  if [[ -n "$memoire_file" ]]; then
    mproj="$(_mrp_memoire_get "$memoire_file" project)"
    if [[ -n "$mproj" ]]; then
      MRP_KEY="$mproj" MRP_ROOT="${memoire_file%/.memoire}" MRP_SOURCE="memoire-file"
      return 0
    fi
  fi

  local fallback
  fallback="$(basename "$dir" 2>/dev/null)" || fallback=""
  [[ -n "$fallback" ]] || fallback="$dir"

  # 3. Registry longest-prefix match (fail-open to basename on any problem).
  if command -v jq >/dev/null 2>&1 \
     && [[ -f "$MEMOIRE_PROJECTS_REGISTRY" ]] \
     && jq -e '.projects | type == "object"' "$MEMOIRE_PROJECTS_REGISTRY" >/dev/null 2>&1
  then
    local hit
    hit="$(jq -r --arg d "$dir" '
      [ .projects | to_entries[] as $e
        | ($e.value.paths // [])[] as $p
        # real path-prefix: exact dir, or a component-boundary descendant.
        # "/home/a/b" matches "/home/a/b" and "/home/a/b/c", never "/home/a/bc".
        | select($d == $p or ($d | startswith($p + "/")))
        | {id: $e.key, p: $p, plen: ($p | length)} ]
      | if length > 0 then (max_by(.plen) | "\(.id)\t\(.p)") else empty end
    ' "$MEMOIRE_PROJECTS_REGISTRY" 2>/dev/null)" || hit=""
    if [[ -n "$hit" ]]; then
      MRP_KEY="${hit%%$'\t'*}" MRP_ROOT="${hit#*$'\t'}" MRP_SOURCE="registry"
      return 0
    fi
  fi

  # 4. Basename fallback.
  MRP_KEY="$fallback" MRP_ROOT="$dir" MRP_SOURCE="basename"
  return 0
}

memoire_resolve_project() {
  _mrp_resolve_core "$@"
  printf '%s\n' "$MRP_KEY"
  return 0
}

# Verbose form for capture adapters: `key<TAB>root<TAB>source` on one line.
# The adapters forward root+source in the event payload; the server's ingest
# chokepoint uses them for identity-conflict detection (liaison:D239 item 4).
memoire_resolve_project_verbose() {
  _mrp_resolve_core "$@"
  printf '%s\t%s\t%s\n' "$MRP_KEY" "$MRP_ROOT" "$MRP_SOURCE"
  return 0
}

# Executed (not sourced): resolve from argv and print, or gate capture with
# --capture-enabled. Guard is set -u safe and only fires under bash-run
# execution; when sourced (BASH_SOURCE != $0, or unset under a non-bash
# shell) it stays quiet and just exports the functions.
if [[ "${BASH_SOURCE[0]:-}" == "${0}" ]]; then
  if [[ "${1:-}" == "--capture-enabled" ]]; then
    if memoire_capture_enabled "${2:-}" "${3:-}"; then
      echo on
      exit 0
    fi
    echo off
    exit 1
  fi
  if [[ "${1:-}" == "--inject-content-enabled" ]]; then
    if memoire_inject_content_enabled "${2:-}" "${3:-}"; then
      echo on
      exit 0
    fi
    echo off
    exit 1
  fi
  if [[ "${1:-}" == "--verbose" ]]; then
    memoire_resolve_project_verbose "${2:-}" "${3:-}"
    exit 0
  fi
  memoire_resolve_project "${1:-}" "${2:-}"
fi
