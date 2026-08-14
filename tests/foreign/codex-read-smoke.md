# Codex Read Smoke

Purpose: prove Codex can read Memoire through plain HTTP/curl using the local token, without Anthropic-specific plumbing.

This can be run by a Codex session with shell access.

## Start Memoire

From `<repo-root>`:

```bash
make build
rm -rf tmp/codex-smoke-home
MEMOIRE_HOME=./tmp/codex-smoke-home \
MEMOIRE_ADDR=127.0.0.1:37778 \
./bin/memoire-api
```

Leave the API running.

## Seed A Known Record

In another terminal or shell tool:

```bash
cd <repo-root>
MEMOIRE_HOME=./tmp/codex-smoke-home \
MEMOIRE_FOREIGN_NEEDLE_PREFIX=codex-foreign-read \
./tests/foreign/seed-read-smoke.sh
```

The script prints `BASE`, `TOKEN_FILE`, `PROJECT`, and `NEEDLE`. It does not print the token.

## Codex Read Check

Set `TOKEN` from `TOKEN_FILE` without printing it, then check both read endpoints:

```bash
TOKEN="$(tr -d '\r\n' < <TOKEN_FILE>)"
curl -fsS -H "Authorization: Bearer $TOKEN" \
  "<BASE>/search?q=<NEEDLE>&project=<PROJECT>&limit=5" | grep -q "<NEEDLE>"
curl -fsS -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -X POST "<BASE>/context" \
  -d '{"q":"<NEEDLE>","project":"<PROJECT>","limit":5}' | grep -q "<NEEDLE>"
```

## Pass Criteria

- Codex reads `TOKEN_FILE` without printing the token.
- Codex uses curl against `BASE`.
- `GET /search` returns the seeded `NEEDLE`.
- `POST /context` returns the seeded `NEEDLE`.

## Cleanup

Stop Memoire with `Ctrl-C`.
