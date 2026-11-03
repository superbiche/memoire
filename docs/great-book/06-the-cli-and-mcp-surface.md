# 6. The CLI & MCP surface
*Great Book v0.7 edition — 2026-11-02.*

This chapter is the reference half of the book: what you can call, what you can
run, and what a foreign client sees. Everything here was checked against the
source rather than against prose, because prose about a surface goes stale faster
than the surface does. The versioned inventory of HTTP routes and MCP tools is
`docs/API-SURFACE.json`, enforced by contract tests that fail the build on a
mismatch; the routes and tools below are the ones in it. If you find a
disagreement between this chapter and the code, the code wins and the chapter is
the bug.

## The HTTP surface

Everything the service does, it does over HTTP. **The API is the only write
path for captured events**: no client, hook or harness adapter writes an event to
the database directly, which is what keeps one ingest discipline and one audit
trail. The exceptions are operator maintenance tools that open a store
themselves, and each picks its target its own way, so check the effective
target, not one variable, before running one:

| Tool | What it writes | How it picks the store | Migrates? |
|---|---|---|---|
| `memoire-mint-token` | a credential | the service configuration (`MEMOIRE_DB`, default `$MEMOIRE_HOME/memoire.db`) | yes, on SQLite (it opens the store like the API) |
| `memoire-purge` | deletes rows and files | `--db`; else `MEMOIRE_DB` only if it is a PostgreSQL DSN; else `<home>/memoire.db` | no |
| `memoire-redact-derived` | rewrites stored text | `--db`; else `MEMOIRE_DB` only if it is a PostgreSQL DSN; else `~/.memoire/memoire.db` | no |
| `memoire-pgshadow` | a PostgreSQL store | `-pg` or `MEMOIRE_PG_URL` (the SQLite source is `-db`) | only through its `apply` subcommand; `replay` expects the schema already applied |

A SQLite-valued `MEMOIRE_DB` is ignored by purge and redact-derived. The
base URL in a default local install is `http://127.0.0.1:37778`, set by
`MEMOIRE_ADDR`.

### Authentication and scopes

Every request except health, readiness, metrics and the UI shell carries
`Authorization: Bearer <token>`. The local token lives in a file —
`$MEMOIRE_HOME/token`, generated on first start if absent — and the server stores
only its hash. Read-scope requests may alternatively pass the token as a `?token=`
query parameter, which exists so that clients unable to set headers (a browser
tab, a constrained hook) can still read. Four surfaces are unauthenticated:
`GET /v1/health`, `GET /v1/readiness`, `GET /metrics` (Prometheus counters and
gauges only, no stored content crosses it, D224) and the static `/ui/` shell.

Three scopes exist, and a credential carries any subset of them:

| Scope | Covers |
|---|---|
| `read` | the corpus `GET`s — search, timeline, observations, events, sessions, stats, an observation's usage lines — plus `POST /v1/search` and `POST /v1/context` |
| `write` | `POST /v1/events`, `POST /v1/events/batch`, `POST /v1/derive/notify` |
| `admin` | every review, elevation and lifecycle mutation; `DELETE /v1/events/{id}`; and the operator read surfaces: credentials, derivation runs, the outstanding-work list, an observation's review log, the triage reads, and the embeddings seam |

Being a `GET` is therefore not the same as being read-scope. The admin-only reads
expose machinery rather than records: credentials, the history of retired
derivation runs, one observation's review history. A `read` token gets the corpus
and nothing about how the corpus is governed.

The split is deliberate and it is the mechanism behind remote capture: a thin
client on another machine gets a `write`-only token, so it can deposit events into
the hub and can neither read the record back nor change it. Rejections are
audited — an unauthenticated request lands an `auth.reject` audit row and a scope
failure lands `auth.forbidden`, both carrying the request id that is echoed on
every response as `X-Request-ID`.

### Reading: the raw, dated defaults

These are the endpoints that make foreign-client read parity real. All of them
answer to `curl` with nothing but a bearer token.

- `GET /v1/search` and `POST /v1/search` — search over the raw projection (`q`
  required). A hit carries its `occurred_at_epoch` (when the event happened), a
  highlighted snippet, project, kind, session, source adapter and event type.
  The query is not interpreted as a search language: it is split into word tokens
  (letters, digits and `_ @ . / : -`), each token is quoted, and the tokens are
  OR-joined, so a hit matches any of the words and ranking favors those matching
  more. Phrase quotes, `NEAR` and boolean operators in `q` have no special meaning,
  and a query with no usable token is a `400`.
