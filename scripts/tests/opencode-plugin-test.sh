#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# opencode-plugin-test.sh — wrapper for the node-based plugin hook test.
# The plugin runs under Bun inside opencode; the test exercises its hook
# logic (dedupe, deadline race, exit-code gating, part shape) under plain
# node with a fake `$`, so it needs no opencode, Bun, or live state.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

command -v node >/dev/null || { echo "SKIP: node not available"; exit 0; }
node -e 'if (!globalThis.crypto?.randomUUID) process.exit(1)' \
  || { echo "SKIP: node lacks global crypto.randomUUID"; exit 0; }

exec node "$ROOT/scripts/tests/opencode-plugin-test.mjs"
