# Governance Layer Design: Provenance, Use-Policy, Recall Traces

Rework boundary: this document describes existing behavior and compatibility.
The approved target direction and agent discretion live in the maintainers'
rework mandate (private ledger, `policies/rework.md`). Existing review gestures and read filters
do not prescribe the replacement architecture. Unrelated evidence, privacy and
published consumer obligations remain applicable.

Status: RULED 2026-07-02 (see "Rulings" section) — Q1–Q5, Q7 ruled; Q6 and Q8 direction
set, details at implementation. Ruling 7 (defense-at-source redaction) leads v1. Date: 2026-07-02.

Provenance note: the mechanisms below re-derive patterns proven in OB1's
agent-memory layer. OB1 is FSL-1.1 licensed; this document copies no OB1 prose
or SQL DDL — each mechanism is re-stated from first principles onto Memoire's
substrate. Two enum vocabularies (the provenance value set and the
review-action verb set) are adopted from OB1 verbatim and flagged as such
inline. Disposition and borrow list: `inbox/2026-07-02-liaison-ob1-pattern-borrows.md`.

## Core Principle

Agent-written memory starts as evidence, never instruction. The only path to
instruction-grade — a memory that may silently steer future agent behavior —
is human confirmation.

This is not a new stance; it composes three things Memoire and Liaison already
hold:

- Memoire's review surface: `review_state` (`proposed → accepted/rejected/lapsed`,
  D90–D101) already gates which observations count. Governance extends this
  gate; it does not add a parallel status field.
- Liaison's **Judge-always** rule: no model self-certifies its own output.
  Applied here: no derivation run marks its own observations trustworthy.
- Liaison's **curated gate** / `promotion_status` pattern (D72): derivation
  proposes, a human (or explicitly-authorized frontier model) promotes.
  Governance reuses this vocabulary verbatim — no synonyms.

The D46 split stays intact: Memoire is the dumb event substrate, window query,
and **produced-in-window** corroboration; Liaison owns classification,
evidence choice, Judge verdicts, and the **exists-now** check. Everything in
this proposal stays on the Memoire side of that fence. (Scope note,
liaison:D230: the Memoire tool-window is ONE of two admissible corroboration
modes — it covers harness-session evidence; dispatched-worker evidence
corroborates via engine-synthesized handle windows, which never transit
Memoire.)

## Mechanisms

Each mechanism maps to a concrete additive change (migration sketched in
prose only; no SQL in this proposal). All writes go through the Go API — sole
write authority. All schema sketches follow the sanctioned additive shapes:
new nullable columns, new append-only tables, new event types, new triggers.

### 1. Provenance status

Where a memory's claim came from, orthogonal to its review lifecycle.

Vocabulary (five values): `observed` (backed by a concrete captured artifact —
tool event, file write, commit), `inferred` (a model's conclusion from
observed evidence), `user_confirmed` (human validated or corrected it),
`imported` (from an already-trusted external system), `generated` (produced
mid-task, no cited source). This five-token set is OB1's `provenance_status`
vocabulary adopted verbatim (identical spelling and order) — a declared
vocabulary borrow, not a re-derivation; the definitions above are Memoire's.

OB1's two further states, superseded and disputed, are deliberately **not**
provenance values here: Memoire already has `superseded` in `review_state`,
and disputed becomes a review transition (mechanism 3). Provenance answers
"where did this come from" — origin is immutable-ish; lifecycle lives in
`review_state`. One state machine per axis.

Migration sketch: `ALTER TABLE observations ADD COLUMN provenance TEXT`
(nullable; existing rows backfillable: `kind='raw'` → `observed`, derived →
`inferred`). No DB CHECK, matching the `review_state` precedent (D98): value
set enforced in app code. Queryable, hence a column, not `meta_json`.

Invariant: derivation runs may only write `observed`, `inferred`, `generated`.
`user_confirmed` is set exclusively by the review transition engine on a
human confirm action. `imported` requires an explicit import endpoint (later).
(CORRECTION 2026-07-18, memoire:D136 R-1: MODEL-derived rows may only write
`inferred`/`generated` — a model-claimed `observed` is downgraded to
`inferred` at parse, fail-open, counted in run meta `provenance_downgrades`.
`observed` remains legitimate for the capture path and store-level writes.)

