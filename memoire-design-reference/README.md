# Mémoire — design reference

**Edition:** 1.1 · **Design baseline:** 17 September 2026  
**Owner:** Michel Tomas · **Project:** `superbiche/memoire`  
**Purpose:** an enduring reference for the agreed direction, the behavior to preserve, the alternatives to explore, and the evidence required to claim progress.

> An evidence archive underneath a small, evolving understanding of each project, alongside a reliable account of ongoing work. Human attention is focused on meaningful changes in understanding and useful discussion of the project's direction—not approval of individual observations.

## Revision 1.1 — upstream first

The operator rejected the edition 1.0 meeting-first implementation sequence: existing capture, derivation and lifecycle defects should not be carried into a new interview layer. This edition replaces that sequence and the first-agent mission. The overall architecture and eventual meeting/feedback design remain in force.

**Implement and measure the evidence-to-observation foundation first, then observation/task-state lifecycle, incremental consolidation and lifecycle-aware reads, then the operator meeting.** These are dependency-oriented increments, not a demand to perfect every upstream component before testing downstream behavior. Existing capabilities can satisfy a stage when inspected and tested. See [rollout](design/09-experiments-and-rollout.md) and [revision record](REVISION-NOTES.md).

## Read this before implementing

This reference records a design agreed in conversation. It is **not a claim that the described capabilities already exist**, a replacement for the repository's governing instructions, or an instruction to build the entire design in one pass. Public-repository documentation was checked on 17 September 2026; private source contracts, deployed configuration and code execution were not audited. See [source register](sources/README.md).

