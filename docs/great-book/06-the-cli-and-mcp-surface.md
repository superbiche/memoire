# 6. The CLI & MCP surface
*Great Book v0.6 edition — 2026-07-25.*

This chapter is the reference half of the book: what you can call, what you can
run, and what a foreign client sees. Everything here was checked against the
source in the `v0.6` tree rather than against prose, because prose about a
surface goes stale faster than the surface does — the project's own `README.md`
was thirteen days and two eras out of date when this edition was written, and
its list of tools was one of the things that had drifted. If you find a
disagreement between this chapter and the code, the code wins and the chapter
is the bug.

## The HTTP surface

Everything the service does, it does over HTTP. **The API is the only write
authority** — no client, tool, or hook writes to the database directly, which
is what makes a single migration discipline and a single audit trail possible.
The base URL in a default local install is `http://127.0.0.1:37778`, set by
`MEMOIRE_ADDR`.

### Authentication and scopes

Every request except health, readiness, metrics, and the UI shell carries
`Authorization: Bearer <token>`. The token lives in a file — `$MEMOIRE_HOME/token`,
mode `0600`, generated on first start if absent — and the server stores only its
hash. Read-scope requests may alternatively pass the token as a `?token=`
query parameter, which exists so that clients unable to set headers (a browser
tab, a constrained hook) can still read. *(Edition correction, 2026-08-05:
this edition named three unauthenticated surfaces. `GET /metrics` is a fourth,
exempted deliberately (D224): it serves Prometheus counters and gauges only —
no stored content ever crosses it — because the cluster's scraper cannot carry
a bearer token.)*

Three scopes exist, and a credential carries any subset of them:

| Scope | Covers |
|---|---|
| `read` | the corpus `GET`s — search, timeline, observations, events, sessions, stats — plus `POST /v1/search` and `POST /v1/context` |
| `write` | `POST /v1/events`, `POST /v1/events/batch`, `POST /v1/derive/notify` |
| `admin` | every review, elevation, and lifecycle mutation; plus six *read* surfaces: `/v1/credentials`, `/v1/derivation-runs`, `GET /v1/derive/outstanding`, `GET /v1/observations/{id}/review-log`, `GET /v1/embeddings/missing`, and `GET /v1/embeddings/freshness`; and `/v1/admin/lapse-expired` and `PUT /v1/embeddings` |

Being a `GET` is therefore not the same as being read-scope. The operator
surfaces above are readable only with `admin`, because each exposes machinery
rather than memory: credentials, the derivation run history, the cross-project
outstanding-work aggregate, one observation's review history, and the vector
lane's work list and freshness report. A `read` token gets the corpus and
nothing about how the corpus is governed. *(Edition correction, 2026-08-05:
this edition listed four admin-readable `GET`s. The embeddings seam (D218)
added two more, alongside the `PUT` that writes vectors back. The whole
`/v1/embeddings` prefix is admin-gated, `GET`s included, because the work list
carries full observation text and the upsert could poison the vector lane.)*

The split is deliberate and it is the mechanism behind remote capture: a thin
client on another machine gets a `write`-only token, so it can deposit events
into the hub and can neither read the corpus back nor accept an observation.
Rejections are audited — an unauthenticated request lands an `auth.reject`
audit row and a scope failure lands `auth.forbidden`, both carrying the request
ID that is echoed on every response as `X-Request-ID`.

### Reading

These are the endpoints that make foreign-client read parity real. All of them
answer to `curl` with nothing but a bearer token.

- `GET /v1/search` and `POST /v1/search` — search over observations and raw
  events. *(Edition correction, 2026-08-05: this edition described search as
  full-text only. It is lexical-first everywhere, and on the hub's PostgreSQL
  topology an optional hybrid lane fuses MiniLM embeddings with BM25 ranking
  by reciprocal-rank fusion (D218) — same endpoints, same response shape, and
  it fails open to full-text when the embedding service is unreachable. A
  standalone SQLite install stays FTS-only by design.)*
- `GET /v1/timeline` — most recent observations, newest first, no query needed.
- `POST /v1/context` — a prompt-ready formatted block for a topic, rather than
  a result list.
- `GET /v1/observations` and `GET /v1/observations/{id}` — list with filters,
  and single-observation read.
- `GET /v1/observations/{id}/review-log` — the review history of one
  observation (`admin`).
- `GET /v1/events/{id}` — a single raw event.
- `GET /v1/events/tool-window` — tool events inside a time window, the query
  that made assistant-side evidence usable to derivation.
- `GET /v1/sessions` and `GET /v1/sessions/{id}` — session listing and detail.
- `GET /v1/stats` — store counts and write recency, which is also what an
  external write-stall alert polls.
