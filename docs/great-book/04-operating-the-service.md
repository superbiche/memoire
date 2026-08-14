# 4. Operating the service
*Great Book v0.6 edition — 2026-07-25.*

This chapter is for you running your own instance, not for reading over
someone else's shoulder. The reference deployment described at the end is one
person's homelab, and it is included because a concrete example is worth more
than an abstract one — not because you should copy it.

## Two shapes, one binary

There is no "server edition" and no "local edition". **The deployment modes
differ only in configuration, never in code paths**, and that constraint is
the load-bearing one: the moment a hub-only code path exists, the local mode
becomes the untested one, and the untested one is the one that breaks for
every new user.

The ratified contract (`docs/R10-DESIGN.md`, 2026-07-22) is three modes across
two database engines:

| | standalone-SQLite | standalone-PostgreSQL | hub client |
|---|---|---|---|
| API + drainer | local | local | on the hub |
| store | local file | a DSN | the hub's, either engine |
| capture hooks | local queue → local API | same | local queue → `memoire-forward` → hub |
| reads, injection, MCP | localhost API | localhost API | hub URL + bearer token |
| derivation | local, beside the API | same | hub-side only |
| model credentials | local | local | **none on the client** |
| database credentials | none, or a DSN | a DSN | **none on the client** |

The operator directive behind this is on the record verbatim: remote-hub-service
is what the author needs, and local-SQLite is what could make the thing
adoption-friendly. Both halves are load-bearing, and the second is protected
mechanically — see "The adoption guard" below.

"Hub" is not special code. **A hub is a standalone deployment that other
machines point at.** That is the entire difference.

## Standalone: the shape you should start with

Set `MEMOIRE_HOME` (defaulting to `~/.memoire`), build, and run. The service
listens on `127.0.0.1:37778`, stores into `$MEMOIRE_HOME/memoire.db` as SQLite
with FTS5 and write-ahead logging, generates a bearer token into
`$MEMOIRE_HOME/token` at mode `0600` on first start, and creates the queue
directories, raw custody, and log directory underneath the home. There is
nothing else to provision: no database server, no credential store, no second
daemon.

For a throwaway instance — which is the right way to try this — point
everything at a scratch directory:

```bash
MEMOIRE_HOME=./tmp/memoire-home MEMOIRE_DB=./tmp/memoire-home/memoire.db make dev
MEMOIRE_HOME=./tmp/memoire-home ./scripts/smoke.sh
```

For supervision there are two supported paths, and both use the same container
image. `podman compose up --build` is the development and smoke surface. For an
always-on local service, a rootless Podman Quadlet user unit is the supervisor:
`make quadlet-start` builds the image, installs the unit under
`~/.config/containers/systemd/`, starts it, and waits on the health check;
`make quadlet-rebuild` is the after-a-change cycle; `make quadlet-status`,
`quadlet-health`, and `quadlet-stop` do what they say. The unit mounts the
Memoire home into the container at `/data` and restarts always.

That unit carries one design decision worth transplanting into anything you
supervise this way. **`MEMOIRE_DB` is deliberately not in the unit file.** It
lives in a separate `db.env` at mode `0600`, pulled in with `EnvironmentFile`.
There are two reasons, and the second one is the expensive one. A database URL
usually carries a credential, and a credential in a unit file is a credential in
whatever you version or sync. But more importantly, the unit file is *rewritten
on every install and rebuild* — so an inline setting is a setting that silently
reverts. That is not hypothetical: on 2026-07-22 a routine rebuild flipped
production from the PostgreSQL cluster back to the local SQLite file, because
the DSN had been where the rebuild could clobber it. The repair was the
`EnvironmentFile` split plus a deliberate sharp edge — **if `db.env` is missing,
the unit fails to start** rather than falling back to the default store.
Failing to boot is a five-minute problem; writing to the wrong database for a
week is not.

The listener is a second thing to be deliberate about. The unit sets the
service to bind all interfaces inside the container and publishes the port,
which makes it reachable from the LAN. That was ruled explicitly (D165,
2026-07-23): **the bearer token is the trust boundary, and the host firewall is
the outer wall.** If you want a strictly loopback install, that is a
configuration change you make knowingly, not a default you inherit.

Nothing about this shape is a demo mode. It is the same binary, the same
schema, and the same API that a hub runs.

