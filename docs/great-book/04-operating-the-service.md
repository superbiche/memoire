# 4. Operating the service
*Great Book v0.7 edition — 2026-11-02.*

This chapter is for you running your own instance, not for reading over
someone else's shoulder. The reference deployment described in the middle is one
person's homelab, and it is included because a concrete example is worth more
than an abstract one — not because you should copy it.

A word on how much there is to operate. In the v0.6 edition this chapter spent
a third of its length on scheduled derivation and embedding lanes. Those are gone
(chapter 2). What remains to run is the service, its store, its custody volume,
two nightly jobs on the hub, and the capture timers on each workstation.

## Two shapes, one binary

There is no "server edition" and no "local edition". **The deployment modes
differ only in configuration, never in code paths**, and that constraint is the
load-bearing one: the moment a hub-only code path exists, the local mode becomes
the untested one, and the untested one is the one that breaks for every new
user.

The ratified contract (the R10 design, D165, July 2026) is three modes across two database
engines:

| | standalone-SQLite | standalone-PostgreSQL | hub client |
|---|---|---|---|
| API + drainer | local | local | on the hub |
| store | local file | a DSN | the hub's, either engine |
| capture hooks | local queue → local API | same | local queue → `memoire-forward` → hub |
| reads, MCP | localhost API | localhost API | hub URL + bearer token |
| database credentials | none, or a DSN | a DSN | **none on the client** |

"Hub" is not special code. **A hub is a standalone deployment that other
machines point at.** That is the entire difference.

Both halves are load-bearing. Remote-hub-service is what the author needs;
local SQLite is what could make the thing adoption-friendly. The second is
protected mechanically — see "The adoption guard" at the end.

## Standalone: the shape you should start with

Set `MEMOIRE_HOME` (defaulting to `~/.memoire`), build, and run. The service
listens on `127.0.0.1:37778`, stores into `$MEMOIRE_HOME/memoire.db` as SQLite
with FTS5 and write-ahead logging, generates a bearer token into
`$MEMOIRE_HOME/token` on first start, and creates the queue directories, raw
custody, and log directory underneath the home. There is nothing else to
provision: no database server, no credential store, no second daemon.

`scripts/memoire-bootstrap.sh` automates the first steps. `check` verifies the
required tools (curl, jq, sha256sum, GNU find, timeout — the hooks are fail-open,
so a missing tool means silent non-capture); `init` starts the API once to create
the home and token and proves the token is accepted; `systemd` installs a user
service; `hooks-claude` prints the Claude Code hook JSON to merge into your
settings. It refuses ambient hub configuration (`MEMOIRE_DB`, `MEMOIRE_PG_URL`,
`MEMOIRE_FORWARD_URL`): it is standalone-only by contract. `QUICKSTART.md` is the
step-by-step version.

For a throwaway instance — the right way to try this — point everything at a
scratch directory:

```bash
MEMOIRE_HOME=./tmp/memoire-home MEMOIRE_DB=./tmp/memoire-home/memoire.db make dev
MEMOIRE_HOME=./tmp/memoire-home ./scripts/smoke.sh
```

For supervision, a rootless Podman Quadlet user unit is the supported path for an
always-on local service: `make quadlet-start` builds the image, installs the unit
under `~/.config/containers/systemd/`, starts it, and waits on the health check;
`make quadlet-rebuild` is the after-a-change cycle; `quadlet-status`,
`quadlet-health` and `quadlet-stop` do what they say. The unit mounts the
Memoire home at `/data` and restarts always.

That unit carries one design decision worth transplanting into anything you
supervise this way. **`MEMOIRE_DB` is deliberately not in the unit file.** It
lives in a separate `db.env` at mode `0600`, pulled in with `EnvironmentFile`.
A database URL usually carries a credential, and a credential in a unit file is a
credential in whatever you version or sync. More importantly, the unit file is
*rewritten on every install and rebuild* — so an inline setting is a setting that
silently reverts. That is not hypothetical: on 2026-07-22 a routine rebuild
flipped production from the PostgreSQL cluster back to the local SQLite file.
The repair was the `EnvironmentFile` split plus a deliberate sharp edge — **if
`db.env` is missing, the unit fails to start** rather than falling back to the
default store. Failing to boot is a five-minute problem; writing to the wrong
database for a week is not.

The listener is a second thing to be deliberate about. The unit binds all
interfaces inside the container and publishes the port, which makes it reachable
from the LAN. That was ruled explicitly (D165, 2026-07-23): **the bearer token is
the trust boundary, and the host firewall is the outer wall.** If you want a
strictly loopback install, that is a configuration change you make knowingly.

