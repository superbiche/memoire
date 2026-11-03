#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# pg-shadow.sh — local standing-shadow lifecycle (slice 5 groundwork,
# docs/SLICE5-GROUNDWORK-DESIGN.md). Runs the pgvector:pg16 shadow in
# podman and applies schema-pg/. The HL3 shadow will be CNPG; this is the
# development twin.
#
# Usage: pg-shadow.sh up | down | status | url

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NAME="${MEMOIRE_PG_SHADOW_NAME:-memoire-pg-shadow}"
PORT="${MEMOIRE_PG_SHADOW_PORT:-54329}"
PASS="${MEMOIRE_PG_SHADOW_PASSWORD:-memoire-shadow-local}"
# ParadeDB image since 2026-07-22 (option A, D160): the production HL3
# cluster runs it for pg_search BM25, so the local twin must too — the
# store's pg_search path is exercised by conformance only when the shadow
# carries the extension. Ships pgvector as well.
IMAGE="${MEMOIRE_PG_SHADOW_IMAGE:-docker.io/paradedb/paradedb:18-v0.24.3}"
URL="postgres://postgres:$PASS@127.0.0.1:$PORT/memoire"

case "${1:-}" in
  up)
    if ! podman container exists "$NAME" 2>/dev/null; then
      # -e VAR without a value reads it from the environment: the password
      # never appears in the podman argv (slice-5 round-3 F7 closure).
      # --locale=C: the shadow's memoire DB must pass the D0249 collation
      # preflight (C = bytewise = SQLite BINARY parity); UTF8 must be
      # explicit because --locale=C alone would pick SQL_ASCII. Applies on
      # container creation only — `down` removes the container, so a
      # recreate picks this up.
      POSTGRES_PASSWORD="$PASS" podman run -d --name "$NAME" \
        -e POSTGRES_PASSWORD -e POSTGRES_DB=memoire \
        -e POSTGRES_INITDB_ARGS="--locale=C --encoding=UTF8" \
        -p "127.0.0.1:$PORT:5432" "$IMAGE" >/dev/null
    else
      podman start "$NAME" >/dev/null
    fi
    # -h 127.0.0.1 is load-bearing: the image entrypoint boots a TEMPORARY
    # socket-only postgres for init scripts and restarts it — the plain
    # socket probe passes during that phase and callers then hit connection
    # resets on the mapped port. TCP readiness = the FINAL server.
    for _ in $(seq 1 60); do
      podman exec "$NAME" pg_isready -h 127.0.0.1 -U postgres -q 2>/dev/null && break
      sleep 1
    done
    podman exec "$NAME" pg_isready -h 127.0.0.1 -U postgres -q || { echo "pg-shadow: postgres never became ready" >&2; exit 1; }
    # pg_search must exist before Apply provisions the BM25 index (on the
    # real cluster the operator's Database resource owns extension
    # creation; locally the container superuser does). LOUD on the default
    # ParadeDB image — a swallowed failure here silently downgrades every
    # local run to the tsvector fallback while claiming pg_search coverage
    # (D160 round-1 finding 2). A deliberate non-paradedb override image is
    # the only quiet skip.
    case "$IMAGE" in
      *paradedb*)
        podman exec "$NAME" psql -U postgres -d memoire -qc "CREATE EXTENSION IF NOT EXISTS pg_search"
        ;;
      *)
        echo "pg-shadow: non-paradedb image override — pg_search skipped, search falls back to tsvector" >&2
        ;;
    esac
    MEMOIRE_PG_URL="$URL" "$ROOT/bin/memoire-pgshadow" apply
    echo "pg-shadow: up at postgres://postgres:***@127.0.0.1:$PORT/memoire"
    ;;
  down)
    podman rm -f "$NAME" >/dev/null 2>&1 || true
    echo "pg-shadow: down"
    ;;
  status)
    podman ps --filter "name=$NAME" --format '{{.Names}} {{.Status}}'
    ;;
  url)
    # Masked by default: terminals and session captures must not receive
    # the password. `url --unsafe` prints the full URL and exists for the
    # make recipes' in-shell command substitution (captured, never
    # printed; the flag itself is the only thing visible in argv).
    if [[ "${2:-}" == "--unsafe" ]]; then
      echo "$URL"
    else
      echo "postgres://postgres:***@127.0.0.1:$PORT/memoire"
    fi
    ;;
  *)
    echo "usage: pg-shadow.sh up|down|status|url" >&2
    exit 2
    ;;
esac
