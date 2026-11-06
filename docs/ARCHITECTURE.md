# Architecture

> **Planning pointer (2026-07-02):** sequencing decisions for the dual Liaison + Memoire project live in liaison `ROADMAP.md` (master plan; Now gate + Reading protocol). This repo carries slice-level detail only.

> **Split note (2026-07-25, memoire:D178):** the pointer above stands unchanged. This repo now also carries its own `ROADMAP.md`, which is the memoire-local wanted-later ledger ONLY — the deportation target for `MISSING.md` items, carrying `gap:` entries with provenance and a one-line ruling. Cross-project sequencing remains liaison `ROADMAP.md`. The two roadmaps are distinguished by function, not by memory.

Memoire v0.1 is one write authority: the Go API process. Hooks and adapters either call HTTP or write event envelopes into the file queue. The service drains that queue into SQLite and projects each raw event into a search-visible `raw` observation.

The observation's `text` is a search projection, not custody: the payload is kept whole, the text is bounded (`db.BoundSearchText`). NUL is dropped and invalid UTF-8 becomes U+FFFD; an inline `data:…;base64,` body, or a delimited run of 200 or more standard-base64 characters with the entropy of encoded binary (at least 40 distinct characters per 256, upper case, lower case and digits present), becomes `[blob N chars]`; then the text is cut, on a rune boundary, at 192 KiB or at 20,000 letter/digit runs (the marker's run included), whichever comes first, and ends with ` [truncated]`. Postgres computes the FTS column from this text and caps a `tsvector` at 1 MB; the unit cap is what bounds the vector (at most two lexemes per run), the byte cap bounds the row. Paths, URLs, identifiers, hashes and prose are built from a vocabulary and never reach that entropy, and a run glued to a path or URL (preceded by `.`, `:` or `/`) is never a blob; what a user loses is text beyond about 20,000 letter/digit runs (hyphenated compounds count their parts) or 192 KiB of one event.

The HTTP API is canonical. MCP should arrive later as a thin adapter over HTTP, not as a second implementation of search or ingest behavior.

SQLite is acceptable while Memoire is local and single-writer. Before Kubernetes or direct worker writes, run the storage gate documented in `docs/KICKOFF-PREP.md`.

The container image is the deployment unit. Local dev and smoke use `podman compose`; the local always-on supervisor is a rootless Quadlet user unit. HL3 remains the Kubernetes/cdk8s target.

## Shadow-Mode Roadmap

Current Claude shadow mode captures `PostToolUse` and `UserPromptSubmit`. `UserPromptSubmit` was the required prompt-history parity slice because claude-mem stores user prompts automatically even when no durable observation is selected.

`UserPromptSubmit` identity (`scripts/memoire-capture-claude.sh`): `source_event_id` is `claude-userpromptsubmit-` plus the sha256 of the compact JSON `{session_id, hook_event_name, prompt, cwd}`, plus an `ordinal` field when the session already holds identical prompts. The ordinal is the number of lines of the payload's `transcript_path` that already carry this exact prompt as a user message (one fixed-string `grep -c`, about 10 ms on 50 MB); the hook fires before Claude Code appends the prompt, so the first occurrence has ordinal 0, omits the field and keeps the historical id (stored history never re-ingests), the k-th repeat hashes `ordinal:k`, and a re-fire at the same position reproduces the same id. The hub deduplicates on `(adapter, source_event_id)`, so an id without position would drop every repeat ("yes", "go"). Missing `transcript_path` or a missing transcript file (the first prompt of a session) gives ordinal 0 silently; an existing but unreadable transcript, a failed scan or one cut off at 2 s (the hook itself times out at 5 s) gives ordinal 0 and a line in `hooks/claude.log` (fail open). The prompt pattern goes to `grep` on stdin, never into a file. Limits: prompts that are never counted and so still collide when repeated: image or attachment prompts (content is an array), slash commands (the transcript stores `<command-name>` XML, not the typed text), `!` bash-mode input, and any prompt whose escaping differs from `jq` (U+007F, lone surrogates). Test: `scripts/tests/claude-prompt-identity-test.sh`.

Attribution at Stop (rework candidate `docs/rework/retrieval-consumer/attribution-at-stop.md`, optional `Stop` hook `scripts/memoire-attribute-claude.sh`): after a turn that made at least one `memoire_*` call, a headless fork of the session (`claude -p --resume --fork-session`, same model, no tools) is asked which served hits the reply used, and the worker writes one `memory_usage` event per operator turn (payload: served ids, per-id verdict `used | unused | misapplied | contradicted-by-files | fetch-failed` with its reason line, the ids the reply cited, the `memory-only` / `unverified` groups, the raw attribution, the fork's cost). The hook never blocks and always exits 0: each attribution runs as a transient systemd user unit whose control group holds the worker, the fork and its descendants, capped and stopped as a whole by the user manager (no user manager, no attribution); a fork that fails writes no event (an absent signal stays absent). The fork's transcript is moved out of the projects tree so the transcript ingest never reads it as a session, and the fork's own Memoire hooks (capture, inject, the `SessionEnd`/`PreCompact` checkpoint) stay silent under `MEMOIRE_ATTRIB_FORK`. The event is additive: no schema change, excluded from derivation by the event-type allowlist.

`PostToolUse` fires on a successful tool call only. A failed call fires `PostToolUseFailure`, with a top-level `error` string and no `tool_response`. The capture script types that event `claude_PostToolUseFailure` and gives it its own `tool_use_id`-based source event id. It is captured only where the harness settings register that hook; where they do not, tool failures are absent from capture, not absent from the work.

Prompt capture must follow the same custody contract as tool capture:

- producer hooks stay thin and fail-open;
- raw Claude hook JSON lands in the queue and then `raw/`;
- service-owned projection decides what becomes FTS-visible;
- prompt text should become searchable for claude-mem parity, with any future redaction/suppression policy applied centrally rather than in the hook script.

`Stop` / assistant-output capture stays backlog. It is important for full conversation reconstruction, but it widens retention and redaction policy enough that it should not block the next design slice: model-agnostic observation derivation.

## Observation Derivation

Observations are derived artifacts over raw event streams, not real-time judgments by whichever assistant is running the session. The v0.1 `raw` observation projection exists to make events searchable immediately; durable memory observations should later be produced by a replayable derivation job.

The detailed derivation contract is in `docs/DERIVATION.md`. That document is the gate before any schema or worker implementation.

The derivation job should be model-agnostic:

- input is a bounded session/project window of raw events, starting with `UserPromptSubmit` and `PostToolUse`;
- output writes additional `observations` rows with non-`raw` kinds, confidence, source event references in metadata, derivation prompt/version, provider/model label when a model is used, and enough provenance to rerun or discard results;
- capture never depends on derivation success;
- derived observations can be reviewed, superseded, soft-deleted, or rebuilt from `raw/`.

Do not make Claude's sidecar-observer pattern the Memoire architecture. claude-mem's sidecar sees individual tool calls and can miss conversation-level synthesis; Memoire should derive from the raw session stream after custody is already secure.
