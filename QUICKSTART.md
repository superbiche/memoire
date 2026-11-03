# Memoire Quickstart — standalone SQLite

Memoire is a local-first capture and custody service for coding agents: every
agent session is captured as raw events into a store you own, and you search that
record by exact text and date over HTTP, MCP or a shell. It is the source of
record, not a memory layer — deciding what a record means is the job of a
separate system. This quickstart covers the standalone path only: one machine,
SQLite, no external services. Chapter 4 of the Great Book (`docs/great-book/`)
covers PostgreSQL and the hub shape.

License: AGPL-3.0-only (see `LICENSE`).

Runtime prerequisites (Linux): `curl`, `jq`, `sha256sum`, GNU `find`, `timeout`.
The capture hooks are fail-open, so a missing tool means silent non-capture;
`memoire-bootstrap.sh check` verifies all of them before claiming success.
opencode capture additionally needs `sqlite3` ≥ 3.44 with JSON support and
`jq` ≥ 1.7. Running `make test` needs the `sqlite3` CLI too. Building from source needs
Go ≥ 1.25, a C toolchain (the SQLite driver uses CGO), and Node with network
access for the first `npm ci` (the embedded web interface is built during
`make build`).

## Install

From a release tarball, verify, then extract:

```sh
sha256sum -c SHA256SUMS                 # checksum the tarball
gpg --verify SHA256SUMS.asc SHA256SUMS  # signature over the checksum file
tar xzf memoire-<version>-<platform>.tar.gz
cd memoire-<version>-<platform>
./memoire-bootstrap.sh
```

Release artifacts are signed with the maintainer's GPG key `0878846631FFD1E0`;
`gpg --recv-keys 0878846631FFD1E0` fetches it.

From source:

```sh
make build
./scripts/memoire-bootstrap.sh
```

`make build` writes the binaries to `bin/`. The bootstrap lives at the tarball
root (`./memoire-bootstrap.sh`) in a release and under `scripts/` in a checkout;
the commands below use the tarball form, so prefix `scripts/` in a checkout. With
no argument it runs `check` (layout and tools), then `init`: it starts the API
once to create `~/.memoire` (directories, the SQLite schema, a bearer token),
proves the running API accepts that token, and stops it. Nothing outside
`MEMOIRE_HOME` is touched, and ambient hub or engine configuration
(`MEMOIRE_DB`, `MEMOIRE_PG_URL`, `MEMOIRE_FORWARD_URL`) is rejected loudly — this
tool is standalone-only by contract.

## Run it

```sh
./bin/memoire-api
```

The API listens on `127.0.0.1:37778`, serves `GET /v1/health`, and is the only
writer to the store. To run it as a systemd user service instead:

```sh
./memoire-bootstrap.sh systemd
```

Check it:

```sh
curl -s http://127.0.0.1:37778/v1/health
```

## Capture an event, then read it back

You do not need a harness to see the whole loop. `memoire-queue` writes one event
into the file queue without calling the service; the running API drains the queue
into the store.

```sh
./bin/memoire-queue -project quickstart -session-id qs-1 \
  -source-adapter manual -event-type note \
  -text "the staging database must never be seeded from the production dump"
```

It prints the path of the queue file it wrote. A moment later, search for it:

```sh
TOKEN="$(cat ~/.memoire/token)"
curl -s -H "Authorization: Bearer $TOKEN" \
  'http://127.0.0.1:37778/v1/search?q=staging+seeded&project=quickstart&limit=5'
```

The hit carries the event's text, a highlighted snippet, `"kind":"raw"` and
`occurred_at_epoch`, the time the event happened. The query is split into words
and matches any of them, ranking the hits that match more words higher; it is not
a query language, so phrase quotes and operators have no special meaning.

## Capture a real session

Capture adapters are shell scripts that write the same queue. Each is fail-open: a
broken or stopped Memoire never blocks the agent. `MEMOIRE_CAPTURE=off` in the
environment is a session-level kill switch, and a line `capture: false` in a
repository's `.memoire` file disables capture for that repository.

**Claude Code.** Print the hook configuration and merge it into
`~/.claude/settings.json`:

```sh
./memoire-bootstrap.sh hooks-claude
```

The `UserPromptSubmit` and `PostToolUse` entries capture prompts and tool use. The
`SessionStart` entry is the optional context injector, and it is off by default:
it prints nothing unless `MEMOIRE_INJECT=on` is set. You can leave it out.

**Other harnesses.** These sweep the harness's own session files or store, and you
schedule them yourself (cron or a systemd timer, every few minutes):

