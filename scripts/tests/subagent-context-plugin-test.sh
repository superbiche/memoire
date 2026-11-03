#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# subagent-context-plugin-test.sh — wrapper for the OpenCode child-session leg
# of the sub-agent parent-context feature (subagent-context-plugin-test.mjs,
# `run` mode): switch parsing, provider boundary, parent fetch limit, opening
# prompt beyond the page, gates, exclusions, deadlines. Core lane (`make
# test-core`, public): the plugin is public, so its test is. Plain node with a
# fake Bun shell and a fake client: no opencode, no Bun, no network.
#
# A skip is never silent: it prints "SKIP: <reason>" and scripts/tests/
# require-ran.sh (which the Makefile wraps this in) fails the target.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

command -v node >/dev/null 2>&1 || { echo "SKIP: node not found on PATH (required by the plugin suite)"; exit 0; }
node -e 'process.exit(globalThis.crypto?.randomUUID ? 0 : 1)' 2>/dev/null \
  || { echo "SKIP: node $(node --version 2>&1) does not run here or has no global crypto.randomUUID"; exit 0; }

exec node "$ROOT/scripts/tests/subagent-context-plugin-test.mjs" run