The public repository identifies itself as a curated export and points source-repository agents to private instruction and decision surfaces. An implementing agent must inspect the actual source checkout and reconcile terminology and contracts there. Do not invent private ledger entries or presume a capability is missing because this document describes it. [R1](sources/README.md#r1)

There are two deliverables:

- **This reference**, with focused chapters and reusable templates. It explains the whole design and remains useful after the first implementation.
- **[First-agent handoff](FIRST-AGENT-HANDOFF.md)**, which limits the first implementation to a measured, replayable capture-normalization-to-observation path. Meetings are downstream consumers, not its completion criterion.

The modular reference is the maintained form. The [single-file edition](REFERENCE-COMPLETE.md) is supplied as a reading convenience; regenerate it with `python3 build_reference.py` rather than editing both independently.

## The design in twelve commitments

| ID | Commitment | Owning chapter |
|---|---|---|
| DR-01 | Capture and replayable evidence remain independent of downstream inference and human review. | [Evidence](design/02-evidence-views-and-lifecycles.md) |
| DR-02 | Work history, current task state and project understanding are distinct conceptual views over shared evidence. | [Evidence](design/02-evidence-views-and-lifecycles.md) |
| DR-03 | Recent work observations are first-class for their temporal, scoped account of work; `proposed` is not a blanket relevance penalty. | [Intent](design/01-intent-and-invariants.md) |
| DR-04 | Provenance, evidential support, review, importance, freshness and authority to act are separate dimensions. | [Evidence](design/02-evidence-views-and-lifecycles.md) |
| DR-05 | Preserve original problems, recorded rationales, limitations and corrections; derived prose cannot corroborate itself. | [Evidence](design/02-evidence-views-and-lifecycles.md) |
| DR-06 | Work item, execution and harness-session identity remain distinct; uncertain linkage does not prevent useful local identity. | [Identity](design/03-work-identity-and-time.md) |
| DR-07 | Consolidation maintains scoped understanding incrementally and proposes supported semantic changes, not automatic truth. | [Derivation](design/04-derivation-and-consolidation.md) |
| DR-08 | Reads serve purpose, scope and a total context budget; progressive disclosure includes replacement and consumer obligations. | [Retrieval](design/05-retrieval-and-progressive-disclosure.md) |
| DR-09 | Human interaction is a prepared project alignment and design conversation, not an observation inbox. | [Meetings](design/06-alignment-and-design-meetings.md) |
| DR-10 | Reconciliation, interpretation, exploration, decisions and authorization remain distinguishable in one natural conversation. | [Meetings](design/06-alignment-and-design-meetings.md) |
| DR-11 | Automatic measurements and optional scoped feedback drive traceable improvement from the first pipeline increment; meeting feedback ships with the first actual meeting capability. | [Evaluation](design/07-measurement-and-learning.md) |
| DR-12 | Operate local-only where feasible; external processing or memory export is explicit, never a silent fallback. | [Operations](design/08-locality-security-and-operations.md) |

`DR-*` are identifiers in this reference only. They are not entries in Mémoire's existing private decision ledger.

## Document map

| Document | What it owns |
|---|---|
| [01 — Intent and invariants](design/01-intent-and-invariants.md) | Goals, workload, non-goals, status of proposals and central distinctions. |
| [02 — Evidence, views and lifecycles](design/02-evidence-views-and-lifecycles.md) | Evidence foundation; claims; view ownership; authority; publication and correction. |
| [03 — Work identity and time](design/03-work-identity-and-time.md) | Ledger/coordinator/fallback identity, graph relationships, revision-specific reviews, coverage checkpoints. |
| [04 — Derivation and consolidation](design/04-derivation-and-consolidation.md) | Noise reduction, selective extraction, incrementality, source dependencies and latency experiments. |
| [05 — Retrieval and progressive disclosure](design/05-retrieval-and-progressive-disclosure.md) | Purpose-shaped reads, evidence expansion, context replacement, freshness and the consumer contract. |
| [06 — Alignment and design meetings](design/06-alignment-and-design-meetings.md) | Preparation, meaningful-change review, natural design discussion, outcomes and attention budgets. |
| [07 — Measurement and continuous learning](design/07-measurement-and-learning.md) | Metric definitions, optional feedback, diagnosis, experiments, baselines and maturity. |
| [08 — Locality, security and operations](design/08-locality-security-and-operations.md) | Data boundaries, authorization, safe capture, retention, failure behavior and migration. |
| [09 — Experiments and rollout](design/09-experiments-and-rollout.md) | Hypotheses, staged implementation, comparisons, promotion gates and non-blocking unknowns. |
| [10 — Acceptance scenarios](design/10-acceptance-scenarios.md) | Behavioral examples, deterministic checks, semantic evaluation and traceability. |
| [11 — Approaches to borrow](design/11-borrowing-and-research.md) | Verified primary-source patterns, adaptation hypotheses, limitations and evaluation questions. |
| [12 — Decisions and open choices](design/12-decisions-and-open-choices.md) | Agreed boundaries, practical defaults, unresolved mechanisms and document governance. |
| [First-agent handoff](FIRST-AGENT-HANDOFF.md) | Starting mission, scope, sequence, tests, stop conditions and completion report. |
| [Source register](sources/README.md) | Conversation provenance, supplied addendum, checked public references and evidence limits. |

Reusable templates: [meeting packet](templates/meeting-packet.md), [meeting outcomes](templates/meeting-outcomes.md), [meeting feedback](templates/meeting-feedback.md), [improvement experiment](templates/improvement-experiment.md).

## Status language

**Agreed** means the product direction was approved in this conversation. **Required behavior** states what must hold when that capability ships. **Proposed starting point** is a reversible implementation default. **Experiment** is a hypothesis, not a feature promise. **Repository check** requires inspection of the actual source checkout. **Deferred** means not necessary for the first increment, not forgotten.

Conceptual object and operation names illustrate semantics. They do not mandate database tables, exact endpoints, an exhaustive ontology, particular frameworks, or a graph database. Normative words in a behavioral chapter apply to the delivered capability; they do not make every chapter first-agent scope.

## Reading paths

For product direction, read chapters 01, 02, 06 and 07. For retrieval or compaction, read 02, 03, 05 and the relevant acceptance cases. For derivation, read 02, 04, 07 and 11. For implementation, start with the handoff, then the chapters it names, alongside the source repository's instructions.

## The initial definition of progress

A first useful increment takes a bounded, identified capture corpus through versioned normalization and selective observation derivation, preserving evidence and uncertainty. It exposes what was processed, suppressed, retained, failed or left unprocessed. Its diagnostic output supports baseline/candidate comparison for cost, narration, duplication, claim faithfulness and consequential omissions without changing legacy production behavior by accident.

This is a runnable local pipeline improvement, not only instrumentation or a new meeting interface. Promotion requires evidence at the scope claimed; an inconclusive candidate remains unpromoted rather than being described as improved. Later increments establish lifecycle-correct current work and supported consolidation changes before a meeting agent consumes them.

Longer-term success means correct task continuation and challengeable project understanding with less noise, sustainable inference work and operator reconciliation effort proportional to meaningful change. Routine work spanning roughly two days should usually require only a few minutes of reconciliation; a significant pending knowledge change can justify more. Elective design discussion is measured separately. These are targets to test, not achieved results.

## Sources and updates

The governing basis is the operator's approvals in the design conversation [C1](sources/README.md#c1), refined by the supplied retrieval addendum [A1](sources/README.md#a1). External engines are sources of mechanisms, not authorities over the operator's goals. Citations such as `[R1](sources/README.md#r1)` or `[E3](sources/README.md#e3)` resolve in the [source register](sources/README.md). The original addendum is included unchanged at [sources/original-addendum.md](sources/original-addendum.md).

When practice changes a tuning choice, record the experiment and update the owning chapter. When a product invariant changes, record an explicit operator decision and its scope. Do not silently rewrite the historical rationale for either.
