#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# leak-scan.sh — the curated-export leak gate (memoire:D226/D228). Scans a
# candidate tree's file CONTENT and file NAMES against the merged pattern
# set: the generic shapes beside this script plus the sensitive set at
# ~/.memoire/export/leak-patterns.txt (outside every git repo, D228
# ruling 2). Grep-status honesty and the exact-line exemption discipline
# are inherited from the D188 dist scan (rounds 3-5 of that chain).
#
# Public inputs (L0011 P3): the generic pattern file and the exemptions
# file are public and sit in the tree at scripts/leak/; the scanner skips
# the CONTENTS of exactly those two paths, and only while byte-identical to
# the loaded files. A copy anywhere else is judged, and every file name is
# scanned. The sensitive set's file must NEVER
# be in the tree: its presence is a refusal (it lives outside every git
# repo, D228 ruling 2).
#
# Exit: 0 clean, 1 hits (report written), 2 operational error.
# With --require-sensitive (the export pipeline's mode) an absent or empty
# sensitive set is a REFUSAL, not a degrade: a half-armed scan reporting
# clean is the green-but-stale failure class (memoire:D166).

usage() {
  echo "usage: leak-scan.sh --tree <dir> --report <file> [--require-sensitive]" >&2
  echo "         [--exemptions <file>] [--generic <file>] [--sensitive <file>]" >&2
  exit 2
}

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TREE="" REPORT="" REQUIRE_SENSITIVE=0
GENERIC="$HERE/leak-patterns-generic.txt"
# The sensitive set is the CANONICAL ruled path (D228 ruling 2). No
# environment override exists (review r1 F3: an inherited variable could
# swap in any nonempty file and the gate would report clean without ever
# loading a client name). Tests use the explicit --sensitive flag, which
# is visible in the reviewed caller's argv, never ambient.
SENSITIVE="$HOME/.memoire/export/leak-patterns.txt"
EXEMPTIONS=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --tree) TREE="$2"; shift 2 ;;
    --report) REPORT="$2"; shift 2 ;;
    --require-sensitive) REQUIRE_SENSITIVE=1; shift ;;
    --exemptions) EXEMPTIONS="$2"; shift 2 ;;
    --generic) GENERIC="$2"; shift 2 ;;
    --sensitive) SENSITIVE="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[[ -n "$TREE" && -n "$REPORT" ]] || usage
[[ -d "$TREE" ]] || { echo "FAIL: tree not found: $TREE" >&2; exit 2; }

fail() { echo "FAIL: $1" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Merge pattern sources, stripping comments and blanks. Zero patterns is
# an error, never a clean verdict.
PATTERNS="$WORK/patterns.txt"
strip() { grep -vE '^[[:space:]]*(#|$)' "$1" || true; }
[[ -f "$GENERIC" ]] || fail "generic pattern file missing: $GENERIC"
strip "$GENERIC" > "$PATTERNS"
if [[ -f "$SENSITIVE" ]] && [[ -n "$(strip "$SENSITIVE")" ]]; then
  strip "$SENSITIVE" >> "$PATTERNS"
elif [[ "$REQUIRE_SENSITIVE" -eq 1 ]]; then
  fail "sensitive pattern set absent or empty: $SENSITIVE — refusing a half-armed scan (D166; D228 ruling 2)"
else
  echo "WARN: sensitive pattern set absent or empty ($SENSITIVE); scanning with generic shapes only" >&2
fi
[[ -s "$PATTERNS" ]] || fail "merged pattern set is empty"

# The sensitive set's file must never be in the candidate tree.
if [[ -n "$SENSITIVE" && -f "$SENSITIVE" ]]; then
  found="$(find "$TREE" -name "$(basename "$SENSITIVE")" -print -quit)"
  [[ -z "$found" ]] || fail "sensitive pattern file present in candidate tree: $found"
fi

# Public inputs in the tree (L0011): the generic pattern file and the
# exemptions file sit in the public tree at exactly two canonical paths. Only
# those two paths' CONTENTS are skipped, and only while byte-identical to the
# loaded files; a copy anywhere else is scanned, and every file NAME is
# scanned (review r1 F3).
SKIPABS="$WORK/skip-abs.txt"; : > "$SKIPABS"
for pair in "scripts/leak/leak-patterns-generic.txt:$GENERIC" "scripts/leak/leak-exemptions.txt:$EXEMPTIONS"; do
  rel="${pair%%:*}"; pf="${pair#*:}"
  [[ -n "$pf" && -f "$pf" && -f "$TREE/$rel" ]] || continue
  cmp -s -- "$TREE/$rel" "$pf" && printf '%s\n' "$TREE/$rel" >> "$SKIPABS"
done

# Symlinks are forbidden in the candidate tree (targets can point home).
SYMLINKS="$(find "$TREE" -type l)"
[[ -z "$SYMLINKS" ]] || { printf '%s\n' "$SYMLINKS" >&2; fail "symlink(s) in candidate tree (above)"; }

# Load exemptions — typed entries "line\t<path>\t<exact line>" and
# "shapes\t<path>\t<ERE>" (grammar documented in leak-exemptions.txt).
# Exact lines must occur exactly once in their file, else the exemption is
# stale and the run fails. A "line"/"shapes" entry naming a file absent
# from the tree fails the same way — dead exemptions never accumulate.
declare -a EX_PATHS=() EX_LINES=() EXS_PATHS=() EXS_RES=() EXS_USED=()
if [[ -n "$EXEMPTIONS" ]]; then
  [[ -f "$EXEMPTIONS" ]] || fail "exemptions file missing: $EXEMPTIONS"
  while IFS=$'\t' read -r kind p v; do
    [[ -z "$kind" || "$kind" == \#* ]] && continue
    [[ -n "$p" && -n "$v" ]] || fail "malformed exemption entry: $kind"
    [[ -f "$TREE/$p" ]] || fail "exempted file not in tree: $p"
    case "$kind" in
      line)
        n="$(grep -cFx -- "$v" "$TREE/$p")" || true
        [[ "$n" -eq 1 ]] || fail "exempt line occurs $n times (want exactly 1) in $p — update the exemption deliberately"
        EX_PATHS+=("$p"); EX_LINES+=("$v")
        ;;
      shapes)
        EXS_PATHS+=("$p"); EXS_RES+=("$v")
        ;;
      *) fail "unknown exemption type: $kind" ;;
    esac
  done < "$EXEMPTIONS"