### 2. Use-policy flags + hard invariant

Three per-observation flags: `can_instruct` (may be injected as a behavioral
rule), `can_evidence` (may be cited as supporting evidence), and
`needs_confirmation` (surfaced to review queue before any use). *(2026-07-10:
the operator-facing act of setting `can_instruct` — "elevation" — is ruled in
`ELEVATION-DESIGN.md`: vocabulary, gates, and slices.)* OB1's implied
fourth policy — searchable but never auto-injected — is covered here by
`can_instruct=0 AND can_evidence=1`, decided explicitly rather than inherited
as a gap.

Migration sketch: three `ALTER TABLE observations ADD COLUMN ... INTEGER`
columns, defaults `can_instruct=0, can_evidence=1, needs_confirmation=0`.

The hard invariant: **`can_instruct=1` requires
`provenance IN ('user_confirmed','imported')`.** Enforced at the data layer
so a future write path or app bug cannot promote speculation to
instruction-grade: sketch is a pair of `CREATE TRIGGER` objects (INSERT +
UPDATE, additive schema objects) that RAISE on violation, mirrored in the
transition engine. This is the one place this proposal argues for DB-level
enforcement despite the app-only `review_state` precedent — the invariant is
the entire point of the layer (ratification question 3).

Per the D15 guardrail: flags are written from day one, but **no consumer**
(no injection surface) reads `can_instruct` until the schema is ratified
against a real consumer. This is settled D15 policy restated, not an open
question — it is not re-asked in the ratification list below.

### 3. Review-action verbs extending the accept/reject lifecycle

Current legal transitions (D95–D101, `TransitionObservationReview`):
`proposed→accepted`, `proposed→rejected`, `accepted→superseded`,
`accepted→soft_deleted`; `kind='raw'` locked at `accepted`.

The nine verbs below are OB1's review-action vocabulary, adopted verbatim
(only underscore→hyphen spelling changes) — a declared vocabulary borrow; the
mapping onto Memoire's transitions is this proposal's own. Mapped without a
parallel state engine:

- `confirm` = `proposed→accepted` **plus** provenance set to
  `user_confirmed` in the same transition. ~~Existing `/accept` gains this
  as its meaning when the actor is human-scoped.~~ CORRECTION (2026-07-19,
  memoire:D146 R-5): `/accept` does NOT carry this meaning — it is "seen,
  not contested, usable as memory" and leaves provenance untouched. The
  promotion is the dedicated `/confirm` route, and elevation requires a
  prior confirm (provenance `user_confirmed`/`imported`) instead of
  promoting provenance itself. D146 also added `lapsed`
  (`proposed→lapsed` by verdict/bulk/TTL; `lapsed→accepted|rejected`
  on-encounter; rewrite valid from proposed/lapsed/disputed).
- `reject` = existing `proposed→rejected`. Unchanged; row + logs preserved.
- `supersede` = existing `accepted→superseded`. Unchanged.
- `evidence-only`, `restrict-scope`, `mark-stale` = flag/metadata edits
  (`can_instruct=0`, scope narrowing in `meta_json`, staleness note), logged
  as review actions but **not** state transitions. Uses Liaison's
  `ttl_review` semantics for staleness — elapsed means "needs review", never
  auto-demote.
- `dispute` = one new state, `disputed`, with new legal transitions:
  `accepted→disputed`, `disputed→accepted` (re-confirm),
  `disputed→superseded`, `disputed→rejected`. Touch points: the matrix inside
  `validateReviewTransition`, the `allowedReviewState` value set, and the two
  value-set error strings in the store — all in `store.go`; without the
  value-set additions, writes reject before reaching the matrix. Endpoints
  stay thin wrappers per the D90 shape (`/dispute` wraps the one engine).
- `edit` and `merge` are deferred (need content-mutation semantics that
  collide with `content_hash` idempotency; not v1).