## The hub: PostgreSQL on Kubernetes

The hub shape exists because a store that lives on a laptop is only available
when the laptop is. It moves the API, the drainer, and derivation onto a
machine that is always on, and points every workstation at it.

The engine change is separate from the topology change, and worth keeping
separate in your head. `MEMOIRE_DB` accepts either a SQLite path or a
`postgres://` DSN; a dialect seam and an engine-pair conformance battery own
parity between them — which is why the battery exists as a test rather than as
a promise. *(Edition correction, 2026-08-05: this edition read that rule as
"every feature must land behind that seam so all six cells of the matrix keep
working". The ratified lane matrix states it more precisely (D232): parity is
a storage-contract and lifecycle guarantee, not a feature-parity guarantee.
The lanes differ by equipment, not by nature. The invariant core — capture,
replayable custody, the queue, the review lifecycle including elevation, and
full-text search — holds on every cell; the hybrid vector lane is PostgreSQL
only by ruling (D218), and the SQLite engine never grows one. SQLite reads
"FTS-only", never "degraded".)*

On PostgreSQL, full-text search uses the `pg_search` extension's BM25 ranking
where the extension is available, and falls back to plain PostgreSQL text-search
ranking where it is not. That fallback is a real behavior difference, and the
conformance battery specifically probes for the extension rather than assuming
it — an earlier version silently ran the fallback while reporting that the
extension was active, which is exactly the class of failure this system's
honesty rules exist to prevent.

Moving a store between engines is what `memoire-pgshadow` is for. It applies
the PostgreSQL DDL to a target (`apply`), populates it either by copying from
an existing SQLite store (`copy`) or by replaying from raw custody (`replay`),
and produces a side-by-side parity report (`diff`) that exits non-zero when the
two disagree. The replay lane matters more than it looks: because raw custody
is total and events are replayable, a PostgreSQL store can be built from the
original events rather than trusted from a copy.

### The reference install

The author's hub runs on a single-node Kubernetes cluster, and the manifests
are generated with cdk8s from a TypeScript definition in `cdk8s/`, with the
synthesized YAML committed. That last part is a deliberate guard: a `make
cdk8s-verify` target fails if the committed output does not match the source,
because a stale synthesized directory means reviewers approve TypeScript while
the cluster applies old YAML.

What it deploys, in namespace `memoire`:

