# Codex Capture Smoke

Purpose: prove Codex can write a note into Memoire through the local file queue without calling HTTP from the harness.

This uses `scripts/memoire-capture-codex.sh`, which is a thin Codex-default wrapper around the generic text capture adapter.

## Start Memoire

From `<repo-root>`:

```bash
make build
rm -rf tmp/codex-capture-home
MEMOIRE_HOME=./tmp/codex-capture-home \
MEMOIRE_ADDR=127.0.0.1:37778 \
./bin/memoire-api
```

Leave the API running.

## Codex Smoke

From a Codex shell tool in `<repo-root>`:

```bash
NEEDLE="codex-capture-$(date +%s)"
printf "%s" "$NEEDLE" | MEMOIRE_HOME=./tmp/codex-capture-home ./scripts/memoire-capture-codex.sh --project memoire-codex-capture
TOKEN="$(tr -d '\r\n' < ./tmp/codex-capture-home/token)"
for _ in $(seq 1 30); do
  if curl -fsS -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:37778/v1/search?q=$NEEDLE&project=memoire-codex-capture&limit=5" | grep -q "$NEEDLE"; then
    echo PASS
    exit 0
  fi
  sleep 0.2
done
echo FAIL
exit 1
```

The same check is also available as a repeatable local target:

```bash
make codex-capture-smoke
```

That target starts an isolated Memoire API, writes through `scripts/memoire-capture-codex.sh`, searches for the captured needle, then cleans up.

## Pass Criteria

- Codex writes through `scripts/memoire-capture-codex.sh`.
- The Memoire API drains the queued file.
- `GET /v1/search` returns the seeded needle.
- The token is not printed.

## Cleanup

Stop Memoire with `Ctrl-C`.