Nothing about this shape is a demo mode. It is the same binary, the same schema,
and the same API that a hub runs.

## The hub: PostgreSQL on Kubernetes

The hub shape exists because a store that lives on a laptop is only available
when the laptop is. It moves the API and the drainer onto a machine that is
always on, and points every workstation at it.

The engine change is separate from the topology change. `MEMOIRE_DB` accepts
either a SQLite path or a `postgres://` DSN; a dialect seam and an engine-pair
conformance battery own parity between them — which is why the battery exists as
a test rather than as a promise. Parity is a storage-contract and lifecycle
guarantee, not a feature-parity guarantee (D232): the invariant core — capture,
replayable custody, the queue, and search over the raw records — holds on both
engines; the details of the search ranking differ by equipment.

A PostgreSQL store must be created with the plain `C` collation from the libc
provider, so that text ordering matches SQLite's byte order. The service checks
this before applying any schema and refuses to open a store that is out of
contract (D249), rather than half-materializing it.

On PostgreSQL, search uses the `pg_search` extension's BM25 ranking where the
extension is available, and falls back to a plain text-search column where it is
not. The fallback is a real behavior difference, and the health endpoint reports
which one is active (see "What to watch").

The BM25 index deserves an operator's attention for one reason. The first
version indexed only the id and the text; every filter a read applies — kind,
project, review state, session, time — was re-checked row by row, which on a
three-million-row table cost twelve to fifteen seconds for an unscoped search.
The second version of the index carries those columns, and a two-stage read keeps
one remaining non-indexable condition from undoing the gain. Fresh databases get
the new index directly. An existing database is moved by an explicit operator
step, `memoire-pgshadow reindex-bm25` (with `-dry-run` first), which builds the
new index concurrently beside the old one, verifies it, and only then drops the
old one; it needs roughly double the index size in free disk during the build
and was the one-time exception to the additive-migrations rule (a20 = A). `apply`
never rebuilds an existing index as a side effect of a deployment.

Moving a store between engines is what `memoire-pgshadow` is for. `apply` applies
the PostgreSQL DDL; `copy` populates a target from a SQLite store; `replay`
populates it by replaying raw custody; `diff` produces a side-by-side parity
report that exits non-zero when the two disagree; `reindex-bm25` is above. The
replay lane matters more than it looks: because raw custody is total and events
are replayable, a PostgreSQL store can be built from the original events rather
than trusted from a copy.

### The reference install

The author's hub runs on a small Kubernetes cluster, and the manifests are
generated with cdk8s from a TypeScript definition in `cdk8s/`, with the
synthesized YAML committed. The ledger's `make cdk8s-verify` target
fails if the committed output does not match the source, because a stale
synthesized directory means reviewers approve TypeScript while the cluster
applies old YAML. (The cdk8s tree and the deployment scripts live in the private ledger
repository, not the public tree.)

What it deploys, in one namespace:

- a `Deployment` named `memoire-hub`, one replica with a `Recreate` strategy —
  single writer, and the custody volume is read-write-once, so two API pods at
  once must never exist. It runs the API and the in-process drainer. Readiness
  gates on `/v1/readiness`; liveness stays on `/v1/health` with a generous
  timeout, since a restart cannot fix an unreachable database;
- a `ClusterIP` `Service` named `memoire-api-lan` on port `37778`. The durable
  client address is a stable name served over TLS by the cluster's gateway,
  which is owned by the infrastructure layer, not by this repository;
- a `PersistentVolumeClaim` of 48 GiB on an encrypted, replicated storage class,
  holding the Memoire home — queue, raw custody, logs;
- two `CronJob`s, below.

The database is a CloudNativePG cluster in the same namespace. The API never
carries a hand-written DSN: `MEMOIRE_DB` is injected from the CloudNativePG-managed
application secret, so credential rotation is the operator's problem exactly
once, in the database operator, rather than in every workload that talks to it.
The migration onto this shape happened in two steps: the **database** moved first,
on 2026-07-22, while the service still ran on a workstation, and the **service**
moved on 2026-07-24 (D168), which is what actually closed the "get this off my
laptop" goal and is why D172 makes the second event the version boundary.

Images are built in CI, scanned, and referenced by immutable tag **and** digest —
never `:latest`. The release path is: replace the two image references in the
manifest source, synthesize, `make cdk8s-verify`, commit,
then `make deploy` (all in the ledger), which runs a manifest-versus-cluster
parity gate first and refuses unexplained drift (D199). `make deploy-diff` runs that check alone.