- `GET /v1/timeline` — most recent records, newest first, no query needed.
- `POST /v1/context` — a prompt-ready block for a topic rather than a result
  list: one line per hit, `[id] YYYY-MM-DD project label: snippet`, the date being
  the day the event occurred.
- `GET /v1/observations` and `GET /v1/observations/{id}` — list with filters and
  a cursor, and single read. `GET /v1/observations/{id}/usage` returns the
  attribution lines recorded against an observation.
- `GET /v1/events/{id}` — a single raw event; `GET /v1/events/tool-window` — the
  tool events inside a time window of a session (`session_id` required).
- `GET /v1/sessions` and `GET /v1/sessions/{id}` — session listing and detail.
- `GET /v1/stats` — exact store counts and write recency (a full-table count,
  heavy on a large store — not for probes).
- `GET /v1/health`, `GET /v1/readiness`, `GET /metrics` — chapter 4.

Five of these — search (both methods), context, timeline, observation list and
observation by id — carry the L0010 defaults, and they are the part of this
surface most likely to surprise a client written against the previous edition:

- **They serve raw records only.** Without `include_derived=true` (a query
  parameter, or `"include_derived": true` in a POST body) the filter is forced to
  kind `raw`. The retired derivation lane's rows are not served.
- **A derived-kind ask without the opt-in is a `400`.** Asking for a `kind` other
  than `raw`, or for `exclude_kind=raw`, answers with an error that names the
  opt-in: "derived observations are not served by default; pass
  include_derived=true". An old client therefore learns why it gets nothing,
  rather than reading an empty answer.
- **A derived row fetched by id is a `404`** without the opt-in. The by-id route
  takes no kind to reject, so the forced raw kind simply leaves a derived row
  unfound.
- **`occurred_after` bounds the read by occurred time.** It is a Unix epoch in
  seconds, accepted on the GET routes as a query parameter and in the POST bodies
  of search and context. Absent or `0` means no bound. A malformed or negative
  value is a `400`, never a silent full-history read. The by-id route takes no
  bound: an exact fetch is not a window.
- **There is no server-side default window.** The HTTP routes read all of history
  unless you pass `occurred_after`. The thirty-day default belongs to the MCP
  tools (below).

The other filters are unchanged: `project`, `session_id`, `limit` (search defaults
to 20, timeline and the list to 50), `include_noise`, `include_proposed`,
`review_state`, `can_instruct`, `derivation_run_id`, `include_agent_accepted`,
`order` and `cursor` on the list, and `hybrid` on search (which only matters if an
embedding service is configured; none is deployed). These apply on top of the
forced raw kind; they do not lift it.

The opt-in exists for exactly the clients that read derived rows on purpose. The
embedded review interface sends it on every call. The session-start injector sends
it on its two content reads. Everything else, including the MCP server, does not.

Responses to project-filtered reads may carry `identity_alerts` — advisory lines
about conflicting project roots — which the MCP tools prepend to their output.

### Writing

- `POST /v1/events` — the ingest contract. It answers `202` with
  `{"queued": true}` once the bytes are durable in the pending queue; envelope
  validation and the store write happen afterwards in the drainer, and a body
  that does not validate is preserved in `queue/failed/` rather than refused,
  because capture is never blocked. The body is a generic event; each client's
  native fields live inside `payload`, never as top-level columns.
- `POST /v1/events/batch` — the same, many envelopes per request, reporting how
  many were queued and rejected.
- `DELETE /v1/events/{id}` — visibility deletion, not erasure (`admin`). Chapter 3
  explains exactly what it does and does not remove.

### Routes that belong to the retired lane

These routes still exist, are still in the contract, and still work against
stored rows. They are listed so that you recognize them, not because the system
produces the data they serve.

- `POST /v1/observations/{id}/accept`, `/confirm`, `/reject`, `/dispute`,
  `/lapse`, `/rewrite`, `/elevate`, `/demote`, `/review` and `/action`, and
  `GET /v1/observations/{id}/review-log` — the review and elevation vocabulary.
  All `admin`. `accept` and `confirm` are not synonyms: accept moves review state
  only, confirm also promotes provenance, and only a confirmed observation can be
  elevated.
- `POST /v1/sessions/{id}/lapse-remaining` and `POST /v1/admin/lapse-expired` —
  bulk lifecycle transitions.
- `POST /v1/derive/notify` (`write`) and `GET /v1/derive/outstanding` (`admin`) —
  a client marks a session as worth deriving; nothing on the hub acts on it.
- `GET /v1/derivation-runs` and `GET /v1/derivation-runs/{id}` (`admin`) — the
  provenance record of every derivation pass that ran.
- `GET /v1/triage/queue`, `/validated`, `/genres`, `/graduation-candidates`,
  `/override-rates` (`admin`) — the triage lane's reads.
