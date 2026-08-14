# 2. The life of a memory
*Great Book v0.6 edition — 2026-07-25.*

Follow one thing all the way through.

On a Tuesday afternoon you tell an agent, in the middle of a longer message,
that the staging database must never be seeded from the production dump because
the anonymisation job runs after the seed and not before. Nobody writes that
down. Six weeks later, on a different machine, in a different harness, you are
about to be told to seed staging from the production dump — and instead the
model already knows not to.

This chapter is the path between those two moments. It has five stages, and
each one has a rule that only makes sense once you have seen what goes wrong
without it.

## Stage 1 — Capture: take everything, block nothing

When you sent that message, a hook fired. Hooks are the thinnest part of the
system by rule: normalize the input, write a file, exit. They do not call the
model, they do not query the store, and above all **they never block your
session on the service being up.**

The hook wrote a JSON envelope into `$MEMOIRE_HOME/queue/pending/`. That is the
whole write path from a client's point of view. The file name is
`{epoch_ms}-{source_adapter}-{hash8}.json`, chosen so that lexical sort equals
chronological sort equals drain order, and the body carries a real idempotency
key — the filename is not the deduplication mechanism, because filenames are
too easy to collide and too easy to lose.

The service, separately and asynchronously, drains that directory. Draining is
deliberately paranoid, and every one of its rules is a scar from the system
Memoire replaced:

- Claiming a file is an **atomic rename into `queue/inflight/`**, never an
  in-memory flag. A crashed process leaves evidence on disk instead of an
  orphan.
- Completion is **idempotent**: if a file is retried after a crash and the row
  already exists, the drainer treats that as success rather than as a
  duplicate-key failure.
- **Startup reconciliation is mandatory.** Stale files in `inflight/` are swept
  back on boot. The predecessor system's equivalent failure was rows stuck
  forever in a `processing` state that nothing ever revisited.
- There is deliberately **no second, hidden queue table** in the database. The
  queue files are the source of truth until the row commits, and one queue is
  easier to reason about than two.

So: turn the service off for an hour and nothing breaks. Hooks keep dropping
files into `pending/`, disk fills at roughly event-rate times payload-size, and
when the service comes back the drainer reads the directory in lexical order
and ingests everything in the order it happened. Files that fail permanently
land in `queue/failed/` and are kept for `MEMOIRE_FAILED_TTL_DAYS` days —
thirty by default — because a failure you cannot inspect is a failure you
cannot fix.

One thing does happen before the pending file exists, and it is the single
exception to "capture takes everything": **secret-shaped content is redacted at
the source.** The redaction runs inside the queue writer, before the file is
written, so that neither the queue, nor the raw custody copies, nor the
database ever hold the value. That is chapter 3's subject in full, including
why "before the file exists" is the only placement that actually works.

What lands is an **event**, and the event shape is generic on purpose: project,
session id, source adapter, event type, occurred-at, a text field that is
search-visible, and a `payload` object that holds whatever that particular
client natively produces. A Claude Code tool call keeps its `tool_name` and
`tool_input` — inside `payload`, where they belong, rather than as columns that
would make one harness's vocabulary into everyone's schema.

## Stage 2 — Derivation: a model proposes, and only proposes

Raw events are not memory. They are a transcript, and a transcript is
unsearchable in the way that matters: you cannot ask it what you decided. The
derivation stage turns spans of transcript into short, durable, human-readable
statements.

A derivation run takes a **window** — a project plus a session, optionally
bounded by a time range — and produces candidate observations. Two providers
can do the work.

The **rules provider** is deterministic and cheap. It looks for exact typed
markers at the start of a line and turns them into observations directly:

```
Memoire decision:    → kind decision,      state accepted
Memoire preference:  → kind preference,    state accepted
Memoire constraint:  → kind constraint,    state accepted
Memoire task:        → kind task,          state accepted
Memoire fact:        → kind fact,          state accepted
Memoire gotcha:      → kind gotcha,        state accepted
Memoire open_question: → kind open_question, state proposed
```

Look at the right-hand column, because it is the governance thesis in
miniature. **A marker you typed yourself lands `accepted`. Everything a model
proposes lands `proposed`.** The rules provider also carries a handful of other
deterministic rules — a task marker, an open-question detector, a resume-prompt
rule, and one that consumes structured evidence from the sibling project — but
the typed-marker rule is the one worth remembering, because it is the escape
hatch: when something matters and you know it matters, you can put it in memory
in one line without waiting for a model to notice.

The **model provider** is the general case. It sends a window of events to a
local model — an endpoint you run, listed in a registry file at
`$MEMOIRE_HOME/model-endpoints.json` — and asks for observations in a strict
JSON shape. Three things about that registry are deliberate: no hostname is
compiled into the binary, the file names the *environment variable* holding
each endpoint's key rather than the key itself, and the endpoint identity is
recorded on every run so you can tell later which machine produced what.

