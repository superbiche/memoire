# 02 — Evidence, views and update lifecycles

**Status:** required behavior for shipped capabilities; record shapes are conceptual.  
**Owns:** DR-01, DR-02, DR-04 and DR-05.  
**Basis:** [C1](../sources/README.md#c1); [A1, lines 15–69, 97–99](../sources/README.md#a1). Source IDs resolve in [the source register](../sources/README.md).

## 1. Evidence foundation

Retain source events and artifacts under the existing custody and redaction policy, with stable identities and enough origin information to replay derivation or inspect a disputed claim. A captured event is not automatically a true statement about the world: it is evidence of a statement, command, result, review or other occurrence.

Keep mechanically available facts mechanical. An exit code, artifact revision or test result should not depend on the model inventing its representation from a prose retelling. The model can explain significance while retaining the original basis.

The archive need not preserve every sensitive byte forever. Deliberate redaction, deletion and quarantine remain possible under operator policy. “Replayable” means replayable from retained lawful and authorized evidence, with missing sources disclosed—not a claim that nothing can ever be removed. Public Mémoire documentation distinguishes deliberate purge from automatic pruning; do not replace that boundary with an absolute immutability rule. [R3](../sources/README.md#r3)

## 2. Conceptual records and relationships

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

## 3. Dimensions that must not collapse

**Provenance:** who or what produced the material, from which session/run/artifact and by which derivation configuration.

**Evidence character:** directly observed result, reported assertion, or inferred explanation. These categories can be refined in implementation but must not disappear into a confidence number.

**Human review/publication:** whether the operator reviewed a particular interpretation or version, and what exactly was approved.

**Time and freshness:** when the fact applies, when evidence arrived, what the representation covers, and what relevant information remains unprocessed.

**Practical importance:** whether the claim changes current action, a requirement, an explanation or future work. Importance is a routing judgment, not proof.

**Action authority:** whether a current instruction or applicable standing policy authorizes an action. Memory of old permission is not present permission.

**Access scope:** who may read or process the source and its derivatives. Topic tags or session links are not access-control grants.

These dimensions can be stored in existing fields and relations. A universal weighted trust score must not substitute for them.

## 4. Three views, shared evidence

### Work history

Preserve the original problem and intent, constraints and environment, what was tried, what was actually observed, why the solution was accepted when that reasoning is recorded, and what remained unresolved.

A later model reconstruction must be labeled as such. A later operator clarification is valuable evidence, but it was not necessarily the rationale documented at the original decision time. Missing rationale stays missing.

Chronology supports reconstruction, not causation by itself. Two events next to each other, or observations from one derivation run, are not automatically cause/effect or correction/resolution pairs.

### Current task state

Represent objective, active constraints, authorized scope, outstanding work, blockers and supported progress. Distinguish planned, attempted, completed, verified, reviewed, merged and deployed as applicable. They are not interchangeable stages of a universal state machine.

A task can be implemented but awaiting review, reviewed but not merged, or deployed with a known failing check. Concurrent facets should not be flattened into a misleading single status. Resolved items leave the active projection while remaining available historically.

Current task state updates on sufficient evidence without requiring project-understanding review. A blanket `accepted_only` filter is not a valid implementation of this target behavior.

### Project understanding

Maintain scoped requirements, decisions, explanations, architecture, trade-offs, reusable lessons and open structural questions. A useful shape is a maintained answer to a topic question, not a single ever-growing global summary.

A fact's repeated occurrence may motivate a broader interpretation, but repetition alone does not prove generality. A workaround remains a workaround until support or a decision justifies a different role.

## 5. Two update paths

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

## 6. Baselines, challenges and requirements

When new work arrives, do not automatically demote all maintained understanding to obsolete history. Determine what changed.

If production now runs revision B, an old description of revision A becomes historical for that environment. If a test challenges an architectural explanation, retain the explanation and its challenge until resolved. If a new implementation violates a requirement, record the violation rather than treating the implementation as an amendment.

Illustration: a captured import run deletes records on a valid empty feed. That can be the current operational account. A requirement to preserve records on upstream failure may still govern. The interview determines whether the evidence reveals a bug, a missing distinction, or an intentional change in direction.

The current-work view can report the discrepancy immediately. Project-understanding publication follows the relevant interpretation or decision.

## 7. Corrections, publication and dependencies

Retain source links for every consequential published claim and relevant links between derived versions. A correction should identify what it corrects and why; simply editing the latest paragraph loses the basis needed to avoid regenerating the rejected interpretation.

Derived pages may assist navigation, but must not become independent evidence for each other. When a memory page re-enters a harness transcript, mark it as previously derived material. Its repetition is not fresh corroboration. Distinct source handles pointing to copies of one original are not independent sources either.

When evidence is invalidated, corrected, made inaccessible or deliberately deleted, mark affected derivatives for reconsideration. Do not silently keep the same confidence or expose deleted content through cached summaries. See [operations](08-locality-security-and-operations.md).

Publication must apply to an exact displayed revision and base version. Use the repository's transaction or concurrency mechanism to prevent an interview against an old baseline from overwriting a newer decision. A conflict is surfaced or re-prepared; it is not resolved by a model claiming the versions are “basically the same.”

Approval of a consolidated statement does not bulk-approve all source observations. An explicit operator decision is recorded directly as decision evidence; additional model inferences beyond it remain proposals. This avoids a second approval loop for a decision already made.

## 8. Time and reconstruction

Preserve enough information to distinguish:

- What the system knew or believed at an earlier time, using the evidence then available.
- What current evidence suggests was happening at that earlier time.

These differ when evidence arrives late or an interpretation is corrected. A Friday correction can change today's reconstruction of Tuesday without rewriting Tuesday's recorded belief.

The physical implementation need not be a full bitemporal database. It must retain version and evidence relationships sufficient for the supported historical queries. Unknown times remain unknown; ingestion time should not silently become event time.

## 9. Useful defaults, not mandatory schema

Start with stable claim identities, source references, explicit scope, derivation version, review/publication state and correction links. Add richer relation types only when a demonstrated use case requires them. Proposed consolidation operations—add, refine, supersede, resolve, no-change and conflict—are semantic examples, not a committed API.

The first implementation may present views as bounded read products rather than materializing every projection. The essential obligation is that the reader can tell what a statement means, what supports it, where it applies and how current it is.
