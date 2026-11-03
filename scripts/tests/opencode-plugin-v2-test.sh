#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# opencode-plugin-v2-test.sh — wrapper for the node-based test of the plugin's
# OpenCode v2 face (default export { id, setup(ctx) }). Plain node with a fake
# v2 host context: no opencode, no Bun, no live state.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

command -v node >/dev/null || { echo "SKIP: node not available"; exit 0; }
command -v timeout >/dev/null || { echo "SKIP: timeout (coreutils) not available"; exit 0; }

exec node "$ROOT/scripts/tests/opencode-plugin-v2-test.mjs"
