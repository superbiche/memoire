# 3. Custody & redaction
*Great Book v0.7 edition — 2026-11-02.*

Chapter 2 said capture takes everything and never filters. This chapter is the
bill for that promise.

If you record every session with every agent across every project, you have
built two things at once. One is a record you can ask questions of. The other
is the most complete account of your working life that exists anywhere, sitting
on a disk, in plaintext, indexed for fast retrieval. Both of those are true at
the same time, and a system that only talks about the first one is not being
honest with you.

So this chapter is about three questions with uncomfortable answers. What
happens to a secret that gets captured. What "delete" actually means. And what,
as of this version, is genuinely not solved.

## Custody is total, and that is a decision

The rule is binding in the project's vocabulary: **event custody is total.**
Raw events are kept without expiry. There is no TTL on `raw/`, no rotation, no
automatic pruning — nothing in the service or the scripts destroys a raw record
on a schedule of its own. The one tool that removes matched files from `raw/`
is `memoire-purge`, which is dry-run by default, double-gated, and
quarantine-first by default: an age-encrypted archive is written before
destruction unless the operator passes the explicit `--no-quarantine` escape
(D151). Deletion of custody is never automatic and never a background policy;
it is a deliberate operator act.

The reason is that selective capture is a decision made at the worst possible
moment. At capture time nobody knows which session will matter. A filter written
today encodes today's guess about tomorrow's relevance, and when the guess is
wrong the evidence is simply gone, with no record that it ever existed. Total
custody moves every judgment downstream, where it is reversible.

Custody has three layers, and it helps to name them:

- **`raw/`** — one file per first-seen envelope, exactly as the queue held it
  after redaction. This is the replay source: a store can be rebuilt from it.
  The drainer writes the raw copy *before* the event row, and skips the copy
  only for an exact repeat of an already-stored event (L0007), so first write
  wins and a stored event always has custody behind it.
- **The `events` table** — the event as a row: producer, project, session,
  type, times, text and payload. It keeps everything, forever, unless purged.
- **The raw projection** — one `kind=raw` observation per event, written in
  the same transaction, carrying the same text, so reads and search do not scan
  payloads. This layer is *not* custody. It is a convenience copy that can be
  rebuilt from `events`, which is why a future decision to keep it small is
  possible without touching the two layers above it (chapter 2 says where that
  stands: an authorized experiment, not behaviour).

Custody and exposure are separate axes. The default read surfaces return the
raw projection — exact text, tool output, ids, dates — bounded by `days` or
`occurred_after` if the caller asks. What they never do by default is return
derived rows, which are a different thing and are retired (chapter 2).

### Packs

Flat files do not scale: the hub once ran out of inodes with 3.28 million files
in `raw/`, one per POST (L0006). `memoire-raw-pack` packs each closed UTC day
of envelopes into immutable compressed parts named `pack-YYYY-MM-DD.NNN.tar.zst`
inside `raw/`, at most 200,000 members each, after a two-day grace. The rules
are the interesting part:

- Parts are **immutable**. A late-arriving envelope for an already-packed day
  goes into the day's next part; only purge and an explicit rollback
  (`--unpack`) remove a part name, and both advance the custody generation
  first.
- Every reader — replay, purge, the retroactive sweep, the snapshot — goes
  through one walker that yields flat and packed files in one global name order,
  each name once, with the same bytes and times, and refuses duplicates or
  conflicts. `memoire-raw-pack --verify` prints a digest of the whole store that
  is identical before and after packing.
- Writes are verify-then-rename from a staging area *beside* `raw/`, never
  inside it, and flat files are deleted only after the part is published and
  checked.
- Parts are plaintext compressed archives on the custody volume, which is
  encrypted at rest on the hub. That deviates from the original policy of
  age-encrypting archives at creation, by ruling (L0006): every read path would
  have needed the key, which either sits on the same volume (no shred value) or
  makes every purge wait for an off-machine key. The snapshot and quarantine
  copies are age-encrypted.
- The packer, purge, replay, the sweep and the snapshot share one custody lock,
  and the packer refuses to run while a purge is half-finished.

