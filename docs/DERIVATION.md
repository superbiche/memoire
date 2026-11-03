# Observation Derivation Contract

Status: sealed as the v1 implementation brief on 2026-05-26.

This contract defines how Memoire turns raw captured events into durable memory
observations. It is intentionally written before schema or worker
implementation. The human operator accepted the observation kind ontology on 2026-05-26.

## Goals

- Preserve raw custody as the source of truth.
- Make derived memory replayable, auditable, and replaceable.
- Keep provider sovereignty visible in every derivation run.
- Avoid making any producer hook responsible for memory policy.
- Leave a clean slot for future assistant-turn transcript ingest.

## Non-Goals

- No direct `Stop` / assistant-output hook capture in this slice.
- No claim that soft-delete solves secret deletion.
- No vector, graph, MCP, or autonomous memory-agent work.
- No automatic replacement of accepted observations on derivation version bumps.

## Source Of Truth

Raw files under `~/.memoire/raw/` are canonical custody. SQLite `events`, `raw`
observations, and derived observations are rebuildable indexes over raw custody.

Soft-delete is only visibility control. Real deletion remains blocked on the raw
custody purge/rebuild policy tracked as `MISSING.md` #17 in the parent Liaison
planning layer. Derivation work must not weaken that gate or imply that secret
deletion is solved.

## V1 Input Boundary

A derivation run consumes a bounded event window:

- `project` is required.
- `session_id` is preferred for v1 windows.
- Time bounds are recorded as `[start_epoch, end_epoch]`.
- V1 event types are `claude_UserPromptSubmit` and `claude_PostToolUse`,
  plus `liaison_evidence` since 2026-07-18 (memoire:D138 — rule R005,
  `docs/LIAISON-EVIDENCE-CONTRACT.md`; no historical window's composition
  changed, the type did not exist before that date).
- Events are ordered by `(occurred_at_epoch, id)`.

`session_id` may be omitted only for explicit cross-session derivation, such as
project-level synthesis, duplicate detection across sessions, or review of a
time-bounded project window. Cross-session runs must record why the session
boundary was widened in provenance metadata.

Known limitation: v1 derivation sees user prompts and tool-use
metadata/results captured through `UserPromptSubmit` and `PostToolUse`; it does
not see assistant reasoning, assistant prose, hidden chain-of-thought, or
complete assistant turns. This biases v1 toward intent/actions rather than full
conversation synthesis. The bias is acceptable only because provenance records
the input boundary and model-derived outputs default to review.

Future `assistant_turn` events from JSONL transcript ingest should plug into the
same window contract as another event type. Direct `Stop` capture remains
backlog until assistant-output retention and redaction policy is explicit.

## Provider Sovereignty

Every derivation run must record its provider and data boundary. There is no
implicit external-model default.

Allowed initial `provider` values:

- `rules`: deterministic local derivation with no model call.
- `local-qwen`: local model path, first-class in schema/config from day one.
- `openai`: explicit external provider.
- `manual`: human/operator-authored derivation or review.

`manual` is for operator backfill, curated imports, and explicit human review
actions that create or modify observations without delegating the judgment to a
model.

Allowed `data_boundary` values:

- `local`: raw material did not leave the local trust boundary.
- `external`: raw material was sent to an external provider.

Runnable v1 may ship with `rules` and `openai` first, but `openai` must be
explicitly configured and must write `data_boundary="external"`. `local-qwen`
must be implemented before Memoire scales beyond Claude shadow mode or before
external producer onboarding, unless the human operator explicitly resets that gate.

## Output Shape

Derived observations are additional rows in `observations` with non-`raw` kinds.
Each output has:

- `kind`
- `text`
- `confidence`
- `project`
- `session_id`
- `occurred_at_epoch`
- `created_at_epoch`
- `review_state`
- `derivation_run_id`
- `content_hash`
- provenance metadata

`occurred_at_epoch` is the latest source event timestamp that contributes to the
specific observation, not the derivation window end. Window end belongs in run
metadata.

## Kind Ontology

V1 kinds are shaped by retrieval needs, not producer output categories. A kind
answers the question: "what will a future consumer ask Memoire to retrieve or
filter?"

Accepted v1 kinds:

- `decision`: a settled technical, product, or sequencing decision.
- `preference`: durable human operator/project preference.
- `constraint`: rule, safety boundary, non-goal, or policy limit.
- `task`: actionable follow-up or re-entry point.
- `fact`: stable project/runtime fact.
- `open_question`: unresolved question, design fork, uncertainty, or decision
  that still needs human operator review.
