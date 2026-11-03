# Model-Backed Derivation Provider

Status: implemented (rules + local model providers live).

> **CORRECTION 2026-07-18 (memoire:D137, liaison:D218 R7):** shipped reality —
> the SessionEnd hook, `scripts/memoire-derive-sweep.sh`, and
> `scripts/derive-timer.sh` run automatic local-model derivation. The
> "no automatic/background derivation" fences below are superseded by the
> binding locality condition: automatic or background derivation is legal
> ONLY for `data_boundary=local` providers; any external provider stays
> explicit per-run opt-in. Statements below forbidding automatic/background
> model derivation read as historical first-slice fences under this
> correction.

This document defines the model-backed derivation provider family for Memoire.
It extends the sealed derivation contract in `docs/DERIVATION.md` and sits
beside the deterministic `rules` provider in `docs/RULES.md`.

Rules are not scaffolding to discard. They remain the operator-intentional,
deterministic, replay-safe capture path. Model-backed providers are a parallel
strategy for synthesis and interpretation that rules intentionally refuse to do.

## Goals

- Derive useful observations when structural rules are too narrow.
- Keep provider sovereignty explicit per run.
- Keep all model-derived observations reviewable before accepted memory.
- Preserve replayability, provenance, and idempotency.
- Make cost and data-boundary choices visible before any model call.

## Non-Goals

- No hidden default external calls.
- No model-invented observation kinds.
- No assistant-turn synthesis until transcript ingest exists.
- No automatic supersession of accepted observations.
- No scheduler in the first implementation slice.

## Provider Selection

Supported provider values come from `docs/DERIVATION.md`:

- `openai`: external provider, `data_boundary=external`.
- `local-qwen`: local provider, `data_boundary=local`.

Provider names identify the provider family, not a deployment host. Local host
identity is recorded separately as `endpoint_id` so a run can say
`provider=local-qwen`, `endpoint_id=hl3`, and `model=qwen-...` without minting a
new provider for each Memoire host. This keeps provider/model idempotency stable
while making per-host audit explicit.

`rules` remains separate and is not a fallback for model failures inside the same
run. If both strategies are desired, operators run both and get separate
`derivation_runs` rows with different provider values.

Provider selection is explicit per run:

- CLI flag: `-provider=openai|local-qwen`.
- Optional config default may be added later, but the first implementation
  should require an explicit provider flag.
- `openai` must refuse to run unless an explicit external-provider opt-in is
  present for that invocation.
- `local-qwen` is first-class even if it is not the first runnable provider.
- `local-qwen` runs require an explicit `endpoint_id` such as `hl3` or `hl5`.

Gate: `local-qwen` must be implemented before Memoire scales beyond Claude
shadow mode or before external producer onboarding, unless the human operator explicitly
resets that gate.

Current first-runnable decision: implement `local-qwen` first against the stable
always-on HL3 endpoint. OpenAI remains a later explicit external path.

Note (scheduled-lane topology, 2026-09-07 — operator ruling; supersedes the
2026-07-22 HL3-daily/HL5-one-off routing this file's endpoint examples were
written against): scheduled derivation runs on ONE lane, the hourly
`memoire-derive-sweep`, against ONE endpoint — `hl7-conductor`, a dedicated
always-on small deriver addressed through the Conductor's stable `deriver`
routing alias. Its registry entry declares a derive profile, so its window
arithmetic is per-endpoint data rather than the shared byte constant (see
`docs/DERIVE-PROFILE-DESIGN.md`). `hl3` remains registered as a MANUAL fallback
an operator names explicitly and is no longer scheduled; `hl5` is gone from the
registry entirely. The `hl3`/`hl5` endpoint ids appearing above and under
"Provenance Requirements" stay as ILLUSTRATIONS of the endpoint-id mechanism —
the rule they illustrate (host identity is config, recorded per run, never a
provider name) is unchanged.

## Input Windows

The first model-backed provider should run over bounded session windows, not
individual event trickles.

Canonical input event types (derivation version `observation-derive-v2`,
corrected 2026-07-11 — turn-ingest campaign Phase 1):