- `GET /v1/health` and `GET /v1/readiness` — two of the unauthenticated
  endpoints. Health returns queue depths, oldest-queue age, last write epoch,
  database and WAL sizes, `redaction_contract_violations`,
  `clock_skew_future_events`, `search_engine` (`fts5`, `bm25`, or
  `tsvector`), `vector_lane` (`pgvector` or `absent`), and `hybrid_search`
  (`absent`, `off`, or `on`) with its `hybrid_fail_open_total` and
  `hybrid_literal_skips_total` counters; chapter 4 explains why the engine
  field is worth polling. *(Edition correction, 2026-08-05: the three vector
  and hybrid fields and their two counters arrived with the hybrid lane (D218)
  and are not in this edition's original list. The fail-open counter is the one
  to watch: it climbs when the embedding service is unreachable and searches
  quietly degrade to full-text, which is exactly the failure a health endpoint
  exists to make visible.)* Every
  counter is best-effort — a failure to compute one reports zero rather than
  failing the endpoint. `version` is stamped at build time from the `VERSION`
  file (an unstamped local build honestly reports `dev`). Readiness is a real
  probe: it round-trips the store and proves the pending queue directory can
  take a write, answering 200 `{"status":"ready"}` with a per-check body when
  both pass and 503 `{"status":"not_ready"}` naming the failed check when
  either does not. Because the endpoint is unauthenticated, the per-check
  values are a closed vocabulary (`ok` / `failed`) — the underlying error
  goes to the server log, never the wire.

### Writing and reviewing

- `POST /v1/events` and `POST /v1/events/batch` — the ingest contract. The body
  is a generic event; each client's native fields live inside `payload`, never
  as top-level columns.
- `DELETE /v1/events/{id}` — visibility deletion, not erasure. Chapter 3
  explains exactly what it does and does not remove.
- `POST /v1/observations/{id}/accept`, `/confirm`, `/reject`, `/dispute`,
  `/lapse`, `/rewrite`, `/elevate`, `/demote`, and the combined `/review` — the
  full review vocabulary, all `admin`. Note that `/accept` and `/confirm` are
  **not** synonyms: accept moves review state only, confirm also promotes
  provenance to `user_confirmed`, and only a confirmed (or imported)
  observation can be elevated. Chapter 2 explains why the two were split.
- `POST /v1/sessions/{id}/lapse-remaining` and `POST /v1/admin/lapse-expired` —
  bulk lifecycle transitions for a session's leftovers and for the expiry
  sweep.
- `POST /v1/derive/notify` and `GET /v1/derive/outstanding` — a client marks
  that a session is worth deriving, and an operator surface reads what is
  outstanding. Notify sits at `write` scope on purpose: a thin client may
  declare intent without gaining any read access.
- `GET /v1/credentials` and `POST /v1/credentials/{id}/revoke` — credential
  inventory and revocation.
- `GET /v1/derivation-runs` and `GET /v1/derivation-runs/{id}` — the provenance
  record of every derivation pass.

`GET /ui/` serves the embedded review single-page app. The shell itself is
auth-exempt because it is static; every `/v1` call the app makes still carries
the bearer token.

## The command line

`make build` produces nine binaries into `bin/`:

| Binary | What it does |
|---|---|
| `memoire-api` | the service: HTTP server, queue drainer, embedded UI |
| `memoire-derive` | run a derivation pass over a session window, in either strategy |
| `memoire-queue` | write one event straight into the file queue, no service needed |
| `memoire-forward` | the client half of remote capture: drain a local queue to a remote hub |
| `memoire-mint-token` | mint a bearer credential with chosen scopes, print it once |
| `memoire-purge` | hard purge — the deliberate, gated erasure tool |
| `memoire-redact-derived` | apply the redaction ruleset to the derived stores |
| `memoire-pgshadow` | the SQLite⇄PostgreSQL groundwork harness: apply, copy, replay, diff |
| `memoire-mcp` | the MCP server (below) |

Two more command packages exist and are deliberately not in the default build.
`services/api/cmd/sweep` builds `memoire-retro-sweep`, the retroactive
redaction sweep of chapter 3 — it is manual tooling, built when a sweep is
actually being run, and the retention conformance test asserts its source is
present precisely because the conformance table calls that item
"manual-tooling". `services/api/cmd/searchcompare` is a one-off evidence
harness for comparing search ranking across engines; its own header says it
lives in the gitignored scratch directory on purpose.

Three habits recur across these tools and are worth knowing before you run any
of them:

**Destructive tools are dry-run by default and double-gated.** `memoire-purge`
and `memoire-redact-derived` both do nothing unless you pass `--apply` *and*
set a matching environment variable (`MEMOIRE_PURGE_APPLY=yes`,
`MEMOIRE_REDACT_DERIVED_APPLY=yes`). One gate is a typo away from firing; two
gates in different modalities are not.

**They refuse to run beside a live writer.** `memoire-purge` aborts if a
`memoire-api` or `memoire-derive` process is alive, and `memoire-redact-derived`
aborts on a live `memoire-derive`. The reason is concrete: the API's drainer
copies queue files into raw custody, and its writes would queue behind the
purge's long transaction and land after it. The skip-quiesce environment
variables exist for hermetic test fixtures and are never for a live store.

**`memoire-derive` carries its overrides as explicit manual flags.** The
targeting hygiene that protects the derivation lane — the project denylist and
the minimum-content window guard — can each be overridden
(`-ignore-denylist`, `-no-min-content-guard`), but only by someone typing the
override. The same binary also carries the sweep helpers the scheduled lanes
call: `-list-targets`, `-list-open-intents`, `-settle-intents`, `-median-epoch`.

`memoire-mint-token` is the credential surface. It generates a random token,
registers only its hash, and prints the plaintext exactly once on stdout; you
move it to the client yourself. Scopes come from `-scopes`, defaulting to
`read,write`, and an unknown scope name is a hard error rather than a silent
drop.

## The MCP server

`memoire-mcp` speaks the Model Context Protocol over stdio, which is how
Memoire reaches any harness with an MCP client. Registering it is one line in
the harness's MCP configuration pointing at the binary.

It exposes exactly four tools, and all four are **read-only**. There is no MCP
write path, no accept tool, and no elevate tool. An agent can consult the
memory; it cannot decide what the memory should contain. That is a governance
boundary, not an oversight, and chapter 2 explains why the elevation gate is
operator-pull only.

- **`memoire_search`** — search over observations. Returns compact hits (id,
  kind, date, project, snippet), enough to judge relevance in one round trip.
  Parameters: `q` (required), `project`, `kind`, `exclude_kind`, `limit`
  (default 20), `include_proposed` (default true). *(Edition correction,
  2026-08-05: described here as full-text only. The tool calls `GET /v1/search`
  and therefore rides whichever lane that install runs — the hybrid MiniLM and
  BM25 fusion on a PostgreSQL hub (D218), full-text on standalone SQLite. The
  tool's parameters and its hit format are the same either way.)*
- **`memoire_timeline`** — the most recent observations, newest first, no query
  required. Same compact hit format. Parameters: `project`, `exclude_kind`,
  `limit` (default 15), `include_proposed` (default true).
- **`memoire_get_observations`** — fetch full bodies by id, where the ids come
  from a search or timeline hit. Takes `ids` as an array of numbers.
- **`memoire_context`** — a topic query that returns a pre-formatted block
  suitable for pasting straight into a prompt. Parameters: `q` (required),
  `project`, `limit` (default 10).

Two defaults encode the design and will surprise you if you do not know them.
First, `exclude_kind` defaults to `raw`, so search returns derived observations
and not capture noise; pass an empty string to search raw events too. Second,
`include_proposed` defaults to true, and a `~` after the kind marks an
unreviewed observation. That is capture-first showing through the read surface:
you see everything the system has, with its confidence status visible, rather
than only the part that has been through a human queue.

That second default belongs to MCP, not to the API underneath. The HTTP read
endpoints default to `accepted` only, and return the low-trust band —
`proposed` and `lapsed` — solely when the caller passes `include_proposed=true`
or filters to kind `open_question`, which the store always widens because a
question invisible until review is a question nobody answers. The MCP tools and
the Claude injector (`scripts/memoire-inject-claude.sh`) both set the flag on
your behalf; a hand-written `curl` does not. If a search through an agent shows
`~` rows and the same query through `curl` does not, this is why.

The intended calling pattern is two-step — search or timeline for compact hits,
then `memoire_get_observations` for the bodies you actually want. Pulling full
bodies for twenty hits when you needed two is the common waste.

## The capture hooks

Capture adapters are shell scripts under `scripts/`, thin by rule: normalize
the input, write a queue file or make one HTTP call, exit. They never block a
session and they fail open.

- **Claude Code** — `memoire-capture-claude.sh` for live hooks, plus
  `memoire-capture-claude-transcripts.sh` for backfilling history from
  transcript files.
- **Codex** — `memoire-capture-codex.sh` and `memoire-capture-codex-rollout.sh`,
  the second parsing Codex's rollout files.
- **Qwen** — `memoire-capture-qwen.sh` and `memoire-capture-qwen-rollout.sh`,
  the latter governed by its own implementer contract in
  `docs/QWEN-ROLLOUT-CAPTURE-CONTRACT.md`.
- **opencode** — `memoire-capture-opencode.sh`, with an injection plugin at
  `scripts/opencode-plugin/memoire.js`.
- **Anything else** — `memoire-capture-text.sh` takes plain text, which is the
  escape hatch for a harness with no adapter.

Injection is the reverse direction: `memoire-inject-claude.sh` runs at session
start and emits an ordered run of conditional blocks. Two carry memory — recent
context (semantic observations for the current project, raw excluded,
unreviewed included) and operating notes (the elevated `can_instruct`
observations). The rest report operational state ahead of them: a dead
transport, a capture backlog, pull mode, the project-identity ask, a clock
alert, a review backlog, and identity alerts. Every block fails open — nothing
here can block a session start, and every path exits 0 — so a failure in one
content block cannot suppress the other. The network reads do share one small
aggregate budget, and when it is spent the remaining network blocks are skipped
by design rather than allowed to overrun the hook's deadline. *(Edition
correction, 2026-08-05: this edition described two independent blocks; the
script emits nine conditional block types, and their independence is bounded by
that shared budget.)* The opencode plugin shells out to this same script under
a hard timeout with a second JavaScript race as a belt, because opencode's
message path awaits the hook and a stuck injector would hang a session.

