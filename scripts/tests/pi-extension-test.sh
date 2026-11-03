#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# pi-extension-test.sh — wrapper for the node-based test of the Pi extension
# (scripts/pi-extension/memoire.js). Plain node with a fake Pi API: no Pi,
# no live state.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

command -v node >/dev/null || { echo "SKIP: node not available"; exit 0; }
command -v timeout >/dev/null || { echo "SKIP: timeout (coreutils) not available"; exit 0; }

exec node "$ROOT/scripts/tests/pi-extension-test.mjs"
