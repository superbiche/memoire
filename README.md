# Memoire

Memoire is a local-first capture and custody service for people who work with
AI coding agents. It records what your agents and you actually did — prompts,
tool calls, tool output, transcripts — from several harnesses into one store you
own, keeps every byte, and lets you search that record by exact text and date
over HTTP, MCP or a shell.

It is the **source of record**, not the memory layer. Deciding what a record
*means* — what was decided, what was learned — belongs to a separate
agent-memory system, [Hindsight](https://github.com/vectorize-io/hindsight), which reads from it. Memoire answers a narrower
question: what exactly was said or run, and when. Its read surfaces serve raw,
dated records and hold no derived memory by default. (Memoire used to derive
and review observations itself; chapter 2 of the Great Book tells what became
of that.)

## Who it is for

Anyone who runs coding agents (Claude Code, Codex, Qwen, opencode and others)
and wants a durable, greppable, self-hosted record of those sessions, with no
account, no hosted tier and no telemetry. Memoire is single-operator by design.

## What works today

- **Capture**, fail-open and never blocking a session: Claude Code (hooks plus
  a transcript sweep), Codex, Qwen, opencode and Muse Code (sweeps of their own
  session files), Pi (extension), sessions driven through `acpx`, and a generic
  text adapter. Gemini has no adapter.
- **Custody**: every event is kept without expiry in a replayable store and in
  `raw/` envelope files (packed into immutable compressed parts by
  `memoire-raw-pack`). Deleting an event only hides it; `memoire-purge` is the
  deliberate, gated hard delete.
- **Redaction at the source**: secret-shaped content is removed before anything
  is written, by the shared `redact-v7` ruleset (Caviar). It fails closed.
- **Search and reads**: full-text search, timeline, context and by-id reads over
  raw records, with a date on every hit and an optional `occurred_after`
  bound. Four read-only MCP tools (`memoire_search`, `memoire_timeline`,
  `memoire_context`, `memoire_get_observations`); the three that search look back
  30 days by default.
- **Two shapes, one binary**: standalone on SQLite (one binary, one
  `MEMOIRE_HOME`, no external services) or PostgreSQL, and a hub that thin
  clients forward to (chapter 4 describes a reference Kubernetes deployment).

```
 Claude Code  Codex  Qwen  opencode  Muse  Pi  acpx  text
      └──────────┴──────┴─────┴────────┴─────┴────┴──────┘
                          │ capture (fail-open, redacted)
                          ▼
                  queue/pending/*.json
                          │ drain
                          ▼
           raw/ envelopes  +  events  +  raw projection (indexed)
                          │
          ┌───────────────┴────────────────┐
          ▼                                ▼
  dated lookup: HTTP, MCP, curl     Hindsight, audit, replay
```

## Get started

`QUICKSTART.md` takes you from a build to a captured and searched event on the
standalone SQLite shape.

```sh
make build
./scripts/memoire-bootstrap.sh
./bin/memoire-api
```

## Read more

The Great Book in `docs/great-book/` is the manual, one edition per version:

1. Identity & the custody thesis
2. The life of a captured event (including the retired derivation lane)
3. Custody & redaction
4. Operating the service
5. The seams (Hindsight, Liaison, Conclave, the redaction core, harnesses)
6. The CLI & MCP surface

`docs/API-SURFACE.json` is the versioned HTTP and MCP inventory, enforced by
contract tests. `MISSING.md` lists the known gaps of the current version.

## Status and license

Pre-release `0.x`; the version and its goal are in `VERSION`. Licensed
AGPL-3.0-only — see `LICENSE`.
