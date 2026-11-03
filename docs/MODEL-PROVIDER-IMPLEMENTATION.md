# Model Provider Implementation Brief

Status: implemented.

> **CORRECTION 2026-07-18 (memoire:D137, liaison:D218 R7):** automatic
> local-model derivation shipped (SessionEnd hook + derive-sweep + timer).
> Binding condition: automatic/background derivation only for
> `data_boundary=local` providers; external providers stay explicit per-run
> opt-in. The hard constraint below is amended accordingly.

This brief turns `docs/MODEL-PROVIDER.md` into an implementation sequence. It is
not a replacement contract. If this brief conflicts with `docs/DERIVATION.md`,
`docs/RULES.md`, or `docs/MODEL-PROVIDER.md`, the sealed contract docs win and
this brief must be corrected.

## Objective

Add model-backed observation derivation as an operator-triggered, review-first
path parallel to the existing deterministic `rules` provider.

The first runnable model-backed path should be useful for sampled manual windows
without creating hidden external calls, scheduler behavior, or accepted memory
without review.

## Hard Constraints

- No automatic or background EXTERNAL model calls (memoire:D137: automatic
  derivation is legal only for `data_boundary=local` providers).
- Provider and model are explicit per run.
- `openai` requires explicit external opt-in on each invocation.
- `openai` writes `data_boundary=external`; `local-qwen` writes
  `data_boundary=local`.
- Local provider host identity is `endpoint_id`, not a provider-name suffix.
  Use `provider=local-qwen`, `endpoint_id=hl3`, and a concrete model name.
- Model-derived observations default to `review_state=proposed`.
- Accepted reads remain provider/model-agnostic by default.
- Operator inspection must expose provider/model/run metadata well enough to
  compare outputs.
- Unknown candidates are audit data only; `kind="unknown"` never becomes an
  observation row.
- V1 still derives without assistant turns. Provenance must make that visible.
- Raw custody / secret deletion remains blocked on the upstream policy tracked
  as `MISSING.md` #17; model prompting is not a secret-control mechanism.

## Proposed Slice Plan

### Slice 1: Schema And Store Plumbing

Purpose: make model identity queryable before any model-backed writes exist.

Changes:

- Add `schema/0006_derivation_model.sql`.
- Add nullable `derivation_runs.model TEXT`.
- Add nullable `derivation_runs.endpoint_id TEXT`.
- Add an index that supports operator inspection by
  project/provider/model/endpoint/time.
- Extend `db.DerivationRunInput`, `db.DerivationRunRecord`, and
  `db.DerivationRunFilter` with `Model` and `EndpointID`.
- Update derivation run idempotency to include `model`:
  `(provider, model, derivation_version, project, session/window, input_hash)`.
- Do not include `endpoint_id` in the v1 idempotency tuple. Endpoint identity is
  audit and inspection metadata, not provider-family identity.
- `endpoint_id` is required for local providers and nullable for external
  providers. Do not write a sentinel such as `openai-default`.
- Same provider/model/derivation version on different endpoints is treated as
  the same derivation. To compare endpoint behavior, bump `derivation_version`.
- Keep `rules` runs valid with empty model.
- Extend `GET /v1/derivation-runs` with `model=` and `endpoint_id=` filtering.
- Preserve compatibility with existing runs where `model` is null or empty.

Index migration shape:

- `schema/0004_derivation.sql` already created unique index
  `idx_derivation_runs_idempotency` without `model`.
- If that unique index remains unchanged, SQLite will still reject two
  same-provider runs over the same logical window/input even when the model
  column differs.
- Replace `idx_derivation_runs_idempotency` with the wider unique tuple:
  `(provider, COALESCE(model, ''), derivation_version, project,
  COALESCE(session_id, ''), window_start_epoch, window_end_epoch, input_hash)`.
- This widens a uniqueness predicate; it does not delete observation rows or
  derivation-run rows. It preserves the additive-only spirit while aligning the
  database constraint with the model identity contract.
- Migration must include a sanity check in the implementation notes/tests:
  `SELECT COUNT(*) FROM derivation_runs` before and after the index replacement
  must match.

Tests:

