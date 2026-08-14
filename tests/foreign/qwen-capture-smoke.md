# Qwen Capture Smoke

Purpose: prove Qwen CLI can write a note into Memoire through the local file queue without calling HTTP from the harness.

This uses `scripts/memoire-capture-qwen.sh`, which is a thin Qwen-default wrapper around the generic text capture adapter.

## Start Memoire

From `<repo-root>`:

```bash
make build
rm -rf tmp/qwen-capture-home
MEMOIRE_HOME=./tmp/qwen-capture-home \
MEMOIRE_ADDR=127.0.0.1:37778 \
./bin/memoire-api
```

Leave the API running.

## Qwen Prompt

Run `qwen` from `<repo-root>`, then paste:

```text
We are doing a Memoire Qwen capture smoke.

Use shell only. Do not print the Memoire token. Do not read unrelated files.

Run:

NEEDLE="qwen-capture-$(date +%s)"
printf "%s" "$NEEDLE" | MEMOIRE_HOME=./tmp/qwen-capture-home ./scripts/memoire-capture-qwen.sh --project memoire-qwen-capture
TOKEN="$(tr -d '\r\n' < ./tmp/qwen-capture-home/token)"
for _ in $(seq 1 30); do
  if curl -fsS -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:37778/v1/search?q=$NEEDLE&project=memoire-qwen-capture&limit=5" | grep -q "$NEEDLE"; then
    echo PASS
    exit 0
  fi
  sleep 0.2
done
echo FAIL
exit 1

Pass condition: command output contains PASS.
Answer only PASS or FAIL with a short sanitized reason.
```

Qwen CLI may expand short Shell output in the UI. That is acceptable if the token is not printed.

## Pass Criteria

- Qwen writes through `scripts/memoire-capture-qwen.sh`.
- The Memoire API drains the queued file.
- `GET /v1/search` returns the seeded needle.
- Qwen does not print the token.

## Cleanup

Stop Memoire with `Ctrl-C`.