- `GET /v1/embeddings/missing`, `PUT /v1/embeddings`, `GET /v1/embeddings/freshness`
  (`admin`) — the embedding worker's seam.
- `GET /v1/credentials` and `POST /v1/credentials/{id}/revoke` (`admin`) —
  credential inventory and revocation. These are live, not retired.

`GET /ui/` serves the embedded review single-page app. The shell itself is
auth-exempt because it is static; every `/v1` call the app makes carries the
bearer token. It is a review tool for stored derived rows, and it is the one
first-party client that asks for them.

## The command line

`make build` produces eleven binaries into `bin/`:

| Binary | What it does |
|---|---|
| `memoire-api` | the service: HTTP server, queue drainer, embedded UI |
| `memoire-queue` | write one event straight into the file queue, no service needed |
| `memoire-forward` | the client half of remote capture: drain a local queue to a remote hub |
| `memoire-mint-token` | mint a bearer credential with chosen scopes, print it once |
| `memoire-purge` | hard purge — the deliberate, gated erasure tool |
| `memoire-redact-derived` | re-apply the current redaction ruleset in place to stored rows |
| `memoire-raw-pack` | pack closed days of raw custody into immutable compressed parts |
| `memoire-pgshadow` | the SQLite⇄PostgreSQL harness: `apply`, `copy`, `replay`, `diff`, `reindex-bm25` |
| `memoire-mcp` | the MCP server (below) |
| `memoire-derive` | run a derivation pass over a session window — manual, retired as a lane |
| `memoire-triage` | the triage lane's judging sweep, appending suggestions — not scheduled |

The release tarball ships eight of them: `memoire-api`, `memoire-queue`,
`memoire-mint-token`, `memoire-mcp`, `memoire-derive`, `memoire-forward`,
`memoire-purge` and `memoire-redact-derived`. The operations image used on the hub carries a different set —
`memoire-derive`, `-queue`, `-purge`, `-raw-pack`, `-redact-derived`,
`-mint-token`, `-pgshadow` and `-triage` — together with the scripts.

Two more command packages exist and are deliberately not in the default build.
`services/api/cmd/sweep` builds `memoire-retro-sweep`, the retroactive redaction
sweep of chapter 3 — manual tooling, built when a sweep is actually being run.
`services/api/cmd/searchcompare` is a one-off evidence harness for comparing
search ranking across engines.

Three habits recur across these tools:

**Destructive tools are dry-run by default and double-gated.** `memoire-purge` and
`memoire-redact-derived` do nothing unless you pass `--apply` *and* set a matching
environment variable (`MEMOIRE_PURGE_APPLY=yes`, `MEMOIRE_REDACT_DERIVED_APPLY=yes`).
One gate is a typo away from firing; two gates in different modalities are not.

**They refuse to run beside a live writer.** On SQLite, `memoire-purge` aborts if a
`memoire-api` or `memoire-derive` process is alive, and `memoire-redact-derived`
aborts on a live `memoire-derive`. The reason is concrete: the API's drainer copies
queue files into raw custody, and its writes would queue behind the tool's long
transaction and land after it. On the PostgreSQL hub a process check cannot see
other pods, so both tools instead refuse to apply without
`--i-have-quiesced-the-hub`, the operator's attestation that the hub is scaled
down and its store jobs are suspended.

**`memoire-raw-pack` is dry-run by default** and writes only with `--apply`;
`--verify` prints a digest of the whole raw store and `--unpack PART` rolls one
part back.

`memoire-mint-token` generates a random token, registers only its hash, and prints
the plaintext exactly once. `-label` is required; scopes come from `-scopes`,
defaulting to `read,write`.

`memoire-queue` deserves a note, because its name misleads: it is not a queue
inspector. It writes a single event directly into `$MEMOIRE_HOME/queue/pending/`
without going through the API at all, taking the whole event shape as flags —
`-project`, `-session-id`, `-source-adapter` (default `manual`), `-event-type`
(default `note`), `-text` or `-text-file`, `-payload-json` or `-payload-file`,
`-metadata-json`, `-occurred-at` or `-occurred-at-epoch`, `-source-event-id`
(without it each run gets a fresh id, so repeating a command records a second
event), the identity fields
`-project-root` and `-project-source`, and `-batch-file` for a JSONL file of
events. It is the offline capture path and the tool a shell script reaches for
when the service may be down.

