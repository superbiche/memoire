# 3. Custody & redaction
*Great Book v0.6 edition — 2026-07-25.*

Chapter 2 said capture takes everything and never filters. This chapter is the
bill for that promise.

If you record every session with every agent across every project, you have
built two things at once. One is a memory. The other is the most complete
record of your working life that exists anywhere, sitting on a disk, in
plaintext, indexed for fast retrieval. Both of those are true at the same time,
and a memory system that only talks about the first one is not being honest
with you.

So this chapter is about three questions with uncomfortable answers. What
happens to a secret that gets captured. What "delete" actually means. And what,
as of this version, is genuinely not solved.

## Custody is total, and that is a decision

The rule is stated in `docs/VOCABULARY.md` as binding law: **event custody is
total; taste is applied at derivation and injection, never by deleting
capture.** Raw events are kept without expiry. There is no TTL on `raw/`, no
rotation, no automatic pruning — nothing in the service or the scripts removes
a raw file on a schedule of its own. *(Edition correction, 2026-08-05: this
edition stated the invariant as "no code path anywhere deletes from raw
custody", which is stronger than the truth and contradicts this chapter's own
purge section. `memoire-purge` does remove matched files from `raw/` — dry-run
by default, double-gated, and quarantine-first by default: an age-encrypted
archive is written before destruction unless the operator passes the explicit
`--no-quarantine` escape (D151). The invariant that actually holds is the one above:
deletion of raw custody is never automatic and never a background policy, only
a deliberate operator act.)*

The reason is that selective capture is a decision made at the worst possible
moment. At capture time nobody knows which session will matter. A filter
written today encodes today's guess about tomorrow's relevance, and when the
guess is wrong the evidence is simply gone, with no record that it ever
existed. Total custody moves every judgment downstream, where it is reversible.

The counterweight is that raw events are **not projected by default**. There is
no full-text index over raw events at all — only observations have an FTS
surface — and the agent-facing surfaces exclude the `raw` kind by default, the
MCP tools and the injector both setting `exclude_kind=raw` unless the caller
clears it. So the raw layer is durable and reachable, but it does not leak into
search results, injection blocks, or model prompts unless something explicitly
asks for it. Custody and exposure are separate axes, and keeping them separate
is what makes total custody survivable.

Note that this fence is about *raw events*, not about unreviewed observations.
Derived observations start in `proposed`, and proposed is not hidden: the MCP
tools and the injector return it by default, marked `~`, while plain HTTP reads
return `accepted` only unless the caller opts in. Chapter 2 gives the
per-surface rule. Low trust is displayed here, not suppressed — the thing
custody keeps out of the projection is the unprocessed raw layer.

## Redaction happens before the file exists

The obvious place to strip secrets from a memory system is at read time, or at
derivation, or in a nightly cleanup job. All three are wrong, and the reason is
mechanical: by the time any of them runs, the secret has already been written
to a queue file, copied into raw custody, and committed to the database. You
are now scrubbing three surfaces and hoping you found them all.

Memoire redacts **at the source**, inside the queue writer, before the pending
file is created. The consequence is stated in the code as plainly as it can be:
neither the queue, nor the raw custody copies, nor the database ever holds the
value. There is nothing to scrub afterwards because nothing was written. The
ruleset is versioned (`redact-v1`), it runs over the event's text, payload, and
metadata, it annotates the metadata with what it hit, and it is on by default
— `MEMOIRE_REDACT=0` turns it off, which is a thing you should have a reason
for.

The rules are ordinary patterns, and knowing which ones exist tells you what
this does and does not protect you from: PEM private-key blocks, JSON web
tokens, AWS access keys, OpenAI keys, GitHub tokens including fine-grained
personal access tokens, GitLab tokens, Slack tokens, bearer values in an
`Authorization` header, and assignments to any variable whose name contains
`SECRET`, `TOKEN`, `PASSWORD`, `API_KEY`, `PRIVATE_KEY`, `ACCESS_KEY`, or
`CREDENTIAL`. That is a good list and it is not a complete one — a credential
with a bespoke shape in a variable named `foo` will pass straight through. This
is a backstop against the common accidents, not a guarantee.

