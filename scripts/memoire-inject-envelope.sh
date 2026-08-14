#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-inject-envelope.sh — SessionStart hook wrapper for harnesses that
# require the Claude-compatible JSON envelope on hook stdout (qwen-code:
# plain stdout is NOT injected into model context; the envelope is —
# measured live 2026-07-12, qwen-code 0.19.9; see
# inbox/2026-07-12-liaison-qwen-inject-needs-envelope.md).
#
# No logic fork: runs the real inject script (stdin passed through so it can
# read the hook JSON) and wraps its stdout as
#   {"hookSpecificOutput": {"hookEventName": "SessionStart", "additionalContext": "..."}}
# Empty inject output emits nothing. Fail-open: any error exits 0 so session
# start is never blocked.
#
# The payload travels to jq via the printf builtin on stdin, never as exec
# argv — a single argv entry caps at ~128 KiB on Linux (MAX_ARG_STRLEN) and
# would silently drop large valid contexts, besides exposing the context in
# /proc/*/cmdline. printf '%s\n' restores the one trailing newline that
# command substitution strips, keeping the wrapped payload byte-identical to
# the inner script's plain stdout.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd)" || exit 0
INJECT="$SCRIPT_DIR/memoire-inject-claude.sh"

command -v jq >/dev/null 2>&1 || exit 0
[[ -x "$INJECT" ]] || exit 0

# Bound the inner script so a hang (blocked stdin/token read, stuck curl)
# cannot stall session start; the inner curl timeouts are 2s each, so 10s is
# generous. TERM then KILL (-k): a TERM-ignoring inner process must still
# die. The deadline must be a POSITIVE integer — GNU timeout treats 0 as "no
# timeout" — so junk or 0 falls back to the default (same discipline as the
# sweep's CAPTURE_TIMEOUT). Without coreutils timeout the wrapper suppresses
# injection entirely (fail-open) rather than run unbounded.
DEADLINE_S="${MEMOIRE_INJECT_WRAPPER_TIMEOUT:-10}"
[[ "$DEADLINE_S" =~ ^[1-9][0-9]*$ ]] || DEADLINE_S=10
command -v timeout >/dev/null 2>&1 || exit 0

CONTEXT="$(timeout -k 2 "$DEADLINE_S" "$INJECT" 2>/dev/null)" || CONTEXT=""
[[ -n "$CONTEXT" ]] || exit 0

printf '%s\n' "$CONTEXT" | jq -Rsc \
  '{hookSpecificOutput: {hookEventName: "SessionStart", additionalContext: .}}' 2>/dev/null
exit 0
