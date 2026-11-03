# Runbook

Where the system stands and what to pick up next are not in this file:
read `VERSION` and `MISSING.md` and the re-entry point at the head of `PROGRESS.md`. Everything below is standing operations.

## Health

```bash
curl http://127.0.0.1:37778/v1/health
curl http://127.0.0.1:37778/v1/readiness
```

Operational threshold check:

```bash
./scripts/check-health.sh
```

The check exits non-zero for service-unreachable, service-not-ready, queue depth,
oldest queued age, disk pressure, WAL pressure, and optional write-stall checks.
Tune thresholds with:

```bash
MEMOIRE_MAX_QUEUE_DEPTH=20
MEMOIRE_MAX_QUEUE_AGE_SECONDS=300
MEMOIRE_MAX_DISK_PERCENT=85
MEMOIRE_MAX_WAL_BYTES=104857600
MEMOIRE_MAX_LAST_WRITE_AGE_SECONDS=0
```

`MEMOIRE_MAX_LAST_WRITE_AGE_SECONDS=0` disables write-stall detection. Enable it
only from an active-session wrapper or cron context that knows Memoire should be
receiving writes.

For the local Quadlet service:

```bash
make quadlet-health
make quadlet-rebuild
journalctl --user -u memoire.service -n 80 --no-pager
```

## Smoke

```bash
./scripts/smoke.sh
```

## Read-surface errors (search, context)

`/v1/search`, `/v1/context`, `/v1/observations`, `/v1/derivation-runs` and the
tool-event window read classify store errors at the handler:

| Response | Cause | Where to look |
|---|---|---|
| `400` with the store message | The store rejected the request itself: an empty query (`invalid search query: empty search query`), a bad cursor, or an invalid tool-window filter (`session_id is required`, `end must be >= start`). | The caller. |
| `500`, `{"error": "internal error while reading the store; ...", "request_id": "..."}` | Any other store error: a Postgres error with a SQLSTATE, a SQLite error, a context deadline, a connection failure. | The server log, by request id. |

The request id is also the `X-Request-ID` response header. Each `500` writes
one `read request failed` log line with `request_id`, `route`, `sqlstate`
(empty for non-Postgres errors), `error`, `query_len`, `project`, `kind` and
`limit`. The query text is never logged: search queries are operator content,
so reproduce from the caller's side with the request id.

The `error` field is `err.Error()` as the store or driver returned it,
truncated to 200 characters. That is safe for query text because
`SanitizeFTS` double-quotes every token, so no engine syntax error echoes the
raw query; a query text showing up in that field would be a sanitiser defect.
A client disconnect (`context canceled`) answers 499 and logs at `WARN`, not
`ERROR`, so it does not bury the SQLSTATE lines. A failure of the run-sibling
lookup after a good search is handled the same way as a search failure.

```bash
journalctl --user -u memoire.service --no-pager | grep 'read request failed'   # standalone
kubectl -n memoire logs deploy/memoire-hub | grep 'request_id=<id>'            # hub
```

A `sqlstate=XX000` (`unrecognized node type`) is a pg_search planner error,
not a query problem; it is tracked in `MISSING.md` #13. Attach the request id
when reporting an occurrence.

## BM25 search index (pg_search) — the v2 swap (operator step, a20 = A)

The Postgres read surface scores with the `pg_search` BM25 index on
`observations`. Two exact definitions are in force (a20 = A, 2026-11-01 —
the one one-time exception to "migrations additive", granted for this search
index only):

- `idx_observations_bm25` — the pre-a20 `(id, text)` index. The read filter
  (`observationWhere`: kind, project, review_state, session_id,
  derivation_run_id, occurred_at_epoch, deleted_at_epoch, noise_candidate,
  can_instruct) is not indexed by it, so pg_search re-checks it as a
  `heap_filter` on every matched row — that is the 12–15 s unscoped
  `/v1/search` and `/v1/context` on the ~3.15M-row hub.
- `idx_observations_bm25_v2` — carries those columns (`kind` / `project` /
  `review_state` / `session_id` / `derivation_run_id` as `pdb.literal`
  text, the rest as numerics), so the same query pushes every pushable
  predicate INTO the Tantivy query. The feed actor-gate (D257:
  exclusion-shaped and fail-closed, riding every shape including
  `kind='raw` — a migrated corpus can hold an `agent:*` accept row the
  current API cannot write) is the one non-pushable predicate: a NOT
  EXISTS in the inner read's WHERE demotes this exact shape to a
  full-fetch scan plus anti-join (measured 431 ms unbounded / 404 ms
  with the 30-day bound at 1M synthetic on this index, 414 / 397 with a
  50k-row review log). So the Postgres bm25 search applies it in a
  one-statement two-stage read instead: the window is a CTE (the same
  Top-K SELECT with the gate removed, M = max(4k, 100) rows by the
  full score order, grown x4 — clamped to the cap — while the top
  keeps under-filling, with a fallback to the single-query form at
  the cap), materialized once; the statement's outer applies the same
  NOT EXISTS on the ≤ M rows AND reports the window's occupancy (a
  sentinel row fed by a one-row count over the same materialized
  CTE), so the under-fill decision uses the statement's own snapshot
  — a separate count statement would take a later one (round-4
  F6). Exact by construction. The observation side of the gate is bounded
  by the window; the anti-join still reads the review log's agent-accept
  entries (index-only scan on `idx_observation_review_log_accepts`), so
  its cost grows with that population, not with the candidate set. Measured on 1M synthetic rows (this
  branch's image, paradedb 18-v0.24.3, 16 workers, the 15-column
  production projection, worst-case term, medians of 3): two-stage
  23.4 ms unbounded / 24.7 ms with the 30-day bound `/v1/context`
  sends (25.3 / 23.9 ms with the 50k-row log); the gate-free Top-K
  reference 22.0 / 22.0 ms; the same gate in the inner's WHERE
  431 / 404 ms (414 / 397 ms with the log). The inner is
  window-bounded (corpus-independent); the ≈20× factor against the
  12–15 s live-hub number is an estimate from synthetic data, not a
  guaranteed scaling — measure it at the swap.

Detection accepts both definitions, so `GET /v1/health` →
`search_engine: "bm25"` before and after the swap, and the API needs no
restart (the planner simply picks whichever index exists; a hub running
during the swap starts using v2 as soon as the old one is dropped).

The swap is an explicit operator step — `memoire-pgshadow apply` NEVER
rebuilds an existing index (a multi-million-row build is not a deployment
side-effect; it creates v2 directly on fresh databases only). The command
exists in the image built from this branch — the current hub image
(0.7.0-dev.g80bff80) cannot run it:

1. From an ops-image pod on the hub DSN, dry run first:
   `MEMOIRE_PG_URL=$MEMOIRE_DB memoire-pgshadow reindex-bm25 -dry-run`
   (prints the planned DDL and the timeouts, changes nothing).
2. The real run: `MEMOIRE_PG_URL=$MEMOIRE_DB memoire-pgshadow reindex-bm25`.
   It builds `idx_observations_bm25_v2` with `CREATE INDEX CONCURRENTLY`
   (outside a transaction, on a session with `lock_timeout=30min` and
   `statement_timeout=6h`), verifies the index is valid+ready (an invalid
   leftover is dropped and the run fails loud), then
   `DROP INDEX CONCURRENTLY idx_observations_bm25`.
3. Duration: measured 2.55 s per 1M synthetic rows on a 16-worker
   workstation, v2 ≈ 11% of the heap size (73 MB index vs 661 MB heap at
   1M). Scale to the hub by its `observations` heap size — the live
   estimate is minutes; the 6-hour statement bound is a backstop against a
   wedged build, not an expectation. The final exclusive pass of the
   concurrent build waits out writers (30-minute lock bound; a loud
   SQLSTATE 55P03 after it — just re-run). A Ctrl-C mid-build leaves an
   invalid v2, which the next run drops and rebuilds.
4. Idempotence: a repeat run with v2 valid and the old one gone is a
   no-op; a run that finds v2 valid with the old one still present (an
   interrupted first run) resumes by dropping the old only — no rebuild,
   the v2 index OID is untouched.
5. Disk: during the build BOTH indexes coexist (v2 lands beside the old
   and the old is dropped only after v2 validates), so budget roughly
   double the index size on the data volume until the drop lands; check the
   volume's free space before the run.

## Queue

Pending queue files live under `~/.memoire/queue/pending`. The service atomically renames files into `inflight`, copies them to `~/.memoire/raw/`, validates the envelope, ingests valid events, and removes the inflight file after commit. Failed files move to `failed` with a `.error.json` sidecar. `raw/` is the append-only custody copy; `failed/` is operational state only.

HTTP ingest follows the same internal path:

```bash
TOKEN="$(cat ~/.memoire/token)"
curl -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -X POST http://127.0.0.1:37778/v1/events \
  -d '{
    "schema_version": 1,
    "project": "memoire",
    "session_id": "manual",
    "source_adapter": "curl",
    "event_type": "note",
    "payload": {"message": "queued over HTTP"}
  }'
```

`POST /v1/events` returns `202` once bytes are durable in `queue/pending/`. Envelope validation and SQLite ingest happen asynchronously in the drainer. HTTP-speaking producers should delete local copies only after `202`.

Write a queue event without calling HTTP:

```bash
make build
MEMOIRE_HOME=./tmp/memoire-home \
./bin/memoire-queue \
  -project memoire \
  -source-adapter manual \
  -event-type note \
  -text "queued from a producer"
```

The command prints the pending queue file path. Hook producers should prefer this writer over hand-rolled JSON.

Generic text capture adapter:

```bash
echo "note from a hook or shell" | \
  MEMOIRE_HOME=./tmp/memoire-home \
  ./scripts/memoire-capture-text.sh \
    --project memoire \
    --source-adapter manual-text
```

This wrapper is the first hook-friendly surface: it reads text from stdin or `--text`, writes through `memoire-queue`, and does not require the API to be running.

Qwen capture wrapper:

```bash
echo "note from Qwen" | \
  MEMOIRE_HOME=./tmp/memoire-home \
  ./scripts/memoire-capture-qwen.sh \
    --project memoire
```

The Qwen wrapper only sets Qwen defaults (`source_adapter=qwen-cli`, `event_type=note`) before delegating to the generic text capture adapter.

Codex capture wrapper:

```bash
echo "note from Codex" | \
  MEMOIRE_HOME=./tmp/memoire-home \
  ./scripts/memoire-capture-codex.sh \
    --project memoire
```

The Codex wrapper only sets Codex defaults (`source_adapter=codex-cli`, `event_type=note`) before delegating to the generic text capture adapter.

Codex rollout capture (foreign-harness tool events):

```bash
# Ingest rollouts modified in the last 24h:
./scripts/memoire-capture-codex-rollout.sh --scan

# Ingest one specific session transcript:
./scripts/memoire-capture-codex-rollout.sh ~/.codex/sessions/2026/07/01/rollout-*.jsonl

# Preview without writing:
./scripts/memoire-capture-codex-rollout.sh --dry-run --scan --since "2 hours ago"
```

