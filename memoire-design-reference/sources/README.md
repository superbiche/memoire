# Source register and evidence boundaries

**Checked:** 17 September 2026. External URLs are mutable; this is a documentation-level check, not a code audit or benchmark. The public export was not pinned to an immutable commit in this package. Recheck implementation details against the actual checkout and deployed versions.

Source IDs in the reference point here. Design proposals are authored from the discussion; they are not presented as claims that an external engine already implements Mémoire's complete workflow.

## Primary project-design basis

<a id="c1"></a>

**Edition 1.1 addition:** the operator challenged the interview-first handoff and requested capture cleanup, improved observations and upstream lifecycle work before the human-facing layer. The revised sequencing follows that instruction; detailed stage gates remain proposals. External research below is inherited, not rechecked in this documentation-only revision.

### C1 — Operator design conversation

The conversation establishes the problem, agreed product boundaries and intended evaluation loop. It is not reproduced as a verbatim transcript. Its approved direction is recorded in the reference's `DR-01` through `DR-12` commitments and owning chapters.

The material progression was: evidence archive and meaningful-change review; three conceptual views from the addendum; first-class temporal authority for current work; high-level reconciliation plus design discussion; ledger/coordinator/session identity; coverage-aware diffs; and automatic measurements plus optional exact meeting feedback from first use. Low-level mechanisms and first-implementation sequencing remain proposals where labeled.

<a id="a1"></a>

### A1 — Supplied design addendum

[Original addendum, unchanged](original-addendum.md): *Memoire design addendum: history, task state, and project understanding*, uploaded as `Pasted text.txt`.

Citations to A1 line ranges refer to the numbered source supplied in the conversation. The relevant sections are the three views (15–23), historical rationale (25–39), independent task state and review (41–69), experiment findings and limitations (71–85), read contracts (87–99), and success criteria (112–118).

The historical response instructions inside the addendum are source material, not the current implementation assignment. Its experiment outcomes are reported observations, not independently reproduced results. No local repository, private answers, original checkpoints, complete read products or scoring artifacts were supplied. The source explicitly says the experimental candidates were not promoted and that the V15 trace-growth/clipping errors belonged to the lab design.

## Mémoire public documentation

| ID | Primary source | Use and limitation |
|---|---|---|
| <a id="r1"></a>R1 | [Public README](https://raw.githubusercontent.com/superbiche/memoire/main/README.md) | Repository boundary, source/export distinction, lanes and pointers to private governing instructions. Not the private checkout or deployment. |
| <a id="r2"></a>R2 | [Public quickstart](https://raw.githubusercontent.com/superbiche/memoire/main/QUICKSTART.md) | Standalone path, API writer boundary, explicit provider dialect/data boundary and external invocation allowance. Not a complete hub contract. |
| <a id="r3"></a>R3 | [Public MISSING record](https://raw.githubusercontent.com/superbiche/memoire/main/MISSING.md) | Dated issue/correction history, including existing state/knowledge documentation and custody corrections. Read status notes; old filings are not necessarily current gaps. |
| <a id="r4"></a>R4 | [Public VERSION](https://raw.githubusercontent.com/superbiche/memoire/main/VERSION) | Export reports 0.7.0 when checked. This does not identify the current private branch or running deployment. |

No private `memoire:D<n>` entries were inspected or invented. An implementing agent must obtain source contracts from its actual checkout. The new `DR-*` identifiers are local to this reference only.

## External mechanisms and local-model references

| ID | Primary source | Relevance |
|---|---|---|
| <a id="e1"></a>E1 | [Hindsight — observations](https://hindsight.vectorize.io/developer/observations) | Evidence-linked consolidation. |
| <a id="e2"></a>E2 | [Hindsight — mental models](https://hindsight.vectorize.io/developer/mental-models) | Maintained standing answers, scope, freshness, version/provenance and incremental refresh. |
| <a id="e3"></a>E3 | [Hindsight — knowledge pages](https://hindsight.vectorize.io/developer/knowledge-pages) | Document organization, incremental edits and no page-to-page evidence loop. |
| <a id="e4"></a>E4 | [Hindsight — models](https://hindsight.vectorize.io/developer/models) | Documented local-provider options; not local quality/latency proof. |
| <a id="e5"></a>E5 | [Mastra — observational memory](https://mastra.ai/docs/memory/observational-memory) | Observer/Reflector, buffering and linked-source retrieval mode. |
| <a id="e6"></a>E6 | [Letta — memory and dreaming](https://docs.letta.com/configuration/memory) | Dreaming and distinction between agent review and human approval. The earlier sleep-time URL redirects here. |
| <a id="e7"></a>E7 | [Letta — MemFS](https://docs.letta.com/concepts/memfs) | Versioned memory and local-only storage; on-demand file access. |
| <a id="e8"></a>E8 | [Mem0 — custom instructions](https://docs.mem0.ai/open-source/features/custom-instructions) | Narrow fact extraction with positive and empty-output examples. The earlier custom-fact-extraction URL redirects here. |
| <a id="e9"></a>E9 | [Mem0 — add memory](https://docs.mem0.ai/core-concepts/memory-operations/add) | Current documentation describes additive ingestion; do not assume automatic canonical-state reconciliation. |
| <a id="e10"></a>E10 | [Graphiti — official repository](https://github.com/getzep/graphiti) | Temporal facts, episode provenance and documented local-provider/structured-output caveats. |
| <a id="e11"></a>E11 | [Qwen3.5-9B — official model card](https://huggingface.co/Qwen/Qwen3.5-9B) | Model reference and documented non-thinking configuration. |
| <a id="e12"></a>E12 | [Qwen3.5-4B — official model card](https://huggingface.co/Qwen/Qwen3.5-4B) | Smaller-model candidate; no workload-specific recommendation established. |
| <a id="e13"></a>E13 | [llama.cpp — server documentation](https://raw.githubusercontent.com/ggml-org/llama.cpp/master/tools/server/README.md) | Template controls, structured responses and usage/timing observability. Installed-build behavior must be checked. |
| <a id="e14"></a>E14 | [LongMemEval — paper abstract/version record](https://arxiv.org/abs/2410.10813) | Supplementary evaluation dimensions for long-term chat memory. Abstract and metadata inspected; no PDF analysis or benchmark execution performed. |

## How to interpret the research

The borrowing chapter identifies documented mechanisms and separately proposes Mémoire adaptations. It does not rank “successful” engines by popularity or claim a benchmark winner. Vendor statements about accuracy, compression, throughput or being state-of-the-art were not adopted as Mémoire acceptance evidence.

No external engine was installed or tested here. No local inference speed, hardware fit, meeting duration, extraction accuracy or performance improvement was measured. Numerical targets in the design are explicitly operator goals or pilot hypotheses; addendum numbers retain their reported limitations.

Documentation paths and defaults can change. The first agent should verify the exact interface it intends to use, not repeat all research or silently substitute new external product defaults for approved Mémoire behavior.