- Migration applies once across reopen.
- Existing `rules` idempotency still works with empty model.
- Same provider/window/input but different model creates distinct runs.
- `GET /v1/derivation-runs?model=...` filters correctly.
- `GET /v1/derivation-runs?endpoint_id=hl3` filters correctly.

Non-goals:

- No prompt construction.
- No model parser.
- No provider registry.
- No external calls.

### Slice 2: Prompt Builder And Strict Parser

Purpose: implement deterministic model-run preparation and response validation
without calling any provider.

New package:

- `services/api/internal/modelderive`

Suggested types:

```go
type Window struct {
    Project    string
    SessionID  string
    StartEpoch int64
    EndEpoch   int64
}

type Request struct {
    Provider          string
    Model             string
    DerivationVersion string
    PromptVersion     string
    OutputSchema      string
    Window            Window
    Events            []db.EventRecord
    Limits            Limits
}

type Limits struct {
    MaxInputEvents       int
    MaxPromptBytes       int
    MaxOutputObservations int
}

type ParsedResponse struct {
    Observations       []CandidateObservation
    UnknownCandidates  []UnknownCandidate
    RejectedCandidates []RejectedCandidate
}
```

Prompt builder requirements:

- Source events ordered by `(occurred_at_epoch, id)`.
- Include only canonical fields defined in `docs/MODEL-PROVIDER.md`.
- Include the sealed kind definitions verbatim.
- Include the v1 limitation that assistant turns are absent.
- Include the secret-handling guidance exactly as contract language, without
  claiming hard redaction.
- Enforce `MaxInputEvents` and `MaxPromptBytes` before any provider call.
- Return a prompt plus deterministic prompt metadata for `derivation_runs.meta_json`.

Parser requirements:

- Accept JSON only, no prose wrapper.
- Require top-level `"schema_version": "1"`.
- Parse the envelope as a map; capture unknown top-level fields in
  `ParsedResponse.Extra` (key `response_extra` in run `meta_json`).
- Validate the seven sealed kinds plus the reserved `unknown` audit channel.
- Reject `unknown` as an observation write target; only `kind="unknown"` with a
  valid `unknown_analysis` block goes to `UnknownCandidates`.
- Decode each candidate from raw JSON; capture unknown candidate fields in
  `CandidateObservation.Extra` / `UnknownCandidate.Extra` / `RejectedCandidate.Extra`
  (key `model_response_extra` per observation in `meta_json`).
- Route each candidate to exactly one of: `Observations` (write-eligible),
  `UnknownCandidates` (audit-only), or `RejectedCandidates` (capture-and-reject).
- Semantic guardrail for `Observations`: valid sealed kind (incl. `gotcha`),
  non-empty `text`, present numeric `confidence >= 0.4`, non-empty in-window
  `source_event_ids`.
- Candidates that fail the semantic guardrail become `RejectedCandidates` with
  their `extra`, `text`, and `source_event_ids` captured; they are never silently
  discarded.
- Validate unknown structural friction:
  - at least three `tried_kinds`;
  - each tried kind is one of the seven sealed kinds;
  - every `why_not` is non-empty;
  - `proposed_kind_hypothesis` is at least 40 characters.
- A malformed unknown candidate becomes a `RejectedCandidate`, not an audit signal.
- Validate confidence in `[0.0, 1.0]`; drop observations with confidence `< 0.4`.
- Validate every source event id belongs to the input window.
- Trailing-data check: reject any decodable value after the top-level object
  using a second `Decode` (must return `io.EOF`), not `dec.More()`.
- Return per-candidate validation errors without writing anything.
- `parseErr` (envelope-level) means only truly unrecoverable failures: malformed
  top-level JSON, missing/mismatched `schema_version`, missing/non-array
  `observations`, or trailing data. Candidate-level failures keep the parse alive.

Orchestrator requirement (MaxOutputObservations, Finding 1):

- After parsing, if `req.Limits.MaxOutputObservations > 0` and
  `len(parsed.Observations)` exceeds the cap, move overflow observations to
  `RejectedCandidates` (reason: `exceeds MaxOutputObservations N`) before the
  write loop. Overflow records must preserve `Extra`, `Text`, and `SourceEventIDs`.
  `decideStatus` returns `partial` automatically because `RejectedCandidates` is
  non-empty. A value of 0/unset means no implicit cap.