A match is replaced by `[REDACTED:<rule>:<hash8>]`, where the hash is a keyed
HMAC of the secret, using a key generated per-installation at
`~/.memoire/redaction-key` (mode `0600`). That design does more than it looks:
the marker names *which* rule fired, so you can audit the ruleset's behavior;
and because the digest is keyed and stable, the same secret produces the same
marker everywhere, so you can tell whether two events leaked the same
credential or two different ones — without the store holding either.

This law was ruled on 2026-07-02, as ruling 7 of `docs/GOVERNANCE-DESIGN.md`,
and it overturned that document's own proposal. The design had put redaction on
the API side; the operator overruled it, and the driver was not theoretical.
The ruling records it in one line: Claude Code hooks were shipping the contents
of changed secret files into the API *that day*. API-side redaction would have
been a scrub after the fact of something the hook had already written to disk.
The correction was flagged as the highest-priority implementation item in the
design, and it has been live continuously since.

One detail of that mechanism is worth pulling out because it generalizes. Some
producers are contractually required to pre-redact before they hand anything
over — the evidence adapter from the sibling project is one, by the contract in
`docs/LIAISON-EVIDENCE-CONTRACT.md`. If the backstop redactor fires on an event
from that adapter, the producer broke its contract. So the event is stamped as
a contract violation, and the count is surfaced on `GET /v1/health` as
`redaction_contract_violations`. And the stamp is **queue-writer-owned**: a
producer-supplied violation field is stripped unconditionally, redaction on or
off, so a client cannot forge a clean bill of health for itself. A guard whose
subject can set its own result is not a guard.

## Deleting is a visibility act

`DELETE /v1/events/{id}` does not erase anything. It marks the row deleted —
soft-delete columns, with a default `deleted_at_epoch IS NULL` filter applied to
reads — and the row stops being returned. The bytes remain in the database and
in raw custody.

This is called **visibility deletion**, and the project has been deliberate
about never claiming more than that. Soft-delete shipped in the first version
precisely so the distinction would be built in from the start rather than
retrofitted after someone assumed otherwise.

If you actually need the bytes gone, that is a different tool and a deliberate
act. `memoire-purge` performs a hard delete that ignores soft-delete visibility
entirely. What makes it trustworthy is the completeness of its reach, because a
purge that misses one surface is worse than no purge — it produces confidence
without erasure:

- It selects by project, by event ids, or by observation ids.
- It cascades: the raw projection, source-linked observations, the review log,
  source links, and — on a project selector — sessions, derivation runs,
  project roots, and identity alerts.
- It scans **all four file surfaces**: `raw/`, and the queue's `pending/`,
  `inflight/`, and `failed/` directories including their error sidecars. Queue
  envelopes are matched through the drainer's own normalization, so an event
  that has not been ingested yet is still found.
- Matched files are quarantined under age encryption and then removed,
  all-or-error.
- It runs through a phased, crash-safe intent journal (`pre-commit`, then
  `committed`) reconciled against the database, so an interrupted purge can be
  resumed with `--resume-intent` rather than leaving the store in an unknown
  state. A corrupt custody manifest stops a destructive run.
- It writes an audit tombstone (`purge.apply`) and bumps the custody
  generation. The generation bump is what makes purge propagate to mirrors,
  below.
- `--vacuum` is the compaction lever: a WAL truncate checkpoint and a `VACUUM`
  after the hard deletes, with full-text consistency preserved by the schema's
  delete triggers.

Like every destructive tool here, it is dry-run by default, double-gated
(`--apply` plus `MEMOIRE_PURGE_APPLY=yes`), and refuses to run beside a live
`memoire-api` or `memoire-derive` process.

The honest scope of the claim, as recorded in the conformance table: **secret
deletion is solved at application level.** The physical-media caveat — that a
deleted extent on a modern filesystem and SSD is not the same as a destroyed
one — is documented in the purge package and is not something an application
can solve.

## The retroactive sweep, and what it cost

Defense-at-source protects everything captured after 2026-07-02. It does
nothing for what was captured before it. That corpus had to be dealt with
retroactively, and the way it was done is the template for any similar problem.

The tool is `memoire-retro-sweep`. It does not edit files in place. It produces
a **quarantine generation**: a new, redacted generation of the corpus becomes
live, while the originals are archived into an age-encrypted quarantine
tarball, and a JSON report records per-rule hit counts against a reconciliation
invariant that must balance. The point of the generation model is that a sweep
is auditable and reversible right up until the moment the originals are
destroyed, and destroying them is a separate act performed after the archive
has been verified.

