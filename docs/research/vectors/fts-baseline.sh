#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
# D183 — FTS latency baseline over the frozen reference query set
# (reference-queries.tsv beside this script; header skipped). Runs each
# query verbatim (project + limit as issued) ITER times against the hub
# /v1/search, records curl time_total (seconds) + HTTP code per call.
# Output: $OUT (default ./fts-baseline-timings.tsv, noclobber-overwrite).
# The ratified 2026-08-02 baseline run (p95 300ms) is archived at
# tmp/d183-kill-criteria/fts-baseline-timings.tsv.
# Token is read from MEMOIRE_TOKEN_FILE and never printed.
set -euo pipefail
BASE="${MEMOIRE_BASE_URL:-https://memoire.superbiche.me}"
TOKEN_FILE="${MEMOIRE_TOKEN_FILE:?}"
ITER="${ITER:-10}"
IN="$(dirname "$0")/reference-queries.tsv"
OUT="${OUT:-./fts-baseline-timings.tsv}"
: >| "$OUT"
tok="$(cat "$TOKEN_FILE")"
n=0
while IFS=$'\t' read -r id _cat _gt _date _src target limit q; do
  [ "$id" = "id" ] && continue
  [ -n "$q" ] || continue
  n="$id"
  args=(--data-urlencode "q=$q")
  [ "$target" != "-" ] && args+=(--data-urlencode "project=$target")
  [ "$limit" != "-" ] && args+=(--data-urlencode "limit=$limit")
  # one warm-up call, untimed
  curl -sS -G -m 15 -H "Authorization: Bearer $tok" "${args[@]}" \
    -o /dev/null "$BASE/v1/search" || true
  for i in $(seq 1 "$ITER"); do
    t=$(curl -sS -G -m 15 -H "Authorization: Bearer $tok" "${args[@]}" \
      -o /dev/null -w '%{time_total} %{http_code}' "$BASE/v1/search" || echo "NA 000")
    printf '%d\t%d\t%s\n' "$n" "$i" "$t" >> "$OUT"
  done
done < "$IN"
echo "queries=$n iterations=$ITER -> $OUT"