Between the window and the model sit two guards, and both exist because of a
specific bad afternoon.

**The min-content guard** refuses to spend a provider call on a degenerate
window. A window needs at least three non-trivial events (an event counts as
non-trivial at twenty characters of extracted text or more) and at least 500
characters of content in total. The reason is on the record in the code: a
roughly 270-character window hung a local model to its context deadline twice.
A skipped window writes no run record at all, which means the guard
re-evaluates cheaply on the next sweep instead of poisoning the session with a
permanent failure row.

**The project denylist** excludes projects from model derivation entirely. Both
guards can be overridden — `-ignore-denylist`, `-no-min-content-guard` — but
only by a person typing the override on a command line.

Windows that are too large are not truncated; they are **split**. The sweep
compares the prompt's byte size against a ceiling (70,000 bytes by default,
tunable with `MEMOIRE_SWEEP_MAX_PROMPT_BYTES`) and, if it is over, halves the
window at the median event timestamp and recurses — up to six levels, which is
sixty-four legs for one session. If the split cannot resolve bounds, or if it
hits depth six, the window is skipped *with a named marker in the log*
(`split_depth_exceeded`, `unsplittable_window`) rather than silently dropped.
That naming discipline is the same one that runs through the whole system: a
thing that did not happen must say so.

The parser that reads the model's reply follows a law ruled on 2026-06-13 after
a stricter predecessor threw away good output over one stray field: **lenient
on structure, strict on semantics.** Unknown fields are captured into an
`extra` bag rather than discarded. But a candidate still has to be
semantically fit to become a stored observation — a valid sealed kind, a
present text, a numeric confidence — and one that fails lands in the run's
audit list as rejected-or-unknown, with its extras preserved, rather than being
either silently kept or silently vanished. A run leans toward `partial`-with-
capture over `failed`-with-nothing, because a strict system that returns only
the word "failed" and no value is worse than useless.

The kinds are **sealed**. There are seven, and the set is closed: `decision`,
`preference`, `constraint`, `task`, `fact`, `open_question`, `gotcha`. Sealed
means a model cannot invent an eighth by writing one; adding a kind is a ruled
decision that touches the contract, the prompt, and the schema together. There
were six originally — `gotcha` was added as the seventh after the set was
challenged and the challenge was upheld (D20).

Every run records a `derivation_version` alongside its provider, model,
endpoint, and prompt version. This is the field that makes the whole layer
disposable: because observations know which generation produced them, you can
change the prompt, re-derive history, and compare generations — and you can
throw a bad generation away without touching a single raw event. The prompt has
already been through that: version 2, on 2026-07-10, declared assistant turns
first-class evidence, because version 1 had explicitly told the model that
assistant turns were absent, which suppressed exactly the verdicts and outcomes
that make a session worth remembering.

Runs deduplicate on the hash of their input window, so re-sweeping an unchanged
session is a cheap no-op and the scheduled lanes can enumerate bluntly instead
of cleverly. When you *want* a rerun — an A/B on a new prompt — `-force-rerun`
mints a new run and records the forced provenance in the run's metadata.

## Stage 3 — Governance: nothing a model wrote is trusted

Your constraint about the staging database now exists as an observation of kind
`constraint`, in state `proposed`, carrying provenance: which run produced it,
which events it derived from, which model on which endpoint, at what
confidence, under which `derivation_version`.

**`proposed` is the only state a model-derived observation may be born in.**
This is the invariant the whole governance layer protects. A local model reading
six weeks of your transcripts will produce useful statements and also confident
nonsense, and the design's answer is not to make the model better but to make
its output *structurally untrusted* until something with authority says
otherwise.

Being untrusted is not the same as being useless, and this is where Memoire
diverges from systems that hide unreviewed content. **Proposed observations are
searchable and marked `~`, and every surface an agent actually reads through
returns them by default.** The MCP tools default `include_proposed` to true,
and the Claude injector passes it explicitly, so what a session sees is
everything the system knows with its confidence status visible, rather than
only the part that has been through a queue. The marker is the whole mechanism:
a `~` in a search result means "a model said this and nobody has checked it",
which is exactly as much as you need to calibrate.

The visibility default is per-surface, and the distinction matters if you query
the API yourself. **Plain HTTP defaults the other way**: a read with no
`include_proposed=true` returns `accepted` only. The agent-facing surfaces set
that flag for you; `curl` does not set it for you. One exception is wired into
the store rather than the caller — a read filtered to kind `open_question`
always includes the low-trust band, because an unanswered question that only
becomes visible after review is a question nobody will ever answer. Chapter 6
lists which surface defaults which way.

