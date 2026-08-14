# Missing: the current version's punch-list

**Definition (memoire:D173, mirroring liaison:D285):** MISSING holds what the
CURRENT VERSION'S GOAL requires to be functional, stable, complete —
including items of the in-flight run phase. It is CONSUMED within the
version. Everything wanted-later lives in `ROADMAP.md` (`gap:` lines, the
memoire-local wanted-later ledger, memoire:D178). An entry is never deleted:
it is consumed (fixed, or archived with its dated note under
`docs/archive/`) or deported (to `ROADMAP.md`, tagged `gap:`, with provenance
and a one-line ruling). No silent death.

**Release gate (memoire:D172):** no 0.x tag ships with open, non-deported
items. The gate ARMS AT THE NEXT TAG — v0.6.0 is the arming act and is exempt
by sequence, so the gate binds at v0.7: no v0.7 tag with open, non-deported
items.

**Current version goal (v0.6.0):** memoire runs as the ecosystem's hub of
record on HL3 — CNPG primary with standalone-SQLite parity held, capture
never blocked from any harness, derive intake over HTTP, read and injection
parity across the fleet.

Item numbers are stable across sweeps; gaps in the sequence are consumed or
deported items. This file is founded by the 2026-07-25 nightwatch: six
candidates were filed, four kept below, and two (#4, #6) deported to
`ROADMAP.md` at filing time under the memoire:D173 discipline — a candidate
that does not serve the current goal belongs in the roadmap, not here.

---

## 1. ~~The derive sweep exits green when it cannot enumerate its targets~~ (memoire:D168 residual, 2026-07-24 — CLOSED 2026-07-25)

**Observation:** `scripts/memoire-derive-sweep.sh` logs
`sweep skipped=list_targets_failed` and exits 0 when target enumeration
fails. During the 2026-07-24 hub-cutover window this masked a real failure
class: three sweep Jobs failed against PG behind the netpol convergence lag,
and the sweep's own exit code said nothing was wrong. The marker is loud in
the log; the exit status is not.

**Why it matters:** the v0.6.0 goal makes HL3 the hub of record, which means
derivation now runs as a CronJob whose only supervisor is its exit code.
A gate that cannot enumerate its work and still reports success is
indistinguishable from a gate with no work to do — precisely the failure
class memoire:D166 exists to prevent. Fail-open is correct for *capture*
(capture must never be blocked); it is not obviously correct for a *sweep*
that has already been told there is outstanding work.

**Hypothesis:** apply a memoire:D166-style honesty rule — enumeration failure exits
non-zero (or a distinct reserved code) so the CronJob surfaces as failed,
while genuine no-work settlement keeps exiting 0. The bounded PG-readiness
preflight landed in `fix/cronjob-pg-preflight` narrows the window that
produced the masking but does not change the honesty semantics.

**Status 2026-07-25:** open, and explicitly flagged for an operator ruling —
memoire:D168 surfaced it "for ruling" rather than fixing it, because
tightening a fail-open path in the capture/derive lane is a policy call, not
a repair. Carried into the v0.6.0 punch-list rather than deported: the
version's goal is the hub, and this is the hub's own supervision honesty.

**CLOSED 2026-07-25 (consumed — fixed; orthogonal review R1 finding 4).** The
item was already fixed when it was filed; the filing read stale code. Verified
2026-07-25: `scripts/memoire-derive-sweep.sh:187-195` logs
`sweep skipped=list_targets_failed` and then `exit 1`, under a comment naming
the mandatory-enumeration honesty rule as an extension of memoire:D166 —
successful EMPTY enumeration still exits 0, and intent-list degradation stays
fail-open, exactly the hypothesis above. The fix landed in `75f96fa`
("preflight audit repairs: hard-bounded probe, gate-ordered, enumeration
honesty rule, command-contract harness") and reached `main` as merge `af43445`
("Merge fix/preflight-audit-repairs: bounded gate-ordered preflight +
enumeration honesty rule (ratified at merge)"), whose message records the
operator ratifying the honesty rule at merge — the policy ruling this item was
waiting for. Regression cover, both re-run 2026-07-25 and both printing OK:
`scripts/tests/sweep-split-test.sh:195-200` and
`scripts/tests/derive-db-env-test.sh:100-120` assert the non-zero exit. The
body above is preserved as the filing record; nothing is deleted.

## 2. ~~`inject-and-search` has no owning document~~ (2026-07-24 nightwatch recon §5, memoire:D175 — CLOSED 2026-08-05)

**Observation:** mapping all 18 files under `docs/` against the eight
proposed gestures leaves `inject-and-search` with zero owning documents.
Injection and the MCP tool surface are described only in the README's step 5
and in the scripts themselves (`scripts/memoire-inject-claude.sh`,
`bin/memoire-mcp`). Every other gesture has at least one contract-grade doc;
this one has none.

**Why it matters:** the v0.6.0 goal names "read and injection parity across
the fleet" as one of its four clauses, and memoire:D169 makes a gesture's
view a PRECONDITION of the gesture rather than optional documentation. A
view generated over a gesture with no owning contract can only surface
journal entries — it cannot point a reader at the contract, because there
isn't one. The parity claim then rests on scripts and habit.

**Hypothesis:** a `docs/INJECTION-AND-SEARCH.md` contract stating the two
session-start blocks and their selection rules, the MCP tool surface and its
stability contract, `MEMOIRE_BASE_URL` resolution, and the foreign-client
read-parity requirement — the same grade as `docs/ELEVATION-DESIGN.md`. The
README's step-5 prose is source material, not a substitute: it is moving to
pointer-only under memoire:D173 and its MCP tool list is exactly the kind of
perishable fact a README may no longer carry.

**Status 2026-07-25:** open. Filed by the nightwatch recon; the gap is a real
absence, not a mapping artifact.

**CLOSED 2026-08-05 (consumed — built; memoire:D235).** The hypothesis landed
as written: `docs/INJECTION-AND-SEARCH.md`, an `ELEVATION-DESIGN.md`-grade
owning contract stating the state/knowledge split (memoire:D190), the
session-start blocks with their selection rules (recency, not semantic
ranking; `~`-marking; overflow-loud operating notes), the MCP tool surface and
its stability contract (memoire:D172 versioned surface, memoire:D202 tokenless
handshake), the read-path `MEMOIRE_BASE_URL` rule as shipped (memoire:D165,
R-10 §3.2; residual consumers ledgered as `gap:base-url-unification` in
`ROADMAP.md`), and the foreign-client read-parity requirement with its honesty
note
(the foreign read smokes are manual runbooks, not CI). The doc is drift-linked
to the inject script, the envelope adapter, the MCP tool table, and the server
scope map, so the pre-commit gate now catches its staleness. The body above is
preserved as the filing record.

## 3. ~~`MIRROR-PROTOCOL.md` rules 4 and 5 are unresolved pending operator ratification~~ (2026-07-24 nightwatch recon §5 — CLOSED 2026-08-05)

**CLOSED 2026-08-05:** the rules were ratified all along — memoire:D156
ruling 1 (operator, 2026-07-20): "the contract text merged in 3f99dd3
stands as ratified." Only the document's own status flag was stale; the
D225 README-vs-code audit (round 4 sweep) proved it, and the dated
correction in `docs/MIRROR-PROTOCOL.md` cleared the flag. The body below
is preserved as the filing record.

**Observation:** `docs/MIRROR-PROTOCOL.md` is contract-grade and carries the
gate condition (iii) of liaison:D97, but it flags its own rules 4-5
resolution as pending operator ratification. The flag has stood while the
store itself moved off the workstation.

**Why it matters:** the mirror protocol governs remote custody, and v0.6.0 is
the version in which custody became remote in fact — the store lives on the
HL3 LUKS2 volume, not on the workstation. An unratified pair of rules in the document
that governs how that custody behaves is a live hole in the version's own
subject matter, not a background wish.

**Hypothesis:** the two rules are put to the operator with the post-cutover
facts attached, ratified or amended, and the pending flag is cleared by dated
correction. No implementation is blocked on the answer; the document's
authority is.

**Status 2026-07-25:** open, operator-blocked. Kept in the punch-list rather
than deported — unlike other operator-blocked items, this one governs a
mechanism the v0.6.0 goal directly depends on.

## 5. ~~The drift gate is worktree-hostile: sibling-repo links break every commit from a worktree~~ (Lane 1 finding, 2026-07-25; pre-existing — CLOSED 2026-08-05)

**Observation:** `drift check` runs from the pre-commit hook and resolves
relative links against the checkout root. `README.md` carried
`[Liaison](../liaison)`, which resolves in the main checkout of the home
repo and cannot resolve from any worktree — the gate
fails, and the only way past it is `git commit --no-verify`. The gate that
exists to keep docs honest is the reason commits get made with the gate off.

**Why it matters:** worktrees are now the normal way work happens in this
repo (the whole 2026-07-25 twin runs in one). A doc gate whose standard
failure mode is "bypass it" trains the bypass, and a bypassed gate protects
nothing. This is in-flight run-phase infrastructure of the current version,
which is why it sits here rather than in the roadmap.

**Hypothesis:** repo-external references become textual rather than relative
links (no link, no resolution, no false failure); if a real cross-repo link
is ever needed, it is registered as a drift anchor with an explicit
out-of-tree provenance rather than as a relative path.

**Status 2026-07-25:** PARTIALLY CONSUMED. The `README.md` instance is gone —
the pointer-only rewrite under memoire:D173 makes the Liaison reference
textual, and `drift check` goes from `1 broken link` to clean in this
worktree. The class remains: nothing prevents the next sibling-repo relative
link from re-arming the same trap, and no rule states the convention. Item
stays open until the convention is written down.

**CLOSED 2026-08-05 (consumed — convention written; memoire:D235).** The
README instance was already gone (partial consumption above); the open half
was the unwritten convention, and it now stands in `AGENTS.md` Working Rules:
repo-external references in markdown are textual, never relative links; drift
cannot bind an out-of-tree target, so there is no anchored-link exception —
the reference stays textual. Sweep re-run at closing over
tracked markdown (including hidden dotdirs): zero live sibling-relative
links — the only hit is this item's own backtick-quoted filing text, which
drift's link lint does not parse as a link. The body above is preserved as the
filing record.

## 7. ~~`/v1/health` reports a hardcoded `"version": "0.1.0"`~~ (Great Book lane finding, 2026-07-25 nightwatch; operator ruling R11 2026-07-28 filed it — CLOSED 2026-08-03)

**Observation:** `services/api/internal/handlers/handlers.go:46` emits
`"version": "0.1.0"` as a string literal in the health payload. The repo's
`VERSION` file says `0.6.0`. The number the public read surface reports is five
eras behind the system it describes, and nothing in the build or the test suite
notices, because nothing compares the two.

**Why it matters:** `/v1/health` is a public read surface — it is what a
foreign client, a probe, or an operator reaches for when asking "what am I
talking to". A version string that is wrong by five eras is worse than an
absent one: it answers the question confidently and incorrectly, and every
consumer that branches on it branches on a fiction. The v0.6.0 goal makes read
parity across the fleet a clause of the version; a misstated version is a
parity defect at the first byte of the first response.

**Hypothesis:** either read `VERSION` at startup and carry it on the handler,
or stamp it at build time via `-ldflags -X`. The build-stamp form is the more
honest of the two for a containerized hub (the binary reports what it was built
from, not what happens to be on disk beside it), but either closes the lie. A
regression test asserting health's version equals `VERSION` is what keeps it
closed.

**Status 2026-07-28:** open. Filed by operator ruling R11. Pairs with item #8 —
both are small, both live in the same handler file, and both are read-surface
honesty defects; they should land together in one future reviewed service
slice rather than as two drive-by edits.

**Status 2026-08-03:** RELEASE-BLOCKING (memoire:D225) — the v0.7 tag does not
happen while this surface lies.

**CLOSED 2026-08-03 (consumed — fixed; memoire:D227).** The hypothesis's
build-stamp form landed: `-ldflags -X` from `VERSION` into
`internal/buildinfo` at every shipping entry point (Makefile, Containerfile,
e2e smoke), "dev" on unstamped builds, and — beyond the hypothesis, from the
review chain — `buildinfo.Public()` bounds the stamp to a closed version
shape before the unauthenticated body (a build input is an operator-chosen
string under the memoire:D224 invariant). The regression legs: the e2e smoke
asserts health's version equals `VERSION` over HTTP,
`scripts/tests/version-stamp-test.sh` pins every build entry point, and the
grammar has an accepted/rejected table test. The MCP handshake's identical
`0.1.0` literal fell in the same slice. Live on the hub: `"version":"0.6.0"`
(merge f50ef80, image 0.6.0-dev.gf50ef80). 4-round DEEP review APPROVE,
`tmp/honest-surfaces-review/`. The body above is preserved as the filing
record.

## 8. ~~`/v1/readiness` probes nothing~~ (orthogonal review R1 finding 6 / repair residual 3; operator ruling R16 2026-07-28 — CLOSED 2026-08-03)

**Observation:** `services/api/internal/handlers/handlers.go:62-64` returns an
unconditional `200 {"status":"ready"}`. It touches no store, opens no
connection, writes no queue file. The handler cannot fail, which means it
cannot report failure — the endpoint is a constant dressed as a probe.

**Why it matters:** a hub of record running on Kubernetes is supervised by its
readiness gate and nothing else. A readiness endpoint that always says ready
tells the orchestrator to route traffic to a pod whose database is unreachable
or whose queue directory is unwritable, and it does so at exactly the moment
supervision matters. The 2026-07-24 cutover window already produced the
adjacent failure class — sweep Jobs failing against PG behind the netpol
convergence lag — where the only honest signal came from a log line rather than
from a gate. This is the same fail-open-when-it-should-fail-closed shape that
memoire:D166 exists to prevent, sitting on the endpoint whose entire job is to
be the shape's opposite.

**Hypothesis:** probe the two things the service cannot function without —
store reachability (a cheap round-trip, not a full stats query) and queue
writability — and return `503` with a per-check body when either fails.
Capture-first is not violated: capture is fail-open by design at the queue
writer, and a readiness gate that sheds traffic from a broken pod protects
capture rather than blocking it.

**Status 2026-07-28:** open. Filed by operator ruling R16 from the orthogonal
review's repair residual. Pairs with item #7 — same file, same read-surface
honesty class, one future reviewed service slice.

**Status 2026-08-03:** RELEASE-BLOCKING (memoire:D225) — the v0.7 tag does not
happen while this surface lies.

**CLOSED 2026-08-03 (consumed — fixed; memoire:D227).** Exactly the
hypothesis: store reachability (one read-pool round-trip — the write handle
is deliberately not probed, a busy SQLite writer is not unreadiness; review
F2 demonstrated the first cut shedding capture traffic behind a held write
transaction) plus pending-queue writability (a temp probe no consumer can
pick up), `503` with a per-check body on failure. The body is
closed-vocabulary (`ok`/`failed`) per the memoire:D224 unauthenticated-
surface invariant; driver error text goes to the server log only. The hub's
readinessProbe now points at `/v1/readiness` (explicit `timeoutSeconds` — the
k8s 1s default sat below the handler's budget); liveness stays on
`/v1/health`, whose dependency-boundness is the parked audit-lane residual
`gap:liveness-surface` in `ROADMAP.md` (review F4, preexisting). Live on the
hub: pod Ready under the new gate, readiness answering
`{"checks":{"queue":"ok","store":"ok"}}`. 4-round DEEP review APPROVE,
`tmp/honest-surfaces-review/`. The body above is preserved as the filing
record.

## 9. ~~Standalone derivation has no BYOK endpoint support — two-dialect provider ruled for v0.7~~ (memoire:D232 matrix cell; handoff 2026-08-04 — CLOSED 2026-08-05)

The lane matrix (memoire:D232, README) says standalone derivation
"requires a model endpoint you provide" — but no BYOK provider exists:
the only model-backed provider is the `local-qwen` allowlisted-endpoint
adapter, and the derive lane in a standalone deployment has nothing a
user can point at their own OpenAI-compatible or Anthropic endpoint.
Until the slice lands, the matrix cell is a MISSING pointer, not a claim
(the ruling's own wording discipline).

Ruled shape (docs/LANE-MATRIX-BYOK-HANDOFF-2026-08-04.md §3, R-12 design
in docs/BYOK-DERIVE-DESIGN.md): exactly two dialects — OpenAI-compatible
v1 and Anthropic — CLOSED SET, extended by operator ruling only, never by
PR (D175 closed-set family). Config: one URL, one key (env/file, never
repo-riding config, never logged), one model, explicit dialect selector.
Acceptance is release-blocking: capture → BYOK derive against a stub
endpoint speaking each dialect → proposed rows land with correct
provenance, in the default test path (extension of the standalone smoke;
no network, no real keys). Escape valve if the slice threatens
stabilization: the operator decides between slipping to release week or
downgrading the cell to "documented, untested" — surfaced early, not at
tag time.

**CLOSED 2026-08-05 (consumed — built; BYOK slice, same session as the
ruling).** The `byok` provider exists in `modelderive`: one URL, one key
(env-name or non-world-readable file), one model, explicit dialect from
the closed set, required-explicit data boundary with `-allow-external`
per-run opt-in for external endpoints. OpenAI-v1 dialect generalizes the
in-repo production chat-completions shapes; Anthropic dialect ported
from Reveille Conductor (provenance in source comments). Store gate
widened under the strict-widening carve-out (schema 0016 / schema-pg
0005; data-preservation regression test). Evidence: adapter contract
tests (`byok_adapter_test.go` — both dialects, headers, error taxonomy,
refusals, key custody), CLI guard tests, and the two-dialect stub legs
in `tests/e2e/standalone-sqlite-smoke.sh` (capture → BYOK derive →
proposed rows with byok provenance; no network, no real keys). The body
above is preserved as the filing record.

## 10. ~~The public Great Book edition contradicts shipped v0.6 behavior in chapters 3, 4, and 6~~ (D225 audit r4; operator ruled ship-with-residual 2026-08-05 — CLOSED 2026-08-05)

**Observation:** the D225 pre-tag README-vs-code audit (4-round DEEP
chain, `tmp/readme-audit/`) closed the README/QUICKSTART/ROADMAP surface
clean, and edition corrections repaired the demonstrated staleness in the
index and chapters 1, 2, and 5 — but the round-4 sibling sweep proved the
staleness class survives in the untouched chapters: ch3 claims no code
path deletes from raw custody (the purge tool does; the true invariant is
no automatic pruning) and describes the mirror gate pre-D155/D156; ch4
states the superseded all-cells feature-parity rule (layered capability
ruled in memoire:D232) and lists three hub CronJobs where the manifest
defines five plus the embed workloads; ch6's API reference omits
`/metrics`, the embeddings surfaces, and the hybrid health fields.

**Why it matters:** README directs public readers to the Great Book as
the manual of the system; a manual that contradicts the shipped system is
the D225 lying-surfaces class in prose. Per the book's own rule — one
edition per version — v0.7 needs a fresh edition regardless.

**Ruling (operator, 2026-08-05, D342 checkpoint decision=ship-with-residual):**
the audit chain closes with this residual parked here, not patched
finding-by-finding. The v0.7 Great Book edition refresh reconciles
chapters 3, 4, and 6 (and repeats the sweep before closing), taking
`tmp/readme-audit/findings-r4.md` F5 as its input list. Release-gating:
the v0.7 tag ships its manual.

**CLOSED 2026-08-05 (consumed — fixed; memoire:D235).** The ruled edition
refresh landed as dated edition corrections against every F5 instance:
chapter 3 now states the true custody invariant (deletion from raw custody is
never automatic — the purge act is deliberate, double-gated, and
quarantine-by-default with the ratified `--no-quarantine` escape named,
memoire:D151) and the ratified mirror status (memoire:D156 ruling 1;
age-encrypted snapshot artifacts pushed on the hub's nightly raw-only
schedule, the database leg owned by CNPG continuous archiving); chapter 4
states the layered-capability rule (memoire:D232 — storage/lifecycle parity,
not feature parity; the hybrid lane PG-only, SQLite FTS-only by design) and
the real hub inventory (five CronJobs with schedules verified against
`cdk8s/main.ts`, plus the embed Deployment and Service); chapter 6 names
`/metrics` as the fourth unauthenticated surface (memoire:D224), the six
admin-readable GETs including the embeddings pair (memoire:D218), and the
hybrid search and health surfaces on both the HTTP and MCP references. The
sibling sweep was repeated before closing by the review chain
(`tmp/missings-review/`), per the ruling above: its round 1 caught four
further instances (index/ch1 ratification-and-tag staleness, ch1 derivation
locality and dormant-seam claims, ch2/ch6 two-block injector descriptions),
all corrected in the same slice before merge. The body above is preserved as
the filing record.
