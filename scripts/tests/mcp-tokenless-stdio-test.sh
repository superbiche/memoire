#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# mcp-tokenless-stdio-test.sh — process-level contract for memoire-mcp with
# an unreadable/absent token (memoire:D202): the server must serve the
# JSON-RPC handshake and tools/list with ZERO token I/O before the handshake,
# and fail per-CALL with a diagnosis naming the path tried and the
# MEMOIRE_TOKEN_FILE override. A server that dies (or reads a stream) before
# the handshake presents an auth problem as "this server has no tools".

cd "$(dirname "$0")/../.."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

(cd services/mcp && go build -o "$tmp/memoire-mcp" .)

req_init='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}'
req_inited='{"jsonrpc":"2.0","method":"notifications/initialized"}'
req_list='{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
req_search='{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memoire_search","arguments":{"q":"x"}}}'
req_get='{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"memoire_get_observations","arguments":{"ids":[1]}}}'

fail() {
  echo "mcp-tokenless-stdio-test: FAIL: $1" >&2
  exit 1
}

# Probe A — missing token file: handshake + tools/list served; BOTH tool-call
# shapes (compact handlers and the per-id get_observations handler) fail the
# call with isError and the path+override diagnosis.
out=$(printf '%s\n' "$req_init" "$req_inited" "$req_list" "$req_search" "$req_get" |
  env -i HOME="$tmp/nohome" PATH="$PATH" MEMOIRE_HOME="$tmp/nonexistent" "$tmp/memoire-mcp" 2>"$tmp/a.err")
grep -q '"id":1' <<<"$out" || fail "probe A: no initialize response"
grep -q 'memoire_search' <<<"$out" || fail "probe A: tools/list does not advertise tools"
resp_search=$(grep '"id":3' <<<"$out") || fail "probe A: no response to search call"
resp_get=$(grep '"id":4' <<<"$out") || fail "probe A: no response to get_observations call"
grep -q '"isError":true' <<<"$resp_search" || fail "probe A: search auth failure not isError"
grep -q '"isError":true' <<<"$resp_get" || fail "probe A: get_observations auth failure not isError"
for resp in "$resp_search" "$resp_get"; do
  grep -q 'MEMOIRE_TOKEN_FILE' <<<"$resp" || fail "probe A: call error lacks the override hint"
  grep -q 'nonexistent/token' <<<"$resp" || fail "probe A: call error lacks the token path tried"
done
grep -q 'memoire-mcp:' "$tmp/a.err" || fail "probe A: first failure not logged to stderr"

# Probe B — MEMOIRE_TOKEN_FILE=/dev/stdin falsifier (review round-1 blocker):
# any token I/O before the handshake eats these protocol lines as "the
# token" and the harness sees a dead, tool-less server.
out=$(printf '%s\n' "$req_init" "$req_inited" "$req_list" |
  env -i HOME="$tmp/nohome" PATH="$PATH" MEMOIRE_TOKEN_FILE=/dev/stdin "$tmp/memoire-mcp" 2>"$tmp/b.err")
grep -q '"id":1' <<<"$out" || fail "probe B: pre-handshake token read ate the stream (no initialize response)"
grep -q '"id":2' <<<"$out" || fail "probe B: no tools/list response"

echo "mcp-tokenless-stdio-test: OK"