- `claude_UserPromptSubmit`
- `claude_PostToolUse`
- `claude_assistant_turn`
- `codex_assistant_turn`
- `qwen_assistant_turn`
- `opencode_assistant_turn`
- `pi_assistant_turn` (2026-10-07, no version bump: a new type changes no
  historical window)

Assistant turns (live-captured or transcript-backfilled) are canonical input:
their visible prose rides the event `text` column and is rendered into the
prompt body. *(Correction 2026-07-11: the original V1 set admitted only the
first two types, so the "known limitation — no assistant turns" paragraph that
lived here was true of `observation-derive-v1` but became false the moment the
transcript backfill landed; the builder silently filtered every backfilled turn
out. Admitting them changes each window's input composition and canonical
input hash, hence the derivation version bump to `observation-derive-v2` —
windows re-key deliberately and re-derivation is expected. The `rules`
strategy keeps `observation-derive-v1`; its input set did not change.)*

Run modes (wording aligned 2026-09-12 with shipped behavior, Fibery #353):

- `session`: `project + session_id`, with optional `start_epoch` and
  `end_epoch` bounds. A bound narrows the window to a sub-window of that
  session; an absent bound resolves to the session's first or last canonical
  event. The scheduled sweep has derived this shape since the 2026-07-10
  halving amendment, and every rhythm segment (memoire:D0261) is one. A
  session sub-window is not a third mode: its run row carries `session_id`
  and both resolved bounds, its identity is the ordinary run tuple (see
  Model Identity And Idempotency), and coverage is memoire:D0211 unchanged.
- `time-window`: `project + start_epoch + end_epoch` with no session pinned,
  for cross-session runs.

The CLI enforces exactly this vocabulary: `-session-id`, or both
`-start-epoch` and `-end-epoch`; a session run may add one bound or both.

Do not implement automatic PostToolUse-per-event derivation first. It is too
fragmented and encourages tool-call-local conclusions. Model synthesis should
start at session-window granularity. A later incremental mode may sample recent
PostToolUse windows for low-latency suggestions, but it must use a separate
derivation version.

## Prompt Construction

The prompt is built from canonical source events ordered by
`(occurred_at_epoch, id)`.

An event with no captured content — nothing the model can ground an
observation on — does not get a per-event block at all. Every MAXIMAL RUN of
consecutive content-less events renders as ONE `### events A..B` block holding
the run's id span, its epoch span, its event-type counts and its tool-name
counts (at most twelve named tools, the remainder folded into a single
`other xN` counting events). A run of one collapses too: the rule is uniform.
"Content-less" is the min-content guard's own measure, so the renderer and the
guard can never disagree about what content is. Collapsing changes the
RENDERING only — the input manifest still lists every event, so coverage is
unaffected, the canonical input hash is unchanged, and an id inside a
collapsed run is still a valid citation. Run metadata carries
`collapsed_event_count` and `collapsed_block_count`. *(Added 2026-09-11 with
`derive-observations-v2.5`, Fibery #365.)*

Include for each content-bearing source event:

- event id;
- occurred timestamp;
- event type;
- source adapter;
- source event id when present — ALWAYS, since string citations map through
  it;
- project and session id, omitted when they merely repeat the `## Source
  Window` header, which states both; in time-window mode (no session pinned)
  the session id stays on every event. *(Trimmed 2026-09-11 with
  `derive-observations-v2.5`.)*
- selected canonical payload fields.

For `claude_UserPromptSubmit`, include prompt text.

For assistant-turn events (`claude_assistant_turn`, `codex_assistant_turn`,
`qwen_assistant_turn`, `opencode_assistant_turn`, `pi_assistant_turn`),
include the visible assistant prose carried in the event `text` column.
*(Added 2026-07-11 with `observation-derive-v2`; `opencode_assistant_turn`
joined 2026-07-12, `pi_assistant_turn` 2026-10-07.)*

For `claude_PostToolUse`, include compact tool metadata and safe result summary
only when already present in raw payload. Do not add new redaction claims. Do
not include credentials, request ids, audit rows, FTS snippets, or derived
observation text as primary input. Note plainly what that means today: NO
adapter in this repo produces a result summary — the claude-code hook stores
the raw hook payload, whose `tool_response` is not one of the summary keys the
renderer reads — so a tool event carries the tool name and nothing else, and
therefore always renders inside a collapsed block.

Prompt instructions must state:

- Assistant turns are first-class evidence when present (`claude_assistant_turn`
  events, live-captured or transcript-backfilled), and windows without them are
  prompt+tool-only. *(Corrected 2026-07-10 with prompt v2 — the original "V1
  does not include assistant turns" line actively suppressed verdict/outcome
  extraction once the transcript backfill landed.)*
- Outcome extraction first: final results (verdicts, test outcomes, merges,
  approvals) become `fact`/`gotcha` observations carrying the concrete result
  value; outcomes outrank process notes. *(Added 2026-07-10, prompt v2.)*
- Emit only observations grounded in source event ids.
- Pick kind only from the sealed set:
  - `decision`: a settled technical, product, or sequencing decision.
  - `preference`: durable human operator/project preference.
  - `constraint`: rule, safety boundary, non-goal, or policy limit.
  - `task`: actionable follow-up or re-entry point.
  - `fact`: stable project/runtime fact.
  - `open_question`: unresolved question, design fork, uncertainty, or
    decision that still needs human operator review.
  - `gotcha`: an expectation-violating, evidence-backed causal surprise with a
    concrete cost or failure mode — "what bit us, and why." Use `fact` for
    neutral stable truths, `constraint` for prescriptive rules/non-goals,
    `open_question` only when unresolved, and `gotcha` when the surprise is
    resolved with a concrete cost. Consider `gotcha` before `unknown` for
    surprise, cost, or unexpected side-effect shapes.
- Do not invent kinds.
- Prefer the best-fit kind from the seven. Use `unknown` only when no kind
  plausibly applies, and only with full structural analysis. Choosing `unknown`
  requires explicit rejection rationale for at least three sealed kinds plus a
  hypothesis for what kind would fit.
- Prefer no output over weak inference.
- Do not summarize for its own sake.
- Treat any credential-shaped or secret-shaped content as opaque. Do not
  reproduce, infer, or reconstruct it in observations. Secret handling is
  enforced by the raw custody policy upstream, not by the model.
- Do not claim deletion/purge semantics.

*(Added 2026-08-02, prompt v2.4 — D211 step 3; budget made yielding the same
day, review round-1 finding 2.)* When the window belongs to a session with
existing observations, the prompt carries an "Already Captured (this
session)" section: the session's existing observations (all review states,
raw event mirrors excluded), newest first, budgeted at 100 entries / 16 KiB
of rendered section bytes with per-entry clipping, instructing the model not
to re-emit restatements or paraphrases — only genuinely new content, where
an update that changes a result or status is new content and a rewording is
not. The byte budget YIELDS to `MaxPromptBytes` headroom, down to omitting
the section entirely: the context is advisory and must never push a window
that fits on its own past the cap (a fixed allocation turned a formerly
derivable single-event window into a permanent unsplittable sweep loop).
Only the source window itself can trip the prompt-byte fail-close, and that
refusal is typed (`ErrPromptExceedsMaxBytes`) so the dry-run reports the
capless base size for split decisions instead of erroring. The section is
excluded from the canonical input hash, so run idempotency stays a function
of the source events. `-force-rerun` omits it (an A/B rerun regenerates
exactly the output the context would suppress). Truncation is recorded in
prompt and run metadata (`already_captured`), never silent.

