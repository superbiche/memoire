# Memoire Quickstart — standalone SQLite

Memoire is a local-first memory substrate for coding agents: every agent
session is captured as raw events into SQLite, and past context is searched
and injected back at session start. This quickstart covers the standalone
path only — one machine, SQLite, no external services. The hub/PostgreSQL
topology is documented in the source repository's `docs/` tree
(`ARCHITECTURE.md`, `DEPLOY.md`), which release tarballs do not include.

License: AGPL-3.0-only (see `LICENSE`).

Runtime prerequisites (Linux): `curl`, `jq`, `sha256sum`, GNU `find`,
`timeout` — the capture/inject hooks are fail-open, so a missing tool
means silent non-capture; `memoire-bootstrap.sh check` verifies all of
them before claiming success. opencode capture additionally needs the
`sqlite3` CLI (check notes it if absent). Building from source needs
Go ≥ 1.25, a C toolchain (CGO/SQLite), Node, and the `sqlite3` CLI —
the default test path (`make test`) reads the store with it in the
standalone smoke's derivation legs.

## From a release tarball

Verify, then extract:

```sh
sha256sum -c SHA256SUMS                 # checksum the tarball
gpg --verify SHA256SUMS.asc SHA256SUMS  # signature over the checksum file
tar xzf memoire-<version>-<platform>.tar.gz
cd memoire-<version>-<platform>
./memoire-bootstrap.sh
```

Release artifacts are signed with the maintainer's GPG key
`0878846631FFD1E0`; `gpg --recv-keys 0878846631FFD1E0` fetches it.

## From source

```sh
make build
./scripts/memoire-bootstrap.sh
```

The bootstrap lives at the tarball root (`./memoire-bootstrap.sh`) in a
release and under `scripts/` in the repo; every command below shows the
tarball form — prefix `scripts/` when working from a checkout. It
verifies the layout and required tools, then starts the API once to
initialize `~/.memoire` (directories, SQLite schema, a bearer token),
proves the running API accepts that token, and stops it. Nothing outside
`MEMOIRE_HOME` is touched, and ambient hub/engine configuration
(`MEMOIRE_DB`, `MEMOIRE_PG_URL`, `MEMOIRE_FORWARD_URL`) is rejected
loudly — this tool is standalone-only by contract.

## Run it

```sh
./bin/memoire-api
```

The API listens on `127.0.0.1:37778`, serves `/v1/health`, and is the only
writer to the store. To run it as a systemd user service instead:

```sh
./memoire-bootstrap.sh systemd
```

## Capture your first session

Wire the hooks into your harness. For Claude Code:

```sh
./memoire-bootstrap.sh hooks-claude
```

prints the JSON to merge into `~/.claude/settings.json` — capture on
`UserPromptSubmit` + `PostToolUse`, context injection on `SessionStart`.
Capture hooks are fail-open by design: a broken or stopped Memoire never
blocks the agent session. Codex reuses the same inject script over its
`hooks.json`; Qwen wiring mirrors Claude's (see the capture scripts under
`scripts/`). For opencode, copy `scripts/opencode-plugin/memoire.js` into
your opencode plugin directory and set `MEMOIRE_INJECT_SCRIPT` to this
layout's `scripts/memoire-inject-claude.sh` (the plugin's built-in default
points at a maintainer checkout path).

Then run any agent session and check it landed:

```sh
TOKEN="$(cat ~/.memoire/token)"
curl -s -H "Authorization: Bearer $TOKEN" \
  'http://127.0.0.1:37778/v1/stats'
```

## Search it

```sh
curl -s -H "Authorization: Bearer $TOKEN" \
  'http://127.0.0.1:37778/v1/search?q=<term>&limit=10'
```

The MCP adapter (`bin/memoire-mcp`) exposes the same read surface as four
MCP tools over stdio for harnesses that prefer MCP; point it at the API
with `MEMOIRE_BASE_URL` if you changed the address.

## Configuration (standalone subset)

Everything defaults local; the standalone path needs NO configuration.
The knobs that matter if you deviate:

| Variable | Default | Meaning |
|---|---|---|
| `MEMOIRE_HOME` | `~/.memoire` | store root: DB, queue, raw custody, token, logs |
| `MEMOIRE_ADDR` | `127.0.0.1:37778` | API bind address |
| `MEMOIRE_DB` | `$MEMOIRE_HOME/memoire.db` | SQLite path (a `postgres://` DSN switches engines; honored by the API, refused by the bootstrap) |
| `MEMOIRE_BASE_URL` | `http://$MEMOIRE_ADDR` | the one knob every API consumer honors |
| `MEMOIRE_REDACT` | `1` (on) | defense-at-source secret redaction; `0` disables |
| `MEMOIRE_CAPTURE` | on | `off` is the session-level capture kill switch |
| `MEMOIRE_SCHEMA_DIR` | auto | migrations; resolved cwd-relative, then exe-relative |

Everything else (`MEMOIRE_FORWARD_*`, `MEMOIRE_HL5_*`, `MEMOIRE_SNAPSHOT_*`,
`MEMOIRE_PG_*`, sweep/derive tuning) belongs to the hub/fleet topology and
is documented where those lanes live.

## Optional: derivation (bring your own endpoint)

Raw events can be distilled into reviewed observations by a model you
provide. Point Memoire at any endpoint speaking one of exactly two
dialects — OpenAI-compatible v1 (`/v1/chat/completions`; a local
llama.cpp/vLLM works) or Anthropic (`/v1/messages`). The dialect set is
closed by ruling; the selection is always explicit, never guessed:

```sh
export MEMOIRE_BYOK_BASE_URL="http://localhost:8080"   # your endpoint
export MEMOIRE_BYOK_MODEL="your-model-name"
export MEMOIRE_BYOK_DIALECT="openai-v1"                # or: anthropic
export MEMOIRE_BYOK_DATA_BOUNDARY="local"              # or: external

./bin/memoire-derive -strategy=model -provider=byok \
  -model="$MEMOIRE_BYOK_MODEL" -execute \
  -project=<project> -session-id=<session>
```

(The binary lives at `bin/memoire-derive` in the extracted tarball; from
source, `make build` produces the same path.) The block above is the
keyless case — a local endpoint that wants no key needs no key lines. If
your endpoint requires one, reference it by env NAME and export that
variable too:

```sh
export MEMOIRE_BYOK_API_KEY_ENV="MY_ENDPOINT_KEY"
export MY_ENDPOINT_KEY="…"        # or: MEMOIRE_BYOK_API_KEY_FILE=<path>
                                  #     (file must not be world-readable)
```

You declare where your endpoint lives: `local` (your machine/LAN) or
`external` (a SaaS API) — there is no default, and an external endpoint
additionally requires `-allow-external` on each invocation, because that
run sends the window's raw events off-machine (an external endpoint also
always requires a key source). Keys are referenced by env NAME or key
file, never by value, never logged. Derived observations land as
`proposed` for your review — nothing a model writes is trusted until you
accept it.

Scheduling is yours for now (cron/systemd wanting this hourly is a few
lines); a packaged helper is on the roadmap. The pipeline is documented
in the source repository's `docs/DERIVATION.md`.