**What runs, and what does not.** After the L0010 change the namespace holds the
hub, its database, and two jobs. The derive sweep, the embedding sweep, the
embedding service and its configuration are not in the manifest. Because
`kubectl apply` does not prune, removing them from the manifest did not remove
them from the cluster: the retirement was a one-time operator-gated step that
deleted the five objects explicitly and then deployed with a judged exception for
the parity gate's drift on the removed identities. If you adopt this shape from
an earlier layout, that is the step to repeat.

## Tokens, scopes, and thin clients

Chapter 6 lists the scopes; this is what to do with them.

A single-machine install needs one credential and the service generates it for
you. Everything beyond that is `memoire-mint-token`, which takes a required
`-label` and `-scopes` (default `read,write`; an unknown scope is a hard error),
registers only the hash, and prints the plaintext exactly once — you move it to
the client yourself, at mode `0600`.

The interesting case is a **thin client**: a workstation that captures into a hub
but holds no secrets. It gets a token with `write` scope only. Its hooks write to
a local queue as usual, and `memoire-forward` drains that queue to the hub's
ingest endpoint, deleting each local file only on the hub's queue-accepted
acknowledgement. The client holds no database credentials, and it cannot read the
corpus back. Forwarding is at-least-once; a transport error, a server error or an
authentication failure leaves the item pending and backs the loop off (two
seconds up to five minutes), while any other client error moves that one item to
`failed/` as poison. Items from priority adapters — by default the Claude Code
hook — go ahead of any backlog, and the rest go in slices of 150, so a first
import of a large history does not delay live capture.

One rule follows from the queue design and is easy to violate by accident:
**exactly one consumer per queue.** A machine runs either the drainer (because it
is the service) or the forwarder (because it is a client), never both. That is why
a hub can never be a client of itself.

## Scheduled work

The hub runs two CronJobs, both against the same custody volume and both in the
operations image, with concurrent runs forbidden and no catch-up of missed
windows:

- **`memoire-snapshot`**, daily at 02:30 (Europe/Paris). Runs the snapshot script
  with `--push --raw-only`: it produces the encrypted, generation-stamped
  artifacts of raw custody described in chapter 3 and pushes them to a local
  landing zone that the host's off-site sweeps carry away. It is pinned to the
  node that has that landing zone, so after a hub failover to another node its
  job pod stays pending until its deadline fails it, and that failed job is the
  signal.
- **`memoire-raw-pack`**, daily at 04:15. Packs closed days of raw envelopes into
  immutable compressed parts (`--apply --grace-days 2 --max-seconds 5400`). It
  needs no database access, and its time bound keeps it inside the window between
  the snapshot and the off-site mirror.

On a workstation, the scheduled pieces are systemd user timers: the capture tick
for Claude Code transcripts every ten minutes, one tick per other harness that has
a sweep adapter, and a capture-health timer that notices a stuck forwarding queue
and tells you outside any session, because session-start injection is off by
default and nothing else would. Chapter 6 lists the scripts.

One lesson from the move to the cluster is worth passing on, because it will
happen to you too. The CronJob pods began failing intermittently — they started
before the database was reachable, because network-policy convergence lagged pod
scheduling by a few seconds. The fix was a **bounded** database-readiness
preflight: the pod waits for the database, but only for a fixed budget, and then
fails loudly rather than hanging.

## Migrations

On a normal start the API process applies schema migrations, because it opens
the store through the code path that migrates a SQLite file. `memoire-mint-token`
uses the same path, so pointing it at a SQLite file migrates that file. The other
maintenance tools open their own connections and do not migrate; on PostgreSQL,
`memoire-pgshadow apply` is the explicit schema step and must run before
`memoire-pgshadow replay`. Chapter 6 lists how each tool picks its store. Tools
that need a column fail clearly if it is missing. SQLite
migrations live in `schema/` and the PostgreSQL twin in `schema-pg/`; they are
numbered SQL files applied in order and tracked in a `schema_version` table. A
file that does not register a schema version is a sketch and is never applied.

The rule is **additive-only**, and it is a genuine constraint rather than a
preference. Allowed: creating tables, adding nullable or defaulted columns,
creating indexes, virtual tables and triggers, and — by a narrow ruled carve-out —
widening a check constraint to a strict superset. Forbidden: dropping tables or
columns, renaming anything, changing a column's type, nullability or default in
place, and any repair migration that deletes data. The reason is that clients and
the service drift in version across deploys — a hook script on one machine may be
older than the service it talks to — and an additive schema means an old client is
merely missing a feature rather than broken. It also means the schema accumulates:
the tables of the retired derivation layer are still there, and that is the cost
of the rule, knowingly paid.