Codex has no PostToolUse-hook equivalent, so capture is transcript-driven: the
adapter reads the rollout JSONL files under `~/.codex/sessions/` (and
`archived_sessions/`) and queues EVERY record that carries conversation or
tool content as its own event (`codex_prompt`, `codex_assistant_message`,
`codex_tool_call`, `codex_tool_result`, `codex_item`, `codex_reasoning`,
`codex_compaction`, ...), whole, with the record's own timestamp and a
deterministic `source_event_id` (`codex:<thread>:<ordinal>`). The only
transformations: `encrypted_content` becomes a byte count, and
`event_msg/token_count`, `world_state` and `realtime_item` are not captured;
text reaches the queue byte for byte. It needs jq >= 1.7 and keeps its
working copies in a private directory under `~/.memoire/tmp/codex-capture/`
(mode 0700, removed at exit, a killed run's directory removed by the next run),
never under `/tmp`.
Each finished turn also yields one `codex_assistant_turn` event (the assistant
text, the only Codex type derivation reads); a turn still open on a file idle
for `MEMOIRE_CODEX_STUCK_S` (3600) is emitted as it stands and superseded when
it later closes. Re-running over the same rollouts is idempotent (ids are
deterministic, the hub dedups). Events go to the queue in batches through
`memoire-queue -batch-file`; a byte/line cursor per rollout under
`~/.memoire/capture-cursors/codex-rollouts/` is written after every batch, so
a kill or a time bound loses at most one batch. The cursor identity is the
Claude transcript rule (see [Capture cursors](#capture-cursors)) plus the line
start check; a rollout archived to `archived_sessions/` (or moved back) adopts
the cursor of its old path. A tick line ending in `rescans=N` counts the
rollouts whose cursor was rejected and read again from line 0. Test:
`make codex-rollout-capture-test`.

OpenCode capture (native store reader):

```
./scripts/memoire-capture-opencode.sh --dry-run --scan --since "2 hours ago"
```

OpenCode keeps every session in one live WAL SQLite database
(`~/.local/share/opencode/opencode.db`). The adapter reads it, never writes it,
and queues EVERY conversation record as its own event (`opencode_user_prompt`,
`opencode_assistant_message`, `opencode_reasoning`, `opencode_tool_call`,
`opencode_tool_result`, `opencode_patch`, `opencode_message`, ...), whole, with
the row's own time and a versioned deterministic `source_event_id`
(`opencode:part:<id>:u<time_updated>`), plus one `opencode_assistant_turn` per
finished assistant message (the assistant text, the only OpenCode type derivation
reads). One exception: a part the Memoire plugin itself injected (id prefix
`prt_memoire_`: the session-start block, the sub-agent parent context) is read
and passed by the cursor but never queued, and is left out of the turn text, so
the store does not take its own injected context back in.

Two store schemas (since OpenCode 2.0, fleet cutover 2026-10-06): the adapter
detects the schema at every run (`--schema auto|v1|v2`,
`MEMOIRE_OPENCODE_SCHEMA`). A v2 store keeps its sessions in `session_v2` and
one row per message in `session_message` (the whole message in `data`: a user
row's `text` and `files`, an assistant row's `content` items). Every row emits
`opencode_message` (`opencode:message:<id>:u<time_updated>`, the row minus its
content fields, counts in their place) plus one event per content field or item
(`:text`, `:f<i>`, `:c<i>`, `:c<i>:outcome`, `:compaction`), so nothing is
stored twice; the turn rule is unchanged; every payload carries `schema`. The
v1 tables stay in the file, frozen at the migration: a v2 run reads only the
rows of a session above that session's migration floor (`max(time_updated)` of
its rows in the frozen `message` table, one index range read with the
session-level statement; `MEMOIRE_OPENCODE_V2_FLOOR_MS` replaces it), and a
migrated session keeps its cursor and resumes after its v1 position. The row
is the unit of the spill batch cap (a row deferred past the cap emits nothing
and is read whole at the head of the next batch). A fallback to an archive
copy of the other schema is refused (one log line, exit 1); a fallback during
schema discovery restarts the discovery on the copy without retrying the failed
statement there, so no statement of one schema ever runs against the other; a
group signal during discovery or during the refresh is a clean stop (exit 0,
one log line), never a failure. In v2 the plugin
persists nothing in the store (its blocks ride the system prompt), so there is
nothing to skip.
Test: `tests/foreign/opencode-v2-capture.sh` (part of
`make opencode-capture-test`). It needs `sqlite3` >= 3.44 and `jq`, and reads the store in bounded
statements (one process per statement, a scratch file between the read and the
queue write, nothing scanned whole, rows newer than the quiet slack left for the
next run) so it never holds the harness's write-ahead log. The source is a PATH
(`MEMOIRE_OPENCODE_DB`; a `file:` URI is refused), opened `mode=ro` beside a live
writer and `immutable=1` otherwise, with `MEMOIRE_OPENCODE_DB_FALLBACK` (the
archive copy) taken at any read that fails on open, lock, or corruption. Its
working copies live in `~/.memoire/tmp/opencode-capture/` (mode 0700, a killed
run's directory removed by the next run), never under `/tmp`. One cursor file per
session under `~/.memoire/capture-cursors/opencode/` keeps a position per source
and is written after every batch. Test: `make opencode-capture-test`.

Qwen rollout capture (foreign-harness tool events):

```bash
# Ingest qwen chats modified in the last 24h:
./scripts/memoire-capture-qwen-rollout.sh --scan

# Ingest one specific session transcript:
./scripts/memoire-capture-qwen-rollout.sh ~/.qwen/projects/<cwd-slug>/chats/<sessionId>.jsonl

# Preview without writing:
./scripts/memoire-capture-qwen-rollout.sh --dry-run --scan --since "2 hours ago"
```

qwen-code has no Memoire hook equivalent, so tool-event capture is
transcript-driven: the script parses qwen JSONL files under
`~/.qwen/projects/<cwd-slug>/chats/`, joins assistant `functionCall` parts to
matching successful `tool_result` lines, and emits one `qwen_FileEdit` queue
event per successful `edit` or `write_file`. The event carries
`source_adapter=qwen-cli`, the qwen session id, the matched tool-result
timestamp, the native qwen `tool_name`, and `tool_input.file_path`, which is the
shape `GET /v1/events/tool-window` derives `paths_touched` from. Failed edits
and non-mutating tools are skipped; `--include-prompts` adds
`qwen_UserMessage` events for prompt parity. Re-running over the same
transcripts is idempotent: `source_event_id` is deterministic per
(session, call, path) and the store dedups on it. Payloads carry paths and
transcript provenance only, never `write_file.content`, `edit.old_string`, or
`edit.new_string`. Smoke: `make qwen-rollout-smoke`.

Claude Code hook capture wrapper:

```bash
jq -nc '{session_id:"manual", hook_event_name:"PostToolUse", memoire_text:"note from Claude"}' | \
  MEMOIRE_HOME=./tmp/memoire-home \
  ./scripts/memoire-capture-claude.sh \
    --project memoire
```

Prompt capture smoke:

```bash
jq -nc '{session_id:"manual", hook_event_name:"UserPromptSubmit", prompt:"note from the human operator"}' | \
  MEMOIRE_HOME=./tmp/memoire-home \
  ./scripts/memoire-capture-claude.sh \
    --project memoire
```

The Claude wrapper reads hook JSON from stdin, preserves it as the raw payload,
and writes through `memoire-queue`. It is fail-open by default for hook safety:
write failures are logged under `~/.memoire/hooks/claude.log` (with the
writer's reason, e.g. `redaction key: …` when the marker key cannot be loaded —
fail-closed, redact-v5 ruling Q8) and the hook exits zero so Claude Code keeps
working; the failure is still loud: a persistent desktop toast (critical
urgency, no expiry, `notify-send` over the session bus, skipped when either is
absent; `MEMOIRE_CLAUDE_NO_NOTIFY=1` silences it) names the log — operator
ruling a1, 2026-10-08. It intentionally keeps search-visible text
to compact hook metadata for tool events unless `MEMOIRE_TEXT` or `--text` is
set. For `UserPromptSubmit`, the hook leaves text empty by default and Memoire's
service-owned projection extracts prompt text for search. Repeated identical prompts of one session get distinct ids through a transcript-derived ordinal (scheme in `docs/ARCHITECTURE.md`, Shadow-Mode Roadmap); the smoke above has no `transcript_path`, so it uses the historical id. Raw may contain tool
output or prompt-side metadata; derived/search projection policy belongs in
Memoire, not in each producer hook.

## Attribution at Stop (memoire-attribute-claude)

Optional Claude Code `Stop` hook (`scripts/memoire-attribute-claude.sh`, worker
`scripts/memoire-attribute-claude.py`; candidate card
`docs/rework/retrieval-consumer/attribution-at-stop.md`). After a turn that made
at least one `mcp__memoire__*` call, the worker asks a headless fork of the
session (`claude -p --resume <session> --fork-session --session-id <fork id>`,
same model, no tools) which served hits the reply used, and writes one
`memory_usage` event for the turn. The main transcript never carries the
question or the answer.

Wiring: `./scripts/memoire-bootstrap.sh hooks-claude` prints the `Stop` entry
(a plain command, `"timeout": 15`). Needs `claude`, `python3`, `jq` and a user
systemd manager (`systemd-run --user`).

Lifecycle: one attribution is one transient systemd user unit,
`memoire-attrib-<random>`. The unit's control group holds the worker, the fork
and whatever the fork starts; the user manager caps its runtime
(`RuntimeMaxSec`, 200 s by default) and stops the whole group, `SIGTERM` then
`SIGKILL` after 5 s. The hook starts the unit and returns, so the turn ends at
once. Every attribution runs in the background; there is no synchronous mode
(removed by operator ruling after review round 5). The hook never follows a
unit: it makes one call to the user manager, the start, under a 5 s deadline.

| The start is... | The hook... | The turn waits | The payload |
|---|---|---|---|
| confirmed | returns | tens of ms | is the worker's; the unit removes it at its end if the worker could not |
| refused (no user manager, or the manager says no) | logs `skip: unit ... not started`, returns | tens of ms | is removed by the hook |
| not answered within 5 s | logs `start not confirmed ... not followed`, returns. A unit that did start runs supervised under its cap, as any other, and its event is an ordinary one | 5 s, 6 s if the client has to be killed | is the unit's if it did start; otherwise a later run of the hook removes it once it is an hour old |

Every event carries `mode: background`. `direct` marks the worker run by hand,
outside a unit.

- The hook's exit status is always 0. A worker that breaks, a fork that fails
  and a manager that refuses the unit are logged in `~/.memoire/hooks/attrib.log`.
- Where no user manager answers (container, replay sandbox, CI, a session with
  no `XDG_RUNTIME_DIR`), the hook does nothing and logs the skip. There is no
  unsupervised fallback.
- Worst case for the turn: about 6 s, under the printed timeout of 15.
- A stopped unit (cap, `systemctl --user stop <unit>`, logout) loses that
  attribution: the worker stops the fork, moves its transcript and journals the
  failure; no event is written. A worker killed outright leaves the fork's
  transcript registered, and the next run sweeps it.
- The unit receives named values only, none of them a secret: `PATH`, `HOME`,
  the project key, `MEMOIRE_HOME`, the unit's name, the cap, the paths of the `claude`
  and queue binaries, the model and transcript options below, and
  `CLAUDE_CONFIG_DIR` when set. Nothing else of the session's environment
  reaches it. The worker strips any Anthropic token from the fork's
  environment whatever the user manager's own environment holds.
- The Stop payload waits for the worker in `$XDG_RUNTIME_DIR/memoire-attrib/`
  (memory-backed, private to the user). The worker deletes it; the unit removes
  it at its end when the worker could not.
- To see what runs: `systemctl --user list-units 'memoire-attrib-*'`. The `ask`
  line of the journal names the unit.
- The fork is a session to every hook of the harness. The three Memoire hooks
  it would run exit at once under `MEMOIRE_ATTRIB_FORK`: capture, inject and
  the checkpoint hook `memoire-derive-auto.sh` (`SessionEnd`, `PreCompact`).
  The readiness check of 2026-09-27 found the third one unguarded (one derive
  intent posted for the fork's session id); the guard was added after it. Any
  Memoire hook added later needs the same guard. Hooks that are not Memoire's
  still run in the fork. At the first real attribution after wiring, check
  that no line of `~/.memoire/log/derive-auto.log` carries an id listed in
  `~/.memoire/hooks/attrib/fork-ids`.

| Switch | Effect |
|---|---|
| `MEMOIRE_ATTRIB=off` | no attribution in this environment (also the re-freeze lever of `llm-calls.md`) |
| `MEMOIRE_ATTRIB_MODE` | `off` disables. Any other value has no effect and is logged: there is one mode |
| `MEMOIRE_CAPTURE=off`, `capture: false` in `.memoire` | no attribution where there is no capture |
| `MEMOIRE_ATTRIB_MODEL` | fork model override; default = the session's own model, read from the transcript |
| `MEMOIRE_ATTRIB_FORK_TRANSCRIPT=delete` | delete the fork's transcript once read instead of moving it |
| `MEMOIRE_ATTRIB_TIMEOUT` | the unit's runtime cap in seconds, default 200, enforced by the user manager; an invalid value is logged and the default used |

Where to look:

- `~/.memoire/hooks/attrib/<session-id>.jsonl`: the journal, one line per
  `skip` (with its reason: `no memoire call`, `already asked this turn`), `ask`
  (calls, served ids, model, fork session id, worker pid, unit) and `record` (parse
  result, `ids_subset_served`, counts, fork cost and duration, queue exit). A
  `record` with `fork_error` and no `queue_exit` is a failed attribution: no
  event was written, and the usage view shows no signal for that turn, never
  "unused". A failed turn stays claimed: it is not retried.
- `~/.memoire/hooks/attrib/claims/`: one file per attributed turn, the atomic
  once-per-turn claim. `~/.memoire/hooks/attrib/fork-ids`: every fork session id,
  registered before launch; the transcript ingest skips a transcript named after
  one. Both grow by one entry per attribution and can be pruned by hand once
  the sessions they name are closed.
- `~/.memoire/hooks/attrib/forks/`: the fork transcripts, moved out of
  `~/.claude/projects/` on success and on failure so the transcript ingest and
  the restore-state hook never read a fork as a session. Safe to delete; kept
  for audit.
- `~/.memoire/hooks/attrib.log`: worker stderr and the entry script's skips.
- Review UI: the usage badge on each observation card and the usage section of
  the detail panel (`GET /v1/observations/{id}/usage`).

A verdict naming an id the turn was not served is kept in the event, marked
`served: false` (`ids_subset_served: false`, `unserved_ids`), and never becomes
a usage line: the model's text is untrusted, the served set is read from the
harness's own tool results.

The fork is a headless Claude call under `~/.agents/shared/llm-calls.md`: bounded
(one per memory-consulting operator turn), never with an ambient Anthropic token
(the worker strips `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` from the fork's
environment). One-time billing verification of this consumer: after the first
live fork, check on the plan dashboard that the usage landed on the plan, and
record the check here. Status: first real fork run on 2026-09-27 at 15:58 Europe/Paris
(readiness check, hook fed by hand, model `claude-opus-5-5`). Verification recorded
on 2026-09-27, by account configuration and not by a dashboard reading: the
operator states that extra usage is disabled on the account, so a call lands on
the plan or does not go through; the fork carries no Anthropic token, so the
API billing path is closed as well. The three calls of the readiness check went
through. The same statement covers the replay forks of 2026-09-26 and
2026-09-27. Consequence for the live week: at the plan's limit a fork fails,
no event is written, and `attrib.log` carries the failure. The re-freeze
trigger of `llm-calls.md` stays in force.

Smoke: `make claude-attribute-smoke` (fake `claude`, fake transcripts, isolated
API; failing fork, concurrent stops, worker stopped in mid-fork, unserved id,
ingest skip, stale sweep, mode label).

## Streaming the transcript: checkpoint hook + capture tick

Live hooks capture tool events and prompts as they happen, but the assistant's
own text lives only in the Claude Code transcript file. It reaches Memoire at
three moments (operator ruling 2026-09-08, Fibery #342 — before it, session end
was the only one, and every derivation window cut mid-session held tool events
without the text explaining them):

1. **PreCompact** — `scripts/memoire-derive-auto.sh`, the session checkpoint
   hook. Compaction is a checkpoint, not an ending.
2. **SessionEnd** — the same script, unchanged in every other respect.
3. **The quiet tick** — `scripts/memoire-capture-tick.sh`, every 10 minutes.

The hook ENTRIES are liaison custody: `~/.agents/memoire.json` is the registry
and `liaison memoire link` projects it into `~/.claude/settings.json`. Never
hand-edit the projected file. A `PreCompact` entry with no matcher fires on both
manual and auto compaction. The hook must always exit 0 — exit 2 would block
compaction.

Checkpoint log, one line per fire (`~/.memoire/log/derive-auto.log`):

```
2026-09-08T12:00:00+02:00 event=PreCompact cause=manual notify project=memoire session=<sid> capture=rc=0 drain=ok notify_code=200
```

`event=` is the hook that fired (absent in the payload → `SessionEnd`), `cause=`
the `trigger` (PreCompact: manual|auto) or `reason` (SessionEnd).

Inside an attribution fork (`MEMOIRE_ATTRIB_FORK` set) the hook exits at once:
no capture, no intent, no log line.

Install and drive the tick timer (user units, no credential):

```bash
scripts/capture-tick-timer.sh install     # *:0/10, Persistent, RandomizedDelaySec=60
scripts/capture-tick-timer.sh status
scripts/capture-tick-timer.sh run-once
scripts/capture-tick-timer.sh stop        # or uninstall
```

Tick log (`~/.memoire/log/capture-tick.log`), one summary line per tick plus one
line per notified session:

```
2026-09-08T12:10:07+02:00 tick files_new=4 turns=11 sessions=3 notify=off rc=0
```

`rc=` is the adapter's own exit code. `rc=1` together with `files_new=0` is the
ordinary empty scan — the adapter's long-standing "no transcript files given"
exit, seen when every transcript inside the `--since` window is younger than the
quiet gate. It is not a failure to chase.

Knobs (`~/.memoire/capture-tick.env`, read by the unit as an optional
`EnvironmentFile`): `MEMOIRE_CC_PROJECTS_DIR` (default `~/.claude/projects`),
`MEMOIRE_TICK_QUIET_S` (120 — skip transcripts touched more recently),
`MEMOIRE_TICK_SINCE_S` (172800 — 48h scan bound), `MEMOIRE_TICK_TIMEOUT` (600s
on the adapter), `MEMOIRE_TICK_NOTIFY` (0). Notify is off by default on purpose:
the hourly sweep already enumerates outstanding work from uncovered events, so
the tick changes what a derivation window CONTAINS, not how often one runs.
Kill switches: `MEMOIRE_CAPTURE=off|0|false` disables the tick entirely,
`MEMOIRE_AUTO_DERIVE=0` disables only its notify.

Codex capture tick (separate from the Claude tick, off by default): the Codex
rollout adapter runs from its own script, `scripts/memoire-capture-codex-tick.sh`,
with its own lock, its own time bound and its own systemd timer and unit
(`scripts/capture-codex-tick-timer.sh install|start|stop|status|run-once|uninstall`,
every ten minutes at :05, unit `memoire-capture-codex-tick`). The Claude tick
above does not know it, so a slow or hung Codex run never delays the Claude
capture. It reads the same optional `~/.memoire/capture-tick.env`; nothing runs
until `MEMOIRE_CAPTURE_CODEX=on|1|true` is set there (unset, `off`, `0` or
`false` keep it off silently; another value logs one
`skipped=codex_switch_unrecognised`). Knobs: `MEMOIRE_CODEX_SESSIONS_DIR`
(default `~/.codex/sessions`), `MEMOIRE_CODEX_ARCHIVED_SESSIONS_DIR` (default
`~/.codex/archived_sessions`), `MEMOIRE_TICK_SINCE_S` and `MEMOIRE_TICK_QUIET_S`
as for the Claude tick, `MEMOIRE_CODEX_TICK_TIMEOUT` (240s) and
`MEMOIRE_CODEX_TICK_KILL_S` (10s between TERM and KILL); `install` prints and writes
`TimeoutStartSec` as their sum plus 60s (310s by default) from the environment or the
environment file at that moment, so reinstall after changing either. It needs jq >= 1.7 and
logs one line per tick to `~/.memoire/log/capture-codex-tick.log`, exit 0 always:

```
2026-09-29T12:10:07+02:00 tick_codex files=3 events=1288 failed_files=0 stopped=1 lines_left=52310 rc=0 stop="rollout-2026-09-01T10-00-00-....jsonl: bound reached at line 84689, 52310 lines left"
```

`stopped=1` means the adapter reached its time bound at a batch boundary, wrote
its cursors and will resume at the same file (oldest change first) on the next
tick; a large backlog drains over several ticks. `rc=124` with `timed_out=1`
means the bound expired and TERM ended the adapter, `rc=137` that it had to be
killed; a `fail="..."` part carries the adapter's first failure lines.

Qwen Code capture tick (separate from the other ticks, off by default):
`scripts/capture-qwen-tick-timer.sh install|start|stop|status|run-once|uninstall`
installs `memoire-capture-qwen-tick`, every ten minutes at :09 with a randomized
60s delay and persistent scheduling. It runs `scripts/memoire-capture-qwen-tick.sh`
and the existing Qwen rollout adapter, including user prompts and visible assistant
turns. The service reads the same optional `~/.memoire/capture-tick.env` on each
invocation; the tick never sources it. The operator enables it with
`MEMOIRE_CAPTURE_QWEN=on|1|true`. Unset, `off`, `0` or `false` log
`tick_qwen skipped=qwen_switch_off`; other values log
`skipped=qwen_switch_unrecognised`. `MEMOIRE_CAPTURE=off|0|false` disables it.

It has its own lock and successful per-file metadata state under
`~/.memoire/state/qwen-tick/`. Files under `~/.qwen/projects/*/chats/*.jsonl`
are selected by `MEMOIRE_TICK_SINCE_S` (172800s) and `MEMOIRE_TICK_QUIET_S`
(120s). `MEMOIRE_QWEN_SESSIONS_DIR` overrides the root. Unchanged files are
skipped; changed files replay through the accepted reader's deterministic event
ids. Failed, timed-out or concurrently changing files remain eligible. This is
full-file replay, not Codex byte-tail capture: the reader joins tool results
across the session. The independent process group receives TERM then KILL at
`MEMOIRE_QWEN_TICK_TIMEOUT` (240s) plus `MEMOIRE_QWEN_TICK_KILL_S` (10s).
The installer computes `TimeoutStartSec` as that sum plus 60s from invocation
environment only; reinstall after changing it. Its temporary worktree ExecStart
must be reinstalled from the canonical checkout after reviewed release landing.

One numeric line per tick goes to `~/.memoire/log/capture-qwen-tick.log`:
`tick_qwen files=N events=N failed_files=N unchanged=N stopped=0|1 timed_out=0|1 rc=N`.
No transcript, queue envelope or arbitrary adapter diagnostic is logged. Exit
status is always zero for a completed tick, including skips; inspect its counts.
The tick needs Python 3 and the rollout reader's existing jq/queue dependencies.

Acpx capture tick (separate from the other ticks, off by default):
`scripts/capture-acpx-tick-timer.sh install|start|stop|status|run-once|uninstall`
installs `memoire-capture-acpx-tick`, every ten minutes at :03 with a randomized
60s delay and persistent scheduling. It runs `scripts/memoire-capture-acpx-tick.sh`,
which scans the acpx worker sessions and queues their turns through the acpx
journal adapter (`scripts/memoire-capture-acpx.sh`). The service reads the same
optional `~/.memoire/capture-tick.env` on each invocation; the tick never sources
it. The operator enables it with `MEMOIRE_CAPTURE_ACPX=on|1|true`. Unset, `off`,
`0` or `false` log `tick_acpx skipped=acpx_switch_off`; other values log
`skipped=acpx_switch_unrecognised`. `MEMOIRE_CAPTURE=off|0|false` disables it.

It has its own lock under `~/.memoire/capture-acpx-tick.lock` and relies on the
adapter's own cursor (`~/.memoire/cursors/acpx-capture.json`) — the tick writes no
per-file markers and no state dir of its own. It scans `MEMOIRE_ACPX_SESSIONS_DIR`
(default `~/.acpx/sessions`, else `ACPX_SESSIONS_DIR`) within
`MEMOIRE_TICK_SINCE_S` (172800s) and `MEMOIRE_ACPX_TICK_QUIET_S` (3600s, zero
allowed). `MEMOIRE_ACPX_ARCHIVE_DIR` selects a synced archive sessions dir (unset
defaults to `~/Sync/archives/acpx-history/<short hostname>/sessions`); it is passed
to the adapter only when it is an existing directory, and reported as
`archive=0|1`. The independent process group receives TERM then KILL at
`MEMOIRE_ACPX_TICK_TIMEOUT` (240s) plus `MEMOIRE_ACPX_TICK_KILL_S` (10s).
The installer computes `TimeoutStartSec` as that sum plus 60s (310s by default)
from invocation environment only; each bound falls back to its default
independently; reinstall after changing it. Its temporary worktree ExecStart
must be reinstalled from the canonical checkout after reviewed release landing.

One numeric line per tick goes to `~/.memoire/log/capture-acpx-tick.log`:
`tick_acpx records=N skipped=N failed_records=N failed_events=N summary=0|1 archive=0|1 timed_out=0|1 rc=N`.
`summary=1` means exactly one summary line came from the adapter; `0` means none
or several (the four counts are then 0). No transcript, queue envelope or arbitrary
adapter diagnostic is logged. Exit status is always zero for a completed tick,
including skips. The capture mode joins the adapter's cursor: a `--full` run after
a boundaries-only run over unchanged files re-captures the intermediate messages,
and a full capture covers later full and boundaries runs (a mode change re-captures;
full covers boundaries).

OpenCode capture tick (separate from the Claude and Codex ticks, off by default):
the OpenCode adapter runs from its own script,
`scripts/memoire-capture-opencode-tick.sh`, with its own lock, its own time bound
and its own systemd timer and unit
(`scripts/capture-opencode-tick-timer.sh install|start|stop|status|run-once|uninstall`,
every ten minutes at :07, unit `memoire-capture-opencode-tick`). It reads the same
optional `~/.memoire/capture-tick.env`; nothing runs until
`MEMOIRE_CAPTURE_OPENCODE=on|1|true` is set there (another unrecognised value logs
one `skipped=opencode_switch_unrecognised`). Knobs: `MEMOIRE_OPENCODE_DB`,
`MEMOIRE_OPENCODE_DB_FALLBACK` and `MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR` (the
adapter's own parameters), `MEMOIRE_TICK_SINCE_S` and `MEMOIRE_TICK_QUIET_S` as
for the other ticks, `MEMOIRE_OPENCODE_TICK_TIMEOUT` (240s) and
`MEMOIRE_OPENCODE_TICK_KILL_S` (10s); `install` writes `TimeoutStartSec` as their
sum plus 60s. One line per tick to `~/.memoire/log/capture-opencode-tick.log`, exit
0 always:

```
2026-09-30T12:17:04+02:00 tick_opencode sessions=4 events=812 failed_sessions=0 rejected=0 stopped=0 left=0 rc=0
```

`stopped=1` means the adapter reached its time bound (or received a TERM) at a
batch boundary, wrote its cursors and resumes at the same session on the next tick;
`no_database=1` means neither the source nor its fallback exists (a machine without
OpenCode); `fail="..."` carries the adapter's first failure lines.

Muse capture tick (separate from the other ticks, off by default):
`scripts/capture-muse-tick-timer.sh install|start|stop|status|run-once|uninstall`
installs `memoire-capture-muse-tick`, every ten minutes at :01 with a randomized
60s delay and persistent scheduling. It runs `scripts/memoire-capture-muse-tick.sh`,
which calls the Muse Code store-sweep adapter (`scripts/memoire-capture-muse.sh`)
once over `MEMOIRE_MUSE_SESSIONS_DIR` (default `~/.local/share/muse/sessions`). The
adapter reads every root `session.jsonl` and its `subagent/<id>/session.jsonl`
children, decodes each file with `muse export` (`MEMOIRE_MUSE_BIN`, else `muse` on
PATH, the mise shim, then `~/.local/bin/muse`) and queues every logged record as one
event, plus one `muse_assistant_turn` per completed run and one `muse_session` facts
event per change. A child bound in its root's `subagent.control.child_session_bound`
record carries its parent, subagent id and role; an unbound child (Muse's reminder
sessions) is marked `muse_internal`. It never writes to the Muse store, skips a tree
written in the last `MEMOIRE_MUSE_TICK_QUIET_S` (120s, zero allowed) and checks the
acpx exclusion file (`~/.config/memoire/acpx-exclude`, fail-closed) against the
session workspace before any export. Its cursor is `~/.memoire/cursors/muse-capture.json`
(per-stream sequence marks, queued turns, facts digest).

The service reads the same optional `~/.memoire/capture-tick.env`; nothing runs until
`MEMOIRE_CAPTURE_MUSE=on|1|true` is set there (unset, `off`, `0` or `false` log
`tick_muse skipped=muse_switch_off`; other values `skipped=muse_switch_unrecognised`;
`MEMOIRE_CAPTURE=off|0|false` disables it). Own lock `~/.memoire/capture-muse-tick.lock`;
window `MEMOIRE_TICK_SINCE_S` (172800s); TERM then KILL to the process group at
`MEMOIRE_MUSE_TICK_TIMEOUT` (240s) plus `MEMOIRE_MUSE_TICK_KILL_S` (10s); the installer
writes `TimeoutStartSec` as their sum plus 60s (310s by default). One numeric line per
tick to `~/.memoire/log/capture-muse-tick.log`, exit 0 always:
`tick_muse units=N events=N failed=N rejected=N excluded=N deferred=N skipped=N capture_off=N no_workspace=N summary=0|1 timed_out=0|1 rc=N`.

### Capture triggers: a turn reaches the queue within seconds

The timers above stay as the recovery leg and the history importer. On top of
them, three opt-in triggers run the accepted reader on the ONE session that just
ended a turn, so its content is queued within seconds instead of at the next
tick. A trigger carries no content: it starts the same reader the tick runs, so
the event ids are the tick's and the hub collapses any overlap; the cursor lock
(`skipped=locked`) keeps a trigger and a tick off the same file at once, and the
loser's bytes wait for the next trigger or tick (latency, never loss).

| leg | registered as | script | inline work |
|---|---|---|---|
| Qwen Code | timer only (`memoire-capture-qwen-tick`, :09); no per-turn hook | `scripts/memoire-capture-qwen-tick.sh` | none; gated periodic rollout capture described above |
| Acpx | timer only (`memoire-capture-acpx-tick`, :03); no per-turn hook | `scripts/memoire-capture-acpx-tick.sh` | none; gated periodic capture of acpx worker sessions described above |
| Claude Code | `Stop` (second entry next to the attribution hook), timeout 5, plain (liaison has no `async` passthrough) | `scripts/memoire-trigger-capture-claude.sh` | polls at most 1.5 s for the turn's final text in the transcript (the transcript lags Stop), starts the reader in a session of its own, waits at most 1 s for it |
| Codex | `Stop`, timeout 3; needs the operator's approval in the Codex TUI, and again after any later change to the entry | `scripts/memoire-trigger-capture-codex.sh` | detaches at once (under 100 ms); the detached run polls up to 5 s for the turn's closing record, then runs the reader either way |
| OpenCode | the plugin's `event` hook on `session.idle` (spawned detached, never awaited) | `scripts/memoire-trigger-capture-opencode.sh` | none; live database only (`source=archive skipped` otherwise), 5 s debounce (a delay, no idle dropped), a trailing run for an idle that arrived during a run (`trailing=1`), then at most one more cycle if the marker is raised again after the lock is released (`relock=1`); a marker still raised after that is logged `pending-left=1` and waits for the next idle or the tick |

Switches, read per invocation from `~/.memoire/capture-tick.env` (single key,
last assignment wins, `on|1|true`, never sourced): `MEMOIRE_HOOK_CAPTURE=on`
enables the three trigger legs and is OFF by default; the Codex and OpenCode legs
also need their existing `MEMOIRE_CAPTURE_CODEX` / `MEMOIRE_CAPTURE_OPENCODE`
gates; `MEMOIRE_CAPTURE=off` kills everything, whether it is written in
that file (`off|0|false`, last assignment wins; logged `status=disabled`) or set in
the process environment (`status=capture-off`). The ticks, including Qwen Code, do not read
`MEMOIRE_HOOK_CAPTURE`; Qwen Code has no per-turn trigger leg. The detached part re-reads the switches itself. A Claude
attribution fork (`MEMOIRE_ATTRIB_FORK`) and a re-entered Stop
(`stop_hook_active`) are skipped. Every trigger exits 0 whatever happens and logs
one line per stage (`hook`, `run`) to `~/.memoire/log/trigger-<claude|codex|
opencode>.log`: session id, sizes, counts, timings and the outcome, never content:

```
2026-09-30T21:02:11+02:00 memoire-trigger-claude: stage=hook status=spawned session=fe24a598-... polls=1 poll_end=signal poll_ms=312 bytes=214179 run=done elapsed_ms=580
2026-09-30T21:02:11+02:00 memoire-trigger-claude: stage=run status=captured session=fe24a598-... bytes=214179 turns=1 rc=0 elapsed_ms=240
```

Statuses: `captured`, `empty` (nothing new), `skipped=locked`, `skipped` with
`source=archive` (OpenCode), `disabled`, `capture-off`, `pending-marked`
(OpenCode: a run was in progress), `error=...`. `poll_end` is `signal` (the
final text was found), `stable` (the file stopped growing) or `bound`; Codex
logs `closing=present|stable-absent|timeout`. Knobs (validated, junk falls back):
`MEMOIRE_TRIGGER_POLL_MS` (Claude: at most 1500), `MEMOIRE_TRIGGER_STEP_MS` (at
least 100), `MEMOIRE_TRIGGER_WAIT_MS` (Claude: at most 1000; values outside are
clamped, so no environment can push the Claude hook past 3 s),
`MEMOIRE_TRIGGER_READER_TIMEOUT_S`, `MEMOIRE_OPENCODE_TRIGGER_DEBOUNCE_S` (5),
`MEMOIRE_OPENCODE_TRIGGER_SETTLE_S` (3), `MEMOIRE_OPENCODE_TRIGGER_MAX_SECONDS`
(30). The OpenCode log line carries `source=` as the reader reports it
(`live`, or `archive` when the reader fell back by its own rule). The OpenCode
wrapper removes the state files `hooks/oc-<sid>.{lock,last,pending}` of sessions idle
for more than 7 days at its start (50 files at most per run); the trigger logs
(`log/trigger-*.log`) are not rotated by the triggers: the log is the operator's to
rotate, as the tick logs are. Tests: `scripts/tests/capture-trigger-test.sh` (scripts) and
`scripts/tests/capture-trigger-plugin-test.sh` (plugin).

### Capture cursors

All three moments run `scripts/memoire-capture-claude-transcripts.sh --tail`,
which keeps one byte cursor per transcript under
`~/.memoire/capture-cursors/cc-transcripts/<sha256 of the transcript path>.json`
(dir 0700, files 0600):

```json
{"path": "/home/.../<sid>.jsonl", "file_id": "f4a8f8459ac3ba93:1234",
 "head_len": 4096, "head_sha": "<sha256>", "tail_sha": "<sha256>",
 "dev_inode": "37:1234",
 "offset": 918273,
 "session_homes": {"<sid>": "<memoire-checkout>"},
 "updated_at": "..."}
```

`file_id` is the statfs `f_fsid` (`stat -f -c %i`) and the inode: on btrfs
`st_dev` is renumbered at every mount, so `dev_inode` cannot survive a reboot,
while `f_fsid` derives from the filesystem UUID and subvolume. `head_sha` hashes
the first `head_len` = min(4096, `offset`) bytes, `tail_sha` the (up to) 4096
bytes ending at `offset`. An idle transcript with the same `file_id` costs a
stat; any other case must still match both, so a transcript replaced by an
appended copy (a Syncthing receiver renames every update over the old inode)
resumes at its offset, while one whose content changed inside either window (a sync
conflict winner) is read again. A change confined between the two windows, or
a same-size rewrite of an idle transcript, is not detected: the writers only
append. A cursor from before
`file_id` is accepted when its inode still matches and is rewritten. `dev_inode`
is still written so a rollback to the older reader does not re-send.

Only `[offset, last complete newline)` is read, so a transcript being appended
to WHILE it is captured never yields a half line. `session_homes` maps each
session id to the first cwd seen for it, which keeps a tail chunk attributing
to that session's launch project — one entry per session, because a transcript
resumed in place carries a second session id that must attribute to its own
project. A cursorless file, a cursor that does not describe the file (logged
as "cursor does not match the file, full pass"), or a size below `offset`
means a full pass from 0 (and the recorded homes step aside, since such a pass
sees every session's own first line).

To reset one transcript, delete its cursor file — the next pass re-reads the
whole transcript, which is safe and wasteful: the hub collapses the re-sent
events on `idempotency_key` (memoire:D153), but keeps one raw file per POST. To reset everything, remove
the `cc-transcripts` directory. Every cursor problem (unwritable dir, missing
`sha256sum`, a contended lock) degrades to a full pass plus one stderr line; the
adapter's exit codes are unchanged, and running it without `--tail` still does
exactly what the retro backfill always did.

The first tick after install bootstraps every cursorless transcript inside the
48h window and can hit `MEMOIRE_TICK_TIMEOUT`; cursors are written per file, so
the next tick resumes where it stopped.

### Sub-agent text

The reader also captures the assistant `text` of Claude Code sub-agents
(since 2026-10-02): `<projects>/<project-dir>/<sessionId>/subagents/agent-<id>.jsonl`,
`.../subagents/workflows/wf_<id>/agent-<id>.jsonl`, and sidechain lines written
inline in a main transcript. Thinking, tool calls, user lines and attachment
lines (injected hook context) are never queued. Sub-agent events keep the
parent's session id and the `claude_assistant_turn` type; their payload adds
`sidechain: true` and `agent_id`, plus `agent_type`, `parent_tool_use_id` and
`spawn_depth` when the sibling `agent-<id>.meta.json` is readable (a missing or
malformed meta file never blocks capture). The project is the parent session's
home, read from the parent transcript (its cursor's `session_homes`, else the
first `cwd` of a non-sidechain line carrying the sub-agent's session id); only a
sub-agent whose parent transcript is absent falls back to its own `cwd`, for
that run only. A sidechain line (sub-agent file or inline) never sets or changes
a session's home: only main-session lines and the parent lookup do, so the
cursor never records a sub-agent's cwd as a home. Compaction agents (`agent-acompact-*`) are queued as
`claude_compact_summary` (source_event_id `cc-compact:<sessionId>:<uuid>`):
kept as evidence, outside derivation's input set, and not counted by
`--emit-sessions`. Each sub-agent file has its own cursor, lock, `--quiet-for`
and `--since` window. Attribution-fork sessions are skipped with their
`<forkId>/subagents/` tree. Sub-agent files written before this change have no
cursor and fall outside the tick's 48h window: read them once with the command
in "Long turns and repair".

### Long turns and repair

A Claude assistant turn of at most 6000 characters is one event. A longer turn
(since 2026-10-02; before that the text was cut at 6000 characters and flagged
`text_truncated: true`) is a deterministic pair:

- the first event is exactly what the reader always emitted: id
  `cc-turn:<sessionId>:<uuid>`, the first 6000 characters, `text_truncated: true`;
- ONE continuation event carries all the remaining text: same event type,
  adapter, session, cwd and `occurred_at`; id `cc-turn:<sessionId>:<uuid>:rest`
  (`cc-compact:<sessionId>:<uuid>:rest` for a compaction summary); payload of
  the first event plus `continues_line_uuid` and `text_offset`, with
  `text_truncated: false`. Sub-agent turns follow the same rule.

First + continuation is the source text. The first event keeps its historical
shape (the first 6000 characters, trailing newlines stripped), so when newlines
end that slice the continuation starts where the first event's text actually
ends: the stripped newlines open it, and `text_offset` is that character offset
(below 6000 in that case). Cases where the equality cannot hold: a NUL character
inside the first 6000 characters is dropped from the first event, as the reader
always did; a turn of at most 6000 characters loses its trailing newlines, as
always (no continuation exists to carry them); a turn whose first 6000 characters
are ALL newlines (never ingested by the old reader) has its leading newlines
dropped, and `text_offset` still counts them.

The split point is fixed at 6000 characters. `MEMOIRE_BACKFILL_TEXT_MAX` is no
longer read by this reader (the Qwen reader still uses it): a changed cap would
move the boundary and break the convergence with events already on the hub. The
continuation text is passed to `memoire-queue -text-file` (a file in a temp
directory of the run, removed after the write), so a 300 KB turn never travels
in argv. `--emit-sessions` counts turns, not events; `--dry-run` prints one
`DRY` line per event and marks a continuation (`continuation id=... text_offset=...`),
and writes nothing at all (no queue call, no cursor, no cursor directory, no lock
file, no temp directory).

The hub keeps the first write of a `source_event_id` and collapses a re-sent
one, so a turn ingested truncated cannot be repaired by re-sending its own id.
The continuation event has its own id: reading the file again produces the same
first event (collapsed) and a continuation that the hub does not have yet. The
outcome is identical whether the turn was first read by the old reader or by
this one.

Repair-and-catch-up, once per workstation, from a checkout of this branch (or
the installed script path):

```
scripts/memoire-capture-claude-transcripts.sh --from-start --since 2592000 --quiet-for 120 ~/.claude/projects
```

Repeat the command until it exits 0: exit 2 means some queue writes failed and
those files keep no cursor, and the tick does not revisit a file older than its
48-hour window.

- `--from-start` reads every given file from offset 0 whatever its cursor says
  (rewritten files, sub-agent files that never had a cursor, main transcripts at
  EOF) and leaves the cursor at the end of the last complete line; the flock and
  every other rule are those of `--tail`. `--since 2592000` bounds a directory
  scan to files modified in the last 30 days; `--quiet-for 120` skips files still
  being written.
- On the hub: each turn already stored is collapsed (first write wins, nothing
  changes), a turn that was stored truncated gains its `:rest` continuation, a
  sub-agent turn or compaction summary not stored yet is created.
- Cost: one `jq` pass per file, about 25 ms of reader time per event plus the
  queue write, and one extra event per long turn (285 of 29,950 turns on one
  workstation over 30 days). A manual run has no `MEMOIRE_TICK_TIMEOUT`: run it
  from a terminal, not through the timer.
- Running it twice queues the same ids again and nothing else; the hub
  collapses all of them. The second run costs the same reader time and queue
  writes as the first (it re-reads everything, the cursor does not shortcut
  `--from-start`), and yields zero new rows. Use plain `--tail` for the routine
  catch-up: it is a stat-only no-op on a file at its cursor.
- A continuation repaired later gets a higher hub `id` than events of the same
  `occurred_at` second that already exist; derivation orders by
  `occurred_at_epoch, id`, so it can follow other events of that second. A
  continuation repaired later also lands in windows that were already derived
  (coverage reports them as outstanding work).

A sub-agent file first read while its parent transcript is absent attributes to
its own cwd for that run and records no home in its cursor. At a later pass with
the parent transcript present, the parent's home wins. A cursor an earlier build
left holding the sub-agent's cwd as home is rewritten with the parent's home at
the next pass, even when the file has gained no bytes.

### Failed writes, skipped lines, inferred timestamps

A file's cursor never moves past text whose queue write failed. A line that
cannot be parsed, even after the surrogate repair below, is skipped, counted and
logged; the cursor passes it.

- A failed queue write (first event, continuation, a failed text-file write or
  an unusable temp directory, a payload that cannot be built) is not counted as
  queued, and the file's cursor is left at, or put back to, the offset where that
  pass started reading the file, and the hub collapses what was already sent
  when the file is read again. A `--from-start` or non-tail pass starts at 0, so
  its cursor is removed and the next pass reads the whole file. Who reads it
  again depends on the file: the timer tick only scans files modified within
  its `--since` window (48 hours), so a file older than that is retried by
  nobody. On exit 2 of a manual `--from-start` or non-tail run, re-run the same
  command until it exits 0: a re-run only re-sends ids the hub already holds.
  The log line says `N queue writes FAILED (cursor left at the start of this pass...)`,
  the final `done:` line repeats the total, and the exit code is 2. A permanently
  failing event therefore keeps its file at the same offset on every pass until
  it is fixed. `--emit-sessions` prints the turns actually queued in that pass
  (a turn whose first event failed is not reported); the replay reports the
  already queued turns again, which derive tolerates.
- A line that is not a JSON object (truncated, NUL byte, text) is skipped alone;
  every valid line after it in the range is still read. The log line reports
  `N unparseable lines skipped`. Before a line is given up, it is retried once
  with each lone UTF-16 surrogate escape (`\ud800`-`\udfff` not part of a valid
  pair) replaced by `\ufffd`; valid pairs are untouched. A line that parses that
  way is captured and counted as `N lines repaired (lone surrogate)`.
- An assistant line with text but no `cwd` is captured with an empty cwd and the
  project of the session home. One with no `timestamp`, or a `timestamp` that is
  not an RFC 3339 string the queue writer accepts, takes the timestamp of the
  nearest previous line of the range that has one, else the file's modification
  time, and its payload carries `occurred_at_inferred: true` (the log line counts
  `timestamps inferred`). A line with text but no `uuid` or `sessionId` cannot be
  given an id: it is skipped and counted (`lines without uuid or sessionId
  skipped`).

## Cutting the window: rhythm cuts (Fibery #343)

The hourly sweep no longer derives a session as one time slice. Each session is
first cut at its own **rhythm** — a long pause, or N new substantive events —
and every segment is derived as its own run with its own input manifest, so
memoire:D211 coverage is unchanged. Session end is not a special case: the
trailing segment just ends at the end of the window. The byte ceiling stays the
hard constraint, and a segment still over it falls back to the existing
median-epoch halving *inside that segment* — rhythm cuts are the first
splitter, halving the fallback, never the reverse. Cross-cut context needs no
wiring: memoire:D212's "Already Captured (this session)" prompt section is
scoped by `(project, session_id)` with no window predicate, so the previous
cut's observations are already in the next cut's prompt.

Two cut classes ship: a long pause and an event-count cap. Topic-shift
detection is out (it needs a model or an embedding). A **compact checkpoint is
not a class**, because nothing in the store marks a compaction: the PreCompact
hook records the cause in its log line only, and the transcript adapter
captures `type == "assistant"` lines exclusively, so the JSONL
`compact_boundary` marker never becomes an event.

Inspect the cuts for a session without deriving anything:

```bash
bin/memoire-derive -cut-points -project memoire -session-id <sid>
```

One epoch per line, ordered; empty output means the window needs no cut and
derives whole. Segments are `[lo, c1]`, `[c1+1, c2]`, … `[ck+1, hi]`, with the
outer edges left unpinned by the sweep so the last segment still picks up
events that land between the read and the execute.

Sweep log markers:

```
2026-09-10T14:00:07+02:00 sweep project=memoire session=<sid> rc=0 {"rhythm":true,"segments":3,"cuts":[1757500000,1757510000]}
2026-09-10T14:00:31+02:00 sweep project=memoire session=<sid> window=0..1757500000 rc=0 {...}
```

`cuts=failed` means the detector could not run (an unreachable store, say); the
sweep falls back to the whole-session window for that session and says so
rather than silently reverting the lane to time slicing.
`session_leg_budget_exhausted` means one session hit its per-tick leg ceiling
(`2^MEMOIRE_SWEEP_MAX_SPLIT_DEPTH`, 64 by default) — the same worst case the
lane had before rhythm cuts existed, now enforced per session rather than per
leg, since cuts multiply legs.

Knobs — all optional, and unset means the `memoire-derive` defaults govern so
the numbers live in exactly one place (`services/api/cmd/derive/cutpoints.go`):

| Env | Flag | Default | Meaning |
|---|---|---|---|
| `MEMOIRE_SWEEP_RHYTHM_CUTS` | — | `1` | `0` disables the pass; the lane returns to pure time slicing |
| `MEMOIRE_SWEEP_CUT_GAP_SECONDS` | `-cut-gap-seconds` | 1800 | long-pause threshold; `0` disables the class |
| `MEMOIRE_SWEEP_CUT_MAX_EVENTS` | `-cut-max-events` | 120 | canonical events since the last cut; `0` disables the class |
| `MEMOIRE_SWEEP_CUT_MAX_SEGMENTS` | `-cut-max-segments` | 0 (unbounded) | segments per pass; a positive value caps the pass and merges the overflow into the last segment, never dropped. The sweep's per-session leg budget governs cost, so the default is unbounded (Fibery #351, first live tick) |

Both defaults are calibration values in the memoire:D72 sense, not measured
ones: 1800 s sits three times above the streaming tick's 10-minute cadence so a
pause can never be a capture artifact, and 120 events is deliberately a
backstop rather than the primary cutter in the first window. Revisit both once
real rhythm-cut legs exist.

A cut never leaves a fragment the min-content guard would skip. That is not
politeness: a guard skip writes no `derivation_runs` row, hence no manifest,
hence no coverage — and the same pause would re-cut the fragment identically on
every later tick, orphaning it permanently with its notify intent open
(memoire:D245). So the detector only cuts where the segment it closes would be
derived, and folds a thin tail back into its predecessor.

The guard floor and the byte ceiling can still trap a segment BETWEEN them,
which is what Fibery #365 fixed. A byte-heavy, content-light segment — a
subagent tool burst, hundreds of `claude_PostToolUse` events with no captured
content — passes the floor as a whole but exceeds the ceiling, so the sweep
halves it on the median epoch; one half holds the content, the other holds
only tool events, fails the floor, and is skipped as `degenerate_window` with
no run row. Nothing is covered, the next tick re-cuts the same segment, and
the real content in it is never derived. The `derive-observations-v2.5`
renderer closes it at the source: a run of content-less events costs one
summary block instead of ~336 bytes of envelope each, so the segment fits
under the ceiling whole and never reaches the halving. The per-run signal is
`collapsed_event_count` (with `collapsed_block_count`) in
`derivation_runs.meta_json` and in the dry-run's `prompt_meta` — a large
count on a small prompt is the fix working.

## The open tail: supersede and the provisional marker (Fibery #344)

The trailing segment ends at the last event **so far**, so every tick derives a
running session mid-arc and gets "so far" observations. Two mechanisms handle
what that costs.

**Supersede.** When a later cut restates one of those observations with more
information, the write-time near-dup gate no longer only rejects it: the
restatement lands as a new `proposed` row carrying `supersedes_id` and the row
it replaces moves to `superseded`, in one transaction. Conditions, thresholds
and their consequences are in `docs/DERIVATION.md` (near-dup gate section);
what an operator sees is `superseded=N` on the sweep's execute line,
`superseded_count` in the execute JSON, and per-supersede custody in run
`meta_json.superseded_candidates` — target id plus the trigram, containment and
growth scores. Those three thresholds are judgment values, not measured ones;
the run metadata exists so a week of real sweeps can re-derive them. A
review-log row under actor `system:derive-supersede` and an
`observation.supersede` audit entry make every transition attributable.

**Provisional.** The open-tail leg is marked `provisional: true` in the run
metadata and in each observation's `meta_json`. It changes nothing else — no
column, no retrieval behavior, not an idempotency input. It is the measurement
hook for the question this ruling opens: what gets superseded fast was
scaffolding.

| Env | Flag | Default | Meaning |
|---|---|---|---|
| `MEMOIRE_SWEEP_SUPERSEDE` | `-no-supersede` (inverted) | `1` (on) | `0` passes `-no-supersede` on every execute leg, so a restatement is rejected as a near-duplicate again. A kill switch, not a tuning knob — the thresholds live in `services/api/internal/modelderive/neardup.go`. It restores the gate's OUTCOME, not the whole of #344: superseded rows stay out of the per-session derivation read-back either way, so a session holding rewrite-retired rows compares against a slightly smaller reference set than before this slice |
| — | `-provisional` | off | the sweep sets it on the OPEN-TAIL leg only: the trailing rhythm segment, the whole-session window when there is no cut (fail-open paths included), and the right half when that tail is halved. Inner segments never carry it |

Find what a run superseded:

```bash
sqlite3 "$MEMOIRE_DB" "SELECT id, supersedes_id, review_state FROM observations WHERE derivation_run_id = '<run>' AND supersedes_id IS NOT NULL;"
```

On the hub (Postgres) read the same through the API: `GET
/v1/observations?...&include_proposed=true` now carries `supersedes_id` on each
record. Note the retired row itself 404s on the by-id read — superseded is a
hidden state on every read surface — so the pointer is a provenance fact today,
not a traversal (chain reads at retrieval are Fibery #335).

## Session-start injection (off by default)

`scripts/memoire-inject-claude.sh` is the one SessionStart injector (Claude
Code and Codex directly, Qwen through `memoire-inject-envelope.sh`, OpenCode
through its plugin). It is **off by default**: with `MEMOIRE_INJECT` unset it
prints nothing (no health header, clock alert, review backlog, recent context
or operating notes), makes no network call, reads no token and exits 0. The
SessionStart entries stay registered; the hook fires and exits silently.
Retrieval is pull, through the search and MCP tools.

`MEMOIRE_INJECT=on` (also `1` or `true`) turns it back on, with every other
`MEMOIRE_INJECT_*` knob unchanged. It is read from the process environment, or
from `$MEMOIRE_HOME/capture-tick.env` (last assignment wins; the file wins over
the environment when it carries the key). Attribution at Stop is unaffected:
it works from the turn's own Memoire tool calls, not from injected context.

With injection off, a stuck or dead forwarding queue on a client is no longer
announced in sessions (no CAPTURE BACKLOG or TRANSPORT DEAD block). The capture
health timer announces it instead, outside any session (see "Capture health
alert"): a desktop notification and an optional sink. On demand,
`scripts/memoire-capture-health.sh --status` prints the verdict, and
`memoire-forward -status` prints the local queue depths and the last forward
status as JSON; the forwarder persists the same last-success / last-error /
consecutive-failure state in `$MEMOIRE_HOME/forward-status.json`. The hub's
`/metrics` and `check-health.sh` see only the hub's own queue.

## Token

The local token is generated at `~/.memoire/token` with mode `600`. On service start, Memoire stores only the token hash and prefix in SQLite `credentials`, then authenticates requests against that credential row. Rotation is currently manual and requires a service restart so the new token is bootstrapped into `credentials`:

```bash
./scripts/token-rotate.sh
```

List credentials:

```bash
TOKEN="$(cat ~/.memoire/token)"
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:37778/v1/credentials
```

Revoke a non-current credential:

```bash
curl -H "Authorization: Bearer $TOKEN" -X POST http://127.0.0.1:37778/v1/credentials/<credential-id>/revoke
```

## Review Queue

Model-derived observations start as `review_state=proposed`. The review queue
lets an operator triage them: accept durable ones, reject noise, or mark
superseded.

All review-mutation endpoints require the admin token. List endpoints accept a
read token.

List proposed observations (read token):

```bash
TOKEN="$(cat ~/.memoire/token)"
curl -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:37778/v1/observations?review_state=proposed&project=memoire"
```

Each observation in the list carries its triage provenance — `kind`,
`confidence`, `derivation_run_id`, and `source_event_ids` — so a reviewer can
trace a proposal back to the raw events it was derived from without a second
request.

Filter by derivation run:

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:37778/v1/observations?review_state=proposed&project=memoire&derivation_run_id=drv_<id>"
```

Accept an observation — promotes it to trusted memory (admin token required):

```bash
curl -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -X POST "http://127.0.0.1:37778/v1/observations/<id>/accept" \
  -d '{"reason_text":"verified correct"}'
```

Reject an observation — removes it from the trusted set without losing the
record (admin token required):

```bash
curl -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -X POST "http://127.0.0.1:37778/v1/observations/<id>/reject" \
  -d '{"reason_text":"not durable"}'
```

`/accept` and `/reject` are the ergonomic verdict endpoints for the proposed
queue (`reason_text` is optional). Both return `{"observation": {...}}` with the
updated record and are idempotency-safe: a second accept/reject on an
already-decided observation returns `400` (illegal transition), never a silent
re-apply. The generic `POST .../review` with a `to_state` body remains for the
non-verdict transitions (`superseded`, `soft_deleted`):

```bash
curl -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -X POST "http://127.0.0.1:37778/v1/observations/<id>/review" \
  -d '{"to_state":"superseded","reason_text":"a newer observation wins"}'
```

Allowed transitions and their preconditions (enforced identically whether
reached via the dedicated verdict routes or `/review`; table current as of
memoire:D146, slice E):

| `to_state`    | Allowed from            | Notes                                                     |
|---------------|-------------------------|-----------------------------------------------------------|
| `accepted`    | `proposed`, `disputed`, `lapsed` | Plain accept: "seen, not contested" — provenance UNTOUCHED. |
| `rejected`    | `proposed`, `disputed`, `lapsed` | Hides from default reads; row + logs preserved.    |
| `lapsed`      | `proposed`              | Verdict, per-session bulk (`/v1/sessions/{id}/lapse-remaining`), or 21d TTL (`/v1/admin/lapse-expired`, actor `system:lapse-ttl`). Searchable, `~`-marked at injection, never feed-eligible, out of queue counters. |
| `disputed`    | `accepted`              | Human challenge of trusted memory.                        |
| `superseded`  | `accepted`, `disputed`  | Use when a newer observation wins.                        |
| `soft_deleted`| `accepted`              | Hides without hard delete.                                |

Dedicated verdict routes beyond accept/reject (memoire:D146):

- `POST /v1/observations/{id}/confirm` — accept **plus** provenance
  promotion to `user_confirmed`. The only promotion gesture; elevation
  requires it first (elevate no longer promotes provenance).
- `POST /v1/observations/{id}/lapse` — park unreviewed (see table).
- `POST /v1/observations/{id}/rewrite` `{"text":...,"reason_text":...}` —
  NOT an edit: creates a NEW accepted `user_confirmed` observation carrying
  the operator's text, superseding the original (valid from
  proposed/lapsed/disputed; filiation via `supersedes_id`).

`raw` kind observations are locked at `accepted` and cannot be transitioned.

Inspect the audit trail for an observation (admin token required):

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:37778/v1/observations/<id>/review-log"
```

Returns `{"review_log":[...], "total": N}` with `from_state`, `to_state`,
`actor`, `reason_text`, and `occurred_at_epoch` per entry.

### Confirm/accept ratio (gesture-degradation metric, memoire:D194)

`a` (accept: "seen, usable") and `c` (confirm: "I vouch") must keep distinct
meanings (memoire:D191/D192). If `c` becomes the new `a`, `user_confirmed`
provenance inflates and the strong verdict loses the value the R-5 split gave
it. The ratio is two counts over the existing review audit trail — no new
infrastructure. On the hub:

The metric SPLITS BY ACTOR CLASS (triage lane v1, R-D). A ratio that mixes
human and machine verdicts measures nothing: the whole question is whether
the OPERATOR's confirm gesture is degrading, and a machine that ever gained
verdict authority would silently move the number. The actor lives on
`observation_review_log`, so the audit row is joined to its review-log twin
— written in the same transaction, at the same `occurred_at_epoch`, for the
same observation:

```bash
kubectl -n memoire exec "$(kubectl -n memoire get cluster memoire-pg -o jsonpath='{.status.currentPrimary}')" -c postgres -- psql -U postgres -d memoire -At -c "
SELECT
  CASE
    WHEN rl.actor LIKE 'agent:%'  THEN 'agent'
    WHEN rl.actor LIKE 'system:%' THEN 'system'
    ELSE 'human'
  END AS actor_class,
  count(*) FILTER (WHERE a.details_json LIKE '%\"verdict\":\"confirm\"%')   AS confirms,
  count(*) FILTER (WHERE a.details_json NOT LIKE '%\"verdict\":\"confirm\"%'
                     AND a.details_json LIKE '%\"to_state\":\"accepted\"%') AS accepts
FROM audit_log a
JOIN observation_review_log rl
  ON rl.observation_id = CAST(a.resource_id AS BIGINT)
 AND rl.occurred_at_epoch = a.created_at_epoch
WHERE a.action = 'observation.review'
GROUP BY 1 ORDER BY 1;"
```

(Standalone SQLite form: same query via `sqlite3 ~/.memoire/memoire.db`,
with `CAST(a.resource_id AS INTEGER)` — `count(*) FILTER` and the `LIKE`
prefixes work on both engines; `details_json` is compact Go JSON, so the
quoted literals match byte-for-byte.)

The same split is on the unauthenticated scrape surface as
`memoire_review_transitions{to_state,actor_class}` — closed compiled-in
labels only, never the actor string (memoire:D224).

Reading discipline (memoire:D194): a periodic read at review-cadence
discussions — not a timer, no alerting. Healthy shape: confirms are a
deliberate minority of positive verdicts IN THE HUMAN ROW; a ratio drifting
toward 1 means the confirm gesture has degraded into reflex. A non-zero
`agent` row is not a health signal at all — it means a machine verdict
landed, which no shipped path can produce, and it is read as an incident.
Baseline at metric birth (2026-07-30, hub): 0 confirms / 13 accepts.

### Triage override rate (triage lane v1, R-D)

The triage lane's own degradation signal: how often the operator FLIPS the
judge rather than applying it, split by verdict class and prompt version
(the idempotency key carries the version, so a prompt change is visible as
a new row rather than as drift inside one). Outcome is computed from source
— the suggestion, and the first HUMAN review-log transition that followed
it — never stored beside the suggestion (memoire:D245). `system:*` is
excluded as carefully as `agent:*`: a re-armed review TTL lapsing a
judge-flagged row is the timer agreeing with the judge, not the operator,
and counting it would let the mechanism memoire:D0253 paused manufacture
this very number:

```bash
kubectl -n memoire exec "$(kubectl -n memoire get cluster memoire-pg -o jsonpath='{.status.currentPrimary}')" -c postgres -- psql -U postgres -d memoire -At -c "
WITH landed AS (
  SELECT ts.prompt_version, ts.suggested_verdict, (
    SELECT rl.to_state FROM observation_review_log rl
    WHERE rl.observation_id = ts.observation_id
      AND rl.occurred_at_epoch >= ts.created_at_epoch
      AND rl.actor NOT LIKE 'agent:%'
      AND rl.actor NOT LIKE 'system:%'
    ORDER BY rl.occurred_at_epoch ASC, rl.id ASC LIMIT 1
  ) AS to_state
  FROM observation_triage_suggestions ts
)
SELECT prompt_version, suggested_verdict,
  count(*)                                      AS suggested,
  count(*) FILTER (WHERE to_state IS NULL)      AS pending,
  count(*) FILTER (WHERE to_state = CASE suggested_verdict
                     WHEN 'accept' THEN 'accepted' WHEN 'confirm' THEN 'accepted'
                     WHEN 'lapse'  THEN 'lapsed'   WHEN 'noise-lapse' THEN 'lapsed'
                     WHEN 'reject' THEN 'rejected' END)                       AS applied,
  count(*) FILTER (WHERE to_state IS NOT NULL AND to_state <> CASE suggested_verdict
                     WHEN 'accept' THEN 'accepted' WHEN 'confirm' THEN 'accepted'
                     WHEN 'lapse'  THEN 'lapsed'   WHEN 'noise-lapse' THEN 'lapsed'
                     WHEN 'reject' THEN 'rejected' END)                       AS flipped
FROM landed GROUP BY 1, 2 ORDER BY 1, 2;"
```

`bin/memoire-triage -override-rates` prints the same table from either
engine without a psql session.

Reading discipline: a rising flip rate on one verdict class is a prompt
problem, not an operator problem — the judge is confidently wrong in a
shape the prompt version names. A flip rate near zero on `reject` is the
one that matters most, because reject 404s the row off the read surface.

## Redaction (defense-at-source)

Secret-shaped content is redacted at the queue-writer boundary — before any
pending file, `raw/` custody copy, or SQLite row exists (GOVERNANCE-DESIGN.md
ruling 7). Both write paths are covered: `memoire-queue` (all capture
adapters, client-side in hooks) and `POST /v1/events` / `/v1/events/batch`
(API backstop for direct HTTP producers).

Rules-only (`redact-v7`, the shared fleet contract
`~/.agents/shared/contracts/redact-v7.md`; no LLM on the write path).
Memoire pins `redact: 7` in `.agents/contracts.json`. The implementation is
the central Go core Caviar (`github.com/superbiche/caviar/redact` v0.3.0, the
public module; rulings a18, a81/a82); `services/api/internal/redact` only
forwards to it. Caviar owns the port, its parity tests, the hash-pinned
corpus and the Python oracle. Builds fetch the module like every other public
dependency (`proxy.golang.org`, checked by `go.sum` and `sum.golang.org`), so
no registry credential, file proxy or `GONOSUMDB` entry is needed anywhere;
the committed file proxy of ruling a33 is retired. A rule change is a Caviar
release plus an exact `go.mod` bump
(`records/history/L0008-redact-v7-public-caviar.md`, superseding the build
route of `records/history/L0005-redact-v6-consumes-caviar.md`).
History of the stamp: `redact-v1` (D132, 2026-07-02), `redact-v2`
(2026-10-07, `json-secret-field`, `url-credentials`, marker ownership),
`redact-v5` (2026-10-08, operator rulings Q1–Q8; v3 and v4 were superseded
before any consumer adopted them; the hub was rolled back from it on
2026-10-09 for over-redaction), `redact-v6` (2026-10-10, rulings a18/a28),
`redact-v7` (2026-10-12, rulings a81/a82). When the filter runs, every valid envelope
with object (or no) metadata is stamped `redaction_rules_version` with the
ruleset of the LAST Memoire writer that filtered it, hit or no hit: a pending
file redacted on a capture host and forwarded is re-stamped by the hub's own
filter (its hit list is kept and merged), so the stamp is the latest
attestation, not the original capture's. Reading it on older stock: a
`redact-v6`, `redact-v5`, `redact-v2` or `redact-v1` stamp means "last
filtered before `redact-v7` was adopted" — a cautious starting selection, not a complete inventory: the two v2
rules ran under the `redact-v1` label for part of 2026-10-07
(`capture-live-20261007c` to `capture-live-20261007d`); events written before
the universal stamp carry NO version unless a rule hit them; and a capture
made with `MEMOIRE_REDACT=0`, or carrying non-object metadata, is unstamped at
any date. The v1/v2 rule sets had a confirmed leak class (`std-21`: a nested
YAML key under `secrets:`, the assignment rule's `\s*` crossing the newline
and wrapping the KEY while the value leaked); the derived-store re-sweep with
`memoire-redact-derived` under the current ruleset is the response for
stored stock (on the hub: "Re-sweep on the Postgres hub" below; the v6 run
is deferred by operator ruling, 2026-10-10). No version-selective replay
exists yet.

Rules (28, contract order is law: shapes first, then query parameters and
headers, then JSON fields and objects, URL credentials, CLI flags, mysql `-p`,
env assignments, bare bearer phrases): PEM private-key blocks (procedure P3:
BEGIN located, END or the first blank line after the header block, bounded by
the next BEGIN), JWTs, AWS access keys (`AKIA`/`ASIA`), OpenAI keys, GitHub
classic + fine-grained PATs, GitLab PATs, liaison letterbox tokens
(`lbx1_…`), Slack tokens, Google API keys, Stripe keys, Hugging Face, npm,
PyPI and Tailscale tokens, age secret keys, Grafana service-account tokens,
signed-URL / OAuth query parameters (`X-Amz-Signature`, `sig`, `code`…),
`Authorization: Bearer` values, credential headers (`Cookie`, `X-Api-Key`,
`Private-Token`…, the rest of the line), bare values under secret-named JSON
fields (`json-secret-field` — as escaped text in strings at escape depth up
to 6 and in structured payloads; value only, key stays readable; a number of
8+ characters under such a key is redacted too), schema defaults and
examples and every other direct string child of a secret-named object
(`json-schema-default`, `json-secret-object`; JSON Schema vocabulary and
identifier keys such as `type`, `username`, `url` stay readable), userinfo
credentials in URLs (`url-credentials` — password only, authority only,
scheme bounded to 32 characters), `--password`/`--token`-style CLI flags,
mysql `-p<value>` on the command line, env-style secret assignments
(`*SECRET|TOKEN|PASSWORD|PASSWD|API_KEY|APIKEY|PRIVATE_KEY|ACCESS_KEY|CREDENTIALS?`
keys, case-insensitive, `=` or `:` separators on the SAME line, quoted values
including spaces, `export`/`const`/`set`… prefixes; since v6 a bare
configuration or environment read — `os.environ[...]`, `os.getenv(...)`,
`settings.X`, `process.env.X`, `config.get(...)` — is a reference, not a
value, and stays readable after any key, and a bare code expression stays
readable after a code-cased key, while `.env`/shell values and quoted or YAML
literals stay redacted; the contract's §What changed from v5 owns the exact
tests), and bare `bearer`/`token`
phrases followed by a 20+-character word. The secret-key family for JSON
keys is `api[_-]?key|secret|token|password|passwd|access[_-]?key|private[_-]?key|credentials?|authorization|auth|cookie`
as the whole key or its suffix. Floor: 8 characters for assignment, header,
CLI and JSON-field values (ruling Q3); none for URL passwords and shape
tokens. Secret-shaped JSON map KEYS are redacted too, and NON-SECRET numeric
values survive bit-exact (UseNumber round-trip; a number of 8+ characters
under a secret-named key is replaced by a marker string, hashed over the
reference module's `json.dumps` text so markers correlate across
implementations). Marker ownership (P0): a span already carrying a
`[REDACTED:…]` marker is never re-wrapped by a later CAPTURING rule, hit
counts describe matched spans; the whole-match shape rules never skip, so a
PEM block that contains marker text is still redacted whole. Nesting deeper than 512
containers (or 512 enclosing secret-named objects in text) fails CLOSED: the
write is refused, never written unredacted.
Reference/placeholder shapes are exempt and survive readable when they are
the COMPLETE value: `op://` handles, `$VAR`/`${VAR}`/`${{…}}`/`$(…)`/`{{…}}`
templates, `{env:…}` references, `<placeholders>`, `%VAR%`, the words
`required`/`optional`/`undefined`/`redacted`/`[filtered]`…, runs of three or
more `*`, and changeme/placeholder/example/dummy/your- prefixes. Known
collisions, by design: ordinary text shaped like a credential (`token` plus
any 20+-character word, `code=` query values) is redacted — information loss
without leak.

A match becomes `[REDACTED:<rule>:<hash8>]` — hash8 is an HMAC-SHA256 prefix
keyed by a machine-local key (`$MEMOIRE_HOME/redaction-key`, auto-created,
0600), so occurrences of the same secret correlate across events while an
offline reader cannot dictionary-attack weak passwords from markers. Losing
the key only breaks cross-event correlation, never data. A matched value is
replaced by its marker; a marked value is never re-wrapped by a later
capturing rule (a PEM block is still taken whole), but a second secret inside
the same assignment is not separately redacted.
Operator ruling 2026-10-07 (Fibery #616 decision 6): that narrow coverage
reduction is ACCEPTED — an opaque, shapeless secret that cohabits with an
already-marked one inside a capturing rule's span (an env value, a URL
password) stays readable so the URL host/path and the JSON structure around
the marker survive; redacting the remaining segments would need entropy
guessing, which the rules-only contract forbids. Secrets with a recognised
shape or under a secret-named key remain covered wherever they sit.
The envelope's metadata records `redactions` (rule + count) and
`redaction_rules_version`.

Escape hatch: `MEMOIRE_REDACT=0` disables the filter (explicit operator
choice). Smoke: `make redaction-smoke`.

Limits: redaction protects new writes only — events ingested before this
landed may still hold secrets (hard purge: `memoire-purge`, below).
Detection is shape-based; a secret with no recognizable shape passes.

### Re-sweep on the Postgres hub (memoire-redact-derived)

`memoire-redact-derived` re-applies the current ruleset (`RulesVersion`) in
place to every derived text column (events, observations, derivation runs,
sessions, review log, triage suggestions, usage reasons, audit log) — the
response for stock stored under an older ruleset. It selects the Postgres
engine exactly like `memoire-purge`: `--db postgres://…`, or no `--db` and a
Postgres DSN in `MEMOIRE_DB` (from secret `memoire-pg-app`, key `uri` — never
typed or printed); a non-DSN `MEMOIRE_DB` is ignored. The report names the
store with the DSN password redacted.

Same behaviour as on SQLite: dry-run by default, the
`MEMOIRE_REDACT_DERIVED_APPLY=yes` double gate, the identity-path preflight
(a hit in `project_roots.root` or `project_identity_alerts.root/known_root`
refuses apply — re-key procedure first), fail-closed on a document nested
past the depth limit, `content_hash` recomputed for rewritten observations,
idempotent (a second pass changes nothing). Postgres differences:

- The process check cannot see other pods: apply refuses without
  `--i-have-quiesced-the-hub`, the operator's attestation that step 2 below
  was done. The tool never scales anything and applies no DDL.
- Apply is ONE `REPEATABLE READ` transaction over the preflight, every scan
  and every update: all or nothing. A dry-run reads one `READ ONLY` snapshot.
- Search follows by itself: `observations.text_tsv` is a generated column.
- The vector lane is invalidated, not rewritten: the embedding of every
  observation whose text changes is deleted in the same transaction (its
  vector and `text_hash` were computed from the pre-redaction text); the
  embed sweep re-embeds the redacted text on its next run (report:
  `tables.observations.embeddings_invalidated`, also counted by the
  dry-run).
- The redaction key is the hub's own (`/data/redaction-key` on the custody
  PVC), so markers correlate with the hub's write path. The tool refuses to
  run if the key file is absent; it never mints one.
- Physical retention: rewritten values follow the same age-out as a purge
  (heap until autovacuum, PITR backups until their retention passes). Rotation of any leaked credential remains mandatory.

Procedure (`kubectl -n memoire`; an ops image that contains this
`memoire-redact-derived` build — rebuild and repin `OPS_IMAGE` first):

```sh
# 0. Operator pod: the "Hard purge on the Postgres hub" pod spec, named
#    memoire-redact-op (ops image, custody PVC at /data, MEMOIRE_DB from the
#    secret). Check the key exists and is at least 16 bytes, without
#    reading it (the loader would regenerate a short or unreadable key,
#    breaking marker correlation; a fresh hub key is never authorized):
kubectl -n memoire exec memoire-redact-op -- sh -c 'test -r /data/redaction-key && [ "$(wc -c < /data/redaction-key)" -ge 16 ] && echo key-ok'
#    No "key-ok": stop and investigate the key's custody before going on.

# 1. Dry-run (hub may stay up): read-only counts per table and rule.
kubectl -n memoire exec memoire-redact-op -- memoire-redact-derived \
  --redaction-key /data/redaction-key \
  --report /data/redact-derived-dryrun-$(date -u +%Y%m%dT%H%M%SZ).json

# (L0010: the derive and embed sweeps are retired; of this list only
#  memoire-snapshot remains to suspend.)
# 2. Quiesce: the purge procedure's step 2 (suspend memoire-derive-sweep,
#    memoire-embed-sweep and memoire-snapshot, wait for active Jobs, scale
#    deploy/memoire-hub to 0).

# 3. Apply.
kubectl -n memoire exec memoire-redact-op -- env MEMOIRE_REDACT_DERIVED_APPLY=yes \
  memoire-redact-derived --redaction-key /data/redaction-key \
  --apply --i-have-quiesced-the-hub \
  --report /data/redact-derived-apply-$(date -u +%Y%m%dT%H%M%SZ).json

# 4. Verify: a second dry-run reports total_changed 0 and no preflight hits.
kubectl -n memoire exec memoire-redact-op -- memoire-redact-derived \
  --redaction-key /data/redaction-key

# 5. Restore (purge procedure step 5), then delete the operator pod. The
#    embed sweep's next run re-embeds the invalidated observations; check
#    the vector-lane freshness look-line afterwards.
```

Known caveat (D145, operator-ratified): `derivation_runs.input_hash` is not
recomputed; an operator re-derive of a pre-redaction window can mint new
runs and proposed duplicate observations.

## Hard purge (memoire-purge)

`bin/memoire-purge` is the application-level destruction path (liaison:D97
decision 4, memoire:D147 slice 1): hard DELETE from SQLite (soft-delete
visibility is ignored), matching `raw/` + queue files quarantined into an
age-encrypted archive then removed, WAL truncated and `VACUUM` run so the
freed pages leave the file, custody generation bumped
(`~/.memoire/custody/MANIFEST.json`, see `docs/MIRROR-PROTOCOL.md`).

Both engines cascade identically (#616 slice E, decision 4): a purged
observation takes its triage suggestions (judge reasoning), its usage rows
(the consuming agent's reason lines) and, where the store has an embeddings
table (`observation_embeddings_pg` on Postgres; a SQLite
`observation_embeddings` table is cascaded the same way if one exists — the
0002 sketch is not applied today, so the report says 0), its embedding with
it, and a purged event takes the usage rows it produced — on SQLite as on
Postgres, since no derived table has an `ON DELETE CASCADE` on SQLite. The
project selector also probes and deletes the project's usage rows on both
engines. The SQLite quiesce guard
(no live `memoire-api` / `memoire-derive`) has no environment bypass: hermetic
tests substitute the guard in-process, operators stop the services.

Exactly one selector per run:

```sh
# Dry-run (default): counts only, zero writes
bin/memoire-purge --project observer-sessions

# Apply — double-gated, quiesce first (stop memoire.service; no memoire-derive)
MEMOIRE_PURGE_APPLY=yes bin/memoire-purge --project observer-sessions \
  --apply --age-recipient age1... --report tmp/purge-report.json

# Targeted: events (+ their raw-projection and source-linked observations)
MEMOIRE_PURGE_APPLY=yes bin/memoire-purge --event-ids 123,456 --apply --age-recipient age1...

# DB-only: derived observations (source events and raw files survive)
MEMOIRE_PURGE_APPLY=yes bin/memoire-purge --observation-ids 789 --apply --no-quarantine
```

Semantics worth knowing before pointing it at anything:

- The quarantine archive is the undo/custody handle (same posture as the
  D144 sweep): destroy it + its key to complete destruction, keep it to
  keep a recovery path. `--no-quarantine` skips straight to destruction.
- Project purges also drop the project's `sessions` and `derivation_runs`
  rows; id selectors leave both (idempotency watermarks).
- An observation with one purged source among several is purged
  (conservative over-delete — its text derives from purged content).
- `audit_log` is never purged; every apply that mutates rows writes a
  `purge.apply` tombstone (counts + selector, no content). No-op and
  convergent re-applies stay silent — no tombstone, no generation bump.
- All four file surfaces are scanned: `raw/`, `queue/pending`,
  `queue/inflight` (crashed-drainer residue would be re-ingested at next
  API start otherwise), `queue/failed` (+ sidecars). Envelope matching
  uses the drainer's own normalization, so files that omitted
  `source_event_id` on disk (direct HTTP producers) still match.
- Crash safety: any apply that dies mid-flight leaves
  `~/.memoire/purge-intent.json` and the next run refuses.
  `--resume-intent` (which requires the full `--apply` +
  `MEMOIRE_PURGE_APPLY=yes` gate — it deletes files) reconciles the
  intent's phase against the DB: a pre-commit intent whose rows survived
  means the transaction rolled back — nothing is deleted, the intent is
  cleared, re-run the apply. A committed intent finishes the file half.
  File deletion is all-or-error: a failed removal keeps the intent and
  the run fails before the generation bump.
- Packed raw custody (see "Raw cold archive"): matching walks flat files
  and part members alike. A matched member is quarantined as
  `raw-pack/<part>/<name>` and removed by rewriting its part without it
  (verified, atomic rename; a part left empty is removed). The report adds
  `raw_packs_rewritten`, and `raw_files_*` count members. The intent
  journal lists packed matches under `files.raw_packed`. Deletion also
  finds every journaled name wherever it lives now, so a file moved
  between a crash and the resume is still destroyed. Apply and resume
  first discard stale part temps and drainer staging copies (they can
  hold the purged bytes). Run and resume take the raw/ custody lock and
  refuse while the packer or a snapshot holds it.
- A corrupt `custody/MANIFEST.json` stops any destructive run (a reset
  generation would break mirror stale-artifact detection) — repair it
  first; a missing manifest is a fresh store and initializes normally.
- Physical media: VACUUM rewrites the DB file but SSD wear-leveling keeps
  old blocks around. This is application-level destruction.

### Hard purge on the Postgres hub

On the hub the store is the CNPG cluster `memoire-pg`; `memoire-purge`
selects the Postgres engine when `--db` is a `postgres://`/`postgresql://`
DSN, or when `--db` is absent and `MEMOIRE_DB` holds such a DSN (ops pods
inject it from secret `memoire-pg-app`, key `uri` — never type or print
it). A non-DSN `MEMOIRE_DB` is ignored: the SQLite default stays
`<home>/memoire.db`. Files still resolve from `--home` (`/data` on the
custody PVC `memoire-home-lh`): `raw/`, `queue/*`, `custody/MANIFEST.json`,
`purge-intent.json`, the quarantine archive.

Same selectors, double gate, intent journal, tombstone, generation bump and
quarantine as on SQLite. Postgres differences:

- The process check cannot see other pods. Apply and `--resume-intent`
  refuse without `--i-have-quiesced-the-hub`, the operator's attestation
  that the steps below were done. The tool never scales anything.
- The cascade also deletes `observation_usage`,
  `observation_triage_suggestions` and `observation_embeddings_pg` rows of
  purged observations/events (report: `usage_rows_deleted`,
  `triage_suggestions_deleted`, `embeddings_deleted`). FTS is the
  generated `text_tsv` column, gone with the row.
- No `VACUUM`: the report says `vacuumed:false` with
  `vacuum_skipped_reason`. Dead tuples stay in the heap until autovacuum,
  and in the PITR object store (`s3://memoire-pitr/`, 30-day retention)
  until the base backups and WAL that predate the purge age out. (The
  logical DR cluster `memoire-pg-dr` was decommissioned on 2026-10-28,
  operator ruling a21; there is no second cluster to check.) Never run `VACUUM FULL` without
  a separate decision (it takes an ACCESS EXCLUSIVE lock).
- Physical retention after a purge is AGE-OUT, not pruning (operator ruling
  2026-10-07, Fibery #616 decision 5): the purged bytes remain recoverable
  from PITR until `purge date + retention`
  — read the live retention from the cluster spec (`kubectl -n memoire get
  cluster memoire-pg -o jsonpath='{.spec.backup.retentionPolicy}'`, 30d at
  the time of writing) and note the retention-clear date next to the purge
  report. Do not expire backups or WAL to shorten it: a leaked credential is
  remedied by ROTATION (mandatory, before or with the purge), the purge only
  removes it from durable memory and agent reads. The same applies to a
  host's SQLite store: `VACUUM` frees the file, earlier snapshots or backups
  of the host keep the bytes until they age out.

Procedure (`kubectl -n memoire`, ops image that contains this
`memoire-purge` build):

```sh
# 0. Operator pod: ops image, custody PVC at /data, DSN from the secret.
kubectl -n memoire apply -f - <<'YAML'
apiVersion: v1
kind: Pod
metadata: { name: memoire-purge-op, namespace: memoire }
spec:
  restartPolicy: Never
  # The custody volume is RWO Longhorn: start on the hub's node, like the
  # cdk8s ops Jobs (the pod keeps the attachment once the hub is at 0).
  affinity:
    podAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - labelSelector: { matchLabels: { app: memoire-hub } }
          topologyKey: kubernetes.io/hostname
  tolerations: [{ key: nvidia.com/gpu, operator: Equal, value: present, effect: NoSchedule }]
  securityContext: { fsGroup: 0, fsGroupChangePolicy: OnRootMismatch }
  imagePullSecrets: [{ name: registry-pull }]   # same as the cdk8s ops Jobs
  containers:
    - name: ops
      image: <OPS_IMAGE from cdk8s/main.ts, rebuilt with this purge>
      imagePullPolicy: IfNotPresent
      command: ["sleep", "7200"]
      env:
        - { name: MEMOIRE_HOME, value: /data }
        - name: MEMOIRE_DB
          valueFrom: { secretKeyRef: { name: memoire-pg-app, key: uri } }
      volumeMounts: [{ name: home, mountPath: /data }]
  volumes:
    - name: home
      persistentVolumeClaim: { claimName: memoire-home-lh }
YAML
kubectl -n memoire wait --for=condition=Ready pod/memoire-purge-op

# 1. Dry-run (hub may stay up): read-only counts.
kubectl -n memoire exec memoire-purge-op -- memoire-purge --home /data --event-ids 2507894,2520360

# 2. Quiesce: record each store/custody CronJob's current suspend state,
#    suspend them all (memoire-raw-pack too: it must not relocate raw files
#    a crashed purge still has to delete; the packer also refuses while
#    purge-intent.json exists), wait for running Jobs, then scale the hub
#    (API + drainer) to 0. Clients' forwarders spool and retry meanwhile.
CJS="memoire-derive-sweep memoire-embed-sweep memoire-snapshot memoire-raw-pack"
for cj in $CJS; do
  echo "$cj $(kubectl -n memoire get cronjob "$cj" -o jsonpath='{.spec.suspend}')"
done >| tmp/purge-cronjob-suspend-states.txt
for cj in $CJS; do kubectl -n memoire patch cronjob "$cj" -p '{"spec":{"suspend":true}}'; done
kubectl -n memoire get jobs                       # no ACTIVE job left
kubectl -n memoire scale deploy/memoire-hub --replicas=0
kubectl -n memoire wait --for=delete pod -l app=memoire-hub --timeout=120s

# 3. Apply. --age-recipient takes the PUBLIC key (age1...) of the quarantine
#    identity; the private key stays in operator custody (see "Quarantine
#    identity" below) and is never needed in the pod.
RCPT=age1...   # the quarantine recipient from operator custody
kubectl -n memoire exec memoire-purge-op -- env MEMOIRE_PURGE_APPLY=yes \
  memoire-purge --home /data --event-ids 2507894,2520360 \
  --apply --i-have-quiesced-the-hub --age-recipient "$RCPT" \
  --report /data/purge-report-$(date -u +%Y%m%dT%H%M%SZ).json

# 4. Verify: a re-run dry-run matches nothing; the rows are gone; the
#    tombstone exists.
kubectl -n memoire exec memoire-purge-op -- memoire-purge --home /data --event-ids 2507894,2520360
kubectl -n memoire exec "$(kubectl -n memoire get cluster memoire-pg -o jsonpath='{.status.currentPrimary}')" -c postgres -- psql -U postgres -d memoire -At -c \
  "SELECT count(*) FROM events WHERE id IN (2507894,2520360);
   SELECT count(*) FROM audit_log WHERE action = 'purge.apply';"

# 5. Restore the PRIOR suspend states (a lane suspended by ruling stays
#    suspended), then remove the operator pod.
kubectl -n memoire scale deploy/memoire-hub --replicas=1
while read -r cj state; do
  kubectl -n memoire patch cronjob "$cj" -p "{\"spec\":{\"suspend\":${state:-false}}}"
done < tmp/purge-cronjob-suspend-states.txt
kubectl -n memoire delete pod memoire-purge-op
```

Quarantine identity: the tool only encrypts, so it needs a recipient, not
a key. Use the fleet's quarantine identity (private key in the operator's
vault, public recipient `age1...` recorded beside it). Derive the
recipient on the workstation with `age-keygen -y <identity file>`; if
the hub PVC already carries an identity at `/data/age-quarantine-key.txt`
the same command inside the pod yields the matching recipient. Do not
create a new identity for the purge and do not copy a private key onto
the PVC. The primary pod name is discovered at run time with
`kubectl -n memoire get cluster memoire-pg -o jsonpath='{.status.currentPrimary}'`
(`memoire-pg-3` since the 2026-10-28 Longhorn move).

If step 3 dies, `/data/purge-intent.json` remains: keep the hub
quiesced and re-run with `--resume-intent --apply
--i-have-quiesced-the-hub` (same env gate). The quarantine archive lands
at `/data/quarantine-purge-<epoch>.tar.age`; if an identity file also
lives on the PVC, the archive is decryptable in place: move it off the
PVC or destroy it to complete destruction.
Snapshot mirrors pushed before the purge still hold the matched `raw/`
files; the generation bump flags them as predating the purge
(`docs/MIRROR-PROTOCOL.md`).

## Read telemetry (L0009 A1)

`/metrics` counts successful reads so retention rulings rest on evidence
(records/history/L0009, operator ruling a2 = A1). Counters are in-process and
reset on restart; VMAgent keeps the series. Labels are closed vocabularies
only, never a query, project or session string.

- `memoire_read_requests_total{route,raw}`: `route` is `search`, `context`,
  `timeline`, `observation_get`, `observation_list`, `session_get` or
  `session_list`. `raw` is `included` when the request filter admits
  `kind=raw` projections, `excluded` when it drops them, and `n_a` for the
  session routes.
- `memoire_read_items_total{route,kind,age}`: items returned, by `kind`
  (`raw` or `derived`) and by the age of the item's `occurred_at` at read time
  (`lt_1d`, `1d_7d`, `7d_30d`, `30d_90d`, `gt_90d`). Session routes return
  no items.

Share of returned items older than 30 days, over the last 30 days:

```promql
sum(increase(memoire_read_items_total{age=~"30d_90d|gt_90d"}[30d]))
  / sum(increase(memoire_read_items_total[30d]))
```

## Failed-queue TTL

The in-process drainer reaps `queue/failed/` items (and their
`.error.json` sidecars) older than 30 days, hourly, mtime-based
(liaison:D97 decision 8). `MEMOIRE_FAILED_TTL_DAYS` overrides the window;
`0` disables. Reaps are logged and audited (`queue.failed_reap`).
`touch(1)` an item under investigation to extend its life. Depth stays
visible as `queue_failed_depth` on `/v1/health`.

## Remote capture forwarding (memoire-forward)

The client half of remote-capable capture (memoire:D147 slice 3, D153).
Capture adapters are unchanged: every one of them writes the LOCAL file
queue through `bin/memoire-queue`. On a client machine (post-cutover
the hub host, later the client host), `bin/memoire-forward` drains that queue by
POSTing each envelope to the hub's `POST /v1/events` and deletes the
local file only on the queue-accept ack (`202` + `"queued":true` —
memoire:D138 echo): the ack means the bytes are durably in the hub's own
fsynced pending queue, so deletion is custody transfer, not loss.

- Configure: `MEMOIRE_FORWARD_URL=https://memoire.superbiche.me/v1` (the
  hub's durable FQDN — TLS at the HL3 envoy gateway, resolves tailnet-only;
  generic form `http://<hub>:37778/v1`) plus the hub's
  bearer token at `MEMOIRE_FORWARD_TOKEN_FILE` (default
  `$MEMOIRE_HOME/forward-token`, mode 0600). The client's own `token`
  file stays distinct.
- Run: `memoire-forward` (loop), `-once` (single cycle), `-status`
  (client-visible queue depths + last forward status as JSON; needs no
  URL and never talks to the hub).
- Delivery is at-least-once: a crash between ack and local delete
  re-forwards, and hub ingest collapses the duplicate via the UNIQUE
  `idempotency_key` column (hash of `source_adapter:source_event_id`;
  envelopes arriving without a `source_event_id` are stamped a
  retry-stable original-bytes id at the hub's HTTP surface).
- Failure taxonomy: transport errors, 5xx, and 401/403 are systemic —
  the item stays pending (off-LAN degrades to capture-and-forward-later)
  and the loop backs off exponentially (2s..5m). Other 4xx are per-item
  poison — moved to `queue/failed/` with an `.error.json` sidecar, same
  shape the drainer writes, so `queue_failed_depth` applies unchanged and
  the forwarder loop runs the same hourly TTL reap the drainer would
  (`MEMOIRE_FAILED_TTL_DAYS`; file-only on clients — no audit row, since
  there is no local store to audit into).
- Priority: items of priority adapters are forwarded before any other
  item, so a backlog (a first Codex/OpenCode import, a machine back
  online) does not delay the live capture of those adapters (default: the
  Claude hook, `claude-code`). The adapter is read from the queue file
  name (`<ms>-<adapter>-<hash>.json`). Live Codex/OpenCode trigger items
  share `codex-cli` and `opencode-native` with the readers' catch-up
  backlog, so they are not prioritised, and listing those adapters would
  promote the backlog too (MISSING #14). Inside each class the name
  (arrival) order is kept. Non-priority items go in slices of 150 (about
  three seconds at the measured ~47 items/s); the pending directory is
  re-read after each slice, so a priority item that arrived mid-cycle waits
  at most one slice. `MEMOIRE_FORWARD_PRIORITY_ADAPTERS` (comma-separated
  adapter names, default `claude-code`; set empty to disable) names the
  priority set. Throughput is unchanged: one POST per item.
- Client visibility: the hub cannot see what never arrived. The inject
  header (`memoire-inject-claude.sh`; session-start injection is off by
  default, `MEMOIRE_INJECT=on` turns it on, and with it off no header is
  printed) surfaces a CAPTURE BACKLOG block
  when the local queue is deep (`MEMOIRE_INJECT_BACKLOG_MIN`, default
  25), old (`MEMOIRE_INJECT_BACKLOG_AGE_S`, default 600s), or has failed
  items; `memoire-forward -status` gives the same counters on demand.
  The forwarder also persists `$MEMOIRE_HOME/forward-status.json`
  (last success/error, consecutive failures) after every active cycle.
  The alert that does not depend on a session is the capture health check
  ("Capture health alert").
- EXCLUSIVITY: a given MEMOIRE_HOME is drained by exactly ONE consumer —
  the local `memoire-api` drainer (hub topology) or `memoire-forward`
  (client topology), never both. Both claim files safely via rename, but
  running both splits the corpus between two stores.

## Clock-skew alert

memoire:D152 one-sided future-skew alert. `/v1/health` reports
`clock_skew_future_events`: events whose `occurred_at_epoch` is AHEAD of
`received_at_epoch` by more than `MEMOIRE_SKEW_ALERT_SECONDS` (default
60; `0` disables). Ahead = unambiguous producer clock fault; behind is
indistinguishable from legitimate queue buffering and is never counted.
`received_at_epoch` is stamped at the HUB's HTTP surface (arrival time,
receiver's clock) so the comparison is cross-machine for forwarded
envelopes — a fast client clock cannot skew both sides together; the
producer's own stamp survives as `metadata.client_received_at_epoch`.
The count is served from the `schema/0012` partial index (clock-fault
candidates only — normally empty), never a table scan.
`/v1/health` reports a counter it could not read within its 3 s budget as
`null`, never `0`; capture-health skips such a value (`clock_skew:unreachable`)
and keeps its baseline. `last_write_epoch` is likewise `null` until the hub's
background exact last-write scan has landed after a start, and
`scripts/check-health.sh` skips its write-stall check while it is null. The inject header
(only when `MEMOIRE_INJECT` is on; it is off by default)
emits a CLOCK ALERT block when the counter is nonzero: check chrony/NTP
on every capture machine (D149 procedure). Window queries are never
touched by this — alert only.

## Capture coverage per harness (harness parity)

`scripts/memoire-coverage.py` is Memoire's side of the fleet harness-parity
gate (liaison `docs/design/HARNESS-PARITY.md`, ruled 2026-10-15). The fleet
inventory `~/.agents/harnesses.json` says which harnesses exist and which acpx
agents run them; `registry/capture-lanes.json` says which capture lane Memoire
declares for each one (`hooks`, `rollout-sweep`, `store-sweep`, `extension`,
or `none` with a reason) and whether an acpx-driven session on that harness
gets `boundaries` (the native lane already captures the turns) or the full
`journal`. The validator checks every lane LIVE — timer active, env flag on in
`~/.memoire/capture-tick.env`, hook present on each event and pointing at an
executable script, extension byte-equal to its source, the acpx exclusion file
readable — and publishes `~/.agents/coverage/memoire.json` (schema
`liaison-coverage/1`: producer project/commit/host, `generated_at`, the sha256
of the inventory it read, one disposition per route). Liaison's
`harness.parity` row reads that file and fails closed when it is missing, older
than seven days, computed against another inventory, or produced by anyone but
`memoire`. Nothing in it is authored: `wired` is observed or it is a gap.

```sh
make coverage          # print the table and publish the file
make coverage-check    # this project's gate: red while any route is a gap
```

`scripts/memoire-capture-acpx.sh` derives its boundaries-only mode from the
same declaration: record `agent_command` basename -> acpx registry agent ->
owning harness -> `acpx: boundaries`. A missing registry, an unknown command
or any lane other than `boundaries` keeps full journal capture; kill switches
and the exclusion file are never overridden. The acpx lane is declared on
`memoire-capture-acpx-tick.timer` (runner `scripts/memoire-capture-acpx-tick.sh`,
installer `scripts/capture-acpx-tick-timer.sh install`, gated by
`MEMOIRE_CAPTURE_ACPX=on` in the env file): those two scripts land with the
scheduled-tick slice (`feat/acpx-journal-tick`, kickoff a64), and until they
do the acpx route reads as a gap. Test: `make test-core` (the coverage-test.sh lane).

## Capture health alert

`scripts/memoire-capture-health.sh` is the check that replaced the session-start
CAPTURE BACKLOG, TRANSPORT DEAD and CLOCK ALERT blocks as the way a client's
stuck or dead capture reaches the operator (MISSING #19). It runs from a user
timer, outside any session, holds no credential, and takes about 50 ms with the
systemctl calls stubbed (`systemctl --user` adds a few milliseconds per call).

**What is checked.** Each condition has a value and a threshold; it fires when
the value reaches the threshold.

| Condition | Fires when | Severity | Knob (default) |
|---|---|---|---|
| `backlog_depth` | pending plus inflight files >= N | warn | `MEMOIRE_INJECT_BACKLOG_MIN` (25) |
| `backlog_age` | oldest pending or inflight file >= S seconds | warn | `MEMOIRE_INJECT_BACKLOG_AGE_S` (600) |
| `failed_items` | any item in `queue/failed` (`.error.json` sidecars excluded) | warn | none |
| `transport_dead` | oldest pending or inflight file >= S seconds, and the forwarder has not succeeded within S (or never, or is not running, or there is no forwarder status) | critical | `MEMOIRE_INJECT_TRANSPORT_DEAD_S` (21600) |
| `forward_failures` | `consecutive_failures` in `forward-status.json` >= N | warn | `MEMOIRE_HEALTH_FAIL_N` (10) |
| `forward_stale` | queue not empty, oldest pending >= grace, and last success older than S (`never` when it never succeeded) | warn | `MEMOIRE_HEALTH_STALE_S` (1800), `MEMOIRE_HEALTH_GRACE_S` (120) |
| `unit:<name>` | an installed and enabled unit is not active, or a timer has no next elapse while its service is not running (systemd shows no next elapse for the whole run of the triggered service; that reads `active:service-running` and is quiet) | warn | none |
| `unit_failed` | the service of an enabled capture tick timer failed at its last run (`Result` not `success`), whatever the timer's own state | warn | none |
| `home_unwritable` | the state file under `$MEMOIRE_HOME` cannot be written (alert mode only; value = the path, detail = the error); see "State file" below | warn | none |
| `clock_skew` | hub `/v1/health` `clock_skew_future_events` rose since the previous run (value = the increase) | warn | `MEMOIRE_HEALTH_CLOCK` (1; 0 skips) |

The depth, age and dead thresholds are the injector's, under its names. The
forwarder conditions need `forward-status.json`; on a hub, which has none, they
are skipped. The forwarder writes that file only after a cycle that did work or
failed, so a long idle gap leaves an old last success: `forward_stale` therefore
waits until the queue itself is older than the grace.

Inflight: the forwarder renames an item from `queue/pending` to `queue/inflight`
for the duration of the POST, and its `Reconcile` returns items left there by a
crashed process to `pending`. An inflight item is therefore an undrained lease
that will be re-queued, and it counts with pending for depth and age (rename keeps
the file's mtime). `queue.pending` in the JSON is the pending count alone, `queue.inflight` the
inflight count and `queue.pending_total` their sum: the number `backlog_depth`
tests and the log line's `pending_total=`. The two always say the same thing.

`transport_dead` means nothing drains the queue. Its limit: the forwarder's
last success is global, so a class of items the hub keeps refusing behind
traffic that succeeds stays at the `backlog_age` warn and never reaches
critical. `unit_failed` sees only a service that exited non-zero, timed out or
was killed; the tick scripts exit 0 on their own skips, so a tick that skips
every run still reads as healthy. A host resumed from a long
suspend, or back from a long time off the network, has old items while its
forwarder is alive and retrying: with a forwarder status whose last success is
within the threshold and the forwarder unit running, that is the ordinary
`backlog_age` warn. It is dead when there is no forwarder status (a hub), no
success yet, a last success older than the threshold, or the forwarder unit is
inactive, failed or not installed.

Units checked: `memoire-forward.service`, `memoire-capture-tick.timer`,
`memoire-capture-codex-tick.timer`, `memoire-capture-opencode-tick.timer`. An
absent unit is "not installed" and never an alert; a disabled unit is the
operator's choice and never an alert. The one exception: when no
`memoire-forward.service` is installed and the oldest pending file is at the
`backlog_age` threshold while `memoire.service` (the local drainer of a hub) is
not active, the missing forwarder fires. With no user bus for `systemctl`, or no
`systemctl`, the unit checks are skipped and `--status` says so. Every
`systemctl` call runs under `timeout` (`MEMOIRE_HEALTH_SYSTEMCTL_TIMEOUT_S`, 5):
the first call that times out stops all further calls, the units are reported
`unknown`, `skipped` names `units:systemctl-timeout` and the log line carries
`systemctl_timeouts=<n>`, so a hung user manager costs a run about one timeout,
far under the unit's `TimeoutStartSec=2m`. A manager that does not answer (a
timeout, an unreachable user bus, an unreadable `is-active` answer, a failed
`list-timers` for a timer that was firing) never reads as a recovery: every
`unit:*` condition and `unit_failed` that was firing at the previous run stays
firing, with value `unknown` and a detail saying the manager did not answer, so
there is no `recovered` event and no "healthy again" toast. When the manager
answers again the real state takes over. A unit that was not firing is not
raised by an unknown. A `list-timers` that fails without timing out adds
`units:list-timers-failed` to `skipped`.

The clock read is one unauthenticated `GET <MEMOIRE_BASE_URL>/v1/health`,
bounded to 3 s. It is skipped when `MEMOIRE_BASE_URL` is unset, and an
unreachable hub is skipped, not an alert. The hub's count is a lifetime count
that never goes down by itself, so the condition fires only on a run where the
count is higher than the one stored in `health-state.json` (`clock_skew_seen`) by
the previous run, and clears on the next run if it did not rise again. A first
run (no stored value) seeds it and does not alert; `--status` never writes it.

**Knobs** come from the process environment or `$MEMOIRE_HOME/capture-tick.env`
(last assignment wins; the file wins over the environment when it carries the
key). A numeric knob must be a positive integer of at most nine digits; anything
else falls back to its default and is named in the log line (`invalid_knobs=`).

**Channels.**

- Desktop: `notify-send` when it is installed and a session bus is reachable
  (`DBUS_SESSION_BUS_ADDRESS`, else `$XDG_RUNTIME_DIR/bus`). Urgency is `normal`
  for warn and `critical` for critical. One notification goes out when the state
  changes or a condition starts firing, one reminder every
  `MEMOIRE_HEALTH_REMIND_S` (21600, six hours) while a condition persists, and
  one "Memoire capture healthy again" notice on recovery. Nothing goes out for an
  unchanged state, a healthy first run, or a change that only removes firing
  conditions at the same state (a shrinking set is good news with nothing to act
  on, and a toast titled like a new alert would mislead; the sink still gets that
  `change` with `cleared` filled and the log says `notify=suppressed-clearing`).
  A de-escalation from critical to warn notifies, titled `(was CRITICAL)`. When
  no state file can be written (see "State file") the previous state is unknown:
  the log says `state_write=failed` once per run, a warn or an ok produces no
  event, and a critical notifies on every run, never as a reminder. A host without `notify-send` or
  without a bus raises no error: the log line says `notify=no-notify-send` or
  `notify=no-bus`.
- State file: `$MEMOIRE_HOME/health-state.json` (0600, written by tmp and
  rename). A full or read-only home usually also stops the capture itself, so an
  unwritable state file must not silence a lasting warn: when the write to the
  home fails, the state goes to `$XDG_RUNTIME_DIR/memoire-health-state.json`
  (tmpfs, per user), else to `${TMPDIR:-/tmp}/memoire-health-state-<uid>.json`,
  and the normal state machine applies there, reminders included. The run raises
  the warn condition `home_unwritable` (value: the home's state path, detail: the
  error), so the operator gets one toast and the sink one `change`, and the log
  says `state_file=fallback`. The previous state is the newest valid file among
  the three (`updated_epoch`), so the home takes over again, and
  `home_unwritable` clears, once it is writable. A fallback is read only when it
  is a regular file owned by the user (never a symlink) and dated at most 300 s
  ahead of now, and the fallbacks are removed as soon as the home is written
  again, so a stale or planted fallback never outlives the home's state. Only when none of the three can
  be written does the previous behaviour apply: `state_write=failed`, a critical
  every run, a warn silent. Writability is probed by creating and removing a
  small file next to the state file, in alert mode only; `--status` writes
  nothing and does not show `home_unwritable`.
- Sink: when `MEMOIRE_HEALTH_SINK` names an executable, it runs on every event
  (change, reminder, recovery) with the status JSON on stdin, bounded by
  `MEMOIRE_HEALTH_SINK_TIMEOUT_S` (10). Its failure or timeout is logged
  (`sink=failed:<rc>`, `sink=timeout`, `sink=not-executable`) and never fatal.
  Nothing else is built for Alertmanager: this is the seam.
- Log: one line per run on stderr, so in the journal under the timer:
  `memoire-capture-health: state=warn firing=backlog_age pending_total=3 oldest_s=700 failed=0 prev=ok event=change notify=sent sink=none ms=48`.

**The sink contract.** A poster for Alertmanager or any other system is written
from this section alone. The sink is started once per event and never on a run
where nothing changed. It gets the one-line JSON below on stdin, plus
`MEMOIRE_HEALTH_EVENT` and `MEMOIRE_HEALTH_STATE` in its environment. Stdout and
stderr are discarded, the exit status is only logged, and it is killed at
`MEMOIRE_HEALTH_SINK_TIMEOUT_S`.

`event` values:

- `change`: the state or the set of firing conditions differs from the previous
  run (a first run that is not ok included). Conditions may have been added, cleared
  or both.
- `reminder`: nothing changed, the state is warn or critical, and
  `MEMOIRE_HEALTH_REMIND_S` has passed since the previous event.
- `recovered`: the state is now `ok` and the previous run was warn or critical.
  `firing` is empty and `cleared` lists everything that was firing.

A healthy first run, an unchanged state and a run with no event never call the
sink. Fields (all always present):

| Field | Type | Meaning |
|---|---|---|
| `event` | string | `change`, `reminder` or `recovered` |
| `previous_state` | string | `ok`, `warn`, `critical`, or `none` (first run, corrupt state file, or no state file could be written) |
| `state` | string | `ok`, `warn` or `critical` now |
| `firing` | array of string | names of the conditions firing now |
| `cleared` | array of string | conditions firing at the previous run and not now; `[]` on a reminder, a first run or an unknown previous state |
| `remind_s` | number | `MEMOIRE_HEALTH_REMIND_S` in effect |
| `host`, `memoire_home` | string | host name, the Memoire home |
| `time`, `time_epoch` | string, number | the run's UTC ISO 8601 time and epoch |
| `conditions` | array of object | every condition evaluated, firing or not: `name` (string), `firing` (bool), `severity` (`warn` or `critical`), `value` and `threshold` (number or string), `detail` (string) |
| `queue` | object | `pending` (pending files only), `inflight`, `pending_total` (pending plus inflight, the count `backlog_depth` tests and the log's `pending_total=`), `oldest_pending_age_s` (age over pending plus inflight), `failed`, all numbers |
| `forwarder` | object | `present` (bool), `consecutive_failures`, `last_success_epoch` (numbers) |
| `units` | object | unit name to `active`, `inactive`, `failed`, `activating`..., `unknown`, `disabled` or `not-installed` |
| `skipped` | array of string | checks not run and why |

Poster rules:

1. On every event, for each `conditions[]` entry with `firing: true`, post (or
   refresh) one alert named after `name`; for each name in `cleared`, resolve its
   alert. A `change` can clear one condition while the state stays warn, and only
   `cleared` says so. Also resolve any alert you posted whose name is in neither
   `firing` nor `cleared` (a condition that disappeared with its check, for
   example a skipped hub read or an uninstalled unit).
2. On `recovered`, resolve everything.
3. The sink is silent while nothing changes, for up to `remind_s` (six hours by
   default). Set each alert's `endsAt` beyond that interval (for example
   `remind_s` plus the 15 minute cadence plus a margin), or re-post the firing
   alerts on every `reminder`; an alert with a short `endsAt` and no re-post
   resolves itself while the condition persists.
4. A run where no state file can be written (home and both fallbacks) sends no warn events at all, and
   critical events on every run with `previous_state: none`: treat each as
   a refresh, never as a signal that something else cleared.

5. Alert identity must include `host`: the three clients emit the same
   condition names (`backlog_age`, `unit:memoire-forward.service`, ...), and one
   Alertmanager would merge them.
6. The check has no heartbeat. The sink is never called while the state is ok,
   so a dead timer, a stopped check or a host that is off is invisible to the
   poster. A poster that must catch those raises its own absence alert, for
   example on a periodic signal it receives from elsewhere, or on a client it
   expects to hear from and does not.
7. `recovered` is true: the check does not send one while a unit condition's
   manager did not answer (those conditions stay firing as `unknown`, see
   Units checked), nor on a failed state write.

Such a poster is written outside this repository.

**Install** (user units, no credential, from the main checkout):

```bash
scripts/capture-health-timer.sh install     # *:0/15, Persistent, RandomizedDelaySec=120
scripts/capture-health-timer.sh status
scripts/capture-health-timer.sh run-once    # then: journalctl --user -u memoire-capture-health.service -n 5
scripts/capture-health-timer.sh stop        # or uninstall
```

The unit reads the optional `~/.memoire/capture-tick.env` and accepts exit codes
1 and 2 as success, so a firing condition does not mark the unit failed.

**Reading `--status`.** `scripts/memoire-capture-health.sh --status` prints one
JSON object and alerts nothing and writes nothing:

- `state`: `ok`, `warn` or `critical`; `firing`: the names of the firing
  conditions; `host`, `time` (UTC), `time_epoch`.
- `conditions[]`: `name`, `firing`, `severity`, `value`, `threshold`, `detail`
  for every condition evaluated, firing or not.
- `cleared`: conditions firing at the previous run and not now, read from
  `health-state.json`; `remind_s`.
- `queue`: `pending`, `inflight`, `pending_total`, `oldest_pending_age_s`, `failed`. `forwarder`:
  `present`, `consecutive_failures`, `last_success_epoch`. `units`: per unit,
  its systemd active state as reported (`active`, `inactive`, `failed`,
  `activating`...), or `unknown` (a timed-out call), `disabled`, `not-installed`.
- `skipped`: checks not run and why (`forward_status:absent`,
  `clock_skew:no-base-url`, `units:no-systemctl`, ...).

Exit code: 0 ok, 1 warn, 2 critical, in both modes. An error of the check itself
(unreadable or missing `MEMOIRE_HOME`, no `jq`, a bad argument) logs one line
with `error=...` and exits 0; the exit code reports the conditions, never the
check's own failures (no `HOME` and no `MEMOIRE_HOME` is one of them).

## Raw cold archive (memoire-raw-pack)

`memoire-raw-pack` (ops image; `bin/memoire-raw-pack` locally) packs the flat
envelopes of each closed UTC day (the day of the file name's millisecond
prefix, older than today minus `--grace-days`, default 2) into new parts
`raw/pack-YYYY-MM-DD.NNN.tar.zst` (at most `--max-files-per-part`, default
200,000 members each) and deletes the flat files a part provably holds. One
inode per part replaces one per POST: the hub's `/data` ran out of inodes on
2026-10-11 with 3.28 M flat files. Rationale:
`records/history/L0006-raw-cold-archive-daily-packs.md`.

- Readers do not change behaviour: Replay (`memoire-pgshadow`), purge and the
  retro-sweep read flat and packed files through `internal/rawstore` in the
  same global name order, with the same bytes and mtimes.
  `memoire-raw-pack --verify` prints a digest of the whole store (names and
  content hashes in walk order), taken under the custody lock. It is identical before and after packing,
  and after a snapshot restore.
- Since operator ruling a65 the drainer copies an item into raw/ only if
  ingesting it can change the store. An exact re-POST (idempotency key already
  stored AND session known) gets no raw copy (L0007), which stops the
  reboot re-send storms from consuming inodes. Raw files written before the
  change keep their duplicates.
- Only names with a 13-digit millisecond prefix are packed; other names stay
  flat. Within that width, name order is chronological order.
- Parts are IMMUTABLE. A late drain (an old queue item whose name falls in an
  already-packed day) goes into the next part of that day, never into an
  existing part. A part name is reused only after `--unpack` has advanced the
  custody generation. Only purge rewrites a part (dropping members), and every
  purge bumps the custody generation, which makes the snapshot re-baseline.
  So the snapshot's by-name membership never holds a stale revision.
- Parts are plaintext zstd tar on the custody volume (LUKS on the hub), like
  the flat files they replace. The snapshot captures them by name and
  age-encrypts them. The HL6 mirror copies them as plaintext onto its own
  LUKS volume, exactly as it copies flat files.
- Staging lives beside `raw/`, never inside it, so the snapshot's raw/
  inventory never sees it: `<home>/.raw-pack-tmp/` (part temps, written,
  re-read and checked, fsynced, then renamed into raw/) and
  `<home>/.raw-incoming/` (the drainer stages each raw copy there, fsyncs it
  and links it into raw/, so raw/ never shows a partially written file).
  Neither is custody: a part temp copies flat files that still exist, and an
  incoming copy duplicates a queue item that is still queued. The packer
  discards its own stale temps; purge discards both before journaling.
- Custody: every raw/ operation except the drainer's single-file publish goes
  through `rawstore.Custody` (operator ruling a76). It takes the lock
  `<home>/.raw.lock` (beside raw/), then checks the purge interlock under it.
  This covers the packer, `--unpack`, `--verify`, purge, Replay and the sweep;
  all take it non-blocking. Replay, the sweep and `--verify` also refuse
  while `<home>/purge-intent.json` exists. The snapshot waits up to
  `MEMOIRE_SNAPSHOT_LOCK_WAIT_S` (1800 s), then fails.
- Purge interlock: the packer refuses while `<home>/purge-intent.json`
  exists (`--purge-intent` to point elsewhere). Purge resume also finds a
  journaled name wherever it lives now, flat or in any part of its day.
- Crash safety: a killed packer leaves at most a temp (ignored, then
  discarded) or a published part whose flat copies were not yet deleted.
  Readers yield each name once; the next run deletes the copies after
  checking their bytes. A name packed with DIFFERENT bytes than its flat
  copy is a custody conflict: every reader and the packer refuse until an
  operator resolves it.
- HL6 mirror (`hl3-mirror.service`, 06:30, rsync over NFS, not under the
  lock): if a packer run overlaps a mirror run, files listed flat but packed
  before transfer vanish from that transfer (rsync rc 24, which the mirror
  counts as success), and the new part is not in that run's inventory. Those
  envelopes then reach the mirror at the next run, inside the part. That is
  a delay of one extra mirror cycle; data already present before the run can
  wait that cycle too, and the run still reports success. The DR exposure is
  HL3 being lost inside that window (known risk R5, infra follow-up on the
  rc-24 handling). Nothing is deleted from the mirror early, because
  `--delete-after` only removes names absent from the source listing. Keep
  packer runs outside the mirror window anyway (the CronJob runs at 04:15
  with `--max-seconds 5400`), and do not run the first apply while a mirror
  run is active (`systemctl status hl3-mirror` on hl6).
- Scale (bench `tmp/raw-archive/bench/SUMMARY.md`, 1 M-file day on ext4):
  packing took 67 s at 360 MB peak RSS; a purge rewrite of one 200k-member
  part took 10 s at 146 MB, with temp space of one part (~360 MB); verifying
  1 M flat files took 16 s at 240 MB. The name listing grows by about 100 B
  per flat file, so the first apply over the hub's ~3.5 M-file backlog needs
  about 1 GB. The CronJob requests 512Mi and is limited to 1536Mi.
- The CronJob `memoire-raw-pack` (04:15 Europe/Paris, after the 02:30
  snapshot, before the 06:30 HL6 mirror) ships SUSPENDED.

```sh
bin/memoire-raw-pack                        # dry-run: JSON report, zero writes
bin/memoire-raw-pack --verify               # store digest; zero writes
bin/memoire-raw-pack --apply --max-parts 10 # bounded apply, oldest days first
```

First apply on the hub (an infrastructure mutation: get the operator's go
first):

1. Deploy api and ops images that carry this build. The drainer's atomic
   publish ships in the api image, and the packer must not run against an
   older drainer. The CronJob stays suspended.
2. From a one-off pod built like the purge operator pod (ops image, custody
   PVC at `/data`, no DSN, memory limit at least 1536Mi), dry-run:
   `memoire-raw-pack --home /data`. Record `--verify` (digest D0).
3. Apply in bounded batches outside 02:30–03:30 and outside any HL6 mirror
   run: `memoire-raw-pack --home /data --apply --max-parts 20`. Repeat until
   `days` is empty. Watch `df -i /data` fall. `--verify` must print D0 plus
   only the envelopes that arrived since.
4. The next HL6 mirror run moves the deleted flat files into
   `/srv/crypt/memoire-home-mirror/_archive/<day>/`, a rename on the same
   filesystem, which consumes no new inodes. The mirror does not reclaim
   those inodes for 14 days, and the parts add a few hundred more.
   `MemoireDataInodesHigh` covers both volumes.
5. Unsuspend the CronJob (`suspend: false` in `cdk8s/main.ts`, synth,
   verify, deploy).

Rollback of one part: `memoire-raw-pack --home /data --unpack
pack-YYYY-MM-DD.NNN.tar.zst`. Never unpack by hand. Under the custody lock,
and refusing while a purge intent exists, it:

1. advances the custody generation in `custody/MANIFEST.json`;
2. restores every member as a flat file (identical bytes and mtimes);
3. removes the part.

The generation bump comes first because a later repack may reuse the part
name. Without the bump, the snapshot would treat the reused name as already
captured and silently omit envelopes added since (review N1). A re-run after
a crash is safe and converges.

## Encrypted mirror snapshots (memoire-snapshot)

Slice 2 (D155). `scripts/memoire-snapshot.sh` produces the age-encrypted
artifact set (nightly DB snapshot via VACUUM INTO + generation-namespaced
raw baseline/incrementals + plaintext MANIFEST.json) into
`$MEMOIRE_HOME/snapshots/`, and `--push` rsyncs it (with `--delete`,
excluding `state.json`) to the HL3 landing zone
`/data/backups/memoire/` via the rrsync-locked `memoire-mirror` identity.
Contract: docs/MIRROR-PROTOCOL.md — notably rule 5: the dedicated
snapshot key's rotation is the mirror crypto-shred lever (the off-site
pipeline never deletes remote files).

- Recipient: `$MEMOIRE_HOME/snapshot-recipient.txt` (age public key).
  Private key custody: Bitwarden "Memoire Snapshot Key" + printed copy
  (D150/D154) — never on disk outside minting/rotation.
- Nightly: `memoire-snapshot.timer` (the hub host's user scope) runs
  snapshot+push before HL3's 03:17 off-site sweep.
- On the hub (a11 = A): the `memoire-snapshot` CronJob needs both the
  custody volume (RWO Longhorn, on the hub's node) and the HL3 landing
  zone (hostPath), so it runs only while the hub is on HL3. After a hub
  failover its Job pod stays Pending until the Job's 3600 s active
  deadline fails it; `CronJobRunFailed` then fires after its 5 min hold.
- Failback to HL3 (the hub prefers HL3 but never moves on its own). RWO
  allows sharing on one node, not attachment on two, and a running custody
  Job keeps the volume on the old node, so drain first:
  1. record each flag (`kubectl -n memoire get cronjob <name> -o
     jsonpath='{.spec.suspend}'`), then `kubectl -n memoire patch cronjob
     <name> -p '{"spec":{"suspend":true}}'` for memoire-snapshot,
     memoire-raw-pack and memoire-embed-sweep;
  2. wait until `kubectl -n memoire get jobs` shows no active custody Job;
  3. `kubectl -n memoire rollout restart deploy/memoire-hub`, then check
     the new pod's node (`kubectl -n memoire get pod -l app=memoire-hub
     -o wide`) and `/v1/readiness`;
  4. patch each flag back to its recorded value; `make -f Makefile.ledger
     deploy-diff` must then report no drift (the deploy gate refuses
     unexplained drift, so a leftover suspend blocks the next deploy).
- Restore: latest `db/memoire-db.db.zst.age` alone restores the store
  (`age -d -i <key> | zstd -d > memoire.db`); raw custody =
  `raw/gen-<G>/baseline` + incrementals in cutoff order.
- Crypto-shred (post-purge, operator): mint a new key, replace the
  recipient file — the next snapshot run detects the rotation and
  re-encrypts EVERYTHING automatically (state tracks the recipient; a
  recipient change forces a full re-baseline) — run snapshot --push,
  VERIFY the push succeeded (manifest authority: every listed artifact
  present), and only then destroy the old key (vault entry + paper).
- Restore authority: only MANIFEST.json-listed artifacts count; unlisted
  files are push residue. VERIFY BEFORE RESTORE — the push protocol
  (delay-updates sync, then manifest, then prune) keeps the set complete
  through interrupted transfers, but a crash during the final rename
  batch can leave a listed artifact with newer bytes than its hash:
  `cd <landing> && jq -r '.artifacts[] | "\(.sha256)  \(.name)"' MANIFEST.json | sha256sum -c -`
  On mismatch: recover the prior bytes from the off-site `_archive`, or
  re-push from staging.
- `MEMOIRE_SNAPSHOT_MAX_INCS` (30) bounds the incremental chain;
  a custody-generation bump always re-baselines and drops the old
  generation from staging and landing.

## Embed lane (vector hybrid, D218)

> **Retired 2026-11-01 (L0010, ruling a13 = A).** The embed Deployment,
> Service and sweep left the manifest and the hub runs FTS only. This section
> is kept as the record of the retired lane.

### L0010 transition (one-time, operator-gated)

`kubectl apply` does not prune, so the manifest change alone leaves the retired
workloads running. With the operator's go:

1. Check nothing is mid-run: `kubectl -n memoire get jobs` (wait out any active
   `memoire-embed-sweep-*` or `memoire-derive-sweep-*` Job).
2. Delete the five retired objects:
   `kubectl -n memoire delete cronjob memoire-derive-sweep memoire-embed-sweep`,
   `kubectl -n memoire delete deployment memoire-embed`,
   `kubectl -n memoire delete service memoire-embed`,
   `kubectl -n memoire delete configmap memoire-model-endpoints`.
3. `make -f Makefile.ledger deploy-diff`, then `make -f Makefile.ledger deploy`.
   The parity gate cannot explain removed identities from its baseline; if it
   refuses on them or on the hub's dropped
   embed env and mount, deploy with the judged exception
   `MEMOIRE_DEPLOY_ACCEPT_DRIFT=1` and record it.
4. The out-of-band Secrets `memoire-embed-token` and `memoire-model-keys` are
   cleanup candidates once no other consumer reads them; keep both custody
   PVCs.

The pgvector lane keeps derived observations embedded (MiniLM q8, 384
dims) for hybrid search. Three moving parts, all hub-side: the
`memoire-embed` Deployment (query-time `POST /embed`), the
`memoire-embed-sweep` CronJob (every 15 min: missing list → embed →
upsert), and the admin-scoped `/v1/embeddings` seam on the API. The sweep
is stateless — "missing" is a live SQL anti-join, so crashed or skipped
runs are always made up by the next tick; there is no queue to lose.

- Lane presence: `GET /v1/health` → `vector_lane: "pgvector"` (or
  `"absent"` — after a deploy, absent means `memoire-pgshadow apply`
  has not provisioned `observation_embeddings_pg` yet, or the pgvector
  extension is missing).
- Freshness (the observable): admin token, then
  `curl -H "Authorization: Bearer $TOK" "https://memoire.superbiche.me/v1/embeddings/freshness"`
  → `{model, missing, oldest_missing_epoch}`.
- **Look-line (ratified, 2h):** `missing > 0` AND `oldest_missing_epoch`
  older than 2 hours = 8 consecutive missed sweep runs — systemic, never
  a blip. Check the CronJob's failed-job history
  (`kubectl -n memoire get jobs | grep embed-sweep`) and the last pod log.
  Steady state is `missing` ≈ 0 (only rows derived since the last tick).
- A sweep that fails with "no progress: observation N still missing"
  is the livelock guard: the worker's sha256/model no longer matches the
  API's CURRENT test (`services/api/internal/db/vector.go`) — a code
  drift between image versions, never a data problem.
- Provisioning (deployment step, idempotent): from an ops-image pod,
  `MEMOIRE_PG_URL=$MEMOIRE_DB memoire-pgshadow -schema-pg /app/schema-pg apply`.
  Safe to re-run; no-op without the extension.
- Model upgrades: bump the model identity in
  `services/api/internal/db/vector.go` + the worker's
  `services/embed-worker/src/embedder.mjs`
  together (one commit), ship both images — every row goes stale at once
  and the sweep re-embeds the corpus at ~15 min/58k-row chunks.

### Hybrid fusion (session B)

Search fuses the FTS and vector lanes by RRF (k=60, both lanes at depth
50) when three preconditions hold: Postgres engine, provisioned pgvector
lane, `MEMOIRE_EMBED_URL` wired. The deployment default is
`MEMOIRE_HYBRID_SEARCH` (`1` = on); a per-request `hybrid=1|0` on
`/v1/search` (or `"hybrid": true|false` in POST bodies) overrides it —
that override is how the acceptance harness measures hybrid on a
flag-off deployment.

- `GET /v1/health` → `hybrid_search`: `absent` (preconditions missing) |
  `off` | `on` (deployment default).
- Failure posture is fail-open: embed error, embed slower than the
  budget (`MEMOIRE_EMBED_BUDGET_MS`, default 150), or a vector-lane
  error all degrade that search to FTS-only — never an outage.
  `hybrid_fail_open_total` on health counts these; a climbing counter
  with `hybrid_search: on` means the embed service is dead or slow
  while searches silently degrade (check the `memoire-embed` pod, then
  the CronJob freshness above).
- Vector lane is derived-only (`kind != raw`); raw rows reach results
  through the FTS lane. Both lanes share the same visibility filters —
  project scoping, review state, noise, deletion — by construction.
- Exact-id guard (acceptance round 2): queries carrying a literal
  identifier (UPPERCASE ticket ids, `#NN` refs, `drv_` run ids, commit
  sha hex) route FTS-only even when hybrid is active — literal queries
  want literal ranking, and RRF measurably diluted their top-10.
  `hybrid_literal_skips_total` on health counts these routings
  (policy, not failure — distinct from the fail-open counter).

### Embed credential (F4 least-privilege split)

The `/embed` seam authenticates with a DEDICATED secret, not the admin
bearer: the serve Deployment mounts only the `memoire-embed-token`
Secret (`MEMOIRE_TOKEN_FILE=/secrets/embed/token`, no custody PVC), and
the API sends the same value (`MEMOIRE_EMBED_TOKEN_FILE`). A leak from
the serve pod grants query embedding only — zero API scopes. The sweep
CronJob is the one embed workload that keeps the custody PVC (read-only)
because `/v1/embeddings` requires the admin bearer.

- Mint (out-of-band, value never in manifests/argv/logs):
  `openssl rand -base64 32 | tr -d '\n' >| /tmp/embed-token && chmod 600 /tmp/embed-token`
  then `kubectl -n memoire create secret generic memoire-embed-token
  --from-file=token=/tmp/embed-token && rm /tmp/embed-token`.
- Rotate: recreate the Secret (`kubectl -n memoire create secret generic
  memoire-embed-token --from-file=token=… --dry-run=client -o yaml |
  kubectl apply -f -`), then restart BOTH Deployments
  (`kubectl -n memoire rollout restart deploy/memoire-hub deploy/memoire-embed`)
  — each side reads its token file once at process start.
- Failure shapes: missing Secret blocks both pods at volume mount
  (fail-closed, visible as ContainerCreating); a configured-but-empty
  token file is a loud API startup error, never a silent fallback to
  the admin bearer; mismatched values surface as `hybrid_fail_open_total`
  climbing (every embed call 401s → searches degrade to FTS-only).
- Local-first (no k8s): leave `MEMOIRE_EMBED_TOKEN_FILE` unset — the
  API falls back to its own custody token and the worker reads
  `$MEMOIRE_HOME/token`, the pre-F4 single-token topology.
