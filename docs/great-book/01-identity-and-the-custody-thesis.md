# 1. Identity & the custody thesis
*Great Book v0.7 edition — 2026-11-02.*

## The thing you keep re-explaining

Start with the evening you have already had.

You spent two hours with an AI coding agent working out why a migration kept
deadlocking. You tried three things, two were wrong, and the third was right for
a reason that took a while to see. The session ended. The next morning you open
the same repo in the same tool, and it knows none of it. Not the wrong turns,
not the reason, not even that the problem existed. You start by explaining your
own conclusion back to the machine that helped you reach it.

That is the first shape of the pain: **memory that dies with the session.** The
work survives in the commit; the reasoning does not survive anywhere. And the
reasoning is the expensive part — the part you paid two hours of attention for.

The second shape is the re-explaining tax, and it compounds. Every new session
begins with the same preamble: this project uses that convention, we decided
against that library, don't touch the generated directory, the staging database
is the one with the odd port. You have typed those sentences dozens of times.
Each time you type them you are doing unpaid work to restore a context that
existed yesterday and was thrown away. When you forget to type them, the agent
does the thing you already ruled out, confidently, and you catch it in review —
or you don't.

The third shape is the one people notice last, and it is the worst. Run more
than one agent harness — Claude Code in the morning, Codex for a research
sweep, a local model for the volume-heavy grinding — and each of them keeps its
own partial memory in its own walls. Whatever the first one learned is
invisible to the second. Not merged, not stale: absent. So you become the
integration layer. You are the thing carrying what-we-decided from one tool to
the next, by hand, in your head, one paste at a time. The tax is paid in
attention, which is the one resource none of this is manufacturing more of.

And underneath all three sits a question that only becomes urgent once you have
answered the others: **whose record is it?** If the account of how you work
lives inside a vendor's product, then your working context is a feature of
someone else's roadmap. It can be re-priced, re-scoped, or retired. It cannot
be grepped, backed up, or moved. A record you cannot read with `curl` is not
yours in any sense that survives contact with a bad quarter.

## What Memoire is

Memoire is a local-first **capture and custody service** for people who work
with AI agents. It records what those agents and you actually did — prompts,
tool calls, tool output, transcripts — from every harness it has an adapter
for, into one store you own. It keeps every byte it receives. It is the
**source of record**: the place a later question about what really happened is
settled, and the place other systems read from. And it answers one question of
its own, over HTTP, MCP and a shell: *what exactly was said or run, and when?*
— a text search and a timeline over raw records, every hit dated.

What it does not do any more is decide what the record *means*. Until the end
of October 2026 it did: a model endpoint read the record, proposed
"observations", a human reviewed them, and the accepted ones were injected into
future sessions. That was Memoire's answer to the pain above, and chapter 2
tells what became of it. The answer now belongs to **Hindsight**, a separate
agent-memory system that the operator adopted as the memory layer in October
and ruled to be the default for memory questions on 2026-11-01 (L0010). Agents
ask Hindsight what was decided or learned. They ask Memoire what happened, and
when, and to confirm a claim against the record. The split is stated in the
text of Memoire's own MCP tool declarations, because those are what an agent
always reads: a hit from Memoire is dated evidence, not current truth, and
memory questions go to Hindsight.

Four verbs describe the service:

1. **Capture** — thin, fail-open adapters write events to a file queue. If the
   service is down, the files pile up and drain later. Capture never blocks a
   session and never blocks on the service.
2. **Keep** — the service drains the queue, copies each envelope into raw
   custody, and writes the event to the store. Nothing is filtered, summarized
   or aged out. Only a deliberate purge destroys.
3. **Serve** — a search, a timeline and a by-id read over the raw records, with
   an optional recent-time bound and a date on every line.
4. **Hand over** — Hindsight, audit and replay read the same record. The store
   can be rebuilt from raw custody; that is what makes it a record rather than
   a cache.

It is a small Go service with a SQLite or PostgreSQL store behind it, a handful
of command-line tools, an MCP server, and a set of capture scripts. It runs on
a laptop. It also runs as a household hub on a Kubernetes cluster, which is how
the author runs it — but the laptop shape is the honest one and is never
allowed to become the second-class one (chapter 4).

## The custody thesis

Three commitments hold the design together. They are not features; they are the
constraints every other decision was judged against, and each of them costs
something real.

**Sovereignty: the record is the operator's.** The store is a file or a
database you own on a machine you control. There is no account, no hosted tier,
no telemetry. This is a binding constraint rather than a slogan, and the sharp
end of it is the next commitment.

**Foreign-client read parity is a hard requirement, not a feature.** A read
surface that only works from one vendor's client is not sufficient — it
reproduces the silo it exists to abolish, just with nicer branding. So every
read path is plain HTTP with a bearer token, curl-shaped, documented, and
verified from clients that have nothing to do with the harness that wrote the
data. This requirement is why the write contract is a generic event and not a
Claude-shaped one. A research pass on 2026-05-21, before any code existed,
changed the planned v0.1 schema for exactly this reason: raw ingestion became
event-shaped (`POST /v1/events`) with each client's native payload preserved
inside a JSON column, instead of promoting one harness's hook vocabulary
(`tool_name`, `tool_input`) into the public write contract.

