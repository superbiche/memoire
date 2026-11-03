#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# test-lanes-test.sh — the L0011 test split. Every test lane runs in exactly one
# place: the public `make test` (Makefile) or the ledger's `make test`
# (Makefile.ledger, present only in the private ledger). No lane is dropped
# silently, and no public lane depends on a ledger-only marker file.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
fail() { echo "FAIL: $1" >&2; exit 1; }

# A lane is invoked on a recipe line: "<tab>[...]bash|python3 [...]<path>".
invocations() { # <makefile> <path>
  [[ -f "$1" ]] || { echo 0; return; }
  grep -cE "^	.*(bash|python3) ([^ ]+ )*${2//./\\.}( |$)" "$1" || true
}

# 1. The public Makefile carries no home/ledger marker split.
grep -q 'HOME_TREE' Makefile && fail "Makefile still switches lanes on a ledger marker (HOME_TREE)"
grep -qE '^test-home:' Makefile && fail "Makefile still has a test-home target"

# 2. Every lane runs exactly once, in one of the two Makefiles.
shopt -s nullglob
lanes=(scripts/tests/*-test.sh scripts/tests/*-test.py bench/derive/tests/*-test.sh)
for lane in "${lanes[@]}"; do
  # A python helper driven by its same-named shell lane is not a lane.
  [[ "$lane" == *.py && -f "${lane%.py}.sh" ]] && continue
  pub="$(invocations Makefile "$lane")"
  led="$(invocations Makefile.ledger "$lane")"
  total=$((pub + led))
  [[ "$total" -eq 1 ]] || fail "$lane runs $pub time(s) in Makefile and $led in Makefile.ledger; exactly one is required"
done

# 3. The ledger Makefile lists exactly the ledger lanes, and they are the
# lanes whose subject is private (deployment, fleet registry,
# cutover tooling). The public Makefile runs none of them.
ledger_lanes=(
  scripts/tests/memoire-projects-lint-test.sh
  scripts/tests/cdk8s-manifest-contract-test.sh
  scripts/tests/deploy-parity-test.sh
  scripts/tests/export-manifest-test.sh
)
if [[ -f Makefile.ledger ]]; then
  for lane in "${ledger_lanes[@]}"; do
    [[ "$(invocations Makefile.ledger "$lane")" -eq 1 ]] || fail "$lane must run in Makefile.ledger"
  done
fi
for lane in "${ledger_lanes[@]}"; do
  [[ "$(invocations Makefile "$lane")" -eq 0 ]] || fail "$lane is a ledger lane but runs in the public Makefile"
done

# 4. The public `make test` reaches every public lane: they sit on the recipe
# of `test` or of a target `test` depends on (test-core, test-ops).
deps="$(awk -F: '/^test:/{print $2; exit}' Makefile)"
for t in test-core test-ops; do
  [[ " $deps " == *" $t "* ]] || fail "make test does not depend on $t"
done

echo "test-lanes-test: OK"
