# 11 — Approaches to borrow and questions to test

**Status:** external documentation checked 17 September 2026; adaptations below are design proposals.  
**Purpose:** preserve the useful research without turning product marketing or another engine's defaults into Mémoire requirements.  
**Sources:** exact primary-source URLs are in the [source register](../sources/README.md).

## 1. Interpretation of “successful engines”

These systems supply documented mechanisms worth examining. This reference does not rank their production success or claim independent benchmarks on the operator's data or hardware. Their model/provider defaults, terminology and deployment costs differ. A capability described by its authors is not evidence that it will solve Mémoire's latency or human-attention problem unchanged.

Borrow mechanisms at existing Mémoire seams. A new engine dependency is not required to adopt a useful idea. Check licenses and current integration details before copying implementation code; this reference recommends conceptual adaptation, not unreviewed code transplantation.

## 2. Hindsight — observations and maintained topic answers

**Documented mechanism.** Hindsight consolidates related retained material into observations with supporting evidence. Mental models maintain answers to selected standing questions, with freshness and provenance/version support; its documentation describes incremental refresh. Knowledge pages provide a document-oriented organization and explicitly avoid reading other pages as evidence. [E1, E2, E3]

**Mémoire adaptation.** Maintain answers about project requirements, architecture, recurring conditions and structural uncertainty; update only the affected scope. Keep source-grounded changes and anti-self-corroboration behavior. Add Mémoire's operator publication boundary rather than treating automatic synthesis as approved knowledge.

**Test.** Compare incremental topic updates with full regeneration for unnecessary churn, missed contradictions, source traceability and total preparation work. A useful page may accelerate reads while making writes more expensive; measure both.

**Locality note.** Hindsight documents local providers, including llama.cpp. That establishes an available integration path, not a quality or speed guarantee for the operator's chosen model. Its defaults and supporting models still require a boundary audit. [E4](../sources/README.md#e4)

## 3. Mastra — observation buffering and reflection

**Documented mechanism.** Mastra uses an Observer and Reflector, with threshold/idle-related buffering. Its current retrieval mode links observation groups to original messages for expansion. The documented reflector rewrites its observation log; it is not equivalent to Mémoire's proposed evidence-backed operator publication workflow. [E5](../sources/README.md#e5)

**Mémoire adaptation.** Batch meaningful activity instead of running a full operation for every event. Preserve expandable source ranges. Use reflection as bounded context maintenance where appropriate, not as an excuse to discard original evidence or recursively certify summaries.

**Test.** Measure source coverage, latency, total context and missed recent transitions at different buffering thresholds. Do not carry external default models or thresholds into local-only mode silently. Compare actual completed context products, not nominal per-layer caps.

## 4. Letta — versioned memory and dreaming

**Documented mechanism.** Letta's dreaming reviews recent conversations and updates memory; its optional agent-review step is another model conversation, not operator approval. MemFS supplies Git-backed memory versions and local-only storage, with files outside the always-present system area available for on-demand access. [E6, E7]

**Mémoire adaptation.** Produce inspectable patches and retain version ancestry; expose compact context with deeper material discoverable. Database versions can provide the desired behavior without replacing Mémoire's store with Git.

**Test.** Verify exact-version approval, concurrent-edit conflict handling and whether explicit operator corrections survive later updates. Do not treat “agent reviewed” as human validation or assume a local memory repository means every inference call is local.

## 5. Mem0 — selective extraction and legitimate abstention

**Documented mechanism.** Mem0's custom-instructions documentation shows allowed fact types, positive and negative examples, and empty fact collections. Its current add-memory documentation describes additive storage rather than automatic overwrite/delete reconciliation. [E8, E9]

**Mémoire adaptation.** Narrow extraction to consequential claims and make no-op output a valid result. Preserve the evidence archive independently. Implement current-state and supersession semantics explicitly rather than assuming an external `add` operation supplies them.

**Test.** Compare noise reduction against recall of critical constraints, outcomes and useful failures. Empty output is successful processing only when the call completed and the eligible scope was actually considered.

## 6. Graphiti — time and provenance

**Documented mechanism.** Graphiti represents changing facts with temporal validity and source-episode provenance. Its documentation supports OpenAI-compatible local model endpoints and warns that smaller/local models can struggle with structured extraction. [E10](../sources/README.md#e10)

**Mémoire adaptation.** Preserve event versus arrival time, validity scope, correction history and historical queries. Keep concurrent environments separate. Adopt the temporal distinctions before deciding whether graph storage is warranted.

**Test.** Late evidence, review of old revisions, changed requirements and branch divergence should remain correct. Measure extraction/reconciliation work and failure rates; structured graph output is not free simply because inference is local.

## 7. Local model and serving references

**Qwen3.5-9B / 4B.** Official cards provide the exact model references; the 9B card documents non-thinking requests. Benchmark the existing 9B pipeline first, then a smaller extractor under controlled conditions. No throughput, memory-fit or quality prediction is established here. [E11, E12]

**llama.cpp.** The server documentation describes request template controls, JSON constraints and timing/usage responses. Inspect the installed build and actual template, record capabilities and preserve absent fields honestly. A valid response schema does not certify a supported claim. [E13](../sources/README.md#e13)

## 8. LongMemEval — supplementary evaluation, not the product scorecard

**Documented scope.** LongMemEval evaluates information extraction, multi-session reasoning, temporal reasoning, knowledge updates and abstention in long-term chat memory. [E14](../sources/README.md#e14)

**Mémoire adaptation.** Reuse those evaluation dimensions where relevant. Add the workload-specific requirements absent from a generic chat-memory score: parallel task/revision identity, actual authorization, meeting usefulness, operator burden, capture gaps and complete consumer context cost.

**Test.** A system should not be promoted solely on benchmark question-answering scores. Use Mémoire fixtures and live meetings alongside external evaluation where it adds information.

## 9. Research boundaries carried into the handoff

The first agent need not install or benchmark all these engines. Start by understanding the mechanisms and inspecting existing Mémoire equivalents. Deeper research is justified when a concrete implementation choice needs it.

External documentation is mutable. Verify exact APIs and defaults at implementation time. Source-check dates are provenance, not a claim of a pinned external software build. The primary purpose of this chapter is to preserve why each mechanism was considered and what would count as evidence that borrowing it helped.
