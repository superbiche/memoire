# Mémoire — first implementing-agent handoff

**Design baseline:** 17 September 2026 · **Reference edition:** 1.1  
**Mission:** implement a measured, replayable capture-normalization-to-observation improvement using existing Mémoire contracts.  
**Supersedes:** edition 1.0's meeting-first mission. The operator explicitly requested upstream cleanup, better observations and lifecycle improvements before the interview.

This is an implementation kickoff, not a request for another general design proposal. The overall program proceeds upstream-first; this agent owns a bounded foundational increment, not every stage of the redesign.

## 1. Outcome expected

Provide a locally runnable path that selects an identified evidence scope, preserves authorized source material, constructs a less noisy model-facing representation, derives selective source-linked observations, and compares the candidate with the current pipeline on the same inputs. Make processing dispositions, omissions, failures, versions and costs inspectable.

The initial human-facing artifact is a diagnostic comparison, not an alignment meeting. Reduce narration and redundant inference without losing original problems, decisions, constraints, relevant outcomes, uncertainty or meaningful failed attempts. Fewer observations alone do not establish improvement.

Keep existing production injection, accepted understanding and capture custody unchanged unless a separately inspected source contract and authorization allow an explicit opt-in change. Candidate replay must not pollute production observation stores. Deliver runnable code and tests, not only an audit or static example.

## 2. Read in this order

Read the actual checkout's governing agent instructions and applicable decision/contracts first. Then read the [reference index](README.md), [intent](design/01-intent-and-invariants.md), [evidence and lifecycles](design/02-evidence-views-and-lifecycles.md), [derivation](design/04-derivation-and-consolidation.md), [measurement](design/07-measurement-and-learning.md) and [rollout](design/09-experiments-and-rollout.md).

Consult [identity](design/03-work-identity-and-time.md), [operations](design/08-locality-security-and-operations.md), relevant [acceptance cases](design/10-acceptance-scenarios.md) and the [retrieval contract](design/05-retrieval-and-progressive-disclosure.md) for touched paths. The meeting chapter defines a downstream consumer, not first-agent scope. Borrow mechanisms selectively; do not install another memory engine to satisfy this assignment.

Do not load the complete reference into every model invocation. Use repository-native commands and conventions rather than inventing an integration framework.

## 3. Boundaries to preserve

Capture remains independent of inference and human review. Capture cleanup is not blanket deletion: preserve authorized evidence after existing redaction/retention controls; improve ingestion correctness where a specific defect is demonstrated and make extractor-input filtering replayable. Do not retain secrets merely to promise raw fidelity.

Proposed current-work observations remain first-class for their scoped temporal account. They do not become accepted project knowledge. Do not globally approve rows or bypass existing publication/authorization checks.

Preserve source provenance, work/session/attempt attribution where available, command/result relationships, relevant revision/environment and event versus processing time. A label is not an identity; an identical result from another execution is not a duplicate delivery of the same event. Do not force a full execution ledger before improving the input path.

Keep plans, reported outcomes and observed verification distinct. Source handles must resolve, but a valid handle alone does not establish that the linked claim is supported. Preserve uncertainty and the original problem behind a solution.

Measurements start now. Optional notes may attach to replay runs or episodes; no operator per-observation scoring is required. Meeting-specific feedback ships later with meetings. Keep deterministic figures, estimates and semantic judgments distinct.

Use the declared local boundary and existing model/storage adapters. No remote fallback, production purge, full historical re-derivation, destructive migration or production cutover is authorized by this handoff. Preserve the actual API/store writer boundary.

## 4. Bounded reconnaissance and baseline

Identify the checkout and revision, private-source versus curated-export status, governing instructions, existing capture adapters, redaction/custody behavior, event schema, model-input assembly, derivation prompts, result validation, lifecycle/indexing paths and test entry points. Inspect existing IDs, replay hooks, timing/token collection and storage lanes.