fi

# Content scan over every text file (-I skips binaries; no extension
# filter). grep status 1 is the wanted no-match; >1 is an I/O or pattern
# error and must FAIL, not read as "no leak" (D188 round-4 rule).
RAW="$WORK/raw-hits.txt"
mapfile -d '' CONTENT_FILES < <(find "$TREE" -type f -print0 | { if [[ -s "$SKIPABS" ]]; then grep -zvxF -f "$SKIPABS" || true; else cat; fi; })
set +e
if [[ "${#CONTENT_FILES[@]}" -gt 0 ]]; then
  grep -HInE -f "$PATTERNS" -I -- "${CONTENT_FILES[@]}" > "$RAW"
  rc_tree=$?
else
  : > "$RAW"; rc_tree=1
fi
set -e
[[ "$rc_tree" -le 1 ]] || fail "content scan errored (grep status $rc_tree)"

# Name scan: paths themselves can leak; every name is scanned.
NAMES="$WORK/names.txt"
(cd "$TREE" && find . -mindepth 1 | sed 's|^\./||') > "$NAMES"
set +e
grep -nE -f "$PATTERNS" "$NAMES" | sed 's/^/NAME:/' >> "$RAW"
rc_names=$?
set -e
[[ "$rc_names" -le 1 ]] || fail "name scan errored (grep status $rc_names)"

# Drop hits matching an exemption: "line" needs same file + full content
# equality; "shapes" needs same file + content matching the declared ERE.
# Everything else survives.
: >| "$REPORT"
while IFS= read -r hit; do
  keep=1
  for i in "${!EX_PATHS[@]}"; do
    prefix="$TREE/${EX_PATHS[$i]}:"
    if [[ "$hit" == "$prefix"* ]]; then
      content="${hit#"$prefix"}"
      content="${content#*:}"   # strip line number
      if [[ "$content" == "${EX_LINES[$i]}" ]]; then keep=0; break; fi
    fi
  done
  if [[ "$keep" -eq 1 ]]; then
    for i in "${!EXS_PATHS[@]}"; do
      prefix="$TREE/${EXS_PATHS[$i]}:"
      if [[ "$hit" == "$prefix"* ]]; then
        content="${hit#"$prefix"}"
        content="${content#*:}"
        if grep -qE -- "${EXS_RES[$i]}" <<< "$content"; then
          # A shape match never absolves the whole line (review r1 F4: a
          # topology/client hit sharing the line with a synthetic fixture
          # was silently masked). Strip the exempted shape substrings and
          # re-judge the remainder against the FULL pattern set — only a
          # line that is clean once the declared shape is removed drops.
          stripped="$(sed -E "s/${EXS_RES[$i]//\//\\/}//g" <<< "$content")"
          if ! grep -qE -f "$PATTERNS" <<< "$stripped"; then keep=0; fi
          EXS_USED[$i]=1
          break
        fi
      fi
    done
  fi
  [[ "$keep" -eq 1 ]] && printf '%s\n' "$hit" >> "$REPORT"
done < "$RAW"

# Stale-exemption guard (review r1 F4): a shapes entry whose ERE matched
# nothing in its file is dead configuration and fails loudly, exactly like
# a stale exact-line exemption.
for i in "${!EXS_PATHS[@]}"; do
  [[ "${EXS_USED[$i]:-0}" -eq 1 ]] \
    || fail "stale shapes exemption: no hit in ${EXS_PATHS[$i]} matched its ERE — remove or update it deliberately"
done

if [[ -s "$REPORT" ]]; then
  echo "leak-scan: HITS ($(wc -l < "$REPORT") lines) — report: $REPORT" >&2
  exit 1
fi
echo "leak-scan: clean ($(wc -l < "$PATTERNS") patterns, tree $TREE)"
