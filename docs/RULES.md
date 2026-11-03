# Derivation Rules

Status: accepted v1 design. The initial rules provider skeleton is implemented.

This document defines the first deterministic `rules` provider for Memoire
observation derivation. It sits under the sealed contract in
`docs/DERIVATION.md`.

## Purpose

The `rules` provider is the local, deterministic baseline. It proves the
derivation pipeline, provenance, review lifecycle, idempotency, and replay
behavior before any model provider writes durable memory.

Rules should only emit observations when evidence is structurally clear. If a
rule needs interpretation, it should not be a rule; it belongs to a model-backed
provider or manual review.

## Provider Metadata

Every rules run writes:

- `provider`: `rules`
- `data_boundary`: `local`
- `derivation_version`: the active derivation contract version
- `prompt_version`: empty
- `confidence_source`: `rule_fixed`

Rules must be replayable from canonical raw events. They must not read FTS
snippets, mutable projection text, or previous derived observations as primary
input unless a future derivation version explicitly allows that.

## Default Output Policy

Rules emit `review_state=accepted` only when output is mechanical and low-risk.
Rules emit `review_state=proposed` when the extraction is useful but still needs
operator judgment.

Rules use fixed confidence values:

- `1.0`: exact structural extraction from explicit operator syntax.
- `0.9`: deterministic extraction from a strongly typed event field.
- `0.75`: deterministic heuristic over clear prompt text.

Rules should not emit below `0.75` in v1. Lower-confidence work belongs to a
model provider. The floor is intentionally conservative: rules below `0.75` are
heuristic enough that they should move to model derivation, while values above
`0.95` are reserved for explicit structural extraction.

## V1 Rule Set

### R001 Explicit Typed Memoire Marker

Input:

- `claude_UserPromptSubmit`
- prompt text contains an explicit typed Memoire capture marker at line start
  outside a fenced code block.

Candidate markers:

- `Memoire decision:`
- `Memoire preference:`
- `Memoire constraint:`
- `Memoire task:`
- `Memoire fact:`
- `Memoire gotcha:`
- `Memoire open_question:`

Output:

- `kind`: derived from the marker.
- `text`: marker body after trimming whitespace.
- `review_state`: `accepted`.
- `confidence`: `1.0`.

Rationale:

The human operator sometimes gives direct durable-memory instructions. A clear typed marker
is structural enough to accept without model judgment and exercises the
retrieval-shaped ontology instead of flattening durable notes into `fact`.

Guardrails:

- Do not capture if marker body is empty.
- Do not infer kind from untyped prose in v1.
- Do not support colloquial `Remember:` in v1.
- Do not capture secrets/redaction-sensitive content differently; purge policy
  remains outside derivation.

### R002 Explicit Task Marker

Input:

- `claude_UserPromptSubmit`
- prompt text contains an explicit follow-up marker at line start outside a
  fenced code block.

Candidate markers:

- `TODO:`
- `Next:`
- `Follow up:`

Output:

- `kind`: `task`.
- `text`: marker body after trimming whitespace.
- `review_state`: `accepted`.
- `confidence`: `1.0`.

Rationale:

Actionable re-entry points are a core retrieval target and can be captured
mechanically when the human operator marks them.

Guardrails:

- Do not convert every imperative sentence into a task.
- Do not capture `TODO:` inside fenced code blocks or pasted code comments.
- Do not emit tasks from assistant text in v1 because assistant turns are not in
  the input boundary yet.

### R003 Explicit Open Question Marker

Input:

- `claude_UserPromptSubmit`
- prompt text contains an explicit unresolved-question marker.

Candidate markers:

- `Open question:`
- `Unresolved:`

Output:

- `kind`: `open_question`.
- `text`: marker body after trimming whitespace.
- `review_state`: `proposed`.
- `confidence`: `1.0`.

Rationale:

`open_question` is retrieval-critical, but even explicit questions should remain
reviewable before joining accepted memory. The read path already includes both
`proposed` and `accepted` open questions by default when `kind=open_question`.

Guardrails:

- Do not emit every question mark as an `open_question`.
- Do not support generic `Question:` in v1; it is often a quick clarification,
  not a durable unresolved thread.
- Do not auto-convert an `open_question` into a `decision`; that transition uses
  review/supersession machinery later.

### R004 Session Re-Entry Prompt

Input:

- `claude_UserPromptSubmit`
- prompt starts with a resume/handoff shape such as `Resume Memoire in ...`.

Output:

- `kind`: `task`.
- `text`: compact re-entry instruction copied from the prompt.
- `review_state`: `proposed`.
- `confidence`: `0.75`.

Rationale:

Resume prompts often contain useful operational next steps, but the boundary is
heuristic. Keep proposed until reviewed.

Guardrails:

- Only emit one task per prompt.
- Prefer exact text over paraphrase.
- Do not emit if an explicit `TODO:` / `Next:` marker already fired for the same
  source event.
- Do not infer tasks merely because the prompt references a concrete
  implementation slice; that is interpretive model-provider scope.

### R005 Liaison Evidence Ingest (added 2026-07-18, memoire:D138)

Input:

- `liaison_evidence` events with `source_adapter=liaison-evidence` whose
  `text` is a JSON object containing `liaison_evidence_observation`.

Output:

- `kind`: `fact`.
- `text`: the event text (the compact D41 evidence envelope) verbatim.
- `review_state`: `accepted`.
- `confidence`: `1.0`.

Contract of record: `docs/LIAISON-EVIDENCE-CONTRACT.md`. Mirrors the retired
`Memoire fact:` echo behavior; a gated-out or malformed envelope yields no
candidate (the Liaison evidence store is authoritative, liaison:D218).

## Non-Rules For V1

Do not implement these as deterministic rules:

- Inferring `decision` from ordinary prose.
- Inferring `preference` from repeated behavior.
- Inferring `constraint` from any sentence containing "must" or "should".
- Inferring tasks from references to implementation slices unless an explicit
  marker or strict resume prefix is present.
- Summarizing sessions.
- Reading assistant output.
- Deriving from rejected/superseded/soft-deleted observations.

These require model judgment, transcript ingest, or a later reviewed rule.

## Registry Shape

Rules should be registered as data, not scattered conditionals.

Suggested Go shape:

```go
type Rule struct {
    ID              string
    Version         string
    Name            string
    InputEventTypes []string
    OutputKind      string
    DefaultState    string
    Confidence      float64
    Apply           func(WindowEvent) []CandidateObservation
}
```

Each emitted candidate should include:

- rule id and version;
- source event ids;
- output kind;
- text;
- confidence;
- confidence source;
- default review state;
- content hash;
- provenance metadata.

Rule IDs are stable. Changing behavior requires either a rule version bump or a
derivation version bump when the change affects replay semantics.

Implementation tests for each rule should cover:

- positive marker extraction;
- negative similar input, especially markers inside fenced code blocks;
- idempotent replay through the existing derivation run and content hash paths.

## Replay And Deduplication

The rules provider uses the same store methods as all derivation providers:

- create or reuse a `derivation_runs` row;
- insert derived observations with source event ids;
- rely on content hash and source-event overlap checks;
- preserve proposed downgrade behavior for overlapping accepted duplicates.

Rules must not bypass review state, provenance, or idempotency because they are
"local."
