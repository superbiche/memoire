# R-10 Design — the service axis (local ⇄ hub) with cross-compat

Status: RATIFIED — Q1-Q4 ruled (operator + strategic instance,
2026-07-22 evening; see §5 for the rulings and §3.4 for the ratified
additions).
Origin: R-10 ruling (D152 era, HL3-CNPG handoff §R-10) + operator
directive 2026-07-22, verbatim: "Memoire should be able to run locally
with SQLite, locally with PG, or hub-service remote (with either DBs).
Remote-hub-service is what I need; local-SQLite is what could make the
app adoption-friendly."

## 1. The two axes

- **DB axis — DONE (D157).** `MEMOIRE_DB` is a SQLite path or a
  `postgres://` DSN; the dialect seam + engine-pair conformance battery
  own parity. Nothing in R-10 touches this axis; every R-10 feature
  must land behind the seam so all six matrix cells work.
- **Service axis — R-10's work.** Where the API + drainer + derivation
  run relative to the machine a session runs on. Today the service
  parts are smeared: capture and reads go through the local API, but
  derivation is local shell scripts + a direct-store binary + ambient
  model keys on the session machine (SessionEnd hook
  `memoire-derive-auto.sh`, hourly sweep). That smear is what blocks a
  thin client and what R-10 removes.

## 2. Deployment matrix (the contract)

Three modes × two engines. Modes differ ONLY in configuration, never in
code paths:

| | standalone-SQLite | standalone-PG | hub client |
|---|---|---|---|
| API + drainer | local | local | on the hub |
| store | local file | DSN (local/LAN PG) | hub's (either engine) |
| capture hooks | local queue → local API | same | local queue → `memoire-forward` → hub `/v1/events` (exists: D147/D153, ack = custody transfer) |
| reads / injection / MCP | localhost API | localhost API | hub URL + bearer token |
| derivation | local, colocated with API | same | hub-side only |
| SessionEnd | derive notify to local API | same | derive notify to hub API |
| model creds | local credstore | same | **none on client** |
| DB creds | none (path) / db.env | db.env | **none on client** |