Secret handling prompt language is best-effort guidance. The hard gate is the
raw custody policy tracked as `MISSING.md` #17 in the parent Liaison planning
layer. When upstream redaction lands, prompt language should update to preserve
redaction tokens verbatim and never reconstruct secrets.

## Output Format

The model must return JSON only, no prose wrapper. Unknown JSON object fields
at the top level or per-candidate are captured in `extra` by the parser and do
not affect store writes.

Shape:

```json
{
  "schema_version": "1",
  "observations": [
    {
      "kind": "decision",
      "text": "Use rootless Quadlet for local shadow-mode lifecycle.",
      "confidence": 0.86,
      "source_event_ids": [123, 124],
      "rationale": "User accepted the local lifecycle design after review."
    }
  ]
}
```

Validation:

- `schema_version` must match the parser-supported response schema.
- `observations` must be an array.
- `kind` must be one of the seven sealed kinds or `unknown`.
- `text` must be non-empty.
- `confidence` must be in `[0.0, 1.0]`.
- `source_event_ids` must be non-empty and all must belong to the input window.
  *(Corrected 2026-07-11, operator ruling.)* Each entry is either the numeric
  event id or a string that EXACTLY matches one in-window event's
  `source_event_id` — the event's second canonical name — which the parser
  maps to the numeric id. Anything else still rejects: an unmatched string is
  ungrounded, a string shared by several in-window events is ambiguous and
  never resolves, a non-number-non-string entry is malformed. Rationale: the
  prompt displays both id namespaces per event and local models ground on the
  distinctive strings; exact-match recognition is not laundering imprecision.