It ran on 2026-07-18, first as a copy-run against a duplicate of the store
(D143), then live the same night on an explicit operator go-ahead (D144).
The numbers: **277,430 files scanned, 670 carrying hits, 2,066 individual
redactions, reconciliation balanced**, in about seven minutes of downtime. The
generation was swapped and the originals were shredded after the 2.3 GB archive
verified. That archive is the crypto-shred handle — the originals are
recoverable only while its age key exists.

The derived stores were swept the following night with `memoire-redact-derived`
(D145): 773 rows, 670 events and 103 observations, convergence verified to
zero, about twenty-five minutes of downtime. Both tools take the service down
rather than working around it, which is the right trade for an operation you
run once.

Two things about that sequence deserve to be said rather than smoothed over.

First, a **sequencing drift was retroactively accepted**. The policy required
assistant-turn ingest to wait until the sweep completed. It did not: roughly
115,000 assistant-turn events (78,579 from Claude Code, 29,459 from Codex,
6,258 from Qwen, 661 from opencode) went live before the live sweep ran. The
operator accepted the drift on 2026-07-18, and the reasoning is specific rather
than convenient — defense-at-source had been guarding every one of those new
writes since 2026-07-02, and the copy-run proved zero leak-through after
activation, so the exposure was always and only the pre-2026-07-02 corpus that
the sweep targeted, which the ingest did not change. The same ruling ordered
the live sweep that night.

Second, a **standing caveat** came out of the derived sweep and has not been
closed: `derivation_runs.input_hash` was not recomputed. Historical
re-derivations can therefore mint new runs and duplicate proposed observations
rather than deduplicating against the old ones. Redacting the derived store is
not the same as rebuilding it, and the policy's derived-store *rebuild* step —
re-deriving observations from the redacted generation — still has no dedicated
tooling.

## Mirrors, and the only lever that gives you time-to-gone

Off-machine copies are governed by `docs/MIRROR-PROTOCOL.md`, which is the
contract of record for the remote-custody gate. Its rules, briefly:

Mirrors receive **snapshot artifacts, never the live store** — nothing tails or
replicates the live database, queue, or raw directories. Every artifact set is
**generation-stamped**, in its path and in a plaintext manifest, and an
artifact without a generation stamp must not be synced. **Purge propagates by
replacement**: a purge bumps the generation, and the next snapshot run drops
the old generation from staging while the push removes it from the landing
zone, which never accumulates superseded generations. Sync is **push-only** —
mirrors never write back into any Memoire surface, and the push identity is
locked to its landing zone. Before any restore, manifest hash verification is
mandatory rather than hygienic.

Then there is rule 4, which is where the protocol stops promising and starts
confessing. The original promise was bounded remote retention: N artifact sets,
a computable time-to-gone by file deletion. **On the actual pipeline that
promise is unsatisfiable.** The transport archives rather than deletes:
overwritten files accumulate in an archive directory, deleted files are never
deleted remotely, and nothing prunes the archive. So the remote *file* lifecycle
is unbounded, and the protocol says so in those words instead of restating a
promise it cannot keep.

What carries time-to-gone instead is rule 5, the encryption layer. Every
artifact is age-encrypted to a dedicated snapshot key **before it leaves the
machine** — the transport's own encryption is not enough, because its keys give
Memoire no shred handle. The lever is a **crypto-shred**: mint a new keypair,
replace the recipient, run the snapshot and push (the tooling detects the
recipient change and re-encrypts every staged artifact, and a matching
generation never skips re-encryption), verify the push completed, and only
*then* destroy the old key in both of its custody locations. Every pre-rotation
remote artifact, including the archive copies the pipeline will never delete,
becomes unreadable ciphertext.

The order is the contract, and getting it wrong is catastrophic in the
ordinary direction: destroying the old key before a verified post-rotation push
destroys the live restore chain.

