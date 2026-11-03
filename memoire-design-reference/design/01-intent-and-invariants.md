# 01 — Intent, goals and invariants

**Status:** agreed product direction; implementation mechanisms remain testable proposals.  
**Owns:** DR-03 and the interpretation of the other reference commitments.  
**Basis:** design conversation [C1](../sources/README.md#c1); addendum [A1, lines 9–23, 67–69, 112–118](../sources/README.md#a1). Source IDs resolve in [the source register](../sources/README.md).

## 1. The problem being solved

Mémoire captures harness activity, including user turns, agent turns and tool activity, and derives observations. The operator reports that Qwen 3.5 9B derivation is too slow and that individually reviewing the resulting observations is not sustainable. The desired product is not a more polished transcript. It is an accessible account of what happened, what matters, and where work stands, with deeper project understanding that can be challenged and improved.

The workload includes parallel conversations, different models, multi-agent execution graphs, dependent work items and agent reviews. Some executions have a ledger or coordinator; ordinary sessions may have neither. Post-compaction continuation is an important consumer, but must not become the architecture's sole organizing use case.

The system must support both active work and reflective discussion. Recent observations cannot wait for a human meeting before becoming useful. At the same time, a recent implementation detail must not quietly become a governing architectural principle.

## 2. Intended products

The common foundation is source evidence retained under the operator's custody policy. Above it, three conceptual views answer different questions:

| View | Primary question | Examples |
|---|---|---|
| Work history | What happened, and how did we reach this outcome? | Problem, attempts, failures, recorded rationale, verification and remaining uncertainty. |
| Current task state | Where are we now, and what remains? | Objective, active constraints, authorized scope, blockers, current verified or reported progress. |
| Project understanding | What do we understand about this project, and what governs future work? | Requirements, decisions, architecture, trade-offs, qualified lessons and structural uncertainties. |

A fourth concern—the quality of Mémoire's own support—belongs to an operational evaluation record. It is not another competing narrative about the target project's domain.

The views do not require three databases or independent ownership of duplicated facts. A supported claim can contribute to multiple views. Its contribution can differ: a test result can establish historical verification and task progress without establishing a domain requirement.

## 3. First-class current work

The operator explicitly rejected treating provisional observations as second-class material. Their review state remains proposed where applicable, but for a question about ongoing work they are the primary account of what is known within their scope and coverage.

This is **scoped epistemic authority**, not universal correctness or permission to act. An agent's completion report is authoritative evidence that it reported completion. A successful captured command establishes a different fact. An independent review establishes another, tied to what was reviewed.

A read about current work should not prefer an old accepted statement merely because it was accepted. A read about governing requirements should not treat recent behavior as an implicit amendment. Return the relevant state of knowledge for the caller's purpose, including unresolved challenges.

Uncertainty attaches to actual deficiencies—missing evidence, disputed attribution, conflicting results—not automatically to the absence of human review.

## 4. Why there is no universal lifecycle

The initial capture → derivation → consolidation → review → validated sketch remains useful for one publication path, but not as a mandatory sequence for all memory.

Captured evidence can be searchable before derivation. Supported task transitions can update without an interview. A material reinterpretation of a requirement may need operator discussion. An explicit operator decision made in that discussion is itself decision evidence and does not need another interview merely to acknowledge it.

Consequently, “accepted,” “current,” “verified,” “important” and “authorized” must not be aliases. Quality and validity are not one scalar trust score.

## 5. Goals

**Useful continuity.** An agent resumes the correct work item with its objective, constraints, outstanding work and latest relevant results, without reopening resolved tasks or borrowing another task's review.

**Challengeable understanding.** Maintained explanations retain scope, support, conflicting evidence and version history. The operator can recover the original problem and distinguish original rationale from later interpretation.

**Low-noise derivation.** Activity that does not change useful state or understanding remains available as evidence without manufacturing an observation or review item. Valuable failures and discoveries remain retainable.

**Sustainable human attention.** Meetings are prepared around consequential changes in meaning, not activity volume. Routine continuity can produce no project-understanding change. The conversation can also develop ideas and direction.

**Measured local operation.** Inference, reads, preparation and feedback are observable. Local-only processing is the default target, with explicit boundaries for any external use. Claimed improvements are measured on representative work rather than inferred from attractive architecture.

## 6. Non-goals

The redesign is not a replacement of Mémoire with another engine. It does not require a graph database, cloud inference, a universal event ontology, a new orchestration platform or an enormous project wiki. It is not a system for automatic approval of model statements. It is not an observation-by-observation human review interface disguised as a meeting.

It is also not an autonomous self-modification program. Agents may propose and test improvements, but changes to behavior remain versioned, attributable and reversible.

## 7. Behavioral invariants

| Invariant | Consequence |
|---|---|
| Source evidence and interpretation are distinguishable. | An interpretation can be corrected without falsifying what the source originally said. |
| Scope is preserved. | A branch result, test case or temporary exception does not silently become project-wide truth. |
| Capture is not gated by derivation or review. | Slow models or absent operator attention do not stop evidence intake. |
| Derived material is not fresh independent evidence for itself. | Re-injected memory and copied summaries cannot inflate corroboration. |
| Review targets are exact. | Approval of one revision does not approve another revision or hidden claims. |
| Recency is not a universal conflict rule. | New behavior can violate an existing requirement without amending it. |
| Silence is not approval or satisfaction. | Unanswered proposals remain pending; absent feedback is missing evaluation. |
| Omissions are part of the read contract. | Clipping, lag and absent matches are not presented as the same result. |
| Attention and context are bounded as combined products. | Many individually small representations cannot silently accumulate without replacement. |
| Improvement has provenance. | A behavior change points to its motivation, configuration and evaluation. |

## 8. Readiness versus certainty

The design is ready to implement because its product boundaries and evaluation obligations are clear. It is not ready because every threshold, model allocation or classification policy has been proven. Those are deliberately left to replay and real meetings.

No further product clarification is required before the first additive, reversible increment. An implementation agent may need to inspect actual source instructions, storage contracts and current components. Only a genuine unresolved authorization, privacy or destructive-migration decision should block the affected action; it should not become a reason to restart the design discussion.