- Unknown JSON object fields at the top level or per-candidate are captured in
  `extra` by the parser and do not affect store writes.

Behavior-changing prompt or output-shape changes increment `schema_version` and
require a derivation version bump under `docs/DERIVATION.md`.

Invalid observations are rejected individually when possible. A completely
malformed response fails the run without writing observations.

### Unknown Audit Channel

`unknown` is not an observation kind. It is an audit channel for genuine ontology
gaps. The store must reject `kind="unknown"` observations; they do not become
rows in `observations`.

Validated unknowns are preserved in `derivation_runs.meta_json` under
`unmapped_candidates[]` with their text, source event ids, and structural
analysis. Run metadata should also expose `unmapped_ratio` so operators can see
whether the prompt needs tuning or the ontology needs review.

Required shape:

```json
{
  "kind": "unknown",
  "text": "...",
  "source_event_ids": [123],
  "unknown_analysis": {
    "tried_kinds": [
      { "kind": "decision", "why_not": "..." },
      { "kind": "fact", "why_not": "..." },
      { "kind": "open_question", "why_not": "..." }
    ],
    "proposed_kind_hypothesis": "name - short description of the shape that would fit"
  }
}
```

Parser-level validation:

- `tried_kinds` length is at least 3.
- Every `tried_kinds[].kind` is one of the seven sealed kinds.
- Every `why_not` is non-empty.
- `proposed_kind_hypothesis` is required, non-empty, and at least 40
  characters.

Validation failure means the unknown candidate is malformed, not an audit
signal. A malformed unknown candidate is moved to `rejected_candidates[]` with
its captured `extra`, `text`, and `source_event_ids` preserved for audit — it
is never silently discarded.

## Review And Confidence

All model-derived observations default to `review_state=proposed`.

Confidence source is `model_self_report` unless a later calibration pass exists.
Confidence influences sorting/review priority only; it does not make an
observation accepted truth.

The provider should ask the model to calibrate confidence as:

- `0.9-1.0`: directly supported by explicit source evidence.
- `0.7-0.9`: strong inference from clear source evidence.
- `0.4-0.7`: plausible but ambiguous; emit only if useful as proposed.
- `<0.4`: do not emit.

The implementation should drop model observations with confidence `< 0.4`
before store insertion.

## Data Boundary Discipline

`openai`:

- must write `provider=openai`;
- must write `data_boundary=external`;
- must require explicit operator opt-in per run;
- must record model name and prompt version in provenance and in
  `derivation_runs.model`.

`local-qwen`:

- must write `provider=local-qwen`;
- must write `data_boundary=local`;
- must record a required endpoint id such as `hl3` or `hl5`;
- must record local model identifier and prompt version in provenance and in
  `derivation_runs.model`.

The store already rejects provider/data-boundary mismatches. Provider callers
must also fail early with a clear operator error.

## Model Identity And Idempotency

Model identity is queryable schema, not only provenance metadata.

Before model-backed providers write observations, add
`derivation_runs.model TEXT` and `derivation_runs.endpoint_id TEXT` as additive
schema changes. `model` participates in idempotency; `endpoint_id` is audit and
inspection metadata for local execution identity. `endpoint_id` is required for
local providers and nullable for external providers; OpenAI does not get a
sentinel endpoint id.

Run-level idempotency key:

- `provider`
- `model`
- `derivation_version`
- `project`, `session_id` (empty in time-window mode), and the resolved
  `window_start_epoch` / `window_end_epoch` — one tuple for both run modes,
  so a session sub-window never dedups against its whole session