- `gotcha`: an expectation-violating, evidence-backed causal surprise with a
  concrete cost or failure mode — "what bit us, and why."

Pre-run v1 amendment (2026-06-13, human operator ruling): `gotcha` was added before any
model observations existed. No `derivation_version` bump and no schema migration
are required: `kind` is unconstrained `TEXT` in the schema, so a new kind is
immediately write-eligible.

`summary` is intentionally not a v1 observation kind. It is too easy to use as a
fallback bucket when the derivation cannot choose a retrieval-shaped category.
Prompt-ready context summaries can still be generated later as read-side
artifacts or as a separate summarization layer, but durable observations should
prefer the narrower kinds above.

The `decision` / `constraint` overlap is real and tolerable in v1. A decision
records what was chosen; a constraint records the rule or boundary that should
shape future choices. The same source evidence may produce both when both
retrieval shapes are useful. Same-text cross-kind collisions remain different
typed observations with duplicate-content warnings, not automatic merges.

Overlap guidance for adjacent kinds:

- `gotcha` vs `fact`: a `fact` is a neutral, stable truth ("the API returns
  dates in epoch seconds"). A `gotcha` is a resolved surprise with a cost
  ("calling `/orders` with no filter silently re-queues all open orders —
  expected idempotent read, got destructive side-effect"). Use `gotcha` when the
  knowledge is "this bit us" not "this is how it works."
- `gotcha` vs `constraint`: a `constraint` is prescriptive ("do not call
  `/orders` without a filter"). A `gotcha` is descriptive-cautionary ("we called
  it without a filter and it re-queued everything"). The same incident can
  produce both: the `gotcha` records what happened; the `constraint` captures the
  rule it should enforce going forward.
- `gotcha` vs `open_question`: an `open_question` is unresolved. A `gotcha` is
  resolved — the surprise already occurred and the failure mode is understood.
  An unresolved "might this produce an unexpected side-effect?" is an
  `open_question`; the confirmed "it did produce side-effect Y" is a `gotcha`.

Adding or changing kinds after sealing requires a derivation version bump and
reviewed migration behavior. The `gotcha` amendment is exempt because it was
applied before any model observations existed.

## Review Lifecycle

`review_state` is operational data and must be queryable schema, not only
`meta_json`.

Allowed states:

- `proposed`
- `accepted`
- `rejected`
- `superseded`
- `soft_deleted`

Rule-derived mechanical observations may start as `accepted` when deterministic
and low-risk. Model-derived observations start as `proposed`.

A derivation version bump creates new proposed observations. It may record
candidate supersession metadata, but it must not automatically supersede
accepted observations. Superseding accepted observations is an explicit
human/operator migration step.

## Review Permissions

Review-aware read surfaces are available to any authorized read token. This
includes `GET /v1/observations` and normal search/timeline/context behavior once
those paths are review-aware.

Review transitions are admin-only in v1. `POST /v1/observations/:id/review`
requires an admin-scoped credential because accepting, rejecting, superseding, or
soft-deleting a derived observation changes Memoire's durable memory state.

## Confidence

`confidence` is a `REAL` value in `[0.0, 1.0]`.

- `1.0`: deterministic or directly structural.
- `0.7` to `0.95`: strong extraction from clear evidence.
- `0.4` to `0.7`: plausible but ambiguous.
- Below `0.4`: normally not emitted except for debug/proposed output.

Confidence is retrieval/ranking metadata. It does not mean truth is accepted;
`review_state` controls trust and visibility.

Confidence must also record its source in provenance metadata:

- `rule_fixed`: deterministic value assigned by a rule.
- `model_self_report`: value returned by a model and not independently
  calibrated.
- `operator`: value assigned or adjusted by a human reviewer.
- `calibrated`: value produced by a later calibration pass.

V1 model confidence is `model_self_report` unless a separate calibration step is
implemented.

## Provenance

Every derived observation must include enough provenance to audit or rebuild it.
Provider/model labels are mandatory when a model is used.

Example provenance metadata:

```json
{
  "source_event_ids": [123, 124],
  "source_event_keys": ["claude-code:abc", "claude-code:def"],
  "source_window": {
    "project": "memoire",
    "session_id": "session-1",
    "start_epoch": 1770000000,
    "end_epoch": 1770000600,
    "event_types": ["claude_PostToolUse", "claude_UserPromptSubmit", "claude_assistant_turn", "codex_assistant_turn", "opencode_assistant_turn", "pi_assistant_turn", "qwen_assistant_turn"]
  },
  "derivation": {
    "version": "observation-derive-v2",
    "strategy": "model",
    "provider": "openai",
    "data_boundary": "external",
    "model": "gpt-5.5",
    "prompt_version": "derive-observations-v2.2",
    "confidence_source": "model_self_report",
    "input_hash": "sha256:..."
  }
}
```

`derivation.version` is the semantic contract version for the derivation
pipeline, including input selection, output schema, hash semantics, and review
behavior. `prompt_version` is the model prompt/template version used inside that
pipeline. A `prompt_version` change may occur under the same derivation version
only when it is behavior-preserving; behavior-changing prompt edits require a
derivation version bump.

Source-id validation correction (2026-07-11, operator ruling): a model-emitted
`source_event_ids` entry may be either the numeric event id or a string that
EXACTLY matches one in-window event's `source_event_id` — two canonical names
for the same event — and the parser maps exact string matches onto the numeric
id before validation. Everything else still rejects (unmatched string =
ungrounded; string shared by several in-window events = ambiguous, never
resolves; other JSON types = malformed), so the strict-parser philosophy
holds. Full mechanics: docs/MODEL-PROVIDER.md "Validation".

## Canonical Hashes

### Input Hash

`input_hash` is SHA-256 over canonical JSON containing ordered source event
tuples:

```json
[
  {
    "event_id": 123,
    "idempotency_key": "...",
    "source_adapter": "claude-code",
    "source_event_id": "...",
    "event_type": "claude_UserPromptSubmit",
    "occurred_at_epoch": 1770000000,
    "payload": {}
  }
]
```

Canonicalization rules:

- Sort tuples by `(occurred_at_epoch, event_id)`.
- Use stable JSON object key ordering.
- Include the canonical raw payload needed by the derivation.
- Exclude DB-side mutable or derived fields: `received_at_epoch`, credentials,
  request IDs, audit IDs, FTS snippets, and current projection text.

If a derivation uses service-projected text as input, that text must be
recomputed from canonical payload inside the derivation version, not read as a
mutable DB-side field. This keeps the hash stable for the same logical raw
input.

### Content Hash

`content_hash` is SHA-256 over canonical JSON:

```json
{
  "project": "memoire",
  "kind": "decision",
  "normalized_text": "Decided to use Quadlet for local shadow-mode lifecycle"
}
```

Canonicalization rules:

- Normalize text with Unicode NFC.
- Trim leading/trailing whitespace.
- Collapse internal ASCII whitespace runs to a single space.
- Preserve case for now; do not stem, translate, or remove punctuation.
- Include `project` and `kind`.

Including `kind` means the same text classified as `decision` and `constraint`
is not the same observation. It is a duplicate-content or reclassification
candidate, not an automatic duplicate. This avoids silently merging a semantic
change into the wrong downstream category.

## Idempotency And Overlap

Derivation needs two idempotency layers:

- Run-level idempotency: the same provider, derivation version, window, and
  `input_hash` must not create duplicate runs.
- Observation-level idempotency: the same `content_hash` with overlapping source
  event IDs must not create another accepted observation.

The same `content_hash` from non-overlapping source events is an independent
recurrence. It should be treated as reinforcing evidence or a supersession
candidate, not as an automatic duplicate and not as an automatically accepted
new observation.

For v1, overlapping-window duplicates should become `proposed` duplicate or
supersession candidates rather than automatic accepted inserts. Automatic merge
is a later policy decision.

*(Amended 2026-08-02, D211 step 3 — operator-ruled.)* Within a SESSION, the
recurrence-as-reinforcement stance above is superseded by the write-time
near-dup gate: a model candidate that is a paraphrase (or exact restatement)
of an existing same-session observation — any review state, raw event mirrors
excluded — is rejected capture-don't-discard under the calibrated two-tier
policy (trigram-Jaccard >= 0.45 with equal digit-token sets, or >= 0.90
unconditionally; calibration record `tmp/neardup-calibration-2026-08-02.md`).
The comparison set is read after the provider returns, and every rejection —
including a match against a candidate inserted earlier in the same
response — stands on the transactional liveness decision: the matched row
must be live inside the same transaction as the would-be insert (review
rounds 1-3). The guarantee ends when that transaction ends: deletion after
the decision does not retro-invalidate a rejection, the same as deletion
after run completion would not. The operator
explicitly accepted losing occasional genuine observations over keeping
paraphrase duplicates. Cross-session recurrence keeps the
reinforcing-evidence semantics — the gate never compares across sessions.

*(Amended 2026-09-11, Fibery #344 — operator-ruled.)* The gate has a THIRD
outcome, **supersede**, which re-classifies pairs it rejects today and never
touches a pair that passes. Streaming capture (memoire:D260) and rhythm cuts
(memoire:D261) mean a session is derived in many cuts while it is still
running, so a cut mid-arc yields "so far" observations and a later cut often
restates one with more information. That restatement now lands as a new
`proposed` row carrying `supersedes_id`, while the row it replaces moves to
`superseded` in the same transaction — instead of being lost as a paraphrase or
landing as a second triage row.

Supersede requires ALL of: a near-dup HIT under the policy above; the matched
row the insert transaction finds LIVE is in review state `proposed` (a machine
proposal may replace a machine proposal — a human-ruled row is superseded only
by a human gesture, memoire:D146 rewrite); that row comes from an EARLIER run —
neither an earlier candidate of this response NOR a row carrying this run's own
id, which a stale-running takeover makes possible since it reuses the id (a row
with no run id at all, a rewrite successor, is neither); containment `|tri(old) ∩ tri(new)| / |tri(old)| >=
0.80` ("it restates"); and growth `|tri(new) \ tri(old)| >= 0.15 * |tri(new)|`
AND at least 40 added normalized runes ("it adds"). The three thresholds are
uncalibrated first values in the memoire:D72 sense; every supersede records the
target and all three scores in run `meta_json.superseded_candidates` so they can
be re-derived from real sweeps.

Consequences, stated because they are not gaps: in the unconditional tier
(trigram >= 0.90) the growth ratio cannot be met, so near-identical
restatements stay rejected; a same-length paraphrase stays rejected; and since
a pure extension has `growth = 1 - trigram`, the lo tier caps growth at 0.55 —
a candidate that roughly more than doubles its target falls out of the GATE and
lands as an ordinary new row, exactly as it does today. The transition is a
dedicated write with its own compare-and-set precondition (`proposed` AND live,
rows-affected authoritative); `proposed -> superseded` stays illegal through
the public review verb, as memoire:D146 ruled for rewrite. The target gets a
review-log row under the system actor `system:derive-supersede` and an
`observation.supersede` audit entry; the successor keeps its OWN source-event
links, because a later cut has events of its own. Superseded rows leave the
per-session derivation read-back, so neither the already-captured prompt
section nor the next gate compares against a row whose content lives on in its
successor. `-no-supersede` (sweep: `MEMOIRE_SWEEP_SUPERSEDE=0`) restores the
previous gate OUTCOME — same reason string, same counters — but not the whole
slice: the superseded-row exclusion above is a read-shape change and applies
whatever the switch says, so a session holding rows retired by an operator
rewrite compares against a slightly smaller reference set than it did before.

## Replay

Replay must be deterministic for the same raw input and derivation version:

- Re-running the same derivation version over the same input does not duplicate
  accepted output.
- A changed derivation version creates a new run and proposed output.
- Rebuild mode may discard derived observations for a derivation version and
  recreate them from raw custody.
- Capture and raw projection never depend on derivation success.

## Additive Schema Shape

Exact migration details belong to the implementation slice, but the contract
expects additive schema only.

Likely additions:

- `derivation_runs` table with provider, data boundary, version, window,
  `input_hash`, status, and timestamps.
- `observations.review_state TEXT`.
- `observations.derivation_run_id TEXT`.
- `observations.content_hash TEXT`.
- Indexes on `review_state`, `derivation_run_id`, and
  `(project, kind, content_hash)`.

Provenance can remain in `meta_json` because it is detailed, evolvable, and
rebuildable. Review state must be queryable schema because operators and read
paths will filter on it.

## Ratification Checklist

Ratified on 2026-05-26:

- The human operator accepted the v1 kind ontology in this document.
- The human operator accepted the first-runnable provider gate:
  `rules` + explicit `openai` may run first, but `local-qwen` lands before
  Memoire scales beyond Claude shadow mode or external producer onboarding.
- The implementation brief states that model-derived observations default to
  `proposed`.
- The implementation brief states that version bumps never auto-supersede
  accepted memory.

## Deferred Implementation Notes

These notes should guide read-path and lifecycle implementation, but they do not
change the sealed v1 derivation contract.

- `open_question` retrieval for session restore should likely include both
  `proposed` and `accepted` observations, unlike most kinds. Model-derived
  questions correctly start as `proposed`, but "what is unresolved?" is useful
  before review.
- `open_question` to `decision` transition should use the existing supersession
  machinery when an unresolved question becomes settled.
- The deterministic `rules` provider design lives in `docs/RULES.md`.
- The model-backed provider design lives in `docs/MODEL-PROVIDER.md`.
