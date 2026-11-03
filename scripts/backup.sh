#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

DB="${MEMOIRE_DB:-${MEMOIRE_HOME:-$HOME/.memoire}/memoire.db}"
DEST_DIR="${MEMOIRE_BACKUP_DIR:-${MEMOIRE_HOME:-$HOME/.memoire}/backups}"
mkdir -p "$DEST_DIR"

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
sqlite3 "$DB" ".backup '$DEST_DIR/memoire-$stamp.db'"
echo "$DEST_DIR/memoire-$stamp.db"

