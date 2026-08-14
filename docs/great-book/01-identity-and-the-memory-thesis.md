# 1. Identity & the memory thesis
*Great Book v0.6 edition — 2026-07-25.*

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
answered the others: **whose memory is it?** If the record of how you think
lives inside a vendor's product, then your working context is a feature of
someone else's roadmap. It can be re-priced, re-scoped, or retired. It cannot
be grepped, backed up, or moved. A memory you cannot read with `curl` is not
yours in any sense that survives contact with a bad quarter.

## What Memoire is

Memoire is a local-first memory service for people who work with AI agents. It
captures everything those agents and you actually did — prompts, tool calls,
transcripts — into one store you own; it derives durable, human-readable
observations from that raw record using a model endpoint you provide; it lets
you review those observations and mark the rare ones that should shape every
future session; and it serves the whole thing back to any client that can make
an HTTP request.

*(Edition correction, 2026-08-05: this edition said derivation ran on "a model
you run", here and in the verb list below. Truer: derivation takes a model
endpoint you point it at, with its data boundary declared explicitly on every
run — there is no default: `local`, or `external`, which additionally requires
a per-run opt-in and a key source before any raw window leaves the machine
(the BYOK provider, D233). The endpoint must
speak one of exactly two dialects, OpenAI-compatible `v1` or Anthropic; that
set is closed and widens only by operator ruling.)*

The five verbs, which are also the shape of the next chapter:

1. **Capture** — thin, fail-open adapters write events to a file queue. If the
   service is down, the files pile up and drain later. Capture never blocks a
   session and never blocks on the service.
2. **Derive** — the model endpoint you configure reads windows of raw events
   and proposes observations in seven sealed kinds. Everything it produces
   lands as `proposed`. Nothing a model wrote becomes trusted memory on its
   own.
3. **Review** — a keyboard-first web queue where you accept or reject. Ignoring
   this queue is a supported lifestyle; unreviewed observations are still
   searchable, marked `~`.
4. **Elevate** — the rare, deliberate act of making one observation ride every
   session start. Operator-pull only, behind a confirm step, never queued and
   never badged.
5. **Inject & search** — a session-start context block on every harness that
   supports one, plus a search surface reachable over MCP, over HTTP, and from a
   shell.

It is a small Go service with a SQLite or PostgreSQL store behind it, an
embedded review UI, a handful of command-line tools, and a set of capture
scripts. It runs on a laptop. It also runs as a household hub on a Kubernetes
cluster, which is how the author runs it — but the laptop shape is the honest
one and is never allowed to become the second-class one.

## The memory thesis

Three commitments hold the design together. They are not features; they are the
constraints every other decision was judged against, and each of them costs
something real.

**Sovereignty: the memory is the operator's.** The store is a file you own on a
machine you control. There is no account, no hosted tier, no telemetry. This is
a binding constraint rather than a slogan, and the sharp end of it is the next
commitment.

**Foreign-client read parity is a hard requirement, not a feature.** A memory
surface that only works from one vendor's client is not sufficient — it
reproduces the silo it exists to abolish, just with nicer branding. So every
read path is plain HTTP with a bearer token, curl-shaped, documented, and
verified from clients that have nothing to do with the harness that wrote the
data. This requirement is why the write contract is a generic event and not a
Claude-shaped one. A research pass on 2026-05-21, before any code existed,
changed the planned v0.1 schema for exactly this reason: raw ingestion became
event-shaped (`POST /v1/events`) with each client's native payload preserved
inside a JSON column, instead of promoting one harness's hook vocabulary
(`tool_name`, `tool_input`) into the public write contract. The correction is
recorded in `docs/KICKOFF-PREP.md`, and it is the earliest example of the
parity requirement outranking convenience.

**Capture-first: taste is applied at derivation and injection, never by
deleting capture.** This is the binding sentence of the whole system, and it
lives verbatim in `docs/VOCABULARY.md`, the binding vocabulary of record.
Capture is total and unfiltered. The
raw event log takes everything, including the noise, including the failed
attempts, including the sessions that went nowhere. Filtering happens
downstream — a derivation pass decides what is worth turning into an
observation, an injection decides what is worth putting in front of a model,
and a search decides what is worth returning. What none of those layers is
allowed to do is reach back and remove the raw record, because the moment
capture becomes selective, it becomes a judgment made by whoever wrote the
filter, at a time when nobody knew yet what would matter.

The practical consequence is a discipline you will see enforced in
uncomfortable places later in this book. A parser that meets an event shape it
does not recognize does not discard it — the ruled law is "lenient on
structure, strict on semantics", and it exists because an early strict parser
silently dropped events it could have kept. Deletion is visibility deletion by
default: the row stops being returned, the bytes stay in custody until a purge
tool is run deliberately. That is chapter 3's subject, and chapter 3 is also
where the honest limits of the claim are stated.