- `input_hash`

`endpoint_id` is not part of the run-level idempotency tuple in v1. The same
provider/model/derivation version on different endpoints is treated as the same
logical derivation. To compare endpoint behavior, bump `derivation_version`.

Running two models over the same window creates independent runs. For example,
`gpt-4o-mini` and `gpt-4o` against the same window are distinct derivations.

Observation-level idempotency is scoped within
`(provider, model, derivation_version)`. Different models produce independent
observation tracks even when `content_hash` collides.

Default reads show accepted observations regardless of which provider/model
produced them. Operator inspection surfaces compare and filter by provider/model
when needed.

Dedup escape hatches (added 2026-07-11, turn-ingest Phase 1):

- **Stale-`running` recovery.** A run abandoned at `running` (its process was
  killed mid-call) past an age ceiling — 2× the execute timeout when the caller
  sets one, else 2h — retries in place with failed-retry semantics: same run
  id, `retry_count` bumped. Both retry paths stamp
  `meta_json.last_attempt_start_epoch`, which is also the staleness clock, so
  an in-flight retry is never stolen. Without this, a killed derive
  watermarked its window forever (running rows dedup unconditionally).
- **`-force-rerun`.** Mints a NEW run for a window that would otherwise dedup
  (prompt/model A/B reruns). The idempotency index is UNIQUE over the tuple
  including `input_hash` and schema is additive-only, so the forced row stores
  the canonical hash with a visible `#forced-<nonce>` suffix in the
  `input_hash` column; `meta_json` records `forced: true`,
  `canonical_input_hash`, and `superseded_run_id` (the most recent prior run
  for the window). Default idempotency is untouched — unforced callers still
  dedup against the original run.

Default-model guidance, not a hard requirement:

- OpenAI should start with a `gpt-4o-mini`-class model.
- Local-Qwen should start with a Qwen-7B/14B-class model.

Small, cheap extraction models are the right default for structured extraction
against a sealed kind set. Heavy reasoning models should be opt-in rather than
the default.

## Failure Modes

Every model run should end in one of these states:

- `completed`: valid response processed.
- `failed`: provider failure or an unrecoverable envelope/array failure (malformed
  top-level JSON, malformed `schema_version`, missing or non-array `observations`,
  trailing data after the top-level object). No observations are written.