**Capture-first: nothing downstream may delete capture.** Capture is total and
unfiltered. The raw event log takes everything, including the noise, including
the failed attempts, including the sessions that went nowhere. Whatever taste a
system applies to that record — what is worth remembering, what is worth
showing a model — is applied *by the layer that reads it*, and that layer is
not allowed to reach back and remove the record. The reason is that selective
capture is a judgment made at the worst possible moment: when nobody knows yet
what will matter. The binding vocabulary of the project used to word this as
"taste is applied at derivation and injection, never by deleting capture"; with
derivation gone, the part that survives intact is the clause after the comma.

The practical consequence is a discipline you will see enforced in
uncomfortable places later in this book. An ingest path that meets an event
shape it does not recognize does not discard it. Deletion is visibility
deletion by default: the row stops being returned, the bytes stay in custody
until a purge tool is run deliberately. And the raw projection — the searchable
copy of each event — is explicitly *rebuildable from the events table*, which is
why a future decision to keep that copy small is possible without touching
custody (chapter 4 says where that stands). Chapter 3 is where the honest
limits of total custody are stated.

## What Memoire is not

It is not the memory layer. That is the largest change of this edition and the
easiest to miss, because the code for the old layer is still in the tree
(chapter 2). Hindsight is the memory agents consult; Memoire does not run it,
ship it, or depend on it at runtime.

It is not a chat product, and it has no opinion about how you talk to models.
It is not a RAG pipeline: retrieval is lexical — full-text search with BM25
ranking on PostgreSQL where the search extension is present, FTS5 on SQLite.
There is a vector code path in the tree, but nothing deploys the embedding
service it needs, and it is off (chapter 2). It is not an autonomous agent — no
background process rewrites, merges or forgets your records on its own
initiative. And it is not a team system: the operator is the only principal the
design assumes, and multi-principal authorization was deliberately kept out of
scope rather than half-built.

It is also not the only thing in its own ecosystem. Memoire is the capture and
custody strand of a three-project braid: **Liaison** carries the shared
instruction layer, the skill library, and the model-routing and evidence
machinery; **Conclave** is the multi-model dispatcher; Memoire keeps the
record. They are siblings under contract, not one repository with
subdirectories — nothing in Memoire's product runtime depends on either of the
others (D169, D171). Chapter 5 is entirely about those seams and the others
Memoire has.

## The name, and the layers under it

The project was named on 2026-05-21, in the same pass that fixed the v0.1
scope: **Memoire** in prose, `memoire` in technical identifiers. The name was
chosen to span more than what was being built that week: short-term and
session memory, working memory, long-term memory, sensitive memory, suppressed
or forgotten memory, vector memory, graph memory, reconstructed memory. The
recorded intent was that those layers could attach under one name instead of
spawning a new product each time.

Read that as a promise about scope discipline, not a roadmap, and read what
happened to it honestly: the layers above the record were built and, one by
one, either retired or handed to another system. What ships is the raw store,
its read surface, and custody. The schema still carries tables for the retired
derivation layer — derivation runs, review logs, triage suggestions — and the
migration directory holds two early sketches, for a vector and a graph layer,
that the migrator never applies (they register no schema version). The
derivation tables stay because the migration rule is additive-only
(chapter 4), and because deleting the rows of a retired layer would be a
separate, reviewed act.

## Editions, versions, and how to read this book

Memoire versions itself: a `VERSION` file, git tags, pre-release `0.x` (D172).
What the version number covers is the system contract — the HTTP and MCP
surface, `schema_version`, the capture adapter contracts, and the deployment
target. A bump means one of those changed in a way a client would notice.

The history was reconstructed after the fact, from the commit record: v0.1 the
raw store, v0.2 derivation, v0.3 governance and redaction, v0.4 product surface
and multi-harness parity, v0.5 identity, evidence and custody, v0.6 the hub era
(the service and its PostgreSQL database moved onto a Kubernetes hub in July
2026), and v0.7 the first public release (D176). Inside the 0.7 line the
purpose of the system changed — L0010 — which is why this edition is a rewrite
and not a patch to v0.6.

This book is the written layer, and one edition is written per version. It is
never generated. Chapter 2 follows one event from capture to its hand-off.
Chapter 3 is about custody, redaction, and what is not solved. Chapter 4 is for
the stranger deploying their own instance. Chapter 5 is the seams. Chapter 6 is
the surface reference — endpoints, tools and commands, verified against the
code rather than against prose.

---

*Next: chapter 2, [The life of a captured event](02-the-life-of-a-captured-event.md) —
one event, from a hook firing to a dated line in front of a model.*
