#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

HOME_DIR="${MEMOIRE_HOME:-$HOME/.memoire}"
TOKEN_FILE="${MEMOIRE_TOKEN_FILE:-$HOME_DIR/token}"
mkdir -p "$HOME_DIR"

if [[ -e "$TOKEN_FILE" ]]; then
  mv "$TOKEN_FILE" "$TOKEN_FILE.$(date -u +%Y%m%dT%H%M%SZ).bak"
fi

python3 - <<'PY' > "$TOKEN_FILE"
import secrets
print(secrets.token_urlsafe(32))
PY
chmod 600 "$TOKEN_FILE"
echo "rotated $TOKEN_FILE"
echo "restart Memoire to bootstrap the new token hash into credentials"