### The invariant, and where it is enforced

Alongside the review state, each observation carries a **provenance** value and
three capability flags. The provenance says where the statement came from, in a
closed vocabulary of five: `observed` (a concrete captured artifact),
`inferred` (a model's conclusion from observed evidence), `user_confirmed` (a
human validated or corrected it), `imported` (it came from a trusted external
system), and `generated` (produced mid-task with no cited source). The flags say
what it may be used for: `can_instruct` (may be injected as a behavioral rule,
default off), `can_evidence` (may be cited as supporting evidence, default on),
and `needs_confirmation` (surface it for review before use, default off).

Then there is one rule that ties them together, and it is the reason the
governance layer exists at all:

> `can_instruct = 1` requires `provenance IN ('user_confirmed', 'imported')`.

In plain terms: **nothing may instruct a model unless a human confirmed it or a
trusted system supplied it.** A statement a model inferred can be searched,
cited as evidence, and read by you — it can never become a standing
instruction.

What makes this more than a good intention is *where it is enforced*. This
project's usual convention is to enforce invariants in application code. This
one is enforced in the database, by a pair of triggers on insert and update
that abort the write with an explicit message. The design says why the
exception was made: the invariant is the entire point of the layer. Application
enforcement protects you from your own code paths; a trigger protects you from
code paths that do not exist yet, including the ones a future contributor
writes without reading this chapter.

A dated correction hardened it further. On 2026-07-18 (D136) the rule was
tightened at the parser: a derivation run may only ever write `inferred` or
`generated`, and a model that claims `observed` for its own output has that
claim **downgraded to `inferred` on the way in**, fail-open, with the downgrade
counted in the run's metadata. The reason is that the original rule closed the
front door — a model could not write `user_confirmed` — while leaving a side
window open. `observed` means "a concrete artifact says so", and a model
asserting that about its own conclusion is laundering an inference into a fact.
Nothing downstream had to change; the lie is simply corrected at the boundary
and counted, so the frequency is visible rather than assumed.

Every state change is logged. An observation's review log records who did what
and why, which matters because the review vocabulary is richer than
accept/reject: an observation can be **accepted**, **rejected**, **disputed**
(you disagree but do not want it gone), **lapsed** (it aged out unreviewed),
**confirmed**, or **rewritten**. Lapsing happens in bulk, by session or by
expiry sweep, and a lapsed observation is still returned and still marked `~` —
because the tilde means unreviewed, and "nobody got around to it" is a kind of
unreviewed.

## Stage 4 — Review and elevation: three different acts

There is a review UI, embedded in the service at `/ui/`, keyboard-first, built
for going through a queue quickly. **Ignoring it is a supported lifestyle.**
That sentence is in `docs/VOCABULARY.md` as binding law, and it is a design
commitment rather than an apology: if the system only worked for people who
process their review queue, it would work for nobody by the second month.

What the UI gives you is a set of distinct acts that are easy to conflate and
were deliberately separated (`docs/ELEVATION-DESIGN.md`, ruled 2026-07-10):

**Accepting** an observation says "seen, not contested, usable as memory". It
changes review state and nothing else. An accepted observation ranks and reads
as reviewed; it does not thereby appear in front of any model, and — this is
the part that surprises people — **it does not change the observation's
provenance.**

**Confirming** is the stronger verdict: it accepts *and* promotes provenance to
`user_confirmed` in one statement. This is the act that says "I, a human,
vouch for this statement."

Those two used to be one thing. Plain accept quietly promoted provenance, which
meant a fast pass through the review queue was silently minting
human-vouched-for memory at a rate no human was actually vouching at. The split
was ruled on 2026-07-19 (D146), and the design document performs the correction
on itself in place — the old sentence claiming `/accept` carried this meaning
is struck through with the replacement beneath it, which is what an
append-only doctrine looks like when it is working.

**Elevating** sets `can_instruct` and is the act that gives an observation
reach: it is injected into *every* session start, for its project, forever,
until you demote it. It is deliberately expensive. Elevation requires **both**
that the observation is accepted and that its provenance is `user_confirmed` or
`imported` — so in practice you must confirm it, then elevate it, and the code
comment names why: it keeps the **double-human-gesture invariant** real. Two
deliberate acts, two rows in the review log. Attempting to elevate an
unconfirmed observation returns a clean conflict rather than a database error,
because the same rule is enforced twice — once as a friendly check and once as
the trigger of the previous section, which no code path can route around.

Elevation is also **operator-pull only**: nothing queues candidates for you,
nothing badges them, no model may request it. There is no MCP tool that
elevates, and that absence is load-bearing. Demoting is the same act in reverse
and is cheap to perform.