## Redaction happens before the file exists

The obvious place to strip secrets from a record is at read time, or in a
nightly cleanup job. Both are wrong, and the reason is mechanical: by the time
either runs, the secret has already been written to a queue file, copied into
raw custody, and committed to the database. You are now scrubbing three
surfaces and hoping you found them all.

Memoire redacts **at the source**, inside the queue writer, before the pending
file is created. The consequence is stated in the code as plainly as it can be:
neither the queue, nor the raw custody copies, nor the database ever holds the
value. There is nothing to scrub afterwards because nothing was written.

The ruleset is no longer Memoire's own. It is the shared fleet contract
`redact-v7`, implemented once in a separate public Go module, Caviar, which
Memoire's `internal/redact` package only forwards to; a rule change is a Caviar
release and a version bump here, never an edit in this tree. The redactor runs
over the event's text, payload and metadata, using explicit patterns and
bounded capture procedures — no entropy guessing, no model calls. It annotates
the envelope's metadata with what it hit and the rules version, and it is on by
default: `MEMOIRE_REDACT=0` turns it off, which is a thing you should have a
reason for.

It **fails closed**. If the scrub cannot finish — a document nested past the
depth limit — or the redaction key cannot be loaded, the write is refused and
nothing reaches the queue. A redactor that falls back to writing the original
when it errs protects nobody on the day it errs.

The rules are ordinary patterns, and knowing the families tells you what this
does and does not protect you from: private-key blocks, JSON web tokens, cloud
and API keys with recognizable prefixes, source-host and chat-service tokens,
bearer values in an `Authorization` header, and assignments to any variable or
key whose name says it is a secret, token, password, key or credential. That is
a good list and it is not a complete one — a credential with a bespoke shape in
a variable named `foo` will pass straight through. This is a backstop against
the common accidents, not a guarantee, and Caviar says the same of itself. The
exact table is in Caviar, not here.

A match is replaced by `[REDACTED:<rule>:<hash8>]`, where the hash is the first
eight hex characters of an HMAC-SHA256 of the secret, keyed by a per-installation
key generated at `$MEMOIRE_HOME/redaction-key` (mode `0600`). That does more than
it looks: the marker names *which* rule fired, so you can audit the ruleset's
behavior; and because the digest is keyed and stable, the same secret produces
the same marker everywhere, so you can tell whether two events leaked the same
credential or two different ones — without the store holding either.

This law was ruled on 2026-07-02, and it overturned the design document's own
proposal. The design had put redaction on the API side; the operator overruled
it, and the driver was not theoretical: Claude Code hooks were shipping the
contents of changed secret files into the API *that day*. API-side redaction
would have been a scrub after the fact of something the hook had already
written to disk.

One detail generalizes. Some producers are contractually required to pre-redact
before they hand anything over — the evidence adapter from the sibling project
is one (chapter 5). If the backstop redactor fires on an event from that
adapter, the producer broke its contract. So the event is stamped as a contract
violation, and the count is surfaced on `GET /v1/health` as
`redaction_contract_violations`. The stamp is **queue-writer-owned**: a
producer-supplied violation field is stripped unconditionally, redaction on or
off, so a client cannot forge a clean bill of health for itself. A guard whose
subject can set its own result is not a guard.

The envelope also records which rules version stamped it. Older stock — an
earlier `redact-v*` stamp, or none — is where to start looking when a rules
bump means the stored corpus should be re-swept. `memoire-redact-derived` is
that tool. Despite its name it has nothing to do with derived observations: it
re-applies the current ruleset in place to the *stored rows* that were derived
from raw custody — events, the observations table including the raw projection,
and the audit-bearing text columns — in one repeatable-read transaction on
PostgreSQL, dry-run by default, double-gated by `--apply` and
`MEMOIRE_REDACT_DERIVED_APPLY=yes`. It does not rewrite `raw/` files; that is
the retroactive sweep's job, below.

## Deleting is a visibility act

