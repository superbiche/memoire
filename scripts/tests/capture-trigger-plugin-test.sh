#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# capture-trigger-plugin-test.sh — wrapper for capture-trigger-plugin-test.mjs,
# the OpenCode plugin's session.idle spawn leg. Core lane (`make test-core`).
# A skip is never silent: it prints "SKIP: <reason>" and scripts/tests/
# require-ran.sh (which the Makefile wraps this in) fails the target.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
command -v node >/dev/null 2>&1 || { echo "SKIP: node not found on PATH (required by the plugin suite)"; exit 0; }
exec node "$ROOT/scripts/tests/capture-trigger-plugin-test.mjs"