Tests:

- Prompt contains sealed kind definitions (all seven) and v1 assistant-turn
  limitation.
- Prompt contains `gotcha` disambiguation guidance (prefer `gotcha` over `unknown`
  for surprise/cost shapes).
- Prompt builder excludes derived observation text and audit rows.
- Max input events and max prompt bytes fail closed.
- Parser accepts one valid observation for each of the seven sealed kinds
  (including `gotcha`).
- Parser rejects malformed top-level JSON.
- Parser fails schema-version mismatch.
- Parser rejects source event ids outside the window.
- Parser drops confidence `< 0.4`.
- Parser captures top-level unknown fields in `ParsedResponse.Extra`.
- Parser captures per-candidate unknown fields in `CandidateObservation.Extra`
  on stored observations.
- Parser captures per-candidate unknown fields in `UnknownCandidate.Extra`.
- A semantically-invalid candidate with extra fields becomes a `RejectedCandidate`
  with its `extra` captured, not silently discarded.
- Parser preserves valid unknown candidates as audit data.
- A malformed unknown candidate becomes a `RejectedCandidate`, not an audit signal.
- Parser treats unrecognized kind outside the seven sealed kinds plus `unknown`
  as a `RejectedCandidate`.
- Robust trailing-data rejection (second `Decode` must yield `io.EOF`).
- `gotcha` candidate becomes a stored observation, not an unknown candidate.
- A valid unknown candidate may list `gotcha` in `tried_kinds`.
- `TestOrchestratorEnforcesMaxOutputObservations`: cap=N, N+1 valid candidates →
  N written + 1 cap-rejected + `partial` status + only N rows persisted.
- `TestOrchestratorCapNoopWhenUnset`: 0 cap → all valid candidates written.
- Orchestrator tests verify extras survive into run and observation `meta_json`
  and that salvageable extra-bearing responses produce `partial`, not `failed`.

Non-goals:

- No `derivation_runs` writes.
- No provider registry.
- No HTTP endpoint changes.
- No external or local model call.

### Slice 3: Dry-Run CLI

Purpose: let operators inspect model prompts and parser behavior before Memoire
can call a model.

Preferred CLI shape:

- Extend `memoire-derive` with `-strategy=rules|model`.
- `-strategy=model` initially requires `-dry-run`.
- Required flags: `-project`, either `-session-id` or time bounds,
  `-provider`, `-model`.
- Optional limit flags: `-max-input-events`, `-max-prompt-bytes`,
  `-max-output-observations`.
- `-json` emits prompt metadata and a prompt preview, not the full prompt by
  default.
- Add `-prompt-out path` to write the full prompt to an operator-chosen local
  file.
- Add `-parse-response path` so parser fixtures can be run against a saved
  provider response without a provider adapter.

Dry-run behavior:

- Fetches the same source event window the model provider will use.
- Builds the prompt and computes the same canonical input hash.
- Does not insert a `derivation_runs` row by default.
- Dry-run stays non-persistent in v1. If `-record-run` is added later, it must
  write status `dry_run` only after a schema/state decision. Do not add that in
  this slice.

Tests:

- CLI fails without `-dry-run` for `-strategy=model`.
- CLI fails without explicit provider/model.
- CLI emits prompt metadata for a seeded session window.
- CLI parser path reports valid/proposed candidates from a fixture.

Non-goals:

- No provider calls.
- No observation writes.
- No dry-run persistence.

### Slice 4: Provider Registry And Run Orchestration Skeleton

Purpose: add the provider abstraction and the store/write orchestration while
using fake providers in tests.

New interfaces:

```go
type Provider interface {
    Name() string
    DataBoundary() string
    Generate(ctx context.Context, req Request) (ProviderResponse, error)
}

type ProviderResponse struct {
    RawResponse json.RawMessage
    Usage       Usage
    Refusal     string
}
```

Orchestration flow:

1. Validate provider/model/window/limits.
2. Enforce provider/data-boundary discipline before prompt construction.
3. Fetch source events.
4. Build prompt and input hash.
5. Create or detect a derivation run including provider/model/version/window/hash.
6. If duplicate, return duplicate result without calling the provider.
7. Call provider with timeout and limits.
8. Parse response.
9. Insert valid candidates as proposed derived observations.
10. Write run metadata containing usage, rejected candidates, unknown candidates,
    refusal/failure metadata according to the v1 same-table artifact policy.
11. Mark run `completed`, `partial`, or `failed`.

Artifact retention:

- Store raw provider responses for `failed` and `partial` runs only in v1.
- For `completed` runs, keep parsed observations, rationale, usage, and
  prompt/run metadata. Do not retain the raw provider response by default.

Important schema note:

- Current `EnsureDerivationRun` inserts status and completed timestamp once. This
  slice likely needs a store method to update run status and metadata after the
  provider returns. Additive method only; no schema rewrite.

Tests:

- Duplicate run short-circuits before provider call.
- Fake provider valid response writes proposed observations.
- Fake provider mixed valid/invalid response writes valid observations and marks
  run `partial`.
- Provider timeout marks run `failed` with no observations.
- Provider refusal marks run `failed` or `partial` depending on whether valid
  observations exist.
- Unknown candidates are persisted in run metadata only.
- Run metadata includes provider, model, prompt version, schema version, usage,
  and unmapped ratio.

Non-goals:

- No real OpenAI adapter.
- No real local-Qwen adapter.
- No scheduler.

### Slice 5: First Real Provider Adapter

Decision: first runnable provider is `local-qwen` against the stable always-on
HL3 endpoint. OpenAI remains a later explicit external-provider adapter.

Provider naming:

- Use `provider=local-qwen`, not host-specific provider ids such as
  `local_qwen_hl3`.
- Require `endpoint_id=hl3` for the first local-Qwen adapter.
- Surface `endpoint_id` in derivation-run inspection.
- Do not hide provider/model/endpoint behind config defaults. The CLI must
  still require `-provider`, `-model`, and local provider `-endpoint-id`.

OpenAI adapter requirements:

- Requires explicit external opt-in flag, for example
  `-allow-external-provider`.
- Requires API key from environment or configured secret source. Never log it.
- Requires model allowlist.
- Requires max prompt bytes and max output observations.
- Records `data_boundary=external`.
- Records usage/cost metadata when available.

Local-Qwen adapter requirements:

- Requires explicit endpoint/model.
- Requires endpoint/model allowlist.
- Performs availability check before the run.
- Requires timeout and prompt/output limits.
- Records `data_boundary=local`.
- Records local endpoint/model metadata without leaking unnecessary host details
  into observations.

Tests:

- Adapter refuses missing required configuration.
- Adapter refuses provider/data-boundary mismatch.
- Adapter maps provider errors/refusals into orchestrator failure states.
- Adapter tests use HTTP fakes, not real network provider calls.

Non-goals:

- No background scheduling.
- No full shadow-mode backfill.
- No automatic acceptance or supersession.

## Resolved Decisions

- Slice 1 and Slice 2 stay separate.
- Dry-run is non-persistent in v1.
- `memoire-derive` absorbs model strategy via `-strategy=rules|model`.
- `MaxPromptBytes` is the universal v1 cost gate. OpenAI can add token
  estimation later if needed.
- Raw provider responses are stored for `failed` and `partial` runs only.
- First runnable provider is `local-qwen` on endpoint `hl3`.
- Model-aware idempotency uses a contract-aligning wider unique index tuple,
  not a new table and not a provider-specific workaround.
- `endpoint_id` is nullable for external providers and required for local
  providers.
- Endpoint allowlist enforcement belongs to the adapter slice, not Slice 1.
- Endpoint is excluded from idempotency; endpoint A/B comparisons require a
  derivation version bump.

## Ratification Checklist

Before implementation delegation:

- Opus challenge resolved.
- Slice boundaries accepted.
- Endpoint identity shape accepted.
- `derivation_runs.model` and `derivation_runs.endpoint_id` migration shape
  accepted.
- Worker brief names exact files, tests, and non-goals for the next slice only.