The remaining scripts are operational rather than capture:
`memoire-derive-auto.sh` (the session-end trigger), `memoire-derive-sweep.sh`
and `derive-timer.sh` (the periodic sweep that catches foreign-harness sessions
the hook never sees), `memoire-derive-hl5-trigger.sh`,
`memoire-conductor-prewarm.sh`, `memoire-resolve-project.sh` and
`memoire-project-move.sh` (identity), `memoire-snapshot.sh` and `backup.sh`,
`token-rotate.sh`, `check-health.sh`, `smoke.sh`, `seed.sh`, and the quadlet
and pg-shadow helpers.

`memoire-queue` deserves a note of its own, because its name misleads: it is
not a queue inspector. It writes a single event directly into
`$MEMOIRE_HOME/queue/pending/` without going through the API at all, taking
the whole event shape as flags — `-project`, `-session-id`, `-source-adapter`,
`-event-type`, `-text`, `-payload-json` or `-payload-file`, and the identity
fields `-project-root` and `-project-source`. It is the offline capture path
and the tool a shell script reaches for when the service may be down.

**One harness is named in this project's own instruction files and has no
capture adapter: Gemini.** `GEMINI.md` exists as an instruction bridge, and the
design documents mention Gemini CLI as a foreign client, but there is no
`memoire-capture-gemini.sh` and no rollout parser for it. A Gemini session can
*read* Memoire — that is what curl parity and MCP are for — and cannot be
captured. Stating that plainly is more useful than a table with an
optimistically checked box.

## Environment variables

The service reads its configuration from the environment, all of it optional:

| Variable | Default | Meaning |
|---|---|---|
| `MEMOIRE_HOME` | `~/.memoire` | the data root: store, queue, raw custody, token, logs |
| `MEMOIRE_ADDR` | `127.0.0.1:37778` | listen address |
| `MEMOIRE_DB` | `$MEMOIRE_HOME/memoire.db` | SQLite path, or a `postgres://` DSN |
| `MEMOIRE_SCHEMA_DIR` | `schema` | migration directory |
| `MEMOIRE_MODEL_ENDPOINTS` | `$MEMOIRE_HOME/model-endpoints.json` | the derivation endpoint registry |
| `MEMOIRE_DERIVE_DENYLIST` | beside the registry | projects excluded from derivation |
| `MEMOIRE_REDACT` | `1` | defense-at-source redaction; `0` disables it |
| `MEMOIRE_FAILED_TTL_DAYS` | `30` | how long failed queue files are kept |
| `MEMOIRE_FORWARD_URL` | unset | remote hub URL for forward mode |
| `MEMOIRE_FORWARD_TOKEN_FILE` | `$MEMOIRE_HOME/forward-token` | the hub token, kept separate from this machine's own |
| `MEMOIRE_SKEW_ALERT_SECONDS` | `60` | clock-skew alert threshold |

Under `MEMOIRE_HOME` the service creates `queue/pending`, `queue/inflight`,
`queue/failed`, `raw/`, and `log/`. The queue directories are the durable
outbox of chapter 2; `raw/` is the custody copy of chapter 3.

---

*This is the last chapter. For where the system stands right now rather than
what it is, read `docs/state/DIGEST.md`; for the reasoning behind any rule in
this book, read the D-numbered entry it cites in `DECISIONS.md`.*