The protocol closes on a section called **Failure honesty**, and it is the part
worth copying into any system you build. Between a purge and the next push,
purged data exists in the landing zone's previous artifact set. Between a purge
and a key rotation, purged data exists on mirrors as ciphertext under a live
key. Rule 7 forces both states to be *named in the purge record*: if the
crypto-shred lever was not exercised, the purge record must state plainly that
pre-purge ciphertext remains on mirrors under a live key, and until that is
journaled the purge is complete locally but **open remotely** and must be
reported that way.

## What is not solved

A release-grade manual that lists only capabilities is a sales document. Here
is the current state of the unfinished work, taken from the conformance table
that maps every clause of the retention and redaction policy (ruled 2026-07-07
as liaison:D97) to its implementation status. That table is drift-guarded by
`scripts/tests/retention-conformance-test.sh`, which asserts every status cell
and additionally probes for the mechanical artifact any "implemented" or
"manual-tooling" row claims — so a row cannot quietly disagree with the code.
It was last verified on 2026-07-18.

- **Cold archives do not exist.** The policy calls for monthly compressed cold
  archives that replace live data. There is no tooling for it. The snapshot
  tooling produces *additive* mirror artifacts; it reads the store and deletes
  nothing, so it is a backup surface and not a retention lever.
- **Archive encryption is therefore also absent**, in the sense that there are
  no cold archives to encrypt. The pattern exists — the retro-sweep's
  quarantine tarball is age-encrypted at creation — but as manual tooling for a
  different job.
- **The non-local custody gate has no enforcement mechanism.** It is policy
  plus a table, and nothing in code stops someone from violating it. *(Edition
  correction, 2026-08-05: this edition also said the gate was held closed by
  the absence of any sync, and that the age-encrypted-artifact condition was
  unmet. The snapshot tooling has since overtaken both: it produces
  age-encrypted, generation-namespaced database and raw-custody artifacts and
  pushes them off-site in three phases; the hub schedules that push nightly
  for raw custody only (`--raw-only` — the database leg is owned by
  CloudNativePG's continuous archiving, as chapter 4 describes). So artifacts
  do leave the machine, encrypted, and the
  age-encrypted-artifact condition is met. What has not changed is the part
  this bullet is about — the gate is still policy plus a table, with no
  mechanism behind it.)*
- **Mirror rules 4 and 5 were amended against the real pipeline.** The
  amendment was flagged as pending in the protocol rather than silently
  rewritten, which is the correct handling. *(Edition correction, 2026-08-05:
  the operator ratified that amendment on 2026-07-20 (D156, ruling 1) —
  remote file-level retention is unsatisfiable on a copy-only pipeline, so
  rotation of the dedicated snapshot key is the purge-propagation lever of
  record. The protocol now carries the ratified status; this edition's
  "pending" was already stale when it was published.)*
- **The derived-store rebuild has no tooling**, and the un-recomputed input
  hashes mean a historical re-derivation would mint duplicates.
- **Physical erasure is out of scope.** Application-level deletion is what is
  claimed; media-level destruction is not.
- **The off-site pipeline has unfinished edges.** Its archive directory has no
  pruning tooling, which is what makes the remote file lifecycle unbounded; a
  second mirror destination is proposed and not wired; and the pipeline's own
  nightly sweep has a known unresolved health fault. None of these break the
  crypto-shred lever, which is precisely why that lever rather than file
  deletion is the thing the protocol leans on.

Two smaller things are worth knowing rather than discovering. The audit log is
**not** purged — purge tombstones survive the data they describe, on purpose, so
that "this was deleted" remains answerable afterwards. And the conformance
table's own "last verified" stamp reads 2026-07-18, which predates some of the
mirror work it evaluates; at least one condition marked unmet may simply be
unreconfirmed rather than genuinely open. A table that tells you when it was
last checked is more useful than one that implies it is always current, but
read the date.

There is also one thing that is solved for this repository's operator and
explicitly **not** shipped. Quarantine key custody here involves a password
vault and a printed paper copy, wired through operator-personal infrastructure
that is not part of Memoire and that this repository never reads. The
conformance table carries a release note saying so directly: if the project is
released, users get the sweep's `--age-recipient` flag and nothing else — key
generation, custody, and backup are explicitly theirs, and the accompanying
documentation must say so. This chapter is that documentation, and this is it
saying so.

---

*Next: chapter 4, [Operating the service](04-operating-the-service.md) — the two
shapes you can run this in, and what each one costs.*