- "Hub" is not special code — it is a standalone deployment that other
  machines point at. The hub host today is already the hub (API quadlet +
  timers on the hub host, store on the derive host's CNPG); the client
  host becomes the first thin client.
- Adoption story = the standalone-SQLite column: one binary, one
  `MEMOIRE_HOME`, zero external services. It must never grow a hard
  dependency on PG, systemd credstore, or a second daemon.

## 3. What R-10 actually builds

### 3.1 Derive intake over HTTP (the core move)

New API surface (auth: existing bearer credentials; all handlers go
through the store seam so both engines serve them):

- `POST /v1/derive/notify {project, session_id}` — record a derive
  intent for a closed session. Replaces the client-side SessionEnd
  derive: `memoire-derive-auto.sh` collapses to a curl against the
  configured base URL. Kills ambient model keys AND direct store access
  on session machines.
- `GET /v1/derive/outstanding` — the STORE-BACKED outstanding-work
  answer, per (project, session): the AGGREGATE of live canonical
  events no completed run's input snapshot covers — count, byte weight
  (the size-classification input), and the bounds of the uncovered set.
  AMENDED SEMANTICS (round-1 MED-1, RATIFIED 2026-07-23 — supersedes
  the draft's "window tuples split by size class" wording):
  a row is NOT a derivable window — covered islands may lie inside its
  bounds; windowing/splitting stays with the sweep's own split logic,
  and consumers classify by UncoveredBytes. COVERAGE MECHANISM (of
  record, RATIFIED by the operator 2026-07-23 morning — review rounds
  2-4 rejected every timestamp/id heuristic in turn): a completed run covers exactly
  its recorded INPUT MANIFEST (`run_input_events` — the event ids its
  fetch returned, recorded by every completed-run writer; a
  completed dedup with identical input_hash retro-attaches the manifest,
  which is what heals pre-manifest rows). Manifest-less runs provide NO
  coverage — absence of evidence reads outstanding, never covered; the
  one-time cost is honest re-reporting of pre-deploy sessions until
  their first post-deploy sweep tick. Intent eligibility is causal and
  strict (an attempt serves only when its own fetch postdates the
  LATEST notify). KNOWN LIMIT (pending the same ratification): a served
  intent proves the fetch postdated the notify, not that in-flight
  capture had landed — the hook's bounded drain-wait narrows this to
  the hub drainer's poll window, and late arrivals stay visible via
  outstanding. Aggregate row semantics and the no-work expiry policy
  (48h TTL; intents for sessions WITH events deliberately never age
  out — an outage-stalled intent is the truthful open alarm, ruled on
  the vacation scenario) were RATIFIED in the same morning ruling.
  This surface is the check D163's MED-2 residual is
  parked on: the HL5 early-fire trigger stops counting structural
  markers and asks the store; arming becomes honest.

Sweep execution stays where it is: hub-side scripts + systemd timers +
the derive binary colocated with the API (direct store access on the
hub is fine and keeps the API the only HTTP write authority; the derive
binary remains part of the same Go codebase). Sweep scheduling (as
shipped in R-10.2): open intents run FIRST via `-list-open-intents`,
the `-list-targets` recency enumeration stays as the safety net, and
the union is deduped — the outstanding query is the trigger/operator
surface, not the sweep's scheduler.

### 3.2 Read-path base URL unification

One knob for every consumer that talks to the API (inject hooks, MCP
service, notify, status tooling): `MEMOIRE_BASE_URL` (default
`http://127.0.0.1:37778`) + optional token file for non-localhost.
Hub clients set exactly two things: `MEMOIRE_FORWARD_URL` (exists) and
`MEMOIRE_BASE_URL`. An audit of current hooks for hardcoded localhost
is part of the slice.

### 3.3 Credential consolidation

Model endpoint keys (HL3/HL5/Conductor) live only where derivation
executes — the hub's systemd credstore (already true since D162/D163).
R-10 removes the remaining client-side assumptions: the SessionEnd hook
no longer needs any key, and the fleet wiring (`~/.agents/memoire.json`
derive role) switches from script-with-store-access to notify-only.

### 3.4 Ratified additions (2026-07-22 rulings)

- **Notify idempotency, pinned by test**: two `POST /v1/derive/notify`
  for the same (project, session_id) = one intent. Hooks are fail-open
  and sometimes re-fire; the surface must be safe under that.
- **Token scope, stated**: notify is a write reachable with a client
  token. Acceptable — an intent only marks work — and the contract says
  so explicitly instead of inheriting it silently.
- **Standalone-SQLite smoke in the default test path**: boot the full
  standalone-SQLite column (API + drainer + store on a scratch home,
  zero PG, zero credstore, zero second daemon) and exercise one
  capture→drain→search loop. The build fails if anyone introduces a
  hard dependency that breaks the adoption column. This is the Q4
  constraint made mechanical.
- **Notify→completion latency journaled** (mandatory in R-10.2):
  intents carry timestamps; the sweep records per-intent latency at
  completion. The freshness revisit (post-R-10.4) happens on these
  measured numbers, not intuitions.

## 3.5 Topology of record (supersession note)

D147 slice 7 said "the hub host as client zero, then the client host".
Superseded in shape by the real topology: the hub host became the HUB
(API quadlet + timers on the hub host, store on the derive host's CNPG)
and can never be a client of itself (D153 exclusivity: one consumer per
queue — drainer XOR forwarder). The client host is therefore the FIRST
thin client.

POST-R-10.4 slice (journaled, not a gate): the hub's placement on the
hub host is transitional against the founding goal — the store is 24/7
but the service is only as available as the hub host. Because the hub
is not special code, relocating the service to the derive host is a
pure config/deploy slice; sequence it after R-10.4 proves the client
mode. That slice closes the original "get Memoire off the hub host"
chantier in the strong sense.

## 4. Slices

1. **R-10.1** — store queries + HTTP surface: outstanding-work query
   (engine-pair battery case), `/v1/derive/notify`,
   `/v1/derive/outstanding`. No behavior change for existing lanes.
2. **R-10.2** — SessionEnd rework: `memoire-derive-auto.sh` becomes
   notify-only (curl, fail-open, no keys, no store); fleet
   `memoire.json` update; sweep consumes notify intents (priority) +
   its own enumeration (safety net).
3. **R-10.3** — arm the D163 trigger on `/v1/derive/outstanding`
   (closes the MED-2 ruled-parked residual; `MEMOIRE_HL5_TRIGGER=1`).
4. **R-10.4** — the client host as first thin client: forwarder + base URL +
   token, zero creds, zero derive. Proves the hub-client column live.
5. **R-10.5** — standalone-SQLite packaging pass: quickstart doc, single
   binary + hooks bootstrap, config surface audit (everything defaults
   to local). The adoption artifact.

Adjacent residuals that fold in naturally when touched (not gates):
PG ports of cmd/purge + redact-derived (hub-side operator tools);
snapshot pg_dump leg (MIRROR-PROTOCOL); failed-run triage.

## 5. Rulings (operator + strategic instance, 2026-07-22)

1. **Q1 — sweep-only, WITH instrumentation.** Notify marks intent; no
   immediate derive. Decisive argument: immediate derive couples hub
   load to client session-close rhythm (N sessions closing together =
   N concurrent derivations); the sweep serializes naturally.
   MANDATORY in R-10.2: journal the notify→completion latency per
   intent. The freshness revisit after R-10.4 happens on measured
   numbers.
2. **Q2 — on-the-fly, firmly.** Outstanding-work is computed from
   events × derivation_runs; NO coverage table. Rationale of record: a
   coverage table is a cache that can lie — one more writer path whose
   invariant needs guarding, to save a query on a small corpus.
   Computed-from-source is "no claim, only evidence" applied to the
   schema itself. If the query ever shows up in timings:
   partial-index discipline first (0008 lesson, both engines), table
   last. (The notify-intents table is NOT a coverage cache — intents
   are source data, client statements with timestamps.)
3. **Q3 — the client host is NOW.** Onboarding the morning of 2026-07-23,
   operator at the machine; prep (code, wiring, review) the evening
   before. Clock discipline first (D149: client-host NTP verified BEFORE
   capture flows).
4. **Q4 — constraint now, artifact later.** The standalone-SQLite
   CONSTRAINT enters the chantier immediately (the smoke in §3.4); the
   packaging ARTIFACT (quickstart, bootstrap, distribution) is parked
   for the release milestone — packaging done now would be redone once
   release decisions (name, channel, redaction defaults for
   non-operator users) exist.
