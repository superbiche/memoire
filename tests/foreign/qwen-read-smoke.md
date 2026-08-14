# Qwen Read Smoke

Purpose: prove Qwen CLI can read Memoire through plain HTTP/curl using the local token, without Anthropic-specific plumbing.

## Start Memoire

From `<repo-root>`:

```bash
make build
rm -rf tmp/qwen-smoke-home
MEMOIRE_HOME=./tmp/qwen-smoke-home \
MEMOIRE_ADDR=127.0.0.1:37778 \
./bin/memoire-api
```

Leave the API running.

## Seed A Known Record

In another terminal:

```bash
cd <repo-root>
MEMOIRE_HOME=./tmp/qwen-smoke-home \
MEMOIRE_FOREIGN_NEEDLE_PREFIX=qwen-foreign-read \
./tests/foreign/seed-read-smoke.sh
```

Copy the printed `BASE`, `TOKEN_FILE`, `PROJECT`, and `NEEDLE` values. Do not copy or print the token.

## Qwen Prompt

Run `qwen` from `<repo-root>`, then paste this prompt with the seeded values filled in:

```text
We are doing a Memoire foreign-client read smoke.

Use shell/curl only. Do not print the token. Do not read unrelated files.

Token path: <TOKEN_FILE>
Base URL: <BASE>
Project: <PROJECT>
Needle: <NEEDLE>

Set TOKEN from the token file without printing it.

Then run:
1. GET /search with q=<NEEDLE>, project=<PROJECT>, limit=5
2. POST /context with q=<NEEDLE>, project=<PROJECT>, limit=5

Pass condition: both responses include the needle.

Answer only:
PASS
or
FAIL: <short sanitized reason>

Do not include the token or full JSON response.
```

Qwen CLI may expand short Shell tool output in the UI. That is acceptable for this smoke if the token is not printed and the final answer is `PASS`.

## Pass Criteria

- Qwen reads `TOKEN_FILE` without printing the token.
- Qwen uses curl against `BASE`.
- `GET /search` returns the seeded `NEEDLE`.
- `POST /context` returns the seeded `NEEDLE`.
- Qwen final answer is `PASS`, or a short sanitized failure.

## Cleanup

Stop Memoire with `Ctrl-C`.
