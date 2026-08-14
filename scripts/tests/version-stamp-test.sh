#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# version-stamp-test.sh — contract pins on the build-stamp path (MISSING
# #7): every shipping build of memoire-api must carry the ldflags -X stamp
# from the VERSION file, and no read surface may carry a version literal.
# The behavioral leg (stamped binary reports VERSION over HTTP) lives in
# tests/e2e/standalone-sqlite-smoke.sh; this test keeps the stamp from
# silently falling off a build entry point, which the e2e leg (its own
# go build line) cannot see.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fail() { echo "FAIL: $1" >&2; exit 1; }

STAMP='memoire/services/api/internal/buildinfo.Version'

# 1. The Makefile's memoire-api build line carries the stamp (via
#    GO_LDFLAGS) and GO_LDFLAGS reads the VERSION file.
grep -q "GO_LDFLAGS := -X $STAMP=\$(shell head -1 VERSION)" "$ROOT/Makefile" \
  || fail "Makefile GO_LDFLAGS must stamp buildinfo.Version from VERSION"
grep -Eq "go build .*-ldflags '\\\$\(GO_LDFLAGS\)' -o bin/memoire-api" "$ROOT/Makefile" \
  || fail "Makefile memoire-api build must pass -ldflags '\$(GO_LDFLAGS)'"

# 2. The container build (the hub's actual artifact) carries the stamp
#    from the same VERSION file.
CONTAINERFILE="$ROOT/services/api/Containerfile"
grep -q -- "-X $STAMP=\$(head -1 VERSION)" "$CONTAINERFILE" \
  || fail "Containerfile api build must stamp buildinfo.Version from VERSION"

# 3. The e2e smoke builds with the same stamp (its assertion is the
#    behavioral proof; an unstamped smoke build would assert against "dev").
grep -q -- "-X $STAMP=\$REPO_VERSION" "$ROOT/tests/e2e/standalone-sqlite-smoke.sh" \
  || fail "standalone smoke must build with the version stamp"

# 4. The MCP handshake version is stamped, not a const literal.
grep -q "go build -ldflags '-X main.serverVersion=\$(shell head -1 VERSION)' -o bin/memoire-mcp" "$ROOT/Makefile" \
  || fail "Makefile memoire-mcp build must stamp main.serverVersion"
grep -q 'var serverVersion = "dev"' "$ROOT/services/mcp/main.go" \
  || fail "mcp serverVersion must be the stampable var with the honest dev default"

# 5. The unstamped default is "dev" — honest, never a semver literal —
#    and the health handler reads buildinfo, not a string.
grep -q 'var Version = "dev"' "$ROOT/services/api/internal/buildinfo/buildinfo.go" \
  || fail "buildinfo default must be \"dev\""
grep -q '"version":.*buildinfo.Public()' "$ROOT/services/api/internal/handlers/handlers.go" \
  || fail "health must serve the bounded buildinfo.Public() token (D224; review F1)"
if grep -Eq '"version":\s*"[0-9]' "$ROOT/services/api/internal/handlers/handlers.go"; then
  fail "health carries a version literal again"
fi

echo "OK version-stamp-test"