## Backups

Three mechanisms, for three purposes.

`scripts/backup.sh` is the simple one: a SQLite `.backup` into a timestamped file
under the Memoire home. It is for the standalone shape, it is quick, and it is the
right thing to run before you do something you might regret.

`scripts/memoire-snapshot.sh` is the real custody mechanism for raw custody, and it
produces the artifact set chapter 3 describes: generation-stamped archives and, in
its full form on SQLite, a compressed database snapshot — each age-encrypted before
it leaves the machine, accompanied by a plaintext manifest carrying the generation,
artifact names and hashes, never payload content. The manifest is the restore
authority, and verifying it before any restore is mandatory.

**On the hub, the database leg is switched off**, and the reasoning generalizes.
Once the store is a managed PostgreSQL cluster, the database already has a better
backup than a nightly file copy: continuous archiving with point-in-time recovery,
running since 2026-07-22. Two backup systems covering the same data is not
redundancy, it is two things to keep correct and one of them silently rotting. The
snapshot job therefore never touches the database, and the raw custody leg
continues, because raw custody is a filesystem tree that no database operator is
backing up for you. Everything about what happens to those artifacts once they
leave the machine is chapter 3's subject.

## What to watch

`GET /v1/health` is the single most useful thing to poll, because it carries far
more than liveness. In one unauthenticated request you get the version, the
pending queue depth, the failed-queue depth, the age of the oldest queued file, the
last write timestamp, database and write-ahead-log sizes, the redaction contract
violation count, a count of events whose timestamps are implausibly in the future,
and — the one nobody thinks to expose — **which search engine is actually in use**,
reported as `fts5`, `bm25` or `tsvector`. That last field exists because a
PostgreSQL deployment that loses its search extension or its index silently
degrades to the weaker ranking and keeps answering queries. Publishing the engine
turns an invisible degradation into a one-line check. (The body also still
carries fields for the retired vector lane. `vector_lane` reports the database's
capability, so a PostgreSQL store that kept its vector schema still says
`pgvector` after the lane was retired; `hybrid_search` reads `absent` when no
embedding service is wired; the two counters stay numbers. None of them means
the retirement is incomplete.) The endpoint reads its counts from a lightweight path on purpose: an
exact count over the observations table once took eleven seconds on the hub and
crash-looped it under its own liveness probe.

`GET /v1/readiness` answers the orchestrator's question directly: it round-trips
the store through its read pool and proves the pending queue directory can take a
new file, returning 200 when both pass and 503 with a per-check body (`store` and
`queue`, each `ok` or `failed`) when either does not. Because it is unauthenticated,
the failure body never carries error text, only that closed vocabulary; the
underlying error is in the server log.

`GET /metrics` serves Prometheus counters and gauges with no authentication and no
stored content (D224). Among them are a counter of reads by route and by whether the
request admitted raw records, and a counter of returned items by kind and by age
bucket — the telemetry that L0009 added so that retention questions rest on
evidence, and that L0010 used to decide what the read surface is for.

The alerts worth wiring, in rough order of usefulness: the service being
unreachable; queue depth or age above a threshold (both matter — a queue that is
small but old is a stalled drainer); a write stall during an active session; disk
and inode pressure on the Memoire home; the snapshot job failing; and the daily
smoke going red. `scripts/check-health.sh` bundles several of these with tunable
thresholds. Clock skew has its own threshold (`MEMOIRE_SKEW_ALERT_SECONDS`, 60 by
default), which sounds fussy until you have events from a machine with a wrong
clock interleaved into a session window.

## The adoption guard

One last thing, because it is the most transferable idea in this chapter.

The standalone-SQLite mode is not maintained by good intentions. A smoke test in
the **default** test path (`make test`) boots the entire local column — API,
drainer and store on a scratch home, with zero PostgreSQL, zero credential store
and zero second daemon — and runs a capture-drain-search loop. If anyone
introduces a hard dependency that breaks the simple deployment, **the build fails**.

That is what turns a stated commitment into a held one. The commitment that "it
should still work locally" is the first casualty of a productive month working on
the hub, and no amount of remembering will save it. A test will.

---

*Next: chapter 5, [The seams](05-the-seams.md) — where Memoire meets its sibling
projects, and what it deliberately does not own.*
