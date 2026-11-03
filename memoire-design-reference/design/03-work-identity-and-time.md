# 03 — Work identity, execution graphs and time

**Status:** agreed distinctions; integration shapes require repository inspection.  
**Owns:** DR-06 and the coverage semantics used by reads and meetings.  
**Basis:** [C1](../sources/README.md#c1); [A1, lines 49–51, 95–99](../sources/README.md#a1). Sources: [register](../sources/README.md).

## 1. Identity belongs to several layers

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

## 2. Combine the operator's three integration options

### Ledger integration

Where a run/review ledger exists, ingest its job, run, worker, reviewer and outcome references. Treat it as evidence of recorded execution state. A ledger's successful process completion does not, by itself, prove that the requested objective was semantically achieved.

The adapter should be replay-safe. Re-reading the same record must not create duplicate executions or corroborating evidence. Preserve the external record identity and version where available.

### Coordinator declarations

A coordinating agent can declare the intended work item, decomposition, dependencies, roles and review assignments at dispatch time. This is preferable to reconstructing all relationships through textual similarity afterward.

Preserve the distinction between plan and result: declaring a dependency, assigning a reviewer or scheduling a job does not establish dependency satisfaction, review completion or successful execution.

The coordinator is a source, not an unrestricted identity authority. Keep declaration provenance and allow later correction when a job was misattributed or a scope changed.

### Session-local fallback

When neither integration exists, generate a stable local work identity mechanically, anchored to the harness-session UUID. Let the agent generate a human-readable task summary; the summary is not the identifier.

A display can read `[work ID] — Investigate import data loss`, with the source session linked beneath it. Label edits must not break identity. A session may acquire more than one work item; a work item may later be linked to another session.

Do not routinely interrupt the operator with “I cannot identify the task.” Preserve a useful narrow identity and expose unresolved broader linkage where relevant. Ask only when the ambiguity would affect a consequential action or merge that cannot safely remain unresolved.

## 3. Linking without falsifying certainty

When later evidence establishes that two local items belong to one objective, preserve aliases and original attribution. Record how and when the relationship was established. Do not rewrite history as though the original collector knew the relationship.

Similar labels are candidates for investigation, not permission to merge. Two “review the migration” items may concern different branches, revisions or reviewer obligations. Different labels may describe the same objective. Strong linkage can come from an explicit external ID, a coordinator relation or operator clarification, with conflicts still preserved.

The initial design does not require automatic semantic task merging. Conservative local identity plus explicit linking is preferable to confident cross-task contamination.

## 4. Graphs, dependencies and completion

Represent parent/child work and dependency relationships only to the extent supplied or supported. A dependency graph is not automatically a chronological ordering of all events. Concurrent work can legitimately remain divergent.

Each task state should identify outstanding completion conditions. Completing one child does not automatically close the parent; completing one review does not close another task's review. A parent graph is complete only under its applicable completion criteria.

A review must identify its target work, execution or artifact revision. “Approved revision A” does not approve revision B after additional edits. A generic `reviewed=true` field without a target is insufficient for these workflows.

Agent reviews are operational evidence. Human acceptance of a project interpretation is a different act. A record may contain both, but must name which occurred and what each applied to.

## 5. Facts retain environment and revision scope

“Passing on branch A, failing on branch B” can be the correct current state. Do not flatten this into a single universal passing/failing answer. Likewise, a local experiment is not a production deployment, and a successful test invocation is not blanket validation of an entire repository.

The consumer's work/environment scope determines which state is primary. A broader project orientation may show the disagreement. Unknown environment or revision should be visible when it limits use.

## 6. Evidence time and arrival time

Retain event time when supplied, ingestion time, and processing/version timestamps where relevant. Clock skew and late arrival mean timestamp sorting is not a complete account of causality.

Sequence or cursor information from a source can help establish coverage within that source. It does not establish a global total order across independent harnesses. Avoid inventing such an order merely to produce a neat timeline.

A late-arriving review of an old revision belongs to that target and time scope. It does not automatically override a newer result for a different revision.

## 7. Coverage checkpoints

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

## 8. What a diff means

A semantic diff compares a baseline understanding with newly incorporated relevant evidence and its proposed implications. It is not simply the set of observations created after the previous meeting timestamp.

Late evidence, corrections, newly linked work and invalidated sources can all change the diff. For reproducibility, record the baseline version, checkpoint, selected evidence and preparation configuration.

Prepare a meeting against a stable checkpoint. Evidence arriving during discussion remains visible as newer material; deliberately rebase an affected proposal or carry it forward. Do not silently change the proposal being approved. Publication checks the base version again.

## 9. First implementation boundary

Start with available IDs and stable local fallback. Support explicit provenance and at least one multi-task case. Make ledger/coordinator integrations adapter seams rather than prerequisites for every session. Do not build a complete execution-graph orchestrator or automatic task-resolution model before the first capture-to-observation increment can be exercised. Minimal source/task attribution belongs to the foundation; richer integration can follow measured need.

Acceptance examples: parallel tasks with one completed review; review of an old revision; duplicate labels across namespaces; delayed evidence; and one task continued in another session. See [acceptance scenarios](10-acceptance-scenarios.md).
