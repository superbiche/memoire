# Missing: the current version's punch-list

**Definition:** MISSING holds what the CURRENT VERSION'S GOAL (in `VERSION`)
requires to be functional, stable, complete — including items of the in-flight
run phase. It is CONSUMED within the version. Everything wanted-later lives in
`ROADMAP.md` (`gap:` lines). An entry is never deleted: it is consumed (fixed,
or archived with its dated note under `docs/archive/`) or deported (to
`ROADMAP.md`, tagged `gap:`, with provenance and a one-line ruling). No silent
death. A release tag is the operator's call; open items carry into the next
version.

Item numbers are stable across sweeps; gaps in the sequence are consumed or
deported items. This file is founded by the 2026-07-25 nightwatch: six
candidates were filed, four kept below, and two (#4, #6) deported to
`ROADMAP.md` at filing time — a candidate that does not serve the current
goal belongs in the roadmap, not here.

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

## 11. ~~The sweep's ceiling probe latches its fallback on a first target that reports no limits, so a declared derive profile can never govern~~ (live incident 2026-09-07, memoire:D0259 deploy — CLOSED 2026-11-01)

**Observation:** `scripts/memoire-derive-sweep.sh:359 resolve_ceiling` sets
`CEILING_RESOLVED=1` on its FIRST line, BEFORE reading whether the dry-run JSON
it was handed carries an `effective_limits` block. Several first-target
outcomes carry none: the `-execute` guard skip
(`services/api/cmd/derive/main.go:818-828`, no `effective_limits` key), the
`project_denied` skip (`main.go:695-702`), and the
`no source events matched derivation window` error (`main.go:484`), which emits
no JSON at all. Only the two full dry-run exits emit the block (`main.go:499`,
`main.go:605`). When the first target of a sweep lands on one of the others,
the probe records `source=fallback`, keeps the script's 70000, and — because
resolution is once per invocation — passes `-max-prompt-bytes 70000`
EXPLICITLY on every later leg, where by the ratified precedence
(`docs/DERIVE-PROFILE-DESIGN.md` §3) an explicit flag outranks the endpoint
profile. The profile is then unreachable for that whole sweep.

**Evidence (first hour of the memoire:D0259 deployment, image
`0.7.0-dev.g0c1297e`):** the 11:03 run logged
`sweep ceiling=70000 source=fallback endpoint=hl7-conductor` to
`/data/log/derive-auto.log`, and every leg's `meta_json` recorded
`max_prompt_bytes=70000` with no profile source. At 11:07:39 the `liaison` leg
sent a 57375-byte prompt; HL7 reported `n_tokens=32767 truncated=1` and the run
recorded `failed`.

**Why it matters:** memoire:D0259 makes the endpoint profile the single source
of window arithmetic for the only scheduled derive lane, and
`docs/DERIVE-PROFILE-DESIGN.md` exists because a byte ceiling shared across
models with different tokenizers and contexts is a latent bug class — the one
the 2026-08-24 empty-content autopsy diagnosed. This defect reinstates that
exact class silently: the manifest declares 32k geometry, the log says
`source=fallback`, and the operator's only signal is truncated prompts and
failed runs. A declaration that cannot be depended on is worse than no
declaration, because it is believed.

**Hypothesis (either half suffices; both are cheap):** in the script, resolve
only when the block is actually present — move the `CEILING_RESOLVED=1` latch
below the parse and keep probing on a bodiless outcome, so a skip or an error
costs a later probe rather than the whole sweep's geometry. In the CLI, emit
`effective_limits` on EVERY `-strategy=model` exit path, dry-run and execute,
skip and error alike — the same reasoning review r1 F2 already applied to the
`fully_covered` dry-run skip ("a caller reading the endpoint's geometry must
not have to hit a non-skipped window to learn it") simply carried to the
remaining paths. Prefer both: the script fix makes the sweep robust to any
future bodiless exit, the CLI fix makes the contract honest.

**Status 2026-09-07:** open. INTERIM MITIGATION deployed in the same act as the
filing: the hourly lane pins `MEMOIRE_SWEEP_MAX_PROMPT_BYTES=43929` in
`cdk8s/main.ts`, which is the profile's own
`floor((32768 − 12288 − 512) × 2.2)` by construction. The pin buys correctness
at the cost of the property the profile slice was built for — the number is
once again a constant in a manifest rather than data on an endpoint — so it is
explicitly temporary. `scripts/tests/cdk8s-manifest-contract-test.sh` recomputes
the ceiling from the registry literal and asserts the pinned env equals it, so
the two cannot drift while the pin stands; both the pin and that assertion are
removed when this item is consumed. Note the mitigation does NOT cover a future
endpoint whose profile differs from this lane's — it is a per-lane constant
again, which is the whole reason to fix the probe rather than live with it.

**CLOSED 2026-11-01 (consumed — moot; L0010 ruling a13 = A).** Hindsight owns
derived memory and Memoire's derivation lane is retired: the
`memoire-derive-sweep` CronJob, its `memoire-model-endpoints` registry
ConfigMap and the `MEMOIRE_SWEEP_MAX_PROMPT_BYTES` pin left the manifest, and
the manifest contract test's ceiling assertion went with them. The probe defect
in `scripts/memoire-derive-sweep.sh` is unfixed and would return with the lane;
reviving derivation means reopening this item first. The body above is
preserved as the filing record.

## 12. ~~A guard-skip leg counts as a success, so the all-fail exit gate cannot fire on a real sweep — four days of 100%-failed hourly runs reported green~~ (live incident 2026-09-07 — CLOSED 2026-11-01)

**Observation:** the D166/memoire:D198 all-fail gate
(`scripts/memoire-derive-sweep.sh:491`) fires only when
`LEGS_FAILED + LEGS_STATUS_FAILED == LEGS_ATTEMPTED`. `run_execute`
(`:290-309`) increments `LEGS_ATTEMPTED` for EVERY leg it runs, including the
ones the binary turns away before any provider call: the min-content guard skip
(`services/api/cmd/derive/main.go:818-828`) and the denylist skip
(`main.go:692-702`). Neither skip JSON carries a `status` key — verified by
running the script's own `json_str` against a `degenerate_window` skip body, which
returns empty — so `json_str "$out" status` is not `"failed"` and the leg lands
in the denominator as a SUCCESS. One thin session in the enumeration is
therefore enough to hold the gate shut for the whole sweep, however many real
legs failed. Thin sessions are not rare: the derive bench's own 11-session
reference set carries six `degenerate_window` skips.

**Evidence (from `derivation_runs` on memoire-pg, read 2026-09-07):** after the
HL3 alias rename of 2026-09-03 the hourly `hl3` lane failed every real leg for
four days — 09-04 123 failed / 0 completed, 09-05 109/0, 09-06 131/0, 09-07
87/0 before the repoint — each with
`modelderive: local-qwen endpoint "hl3" unavailable: model "hl3-qwen3.6-35b-a3b-iq3_xxs" not served by endpoint (serving: qwen3.6-35b-a3b-ud-q4_k_xl-solo)`
at stage `provider_call`. Every hourly Job nonetheless reported `Complete 1/1`
in about 40 s, so the CronJob failed-run rule in the homelabs vm-stack
(`vmrules-cronjobs.yaml`, textual reference — infra repo, homelabs
`hl3/infra/victoria-metrics/`) had nothing to fire on. Only the nightly HL5
lane derived anything for those four days, and nobody noticed.

**Why it matters:** this is the third instance of the class D166 and
memoire:D198 were each written to close — the 42P01 window (every leg rc=1,
lane recorded success) and the 89-failures night (every run `status=failed` at
rc=0). Both fixes judged the LEGS; neither noticed that the denominator admits
legs that never attempted anything. The lane's only supervisor is its exit
code, so a gate that cannot fire in the presence of one thin session is a gate
that does not exist in production. Worse than the silence: the sweep's honest
per-leg log lines were all there, and the green exit is what stopped anyone
from reading them.

**Hypothesis (two independent layers; the second does not depend on the first):**

1. *Sweep gate.* Count only legs that actually reached the provider. Move the
   guard and denylist skips out of `LEGS_ATTEMPTED` into their own
   `LEGS_SKIPPED` counter — a skip is a no-op, not a success — so the gate reads
   "every leg that TRIED, failed". This preserves D166's ruled scope exactly:
   partial success (any leg completed or partial) still records lane success,
   and structural skips (`split_depth_exceeded`, `unsplittable_window`) already
   sit outside both counters. Detection is a positive test on the skip JSON's
   `skipped` key rather than the absence of `status`, so a future skip shape
   cannot silently rejoin the denominator. Regression cover belongs beside the
   existing all-fail assertions in `scripts/tests/sweep-split-test.sh`: one
   sweep of N failed legs plus one guard skip must exit non-zero.

2. *Alerting, independent of any exit code.* The series already exist and are
   simply unwatched: `memoire_derive_consecutive_failed_runs` and
   `memoire_derive_failed_runs_recent{genre}` are exported by
   `services/api/internal/handlers/metrics.go:241-246`, and
   `deriveFailureGenre` already classifies this exact message as
   `client_config` (`metrics.go:336`). No alert rule consumes either one — the
   memoire rule group (infra repo, homelabs
   `hl3/infra/victoria-metrics/memoire-metrics.yaml`, textual reference) covers
   metrics-down, hybrid fail-open, embeddings staleness, freshness-series
   absence and search-engine degradation, and stops there. A rule on
   consecutive failed runs, and one on a non-zero `client_config` genre, would
   have paged within the first hour of 09-04 no matter what the sweep exited.
   That half is an infra-repo change, filed here because the gap is memoire's
   to name and the metric is memoire's to keep exported.

**Status 2026-09-07:** open, both layers. The specific misconfiguration that
exposed it is already gone — the hl3 registry alias was corrected in the
memoire:D0259 slice — but nothing about the gate or the missing rule changed,
so the next endpoint-level misconfiguration is silent again for as long as
nobody reads the logs. Filed together with MISSING #11: both are defects in the
same lane's honesty surface, one making a declared limit unreachable and this
one making a dead lane look alive.

**CLOSED 2026-11-01 (consumed — moot; L0010 ruling a13 = A).** Closed with
MISSING #11: no scheduled sweep runs, so its all-fail gate guards nothing. The
gate defect is unfixed in the script and returns with the lane. The body above
is preserved as the filing record.

## 13. Plain queries on `/v1/search` and `/v1/context` answered `unrecognized node type: -1` on the hub, then stopped — root cause unknown (live incident 2026-09-29; operator ruling 2026-10-01)

**Observation:** on 2026-09-29 seven plain queries through `/v1/search` and
`/v1/context` answered `HTTP 400 unrecognized node type: -1 (SQLSTATE XX000)`,
a Postgres planner error. `unrecognized node type` is a generic Postgres
node-dispatch error; the pg_search extension's custom scan (the `o.text ||| ?`
/ `pdb.score` read in `searchFTS`) is the most likely source, unproven. The
seven queries, by shape: plain words run against projects including
`compaction-lab`, `personal-infra` and `liaison`; six phrases of four to eight
words and one single word (the latter answered normally through
`memoire_context` with the same arguments); none used `OR`, quotes or any
operator. The verbatim list is kept in the private audit lane,
not here.
`SanitizeFTS` quotes every token, so the query text is not the evident
trigger.

**Not reproducible on 2026-10-01:** the same seven queries answer normally
after the latest bulk import of older history (no evidence file kept). The
hub logs held no trace of the failure: the read handlers mapped every store
error to 400 and logged nothing.

**Why it matters:** a planner error that comes and goes is an index or plan
state fault (candidates: pg_search index segments mid-merge or mid-import, a
stale plan against a changed table, an extension version edge), and the read
surface was reporting it as the caller's mistake.

**Done 2026-10-01 (observability only):** read handlers now classify store
errors. `db.ErrInvalidQuery` (empty query) and `db.ErrBadCursor` stay 400;
every other store error is 500 with a generic body plus `request_id`, and one
log line carries `request_id`, route, `sqlstate`, message, `query_len`,
project, kind and limit (never the query text). The next occurrence carries the
request id and SQLSTATE in the hub log (`kubectl -n memoire logs deploy/memoire-hub`;
see `docs/RUNBOOK.md`, "Read-surface errors").

**Open:** find the root cause. First step: on the next 500 with
`sqlstate=XX000`, record the request id, re-run the same query with
`EXPLAIN` on the hub, and check the `pg_search` index state on the hub
(segment and merge status) and any import or
`REINDEX` running at that time.

## 14. The forwarder posts one item per request; batch ingest would lift the ceiling (forwarder-priority lane, 2026-10-01)

**Observation:** `ForwardOnce` (`services/api/internal/forwarder/forwarder.go`)
sends one `POST /v1/events` per queue item and waits for the ack before the
next. Measured 2026-09-30 13:11: 949 items in 20 s, about 47 items/s. A
first import by the Codex/OpenCode readers queued 143,306 items (about 51
minutes of forwarding); a 76,242 burst, about 27 minutes.

**Mitigation (branch `fix/forwarder-priority`, 2026-10-01):** adapter
priority plus bounded slices (`docs/RUNBOOK.md`, "Remote capture
forwarding"): live items of the priority adapters (default `claude-code`, the
Claude hook) no longer wait behind a backlog. Total drain time of the backlog
is unchanged. Limitation: the live Codex and OpenCode capture triggers write
items under the readers' own adapter names (`codex-cli`, `opencode-native`),
the same names as the catch-up backlog, so the file name cannot tell a live
turn from an import and those adapters are not prioritised; listing them in
`MEMOIRE_FORWARD_PRIORITY_ADAPTERS` would promote the whole backlog with
them. The remedy is producer-side: a distinct adapter or a file-name tag for
trigger-written items.

**Hypothesis:** the hub already exposes `POST /v1/events/batch`
(`handlers.CreateEventBatch`, routed in `server/server.go`). Moving the
forwarder to it is client-side work: send N items per request, delete the
local files of the items the response lists as queued, keep the ones it lists
as rejected under the existing poison rules (a per-index `rejected` entry
maps to the failed/ move). Idempotency was checked: the key is the hash of
the envelope's own `source_adapter:source_event_id` (`db.EventInput.IdempotencyKey`).
The batch route's `"http-batch"` argument to `queuewriter.WriteBytes` only
names the hub-side queue file, and the single route's `"http"` does the same;
the `http-body-<sha256>` stamp for envelopes without `source_event_id` is
computed the same way on both routes, and matches across routes only when
the batch client embeds the file bytes verbatim (every client queue file
carries a `source_event_id`, so the stamp is not what identifies them). A
first batch pass therefore does not double the corpus.

**Open points:** the batch response carries queue names, not a per-item ack
in request order beyond `index` in `rejected`; the client needs a request
size bound (body size, N) and a partial-failure rule for a 5xx on a batch
(whole batch stays pending; re-send is idempotent).

**Status 2026-10-01:** open.

## 15. The Qwen adapter is not scheduled on a workstation and drops user prompts by default (three-track audit, 2026-09-29; operator ruling 2026-10-01)

**Observation:** `scripts/memoire-capture-qwen-rollout.sh` exists, but nothing
runs it on a workstation: no timer, no capture switch. User prompts are
emitted only with `--include-prompts`. The Codex and
OpenCode readers were in the same state and were brought live on
2026-09-30 (timer, switch, prompt capture on); Qwen was left out because it
was not in use.

**Open:** give the Qwen reader the same shape as the Codex one: a timer
unit, the capture switch, prompt capture on by default, a registry entry for
the wiring, and a 30-day catch-up at first run.

**When:** Parked without a deadline (operator ruling 2026-10-02): Qwen is no longer used on the fleet; no work planned.

**Status 2026-10-02:** parked.

## 16. OpenCode reader: a bounded run spends its budget re-reading every candidate session (30-day catch-up, 2026-09-30)

**Observation:** during the 30-day catch-up
(`--scan --summary --quiet-for 120 --since @<30d> --max-seconds 12`), 103
consecutive runs from 18:04 to 19:47 each reported
`candidates=825 reads=826 skipped_unchanged=799 sessions=1 events=0 stopped=time left=26`.
A run reads every candidate session (about 30 ms each at most) before it
processes any, processes one session and stops on time; the 26 sessions
reported as left never advance. A larger time budget (the timer's) hides it.
The cost of a run is proportional to the number of candidates, whatever
changed.

**Hypothesis:** keep a per-session change marker (updated time or row count)
in the cursor file so an unchanged session costs no read, and order the
candidates so the sessions left by the previous run are processed first.

**Scope:** catch-up mode. The live timer is unaffected as long as the
candidate count stays small.

**When:** in the catch-up round (with #14 and point 1 of #17), before the
full import of history older than 30 days.

**Status 2026-10-01:** open.

## 17. Codex/OpenCode readers: the quiet-for gate hides a backlog, and a session that never goes quiet is not captured while it runs (30-day catch-up, 2026-09-30)

**Observation:** the catch-up (`--quiet-for 120`) reported
`stopped=0 lines_left=0` at 18:04 while 25 Codex sessions that had been
active all afternoon were deferred at every chunk. Their backlog (76,242
events going back to 2026-09-27) surfaced at the first timer tick after they
went quiet, as one burst (about 27 minutes of forwarding, see #14).

**Two points:**

1. The summary does not count gated lines: a deferred backlog is reported
   as nothing left. `left` / `lines_left` should include the lines held back
   by the gate, so the deferral is visible.
2. A continuously active session is never captured while it runs. The
   capture triggers (`MEMOIRE_HOOK_CAPTURE`, off by default) are the
   mechanism meant to close this; whether they do for a long-running session
   is unverified. If they do not, the gate should capture up to the last
   closed turn instead of skipping the whole file.

**Mitigation in place:** the forwarder priority (#14) keeps such a burst
from delaying Claude hook events.

**When:** point 1 in the catch-up round (with #14 and #16). Point 2 at the
switch-on of the capture triggers, with a live check on a long session.

**Status 2026-10-01:** open.

## 18. Results of Memoire's own read tools are captured back into the store (review of the plugin-part fix, 2026-10-01)

**Observation:** when an agent calls a Memoire read tool (search, context,
timeline, observations), the result is captured like any tool result, on
every adapter: the Claude `PostToolUse` hook is wired with no matcher and
keeps the raw hook payload, `tool_response` included
(`scripts/memoire-capture-claude.sh`); the OpenCode reader queues every tool
part; the Codex reader queues every function-call output. Store content
therefore re-enters the store as raw events. This is a different path from
the plugin's injected parts, which the OpenCode reader no longer queues
since 2026-10-01.

**Why it matters:** the record is legitimate (it is what the agent asked for
and saw), but derivation and search then read Memoire's own output as if it
were new evidence, and any measurement of retrieval quality counts it.

**Open, needs a ruling first:** keep the event and mark it (so derivation and
search can exclude it), strip the result body at capture, or leave as is.
Related, undecided: whether Codex persists SessionStart hook context into its
rollout (not determinable from the reader's code; a run in a scratch Codex
home with a marker would settle it), and the plugin-part events ingested
before 2026-10-01, which remain on the hub.

**When:** before the retrieval-quality measurement, with #13.

**Status 2026-10-01:** open.

## 19. ~~No alert outside sessions when a client's forwarding queue is stuck or dead~~ (injection switched off, 2026-10-01; operator ruling 2026-10-02 — CLOSED 2026-10-03)

**Observation:** the only place a stuck or dead forwarding queue on a client
was announced was the session-start injection (the CAPTURE BACKLOG and
TRANSPORT DEAD blocks, and the clock alert). Injection is off by default
since 2026-10-01, so nothing announces it any more. The state is still
recorded: `memoire-forward -status` and `$MEMOIRE_HOME/forward-status.json`
carry the queue depths, the last success, the last error and the count of
consecutive failures, but someone has to look. The hub's `/metrics` and
`check-health.sh` see only the hub's own queue.

**Why it matters:** capture can stop on a workstation with no visible sign;
the gap is found only when events are missing.

**Hypothesis:** a check on each client that reads `forward-status.json` and
the queue depths on a timer and raises an alert through a channel that does
not depend on a session (desktop notification, the monitoring stack, or a
line the hub exposes per client). The thresholds the injector used (backlog
age, consecutive failures, clock offset) are the starting point. The channel
is an operator choice.

**When:** in the catch-up round (with #14, #16 and point 1 of #17).

**Status 2026-10-02:** open.

**CLOSED 2026-10-03 (consumed — built, ahead of Hindsight, per the 2026-10-02/03
operator rulings).** `scripts/memoire-capture-health.sh` is a check with no
credential that reads the local queue, `forward-status.json` and the state of
the capture units (`memoire-forward.service` and the three capture-tick timers),
and optionally the hub's unauthenticated `/v1/health` clock count. It takes the
injector's thresholds and knob names (depth 25, oldest 600 s, dead 21600 s,
failed items), exits 0, 1 or 2 for ok, warn and critical, and prints its verdict
as JSON with `--status`. The user timer `memoire-capture-health.timer`
(`scripts/capture-health-timer.sh`, every 15 minutes) alerts through a desktop
notification (`notify-send`; one per state change, a reminder every six hours,
one notice on recovery; no error on a host with no bus) and through a pluggable
sink, `MEMOIRE_HEALTH_SINK`, which receives the status JSON on every change,
reminder and recovery. The poster to the operator's Alertmanager is outside this
repository and is written against that sink. Cover:
`scripts/tests/capture-health-test.sh` (`make test-ops`). Runbook: "Capture
health alert" in `docs/RUNBOOK.md`. The body above is preserved as the filing
record; nothing is deleted.

## 20. The Great Book edition describes a Memoire that derives and serves memory (L0010, 2026-11-01)

**Observation:** the v0.6 Great Book (`docs/great-book/`) is built on the memory
thesis: chapters 1 and 2 follow a captured remark into a derived observation in
front of a model, chapter 4 operates the derive and embed lanes, chapter 6
documents the derived read defaults and the old MCP declarations. L0010 (rulings
a13 = A, a14 = A) retired derivation and made the read surfaces a raw, dated
recent lookup, with Hindsight as the memory layer. The chapters now carry an
edition note pointing here; their text is not rewritten.

**Why it is in MISSING:** the v0.7 goal ships a Great Book edition; shipping one
that describes the retired lane is the staleness class MISSING #10 closed.

**Hypothesis:** one edition pass after the L0010 implementation converges:
chapter 1 restates identity as capture, custody and source of record for
Hindsight; chapter 2 follows an event into custody, the recent lookup and the
Hindsight import; chapters 4 and 6 drop the retired lanes and document
`include_derived`, `occurred_after` and the MCP `days` parameter; chapter 5
drops the Liaison read-back feed ("The feed pulls memory back out" and its
`liaison/MEMOIRE-FEED-CONTRACT.md` pointers), retired 2026-11-01 — liaison L0005.

**Status 2026-11-01:** open.

**Status 2026-11-02:** edition written (a25 = A). The v0.7 Great Book (index and
chapters 01–06, 01 and 02 retitled), README.md and QUICKSTART.md describe the
post-L0010 system; independent reviews `2026-11-02-memoire-great-book-v07-edition`
(3 rounds, PASS) and `2026-11-02-memoire-readme-quickstart-v07` (PASS with two
minor residuals, fixed). Still open: `VERSION`'s goal and the book's
public-versus-private sentences wait for the repository decision (L0011);
chapters 3 and 4 get a pass after the R2 window experiment.