## What Memoire is not

It is not a chat product, and it has no opinion about how you talk to models.
It is not a RAG pipeline: retrieval is lexical-first (FTS/BM25), and the
graph layer is a named seam in the schema rather than a working feature.
*(Edition correction, 2026-08-05: the vector seam is no longer empty — the
hub topology runs an optional hybrid retrieval lane, MiniLM embeddings fused
with BM25 by reciprocal-rank fusion, memoire:D218; the standalone SQLite lane
stays FTS-only by design.)* It is not an autonomous memory agent — no
background process rewrites, merges, or forgets your observations on its own
initiative. And it is not a team system: the operator is the only principal the
design assumes, and multi-principal authorization was deliberately kept out of
scope rather than half-built.

It is also not the only thing in its own ecosystem, which is worth stating
early because it explains the shape of several chapters. Memoire is the
shared-memory strand of a three-project braid: **Liaison** carries the shared
instruction layer, the skill library, and the model-routing and evidence
machinery; **Conclave** is the multi-model dispatcher; Memoire is the memory.
They are siblings under contract, not one repository with subdirectories —
nothing in Memoire's product runtime depends on either of the others (D169,
D171). Where they meet, this book points at the contract documents rather than
restating them, and chapter 5 is entirely about those seams.

## The name, and the layers under it

The project was named on 2026-05-21, in the same pass that fixed the v0.1
scope: **Memoire** in prose, `memoire` in technical identifiers. The name was
chosen to span more than what was being built that week. The first slice was a
raw durable store with full-text search and an HTTP read surface; the layers
sketched behind it were short-term and session memory, working memory,
long-term memory, sensitive memory, suppressed or forgotten memory, vector
memory, graph memory, and reconstructed memory. The decision recorded at the
time was that all of those should live under one name rather than spawning a
new product every time a layer arrives.

Read that as a promise about scope discipline, not a roadmap. What ships today
is the raw store, the derivation layer, the governance layer, the read surface,
and — on the hub's PostgreSQL topology only — vector memory, as the optional
hybrid retrieval lane named earlier in this chapter. The rest are seams —
places in the schema and the API where a later layer can attach without a
migration that rewrites history; graph memory is the clearest of them, tables
present and nothing writing to them. Additive-only migrations have been the
rule since day one, precisely so that the promise stays cheap to keep.
*(Edition correction, 2026-08-05: this edition counted every layer past the
read surface as a dormant seam, which its own earlier correction already
contradicted — the vector lane ships (D218). The exception is written into the
sentence above rather than left to the reader to reconcile.)*

## Editions, versions, and how to read this book

Memoire versions itself: a `VERSION` file, git tags, pre-release `0.x`
(D172). What the version number covers is the system contract — the HTTP and
MCP surface, `schema_version`, the capture adapter contracts, and the
deployment target. A bump means one of those changed in a way a client would
notice.

The version history was reconstructed after the fact, from the commit record
and the working journals rather than from tags that were never cut: v0.1 the
raw store, v0.2 derivation, v0.3 governance and redaction, v0.4 product surface
and multi-harness parity, v0.5 identity, evidence and custody, and v0.6 the hub
era, which is the current one (D176, proposed at the time of writing and
pending the operator's ratification). *(Edition correction, 2026-08-05: the
operator confirmed that back-numbering on 2026-07-28, and the `v0.6.0` tag it
was waiting on has been cut.)* The boundary for v0.6 is a migration: the
service and its database moved onto a Kubernetes hub with a CloudNativePG
cluster behind it, finishing on 2026-07-24. That migration is what makes this
edition's snapshot coherent, and D172 makes it the version boundary.

This book is the written layer, and one edition is written per version. It is
never generated. It is also deliberately not three other things:

- It is not current state. "Where are we right now" lives in the generated
  digest under `docs/state/`, which is regenerated on every journal mutation.
- It is not the agent-facing precondition. Agents performing a gesture read
  their view under `docs/views/`; reading this book does not satisfy that.
- It is not the audit trail. `DECISIONS.md` and `BURN-DECISIONS.md` are the raw
  custody, and this book cites them by D-number without ever replacing them.

Citations follow the ecosystem's convention (D170): a bare `D<n>` is a Memoire
journal entry, and a cross-repo citation carries its prefix, as in
liaison:D281. Paths are given relative to the repository root.

The chapters ahead assume nothing about the author's own setup. Chapter 2
follows one event from capture through to the moment it shapes a session.
Chapter 3 is about custody, redaction, and what is honestly not solved yet.
Chapter 4 is for the stranger deploying their own instance, in either of the
two supported shapes. Chapter 5 is the seams. Chapter 6 is the surface
reference — endpoints, tools, and commands, verified against the code rather
than against the prose.

---

*Next: chapter 2, [The life of a memory](02-the-life-of-a-memory.md) — one
event, from a hook firing to a session start weeks later.*