`DELETE /v1/events/{id}` does not erase anything. It marks the event and its
projection row deleted — soft-delete columns, with a default
`deleted_at_epoch IS NULL` filter applied to reads — and the row stops being
returned. The bytes remain in the database and in raw custody. The route needs
`admin` scope: every capture hook carries `write`, and a leaked hook token must
not be able to hide events.

This is called **visibility deletion**, and the project has been deliberate
about never claiming more than that. If you actually need the bytes gone, that
is a different tool and a deliberate act. `memoire-purge` performs a hard
delete that ignores soft-delete visibility. What makes it trustworthy is the
completeness of its reach, because a purge that misses one surface is worse than
no purge — it produces confidence without erasure:

- It selects by project, by event ids, or by observation ids.
- It cascades: the raw projection, source-linked observations, the review log,
  source links, and — on a project selector — sessions, derivation runs,
  project roots, and identity alerts.
- It scans **all four file surfaces**: `raw/` (flat and packed), and the queue's
  `pending/`, `inflight/` and `failed/` directories including their error
  sidecars. Queue envelopes are matched through the drainer's own normalization,
  so an event that has not been ingested yet is still found.
- Matched files are quarantined under age encryption and then removed,
  all-or-error.
- It runs through a phased, crash-safe intent journal reconciled against the
  database, so an interrupted purge can be resumed with `--resume-intent`
  rather than leaving the store in an unknown state. A corrupt custody manifest
  stops a destructive run.
- It writes an audit tombstone and bumps the custody generation, which is what
  makes purge propagate to mirrors, below.

Like every destructive tool here, it is dry-run by default and double-gated
(`--apply` plus `MEMOIRE_PURGE_APPLY=yes`). On SQLite it refuses to run beside a
live `memoire-api` or `memoire-derive` process. On the PostgreSQL hub a process
check cannot see other pods, so it refuses instead without
`--i-have-quiesced-the-hub`, the operator's attestation that the hub is scaled
down and its store jobs suspended; the tool never scales anything itself.

The honest scope of the claim: **secret deletion is solved at application
level.** Two physical caveats ride with it. On SQLite, `--vacuum` compacts the
file; on PostgreSQL the tool does not vacuum (autovacuum reclaims dead tuples),
and database backups and write-ahead-log archives keep the deleted data until
their own retention expires. And a deleted extent on a modern filesystem or SSD
is not a destroyed one. Neither is something an application can solve; both are
named in the tool's own report.

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
(D143), then live the same night on an explicit operator go-ahead (D144). The
numbers: **277,430 files scanned, 670 carrying hits, 2,066 individual
redactions, reconciliation balanced**, in about seven minutes of downtime. The
generation was swapped and the originals were shredded after the 2.3 GB archive
verified. That archive is the crypto-shred handle — the originals are
recoverable only while its age key exists. The stored database rows were swept
the following night with `memoire-redact-derived` (D145): 773 rows, convergence
verified to zero.

A **sequencing drift** was retroactively accepted. The policy required
assistant-turn ingest to wait until the sweep completed; roughly 115,000
assistant-turn events went live before the live sweep ran. The operator accepted
the drift on 2026-07-18, and the reasoning is specific rather than convenient —
defense-at-source had been guarding every one of those new writes since
2026-07-02, and the copy-run proved zero leak-through after activation, so the
exposure was always and only the pre-2026-07-02 corpus that the sweep targeted.

## Mirrors, and the only lever that gives you time-to-gone

Off-machine copies are governed by the project's mirror protocol, the contract
of record for the remote-custody gate. Its rules, briefly:

Mirrors receive **snapshot artifacts, never the live store** — nothing tails or
replicates the live database, queue, or raw directories. Every artifact set is
**generation-stamped**, in its path and in a plaintext manifest, and an artifact
without a generation stamp must not be synced. **Purge propagates by
replacement**: a purge bumps the generation, and the next snapshot run drops the
old generation from staging while the push removes it from the landing zone.
Sync is **push-only** — mirrors never write back into any Memoire surface.
Before any restore, manifest hash verification is mandatory rather than
hygienic.

