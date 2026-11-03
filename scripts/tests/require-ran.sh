#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# require-ran.sh <command...> — run a REQUIRED test suite and fail the target
# when it did not run: a suite that prints a line starting with "SKIP" (missing
# tool, unsupported runtime) exits 0 by itself, which would make `make test`
# green without the suite having run. A suite that exits 0 must also print its
# own "<name>: OK" line (flush left): a suite that exits 0 silently, or dies
# early with a success status, did not prove that it ran. The suite's own
# output passes through.

(( $# > 0 )) || { echo "usage: require-ran.sh <command...>" >&2; exit 2; }
out="$(mktemp "${TMPDIR:-/tmp}/require-ran.XXXXXX")" || exit 2
trap 'rm -f "$out"' EXIT
"$@" 2>&1 | tee "$out"
rc="${PIPESTATUS[0]}"
(( rc == 0 )) || exit "$rc"
if grep -q '^SKIP' "$out"; then
  echo "require-ran: REQUIRED SUITE DID NOT RUN ($*): the SKIP line(s) above say why" >&2
  exit 3
fi
if ! grep -Eq '^[A-Za-z0-9._-]+: OK$' "$out"; then
  echo "require-ran: SUITE EXITED 0 BUT printed no \": OK\" line ($*): it did not prove that it ran" >&2
  exit 4
fi
exit 0
