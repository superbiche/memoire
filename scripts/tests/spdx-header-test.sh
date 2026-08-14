#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# spdx-header-test.sh — memoire:D180 per-file-header artifact (R-10.5,
# round-1 finding 13). Enumerates the governed source inventory BY
# EXTENSION and fails on any tracked file missing the SPDX identifier in
# its first three lines. Generated surfaces (cdk8s/dist, docs/state,
# docs/views) carry no headers and are not in these families.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

# Enumerate first and refuse an empty inventory: if git enumeration fails
# (e.g. run outside a git worktree) the loop below would pass VACUOUSLY —
# the green-but-unchecked class (memoire:D166).
FILES="$(git ls-files '*.go' '*.sh' '*.ts' '*.js' '*.vue' '*.mjs' '*.css' \
  '*.sql' '*.html' '*Containerfile*' 'Makefile' '*/Makefile' | grep -v '^cdk8s/dist/')"
[[ -n "$FILES" ]] || { echo "FAIL: governed-source enumeration came back empty" >&2; exit 1; }

fail=0
while IFS= read -r f; do
  if ! head -3 "$f" | grep -q "SPDX-License-Identifier: AGPL-3.0-only"; then
    echo "FAIL: missing SPDX header: $f" >&2
    fail=1
  fi
done <<< "$FILES"

[[ "$fail" -eq 0 ]] || exit 1
echo "spdx-header-test: OK"
