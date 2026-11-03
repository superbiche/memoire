# 04 — Selective derivation and incremental consolidation

**Status:** target behavior with explicit optimization experiments.  
**Owns:** DR-07 and reduction of inference/noise upstream of meetings.  
**Basis:** [C1](../sources/README.md#c1), [A1, lines 25–39, 55–65](../sources/README.md#a1); borrowing references [E1–E8, E11–E13]. See [source register](../sources/README.md).

## 1. Optimize the pipeline before assuming the model is the problem

The reported baseline uses Qwen 3.5 9B for observation derivation. No deployment benchmark or inference trace was available for this reference. “Too slow” could reflect queue delay, repeated large inputs, output narration, reasoning generation, retries, concurrency contention or decoding speed.

First measure the current path. Compare a redesigned pipeline with the same model before comparing model sizes. Adding a stronger consolidation model while retaining every existing extraction call can increase total work instead of reducing it.

The desired output is not the smallest observation count. It is fewer unnecessary calls and less low-value generated text while preserving consequential outcomes, constraints and current state.

## 2. Normalize and group before inference

Deterministic processing should remove duplication from the extractor input without deleting the underlying authorized evidence. Useful candidates include repeated progress announcements, repeated injected instruction blocks, identical tool output and large file dumps whose relevant result can be represented by an excerpt and artifact reference.

Preserve tool invocation/result association, result status, source boundaries and useful error details. Extract mechanically available metadata without another model call. Mark injected memory as derived material so that recapture does not create fresh corroboration.

Do not equate read-only work with irrelevance: reading can discover a constraint or a root cause. Do not discard failed attempts universally: an environment-specific failure and its conditions can be valuable reusable knowledge. The activity itself may be noise while the discovery matters.

Normalization must not erase negative evidence, branch distinctions or repeated failures whose recurrence is itself significant. Exact deduplication of repeated capture is different from collapsing separately occurring events.

## 3. Semantic batches, not one call per event

Group a meaningful episode or completed interaction, preserving enough context to interpret results. Candidate triggers include completed tool sequences, session idleness and a bounded normalized-input threshold. Avoid arbitrary cuts between the command and the result needed to understand it.

A lightweight live path may extract high-value state transitions before a longer episode is ready. This does not require rerunning the full episode repeatedly: processed ranges and shared evidence should be explicit.

Initial token budgets are experiments, not defaults guaranteed by this design. A few thousand normalized input tokens and a 512–1,024-token output cap are possible pilot settings from the discussion. Validate them against actual schemas and examples; a cap that routinely truncates useful claims is not an optimization.

Failed, refused, truncated and successfully empty outputs are different states. A valid empty extraction means “no eligible claim identified in this processed input.” It does not mean no activity occurred or all possible significance has been ruled out.

## 4. Narrow extraction contract

A starting prompt should ask for explicitly supported candidate claims: decisions, constraints, preferences where actually expressed, meaningful outcomes, open issues, state changes and potentially reusable lessons. It should permit an empty collection and include negative examples.

It should preserve proposed versus attempted versus completed versus verified. It must not transform an assistant's intention into an accomplished action, or infer a durable preference from one incidental choice. Each retained claim needs a source reference and applicable scope.

Illustrative distinctions:

| Source evidence | Retainable assertion | Unsupported upgrade |
|---|---|---|
| Agent says it will run tests. | Tests are planned or intended. | Tests passed. |
| Agent says tests passed; no captured result. | Agent reported a passing test run. | Independently verified success. |
| Captured command succeeds at revision A. | That command succeeded at A under the captured conditions. | Production deployment is healthy. |
| Patch changes one import case. | That case's behavior changed. | A new domain invariant applies project-wide. |
| Operator explicitly adopts an approach. | Decision with its stated scope and effective context. | Authorization for every downstream action. |

Structured output supports validation of shape, not semantic truth. Schema-valid hallucinations remain possible. Validate source references and mechanically checkable values; evaluate semantic faithfulness with representative cases.

## 5. Consolidation as scoped state maintenance

Consolidation consumes the relevant current understanding, new candidate claims, original supporting evidence and applicable operator corrections. It proposes a patch or a supported no-change result.

```text
Existing topic version
+ newly incorporated relevant claims/evidence
+ recorded corrections and unresolved challenges
→ proposed semantic operations + sources + questions
```

Use stable scopes such as project, topic and environment where appropriate. Session identity is usually provenance, not the permanent boundary of learning. Access-control boundaries still constrain cross-session consolidation.

Standing questions can organize understanding: which requirements govern this component, why was this architecture selected, what remains structurally uncertain, or which failure conditions are recurrent? Do not generate a page for every possible question before it proves useful.

## 6. Semantic operations

An implementation can express additions, refinements, supersession, issue resolution, qualification, no-change and conflict. These are examples, not mandated endpoint verbs.

The proposed change must show the previous meaning, proposed meaning, support and counterevidence, affected scope and practical consequence. A rewritten paragraph alone hides whether the model introduced a new requirement or merely improved wording.

Preserve unchanged content where possible. Rewriting an entire project summary every cycle creates unnecessary work and obscures the change being evaluated. Periodic broader consistency checks can still be useful when evidence suggests accumulated drift; they need not be the default operation.

## 7. Conflict handling

Check scope before resolving an apparent contradiction. Different branches, environments, time ranges or review targets may explain divergent claims. Preserve legitimate divergence.

Do not use “newest wins” universally. Recent agent text cannot silently overturn an explicit requirement; older accepted descriptions cannot hide new contradictory operational evidence. A proposed explanation can remain pending while the current-work view immediately reports the discrepancy.

A model confidence score is not a substitute for evidence or operator authority. When the evidence is insufficient, state the specific unresolved question. Preparation should investigate available evidence before sending the question to the operator.

## 8. Avoid self-reinforcing summaries

Trace understanding to original evidence through inspectable dependencies. Pages can guide retrieval, but must not serve as independent support for one another. Repeated copies of a derived statement count as one origin where identifiable.

Persist operator corrections with the rejected interpretation and its scope. A fresh consolidation run must not recreate the same mistake from the same old sources. A later materially different question may still be raised, but it must identify what changed.

Deleting or invalidating evidence should trigger reconsideration of dependent representations under the custody policy, not automatic restoration from a hidden summary cache.

## 9. Local model experiments

Keep the current model as baseline and record actual model identifier, quantization, template, serving build, reasoning mode, sampling and relevant concurrency. Do not infer these from an endpoint alias.

The official Qwen3.5-9B card documents non-thinking requests using `chat_template_kwargs` with `enable_thinking: false`. Verify the deployed server/template behavior rather than assuming the setting reaches the model. The available 4B variant is a candidate to test, not a proven replacement. [E11, E12]

The llama.cpp server documentation exposes template arguments, constrained JSON responses and timing/usage fields. Support and field behavior depend on the deployed build; use capability-aware collection and retain missing metrics as missing. Correct JSON is not evidence of correct extraction. [E13](../sources/README.md#e13)

A useful allocation experiment is deterministic normalization, bounded small-model extraction, stronger local consolidation for changed topics, and a capable local conversational reviewer. Measure total inference and interference with active work—not just extraction throughput in isolation.

## 10. Scheduling and failure behavior

Inference workers should have bounded retries, explicit terminal failure and replayable work identity. Use idempotent processing and checkpoints so retries do not duplicate claims or lose coverage. Prefer batch scheduling that does not starve active reads or the operator conversation.

Capture continues when inference is unavailable. Readers receive coverage information; preparation must not claim complete understanding over failed ranges. No automatic cloud escalation is permitted in local-only mode.

The first increment improves and measures the capture-to-observation path itself. Existing derivation remains a comparison baseline, not a dependency that the new meeting layer must tolerate unchanged. Preserve capture custody; normalize the authorized extractor input as a replayable projection with source mappings. Test batching, abstention and source/scope faithfulness before adding a downstream meeting.

Normalization dispositions must be inspectable: included, excerpted, deduplicated capture, suppressed from model input, or unavailable. Keep rule/version and source references, with suppression reasons where applicable. This does not authorize storing secrets before redaction or retaining deleted evidence. Identical content from independent executions must not be mistaken for duplicate transport delivery.

The next increments address observation reconciliation/current state and then supported understanding changes. A claim becoming resolved, superseded, contradicted or irrelevant to active work is a lifecycle operation, not an instruction to erase its history or mark it human-accepted. Unresolved conflicts remain explicit; scope and recency do not alone decide truth.

Wholesale re-derivation, automatic backfill and model replacement remain separate, measured changes. The detailed sequence and gates are in [rollout](09-experiments-and-rollout.md).