- `partial`: salvageable candidate-level semantic failures with capture. Valid
  candidates are written as proposed observations; invalid candidates (wrong kind,
  missing fields, bad confidence, out-of-window ids, malformed unknown audit) are
  moved to `rejected_candidates[]` with their `extra` captured. The run status is
  `partial` whenever at least one candidate is rejected, regardless of how many
  are written — EXCEPT `near_duplicate` rejections *(amended 2026-08-02, D211
  step 3)*: the write-time near-dup gate rejecting a paraphrase of an existing
  session observation (or of an earlier candidate in the same response) does
  not degrade the run. A response whose only rejections are near-duplicates
  completes — including with zero writes — because only a COMPLETED run's
  input manifest covers its events; a `partial` verdict there would re-feed
  the same events to every future sweep and regenerate the same rejected
  paraphrases tick after tick. Race honesty (round-1 finding 1; guarantee
  named precisely in rounds 2-3): the gate compares against the session as
  read AFTER the provider returned, and every rejection — session-row and
  intra-response matches alike, the latter under the earlier candidate's
  inserted row id — stands on the TRANSACTIONAL LIVENESS DECISION: the
  matched row must be live inside the same transaction as the would-be
  insert (on a live match that transaction ends without writing; on none,
  the same transaction performs the insert). The guarantee deliberately
  ends when that transaction ends. A matched row deleted after the
  decision — before or after the run-status write — does not retro-invalidate
  the rejection, exactly as a deletion one microsecond after run completion
  would not: no finite transaction design can extend liveness into the
  future, and custody holds both sides regardless (the rejected text in run
  metadata, the deleted row in soft-deleted custody). The residual — two
  lanes interleaving their write phases can cross-admit within the same
  milliseconds — is the D211 cross-lane residual class, narrowed from
  provider-call minutes to the write window. The
  rejected text, its match target, and the similarity score are retained in
  `rejected_candidates[]` (capture-don't-discard), and
  `candidate_reason_counts.near_duplicate` makes the run explainable from
  metadata alone. *(Amended 2026-09-11, Fibery #344.)* The gate's third
  outcome, **supersede**, is a WRITE, not a rejection: a candidate that
  restates and grows on an earlier cut's still-`proposed` observation is
  written with `supersedes_id` while that row moves to `superseded` in the same
  transaction (conditions and thresholds in `docs/DERIVATION.md`). It counts in
  `written_observation_count`, so the status table above is unchanged — a
  supersede-only response lands on `completed` exactly as a plain-write-only
  one does. Run metadata carries `superseded_count` and
  `superseded_candidates[]` (text, target id, and the three scores, the same
  custody shape as `rejected_candidates[]`), and
  `candidate_reason_counts.supersede` rides beside the rejection classes so one
  map still accounts for every candidate. `-no-supersede` restores the
  rejection.

  *(Fibery #344.)* A run over a window whose TAIL IS STILL OPEN — the sweep
  derives a running session's last segment every tick, mid-arc — is marked
  `provisional: true` in the run metadata and in each written observation's
  `meta_json` via `-provisional`. It is provenance only: no column, no
  retrieval behavior, and never an idempotency input.

Failure cases to handle explicitly:

- timeout;
- provider unavailable;
- invalid credentials or missing local model;
- malformed JSON;
- schema-invalid observations;
- unrecognized observation kind: any value outside the seven sealed kinds and
  `unknown`;
- source event id not in window;
- confidence outside `[0.0, 1.0]`;
- model refusal;
- partial/truncated response;
- budget/rate-limit refusal before request.

Prefer `partial`-with-capture over `failed`-with-nothing wherever the envelope
and observations array are salvageable. Hard `failed` applies only when the
provider fails or the top-level structure is unrecoverable. Per-candidate
failures yield `partial`, not `failed`, and captured `extra` and text are
always preserved in the reject record.

## Model Artifacts

For v1, rationale, refusal metadata, failure metadata, and raw provider
responses for `failed` or `partial` runs live in `derivation_runs.meta_json`.
Completed runs store parsed observations, rationale, and prompt/run metadata,
but do not retain the raw provider response by default. Do not add
model-artifact tables yet. Do not claim a retention policy or
sensitivity-aware encryption for model artifacts in v1.

Deferred follow-ups are captured in
`liaison/inbox/2026-05-27-memoire-model-provider-storage.md`:

- Storage scope: what to keep durably. Rationale is probably useful long-term;
  raw response may only be worth keeping on failure; full prompt probably should
  not be retained by default.
- Storage location: whether `meta_json` scales at higher volume, whether
  sensitive raw responses need separate retention/encryption, and whether
  rationale should become queryable schema.

Revisit after 50-100 model-backed runs, before leaving manual-only model
derivation, or when `MISSING.md` #17 lands.

## Cost Discipline

No background automatic model derivation in the first slice. (CORRECTION
2026-07-18, memoire:D137: automatic local-provider derivation shipped — see
the header correction; the locality condition is the binding fence now.)

First implementation should be operator-triggered only:

- explicit window: session (optionally bounded) or time-window;
- explicit provider;
- explicit model;
- optional dry-run mode before writes;
- hard max input events;
- hard max prompt bytes;
- hard max output observations;
- per-run timeout;
- per-run estimated token/byte report where available.

OpenAI-specific gates:

- require external opt-in;
- require a configured model allowlist;
- require per-run max cost or max token setting before the call.

Local-Qwen-specific gates:

- require local endpoint/model availability check;
- require a configured endpoint/model allowlist;
- require timeout and max prompt/output sizes to avoid runaway local jobs.

Note (v1 liveness-only probe, Finding 5): the v1 local-Qwen availability probe
checks endpoint liveness only (`GET /v1/models`, any 2xx). Model-presence
enforcement — parsing the `/v1/models` response and requiring `req.Model` to be
listed — ships with the persistent `-execute` path in Slice 6. Until then,
the endpoint/model allowlist in `QwenConfig` is the static enforcement gate;
a live endpoint serving the wrong model will fail during generation rather than
at the availability probe.

Secret source, FOR NOW (Slice 6, D73): the `-execute` path resolves the bearer
token by env-var NAME (`api_key_env` in the endpoint registry, e.g.
`HL3_QWEN_API_KEY`) via the injected `EnvLookup` at call time. The token value is
never stored in config and never logged. This is the ratified interim. The HL
API keys will be rotated and encrypted on the HLs (~1 week out). When that lands,
this name-based resolution breaks — and that breakage IS the deliberate trigger
to migrate Memoire's secret source to the bw/vault track (MISSING.md #17). No
vault/bw abstraction is added in Slice 6. The CLI pre-flights the configured
env var so a missing secret fails loud naming ONLY the variable, never a value,
rather than being recorded as a silent `failed` run.

Sampling vs full coverage:

- First model-backed runs should be manual sampled windows.
- Full-coverage derivation over all shadow-mode sessions is deferred until
  review ergonomics and budget controls are proven.

Targeting hygiene (added 2026-07-10, sweep-hygiene brief; scheduled sweeps
must not burn the local brain on populations that can never yield memory):

- Min-content window guard: the orchestrator skips windows with fewer than 3
  non-trivial events (>= 20 chars of extracted text each) or under 500 total
  content chars, BEFORE creating a run record or calling the provider. The
  skip is a marker in CLI output/logs only (`skipped=degenerate_window` plus
  the measured numbers); no `derivation_runs` row is written, so the window is
  re-evaluated when it grows. Operator bypass: `-no-min-content-guard`.
- Project denylist: `derive-denylist.json` next to the endpoint registry
  (override: `MEMOIRE_DERIVE_DENYLIST`), exact names or `path.Match` globs
  (`agent-*`). A denied project skips with `skipped=project_denied` before any
  endpoint/store access. Missing file = empty denylist; malformed file = loud
  operator error. Operator bypass: `-ignore-denylist`.
- Completion cap: the local-Qwen adapter always sends `max_tokens`
  (default 16384, `-max-completion-tokens` / `QwenConfig.MaxCompletionTokens`)
  so degenerate input cannot make a small model generate until the deadline.
  *(Default raised from 4096 on 2026-07-11: a thinking model can spend the
  whole budget on reasoning_content and return empty content — a silent
  yield-killer for unattended runs; 16384 is empirically proven on the
  turn-ingest e2e workload.)*
- Partial-run diagnostics: run metadata carries `candidate_reason_counts`
  (one counter per rejection class, plus `unknown_kind` and
  `empty_observations`) so a `partial, written=0` run is explainable from
  `derivation_runs.meta_json` alone.

## Run Timing

First implementation target: manual session-window runs.

Do not start with automatic `PostToolUse` windows. They are useful later for
low-latency suggestions, but v1 model derivation should synthesize session-level
operator intent and unresolved threads.

Possible later modes:

- `session-end`: triggered by transcript ingest or explicit operator command.
- `recent-window`: bounded last-N-events derivation for active sessions.
- `cross-session`: project/time-window synthesis after assistant-turn ingest and
  review tooling improve.

Each mode that materially changes input semantics needs a derivation version or
prompt version decision before it writes.

## Registry Shape

Model providers should use a registry parallel to the rules registry.

Suggested shape:

```go
type ModelProvider struct {
    Name            string
    DataBoundary    string
    SupportsDryRun  bool
    Generate        func(ctx context.Context, req ModelRequest) (ModelResponse, error)
}
```

`ModelRequest` should carry:

- derivation version;
- prompt version;
- provider name;
- model name;
- endpoint id when applicable;
- source window;
- canonical event inputs;
- budget limits;
- output schema version.

`ModelResponse` should carry:

- raw provider response for failed or partial run metadata;
- parsed candidate observations;
- usage/cost metadata when available;
- refusal/failure metadata when applicable.

## Implementation Order

1. Add operator-facing inspection for derivation runs and proposed observations.
2. Add model-provider dry-run prompt construction with no provider call.
3. Add strict response parser/validator tests using fixtures.
4. Add `local-qwen` provider behind explicit operator flags.
5. Run sampled manual windows only.
6. Review outputs before considering scheduler or broader coverage.