Every verb logs to `observation_review_log` (existing shape:
`observation_id, from_state, to_state, actor, reason_text, occurred_at_epoch`).
Flag-only verbs cannot ride `TransitionObservationReview` —
`validateReviewTransition` rejects `from == to` — so they need a separate
append-only write path into the same log, recording `from_state = to_state`
plus the verb in one nullable `action TEXT` column added to that table.
Recommendation: the column (queryable), still append-only.

### 4. Recall traces + audit events — as ordinary Memoire events

What was retrieved, what was offered, what was used. Modeled as normal events
through the existing capture queue — no new logging primitive, no new table.

New `event_type` values (free TEXT, zero migration, same precedent as the
`gotcha` kind):

- `recall_trace`: emitted by the read surface per retrieval.
  `payload_json` sketch: `{ query, scope: {project, session_id, principal_id},
  policy_snapshot: {flags_filter}, returned: [{observation_id, rank}],
  truncated: bool }`.
- `recall_feedback`: optional, emitted by the caller (Liaison, harness
  bridge) after the fact: `{ recall_trace_event_id, used: [ids],
  ignored: [ids], reason }`. Fence: **no consumer acts on feedback** — it is
  inert audit data; any auto-tuning of recall from it is an L3 memory-agent
  decision, explicitly out of scope here.

Governance decisions (invariant violations, scope-widening grants) log to the
existing `audit_log` (`resource_type='policy'`) — it is already generic
enough; no sibling table.

What the read surface gains: a future read-only endpoint in the
`GET /v1/events/tool-window` mold (D116) — session-scoped, non-authoritative,
self-disclosing its caveats (`recall_feedback` coverage is best-effort) —
that reconstructs "what was available vs what was chosen vs what happened
next" for any misbehaving session. Separates retrieval faults from model
faults from write-back faults.

Cost note: recall traces are reads writing events. v1 ships them behind an
opt-in flag on the read endpoint (ratification question 5).

### 5. Scope ladder + no-automatic-promotion

Memoire's real ladder is flatter than OB1's five tiers:
`principal < session < project < global`. These axes already exist as
indexed columns (`principal_id`, `session_id`, `project`) — no schema change.

The borrowed rule is behavioral: **no tier auto-promotes.** A
session-derived observation never silently becomes project-ambient; a
project observation never silently serves cross-project recall. Concretely:

- Derivation writes observations with the source window's `project` and
  `principal_id`; it never widens them.
- Recall defaults to same-project scope. Cross-project recall requires an
  explicit request parameter and produces an `audit_log` entry
  (`action='scope_widen'`).
- Widening an observation's own scope is a review verb (human), not a
  derivation or API-default behavior.

### 6. Runtime attribution

`derivation_runs` already carries `provider, model, endpoint_id,
data_boundary, prompt_version, input_hash` — provenance discipline exists at
run level. The gap is per-observation attribution for **foreign harnesses**
writing events directly (Codex, Qwen, Gemini, opencode): without it, a bad
claim can't be traced to one model's failure pattern vs one harness's bug.

Sketch: no new columns yet. Foreign clients are required (API-validated) to
include `runtime`, `runtime_version`, `model` keys in `events.meta_json`;
derivation copies the dominant attribution into each observation's
`meta_json`. Placement follows the stated evolvable/rebuildable pattern.
Note: AGENTS.md:23 assigns event detail to `payload_json`, not `meta_json`;
if ratification prefers honoring that rule as written, attribution moves to
`events.payload_json` instead — same mechanism either way. Promote to
columns only when a real query needs it (flagged as a later, separate
decision). No widening of the `derivation_runs.provider` CHECK is needed for
v1 — governance actors are review actors, not derivation providers.

### 7. Write-back allow/deny content policy

Two layers, neither an LLM:

