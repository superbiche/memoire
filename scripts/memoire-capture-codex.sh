#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

export MEMOIRE_SOURCE_ADAPTER="${MEMOIRE_SOURCE_ADAPTER:-codex-cli}"
export MEMOIRE_EVENT_TYPE="${MEMOIRE_EVENT_TYPE:-note}"

# Project attribution is delegated to memoire-capture-text.sh, which runs the
# shared resolver over the launch cwd (session home). Do NOT pre-set
# MEMOIRE_PROJECT to basename here: that env value is a hard override and would
# defeat the registry disambiguation. MEMOIRE_PROJECT set by the caller still
# wins, as before.
exec "$ROOT/scripts/memoire-capture-text.sh" "$@"