- a `Deployment` named `memoire-hub` — the API and drainer;
- a `Service` named `memoire-api-lan`, of type `LoadBalancer`, on port `37778`,
  the legacy LAN path to the hub — the durable client address is
  `https://memoire.superbiche.me` (TLS at the cluster's envoy gateway,
  infra-owned; the name resolves only on the author's tailnet by design);
- a `PersistentVolumeClaim` named `memoire-home`, 48 GiB, holding the Memoire
  home — queue, raw custody, logs;
- five `CronJob`s (below);
- a `Deployment` and a `ClusterIP` `Service`, both named `memoire-embed` — the
  vector lane's embedding worker, which answers query-time embedding requests
  on port `37779` and holds only its own dedicated token, never the database
  DSN. *(Edition correction, 2026-08-05: this edition was written when the hub
  deployed three CronJobs and no embed workload at all; the hybrid lane (D218)
  added the worker, its Service, and a fifth CronJob. The scheduled-work
  section below carries the full inventory.)*

The database is a CloudNativePG cluster running in the same namespace. The API
never carries a hand-written DSN: `MEMOIRE_DB` is injected from the
CloudNativePG-managed application secret, so credential rotation is the
operator's problem exactly once, in the database operator, rather than in every
workload that talks to it.

The migration onto this shape happened in two steps and the distinction is
instructive. The **database** moved first, on 2026-07-22 — the store became the
managed PostgreSQL cluster while the service itself still ran on a workstation.
The **service** moved on 2026-07-24 (D168), which is what actually closed the
"get this off my laptop" goal, because until then the data was available
around the clock and the service was only as available as the workstation
hosting it. D172 makes the second event, not the first, the version boundary.

## Tokens, scopes, and thin clients

Chapter 6 lists the scopes; this is what to do with them.

A single-machine install needs one credential and the service generates it for
you. Everything beyond that is `memoire-mint-token`, which generates a token,
registers only its hash, and prints the plaintext exactly once — you move it to
the client yourself, at mode `0600`.

The interesting case is a **thin client**: a workstation that captures into a
hub but holds no secrets. It gets a token with `write` scope only. Its hooks
write to a local queue as usual, and `memoire-forward` drains that queue to the
hub's ingest endpoint, deleting each local file only on the hub's
queue-accepted acknowledgement — the ack *is* the custody transfer, so an
interrupted forward loses nothing. The client holds no database credentials and
no model credentials, and it cannot read the corpus back or accept an
observation. Session-end simply notifies the hub that a session is worth
deriving; the hub's own scheduled sweep does the work.

One rule follows from the queue design and is easy to violate by accident:
**exactly one consumer per queue.** A machine runs either the drainer (because
it is the service) or the forwarder (because it is a client), never both. That
is why a hub can never be a client of itself.

The `notify` endpoint deliberately sits at `write` rather than `admin` scope,
and the contract says so explicitly rather than leaving it to be inferred: an
intent only marks work. It is also idempotent by test — two notifications for
the same session are one intent — because hooks are fail-open and sometimes
re-fire.

## Scheduled work

The hub runs five CronJobs, all against the same Memoire home volume:

- **`memoire-derive-sweep`**, hourly at minute 3. The main derivation lane. It
  consumes notify intents first, then falls back to enumerating every session
  with recent events as a safety net. Because runs deduplicate on their input
  hash, re-sweeping an unchanged session costs almost nothing, which is what
  lets the enumeration be blunt rather than clever.
- **`memoire-derive-sweep-hl5`**, daily at 04:03. The heavier nightly lane,
  pointed at a larger model endpoint.
- **`memoire-derive-sweep-hl5-early`**, at minute 33 of every hour from 22:00
  through 03:00. An opportunistic early run of that same heavy lane: it fires
  only when its own gates pass, and the 04:03 nightly remains the backstop
  that runs regardless.
- **`memoire-snapshot`**, daily at 02:30. Produces the encrypted snapshot
  artifacts described in chapter 3.
- **`memoire-embed-sweep`**, every fifteen minutes. The hybrid lane's
  freshness sweep: it asks the API which derived observations have no current
  embedding and writes the missing ones back. Its entire state question is
  that missing list, so a skipped run is made up from the table itself rather
  than from a queue that could silently die.

*(Edition correction, 2026-08-05: this edition was published when the hub ran
three CronJobs — the hourly derive lane, the nightly derive lane, and the
snapshot. The early nightly lane and the embedding freshness sweep (D218) have
since joined them.)*

The derivation lanes are why chapter 2's guards matter operationally: the
denylist and the min-content guard are what stand between an hourly cron job
and a GPU spending the night deriving nothing from empty windows.

One recent lesson is worth passing on because it will happen to you too. When
the service moved to the cluster, the CronJob pods began failing intermittently
— they were starting before the database was reachable, because network policy
convergence lagged pod scheduling by a few seconds. The fix was a **bounded**
PostgreSQL-readiness preflight: the pod waits for the database, but only for a
fixed budget, and then fails loudly rather than hanging. A follow-up pass
ordered the preflight gates correctly and added a rule that came out of the
same review — an enumeration step that cannot enumerate must report failure
rather than exiting green on an empty list. A sweep that finds nothing because
it could not look is indistinguishable from a sweep that found nothing, unless
you make it distinguishable.

## Migrations

The API process owns schema migration, and every other tool treats the schema
as already applied and fails clearly if a column it needs is missing.
Migrations are numbered SQL files applied in order, tracked in a
`schema_version` table.

The rule is **additive-only**, and it is a genuine constraint rather than a
preference. Allowed: creating tables, adding nullable or defaulted columns,
creating indexes, virtual tables, and triggers. Forbidden: dropping tables or
columns, renaming anything, changing a column's type, nullability, or default
in place, and any repair migration that deletes data.

The reason is that clients and the service drift in version across deploys —
a hook script on one machine may be older than the service it talks to — and
an additive schema means an old client is merely missing a feature rather than
broken. It also means the schema accumulates: there are dormant tables in there
for layers that do not exist yet, and that is the cost of the rule, knowingly
paid.

## Backups

Two mechanisms, for two different purposes.

`scripts/backup.sh` is the simple one: a SQLite `.backup` into a timestamped
file under the Memoire home. It is a local snapshot, it is quick, and it is the
right thing to run before you do something you might regret.

`scripts/memoire-snapshot.sh` is the real custody mechanism, and it produces
the artifact set chapter 3 describes: generation-stamped archives of raw
custody and, in its full form, a compressed database snapshot — each
age-encrypted before it leaves the machine, accompanied by a plaintext manifest
carrying the generation, artifact names, and hashes, names and hashes only,
never payload content. The manifest is the restore authority, and verifying it
before any restore is mandatory rather than hygienic.

**On the hub, the database leg of that script is switched off**, and the
reasoning generalizes. Once the store is a managed PostgreSQL cluster, the
database already has a better backup than a nightly file copy: continuous
archiving with point-in-time recovery, running since 2026-07-22. Two backup
systems covering the same data is not redundancy, it is two things to keep
correct and one of them silently rotting. So the hub's snapshot job runs
`--push --raw-only` — it never touches the database — and the raw custody leg
continues, because raw custody is a filesystem tree that no database operator
is backing up for you.

One operational wrinkle from that job is worth stealing. It pushes into a
landing zone on the host that an unprivileged mirror identity also writes to,
so it explicitly chowns what it drops to that identity's uid and gid. Without
that, a single push running as root locks the standing mirror writer out of its
own directory — the failure appears days later as backups that quietly stopped
being copied off-site.

**Everything about what happens to those artifacts once they leave the machine
— retention, purge propagation, and the crypto-shred lever that is the only
real time-to-gone guarantee — is chapter 3's subject and is not repeated
here.** If you are going to push snapshots anywhere, read that chapter first.

## What to watch

`GET /v1/health` is the single most useful thing to poll, because it carries
far more than liveness. In one unauthenticated request you get the pending
queue depth, the failed-queue depth, the age of the oldest queued file, the
last write timestamp, database and write-ahead-log sizes, the redaction
contract violation count, a count of events whose timestamps are implausibly
in the future, and — the one nobody thinks to expose — **which search engine is
actually in use**, reported as `fts5`, `bm25`, or `tsvector`.

That last field exists because of a specific failure: a PostgreSQL deployment
that loses its search extension or its index silently degrades to the weaker
ranking and keeps answering queries, so search quietly gets worse and nothing
reports an error. Publishing the engine turns an invisible degradation into a
one-line check.

`GET /v1/readiness` answers the orchestrator's question directly: it
round-trips the store through its read pool and proves the pending queue
directory can take a new file, returning 200 when both pass and 503 with a
per-check body (`store` / `queue`, each `ok` or `failed`) when either does
not. The read pool is a deliberate choice, not an omission: SQLite's single
write connection is legitimately held for the duration of bulk transactions,
and a probe that queued behind it would mark a merely busy pod unready and
shed capture traffic; on PostgreSQL both handles share one pool, so nothing
is skipped. That is
the endpoint a container readiness gate should point at — the hub's own
readiness probe does — while liveness stays on `/v1/health`: a restart cannot
fix an unreachable database, so a dependency failure should shed traffic, not
crash-loop the pod. Because readiness is unauthenticated, the failure body
never carries error text, only the closed check vocabulary; the underlying
error is in the server log.

The alerts worth wiring, in rough order of usefulness: the service being
unreachable; queue depth or age above a threshold (both matter — a queue that
is small but old is a stalled drainer); a write stall during an active session;
disk pressure on the Memoire home; and the daily smoke going red. Clock skew
has its own threshold (`MEMOIRE_SKEW_ALERT_SECONDS`, 60 by default), which
sounds fussy until you have events from a machine with a wrong clock
interleaved into a session window and derived as though they belonged there.

## The adoption guard

One last thing, because it is the most transferable idea in this chapter.

The standalone-SQLite mode is not maintained by good intentions. A smoke test
in the **default** test path boots the entire local column — API, drainer, and
store on a scratch home, with zero PostgreSQL, zero credential store, and zero
second daemon — and runs one capture-drain-search loop. If anyone introduces a
hard dependency that breaks the simple deployment, **the build fails**.

That is what turns a stated commitment into a held one. The commitment that
"it should still work locally" is the first casualty of a productive month
working on the hub, and no amount of remembering will save it. A test will.

---

*Next: chapter 5, [The seams](05-the-seams.md) — where Memoire meets its
sibling projects, and what it deliberately does not own.*