Write a small capability-and-contract map: existing, partial, missing or unknown, with relevant entry points and proposed changes. Do not infer absence from the design terminology. The public README identifies the repository as an export; do not invent private decisions. A real conflict stops only the affected operation while independent safe fixture/instrumentation work can continue. [R1](sources/README.md#r1)

Select a bounded representative corpus, preferably permitted redacted real sessions when available, plus authored regression fixtures for missing edge cases. Include narration/no-op work, a read-only discovery, useful failure evidence, a real decision, unresolved work, misleading completion claims, repeated capture and two parallel tasks. Preserve provenance of real versus synthetic inputs; do not describe authored fixtures as live validation or reproduction of the addendum's scores.

Run or reconstruct the current baseline through the existing path. Capture selected evidence, serving/model/prompt/settings, input/output, errors, gaps, costs and timing availability. Unknown metrics are not zero. Do not scan the entire private archive or perform production writes by default.

## 5. Implement the bounded foundation

### 5.1 Replay-safe evidence selection and identity

Reuse available project/work/session/range selectors. Retain stable evidence references and a coverage checkpoint that can describe gaps; a newest timestamp is not sufficient. Reuse namespaced external IDs. Where necessary, use a mechanically generated session-anchored work ID and changeable descriptive label without asserting uncertain cross-session linkage.

Maintain call/result and attempt/review targeting where present. Test two parallel tasks. Adding ledger/coordinator adapter seams is sufficient; building the ledger or an automatic task merger is out of scope.

### 5.2 Versioned normalization and input dispositions

Build a model-input projection from captured evidence rather than overwriting sources. Candidate rules include duplicate transport suppression, exclusion of repeated injected memory/instructions as fresh evidence, reduced progress narration and bounded tool-output excerpts with precise expansion references.

Record which ranges were included, excerpted, deduplicated, suppressed from the model input or unavailable, together with transformation version and applicable reasons. Preserve omitted source references within retention/access policy so sampled material can be checked. Do not treat independently repeated failures as duplicates simply because the text matches.

Retain useful error details, decision/constraint statements, relevant scope and results necessary to interpret the action. A read can reveal important information; a failed attempt can explain an enduring constraint. No blanket removal of read-only tools or failed executions. A payload limit must expose clipping rather than silently claim completeness.

### 5.3 Batching with bounded freshness

Group completed interactions or meaningful tool sequences within the existing architecture. Avoid splitting a command from its result or mixing unrelated work. Preserve incomplete sequences explicitly instead of manufacturing completion.

Use reversible, recorded flush/size defaults. Report batching delay and oldest unprocessed evidence. Large offline throughput gains are insufficient if active task state becomes materially less fresh. Processed ranges must prevent accidental repeated inference, while late results and explicit corrections remain incorporable.

### 5.4 Selective observation derivation

Retain the existing Qwen 3.5 9B deployment as the controlled model baseline, subject to actual checkout/configuration inspection. Narrow the extraction contract to supported meaningful developments with source links, applicable scope and uncertainty. Permit successful empty output for narration-only input.

Distinguish proposed, attempted, reported-complete and observed-verified work. Do not derive a standing requirement from one patch or a durable preference from an incidental choice. Tag previously derived/injected memory so it cannot corroborate itself.

Use existing structured-output support and validation. Validate references and mechanically checkable facts. Separate successful empty extraction from unavailable input, model failure/refusal, invalid structure and truncation; retry or split using bounded policy and disclose unprocessed ranges. Do not accept partial valid-looking output as complete without an explicit completeness signal.

Record actual thinking/template settings; changing them is a separate measured experiment. A model migration is not required. Reduce input/calls/narration first so model-size effects are not confounded with pipeline effects.

### 5.5 Minimum lifecycle seam, not the whole lifecycle engine

Retain source dependencies, stable output/run identifiers, work scope and processing dispositions needed for later reconciliation. Follow existing idempotency and writer contracts; distinguish replayed candidate outputs from live outputs and from human acceptance.

If existing lifecycle behavior already resolves, qualifies or supersedes relevant observations, reuse and test it. If absent, identify the smallest contract needed for the next stage rather than implementing speculative global merge/retirement rules. Never collapse conflicting branches or reviews by text similarity.

The first agent need not deliver autonomous consolidation, comprehensive state reducers or publication orchestration. Observation lifecycle and supported understanding changes are the next upstream increments before meetings.

### 5.6 Measured comparison and inspectability

Provide a repository-native command or equivalent local interface to run the selected corpus through baseline and candidate, inspect sources and transformations, compare outputs, and export an evaluation record. Ordinary structured records plus a readable report are sufficient; no chat UI or dashboard is needed.

Measure input/output size, reported tokens where available, calls, queue/processing time, retries, invalid/truncated/empty outputs, processed gaps and resource conditions relevant to the comparison. Include pre/post-processing and work shifted to later stages; avoid claiming net savings from one stage alone. Clearly label estimated token counts and shared-hardware attribution limits.

Evaluate important-claim retention, narration, duplication, supported scope, original-problem preservation and consequential omissions. Report semantic assessor/rubric and sample counts. Use at least one held-out case and one previously successful case. Sample suppressed input; do not require operator inspection of every observation.

Keep a few fixed downstream probes such as “what happened?”, “what is still open?” and “which evidence verifies completion at this revision?”. These test the usefulness of intermediate outputs without building a meeting or treating a polished summary as proof of extraction quality.

### 5.7 Reversibility

Keep changes opt-in or isolated using source-repository conventions. Disabling candidate processing must leave capture, existing reads and accepted understanding usable. Preserve evaluation provenance without leaking sensitive text into public logs. Use supported migration procedures only where necessary and authorized.

## 6. Required checks

The [acceptance catalogue](design/10-acceptance-scenarios.md) describes the whole product. Apply only the relevant first-slice cases plus these concrete foundation obligations:

| Check | Evidence required |
|---|---|
| Custody and dispositions | Original permitted evidence remains retrievable; projection mappings and redaction/clipping/availability distinctions are inspectable. |
| Deduplication versus recurrence | Re-delivery of one event does not create duplicate work; separately occurring identical failures remain distinct evidence. |
| Semantic preservation | A meaningful read discovery, useful failure, original problem and constraint survive; narration-only input may yield an empty collection. |
| Scope and evidence | Parallel tasks and revisions remain distinct; plans/reports are not upgraded into observed verification; invalid handles are rejected. |
| Processing honesty | Missing results, failures, truncation, valid abstention and coverage gaps remain distinguishable; later success does not erase earlier gaps. |
| Comparison | Same-corpus baseline/candidate records include cost, freshness, semantic limits and sampled omissions; held-out and positive cases are reported. |
| Boundary protection | Replay creates no duplicate production observations; current-work usefulness does not imply human acceptance; no cloud fallback or destructive cutover. |
| Disablement | The candidate can be disabled without breaking capture/legacy behavior or erasing source/evaluation provenance. |

Relevant catalogue entries include AC-02, AC-04–AC-10, AC-11–AC-19, AC-29, AC-43 and AC-47–AC-50 where the corresponding path is touched. Do not claim all are fully implemented if only their foundational part was exercised.

Run repository-required tests and targeted tests for modified components. Exercise supported storage combinations actually touched and state which were run. Stub tests establish plumbing, not real-model extraction quality. If no permitted real corpus or local endpoint is available, still provide runnable fixtures and the local evaluation path; state the missing evidence and leave quality promotion pending. Never substitute a remote model silently.

## 7. Scope discipline and completion

Do not build the interview, meeting packet orchestration, meeting feedback UI, new graph database, replacement engine, full fleet ledger, automatic semantic task merger, global model bake-off or autonomous self-modifying optimizer in this assignment. Do not bulk-clean old production observations or backfill all history.

If scope grows, narrow the supported input adapter, corpus or normalization rules while preserving the evidence-to-observation comparison and safeguards. Do not retreat to only telemetry, or change the mission back to a meeting because that is easier to demonstrate.

Deliver a compact completion report with implemented behavior, source-contract map, modified components, local run/inspect commands, baseline/candidate examples and measurements, tests actually run, failed cases and unmeasured aspects, promotion decision and disablement. Identify the next lifecycle defect or experiment justified by evidence, not a generic request to implement all remaining chapters.

**Implementation complete:** a bounded, safe, locally runnable candidate normalization/batching/observation path plus reproducible inspection, tests and comparison records. **Ready to promote:** the candidate meets declared integrity and scoped quality/cost/freshness gates without hiding consequential omissions. These are separate claims; inconclusive results do not authorize promotion.

Ask the operator only for a consequential unresolved source-contract conflict, authorization boundary or unavailable necessary input. Use reversible documented defaults for tunable thresholds. Do not ask the operator to restate the approved design or evaluate every derived observation.

## Source and revision note

[C1](sources/README.md#c1) is the design conversation including the operator's upstream-first correction. [A1](sources/README.md#a1) is the supplied addendum; its original experiment limitations remain in force. [R1](sources/README.md#r1) records the previously checked public README, not a new code audit. The actual source checkout governs implementation commands and contracts.
