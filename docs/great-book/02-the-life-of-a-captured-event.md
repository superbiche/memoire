# 2. The life of a captured event
*Great Book v0.7 edition — 2026-11-02.*

Follow one thing all the way through.

On a Tuesday afternoon you tell an agent, in the middle of a longer message,
that the staging database must never be seeded from the production dump because
the anonymisation job runs after the seed and not before. Nobody writes that
down. Six weeks later, on a different machine, in a different harness, someone
asks what exactly you said — and whether the agent ran the seed anyway.

This chapter is the path of that sentence through Memoire, and the path out of
it to the system that turns it into memory. It has five stages, and each one
has a rule that only makes sense once you have seen what goes wrong without it.
A sixth section, at the end, tells the story of the stages that used to come
next and no longer do.

## Stage 1 — Capture: take everything, block nothing

When you sent that message, a hook fired. Hooks are the thinnest part of the
system by rule: normalize the input, write a file, exit. They do not call a
model, they do not query the store, and above all **they never block your
session on the service being up.**

Not every harness has hooks, and Memoire does not pretend otherwise. Claude Code
is captured live through hooks on prompt submission and on tool use, plus a
checkpoint at compaction and at session end, plus a timer that streams the
assistant's own text out of the transcript file every ten minutes (the
assistant's words live only there). Codex, Qwen and opencode are captured by
timers that sweep their own session files or stores; Muse Code is captured the
same way; Pi through an extension; sessions driven through the `acpx` client
by a journal sweep; and a generic text adapter takes anything else. Gemini has
no adapter at all (chapter 5 lists the lanes and says so plainly).

Every one of those paths ends the same way. The adapter builds a JSON envelope
and writes it into `$MEMOIRE_HOME/queue/pending/`. That is the whole write path
from a client's point of view. The file name is
`{epoch_ms}-{source_adapter}-{sha256}.json`, chosen so that lexical sort equals
chronological sort equals drain order. The hash in the name is the full SHA-256
of the body, not a short prefix, because a same-millisecond collision between
two different bodies would silently replace an envelope its producer already
holds an acknowledgement for. Deduplication does not rest on the filename: ingest derives an idempotency key
from the adapter and the producer's own event id, and a second event with the
same key is collapsed. When the producer gives no id, what happens depends on the
path: an envelope posted over HTTP gets an id derived from a hash of its original
body, so a re-sent body collapses; the local queue writer (and `memoire-queue`)
generates a fresh time-and-random id, so running the same command twice records
two events unless you pass `-source-event-id`. Only an event that reaches ingest
with no id at all falls back to a hash of its content.

One thing does happen before the pending file exists, and it is the single
exception to "capture takes everything": **secret-shaped content is redacted at
the source.** The redaction runs inside the queue writer, before the file is
written, so that neither the queue, nor the raw custody copies, nor the
database ever hold the value. It fails closed: if the filter cannot finish
(on pathologically nested input) or its key cannot be loaded, the write is
refused and nothing reaches the queue, rather than being written unredacted. Chapter 3 is the subject in full.

What lands is an **event**, and the event shape is generic on purpose. An
envelope must carry a schema version of 1, a project, a session id, a source
adapter, an event type and a `payload`; it may carry an occurred-at time, a
`text` field that is search-visible, and metadata. Search-visible is a
projection with bounds, not a copy: NUL is dropped and invalid UTF-8 becomes
U+FFFD; an inline screenshot, or any delimited run of two hundred base64
characters with the entropy of encoded binary, becomes `[blob N chars]`; and
text past 192 KiB or 20,000 letter/digit runs (a hyphenated compound counts
its parts) is cut and marked `[truncated]` — the payload keeps the whole
thing. A Claude Code tool call keeps
its `tool_name` and `tool_input` inside `payload`, where they belong, rather
than as columns that would make one harness's vocabulary into everyone's
schema.

If the service is not reachable at all — a laptop that is off the network, a
hub that is being redeployed — nothing breaks. On a machine that forwards to a
hub, a separate client program drains the local queue by posting each envelope
and deletes the local file only when the hub answers that the bytes are durable
in its own queue. The acknowledgement *is* the custody transfer, so an
interrupted forward loses nothing; it may send twice, and ingest collapses the
duplicate.

## Stage 2 — Drain and custody: raw first, then the row

The service, separately and asynchronously, drains the pending directory.
Draining is deliberately paranoid, and every one of its rules is a scar from
the system Memoire replaced:

- Claiming a file is an **atomic rename into `queue/inflight/`**, never an
  in-memory flag. A crashed process leaves evidence on disk instead of an
  orphan.
- **Startup reconciliation is mandatory.** Stale files in `inflight/` are
  moved back to `pending/` on boot. The predecessor's equivalent failure was
  rows stuck forever in a `processing` state that nothing ever revisited.
- There is deliberately **no second, hidden queue table** in the database. The
  queue files are the source of truth until the row commits.
- Files that fail permanently — an envelope that does not validate, an ingest
  error — move to `queue/failed/` with an error sidecar and are kept for
  thirty days by default, because a failure you cannot inspect is a failure you
  cannot fix.

The order of the next two steps is the heart of custody. **The raw copy is
written before the row.** The drainer copies the envelope into `raw/` — staged
beside it and linked in, so that `raw/` never shows a half-written file — and
only then ingests it. A stored event therefore always has a custody copy; the
reverse failure, a row with nothing behind it, cannot happen. There is one
exception, ruled in L0007: if the envelope is an exact repeat of an event that
is already stored, ingest would write nothing, so no new raw file is written
either. Without that rule the clients' habit of re-sending a window after a
reboot turned 44 percent of the hub's raw files into duplicates, and the volume
ran out of inodes.

Ingest writes, in **one transaction**, the event row, the session row if the
session is new, an audit entry, and a second row in the table of observations:
the **raw projection** of the event. The event row is the record — payload,
text, times, producer, request id. The projection is a derived convenience: one
searchable row per event, kind `raw`, carrying the same text, so that search
and the timeline do not have to scan payloads. L0010 states the property that
makes the arrangement safe to reason about: the projection can be rebuilt from
`events`, because the two production insertion paths write the same text to
both and raw replay does the same. It is not a proof of equality for every
historical row, and a rebuild would not reproduce projection ids.

Raw custody itself ages gracefully. A nightly job packs each closed UTC day of
envelopes into immutable compressed parts, one inode per part instead of one
per event. Readers see the same bytes in the same order whether a file is flat
or packed. Chapter 3 gives the rules.

## Stage 3 — The raw projection and its search

Your sentence is now an event, a raw observation, and an envelope in custody.
It is searchable because the projection is indexed, and the index depends on
the engine. On SQLite, an FTS5 virtual table follows the observations table
through triggers. On PostgreSQL, the hub's engine, search uses the `pg_search`
extension's BM25 ranking over an index that also carries the columns every read
filters on — kind, project, review state, session, time — so that a filtered
query is answered inside the index instead of re-checked row by row against a
three-million-row heap. Where the extension is absent, PostgreSQL falls back to
a plain text-search column and a weaker ranking, and the health endpoint says
which engine is in use.

That index was a deliberate repair. In the first hours after the L0010 change
went live, an unscoped search over the raw projection took twelve to fifteen
seconds and hit the gateway's timeout, because the filters were not in the
index. The swap to the wider index is an explicit operator step, never a side
effect of deploying (chapter 4), and a repeat search measured in the low
hundreds of milliseconds afterwards.

The projection is also the largest structure in the database, by a wide margin
(L0009 and L0010 measured it at roughly half the store, mostly indexes). That is
what the R2 ruling of L0010 is about, and it is the one place this book must
not run ahead of the code: **the operator authorized an experiment** on whether
the searchable copy of events older than a recent window could be removed while
the events and the raw custody stay untouched. The window would be a stated
number of days by occurred time, with a short grace by received time so that a
freshly backfilled session stays searchable. Nothing of this is implemented.
Production eviction is explicitly conditional on the experiment showing real
savings *and* on a rehydration contract for rebuilding a bounded range on
demand. Until then every projection row is kept.

## Stage 4 — The dated read

Six weeks later someone asks what you said. They have two doors.

The default door is Memoire's read surface. Since L0010 **its default reads
serve raw records only**: the search, context, timeline, observation list and
by-id routes force the raw kind unless the caller adds `include_derived=true`.
A request that asks for a derived kind — a kind other than `raw`, or
`exclude_kind=raw` — without the opt-in is answered with a `400` that names the
opt-in, so that an old client learns why it gets nothing instead of reading an
empty answer. Every read can be bounded to recent history with
`occurred_after`, a Unix epoch in seconds; a malformed or negative bound is a
`400`, not a silent full-history read. Every hit carries its date. The MCP tools
are the agent-facing form: a `days` parameter, default thirty, with `0` lifting
the bound, and a standing paragraph in each tool's description that tells the
agent the hit is dated evidence, not current truth, and that questions of what
was decided or learned belong to Hindsight. Chapter 6 gives the exact shapes.