The reason for the asymmetry is a resource argument. Every elevated observation
costs tokens and attention in every future session on that project. Ten of them
is a useful standing brief. A hundred of them is a preamble nobody reads,
including the model. The confirm step exists to make you feel the cost at the
moment you incur it.

Two sharpenings were ruled on 2026-07-29 (memoire:D191), after pull-based
retrieval (memoire:D190) made the notes block the only unconditional knowledge
surface on flagged projects. First, elevation is not a truth tier: confirming
is the strongest statement about *truth* this system offers, and knowledge
that is merely important belongs there — searchable, citable, believed.
Elevation answers a different question: "must this constrain sessions whose
subject has nothing to do with it?" Caveats, gotchas, invariants — yes.
Important facts — no. Second, the injector now says so when the standing
brief overflows: the notes read is capped, and a cap the operator cannot see
is an eviction — older standing orders silently stop riding. An order evicted
this way is **benched** (memoire:D194): still confirmed, still elevated, not
lapsed and not demoted — it simply no longer rides, because the brief is
full and the newest cap gets the seats. When more elevated observations
exist than the block will carry, the block's header warns loudly and names
the benched, because a standing order that silently stopped standing is the
one failure mode this layer must not have.

**Registry promotion** is the third act, and it belongs to a different layer
entirely — moving a piece of knowledge out of the memory store and into a
durable instruction file that ships with a repository. That is a decision about
project doctrine, not about memory, and Memoire's job is to make the candidate
visible, not to perform the promotion.

So: at some point in those six weeks you saw your staging-database constraint
in the review queue and confirmed it — you, personally, vouching that this is
true. Then, because it is exactly the kind of thing that must never be
re-derived at the wrong moment, you elevated it. Two gestures, on purpose.

## Stage 5 — Injection and search: the memory arrives

Six weeks later, on the other machine, a session starts. A session-start hook
runs and emits a short ordered run of blocks, each one conditional on the state
it reports. Two of them carry memory.

The first is **recent context**: semantic observations for the current project,
newest first, raw capture excluded, unreviewed included and marked. It is a
digest of what has been happening here lately, and it is why no session on any
harness starts fully blind.

The second is **operating notes**: the elevated observations, the ones you
confirmed should ride every session. Your staging-database constraint is in
this block.

Everything the injector emits fails open. Nothing it does can block a session
start; every path through it exits 0. A failure fetching recent context does
not suppress operating notes, and the reverse, because a partial memory is
enormously better than none and a hook that dies loudly is worse than a hook
that returns less. That independence has one deliberate bound: the network
reads share a single small aggregate time budget, and once it is spent the
remaining network blocks are skipped rather than allowed to overrun. The whole
injector runs under a hard timeout for the same reason — one harness awaits the
injection hook on its message path, so a stuck injector would hang a real
session, and a memory system that can hang your editor will be uninstalled
within a week regardless of how good its recall is.

*(Edition correction, 2026-08-05: this edition described the injector as two
independent blocks. Those two still carry the memory, but they arrive last: the
script emits operational-state blocks ahead of them, each conditional — a dead
transport, a capture backlog, the pull-mode notice for a project with content
injection off, the project-identity ask when a repo carries no `.memoire` file,
a clock alert, a review backlog, and identity alerts. Nine block types in all,
all fail-open; the network reads among them share the one aggregate budget,
and the local blocks print before any network work begins.)*

Injection is the push side. The pull side is search, and it is available three
ways: over HTTP to anything that can hold a bearer token, over MCP to any
harness with an MCP client, and from a terminal with `curl`. Search is
lexical-first and ranked — FTS5 on SQLite, BM25 on PostgreSQL where the
search extension is available, with a plainer ranking as the fallback.
*(Edition correction, 2026-08-05: the vector seam is no longer dormant — the
hub PG topology runs an optional hybrid lane, MiniLM embeddings fused with
BM25 by reciprocal-rank fusion (memoire:D218), written by the embed worker's
freshness sweep. The standalone SQLite lane stays FTS-only by design, and the
graph tables remain dormant seams that nothing writes to.)*

Search defaults tell you what the system thinks it is for: raw events are
excluded unless you ask for them, and unreviewed observations are included
unless you exclude them. Derived memory first, capture noise on request,
confidence status always visible.

---

That is the life of one memory: captured without judgment, derived into a
proposal, held untrusted but visible, promoted deliberately by a human, and
delivered back at the moment it is needed. Every stage is reversible except the
first, and the first is the only one that must never be.

*Next: chapter 3, [Custody & redaction](03-custody-and-redaction.md) — what
"captured without judgment" costs, and what deleting actually does.*