- **Capture filter (API-side, at the single write authority):** deny raw
  transcripts-as-observations, secret-like strings (regex + entropy pass —
  implementation needed, not specified by the pattern), and oversized code
  blocks. Two further OB1 deny categories — scratch content and unsourced
  broad claims — are **not** rule-detectable: regex/entropy/size cannot judge
  them, and enforcing them would put a semantic judge on the write path,
  which the no-LLM-on-write non-goal forbids. They are deferred, not adopted
  (ratification question 8). Denied writes are rejected with a
  reason, not silently dropped. Raw *event* custody is unaffected — this
  filters observation write-back, not event capture.
- **Derivation-kind guidance:** the allow-list maps onto the sealed kinds
  with no new kind: decisions→`decision`, constraints→`constraint`,
  preferences→`preference`, lessons/failures→`gotcha`, open
  questions→`open_question`, next steps→`task`, outputs and artifact
  references→`fact`. A "policy" kind is explicitly **not** introduced —
  governance state lives in columns/flags/events, and the kinds list stays
  sealed (derivation version bump required to change it).

## Non-Goals

- No vectors, graphs, or semantic recall.
- No cloud dependency; local-first stands.
- No LLM-on-write: capture filter is rules-only (regex/entropy/size).
- No schema changes land from this proposal — migration sketches only,
  pending ratification (D15: no schema gets consumers until ratified against
  a real one).
- No claim of redaction or erasure: soft-delete remains visibility-only;
  deny-filter prevents new leaks but does not purge old ones.
- Nothing that breaks foreign-client read parity — every governance surface
  is plain HTTP + SQLite, readable from any harness.

## Sequencing Sketch

**v1 (cheap, high value):**

1. `provenance` column + backfill — unblocks the use-policy invariant.
2. Use-policy flags + trigger invariant — the structural guarantee;
   unblocks any future injection surface being safe by construction.
3. `confirm` semantics on `/accept` (provenance bump) + `action` column on
   `observation_review_log` — governance becomes visible in the existing
   review flow immediately.
4. Write-back deny filter (secrets/size rules) — closes the leak surface
   before more harnesses write.

**v2:** `disputed` state + `/dispute` endpoint; `recall_trace` /
`recall_feedback` event types (opt-in); scope no-promotion enforcement +
`scope_widen` audit entries.

**Later:** recall-trace read endpoint (tool-window mold); foreign-harness
attribution validation; `imported` provenance + import endpoint; `edit`/
`merge` verbs; attribution column promotion if queries demand it.

## Ratification Questions for the Human Operator

1. **Provenance vocabulary = five origin values only** (superseded/disputed
   stay in `review_state`)? Recommendation: yes — one state machine per
   axis. Strongest counter: OB1 proved the seven-state shape as a single
   field; splitting it means trust status requires reading two columns, and
   future OB1-pattern reuse maps less directly.
2. **`provenance` as a real column, not `meta_json`?** Recommendation:
   column — it must be queryable and invariant-enforceable. Counter:
   AGENTS.md leans against promoting detail to columns without a decision;
   `meta_json` first would be the conservative default.
3. **DB-level triggers for the can_instruct invariant?** Recommendation:
   yes — triggers + app enforcement; the invariant is the layer's whole
   value. Counter: repo precedent (D98) is app-only enforcement; two
   enforcement layers can drift, and while the schema already carries three
   mechanical FTS-sync triggers (schema/0001), these would be the first
   triggers enforcing a business invariant.
4. **Add `disputed` to `review_state` with the four transitions named
   above?** Recommendation: yes, via the one transition engine (the three
   `store.go` touch points named in mechanism 3).
   Counter: the transition matrix is deliberately small and sealed-ish; a
   `disputed` flag in `meta_json` would avoid touching it at all.
5. **Recall traces opt-in (v1) vs default-on?** Recommendation: opt-in —
   reads writing events is a volume and self-amplification risk. Counter:
   opt-in telemetry never gets turned on; the debugging value only exists if
   traces are there *before* the incident.
6. **Cross-project recall requires explicit parameter + audit entry?**
   Recommendation: yes. Counter: cross-project memory is Liaison's founding
   use case; friction here taxes the main workflow to guard against a
   leak-class Memoire's single-user reality may not have yet.