Then there is the rule where the protocol stops promising and starts
confessing. The original promise was bounded remote retention: N artifact sets,
a computable time-to-gone by file deletion. **On the actual pipeline that
promise is unsatisfiable.** The transport archives rather than deletes:
overwritten files accumulate in an archive directory, deleted files are never
deleted remotely, and nothing prunes the archive. So the remote *file*
lifecycle is unbounded, and the protocol says so instead of restating a promise
it cannot keep (ratified at D156).

What carries time-to-gone instead is the encryption layer. Every artifact is
age-encrypted to a dedicated snapshot key **before it leaves the machine**. The
lever is a **crypto-shred**: mint a new keypair, replace the recipient, run the
snapshot and push (the tooling detects the recipient change and re-encrypts
every staged artifact), verify the push completed, and only *then* destroy the
old key in both of its custody locations. Every pre-rotation remote artifact,
including the archive copies the pipeline will never delete, becomes
unreadable ciphertext. The order is the contract: destroying the old key before
a verified post-rotation push destroys the live restore chain.

On the hub, the snapshot job runs `--push --raw-only`. It handles raw custody,
which no database operator backs up for you. The database leg is not the
snapshot's job there: CloudNativePG archives continuously with point-in-time
recovery, and two backup systems for the same data is two things to keep correct
(chapter 4).

The protocol closes on **failure honesty**. Between a purge and the next push,
purged data exists in the landing zone's previous artifact set. Between a purge
and a key rotation, purged data exists on mirrors as ciphertext under a live
key. If the crypto-shred lever was not exercised, the purge record must say so
plainly, and until that is journaled the purge is complete locally but **open
remotely** and must be reported that way.

## What is not solved

A manual that lists only capabilities is a sales document. Here is the current
state of the unfinished work, drawn from the retention and redaction
conformance table (which maps every clause of the ruled policy, liaison:D97, to
its status) and from the code.

- **Cold archives exist, in a different shape than the policy.** The policy
  called for monthly, age-encrypted archives at creation. What exists is daily
  plaintext compressed parts on an encrypted volume (L0006). Nothing is
  pruned, by design.
- **The non-local custody gate has no enforcement mechanism.** It is policy
  plus a table; nothing in code stops someone from violating it. What does exist
  is the encrypted, generation-stamped snapshot pipeline, so the
  encrypted-artifact condition is met.
- **The remote file lifecycle is unbounded.** The off-site transport never
  deletes, and its archive directory has no pruning tooling. Crypto-shred is
  therefore the only time-to-gone lever, and a second mirror destination is
  proposed and not wired.
- **PostgreSQL backups outlive a purge.** The purge tool deletes live rows; the
  continuous archive and base backups keep them until their retention expires.
- **Physical erasure is out of scope.** Application-level deletion is what is
  claimed; media-level destruction is not.
- **The conformance table lags.** It was last verified on 2026-08-23 and
  predates the packs, the hub's snapshot push and the retirement of the
  derivation lane; at least one row still describes a gate as held by the
  absence of any sync. Treat a status cell as a claim with a date, not as the
  current truth.
- **The window on the raw projection is an experiment, not a behaviour.** L0010
  authorizes measuring whether projection rows (and their indexes) for old
  events can be removed while `events` and `raw/` stay whole, with a rehydration
  step to rebuild a range on demand. Until the experiment reports and a
  rehydration contract exists, nothing is evicted. This chapter will be
  revised after it.

Two smaller things are worth knowing rather than discovering. The audit log is
**not** purged — purge tombstones survive the data they describe, on purpose, so
that "this was deleted" remains answerable afterwards. And one thing is solved
for this repository's operator and explicitly **not** shipped: quarantine key
custody here involves a password vault and a printed paper copy, wired through
operator-personal infrastructure that is not part of Memoire. If you run the
retroactive sweep or purge with quarantine, you get the `--age-recipient` flag
and nothing else — key generation, custody and backup are yours.

---

*Next: chapter 4, [Operating the service](04-operating-the-service.md) — the two
shapes you can run this in, and what each one costs.*
