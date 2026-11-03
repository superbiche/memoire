#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${MEMOIRE_QUEUE_BIN:-$ROOT/bin/memoire-queue}"

if [[ ! -x "$BIN" ]]; then
  echo "queue writer not found: $BIN" >&2
  echo "Run: make build" >&2
  exit 1
fi

"$BIN" \
  -project memoire \
  -session-id "seed-$(date +%s)" \
  -source-adapter seed \
  -event-type note \
  -text "seeded through file queue"