The second door is Hindsight itself, which is where an agent should start if
the question is "what did we decide". Which brings us to the hand-off.

## Stage 5 — The hand-off

Memoire is the source of record for Hindsight. According to L0010, the one-off
import of the eligible historical conversations into Hindsight completed in
October 2026 by reading Memoire's `events` table read-only, and live sessions
feed Hindsight directly through its own harness hooks, not through Memoire.
Two limits belong next to that sentence, and L0010 states both: the importer
reads a fixed list of adapters and skips trivial, worker and out-of-scope
sessions, and it renders tool output and hook events into conversation
documents rather than keeping them as events. Hindsight's copy is therefore a
reading of the record, never a replacement for it. If Hindsight is lost or
wrong, the record is still here; if Memoire's recent lookup is unavailable,
Hindsight still answers memory questions. Neither depends on the other to run
(chapter 5).

That is the full life of the staging-database sentence. It was captured without
judgment, held in custody before it was even a row, made findable by exact text
and by date, and offered to a memory system that decides what it means. Every
stage up to the last is reversible by rebuild except the first, and the first is
the only one that must never be.

## What was retired, and why

Until 2026-11-01, Memoire did more with that sentence, and the code for it is
still in the tree. A short account is owed, because a reader will meet its
traces.

Memoire used to run a **derivation lane**. A rules provider turned typed
markers into observations; a model provider, given an endpoint you configured,
read windows of the captured transcript and proposed observations in seven
sealed kinds — decision, preference, constraint, task, fact, open question,
gotcha — each born untrusted. A **governance layer** gated what those proposals
could become: a review queue with accept, confirm, reject, dispute, lapse and
rewrite verdicts; a database-enforced rule that nothing may instruct a model
unless a human confirmed it or a trusted system supplied it; and *elevation*,
the deliberate act of making one observation ride every future session start.
An opt-in **session-start injector** printed recent observations and the
elevated ones into a new session. An **embedding lane** added a vector index
and fused it with the full-text ranking. Chapters 2 through 4 of the previous
edition are the long form of all this.

It was retired for a reason that comes straight from the custody thesis. On
2026-10-03 the operator stopped developing Memoire's derivation and adopted
Hindsight as the derived-memory layer. L0010 then measured what Memoire's read
side was actually used for. Over three and a half days of server-side
telemetry, no search, context or timeline request admitted a raw record;
every item returned was a derived observation, and because derivation had been
suspended since about 10 October, none was younger than three weeks. Those
frozen rows were exactly the reads that could contradict Hindsight, which
consolidates newer input. The session-start injector, the only automatic consumer of
derived rows, was opt-in, and where L0010 checked it, it was not switched on. Keeping a second, stale
memory next to the real one was a liability, not a feature.

On 2026-11-01 the operator ruled that Hindsight owns default memory reads and
that Memoire's derivation lane is retired (L0010, R1 = A). What that did, and
did not do, is worth stating exactly:

- **The scheduled lanes are gone from the deployment**: the derive sweep, the
  embedding sweep, the embedding service and its model-endpoint configuration
  no longer exist in the manifest. The checkpoint hook at compaction and
  session end keeps capturing the transcript, and posts a derivation intent
  only if `MEMOIRE_DERIVE_NOTIFY=1` is set.
- **The default read surfaces stopped serving derived rows.** The rows stay in
  the store: this is a visibility change, reversible, and deleting them would be
  a separate reviewed act.
- **The code remains.** The `memoire-derive` command (rules and bring-your-own
  endpoint strategies), the review and elevation routes, the triage tooling and
  the embedded review interface still ship and still work against stored
  derived rows; the interface and the opt-in injector send `include_derived=true`.
  The sweep scripts are still in the operations image, unscheduled. The vector
  code path is still compiled in and stays off unless an embedding endpoint is
  configured. Reviving the lane means redeploying it and reopening the defects
  filed against it; nothing about it runs by itself.

If you read the old edition, or old notes, and see a number of observations,
review states or hybrid search, that is this lane. It is history, and the
frozen rows it left behind are the only place it is still visible.

---

*Next: chapter 3, [Custody & redaction](03-custody-and-redaction.md) — what
"captured without judgment" costs, and what deleting actually does.*
