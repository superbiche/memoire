# Memoire Agent Instructions

Context marker: `memoire:AGENTS.md`

## Project

Memoire is a local-first event store for AI coding sessions: SQLite with FTS5 (PostgreSQL with ParadeDB for a
shared hub), a replayable file queue, and an HTTP read surface used by Claude Code, Codex, Gemini, Qwen, opencode
and MCP clients. Start with `README.md`, `QUICKSTART.md` and the Great Book in `docs/great-book/`.

Maintainers: when `~/dev/personal/memoire-ledger/AGENTS.md` exists, read it before any work. It carries the
maintainer workflow and the private decision ledger.

## Current state

`VERSION` carries the current version and its one-line goal; `MISSING.md` is that version's punch-list and
`ROADMAP.md` its wanted-later ledger. Do not restate current state in this file.

## Working rules

- Preserve the event-shaped model. Client-specific details belong in `payload`, not top-level schema columns.
- Keep the Go API as the only write authority to the database.
- Keep hooks thin: normalize input, write queue files or call HTTP, and exit quickly.
- Keep migrations additive. Do not drop, rename, or rewrite schema objects.
- Treat soft-delete as visibility deletion only; secret removal goes through purge and redaction.
- Prefer local-first mechanisms: files, SQLite, curl, Podman Compose.
- Repo-external references in markdown are textual, never relative links: a `../sibling` path resolves only in
  one checkout and fails the drift pre-commit gate from every worktree.
- Decision references such as `D97` or `L0010` cite the maintainers' private ledger. Treat them as citations, not
  links, and restate in the public document whatever a reader needs.

## Verification

For code changes:

- Run `make test`.
- Run `make build`.
- When endpoint behaviour changes, run the applicable smoke check against an isolated server.

Isolation: run against disposable state only. Never use the default `MEMOIRE_HOME`, a live `~/.memoire` home, or a
production database or API as a verification target. A non-empty `MEMOIRE_DB` overrides the home-derived database:
pin it to the disposable database or remove it. Go tests bind `MEMOIRE_HOME=t.TempDir()` and `MEMOIRE_SCHEMA_DIR`
through `t.Setenv` (pattern: `setupCLIEnv` in `services/api/cmd/derive/main_test.go`). For HTTP smoke checks, the
server itself must use disposable state; set `MEMOIRE_BASE` and `MEMOIRE_TOKEN_FILE` to that server.
`tests/e2e/standalone-sqlite-smoke.sh` shows a scrubbed environment and a disposable home.

For doc-only changes:

- Run `git diff --check`.

Docs bound in `drift.lock` are checked by the pre-commit hook; when it reports a stale binding, re-read the bound
code and update the doc and the binding together.