7. **Deny filter placed API-side only?** Recommendation: yes — single write
   authority is the one chokepoint. Counter: by the time a secret reaches
   the API it has already left the producing client; defense-at-source
   (hook-side pre-filter) is strictly safer, at the cost of per-harness
   duplication.
8. **Defer the non-rule-detectable deny categories (scratch content,
   unsourced broad claims)?** Recommendation: yes — the v1 filter covers
   secrets, size, and transcript shape only; keeping the other two means a
   semantic judge on the write path, which the no-LLM-on-write non-goal
   forbids. Counter: those two categories are where most low-value
   write-back actually originates; a rules-only filter that skips them may
   pass mostly noise and discredit the filter.

## Rulings (Human, 2026-07-02)

Recorded verbatim-in-substance from the human operator's ratification round; journal markers under D119–D125.

1. **RULED — yes.** Five origin values only; `superseded`/`disputed` stay in `review_state`.
2. **RULED — yes.** `provenance` is a real column (queryable, invariant-enforceable), not `meta_json`.
3. **RULED — yes, condition verified.** Triggers land as replayable versioned migrations. The
   condition ("the whole schema should be replayable — if not, fix now") is already satisfied:
   `store.migrate` applies `schema/*.sql` in order against the `schema_version` table; a fresh DB
   replays 0001–0006 identically. Trigger migrations inherit replayability by construction.
4. **RULED — yes.** `disputed` becomes a `review_state` value with the four named transitions,
   through the one transition engine.
5. **RULED — opt-in stands, with instrumentation required.** The counter is acknowledged as sharp;
   volume is not the present concern, self-amplification is hard to anticipate. v1 ships opt-in
   AND measurable: trace-volume and trace-of-trace counters (events referencing recall events)
   surfaced via stats, so the opt-in/default-on call can be revisited on data instead of instinct.
6. **DIRECTION SET — scope is decided at derivation, not capture.** Raw events stay project-scoped
   by construction. Scope-lifting is a derivation output: obvious cross-project material
   (discipline/hygiene/tooling) may be proposed `global` by the model, obvious project-specific
   stays project-scoped, and the in-between enters a review lane requiring human validation. This
   is not a new "read that writes" pattern — derivation already is that loop, and second-stage
   rescoping rides the existing review-log append path. No automatic promotion: any widening
   beyond project requires the review gate in v1; auto-accept thresholds for the obvious lane wait
   for Q5-style instrumentation. The explicit cross-project recall parameter + audit entry stands.
7. **CHALLENGED — defense-at-source, not API-side only.** Ruled direction: the deny/redaction
   layer moves to a pre-API position — capture boundary (queue writer / capture adapters) redacts
   secret-shaped content BEFORE it reaches queue files, `raw/` custody copies, or the API; the
   API-side filter remains as backstop. Driver is a live real-world failure: Claude Code hooks
   ship changed secret-file contents into the API today. Rules-only redaction (no LLM on write):
   secret-shaped patterns, env/PEM/key-material heuristics, size caps. This supersedes the
   mechanism-7 placement above and is the highest-priority implementation item in this design.
8. **RULED (completed 2026-07-02, second round — no pushback).** Scratch content and unsourced broad
   claims get a classification lane, fully specified:
   (a) the tag is a **flag on the observation** (`noise_candidate`), never a new sealed kind —
   kind stays what-it-is; noise-candidacy is an orthogonal quality judgment, costing zero schema
   beyond the governance v1 columns;
   (b) **the deriving model tags at derivation time** (no extra LLM call); the periodic pass only
   REVIEWS the lane, it never populates it;
   (c) keep/drop rulings land in an **append-only feedback log** (gate-log shape) that the
   periodic pass reads for calibration — "here is what the human operator kept last time";
   (d) hard fence: the lane **never gates capture** — silent dropping through the back door is
   forbidden; it only filters default read surfaces.

Status effect: mechanisms 1–4 are ratified as specified (with ruling 7 superseding mechanism 7's
placement); sequencing reorders — **v1 now leads with the ruling-7 redaction layer**, then
provenance column + invariant + confirm semantics.