| Harness | Script | Notes |
|---|---|---|
| Codex | `scripts/memoire-capture-codex-rollout.sh --scan --tail` | reads `~/.codex/sessions` |
| Qwen | `scripts/memoire-capture-qwen-rollout.sh --scan` | `--help` lists the options |
| opencode | `scripts/memoire-capture-opencode.sh --scan` | needs the `sqlite3` CLI |
| Pi | copy `scripts/pi-extension/memoire.js` to `~/.pi/agent/extensions/` | set `MEMOIRE_INJECT_SCRIPT` to this layout's `scripts/memoire-inject-claude.sh` |
| anything else | `scripts/memoire-capture-text.sh` | text as an argument, `--text`, or piped on stdin |

For opencode there is also a plugin, `scripts/opencode-plugin/memoire.js`; copy it
into your opencode plugin directory and set `MEMOIRE_INJECT_SCRIPT` the same way.
Gemini has no capture adapter.

Then run any agent session and check it landed:

```sh
curl -s -H "Authorization: Bearer $TOKEN" 'http://127.0.0.1:37778/v1/stats'
```

## Search and read it back

Over HTTP, with the token from `~/.memoire/token`:

```sh
BASE=http://127.0.0.1:37778
AUTH="Authorization: Bearer $TOKEN"

# search, optionally only events from the last 24 hours (a Unix epoch in seconds)
curl -s -H "$AUTH" "$BASE/v1/search?q=<words>&limit=10"
curl -s -H "$AUTH" "$BASE/v1/search?q=<words>&occurred_after=$(( $(date +%s) - 86400 ))"

# the most recent events of a project, newest first
curl -s -H "$AUTH" "$BASE/v1/timeline?project=<project>&limit=10"

# a prompt-ready block, one dated line per hit
curl -s -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"q":"<words>","project":"<project>","limit":5}' "$BASE/v1/context"
```

These routes serve raw captured records only. A request for a derived kind
(`kind=` something other than `raw`, or `exclude_kind=raw`) is answered with a
`400` that names the `include_derived=true` opt-in; that opt-in exists for rows
left by the retired derivation lane (Great Book, chapter 2) and a fresh install
has none. The HTTP routes read all of history unless you pass `occurred_after`.

Over MCP, register the stdio server with your harness. The exact shape of the
configuration depends on the harness; the server itself is the binary
`bin/memoire-mcp`, with the API address in `MEMOIRE_BASE_URL` if you changed it:

```json
{
  "mcpServers": {
    "memoire": {
      "command": "/absolute/path/to/bin/memoire-mcp",
      "env": { "MEMOIRE_BASE_URL": "http://127.0.0.1:37778" }
    }
  }
}
```

It reads the bearer token from `$MEMOIRE_HOME/token` (or `MEMOIRE_TOKEN_FILE`) on
the first tool call. It exposes four read-only tools: `memoire_search`,
`memoire_timeline`, `memoire_context` and `memoire_get_observations`. The first
three take a `days` look-back (default 30; `0` lifts the bound), a `project`
filter and a `limit`. Every hit is dated. Their descriptions tell an agent
that a hit is dated evidence, not current truth, and that questions of what was
decided or learned belong to the memory layer.

## Configuration (standalone subset)

Everything defaults local; the standalone path needs no configuration. The knobs
that matter if you deviate:

| Variable | Default | Meaning |
|---|---|---|
| `MEMOIRE_HOME` | `~/.memoire` | store root: database, queue, raw custody, token, logs |
| `MEMOIRE_ADDR` | `127.0.0.1:37778` | API bind address |
| `MEMOIRE_BASE_URL` | `http://$MEMOIRE_ADDR` | API base URL for the hooks, MCP server and tools (`scripts/smoke.sh` reads `MEMOIRE_BASE` instead, with the `/v1` suffix) |
| `MEMOIRE_DB` | `$MEMOIRE_HOME/memoire.db` | SQLite path (a `postgres://` DSN switches engines; honored by the API, refused by the bootstrap) |
| `MEMOIRE_REDACT` | `1` (on) | defense-at-source secret redaction; `0` disables |
| `MEMOIRE_CAPTURE` | on | `off` is the session-level capture kill switch |
| `MEMOIRE_INJECT` | off | `on` turns on the session-start context injector |
| `MEMOIRE_SCHEMA_DIR` | auto | migrations; resolved cwd-relative, then exe-relative |

Secrets are redacted before anything is written; the redaction key is generated at
`$MEMOIRE_HOME/redaction-key`. Everything else (`MEMOIRE_FORWARD_*`,
`MEMOIRE_SNAPSHOT_*`, `MEMOIRE_PG_*`) belongs to the hub topology and is described
in the Great Book, chapters 4 and 6.

## Where next

In the repository:

- `docs/great-book/` — the manual: what the system is, custody and redaction, the
  full HTTP, CLI and MCP reference.
- `MISSING.md` — the known gaps of this version.
- From a checkout, `./scripts/smoke.sh` runs a smoke check against a running API,
  and `make test` runs the default test suite, which includes a standalone-SQLite
  end-to-end test.