`memoire-derive` still builds and still runs: its `rules` strategy turns typed
markers into observations, and its `model` strategy sends a window to an endpoint
you provide (exactly two wire dialects, OpenAI-compatible and Anthropic, declared
explicitly, with the endpoint's data boundary — `local` or `external` — declared
as well, and an explicit per-run opt-in before anything leaves for an external
one). Its sweep
helpers and the scripts built on them are unscheduled. Treat it as a bench tool,
not as part of the running system.

## The MCP server

`memoire-mcp` speaks the Model Context Protocol over stdio, which is how Memoire
reaches any harness with an MCP client. Registering it is one line in the
harness's MCP configuration pointing at the binary. It reads `MEMOIRE_BASE_URL`
(or `MEMOIRE_ADDR`) and a token from `MEMOIRE_TOKEN_FILE` (default
`$MEMOIRE_HOME/token`). The token is read lazily on the first tool call, never
before the protocol handshake, so a configuration problem shows up as an error on
a call rather than as a server that silently advertises no tools.

It exposes exactly four tools, and all four are **read-only**. There is no MCP write
path, no accept tool and no elevate tool. An agent can consult the record; it
cannot decide what the record contains.

Since L0010 the tools are a **dated recent raw lookup**. They serve the raw
projection (the API's default; the tools never send `include_derived`), they take
no `kind`, `exclude_kind` or `include_proposed` parameters, and each of the three
that searches takes a `days` parameter.

- **`memoire_search`** — full-text search over recent raw captures. Parameters:
  `q` (required), `project`, `days`, `limit` (default 20).
- **`memoire_timeline`** — the most recent raw captures, newest first, no query
  required. Parameters: `project`, `days`, `limit` (default 15).
- **`memoire_context`** — a topic query that returns a pre-formatted block ready to
  paste into a prompt, one dated line per hit. Parameters: `q` (required),
  `project`, `days`, `limit` (default 10).
- **`memoire_get_observations`** — full bodies by id, where the ids come from a
  search or timeline hit. Takes `ids` as an array of numbers, at most 50 per call. A
  derived row's id answers with an inline error for that id, because the by-id route
  is raw-only by default.

`days` is a look-back in days by *occurred* time. It defaults to 30; `0` lifts the
bound; a negative value is treated as the default rather than silently lifting the
bound; very large values are capped. The tool turns it into the `occurred_after`
epoch the API takes.

Search and timeline return one compact line per hit:

```
[id] kind~ YYYY-MM-DD project provenance/review_state[!]: snippet
```

A raw capture renders with kind `raw`, provenance `observed` and review state
`accepted`, and the date is when the event occurred. (The `~` and `!` markers
belong to the retired review vocabulary — unreviewed and instruction-bearing — and
do not appear on raw hits.)

**The tool declarations carry a standing rule**, because the declaration is the
text an agent always reads: Memoire is a dated capture log; its hits are raw
records of what happened — prompts, tool calls, tool output, ids; a hit is dated
evidence, not current truth; for what was decided or learned, use Hindsight, and a
Memoire hit corroborates and dates; cite a hit's id in brackets whenever you rely
on it, verify it against the files when one carries it, and say when the hit is the
only carrier found. The three tools that return compact hits add: fetch the full
body of every hit you rely on. The intended calling pattern is two-step — search or
timeline for compact hits, then `memoire_get_observations` for the bodies you
actually want.

## The capture hooks and scripts

Capture adapters are shell scripts under `scripts/`, thin by rule: normalize the
input, write a queue file or make one HTTP call, exit. They never block a session
and they fail open. Chapter 5 gives the lane per harness; the scripts are:

- **Claude Code** — `memoire-capture-claude.sh` for live hooks (events
  `UserPromptSubmit`, `PostToolUse`, `PostToolUseFailure`);
  `memoire-capture-claude-transcripts.sh` for backfilling and streaming assistant
  turns from transcript files; `memoire-derive-auto.sh`, the checkpoint hook at
  compaction and session end, which since L0010 only captures the transcript tail
  (it posts a derive intent only if `MEMOIRE_DERIVE_NOTIFY=1`); and
  `memoire-capture-tick.sh`, the ten-minute timer body.
- **Codex** — `memoire-capture-codex.sh` and `memoire-capture-codex-rollout.sh`,
  with `memoire-capture-codex-tick.sh` as the timer body.
- **Qwen** — `memoire-capture-qwen.sh`, `memoire-capture-qwen-rollout.sh` and the
  tick.
- **opencode** — `memoire-capture-opencode.sh` and its tick, with a plugin at
  `scripts/opencode-plugin/memoire.js`.
- **Muse Code** — `memoire-capture-muse.sh` and its tick.
- **Pi** — an extension at `scripts/pi-extension/memoire.js`.
- **`acpx` sessions** — `memoire-capture-acpx.sh` and its tick.
- **Anything else** — `memoire-capture-text.sh` takes plain text, which is the
  escape hatch for a harness with no adapter. **Gemini has none**; see chapter 5.

The capture scripts and the project resolver honor a session-level kill switch,
`MEMOIRE_CAPTURE=off`, and the repository-level `capture: false` in `.memoire`.

**Injection is the reverse direction, and it is off by default.** Unless
`MEMOIRE_INJECT=on` (or `1`, `true`) is set — in the process environment or in
`capture-tick.env` under the Memoire home — `memoire-inject-claude.sh` fires at
session start and prints nothing, makes no network call and reads no token; pull
through search and the MCP tools is the retrieval path. When switched on it emits an
ordered run of conditional blocks. Seven report operational state: a dead
transport, a capture backlog, a pull-mode notice, the project-identity ask when a
repository carries no `.memoire` file, a clock alert, a review backlog, and identity
alerts. Two carry content — recent context and operating notes — and both read the
**frozen pre-Hindsight derived rows** through an explicit `include_derived=true`.
Enabling injection therefore re-surfaces stale derived memory next to Hindsight's
own; that is the reason it stays off. Every block fails open — nothing here can
block a session start, and every path exits 0 — and the network reads share one
small aggregate time budget, after which the remaining reads are skipped.

The remaining scripts are operational: `memoire-snapshot.sh` and `backup.sh`
(chapter 4), `check-health.sh` (threshold checks over `/v1/health`),
`memoire-capture-health.sh` (the client's stuck-queue alarm), `memoire-coverage.py`
(the lane validator), `memoire-resolve-project.sh` and `memoire-project-move.sh`
(identity), `token-rotate.sh`, `smoke.sh`, `seed.sh`, and the quadlet and
pg-shadow helpers. The derive sweep scripts remain in the operations image,
unscheduled.

## Environment variables

The service reads its configuration from the environment, all of it optional:

| Variable | Default | Meaning |
|---|---|---|
| `MEMOIRE_HOME` | `~/.memoire` | the data root: store, queue, raw custody, token, logs, redaction key |
| `MEMOIRE_ADDR` | `127.0.0.1:37778` | listen address |
| `MEMOIRE_BASE_URL` | `http://$MEMOIRE_ADDR` | the one knob every API consumer (hooks, MCP, injector) honors |
| `MEMOIRE_DB` | `$MEMOIRE_HOME/memoire.db` | SQLite path, or a `postgres://` DSN |
| `MEMOIRE_SCHEMA_DIR` | auto | migration directory (the PostgreSQL twin is its sibling `schema-pg`) |
| `MEMOIRE_REDACT` | `1` | defense-at-source redaction; `0` disables it |
| `MEMOIRE_FAILED_TTL_DAYS` | `30` | how long failed queue files are kept; `0` disables the reap |
| `MEMOIRE_FORWARD_URL` | unset | remote hub URL for forward mode |
| `MEMOIRE_FORWARD_TOKEN_FILE` | `$MEMOIRE_HOME/forward-token` | the hub token, kept separate from this machine's own |
| `MEMOIRE_FORWARD_PRIORITY_ADAPTERS` | `claude-code` | adapters forwarded ahead of any backlog; empty disables |
| `MEMOIRE_SKEW_ALERT_SECONDS` | `60` | clock-skew alert threshold; `0` disables |
| `MEMOIRE_CAPTURE` | on | `off` is the session-level capture kill switch |
| `MEMOIRE_TOKEN_FILE` | `$MEMOIRE_HOME/token` | the bearer the hooks and the MCP server present |

`MEMOIRE_INJECT` is not a service setting: the SessionStart hook reads it. Unset,
session-start injection is off. A few leftover settings only matter to the retired
lanes — `MEMOIRE_EMBED_URL` and `MEMOIRE_HYBRID_SEARCH` (the vector path, off unless
an embedding service is configured), `MEMOIRE_MODEL_ENDPOINTS` and
`MEMOIRE_DERIVE_DENYLIST` (the derive tool's registry and denylist),
`MEMOIRE_DERIVE_NOTIFY` (the checkpoint hook's opt-in).

Under `MEMOIRE_HOME` the service creates `queue/pending`, `queue/inflight`,
`queue/failed`, `raw/` and `log/`. The queue directories are the durable outbox of
chapter 2; `raw/` is the custody copy of chapter 3.

---

*This is the last chapter. For where the system stands right now rather than what it
is, read `MISSING.md` for the public punch-list of known gaps; for the reasoning
behind any rule in this book, the numbered decision it cites is the entry in the
project's private ledgers.*
