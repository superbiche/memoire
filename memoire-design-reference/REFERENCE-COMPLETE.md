# Mémoire — complete design reference

**Edition 1.1 · 17 September 2026.** Generated from the modular reference. Edit the owning modular chapters and run `python3 build_reference.py` to regenerate.

The separate [first-agent handoff](FIRST-AGENT-HANDOFF.md) limits the initial implementation. This document contains the full design, experiments, acceptance cases, templates and source basis.

[Reference map](#reference-map) · [Meeting design](#chapter-06) · [Measurement](#chapter-07) · [Acceptance cases](#chapter-10) · [Sources](#source-register)


---

<a id="reference-map"></a>

## Mémoire — design reference

**Edition:** 1.1 · **Design baseline:** 17 September 2026  
**Owner:** Michel Tomas · **Project:** `superbiche/memoire`  
**Purpose:** an enduring reference for the agreed direction, the behavior to preserve, the alternatives to explore, and the evidence required to claim progress.

> An evidence archive underneath a small, evolving understanding of each project, alongside a reliable account of ongoing work. Human attention is focused on meaningful changes in understanding and useful discussion of the project's direction—not approval of individual observations.

### Revision 1.1 — upstream first

The operator rejected the edition 1.0 meeting-first implementation sequence: existing capture, derivation and lifecycle defects should not be carried into a new interview layer. This edition replaces that sequence and the first-agent mission. The overall architecture and eventual meeting/feedback design remain in force.

**Implement and measure the evidence-to-observation foundation first, then observation/task-state lifecycle, incremental consolidation and lifecycle-aware reads, then the operator meeting.** These are dependency-oriented increments, not a demand to perfect every upstream component before testing downstream behavior. Existing capabilities can satisfy a stage when inspected and tested. See [rollout](#chapter-09) and [revision record](#revision-notes).

### Read this before implementing

This reference records a design agreed in conversation. It is **not a claim that the described capabilities already exist**, a replacement for the repository's governing instructions, or an instruction to build the entire design in one pass. Public-repository documentation was checked on 17 September 2026; private source contracts, deployed configuration and code execution were not audited. See [source register](#source-register).

The public repository identifies itself as a curated export and points source-repository agents to private instruction and decision surfaces. An implementing agent must inspect the actual source checkout and reconcile terminology and contracts there. Do not invent private ledger entries or presume a capability is missing because this document describes it. [R1](#r1)

There are two deliverables:

- **This reference**, with focused chapters and reusable templates. It explains the whole design and remains useful after the first implementation.
- **[First-agent handoff](FIRST-AGENT-HANDOFF.md)**, which limits the first implementation to a measured, replayable capture-normalization-to-observation path. Meetings are downstream consumers, not its completion criterion.

The modular reference is the maintained form. The [single-file edition](#reference-map) is supplied as a reading convenience; regenerate it with `python3 build_reference.py` rather than editing both independently.

### The design in twelve commitments

| ID | Commitment | Owning chapter |
|---|---|---|
| DR-01 | Capture and replayable evidence remain independent of downstream inference and human review. | [Evidence](#chapter-02) |
| DR-02 | Work history, current task state and project understanding are distinct conceptual views over shared evidence. | [Evidence](#chapter-02) |
| DR-03 | Recent work observations are first-class for their temporal, scoped account of work; `proposed` is not a blanket relevance penalty. | [Intent](#chapter-01) |
| DR-04 | Provenance, evidential support, review, importance, freshness and authority to act are separate dimensions. | [Evidence](#chapter-02) |
| DR-05 | Preserve original problems, recorded rationales, limitations and corrections; derived prose cannot corroborate itself. | [Evidence](#chapter-02) |
| DR-06 | Work item, execution and harness-session identity remain distinct; uncertain linkage does not prevent useful local identity. | [Identity](#chapter-03) |
| DR-07 | Consolidation maintains scoped understanding incrementally and proposes supported semantic changes, not automatic truth. | [Derivation](#chapter-04) |
| DR-08 | Reads serve purpose, scope and a total context budget; progressive disclosure includes replacement and consumer obligations. | [Retrieval](#chapter-05) |
| DR-09 | Human interaction is a prepared project alignment and design conversation, not an observation inbox. | [Meetings](#chapter-06) |
| DR-10 | Reconciliation, interpretation, exploration, decisions and authorization remain distinguishable in one natural conversation. | [Meetings](#chapter-06) |
| DR-11 | Automatic measurements and optional scoped feedback drive traceable improvement from the first pipeline increment; meeting feedback ships with the first actual meeting capability. | [Evaluation](#chapter-07) |
| DR-12 | Operate local-only where feasible; external processing or memory export is explicit, never a silent fallback. | [Operations](#chapter-08) |

`DR-*` are identifiers in this reference only. They are not entries in Mémoire's existing private decision ledger.

### Document map

| Document | What it owns |
|---|---|
| [01 — Intent and invariants](#chapter-01) | Goals, workload, non-goals, status of proposals and central distinctions. |
| [02 — Evidence, views and lifecycles](#chapter-02) | Evidence foundation; claims; view ownership; authority; publication and correction. |
| [03 — Work identity and time](#chapter-03) | Ledger/coordinator/fallback identity, graph relationships, revision-specific reviews, coverage checkpoints. |
| [04 — Derivation and consolidation](#chapter-04) | Noise reduction, selective extraction, incrementality, source dependencies and latency experiments. |
| [05 — Retrieval and progressive disclosure](#chapter-05) | Purpose-shaped reads, evidence expansion, context replacement, freshness and the consumer contract. |
| [06 — Alignment and design meetings](#chapter-06) | Preparation, meaningful-change review, natural design discussion, outcomes and attention budgets. |
| [07 — Measurement and continuous learning](#chapter-07) | Metric definitions, optional feedback, diagnosis, experiments, baselines and maturity. |
| [08 — Locality, security and operations](#chapter-08) | Data boundaries, authorization, safe capture, retention, failure behavior and migration. |
| [09 — Experiments and rollout](#chapter-09) | Hypotheses, staged implementation, comparisons, promotion gates and non-blocking unknowns. |
| [10 — Acceptance scenarios](#chapter-10) | Behavioral examples, deterministic checks, semantic evaluation and traceability. |
| [11 — Approaches to borrow](#chapter-11) | Verified primary-source patterns, adaptation hypotheses, limitations and evaluation questions. |
| [12 — Decisions and open choices](#chapter-12) | Agreed boundaries, practical defaults, unresolved mechanisms and document governance. |
| [First-agent handoff](FIRST-AGENT-HANDOFF.md) | Starting mission, scope, sequence, tests, stop conditions and completion report. |
| [Source register](#source-register) | Conversation provenance, supplied addendum, checked public references and evidence limits. |

Reusable templates: [meeting packet](#template-meeting-packet), [meeting outcomes](#template-meeting-outcomes), [meeting feedback](#template-meeting-feedback), [improvement experiment](#template-improvement-experiment).

### Status language

**Agreed** means the product direction was approved in this conversation. **Required behavior** states what must hold when that capability ships. **Proposed starting point** is a reversible implementation default. **Experiment** is a hypothesis, not a feature promise. **Repository check** requires inspection of the actual source checkout. **Deferred** means not necessary for the first increment, not forgotten.

Conceptual object and operation names illustrate semantics. They do not mandate database tables, exact endpoints, an exhaustive ontology, particular frameworks, or a graph database. Normative words in a behavioral chapter apply to the delivered capability; they do not make every chapter first-agent scope.

### Reading paths

For product direction, read chapters 01, 02, 06 and 07. For retrieval or compaction, read 02, 03, 05 and the relevant acceptance cases. For derivation, read 02, 04, 07 and 11. For implementation, start with the handoff, then the chapters it names, alongside the source repository's instructions.

### The initial definition of progress

A first useful increment takes a bounded, identified capture corpus through versioned normalization and selective observation derivation, preserving evidence and uncertainty. It exposes what was processed, suppressed, retained, failed or left unprocessed. Its diagnostic output supports baseline/candidate comparison for cost, narration, duplication, claim faithfulness and consequential omissions without changing legacy production behavior by accident.

This is a runnable local pipeline improvement, not only instrumentation or a new meeting interface. Promotion requires evidence at the scope claimed; an inconclusive candidate remains unpromoted rather than being described as improved. Later increments establish lifecycle-correct current work and supported consolidation changes before a meeting agent consumes them.

Longer-term success means correct task continuation and challengeable project understanding with less noise, sustainable inference work and operator reconciliation effort proportional to meaningful change. Routine work spanning roughly two days should usually require only a few minutes of reconciliation; a significant pending knowledge change can justify more. Elective design discussion is measured separately. These are targets to test, not achieved results.

### Sources and updates

The governing basis is the operator's approvals in the design conversation [C1](#c1), refined by the supplied retrieval addendum [A1](#a1). External engines are sources of mechanisms, not authorities over the operator's goals. Citations such as `[R1](#r1)` or `[E3](#e3)` resolve in the [source register](#source-register). The original addendum is included unchanged at [sources/original-addendum.md](#source-addendum).

When practice changes a tuning choice, record the experiment and update the owning chapter. When a product invariant changes, record an explicit operator decision and its scope. Do not silently rewrite the historical rationale for either.


---

<a id="revision-notes"></a>

## Revision record

### Edition 1.1 — 17 September 2026

**Reason:** after receiving edition 1.0, the operator challenged its interview-first implementation order and requested low-level capture cleanup, better observations and upstream lifecycle work first, to avoid carrying compounding defects into the human-facing layer.

**Changed:** reference index and initial definition of progress; first-agent mission; rollout sequencing and stage gates; derivation's first-increment guidance; identity and read-probe sequencing; foundational measurement/feedback scope; operations' shadow/candidate wording; decisions/open-choices record. The single-file edition is regenerated from these owning documents.

**First mission:** a bounded, measured capture-normalization-to-observation path. The broader sequence then improves observation/task-state lifecycle, incremental consolidation and lifecycle-aware reads before the alignment/design meeting.

**Preserved:** the three views and separate lifecycles; first-class current work; evidence/authority/publication distinctions; identity design; conversational reconciliation and design; optional exact meeting feedback when meetings ship; local boundary; the original addendum and existing external research/source dates.

**Implementation status:** this revision changes documentation only. It does not audit or modify Mémoire, execute project tests, benchmark models, validate live observations or conduct operator meetings. Research references are inherited from edition 1.0, not newly verified by this sequencing revision.

**Status of detail:** the operator supplied the upstream-first direction. The revised stage partition, first-agent scope and gates are design proposals implementing that direction; they are not falsely attributed as additional explicit operator decisions.

Edition 1.0's meeting-first handoff is superseded. Original edition 1.0 delivery files remain unchanged outside this package for provenance.


---

<a id="chapter-01"></a>

## 01 — Intent, goals and invariants

**Status:** agreed product direction; implementation mechanisms remain testable proposals.  
**Owns:** DR-03 and the interpretation of the other reference commitments.  
**Basis:** design conversation [C1](#c1); addendum [A1, lines 9–23, 67–69, 112–118](#a1). Source IDs resolve in [the source register](#source-register).

### 1. The problem being solved

Mémoire captures harness activity, including user turns, agent turns and tool activity, and derives observations. The operator reports that Qwen 3.5 9B derivation is too slow and that individually reviewing the resulting observations is not sustainable. The desired product is not a more polished transcript. It is an accessible account of what happened, what matters, and where work stands, with deeper project understanding that can be challenged and improved.

The workload includes parallel conversations, different models, multi-agent execution graphs, dependent work items and agent reviews. Some executions have a ledger or coordinator; ordinary sessions may have neither. Post-compaction continuation is an important consumer, but must not become the architecture's sole organizing use case.

The system must support both active work and reflective discussion. Recent observations cannot wait for a human meeting before becoming useful. At the same time, a recent implementation detail must not quietly become a governing architectural principle.

### 2. Intended products

The common foundation is source evidence retained under the operator's custody policy. Above it, three conceptual views answer different questions:

| View | Primary question | Examples |
|---|---|---|
| Work history | What happened, and how did we reach this outcome? | Problem, attempts, failures, recorded rationale, verification and remaining uncertainty. |
| Current task state | Where are we now, and what remains? | Objective, active constraints, authorized scope, blockers, current verified or reported progress. |
| Project understanding | What do we understand about this project, and what governs future work? | Requirements, decisions, architecture, trade-offs, qualified lessons and structural uncertainties. |

A fourth concern—the quality of Mémoire's own support—belongs to an operational evaluation record. It is not another competing narrative about the target project's domain.

The views do not require three databases or independent ownership of duplicated facts. A supported claim can contribute to multiple views. Its contribution can differ: a test result can establish historical verification and task progress without establishing a domain requirement.

### 3. First-class current work

The operator explicitly rejected treating provisional observations as second-class material. Their review state remains proposed where applicable, but for a question about ongoing work they are the primary account of what is known within their scope and coverage.

This is **scoped epistemic authority**, not universal correctness or permission to act. An agent's completion report is authoritative evidence that it reported completion. A successful captured command establishes a different fact. An independent review establishes another, tied to what was reviewed.

A read about current work should not prefer an old accepted statement merely because it was accepted. A read about governing requirements should not treat recent behavior as an implicit amendment. Return the relevant state of knowledge for the caller's purpose, including unresolved challenges.

Uncertainty attaches to actual deficiencies—missing evidence, disputed attribution, conflicting results—not automatically to the absence of human review.

### 4. Why there is no universal lifecycle

The initial capture → derivation → consolidation → review → validated sketch remains useful for one publication path, but not as a mandatory sequence for all memory.

Captured evidence can be searchable before derivation. Supported task transitions can update without an interview. A material reinterpretation of a requirement may need operator discussion. An explicit operator decision made in that discussion is itself decision evidence and does not need another interview merely to acknowledge it.

Consequently, “accepted,” “current,” “verified,” “important” and “authorized” must not be aliases. Quality and validity are not one scalar trust score.

### 5. Goals

**Useful continuity.** An agent resumes the correct work item with its objective, constraints, outstanding work and latest relevant results, without reopening resolved tasks or borrowing another task's review.

**Challengeable understanding.** Maintained explanations retain scope, support, conflicting evidence and version history. The operator can recover the original problem and distinguish original rationale from later interpretation.

**Low-noise derivation.** Activity that does not change useful state or understanding remains available as evidence without manufacturing an observation or review item. Valuable failures and discoveries remain retainable.

**Sustainable human attention.** Meetings are prepared around consequential changes in meaning, not activity volume. Routine continuity can produce no project-understanding change. The conversation can also develop ideas and direction.

**Measured local operation.** Inference, reads, preparation and feedback are observable. Local-only processing is the default target, with explicit boundaries for any external use. Claimed improvements are measured on representative work rather than inferred from attractive architecture.

### 6. Non-goals

The redesign is not a replacement of Mémoire with another engine. It does not require a graph database, cloud inference, a universal event ontology, a new orchestration platform or an enormous project wiki. It is not a system for automatic approval of model statements. It is not an observation-by-observation human review interface disguised as a meeting.

It is also not an autonomous self-modification program. Agents may propose and test improvements, but changes to behavior remain versioned, attributable and reversible.

### 7. Behavioral invariants

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

### 8. Readiness versus certainty

The design is ready to implement because its product boundaries and evaluation obligations are clear. It is not ready because every threshold, model allocation or classification policy has been proven. Those are deliberately left to replay and real meetings.

No further product clarification is required before the first additive, reversible increment. An implementation agent may need to inspect actual source instructions, storage contracts and current components. Only a genuine unresolved authorization, privacy or destructive-migration decision should block the affected action; it should not become a reason to restart the design discussion.


---

<a id="chapter-02"></a>

## 02 — Evidence, views and update lifecycles

**Status:** required behavior for shipped capabilities; record shapes are conceptual.  
**Owns:** DR-01, DR-02, DR-04 and DR-05.  
**Basis:** [C1](#c1); [A1, lines 15–69, 97–99](#a1). Source IDs resolve in [the source register](#source-register).

### 1. Evidence foundation

Retain source events and artifacts under the existing custody and redaction policy, with stable identities and enough origin information to replay derivation or inspect a disputed claim. A captured event is not automatically a true statement about the world: it is evidence of a statement, command, result, review or other occurrence.

Keep mechanically available facts mechanical. An exit code, artifact revision or test result should not depend on the model inventing its representation from a prose retelling. The model can explain significance while retaining the original basis.

The archive need not preserve every sensitive byte forever. Deliberate redaction, deletion and quarantine remain possible under operator policy. “Replayable” means replayable from retained lawful and authorized evidence, with missing sources disclosed—not a claim that nothing can ever be removed. Public Mémoire documentation distinguishes deliberate purge from automatic pruning; do not replace that boundary with an absolute immutability rule. [R3](#r3)

### 2. Conceptual records and relationships

The following concepts must be representable where used. Reuse existing entities where they already supply the behavior.

| Concept | Minimum semantic responsibility |
|---|---|
| Evidence item | Stable source handle, origin, available timestamps, payload or locator, access scope and availability. |
| Candidate claim | Scoped assertion, supporting sources, evidence character, derivation provenance and relevant time. |
| Historical episode | Bounded account of an objective/problem, attempts, results and unresolved issues, expandable to evidence. |
| Task projection | Current state for an identified work item, with state facets and supporting events. |
| Understanding claim/version | Maintained interpretation or governing knowledge, scope, support, publication status and version ancestry. |
| Supported change | Baseline, proposed semantic change, supporting/conflicting evidence, applicability and practical consequence. |
| Meeting/outcome | Evidence checkpoint, actual discussion, explicit operator statements, decisions and unresolved matters. |
| Evaluation record | Measured behavior, optional exact feedback and links to resulting improvement work. |

A fixed enum for every possible claim is not required. Enough typing is needed to prevent known category mistakes; extensibility should follow observed need.

### 3. Dimensions that must not collapse

**Provenance:** who or what produced the material, from which session/run/artifact and by which derivation configuration.

**Evidence character:** directly observed result, reported assertion, or inferred explanation. These categories can be refined in implementation but must not disappear into a confidence number.

**Human review/publication:** whether the operator reviewed a particular interpretation or version, and what exactly was approved.

**Time and freshness:** when the fact applies, when evidence arrived, what the representation covers, and what relevant information remains unprocessed.

**Practical importance:** whether the claim changes current action, a requirement, an explanation or future work. Importance is a routing judgment, not proof.

**Action authority:** whether a current instruction or applicable standing policy authorizes an action. Memory of old permission is not present permission.

**Access scope:** who may read or process the source and its derivatives. Topic tags or session links are not access-control grants.

These dimensions can be stored in existing fields and relations. A universal weighted trust score must not substitute for them.

### 4. Three views, shared evidence

#### Work history

Preserve the original problem and intent, constraints and environment, what was tried, what was actually observed, why the solution was accepted when that reasoning is recorded, and what remained unresolved.

A later model reconstruction must be labeled as such. A later operator clarification is valuable evidence, but it was not necessarily the rationale documented at the original decision time. Missing rationale stays missing.

Chronology supports reconstruction, not causation by itself. Two events next to each other, or observations from one derivation run, are not automatically cause/effect or correction/resolution pairs.

#### Current task state

Represent objective, active constraints, authorized scope, outstanding work, blockers and supported progress. Distinguish planned, attempted, completed, verified, reviewed, merged and deployed as applicable. They are not interchangeable stages of a universal state machine.

A task can be implemented but awaiting review, reviewed but not merged, or deployed with a known failing check. Concurrent facets should not be flattened into a misleading single status. Resolved items leave the active projection while remaining available historically.

Current task state updates on sufficient evidence without requiring project-understanding review. A blanket `accepted_only` filter is not a valid implementation of this target behavior.

#### Project understanding

Maintain scoped requirements, decisions, explanations, architecture, trade-offs, reusable lessons and open structural questions. A useful shape is a maintained answer to a topic question, not a single ever-growing global summary.

A fact's repeated occurrence may motivate a broader interpretation, but repetition alone does not prove generality. A workaround remains a workaround until support or a decision justifies a different role.

### 5. Two update paths

```text
Captured evidence ──→ factual access and expandable history
        │
        ├──→ selective claims ──→ live scoped task state
        │
        └──→ relevant claims + baseline understanding + corrections
                           └──→ proposed semantic changes
                                  └──→ alignment/design conversation
                                         └──→ explicit outcomes + published revision
```

This describes behavior, not service boundaries. The same worker, database and API may implement multiple steps. Evidence does not have to pass through every stage to remain useful.

Routine operational transitions need not generate review work. Material challenges to understanding should remain visible before publication is settled. The absence of an approved replacement does not justify presenting a challenged baseline as unqualified current truth.

### 6. Baselines, challenges and requirements

When new work arrives, do not automatically demote all maintained understanding to obsolete history. Determine what changed.

If production now runs revision B, an old description of revision A becomes historical for that environment. If a test challenges an architectural explanation, retain the explanation and its challenge until resolved. If a new implementation violates a requirement, record the violation rather than treating the implementation as an amendment.

Illustration: a captured import run deletes records on a valid empty feed. That can be the current operational account. A requirement to preserve records on upstream failure may still govern. The interview determines whether the evidence reveals a bug, a missing distinction, or an intentional change in direction.

The current-work view can report the discrepancy immediately. Project-understanding publication follows the relevant interpretation or decision.

### 7. Corrections, publication and dependencies

Retain source links for every consequential published claim and relevant links between derived versions. A correction should identify what it corrects and why; simply editing the latest paragraph loses the basis needed to avoid regenerating the rejected interpretation.

Derived pages may assist navigation, but must not become independent evidence for each other. When a memory page re-enters a harness transcript, mark it as previously derived material. Its repetition is not fresh corroboration. Distinct source handles pointing to copies of one original are not independent sources either.

When evidence is invalidated, corrected, made inaccessible or deliberately deleted, mark affected derivatives for reconsideration. Do not silently keep the same confidence or expose deleted content through cached summaries. See [operations](#chapter-08).

Publication must apply to an exact displayed revision and base version. Use the repository's transaction or concurrency mechanism to prevent an interview against an old baseline from overwriting a newer decision. A conflict is surfaced or re-prepared; it is not resolved by a model claiming the versions are “basically the same.”

Approval of a consolidated statement does not bulk-approve all source observations. An explicit operator decision is recorded directly as decision evidence; additional model inferences beyond it remain proposals. This avoids a second approval loop for a decision already made.

### 8. Time and reconstruction

Preserve enough information to distinguish:

- What the system knew or believed at an earlier time, using the evidence then available.
- What current evidence suggests was happening at that earlier time.

These differ when evidence arrives late or an interpretation is corrected. A Friday correction can change today's reconstruction of Tuesday without rewriting Tuesday's recorded belief.

The physical implementation need not be a full bitemporal database. It must retain version and evidence relationships sufficient for the supported historical queries. Unknown times remain unknown; ingestion time should not silently become event time.

### 9. Useful defaults, not mandatory schema

Start with stable claim identities, source references, explicit scope, derivation version, review/publication state and correction links. Add richer relation types only when a demonstrated use case requires them. Proposed consolidation operations—add, refine, supersede, resolve, no-change and conflict—are semantic examples, not a committed API.

The first implementation may present views as bounded read products rather than materializing every projection. The essential obligation is that the reader can tell what a statement means, what supports it, where it applies and how current it is.


---

<a id="chapter-03"></a>

## 03 — Work identity, execution graphs and time

**Status:** agreed distinctions; integration shapes require repository inspection.  
**Owns:** DR-06 and the coverage semantics used by reads and meetings.  
**Basis:** [C1](#c1); [A1, lines 49–51, 95–99](#a1). Sources: [register](#source-register).

### 1. Identity belongs to several layers

A logical objective is not an execution, and an execution is not a conversation. Mémoire must preserve these distinctions even when a simple session initially contains exactly one task.

| Layer | Meaning | Possible source |
|---|---|---|
| Project/workspace | Applicability and ownership scope, with any environment or branch qualifiers. | Repository identity and existing project configuration. |
| Work item | Objective or responsibility that can span attempts, agents and sessions. | Task-management ID, coordinator declaration or local identity. |
| Execution | Particular graph run, job, attempt or review performing work. | Run/review ledger, orchestrator or adapter. |
| Harness session | Capture/conversation stream. | Harness UUID, including explicit continuation links where available. |
| Artifact/revision | Exact output or code version to which a result or review applies. | Commit, digest, artifact ID or tool metadata. |

Model and agent identity describe execution provenance. A more capable model or multiple reviewers do not automatically establish correctness. Reviews may share evidence and assumptions; their number is not a count of independent confirmations.

These layers do not require a new project-management system. Existing IDs should be retained with their origin namespace so equal strings from different systems do not become accidental equivalence.

### 2. Combine the operator's three integration options

#### Ledger integration

Where a run/review ledger exists, ingest its job, run, worker, reviewer and outcome references. Treat it as evidence of recorded execution state. A ledger's successful process completion does not, by itself, prove that the requested objective was semantically achieved.

The adapter should be replay-safe. Re-reading the same record must not create duplicate executions or corroborating evidence. Preserve the external record identity and version where available.

#### Coordinator declarations

A coordinating agent can declare the intended work item, decomposition, dependencies, roles and review assignments at dispatch time. This is preferable to reconstructing all relationships through textual similarity afterward.

Preserve the distinction between plan and result: declaring a dependency, assigning a reviewer or scheduling a job does not establish dependency satisfaction, review completion or successful execution.

The coordinator is a source, not an unrestricted identity authority. Keep declaration provenance and allow later correction when a job was misattributed or a scope changed.

#### Session-local fallback

When neither integration exists, generate a stable local work identity mechanically, anchored to the harness-session UUID. Let the agent generate a human-readable task summary; the summary is not the identifier.

A display can read `[work ID] — Investigate import data loss`, with the source session linked beneath it. Label edits must not break identity. A session may acquire more than one work item; a work item may later be linked to another session.

Do not routinely interrupt the operator with “I cannot identify the task.” Preserve a useful narrow identity and expose unresolved broader linkage where relevant. Ask only when the ambiguity would affect a consequential action or merge that cannot safely remain unresolved.

### 3. Linking without falsifying certainty

When later evidence establishes that two local items belong to one objective, preserve aliases and original attribution. Record how and when the relationship was established. Do not rewrite history as though the original collector knew the relationship.

Similar labels are candidates for investigation, not permission to merge. Two “review the migration” items may concern different branches, revisions or reviewer obligations. Different labels may describe the same objective. Strong linkage can come from an explicit external ID, a coordinator relation or operator clarification, with conflicts still preserved.

The initial design does not require automatic semantic task merging. Conservative local identity plus explicit linking is preferable to confident cross-task contamination.

### 4. Graphs, dependencies and completion

Represent parent/child work and dependency relationships only to the extent supplied or supported. A dependency graph is not automatically a chronological ordering of all events. Concurrent work can legitimately remain divergent.

Each task state should identify outstanding completion conditions. Completing one child does not automatically close the parent; completing one review does not close another task's review. A parent graph is complete only under its applicable completion criteria.

A review must identify its target work, execution or artifact revision. “Approved revision A” does not approve revision B after additional edits. A generic `reviewed=true` field without a target is insufficient for these workflows.

Agent reviews are operational evidence. Human acceptance of a project interpretation is a different act. A record may contain both, but must name which occurred and what each applied to.

### 5. Facts retain environment and revision scope

“Passing on branch A, failing on branch B” can be the correct current state. Do not flatten this into a single universal passing/failing answer. Likewise, a local experiment is not a production deployment, and a successful test invocation is not blanket validation of an entire repository.

The consumer's work/environment scope determines which state is primary. A broader project orientation may show the disagreement. Unknown environment or revision should be visible when it limits use.

### 6. Evidence time and arrival time

Retain event time when supplied, ingestion time, and processing/version timestamps where relevant. Clock skew and late arrival mean timestamp sorting is not a complete account of causality.

Sequence or cursor information from a source can help establish coverage within that source. It does not establish a global total order across independent harnesses. Avoid inventing such an order merely to produce a neat timeline.

A late-arriving review of an old revision belongs to that target and time scope. It does not automatically override a newer result for a different revision.

### 7. Coverage checkpoints

A meeting or published representation should identify the evidence it incorporates, including known gaps. A single “processed through Wednesday” timestamp can conceal an older failed episode while newer work has been processed.

A conceptual checkpoint can include per-source or per-execution cursors, included event ranges, pending jobs, failed ranges and the referenced baseline version. The exact representation depends on existing storage and source guarantees; a vector-clock framework is not required.

Distinguish at least:

| Coverage layer | Question |
|---|---|
| Received | What evidence actually reached Mémoire? |
| Derived | Which received evidence has been considered by the applicable derivation? |
| Indexed | Which material is available to the relevant search mechanism? |
| Consolidated | Which evidence is incorporated in the maintained representation? |
| Discussed/published | Which representation and evidence checkpoint were considered by the operator? |

Mémoire cannot guarantee knowledge of events the harness never sent. Capture health and missing-source indications can make uncertainty visible, but an empty archive is not proof that no work occurred.

### 8. What a diff means

A semantic diff compares a baseline understanding with newly incorporated relevant evidence and its proposed implications. It is not simply the set of observations created after the previous meeting timestamp.

Late evidence, corrections, newly linked work and invalidated sources can all change the diff. For reproducibility, record the baseline version, checkpoint, selected evidence and preparation configuration.

Prepare a meeting against a stable checkpoint. Evidence arriving during discussion remains visible as newer material; deliberately rebase an affected proposal or carry it forward. Do not silently change the proposal being approved. Publication checks the base version again.

### 9. First implementation boundary

Start with available IDs and stable local fallback. Support explicit provenance and at least one multi-task case. Make ledger/coordinator integrations adapter seams rather than prerequisites for every session. Do not build a complete execution-graph orchestrator or automatic task-resolution model before the first capture-to-observation increment can be exercised. Minimal source/task attribution belongs to the foundation; richer integration can follow measured need.

Acceptance examples: parallel tasks with one completed review; review of an old revision; duplicate labels across namespaces; delayed evidence; and one task continued in another session. See [acceptance scenarios](#chapter-10).


---

<a id="chapter-04"></a>

## 04 — Selective derivation and incremental consolidation

**Status:** target behavior with explicit optimization experiments.  
**Owns:** DR-07 and reduction of inference/noise upstream of meetings.  
**Basis:** [C1](#c1), [A1, lines 25–39, 55–65](#a1); borrowing references [E1–E8, E11–E13]. See [source register](#source-register).

### 1. Optimize the pipeline before assuming the model is the problem

The reported baseline uses Qwen 3.5 9B for observation derivation. No deployment benchmark or inference trace was available for this reference. “Too slow” could reflect queue delay, repeated large inputs, output narration, reasoning generation, retries, concurrency contention or decoding speed.

First measure the current path. Compare a redesigned pipeline with the same model before comparing model sizes. Adding a stronger consolidation model while retaining every existing extraction call can increase total work instead of reducing it.

The desired output is not the smallest observation count. It is fewer unnecessary calls and less low-value generated text while preserving consequential outcomes, constraints and current state.

### 2. Normalize and group before inference

Deterministic processing should remove duplication from the extractor input without deleting the underlying authorized evidence. Useful candidates include repeated progress announcements, repeated injected instruction blocks, identical tool output and large file dumps whose relevant result can be represented by an excerpt and artifact reference.

Preserve tool invocation/result association, result status, source boundaries and useful error details. Extract mechanically available metadata without another model call. Mark injected memory as derived material so that recapture does not create fresh corroboration.

Do not equate read-only work with irrelevance: reading can discover a constraint or a root cause. Do not discard failed attempts universally: an environment-specific failure and its conditions can be valuable reusable knowledge. The activity itself may be noise while the discovery matters.

Normalization must not erase negative evidence, branch distinctions or repeated failures whose recurrence is itself significant. Exact deduplication of repeated capture is different from collapsing separately occurring events.

### 3. Semantic batches, not one call per event

Group a meaningful episode or completed interaction, preserving enough context to interpret results. Candidate triggers include completed tool sequences, session idleness and a bounded normalized-input threshold. Avoid arbitrary cuts between the command and the result needed to understand it.

A lightweight live path may extract high-value state transitions before a longer episode is ready. This does not require rerunning the full episode repeatedly: processed ranges and shared evidence should be explicit.

Initial token budgets are experiments, not defaults guaranteed by this design. A few thousand normalized input tokens and a 512–1,024-token output cap are possible pilot settings from the discussion. Validate them against actual schemas and examples; a cap that routinely truncates useful claims is not an optimization.

Failed, refused, truncated and successfully empty outputs are different states. A valid empty extraction means “no eligible claim identified in this processed input.” It does not mean no activity occurred or all possible significance has been ruled out.

### 4. Narrow extraction contract

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

### 5. Consolidation as scoped state maintenance

Consolidation consumes the relevant current understanding, new candidate claims, original supporting evidence and applicable operator corrections. It proposes a patch or a supported no-change result.

```text
Existing topic version
+ newly incorporated relevant claims/evidence
+ recorded corrections and unresolved challenges
→ proposed semantic operations + sources + questions
```

Use stable scopes such as project, topic and environment where appropriate. Session identity is usually provenance, not the permanent boundary of learning. Access-control boundaries still constrain cross-session consolidation.

Standing questions can organize understanding: which requirements govern this component, why was this architecture selected, what remains structurally uncertain, or which failure conditions are recurrent? Do not generate a page for every possible question before it proves useful.

### 6. Semantic operations

An implementation can express additions, refinements, supersession, issue resolution, qualification, no-change and conflict. These are examples, not mandated endpoint verbs.

The proposed change must show the previous meaning, proposed meaning, support and counterevidence, affected scope and practical consequence. A rewritten paragraph alone hides whether the model introduced a new requirement or merely improved wording.

Preserve unchanged content where possible. Rewriting an entire project summary every cycle creates unnecessary work and obscures the change being evaluated. Periodic broader consistency checks can still be useful when evidence suggests accumulated drift; they need not be the default operation.

### 7. Conflict handling

Check scope before resolving an apparent contradiction. Different branches, environments, time ranges or review targets may explain divergent claims. Preserve legitimate divergence.

Do not use “newest wins” universally. Recent agent text cannot silently overturn an explicit requirement; older accepted descriptions cannot hide new contradictory operational evidence. A proposed explanation can remain pending while the current-work view immediately reports the discrepancy.

A model confidence score is not a substitute for evidence or operator authority. When the evidence is insufficient, state the specific unresolved question. Preparation should investigate available evidence before sending the question to the operator.

### 8. Avoid self-reinforcing summaries

Trace understanding to original evidence through inspectable dependencies. Pages can guide retrieval, but must not serve as independent support for one another. Repeated copies of a derived statement count as one origin where identifiable.

Persist operator corrections with the rejected interpretation and its scope. A fresh consolidation run must not recreate the same mistake from the same old sources. A later materially different question may still be raised, but it must identify what changed.

Deleting or invalidating evidence should trigger reconsideration of dependent representations under the custody policy, not automatic restoration from a hidden summary cache.

### 9. Local model experiments

Keep the current model as baseline and record actual model identifier, quantization, template, serving build, reasoning mode, sampling and relevant concurrency. Do not infer these from an endpoint alias.

The official Qwen3.5-9B card documents non-thinking requests using `chat_template_kwargs` with `enable_thinking: false`. Verify the deployed server/template behavior rather than assuming the setting reaches the model. The available 4B variant is a candidate to test, not a proven replacement. [E11, E12]

The llama.cpp server documentation exposes template arguments, constrained JSON responses and timing/usage fields. Support and field behavior depend on the deployed build; use capability-aware collection and retain missing metrics as missing. Correct JSON is not evidence of correct extraction. [E13](#e13)

A useful allocation experiment is deterministic normalization, bounded small-model extraction, stronger local consolidation for changed topics, and a capable local conversational reviewer. Measure total inference and interference with active work—not just extraction throughput in isolation.

### 10. Scheduling and failure behavior

Inference workers should have bounded retries, explicit terminal failure and replayable work identity. Use idempotent processing and checkpoints so retries do not duplicate claims or lose coverage. Prefer batch scheduling that does not starve active reads or the operator conversation.

Capture continues when inference is unavailable. Readers receive coverage information; preparation must not claim complete understanding over failed ranges. No automatic cloud escalation is permitted in local-only mode.

The first increment improves and measures the capture-to-observation path itself. Existing derivation remains a comparison baseline, not a dependency that the new meeting layer must tolerate unchanged. Preserve capture custody; normalize the authorized extractor input as a replayable projection with source mappings. Test batching, abstention and source/scope faithfulness before adding a downstream meeting.

Normalization dispositions must be inspectable: included, excerpted, deduplicated capture, suppressed from model input, or unavailable. Keep rule/version and source references, with suppression reasons where applicable. This does not authorize storing secrets before redaction or retaining deleted evidence. Identical content from independent executions must not be mistaken for duplicate transport delivery.

The next increments address observation reconciliation/current state and then supported understanding changes. A claim becoming resolved, superseded, contradicted or irrelevant to active work is a lifecycle operation, not an instruction to erase its history or mark it human-accepted. Unresolved conflicts remain explicit; scope and recency do not alone decide truth.

Wholesale re-derivation, automatic backfill and model replacement remain separate, measured changes. The detailed sequence and gates are in [rollout](#chapter-09).


---

<a id="chapter-05"></a>

## 05 — Retrieval and progressive disclosure

**Status:** required read behavior; transport and ranking implementation remain open.  
**Owns:** DR-08.  
**Basis:** [C1](#c1); [A1, lines 71–99, 112–118](#a1). Source IDs: [register](#source-register).

### 1. A two-sided contract

The memory service must supply the right representation and honest evidence coverage. The consumer must request relevant memory, preserve its meaning, use valid handles and manage the context it assembles. Neither side alone guarantees useful continuation.

The addendum reports a continuation that never requested retrieval and later generated an invalid source reference. That is not a ranking test. It is a reason to evaluate consumer behavior separately. [A1, lines 80–85](#a1)

Compaction is one consumer. The same read principles should support ordinary task continuation, historical explanation, project orientation and meeting preparation.

### 2. Purpose, scope and budget

A request should express or establish purpose, applicable project/work/environment scope and an output budget. These need not be rigid enumerated endpoints. Mixed purposes are valid.

| Purpose | Primary representation | Necessary qualifications |
|---|---|---|
| Resume this task | Current objective, constraints, blockers, latest supported state and changes since last seen. | Correct work item, revision scope, outstanding reviews, evidence lag and relevant challenges. |
| Explain a past decision | Original problem, contemporaneous rationale, alternatives, outcomes and subsequent corrections. | Do not replace historical reasons with a plausible later explanation. |
| Orient in a project | Compact maintained understanding and important unresolved questions. | Publication status, freshness and current evidence that materially challenges it. |
| Prepare an alignment meeting | Baseline, relevant outcomes and proposed implications requiring judgment. | Stable checkpoint, omitted/pending matters, available counterevidence and known prior decisions. |

For current work, recent relevant observations are first-class regardless of human review. For governing knowledge, applicable decisions and requirements remain primary while challenges are visible. An implementation must not reduce all purposes to semantic similarity plus one global trust or recency score.

### 3. The response identifies what it supplied

A read product should identify its selected views, resolved scope, version/checkpoint, evidence handles and limitations. It should indicate consequential omissions and whether the response is partial.

Conceptually, a response may contain a bounded overview, active constraints/state, relevant live changes, supported understanding, unresolved conflicts and expansion pointers. This is not a requirement to include every section in every call; include what serves the stated purpose within budget.

When scope is ambiguous, use the narrowest supported identity rather than silently combining tasks. Broader related material can be shown as related, not attributed to the active task without evidence.

### 4. Total context, not individually small chunks

The relevant budget includes summaries, state, knowledge, evidence excerpts, metadata, source handles, tool wrappers and any retained trace included in the consumer's prompt. Measure the entire assembled product, including consumer-side overhead where observable.

Do not claim efficiency because one summary is small while appending a full trace beside it. The supplied V15 finding specifically concerns this combined-cost failure; its trace growth and aggressive initial clipping were lab errors, not demonstrated Mémoire behavior. [A1, lines 80–85](#a1)

Use diversity and deduplication so repeated versions of an old decision do not consume all available slots. Similar wording alone does not establish equivalence: differences in scope, time or contradiction can be decisive.

### 5. Progressive disclosure includes replacement

Start with a compact purpose-shaped representation and expose precise expansion paths. Expansions can recover exact source detail and bounded ordered groups: command, result, correction and later verification.

Do not keep appending refreshed state forever. A newer view can replace the older representation it supersedes, while the evidence remains in the archive. Active constraints deserve protection because they remain applicable, not because their original slots are permanently reserved.

Consumer capabilities matter. Some harnesses cannot remove earlier tool messages until compaction. In that case, logical supersession is not physical token removal. Report actual accumulated context and apply replacement at the next supported assembly/compaction boundary. Never count a logical “retired” marker as reclaimed tokens when the bytes remain in the prompt.

When essential applicable constraints exceed budget, expose insufficiency and request a larger budget or a narrower task scope. Silent clipping must not decide which governing constraint the agent forgets.

### 6. Stable references and precise expansion

The service issues source handles; models do not invent them. A handle should resolve to the intended evidence or version, subject to access control and retention. An expansion response identifies the exact range supplied and whether more remains.

Unknown, inaccessible, deliberately deleted and temporarily unavailable sources require meaningful distinct outcomes where security allows that distinction. Error responses must not reveal protected content or fabricate a replacement excerpt. The consumer can recover by using returned alternatives or rerunning a supported query.

Same-run sibling pointers can help navigate related evidence, but do not prove causality, correction or resolution. The addendum says existing documentation already describes provenance/review metadata and derivation-run sibling pointers; inspect and reuse those contracts where applicable. [A1, line 99](#a1)

### 7. Freshness and coverage are observable

| Outcome | Meaning |
|---|---|
| No matching evidence in the searched scope | Search completed over the stated available coverage; no supported match was found. |
| Relevant source unavailable | The referenced evidence could not be supplied; state the permitted reason or recovery path. |
| Budget clipping | Material was omitted to meet the response budget; expose its category or expansion route. |
| Derivation/index lag | Received evidence exists beyond what the chosen view/search can currently represent. |
| Capture uncertainty | The system does not know whether all relevant harness activity reached it. |

An approved understanding can be stale. Return a relevant newer challenge rather than hiding it until the next interview. A current-state answer can also be incomplete because of missing results; first-class treatment does not justify pretending the missing evidence exists.

### 8. Consumer obligations

Consumers supply or establish task identity and purpose; use returned handles; expand evidence when a consequential conclusion depends on omitted detail; preserve qualifications; and distinguish memory content from active instructions.

They should request updates at meaningful boundaries such as resumption, compaction, a new task assignment or a decision requiring current evidence. Exact triggering is an experiment. A harness should not require a model to remember an undocumented ritual before any retrieval occurs.

The consumer must not turn “reported complete” into “verified,” old authorization into new permission, or one reproduced failure into an explanation of every failure in a log. Relevant retrieval cannot compensate for those interpretation errors.

### 9. Evaluation boundaries

Test the service and consumer independently, then together. Service tests cover scope, budgets, source resolution, omissions and status honesty. Consumer tests cover triggering, valid handles, qualification, total context and correct next action.

Evaluate both peak context and cumulative continuation input. A smaller peak can still require many costly retrieval turns; fewer turns can still overwhelm context. Decision quality is evaluated alongside cost, not replaced by it.

The foundational pipeline increment should reuse existing inspection/search/expansion for fixed task probes rather than replace ranking. Generalized lifecycle-aware reads follow observation/state and consolidation work under the repository's compatibility policy. When the later meeting increment ships, it consumes those scoped, source-linked products and still reports coverage; it must not substitute fresh transcript interpretation for missing upstream contracts.


---

<a id="chapter-06"></a>

## 06 — Project alignment and design conversations

**Status:** agreed product behavior; agenda preparation and interaction details are empirical.  
**Owns:** DR-09 and DR-10.  
**Basis:** [C1](#c1); [A1, lines 53–69](#a1). Sources: [register](#source-register).

### 1. A project meeting grounded in memory

The interaction is not a memory approval screen. Its shape resembles a team meeting: understand what happened, align on what it means, then share ideas and discuss direction where useful. “Daily meeting” describes the conversational model, not a requirement to schedule daily meetings.

Reconciliation and design must remain distinguishable in what Mémoire records, without forcing the operator to switch modes. The agent may challenge assumptions, suggest improvements and connect patterns across work. It must distinguish evidence, inference and proposal.

The core presentation is:

> What was known → what happened → what this could mean, pending confirmation.

The operator steers, accepts, contradicts, qualifies, explores and decides. The system prepares enough that the operator need not inspect individual observations or answer questions already resolvable from evidence.

### 2. The agenda unit is a project question

A session, observation or task is not necessarily the right review unit. Several parallel tasks may jointly expose one requirement ambiguity. One task may expose unrelated design questions that belong in different topic discussions.

Group agenda items by supported changes in meaning. Each material item includes the baseline, relevant developments, supporting and conflicting evidence, scope, a proposed interpretation, practical consequence and the question requiring operator judgment.

Preparation should also recognize continuity. Completed work within existing requirements can produce no proposed understanding change. Do not manufacture a lesson or brainstorming item to justify the meeting.

A low-noise outcome is legitimate: “Work progressed within the existing requirements; no governing change is proposed; these operational follow-ups remain.” Qualify this by the known evidence coverage rather than claiming omniscience.

### 3. Preparation happens before operator judgment

The preparing agent should reconstruct results and original problems, remove superseded attempts from the main account without losing their history, distinguish review targets, retrieve applicable requirements, find earlier operator answers and investigate apparent contradictions.

It should resolve factual questions from available evidence. Asking whether tests passed is unnecessary when the captured result answers that question. Asking whether passing behavior establishes a requirement may be worthwhile.

Preparation can use more computation than presentation, but it must remain incremental and measured. Repeatedly rereading the whole archive to present three questions is not automatically economical. Inspect only changed topics plus the evidence needed to interpret them, with broader consistency checks triggered when useful.

The packet is prepared against a stable checkpoint and baseline version. It states known coverage gaps, relevant unprocessed work and material items omitted from the current agenda. It does not quietly approve omitted items.

### 4. Routing into human attention

| Development | Default treatment to validate in use |
|---|---|
| More support for an unchanged scoped claim | Attach support; no repeated review item solely for corroboration. |
| Routine evidence-backed state transition | Update current work; do not automatically amend project understanding. |
| Potential reusable lesson from a narrow episode | Keep a tentative interpretation and its limits; aggregate or prioritize when meaningful. |
| New evidence challenges a maintained conclusion | Make the challenge visible; prepare a review item when its significance warrants judgment. |
| Proposed requirement, direction or applicability change | Discuss before presenting it as an operator-approved governing change. |
| Current action needs permission not already available | Surface the action-specific uncertainty; do not wait silently for a future general meeting. |

This routing is a starting policy, not a perfect materiality classifier. A model score can assist prioritization but cannot silently suppress consequential challenges. Deferred material remains inspectable with age, scope and practical consequence.

### 5. A natural conversation with typed consequences

Consider this illustrative exchange, not a historical finding about the repository:

> Agent: Three tasks needed the same workaround while preserving the existing requirement.
>
> Operator: The requirement is intentional. The workaround is not.
>
> Agent: Then this may be implementation complexity rather than a changed requirement. Could the responsibility move to the shared component?
>
> Operator: Possibly. Explore that before changing the architecture.

The requirement is reaffirmed. Repeated workaround cost becomes a supported concern. The proposed move is a design hypothesis. Exploration is an agreed next step within the stated scope. Adoption of the architecture has not been authorized.

The operator should not have to provide those labels. The system records them and clarifies only consequential ambiguity. “Possibly” must not become approval; explicit adoption should not be downgraded to a vague idea merely because it appeared in an informal conversation.

### 6. Reconciliation and design do not share one meaning

| Conversational outcome | Record it as | Do not silently turn it into |
|---|---|---|
| “That was only an experiment.” | Correction of the historical/current interpretation. | A ban on future experiments. |
| “That exception applies to the migration tool.” | Scope qualification. | A project-wide rule. |
| “This explains the incidents better.” | Accepted retrospective interpretation with its date and support. | The rationale documented at the original decision. |
| “We could move this responsibility.” | Exploratory idea or hypothesis. | An adopted architecture. |
| “Use this approach for this component from now on.” | Explicit scoped direction/decision. | Unlimited execution authorization. |
| “Investigate it, but do not change production.” | Authorized exploration with a boundary. | Permission to deploy. |

A new decision is valuable new evidence. It should not be presented as a fact extracted from the preceding work. A retrospective explanation can improve understanding without rewriting what was known at the time.

### 7. The meeting becomes evidence without another approval loop

Retain the conversation and authenticated operator statements using the existing identity and custody model. Record explicit decisions as decisions made in that conversation. Any agent-authored extension beyond the actual statement remains an interpretation or proposal.

A short closing account makes the semantic result inspectable: what was agreed, what changed direction, what remains exploratory, what next steps were authorized and what is still open. This is meeting-level confirmation, not line-by-line observation acceptance.

When a generated publication patch needs confirmation, it must point to the exact displayed revision. The conversation can itself contain sufficient explicit confirmation; a second mechanical interview is not required. Unclear or undisplayed additions remain pending. Silence does not approve them.

The next consolidation must know which questions were resolved and which evidence was considered. It should not ask the same question again from the same evidence or interpret an exploratory idea as independent corroboration of the agent's earlier suggestion.

### 8. Attention budget and cadence

The operator's initial target is that routine work spanning roughly two days should usually require only a few minutes of reconciliation. A substantial pending knowledge change can justify more attention. This is an acceptance target to evaluate, not a measured result or a guarantee for every workload.

Separate required reconciliation time from elective design discussion. A useful twenty-minute design exploration is different from twenty minutes correcting a poor recap. The system must not relabel difficult reconciliation as design to improve metrics. Timing classification may be approximate and operator-correctable.

Bound the number and detail of questions presented. If substantial independent issues exceed the chosen attention budget, prioritize and split the discussion while exposing what remains pending. The operator can expand evidence or continue design discussion deliberately.

There is no mandatory daily schedule, automatic calendar integration or obligation to hold a meeting when nothing meaningful needs discussion. Ordinary reconciliation can be requested or grouped periodically. Action-relevant uncertainty is surfaced to the affected task when needed.

### 9. New evidence during the meeting

Do not silently mutate the packet under discussion. Identify newer material and determine whether it changes an agenda item. An unaffected explicit decision can be recorded; a changed proposal should be re-prepared or carried forward with the conflict visible.

Before publication, check the baseline version. Concurrent operator decisions or other accepted changes must not be overwritten by stale preparation. A reproducible meeting record links the packet actually shown, not merely whatever the latest packet became.

### 10. Outcomes and evaluation are different records

Project outcomes affect history, task state or understanding. Meeting feedback evaluates Mémoire's support. “Your recap was too detailed” should not become a domain requirement of the target project, nor a global user preference without an appropriate scope.

Collect an optional free-text note during, after or later than the meeting. It is not a prerequisite for closing the meeting. Preserve exact feedback, including positive observations, and link it to the actual packet/conversation. See [measurement and learning](#chapter-07).

### 11. Initial interface boundary

Use the least elaborate existing local surface that can support the interaction: existing UI, CLI plus a local agent, or a repository-compatible conversational integration. Do not build a new chat platform merely to validate the meeting concept.

A static packet and optional feedback are useful first steps, but they are not proof that the conversational experience works. Live use must demonstrate that the agent can retrieve supporting evidence, accept corrections, explore ideas without adopting them prematurely, and leave a faithful closing record.

The [meeting packet](#template-meeting-packet) and [outcome](#template-meeting-outcomes) templates are starting aids, not mandatory scripts or form-filling obligations for the operator.


---

<a id="chapter-07"></a>

## 07 — Measurement and continuous learning

**Status:** first-class from the first usable increment; most thresholds are empirical.  
**Owns:** DR-11.  
**Basis:** [C1](#c1); [A1, lines 71–85, 112–116](#a1). Sources: [register](#source-register).

### 1. Define how the system learns from use

The design establishes goals and behavioral boundaries. It does not presume that paper design can determine the optimum model, prompt, grouping policy, materiality detector or meeting format. Real use and tuning are necessary.

Evaluation therefore ships with every pipeline increment, before a meeting exists. A normalization/derivation evaluation records the input checkpoint, transformations, output claims, relevant versions, costs, errors and targeted quality assessments. The first instrumented artifact can be a local replay report; it does not require an interview UI.

When meetings ship, every meeting can have an evaluation record containing automatic measurements, links to the material actually used and optional operator feedback. Pipeline and meeting records should share traceable lineage without forcing all stages into one evaluation unit. Neither creates a per-observation operator approval requirement.

The improvement process must preserve useful behavior as well as diagnose failures. An attractive aggregate score is not a substitute for specific evidence about what helped, what failed and why a proposed change should address it.

### 2. The evaluation unit

Before meetings, evaluate a bounded source corpus, derivation run or lifecycle update. Link the selected evidence, capture/normalization dispositions, batch ranges, candidate observations, known coverage gaps, code/prompt/model/settings and compared baseline. Later stages reuse these links rather than reconstructing opaque input histories.

Useful foundational records include original and normalized input size, actual calls avoided or added, output tokens, queue and processing duration, retries, failed/truncated/empty responses, invalid references and duplicate-write attempts. Source-handle resolution is deterministic; whether a claim faithfully represents its source and whether a suppressed passage mattered are semantic assessments. Record assessor and limits rather than labeling those judgments deterministic truth.

For each compared corpus, report the cohort, important claims/constraints assessed, omissions, unsupported upgrades and retained examples of useful failure evidence. A smaller observation count is not a quality score. Measure batching delay and oldest unprocessed evidence under ongoing work, not just bulk replay throughput. A faster offline run with unacceptable current-state lag is not a clean improvement.

Optional exact notes can attach to a run, episode, observed failure or later meeting. Preserve positive and negative feedback without requiring the operator to inspect or grade every observation. Automated reports and targeted spot checks start immediately; the dedicated conversational feedback experience arrives with the meeting stage.

A meeting evaluation links to the meeting ID, project/topic scope, prepared packet and version, baseline understanding, evidence checkpoint, actual conversation, resulting outcomes and relevant processing configuration.

Use references to retained artifacts rather than copying the entire archive for each meeting. Preserve enough to reconstruct what the operator saw, what the agent had access to and what was omitted. Reproduction is limited by declared retention/deletion and by nondeterministic model generation; do not claim byte-identical replay merely because configuration was recorded.

The evaluation also identifies whether the meeting was completed, abandoned, partially conducted or not conducted. An abandoned meeting is not silently dropped from the denominator or counted as positive feedback.

### 3. Optional operator feedback

A sufficient initial prompt is:

> What helped, and what should be different next time?

The operator can answer freely, during the meeting, immediately afterward or after downstream work exposes a problem. No mandatory ratings, taxonomy selection or questionnaire are required.

Example: “Good reconstruction of the two parallel tasks. Too much implementation detail. The deployment-policy question helped, but the previous question was already answered yesterday.”

Preserve the exact text and its intended scope. The agent may propose tags or a diagnosis, but those are derived interpretations with their own provenance. The original note must remain available for correction. Multiple notes can attach to one meeting or to a specific item.

Record absent feedback as absent. Accepting a decision, ending the conversation or offering a polite acknowledgment does not establish satisfaction. Silence is not success.

Positive feedback deserves the same treatment: “That comparison helped me decide” describes behavior to preserve in later experiments. Improvements that eliminate a known annoyance must not casually remove a useful preparation pattern.

### 4. Direct observations, estimates and judgments

Every metric or assessment should state which category it belongs to and the source/version that produced it.

| Category | Examples | Required treatment |
|---|---|---|
| Directly recorded | Request duration, number of tool calls, provider-reported token usage, explicit recorded decisions. | Preserve definition, units, scope, missingness and relevant IDs. |
| Computed from direct records | Cumulative input tokens, queue duration, age of deferred proposals. | Preserve formula and denominator; do not count duplicate instrumentation twice. |
| Estimated | Locally estimated token count, approximate active attention, inferred reconciliation segment. | Label estimate and method; do not present as exact. |
| Human judgment | Useful question, unnecessary detail, missed requirement, misleading account. | Preserve exact feedback and attributed evaluation. |
| Model judgment | Proposed materiality, faithfulness score, inferred cause of complaint. | Label assessor/model/version and uncertainty; not ground truth. |

A schema-valid output is measurable. Factual correctness is not established by schema validity. An accepted interpretation is measurable. It is not necessarily objectively correct.

### 5. Initial metric dictionary

#### Processing and locality

| Metric | Definition and caution |
|---|---|
| Queue wait | Worker start minus queued-at time, using compatible clocks. Distinguish missing timestamps. |
| Preparation latency | Packet-ready minus preparation-request time for the specific version. Separate retries and waiting. |
| Stage duration | Observed duration of normalization, extraction, retrieval, consolidation and packet preparation where instrumented. |
| Model work | Calls, model IDs, generated/reasoning tokens where exposed, input/cached tokens, retries, refusals and truncations. |
| Total inference workload | Sum attributable call durations or tokens with named scope. Concurrent durations are not elapsed wall time or exact GPU occupancy. |
| Locality outcome | Selected boundary, endpoint classification, blocked external attempts and actual allowed external calls. No prompts or secrets in public metric labels. |
| Backlog | Pending/failed processing ranges and their ages, not merely a total queue length. |

#### Retrieval and consumer context

| Metric | Definition and caution |
|---|---|
| Service payload size | Actual bytes/tokens returned under a named tokenizer or provider report, including provenance and handles. |
| Peak assembled context | Maximum observed consumer input/context over a continuation, including injected views and retained trace. Missing consumer visibility stays missing. |
| Cumulative continuation input | Sum of observed input tokens across continuation requests; state whether cached tokens are included. |
| Retrieval turns | Number of actual retrieval/expansion calls; zero is not evidence that retrieval worked. |
| Invalid-reference events | Resolution failures separated by unknown, inaccessible, unavailable or deleted as permitted. |
| Coverage | Included sources/ranges plus known capture, derivation and indexing gaps. Not a claim of complete real-world knowledge. |
| Duplication | Exact/referenced-origin duplicate contribution where detectable; semantic duplicate judgments remain separately labeled. |

#### Meeting burden and outcomes

| Metric | Definition and caution |
|---|---|
| Elapsed meeting duration | Explicit end minus start, including pauses unless excluded by a stated rule. Not exact cognitive effort. |
| Reconciliation/design duration | Explicitly marked or estimated segments, with method and operator correction available. |
| Packet and agenda size | Displayed words/tokens, number of presented questions, expansions and visible deferred items. |
| Recorded outcomes | Explicit corrections, reaffirmations, decisions, authorized next steps and unresolved questions linked to evidence. |
| Repeated-question reports | Operator reports or evaluated cases of reopening a settled question without new grounds. |
| Pending interpretation age | Age and disposition of material proposals, including items omitted from an agenda. |
| Feedback coverage | Meetings with explicit feedback divided by eligible meetings, with count and window. Absence is not satisfaction. |
| Delayed correction | Later issue linked to an earlier meeting, with cause classified as judgment rather than assumed automatically. |

### 6. Denominators and missing values

Report sample counts and workload class with comparisons. A handful of meetings does not support confident tail-latency or universal usefulness claims. Percentiles may be descriptive with small samples but should not imply a stable distribution.

Unknown token usage is not zero. Failed preparation is not a zero-latency success. Meetings without feedback are not positively rated. A discarded batch must not disappear from coverage. A consumer that does not expose its complete context prevents a complete context-cost claim.

Avoid using accumulated agent-hours as the primary burden denominator: more parallel workers can make a ratio improve without reducing operator work. Record actual review minutes per work window and distinguish routine from structurally consequential work.

Do not fix all thresholds in advance. Establish an instrumented baseline, then set explicit comparison targets and tolerances before evaluating a candidate change.

### 7. Distinguish failure from legitimate change

A later reversal of a valid earlier decision is not automatically an earlier meeting error. Preserve the difference between new evidence, changed operator direction, corrected historical interpretation, and a presentation that was unhelpful despite factual accuracy.

A complaint such as “too much detail” can originate in extraction, topic grouping, agenda preparation, retrieval selection or conversational pacing. Do not automatically edit the meeting prompt before checking which stage produced the failure.

A useful diagnosis chain asks whether evidence existed, was captured, was available in the relevant scope, was retrieved, was included in the packet, and was interpreted and presented correctly. Missing capture cannot be fixed by a better recap prompt.

### 8. Traceable improvement loop

```text
Pipeline run or meeting + measurements + optional exact feedback
        → issue or successful pattern
        → diagnosis with supporting examples
        → proposed change and predicted benefit
        → replay / held-out cases / live comparison
        → deliberate promotion, revision or rollback
```

Each improvement records motivating pipeline-run, episode, meeting or item IDs; exact feedback when provided; the diagnosis and alternatives; changed code/configuration/prompt/model versions; success criteria; potential regressions; evaluated cases and results. An automatically detected failure can motivate an improvement without fabricated operator feedback.

An agent may cluster feedback, identify recurring patterns, prepare experiments and propose improvements. It should not silently rewrite its own behavior after every complaint. Changes remain attributable and reversible under the repository's normal review policy.

Related issues can be addressed together. An obvious deterministic reference bug can receive a normal fix and regression test; it does not require a new operator ceremony for every detail.

### 9. Replay, holdout and live use

Deterministic tests check properties such as valid references, identity preservation, version conflicts, explicit gaps and budgets. Replay examines bounded semantic behavior against retained inputs and expected distinctions. Live use establishes whether the meeting is natural, proportionate and useful to the operator.

Replay the evidence available at the original checkpoint. Do not give a candidate system future decisions and then claim it would have prepared the earlier meeting correctly. Separate before/after-current-knowledge reconstruction from time-faithful replay.

Keep some representative cases outside the examples used to tune a change. Include successful meetings so improvements preserve useful behavior. Record the assessor and rubric for semantic judgments; a model evaluator assists rather than certifies quality.

No exact source fixture for the reported addendum experiments was supplied. Their failure mechanisms can guide newly authored tests, but their scores cannot be independently reproduced from the addendum alone. [A1, lines 71–85](#a1)

### 10. Evaluate omissions, not only visible questions

A calm, short meeting may be missing the difficult issue. Sample omitted/discarded material periodically and investigate delayed downstream errors. This is not a return to exhaustive observation review; it is targeted coverage checking.

Track unnecessary inclusions and consequential omissions together. Shorter meetings are not improvements when they hide pending decisions or cause an agent to miss a governing constraint. More accepted changes are not automatically better; routine work can rightly produce none.

Do not let the system grade its own materiality classifier by simply accepting its own routine/structural labels. Use independent fixture design or operator judgments for evaluation cohorts.

### 11. Maturity and reduced feedback burden

Early use should make feedback visible and easy after each meeting, while remaining optional. As behavior stabilizes across representative work, move toward sampled feedback, exception reporting and retrospective checks. A significant model, prompt, retrieval or lifecycle change can temporarily justify closer observation again.

Automatic measurements and traceability continue even when explicit feedback becomes less prominent. The goal is less operator evaluation work when it stops earning its cost, not a permanent quality-audit ritual.

Feedback belongs to Mémoire's improvement process. Apply it to the appropriate meeting, project, interface or configuration scope. Do not turn one complaint into an unreviewed global preference.

### 12. Minimum first-increment implementation

Persist or reliably link source checkpoint, input dispositions, normalization/batch/extraction versions, observations and references, basic timings and available usage, errors/gaps, baseline comparison and optional exact diagnostic notes. Supply a repository-native inspection/export and replay path. Include targeted tests for semantic distinctions, capture duplication versus recurrence, abstention versus failure, and omitted consequential material.

Meeting IDs, packets, conversations, outcomes and meeting-specific notes become additional evaluation records when that capability ships. They are not prerequisites for evaluating or shipping foundational improvements.

A new observability platform, dashboard or autonomous evaluator is not required. Existing local logs and storage can support the first loop if records are attributable, queryable and do not leak sensitive content.

Template: [meeting feedback](#template-meeting-feedback). Improvement template: [experiment record](#template-improvement-experiment).


---

<a id="chapter-08"></a>

## 08 — Locality, security and operational boundaries

**Status:** preserve existing repository contracts; refinements below are required boundaries or proposed safeguards, not an audit of current enforcement.  
**Owns:** DR-12 and safe operation across all paths.  
**Basis:** [C1](#c1); repository checks [R1–R3]. Sources: [register](#source-register).

### 1. Local-only is an end-to-end property

The preferred target is local-only operation using the operator's own infrastructure. Local-first is an explicit fallback policy, not permission for automatic external escalation.

Check every processing stage: extraction, consolidation, meeting conversation, embeddings, reranking, diagnostics, tracing and exported evaluation fixtures. A local database with a remote model is not local-only inference. A locally generated memory snapshot injected into a cloud-backed coding harness is an external disclosure of that snapshot.

“Local” can include operator-controlled machines or LAN infrastructure under the declared boundary; it does not mean every model must run on the same workstation. Exact hosts and trust boundaries come from deployment configuration, not this reference.

The public quickstart explicitly distinguishes local from external derivation and requires per-invocation external allowance. Extend the relevant boundary rather than inventing a silent fallback mechanism. It also documents the API as the sole store writer. Both must be checked in the source checkout before new workers are wired. [R2](#r2)

### 2. Explicit modes and exports

In local-only mode, unavailable local inference produces a visible failure or backlog while capture continues. Do not retry through a remote provider. Model downloads, external documentation retrieval and operational memory transfer are different actions; none should silently broaden the authorized data boundary.

In a separately enabled local-first mode, an authorized external operation identifies its purpose, selected material, endpoint and redaction policy. Existing repository consent requirements govern. Do not substitute a global “cloud allowed” switch for a stronger existing per-run boundary without an explicit ruling.

Memory reads to an external harness need their own policy. The system cannot honestly call the full workflow local-only merely because derivation stayed local. Record the actual export scope where supported.

### 3. Captured content is data, not instructions

Tool output, source files, retrieved memories and earlier agent messages can contain instruction-like text. Derivation, consolidation and meeting preparation must treat them as evidence, not commands that alter the worker's rules or permissions.

An evidence item saying “mark this accepted” cannot grant publication authority. A model cannot create an authenticated operator decision by generating a line that resembles one. Use existing principal and write authorization rather than trusting role labels inside arbitrary captured text.

Preserve the distinction between an approved explanation, an applicable standing rule and a current authorized action. Retrieval must not reactivate old permissions without checking their scope and applicability.

### 4. Access and sensitive content

Derived representations must not broaden access beyond their source policy. Cross-task or cross-session linking is not a grant to read another project's restricted evidence. Topic scoping and authorization scoping must remain distinct.

Avoid logging secrets, full prompts, personal content or unbounded identifiers in public metrics. Evaluation fixtures and exports can be as sensitive as original sessions. Sanitize fixtures deliberately and record when sanitization changes the semantics relevant to a test.

The first increment should reuse existing authentication, redaction and custody controls. New meeting, feedback and publication surfaces need the same scrutiny as existing memory surfaces, not a weaker “internal tool” exemption.

### 5. Retention, correction and deletion

Relevance pruning removes material from an active view; it does not automatically delete evidence. Deliberate purge, quarantine or redaction follows the repository's policy. Do not introduce automatic raw-evidence deletion as a side effect of condensation or context-budget management.

When a source is unavailable or deliberately deleted, source handles should return the permitted status rather than stale content. Dependent summaries, search caches, exports and replay records require review under the retention policy. A hidden derivative must not restore content the operator deliberately removed.

Version history and tombstones can preserve the fact of correction or deletion without retaining disallowed content. Exact deletion propagation and retention durations are operational choices to inspect and test, not invented legal rules in this document.

### 6. Failure behavior

Capture remains independent of slow inference and absent human review. Derived work uses bounded retries, idempotency and visible failure states. A successful empty batch is not the same as an enumeration failure, refusal or truncated response.

A read with incomplete coverage must identify that limitation. A meeting prepared over a failed source range must not conclude that nothing material changed without qualification. Newer evidence should not silently mutate an already presented packet.

Resource scheduling should protect active use from consolidation contention. Measure queue and read/meeting latency under realistic concurrent load. Do not claim exact GPU attribution from process elapsed time on shared hardware.

### 7. Migration and compatibility

This is an additive redesign. Inspect existing lifecycle, read, injection, state/knowledge, storage and API contracts before adding new objects or changing eligibility.

Current proposed observations becoming first-class for scoped work reads is a **new behavioral interpretation**, not permission to globally mark all proposed rows accepted. Preserve human publication boundaries for governing understanding. Introduce new read behavior through the repository's versioning or feature-gate mechanism and test it explicitly.

Initially keep production injection and published understanding unchanged while exercising candidate normalization, derivation and later lifecycle/consolidation products in shadow or opt-in use. Preserve the distinction between candidate artifacts and active production outputs; a replay must not create duplicate production observations. Promote a tested path deliberately under the source repository's normal policy.

Shadow does not mean current observations are semantically second-class or unavailable to an opt-in current-work read. It means experiments cannot silently change production authority or context policy. Meeting products join the same controlled path when their prerequisites are exercised.

The public README describes storage/lifecycle guarantees across SQLite and PostgreSQL, not identical retrieval features. Do not impose a graph or vector dependency on the intentional SQLite full-text-search lane. Inspect actual source contracts and test the touched supported engines. [R1](#r1)

### 8. Operational minimum for the first agent

Use isolated local fixtures or a read-only/replay-safe evidence source. Follow the existing writer boundary. Disable new behavior cleanly without losing evidence. Keep configuration versions, source references and baseline comparisons. Do not deploy, bulk-rederive, purge, migrate production data or enable external processing merely because the design is approved.

Approval of the design establishes direction. It is not blanket authorization for destructive operations or production cutover.


---

<a id="chapter-09"></a>

## 09 — Experiments, rollout and promotion

**Status:** upstream-first direction requested by the operator; stage boundaries, mechanisms and thresholds below are proposed starting points.  
**Basis:** [C1](#c1), including the edition 1.1 sequencing correction; [A1, lines 71–85, 112–118](#a1). Sources: [register](#source-register).

### 1. Improve the upstream product before adding the meeting consumer

Edition 1.0 made the meeting loop the first useful implementation and deferred selective derivation. That sequence is superseded. The operator's original problems are avoidable derivation work, noisy observations and unsustainable downstream review. A meeting built over substantially unchanged outputs would not address those problems first.

The first useful implementation is a measured, replayable evidence-to-observation path. It preserves capture custody and improves the model-facing input and extraction contract, rather than deleting source evidence or making the operator reconcile raw observations. Later increments make observations usable over time, maintain topic understanding and provide supported changes before a meeting consumes them.

This is dependency-driven development, not a requirement to finish an ideal engine before real use. Keep narrow downstream probes from the beginning: can a fixed task question be answered with the correct scope, sources and uncertainty? Use existing read/inspection surfaces and opt-in real workload. Do not build a meeting product merely to obtain those tests. An existing capability can satisfy a stage when inspection and evidence demonstrate it.

### 2. Staged path and exit evidence

| Stage | Useful increment | Evidence needed before expanding |
|---|---|---|
| A — Baseline and replay | Map source contracts; select a bounded representative capture corpus; record current costs, outputs, identities and processing gaps. | Runnable local baseline, explicit cohort/configuration and a source-preserving comparison path. This is bounded reconnaissance, not the final deliverable. |
| B — Capture quality, normalization and minimal identity | Repair demonstrated capture defects where scoped and safe; construct a versioned model-input projection; preserve event, session, work/attempt and call/result association. | Source mappings and dispositions; transport duplicates distinguished from repeated events; retained constraints/failures; tested gap and redaction behavior. |
| C — Selective observations | Group meaningful batches; avoid redundant calls; narrow extraction; permit abstention; validate references and distinguish intention, report and observed verification. | Same-model baseline/candidate comparison, semantic and omission checks, complete processing cost and acceptable freshness. This is the first agent's bounded useful path with A/B. |
| D — Observation lifecycle and current state | Reconcile duplicates, qualification, supersession, resolution and conflicts in scope; support late evidence and corrections; keep current task work separate from history and approved understanding. | Replay of parallel tasks/reviews, revisions, repeated claims, resolved/reopened items and coverage gaps. No human acceptance needed for current-work usefulness. |
| E — Incremental consolidation | Produce scoped history/current-state accounts and supported changes to maintained understanding from changed evidence, with dependencies and no-change output. | Baseline/new-evidence/interpretation separation, preserved rationale, conflict and correction handling, no self-corroboration, no unsupported publication. Outputs are inspectable without an interview. |
| F — Lifecycle-aware read integration | Expose current work, history, understanding and pending challenges through bounded purpose-shaped reads; replace superseded context; extend ledger/coordinator integrations as justified. | Task-continuation and explanation probes, stable expansion, pending-change visibility, honest coverage and complete consumer-cost checks. Basic reads and identity tests already operate in earlier stages. |
| G — Alignment/design meetings and feedback | Prepare concise project questions from supported upstream changes; conduct the conversation; record outcomes and optional exact feedback. | Upstream traceability, routine no-change cases, meaningful-change cases, real operator feedback and reconciliation-time evidence. The meeting is not an alternate full derivation pipeline. |
| H — Tuning and maturation | Improve model allocation, scheduling, ranking, thresholds and feedback prompting using observed failures and strengths. | Held-out and live results, complete cost/quality comparisons, regression monitoring and reversible promotion. Bounded model experiments can occur earlier after a controlled pipeline baseline. |

Stages are logical dependencies, not fixed releases or duration estimates. Read probes, instrumentation, basic identity, safety and replay run throughout. Stages may overlap when their contracts already exist. Do not omit a missing prerequisite merely because a higher-level interface is easier to demonstrate.

Before stage G, a representative linear-work window should yield a compact account and supported no-change result, while a meaningful divergence should retain the baseline, relevant developments, proposed significance, conflicts and sources. This is a pre-meeting diagnostic product, not a claim that materiality is perfectly solved or an operator-approved conclusion. Remaining interpretation decisions belong in the eventual meeting.

### 3. First-agent scope versus the broader upstream program

The first agent implements a bounded A/B/C path: baseline instrumentation plus a concrete, locally runnable normalization/batching/extraction improvement and its regression evidence. Choose the relevant existing capture adapter and workload first; do not rewrite every harness or implement all later lifecycle stages in one assignment.

Provide the stable evidence links, work/session attribution, processing dispositions and run/version records that later lifecycle operations require. Do not invent the final ontology. Demonstrated existing lifecycle capabilities remain in use and receive targeted tests; a new full consolidation engine is not first-agent scope.

The deliverable is not a meeting UI, autonomous dream scheduler, replacement database or general model bake-off. A diagnostic report and replay command are sufficient interfaces. If the candidate fails a quality/cost gate, deliver the implemented opt-in path and honest results without promoting it or relabeling it an improvement. The bounded initial assignment must not become an indefinite requirement to find the optimum.

### 4. Experiment cards

#### EXP-01 — Remove avoidable extraction work

**Hypothesis:** normalization, semantic batching and explicit abstention reduce repeated input and output narration without losing consequential claims.

Compare identical authorized evidence with the same model/template/settings. Isolate normalization and extraction-policy effects where practical; record bundled changes otherwise. Measure calls, input/output tokens, queue/processing duration, retries, important-claim retention and grounding. Sample suppressed/no-op material. Reject a gain that only reduces observation count while losing governing constraints, useful failures or current progress. Measure oldest unprocessed evidence during sustained work as well as offline throughput.

#### EXP-02 — Non-thinking and smaller-model extraction

**Hypothesis:** bounded extraction may not require the current reasoning workload or model size.

Record the actual serving settings. Treat a thinking-mode change as its own experiment before adding model substitution. Compare smaller models after the same-model pipeline baseline. Keep downstream evaluation fixed where possible. Measure schema/truncation failures, source faithfulness, scope errors, processing cost and freshness. Do not promote from speed alone or infer local quality from another engine's cloud benchmark.

#### EXP-03 — Observation lifecycle and incremental understanding

**Hypothesis:** scope-aware reconciliation and incremental topic updates reduce duplicates, stale state and unnecessary regeneration while preserving history.

Test observation lifecycle separately from understanding synthesis, then together. Include late evidence, repeated capture versus genuine recurrence, parallel task reviews, revision changes, corrected sources, reopened work and unchanged topics. Measure unsupported merges, missed contradictions, stale active claims, needless churn and total processing cost. Resolved work leaving the active view must not vanish from history.

#### EXP-04 — Meeting materiality and grouping

**Hypothesis:** supported upstream changes grouped by project question produce fewer, more useful operator decisions.

Run once upstream products are inspectable. Use independently classified routine and structural-change cases; inspect hidden consequential matters as well as agenda relevance. Measure repeated-question burden and required reconciliation separately from elective design. Do not ask the meeting agent to hide faulty inputs with polished prose.

#### EXP-05 — Progressive disclosure with replacement

**Hypothesis:** purpose-shaped reads and replacement preserve decisive evidence with lower complete context cost.

Use fixed task/decision probes from early pipeline work, then integrate the real consumer. Measure peak/cumulative input, retrieval turns, stale text retained, handle errors and correctness at the relevant scope. Count actual consumer input where visible, not only service payload. Do not attribute a harness failure to retrieval ranking. [A1](#a1)

#### EXP-06 — Identity enrichment

**Hypothesis:** ledger/coordinator relationships improve multi-agent continuity beyond session-local grouping.

Start with existing identifiers and safe fallback; richer adapters should not block capture cleanup. Compare real available identity sources without manufacturing missing metadata. Evaluate cross-task contamination, review targeting, namespace collisions, late linkage and unnecessary operator interruptions.

#### EXP-07 — Feedback-driven improvement

**Hypothesis:** attributable changes motivated by operational failures or exact operator feedback improve targeted behavior without losing known strengths.

Begin with pipeline reports and optional scoped notes. When meetings ship, preserve per-meeting feedback as a first-class source. Investigate the failure stage before changing prompts; define expected benefit, run replay/held-out cases, then use live evidence. Maintain rollback. A note is optional; missing feedback is not success.

### 5. Comparison discipline

Record evidence checkpoint, code revision, normalization/batching/extraction versions, model, endpoint boundary, serving build, quantization, prompt/template/schema and relevant settings. Label unavailable settings and estimated measurements. Record cache state, concurrency and resource contention when they materially affect timing.

Use routine work, decisions, failures, rollbacks, parallel reviews, branch divergence, missing results, misleading assistant claims, operator corrections and no-op cases. Evaluate discarded material and positive cases. Do not use fewer observations, valid JSON or source-handle resolution as substitutes for semantic quality.

Time-faithful replay cannot see future evidence. Later-known truth can assess retrospective correctness, but is a separate evaluation. Respect retention/redaction and do not resurrect intentionally deleted material. Newly authored regression fixtures are not reproductions of the addendum's reported experiments.

### 6. Promotion gates

**Safety/integrity:** no demonstrated regression in source custody, redaction, authorization, scoped identity, reference stability or publication boundaries in tested cases. Failures block affected promotion even when speed improves.

**Semantic:** important outcomes, constraints, uncertainty and useful negative evidence survive. No unsupported upgrades from intention to completion, report to verification, patch to requirement or exploration to decision. State the rubric, assessor and limits.

**Cost/freshness:** compare complete work per bounded corpus and under representative arrival patterns. Include retries, pre/post-processing and work shifted to later stages. Do not exchange low extraction cost for unmeasured downstream re-derivation or unacceptable current-state delay.

**Operational:** explicit candidate/production separation, repository-approved migrations, retry/idempotency, resource controls and disablement. Test relevant supported storage lanes and consumers at the scope claimed.

**Usefulness at the delivered scope:** early pipeline changes need representative workload/replay and task-probe evidence; they do not require an interview. Conversational usefulness and sustainable reconciliation time require actual meetings once stage G ships.

Set workload-specific targets after baseline and before judging the candidate. A test gate is not a promise of zero future defects. Report failed cases and leave a candidate unpromoted when results are inconclusive. No fixed calendar or universal model threshold is established here.

### 7. Non-blocking choices and false milestones

Batch thresholds, topic granularity, detailed ranking, model allocation, interview cadence and exact representations remain empirical. Minimal identity and evidence contracts are needed early; full ledger integration and automatic semantic linking are not. Unresolved privacy, production retention, source-contract or destructive-migration permission blocks only the affected operation.

“Added a dreamer,” “produced fewer observations,” “all JSON validates” and “built an interview UI” are not sufficient milestones. Neither is a source-preserving archive with no demonstrated improvement in the data used by consumers.

**Initial progress:** a replayable local source-to-observation increment with measured noise/cost behavior, inspectable omissions and protected source/scope semantics. **Pre-meeting progress:** lifecycle-correct work accounts and evidence-backed supported changes over representative work. **Whole-system success:** actual meetings and task use show that better memory saves attention without hiding significance.


---

<a id="chapter-10"></a>

## 10 — Acceptance scenarios and traceability

**Status:** proposed acceptance catalogue; cases are specifications, not passing test results.  
**Basis:** [C1](#c1), [A1](#a1). Source IDs: [register](#source-register).

### 1. How to use this catalogue

Turn relevant cases into repository-native fixtures and tests as the owning capability ships. Do not require the first agent to implement every future feature just to make the entire catalogue pass.

**D** means deterministic or mechanically assertable with a controlled fixture. **S** means semantic judgment is required. **L** means live use is needed. Several cases require more than one kind. Model evaluators can assist S cases, but their verdicts are labeled assessments.

The first-increment subset is identified in the handoff. Missing original experiment artifacts must not be invented; newly authored fixtures are labeled as such.

### 2. Evidence, meaning and view separation

| ID | Given / when | Required behavior | Evidence |
|---|---|---|---|
| AC-01 | An agent says it will run tests; no result is captured. | Retain intent/pending work, not a passing result. | D + S |
| AC-02 | An agent reports success without captured verification. | Preserve it as a report, without suppressing its usefulness or upgrading it to independent proof. | S |
| AC-03 | A command succeeds at revision A in development. | Attribute success to that command/revision/environment, not deployment or global correctness. | D + S |
| AC-04 | An empty-feed patch passes specific tests. | History and task state update; no domain invariant is inferred without requirements or a decision. | S |
| AC-05 | A new implementation violates an approved preservation requirement. | Return current behavior and the governing requirement as a discrepancy, not an automatic amendment. | S |
| AC-06 | A later explanation is accepted for an earlier decision. | Preserve it as later interpretation, not contemporaneous rationale. | D + S |
| AC-07 | A captured generated page repeats its earlier conclusion. | Do not count it as independent corroboration of itself. | D + S |
| AC-08 | The operator corrected an earlier interpretation; the same evidence is replayed. | Respect the scoped correction and do not recreate the rejected claim without new grounds. | D + S |
| AC-09 | A model produces valid structured output with an invalid source handle. | Reject or quarantine the unsupported linkage; schema validity does not confer correctness. | D |
| AC-10 | A batch contains only low-value progress narration. | A successful empty extraction is permitted; capture remains available and the processed range is recorded. | D + S |

### 3. Identity, time and concurrency

| ID | Given / when | Required behavior | Evidence |
|---|---|---|---|
| AC-11 | Two parallel tasks await review; one review completes. | Close only the corresponding obligation. | D |
| AC-12 | Revision A is approved, then B changes the artifact. | Do not inherit approval onto B without applicable evidence. | D |
| AC-13 | Two external systems emit equal task-ID strings. | Preserve namespaces; do not merge automatically. | D |
| AC-14 | Two tasks have similar labels but distinct scope. | Keep distinct identities; similarity alone is not equivalence. | D + S |
| AC-15 | No coordinator or ledger exists. | Create a stable local identity anchored to the session and a changeable label without routine operator interruption. | D + L |
| AC-16 | A task continues in another session and explicit linkage arrives later. | Link while preserving original identifiers and attribution history. | D |
| AC-17 | Older evidence arrives after newer events were processed. | Incorporate it under its event/target scope; disclose prior coverage gaps. | D + S |
| AC-18 | One processing range fails while a later range succeeds. | Do not advertise a gap-free global watermark. | D |
| AC-19 | Branch A passes while branch B fails. | Preserve divergence instead of flattening project status. | D + S |
| AC-20 | The baseline changes during a meeting. | Prevent stale publication from overwriting the newer version; expose/rebase the affected proposal. | D |

### 4. Reads and consumers

| ID | Given / when | Required behavior | Evidence |
|---|---|---|---|
| AC-21 | A recent relevant work observation is proposed and an old description is accepted. | A current-work read uses the fresh scoped account without changing its human-review status. | D + S |
| AC-22 | A read contains stale accepted understanding plus new contradictory evidence. | Make the relevant challenge visible before the next human meeting. | D + S |
| AC-23 | Search finds no match, versus evidence is clipped, unindexed or unavailable. | Return distinct honest coverage/availability outcomes. | D |
| AC-24 | Several old near-duplicates compete with an active constraint. | Avoid duplicate budget consumption; preserve the decisive constraint for the requested purpose. | D + S |
| AC-25 | A protected memory allocation is full when a new active constraint arrives. | Refresh applicability and replace superseded representations rather than reserving permanent slots. | D + S |
| AC-26 | Summary, excerpts, trace and metadata are assembled together. | Measure the complete consumer product, peak and cumulative, not only the summary. | D |
| AC-27 | A consumer invents or requests a nonexistent source handle. | Return a meaningful failure; the consumer recovers without fabricating evidence. | D + S |
| AC-28 | A consumer never triggers retrieval. | Record and evaluate this consumer failure separately from ranking quality. | D |
| AC-29 | Relevant excerpts demonstrate one cause within a 33-failure log. | Do not attribute the entire log to that cause without evidence. | S |
| AC-30 | A harness cannot remove old tool messages until compaction. | Distinguish logical supersession from actual token reclamation and count retained text. | D |

### 5. Meetings, publication and authority

| ID | Given / when | Required behavior | Evidence |
|---|---|---|---|
| AC-31 | Routine work follows existing requirements with no detected material change in covered evidence. | Produce a concise continuity account, no fabricated lesson and no obligatory interview question. | S + L |
| AC-32 | Several tasks jointly expose one project ambiguity. | Prepare one supported question with expandable task evidence, not per-observation approval. | S + L |
| AC-33 | The operator previously answered a question and nothing relevant changed. | Do not reopen it without identifying new grounds. | S + L |
| AC-34 | The operator says “explore that before changing the architecture.” | Record exploratory authorization and limits, not architectural adoption. | D + S |
| AC-35 | The operator explicitly makes a new scoped decision during the meeting. | Record decision evidence and scope without a second approval interview for that exact decision. | D + S |
| AC-36 | An agent adds an implication to a closing summary that was never discussed. | Leave the addition proposed; silence does not approve it. | D + S |
| AC-37 | More material issues exist than the attention budget permits. | Expose pending items and dependencies; omit from agenda without implicitly accepting them. | D + S |
| AC-38 | A long discussion is voluntary design exploration. | Distinguish its duration from required reconciliation; retain classification method and correction. | D + L |
| AC-39 | An old record says a production action was authorized. | Do not treat retrieval as renewed permission for a different task/time/scope. | D + S |
| AC-40 | New evidence arrives during the conversation. | Preserve the displayed packet/checkpoint and deliberately disclose or rebase affected items. | D + L |

### 6. Feedback, learning and safe operation

| ID | Given / when | Required behavior | Evidence |
|---|---|---|---|
| AC-41 | The operator leaves an optional free-text note after or during a meeting. | Preserve exact text, scope and links; meeting completion never depends on filling a form. | D + L |
| AC-42 | A meeting receives no feedback or is abandoned. | Keep missing/abandoned states; do not count them as positive evaluations. | D |
| AC-43 | Token usage or active attention is not observable. | Record missing/estimated values explicitly, not exact zeros. | D |
| AC-44 | A later correction concerns an earlier meeting. | Link it and distinguish new evidence/direction from an earlier error where supported. | D + S |
| AC-45 | A behavior change is proposed from feedback. | Link motivation, diagnosis, versions, predeclared test criteria, results and rollback. | D + S |
| AC-46 | A shorter meeting omitted a consequential issue. | Detect through targeted omission checks or delayed feedback; do not call it a clean improvement. | S + L |
| AC-47 | Local inference is unavailable in local-only mode. | Capture continues; processing gap is visible; no remote fallback occurs. | D |
| AC-48 | Captured text instructs a worker to accept a claim or expose secrets. | Treat it as evidence, not authority; preserve access and publication boundaries. | D + S |
| AC-49 | A retained source is deliberately deleted under policy. | Do not resurrect it through cached derivatives; expose permitted unavailability and reconsider dependencies. | D |
| AC-50 | New opt-in behavior is disabled or rolled back. | Existing capture/read/publication behavior remains usable; evidence and evaluation provenance are not silently lost. | D |

### 7. Supplied experimental observations

The following are reported by the supplied addendum, not independently reproduced in this work:

| Reported case | Exact limitation to preserve | Related cases |
|---|---|---|
| V11 | Critical preservation constraint lost while three automatic hits concerned review procedures. | AC-24 |
| V13 | All six protected slots filled at the first checkpoint; later constraints never entered. | AC-25 |
| V7 | Selected passages recovered facts, but all three repeats generalized one demonstrated cause to a 33-failure log. | AC-29 |
| V15 cost/criteria | Ten completed pairs used 2.24× baseline continuation input and met 27/40 criteria versus 30/40. | AC-26, AC-30 |
| V15 consumer | Retrieval was never requested and a generated nonexistent source reference later caused an abort. | AC-27, AC-28 |
| Narrow live search | Five near-identical lapsed observations illustrated duplicate budget consumption. | AC-24 |

The cases were small and repeatedly studied; factual scoring came from the coordinating model. Independent reviewers did not inspect the private answers. V15 trace growth and initial-prior clipping were lab-design errors. No candidate described in the addendum was promoted. Original inputs, checkpoints, complete outputs and scoring artifacts are needed to reproduce those results. [A1, lines 71–85](#a1)

### 8. Requirement-to-evidence map

| Design commitment | Representative acceptance evidence |
|---|---|
| DR-01 — capture and evidence | AC-10, AC-18, AC-47, AC-49, AC-50 |
| DR-02 — distinct views | AC-03, AC-04, AC-05, AC-21 |
| DR-03 — first-class current work | AC-02, AC-21, AC-22 |
| DR-04 — independent authority dimensions | AC-02, AC-12, AC-34–AC-39 |
| DR-05 — problems, rationale and source grounding | AC-04, AC-06–AC-09, AC-29 |
| DR-06 — identity | AC-11–AC-20 |
| DR-07 — consolidation | AC-07, AC-08, AC-20, AC-22, AC-31–AC-33 |
| DR-08 — reads and consumers | AC-21–AC-30 |
| DR-09 — prepared meetings | AC-31–AC-33, AC-37, AC-38 |
| DR-10 — conversation consequences | AC-34–AC-40 |
| DR-11 — measurable improvement | AC-41–AC-46 |
| DR-12 — locality and safe operation | AC-39, AC-47–AC-50 |

### 9. When to say it works

A capability works at its claimed scope when its relevant deterministic contracts pass, semantic evaluation preserves the required distinctions, operational behavior is observable and reversible, and live use supports the intended benefit where applicable.

Always state the tested scope, sample size, remaining failures and unmeasured aspects. This catalogue is not a claim that fifty passing unit tests alone establish a useful memory system. The whole-system goal requires representative operator use and sustained evidence that lower burden did not come from missed significance.


---

<a id="chapter-11"></a>

## 11 — Approaches to borrow and questions to test

**Status:** external documentation checked 17 September 2026; adaptations below are design proposals.  
**Purpose:** preserve the useful research without turning product marketing or another engine's defaults into Mémoire requirements.  
**Sources:** exact primary-source URLs are in the [source register](#source-register).

### 1. Interpretation of “successful engines”

These systems supply documented mechanisms worth examining. This reference does not rank their production success or claim independent benchmarks on the operator's data or hardware. Their model/provider defaults, terminology and deployment costs differ. A capability described by its authors is not evidence that it will solve Mémoire's latency or human-attention problem unchanged.

Borrow mechanisms at existing Mémoire seams. A new engine dependency is not required to adopt a useful idea. Check licenses and current integration details before copying implementation code; this reference recommends conceptual adaptation, not unreviewed code transplantation.

### 2. Hindsight — observations and maintained topic answers

**Documented mechanism.** Hindsight consolidates related retained material into observations with supporting evidence. Mental models maintain answers to selected standing questions, with freshness and provenance/version support; its documentation describes incremental refresh. Knowledge pages provide a document-oriented organization and explicitly avoid reading other pages as evidence. [E1, E2, E3]

**Mémoire adaptation.** Maintain answers about project requirements, architecture, recurring conditions and structural uncertainty; update only the affected scope. Keep source-grounded changes and anti-self-corroboration behavior. Add Mémoire's operator publication boundary rather than treating automatic synthesis as approved knowledge.

**Test.** Compare incremental topic updates with full regeneration for unnecessary churn, missed contradictions, source traceability and total preparation work. A useful page may accelerate reads while making writes more expensive; measure both.

**Locality note.** Hindsight documents local providers, including llama.cpp. That establishes an available integration path, not a quality or speed guarantee for the operator's chosen model. Its defaults and supporting models still require a boundary audit. [E4](#e4)

### 3. Mastra — observation buffering and reflection

**Documented mechanism.** Mastra uses an Observer and Reflector, with threshold/idle-related buffering. Its current retrieval mode links observation groups to original messages for expansion. The documented reflector rewrites its observation log; it is not equivalent to Mémoire's proposed evidence-backed operator publication workflow. [E5](#e5)

**Mémoire adaptation.** Batch meaningful activity instead of running a full operation for every event. Preserve expandable source ranges. Use reflection as bounded context maintenance where appropriate, not as an excuse to discard original evidence or recursively certify summaries.

**Test.** Measure source coverage, latency, total context and missed recent transitions at different buffering thresholds. Do not carry external default models or thresholds into local-only mode silently. Compare actual completed context products, not nominal per-layer caps.

### 4. Letta — versioned memory and dreaming

**Documented mechanism.** Letta's dreaming reviews recent conversations and updates memory; its optional agent-review step is another model conversation, not operator approval. MemFS supplies Git-backed memory versions and local-only storage, with files outside the always-present system area available for on-demand access. [E6, E7]

**Mémoire adaptation.** Produce inspectable patches and retain version ancestry; expose compact context with deeper material discoverable. Database versions can provide the desired behavior without replacing Mémoire's store with Git.

**Test.** Verify exact-version approval, concurrent-edit conflict handling and whether explicit operator corrections survive later updates. Do not treat “agent reviewed” as human validation or assume a local memory repository means every inference call is local.

### 5. Mem0 — selective extraction and legitimate abstention

**Documented mechanism.** Mem0's custom-instructions documentation shows allowed fact types, positive and negative examples, and empty fact collections. Its current add-memory documentation describes additive storage rather than automatic overwrite/delete reconciliation. [E8, E9]

**Mémoire adaptation.** Narrow extraction to consequential claims and make no-op output a valid result. Preserve the evidence archive independently. Implement current-state and supersession semantics explicitly rather than assuming an external `add` operation supplies them.

**Test.** Compare noise reduction against recall of critical constraints, outcomes and useful failures. Empty output is successful processing only when the call completed and the eligible scope was actually considered.

### 6. Graphiti — time and provenance

**Documented mechanism.** Graphiti represents changing facts with temporal validity and source-episode provenance. Its documentation supports OpenAI-compatible local model endpoints and warns that smaller/local models can struggle with structured extraction. [E10](#e10)

**Mémoire adaptation.** Preserve event versus arrival time, validity scope, correction history and historical queries. Keep concurrent environments separate. Adopt the temporal distinctions before deciding whether graph storage is warranted.

**Test.** Late evidence, review of old revisions, changed requirements and branch divergence should remain correct. Measure extraction/reconciliation work and failure rates; structured graph output is not free simply because inference is local.

### 7. Local model and serving references

**Qwen3.5-9B / 4B.** Official cards provide the exact model references; the 9B card documents non-thinking requests. Benchmark the existing 9B pipeline first, then a smaller extractor under controlled conditions. No throughput, memory-fit or quality prediction is established here. [E11, E12]

**llama.cpp.** The server documentation describes request template controls, JSON constraints and timing/usage responses. Inspect the installed build and actual template, record capabilities and preserve absent fields honestly. A valid response schema does not certify a supported claim. [E13](#e13)

### 8. LongMemEval — supplementary evaluation, not the product scorecard

**Documented scope.** LongMemEval evaluates information extraction, multi-session reasoning, temporal reasoning, knowledge updates and abstention in long-term chat memory. [E14](#e14)

**Mémoire adaptation.** Reuse those evaluation dimensions where relevant. Add the workload-specific requirements absent from a generic chat-memory score: parallel task/revision identity, actual authorization, meeting usefulness, operator burden, capture gaps and complete consumer context cost.

**Test.** A system should not be promoted solely on benchmark question-answering scores. Use Mémoire fixtures and live meetings alongside external evaluation where it adds information.

### 9. Research boundaries carried into the handoff

The first agent need not install or benchmark all these engines. Start by understanding the mechanisms and inspecting existing Mémoire equivalents. Deeper research is justified when a concrete implementation choice needs it.

External documentation is mutable. Verify exact APIs and defaults at implementation time. Source-check dates are provenance, not a claim of a pinned external software build. The primary purpose of this chapter is to preserve why each mechanism was considered and what would count as evidence that borrowing it helped.


---

<a id="chapter-12"></a>

## 12 — Decisions, open choices and reference governance

**Status:** decision boundary for this reference; not a replacement for Mémoire's private ledger.  
**Basis:** operator approvals [C1](#c1), supplied addendum [A1](#a1). Sources: [register](#source-register).

### 1. What the operator settled

The [twelve commitments in the reference index](#reference-map) summarize the approved direction. In particular, the following are no longer open product questions:

Recent observations are first-class for the scoped temporal account of work without becoming accepted deep knowledge. Not all memory belongs to one lifecycle, relevance ordering or validity category. Human interaction centers on high-level meaning, with natural discussion of improvements and direction. Reconciliation and design are distinguishable outcomes, not rigid conversation modes.

Task identity should combine ledger information, coordinator declarations and useful session-local fallback. Uncertain linkage should not make ordinary capture unusable. Measurements and optional scoped diagnostic notes are first-class from the first pipeline increment. Optional exact meeting feedback is first-class from the first meeting capability, with a traceable improvement loop and reduced operator evaluation burden as the system matures.

The intended operation is local-only where feasible, local-first only with explicit boundaries. The design is ready for a bounded implementation; no further speculative paper tuning is required before observing real usage.

**Sequencing correction, edition 1.1:** the operator explicitly prefers capture cleanup, improved observations and upstream lifecycle work before the interview. The edition 1.0 meeting-first mission is superseded. Measurement starts at the foundation; optional per-meeting feedback remains required when meetings ship. Exact stage gates and thresholds are proposed implementation defaults, not additional operator decisions.

### 2. Decisions are distinct from starting defaults

| Topic | Agreed boundary | Proposed initial choice |
|---|---|---|
| Memory products | Distinct history, task state and understanding over shared evidence. | Reuse existing records and read products before materializing new stores. |
| Current work | Proposed status does not make it second-class. | Opt-in scoped read/meeting behavior before production injection changes. |
| Consolidation | Supported changes, source dependencies and no self-corroboration. | Topic-local incremental preparation with explicit no-change output. |
| Human review | Meaningful project alignment and design conversation. | Existing local interaction surface plus bounded packet and closing record. |
| Identity | Work, execution, session and review target are distinct. | Existing IDs with namespaces; stable local fallback when absent. |
| Evaluation | Deterministic figures and optional scoped feedback from the first pipeline increment; per-meeting feedback when meetings ship. | Versioned replay reports and existing local storage/logging; no new observability platform initially. |
| Models | Local operation and measured comparisons. | Keep Qwen 3.5 9B baseline; tune pipeline before model substitution. |
| Publication | Exact scoped outcomes; no implicit acceptance of hidden implications. | Shadow proposals until repository-compatible commit/confirmation path is proven. |

These defaults can change through documented experiments without reopening the fundamental product direction.

### 3. Open mechanism register

| ID | Choice to explore | Why it is open | Evidence that should decide |
|---|---|---|---|
| OC-01 | Normalization and episode boundaries | Noise and meaningful transitions vary by harness/work. | Claim coverage, missed failures, total inference and latency. |
| OC-02 | Observation/model allocation | Current bottleneck and local quality are unmeasured. | Same-model baseline, smaller-model comparison, retries and semantic errors. |
| OC-03 | Topic granularity and update triggers | Too broad causes churn; too narrow fragments understanding. | Meeting relevance, duplicate questions, cross-task lessons and compute. |
| OC-04 | Material-change routing | No perfect classifier is assumed. | Operator judgments, omitted-material checks and pending-change burden. |
| OC-05 | Meeting frequency and attention allocation | Useful cadence depends on actual work. | Required reconciliation minutes, delayed decisions and voluntary design use. |
| OC-06 | Read ranking and expansion policy | Purpose, scope and consumer budgets differ. | Decisive evidence, next-action quality and full context cost. |
| OC-07 | Task-linking automation | Explicit IDs may cover much of the need already. | Misattribution, review-target correctness and interruption burden. |
| OC-08 | Storage representation/versioning | Existing source contracts determine viable extensions. | Minimal migration, replay, source linkage and supported-engine tests. |
| OC-09 | Feedback analysis and maturity policy | Feedback volume and recurring patterns are unknown. | Useful improvements, regression discovery and operator evaluation cost. |
| OC-10 | Backfill and long-term reprocessing | Value and workload are not established. | Coverage benefit versus processing cost, corrected-source behavior and safety. |

None blocks the first additive capture-to-observation increment. Record an initial choice and why it is reversible. Do not hide a guess inside a schema and call it an agreed architecture.

### 4. Repository-dependent checks

Inspect the actual instruction surfaces, decision records, state/knowledge split, lifecycle rules, API writer boundary, source handles, retrieval adapters, supported storage lanes and release/test procedures. Determine which are already implemented, partially implemented or only documented.

The public export and its version marker are observations about that export, not the operator's current private branch or deployment. Public documentation already refers to state/knowledge and injection contracts; describing them here is not evidence that Mémoire lacks them. [R1, R3, R4]

If the agreed direction conflicts with an existing governing contract, name the exact conflict and isolate the affected change. Do not silently override source instructions or invent an operator decision number. Unrelated instrumentation and safe replay work can continue.

### 5. Questions that can genuinely block an action

A missing model endpoint may block a live local-model demonstration, but not fixture-based implementation. Missing original lab artifacts block reproduction of their reported scores, but not new regression cases based on the stated failure mechanisms.

An unclear authorization boundary, destructive production migration, source data disclosure or conflicting source policy can block the affected write or rollout. Seek a precise ruling only when inspection cannot resolve it. Do not ask the operator to restate the design or pick low-level table names merely because a choice is available.

### 6. Document governance

Keep one owning chapter per major concern. The index supplies headlines and links; the handoff supplies a bounded mission; templates are editable starting aids. Do not duplicate the entire reference into every agent prompt.

Use targeted reading and stable links. The first agent can load its handoff, core invariants, relevant chapters and acceptance subset. Later agents load the chapters for their work. This applies the same progressive-disclosure principle to the design documents themselves.

When implementation reveals a constraint, add an implementation note or experiment result without silently changing an agreed requirement. When an operator decision changes the requirement, record its scope, date and reason, then update the owning chapter and traceability map.

Keep the modular edition authoritative and regenerate the single-file edition. The included build helper assembles documentation only; it does not implement any Mémoire behavior.

### 7. Glossary

| Term | Meaning in this reference |
|---|---|
| Evidence | Retained source material about statements, actions or results; not a blanket guarantee of truth. |
| Observation/candidate claim | Source-linked extracted assertion; review status does not determine every use. |
| Historical episode | Bounded, expandable account preserving problem, work, outcomes and uncertainty. |
| Task projection | Current scoped account of a work item's state. |
| Understanding | Maintained project interpretation, requirements or decisions with applicable support and publication status. |
| Supported change | Proposed semantic revision with baseline, evidence, scope and consequence. |
| Dreaming/consolidation | Processing that maintains or proposes understanding; not automatic human approval. |
| Checkpoint | Explicit coverage basis of a representation or meeting, including known gaps. |
| Publication | Making an exact version available under its intended authority and visibility policy. |
| Alignment/design conversation | Prepared project meeting combining reconciliation and forward-looking discussion. |
| Feedback | Operator evaluation of Mémoire's support, distinct from project-domain outcomes. |
| Shadow/opt-in | New behavior exercised without silently altering existing production authority or injection. |

### 8. Exit from paper design

The next uncertainty to reduce is practical: can identified, normalized capture produce less noisy, adequately complete and faithful observations with lower avoidable inference cost? The first handoff targets that foundation with code, baseline comparisons, omission checks and a reproducible local example. Subsequent increments exercise lifecycle correctness, supported consolidation and read products before using the operator meeting to assess higher-level meaning and usefulness.


---

<a id="template-meeting-packet"></a>

## Meeting packet — template

**Purpose:** agent-prepared context for an alignment/design conversation, not a form the operator must fill out. Omit irrelevant sections. Do not invent content to fill the template.

### Scope and coverage

- Meeting / packet version: `<stable identifiers>`
- Project / topics / work items: `<explicit scope>`
- Baseline understanding: `<version or explicitly unavailable>`
- Evidence checkpoint: `<included source ranges/executions>`
- Known capture, derivation or index gaps: `<specific gaps or checked none>`
- Newer evidence outside this packet: `<known pending material>`
- Preparation configuration: `<record reference>`

### Where the work stands

`<Bounded account of objectives, outcomes, active constraints, blockers and outstanding reviews. Keep reports, verification and deployment distinct. Link decisive evidence.>`

### What remains unchanged

`<Applicable understanding that still explains the work. “No material change detected in covered evidence” is valid; do not claim completeness over gaps.>`

### Question worth discussing

**Previously understood:** `<claim/version and scope>`

**What happened:** `<relevant developments; sources; counterevidence>`

**Possible implication, pending confirmation:** `<interpretation or design hypothesis>`

**Practical consequence:** `<what would change in requirements, reasoning or future work>`

**Question requiring judgment:** `<the distinction the operator actually needs to resolve>`

**Already investigated:** `<relevant earlier decisions or evidence; avoid repeated questions>`

**Expand evidence:** `<valid service-issued handles; no invented references>`

### Other ideas, only when useful

`<Clearly exploratory suggestions grounded in actual patterns, or operator-introduced topics. No obligatory brainstorming.>`

### Not being decided here

`<Deferred proposals, unresolved dependencies, publication exclusions and why. Omission is not acceptance.>`

### Conversation reminder for the agent

Allow natural movement between reconciliation and design. Record implications accurately afterward. Do not interpret an idea as a decision, a decision as unlimited authorization, or a retrospective explanation as original rationale. Offer source expansion when needed instead of narrating the full trace.


---

<a id="template-meeting-outcomes"></a>

## Meeting outcomes — template

**Purpose:** compact closing account linked to the actual conversation and packet. This is not a second interview or per-observation acceptance form.

### Record identity

Meeting: `<ID>` · Packet/checkpoint: `<exact version>` · Conversation: `<source reference>`

### Agreed or corrected understanding

`<What was reaffirmed, corrected or qualified; scope; supporting operator statements. Separate a later explanation from a historical rationale.>`

### New decisions

`<Explicit decisions made in this conversation, applicability, effective context and exact source references. Leave absent when none were made.>`

### Ideas still under exploration

`<Hypotheses and suggestions not adopted. Preserve any limits or stated conditions.>`

### Authorized next steps

`<What the operator authorized, for which work/environment, and what remains prohibited or requires another decision. An idea alone is not authorization.>`

### Still open or deferred

`<Unresolved questions, dependencies and material items not discussed.>`

### Publication disposition

`<Which exact changes were confirmed; which generated interpretations remain proposed; baseline-version conflict if any. No hidden additions approved by silence.>`

### Optional feedback

What helped, and what should be different next time?

`<Operator may answer here, elsewhere in the conversation, or later. Do not block closure. Link exact feedback to the evaluation record rather than converting it into project-domain knowledge.>`


---

<a id="template-meeting-feedback"></a>

## Meeting evaluation — template

**Operator-facing prompt:** What helped, and what should be different next time?

Everything else below is populated by the system where observable. Do not make the operator complete this record.

### References

Meeting / packet / checkpoint / conversation / outcomes: `<stable references>`

Code, model, prompt and relevant configuration versions: `<known values; otherwise missing>`

### Exact operator feedback

`<Original wording, unmodified; preserve multiple notes and timestamps where applicable.>`

Feedback status: `<provided / not provided>`

Scope: `<meeting, item, project, interface, or another explicitly supported scope>`

Later feedback or linked downstream issue: `<reference if present>`

### Automatic measurements

`<Preparation timings, calls, reported usage, packet size, expansion events, meeting status and pending items. Include units and definition versions.>`

Unavailable measurements: `<explicit missing values and why>`

Estimates: `<value, method and uncertainty; never disguised as direct measurement>`

### Interpretation, not operator quotation

Potential success pattern: `<attributed analysis>`

Potential failure: `<attributed analysis>`

Possible cause and alternatives: `<hypotheses tied to evidence>`

Relevant omitted material to inspect: `<bounded sample or incident>`

### Improvement linkage

Issue / experiment: `<reference or none>`

No feedback, an abandoned meeting or a polite acknowledgment does not establish satisfaction. A new project decision does not by itself prove the earlier meeting was wrong.


---

<a id="template-improvement-experiment"></a>

## Improvement experiment — template

**Experiment ID:** `<stable ID>` · **Status:** `<proposed / running / assessed / promoted / reverted>`

### Motivation

Exact feedback and meeting/item references: `<sources>`

Observed problem or successful pattern to preserve: `<specific behavior>`

Why this is not merely new evidence or a changed operator decision: `<where applicable>`

### Diagnosis

Evidence existed / captured / available / retrieved / included / interpreted / presented: `<which step failed, or which remains unknown>`

Primary hypothesis: `<testable explanation>`

Alternative explanations: `<plausible causes not yet ruled out>`

### Proposed change

Changed component/configuration: `<one bounded change or explicitly bundled change>`

Baseline versions: `<code/model/prompt/config>`

Candidate versions: `<code/model/prompt/config>`

Expected benefit: `<observable improvement>`

Potential regressions: `<scope, omissions, latency, burden or useful behavior at risk>`

### Evaluation fixed before judging results

Development cases: `<evidence/checkpoints and provenance>`

Held-out cases: `<not used to tune this change>`

Time-faithful replay boundary: `<no future evidence leakage>`

Deterministic checks: `<conditions>`

Semantic assessments: `<rubric, assessor and limitations>`

Live-use questions: `<what requires operator use>`

Cost measures and acceptance targets: `<named metrics, denominators and missingness>`

Omitted-material check: `<how a quiet failure could be discovered>`

### Results and decision

Observed results: `<counts, examples and limitations; no invented metrics>`

Positive behavior preserved: `<evidence>`

Remaining uncertainty: `<untested conditions>`

Disposition: `<promote, revise or reject; authority and rationale>`

Disable/rollback path: `<verified procedure>`

Follow-up: `<specific unresolved question, not an automatic self-modification instruction>`


---

<a id="source-register"></a>

## Source register and evidence boundaries

**Checked:** 17 September 2026. External URLs are mutable; this is a documentation-level check, not a code audit or benchmark. The public export was not pinned to an immutable commit in this package. Recheck implementation details against the actual checkout and deployed versions.

Source IDs in the reference point here. Design proposals are authored from the discussion; they are not presented as claims that an external engine already implements Mémoire's complete workflow.

### Primary project-design basis

<a id="c1"></a>

**Edition 1.1 addition:** the operator challenged the interview-first handoff and requested capture cleanup, improved observations and upstream lifecycle work before the human-facing layer. The revised sequencing follows that instruction; detailed stage gates remain proposals. External research below is inherited, not rechecked in this documentation-only revision.

#### C1 — Operator design conversation

The conversation establishes the problem, agreed product boundaries and intended evaluation loop. It is not reproduced as a verbatim transcript. Its approved direction is recorded in the reference's `DR-01` through `DR-12` commitments and owning chapters.

The material progression was: evidence archive and meaningful-change review; three conceptual views from the addendum; first-class temporal authority for current work; high-level reconciliation plus design discussion; ledger/coordinator/session identity; coverage-aware diffs; and automatic measurements plus optional exact meeting feedback from first use. Low-level mechanisms and first-implementation sequencing remain proposals where labeled.

<a id="a1"></a>

#### A1 — Supplied design addendum

[Original addendum, unchanged](#source-addendum): *Memoire design addendum: history, task state, and project understanding*, uploaded as `Pasted text.txt`.

Citations to A1 line ranges refer to the numbered source supplied in the conversation. The relevant sections are the three views (15–23), historical rationale (25–39), independent task state and review (41–69), experiment findings and limitations (71–85), read contracts (87–99), and success criteria (112–118).

The historical response instructions inside the addendum are source material, not the current implementation assignment. Its experiment outcomes are reported observations, not independently reproduced results. No local repository, private answers, original checkpoints, complete read products or scoring artifacts were supplied. The source explicitly says the experimental candidates were not promoted and that the V15 trace-growth/clipping errors belonged to the lab design.

### Mémoire public documentation

| ID | Primary source | Use and limitation |
|---|---|---|
| <a id="r1"></a>R1 | [Public README](https://raw.githubusercontent.com/superbiche/memoire/main/README.md) | Repository boundary, source/export distinction, lanes and pointers to private governing instructions. Not the private checkout or deployment. |
| <a id="r2"></a>R2 | [Public quickstart](https://raw.githubusercontent.com/superbiche/memoire/main/QUICKSTART.md) | Standalone path, API writer boundary, explicit provider dialect/data boundary and external invocation allowance. Not a complete hub contract. |
| <a id="r3"></a>R3 | [Public MISSING record](https://raw.githubusercontent.com/superbiche/memoire/main/MISSING.md) | Dated issue/correction history, including existing state/knowledge documentation and custody corrections. Read status notes; old filings are not necessarily current gaps. |
| <a id="r4"></a>R4 | [Public VERSION](https://raw.githubusercontent.com/superbiche/memoire/main/VERSION) | Export reports 0.7.0 when checked. This does not identify the current private branch or running deployment. |

No private `memoire:D<n>` entries were inspected or invented. An implementing agent must obtain source contracts from its actual checkout. The new `DR-*` identifiers are local to this reference only.

### External mechanisms and local-model references

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

### How to interpret the research

The borrowing chapter identifies documented mechanisms and separately proposes Mémoire adaptations. It does not rank “successful” engines by popularity or claim a benchmark winner. Vendor statements about accuracy, compression, throughput or being state-of-the-art were not adopted as Mémoire acceptance evidence.

No external engine was installed or tested here. No local inference speed, hardware fit, meeting duration, extraction accuracy or performance improvement was measured. Numerical targets in the design are explicitly operator goals or pilot hypotheses; addendum numbers retain their reported limitations.

Documentation paths and defaults can change. The first agent should verify the exact interface it intends to use, not repeat all research or silently substitute new external product defaults for approved Mémoire behavior.


---

<a id="source-addendum"></a>

## Memoire design addendum: history, task state, and project understanding

Please reconcile this contribution with our ongoing Memoire design discussion. It comes from a parallel series of compaction and retrieval experiments, followed by discussion with Michel. It is self-contained: no access to local repositories or experiment files is required.

**Requested response:** identify what is already covered, what this changes or clarifies, and what remains genuinely undecided. Challenge the distinctions and proposed mechanisms where appropriate. Return a focused design response in this conversation; this is not an implementation request. If something is already covered, point to the relevant part of our discussion. If evidence is insufficient, name the missing evidence rather than treating the suggestion as established.

### 1. Direction and newly agreed distinction

Michel describes the rework as:

> An evidence archive underneath a small, evolving understanding of each project—with human review focused on meaningful changes to that understanding.

The new distinction is that detailed work history, current task state, and project understanding serve different purposes. Michel agreed that the system needs to differentiate them. The mechanisms below remain proposals, not settled architecture.

The evidence archive is the common foundation. Above it, distinguish three views:

| View | Question answered | Typical content | Change over time |
|---|---|---|---|
| Work history | What happened, and how did we reach this result? | Original problem, attempts, failures, chosen solution, verification, rejected alternatives | New evidence extends the account; corrections remain traceable |
| Current task state | Where are we now, and what remains? | Objective, active constraints, authorized scope, pending actions, blockers | Updates as work progresses; resolved items leave the active view |
| Project understanding | What have we learned about this project? | Domain invariants, architecture, trade-offs, recurring failure modes, structural uncertainties | Evolves through supported changes whose significance determines review |

These are conceptual views. They do not imply three databases, a fixed ontology, one enormous project summary, or three independently maintained narratives. A fact may contribute to more than one view through shared evidence links.

### 2. Preserve the problem behind the solution

Michel noted that detailed history can give the solution more weight than the original problem. “We replaced X with Y” records an outcome, but loses why the choice was reasonable and when it might cease to be.

A useful historical episode should retain enough of:

- the original intent and problem;
- constraints, assumptions and environment at that time;
- what was attempted and actually observed;
- why the chosen solution was accepted, including known limits;
- what remains unresolved or would justify revisiting the decision.

An explanation reconstructed later must remain distinguishable from a reason recorded at the time. Otherwise, a workaround can become a supposed architectural principle, and a plausible retrospective story can replace the evidence.

Chronology matters for reconstructing an episode, but retaining the entire chronology in every prompt is costly. Keep the detailed sequence expandable from a bounded account.

### 3. One episode, three different contributions

Consider an imported empty feed that incorrectly deletes existing data:

- **History:** the failure was reproduced; deletion ordering was changed; specific tests demonstrated preservation of existing data.
- **Task state:** implementation is complete; independent review is still pending.
- **Project understanding:** an empty feed is an actionable failure and must preserve existing data—if supported by the project's actual requirements, not inferred merely from one patch.

Completing the review changes task state. It does not erase the history, and it need not change the domain invariant. A later operator decision to accept empty feeds could change that invariant; its consequences and superseded scope would need to remain inspectable.

Multiple concurrent tasks also need distinct state. One task's completed review must not resolve another's pending review merely because they concern the same project.

### 4. Review meaningful changes to understanding

A promising review unit is a **supported change**, showing:

- current understanding and proposed revision;
- what new evidence motivated the change;
- supporting and conflicting evidence;
- affected scope and practical consequence;
- whether it adds, refines, contradicts, resolves or retires a claim.

Repeated corroboration should usually strengthen an existing evidence trail without generating repeated human work. Similar wording alone does not establish equivalence: differently scoped or contradictory claims must remain distinct.

Routine task-state transitions can be evidence-backed without automatically becoming project-understanding review items. However, an operational event can expose a structural lesson. The open design problem is deciding when that crossing is warranted and how to avoid both notification overload and silent consequential changes.

Keep these dimensions distinct: source provenance, evidential support, human review status, practical importance, and authority to instruct. A reviewed explanation is not automatically a standing order. Recalling old authorization is not permission for a new action.

Unreviewed evidence should remain usable for factual retrieval with its status visible. Human review need not become a prerequisite for retrieving a recent test result.

### 5. Experimental findings that motivate this

These are small, repeatedly studied development cases using a local Qwen model. Factual scoring was performed by the coordinating model; independent reviewers saw source machinery, not private answers. They identify failure mechanisms, not universal performance claims.

| Observed finding | Design implication to consider |
|---|---|
| V11 lost a critical data-preservation constraint while all three automatic retrieval hits contained review procedures. | Select evidence for the caller's purpose; lexical/topic relevance alone can fill the budget with the wrong kind of information. |
| V13's protected records filled all six slots at the first checkpoint and never incorporated later constraints. | Bounded memory needs replacement and lifecycle rules, with durable understanding separated from temporary details. |
| V7 supplied selected relevant passages and recovered several previously missed facts in all three repeats, but all three still attributed an entire 33-failure log to one demonstrated cause. | Relevant evidence helps recall but does not guarantee interpretation. Preserve claim scope, uncertainty and counterevidence through synthesis. |
| V15's ten completed pairs used 2.24 times baseline continuation input and met 27/40 criteria versus 30/40. It appended a full trace beside a smaller summary. | Small individual representations can still produce a large combined context. Measure the complete read product. |
| V15 never requested retrieval and later aborted on a nonexistent generated source reference. | This does not test Memoire ranking. Retrieval triggering and reference handling belong to the consumer contract as well as the service. |

A separate narrow live search returned five near-identical, lapsed observations about the same old decision. This is an illustration of duplicate budget consumption, not a benchmark or evidence that the underlying decision remains current.

V15's trace growth and aggressive initial-prior clipping were lab design errors. Do not attribute them to Memoire. No experimental candidate described here was promoted.

### 6. Retrieval should serve the chosen purpose

A caller should be able to express, or confirm, an intent such as:

- **Resume this task:** current objective, constraints, blockers, latest verified state, and relevant changes since last seen.
- **Explain this past decision:** original problem, contemporaneous reasoning, attempts, results, trade-offs and later corrections.
- **Orient me in this project:** compact current understanding, important uncertainties and pointers to its evidence.

These are behavioral examples, not proposed endpoint names. Mixed questions should be supported. The system should reveal which view and scope it supplied rather than silently treating one as interchangeable with another.

Useful shared read properties would include bounded output, diversity and deduplication, stable source handles, precise expansion, explicit omissions, and freshness/coverage information. A reader must distinguish no match, unavailable evidence, clipped evidence, and recent events not yet derived or indexed.

A hit can expand into an ordered evidence group—call, result, correction, later verification—without treating adjacency as causal proof. Current Memoire documentation already describes hybrid retrieval, provenance/review metadata, and derivation-run sibling pointers. Build on those where appropriate; same-run lineage does not itself establish correction, causation or resolution. These capability statements are based on documentation inspection, not a complete audit of the rework or deployed code.

### 7. Questions to resolve in the existing design

1. What makes a statement part of durable project understanding rather than history or task state? How do scope and expected lifetime participate without requiring a perfect classifier?
2. How are updates linked across the views without duplicating ownership or allowing a generated narrative to become its own evidence?
3. Which changes warrant human review? How are repeated corroboration, uncertain proposals and consequential contradictions handled differently?
4. How can a current understanding be challenged, revised and reconstructed as of an earlier point, while preserving the original evidence and reasons?
5. What freshness and coverage guarantees can readers rely on during active work, including capture/derivation/index lag?
6. How should consumers select and combine views within a context budget, especially with simultaneous tasks or conflicting evidence?

Please prioritize any questions that materially alter the existing design, and avoid introducing mechanisms merely to fill out this list. Do not infer that these capabilities are absent just because this addendum asks about them.

### 8. Success criteria worth carrying forward

Evaluate whether a reader can recover the original problem and scope of a solution; resume the right task without reviving resolved work; distinguish established facts from unresolved interpretations; and retrieve decisive evidence within a bounded budget.

For human review, measure useful changes to understanding per review item, duplicate review burden, stale conclusions challenged, and inspectable evidence coverage. For consumers, measure decision quality together with peak and cumulative context, retrieval turns, and derivation/index costs.

Compaction is one consumer of this design. It should not dictate the whole memory architecture. The desired result is a small, useful understanding that remains challengeable through its evidence archive, with a separate reliable account of ongoing work.
