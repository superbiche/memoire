# 06 — Project alignment and design conversations

**Status:** agreed product behavior; agenda preparation and interaction details are empirical.  
**Owns:** DR-09 and DR-10.  
**Basis:** [C1](../sources/README.md#c1); [A1, lines 53–69](../sources/README.md#a1). Sources: [register](../sources/README.md).

## 1. A project meeting grounded in memory

The interaction is not a memory approval screen. Its shape resembles a team meeting: understand what happened, align on what it means, then share ideas and discuss direction where useful. “Daily meeting” describes the conversational model, not a requirement to schedule daily meetings.

Reconciliation and design must remain distinguishable in what Mémoire records, without forcing the operator to switch modes. The agent may challenge assumptions, suggest improvements and connect patterns across work. It must distinguish evidence, inference and proposal.

The core presentation is:

> What was known → what happened → what this could mean, pending confirmation.

The operator steers, accepts, contradicts, qualifies, explores and decides. The system prepares enough that the operator need not inspect individual observations or answer questions already resolvable from evidence.

## 2. The agenda unit is a project question

A session, observation or task is not necessarily the right review unit. Several parallel tasks may jointly expose one requirement ambiguity. One task may expose unrelated design questions that belong in different topic discussions.

Group agenda items by supported changes in meaning. Each material item includes the baseline, relevant developments, supporting and conflicting evidence, scope, a proposed interpretation, practical consequence and the question requiring operator judgment.

Preparation should also recognize continuity. Completed work within existing requirements can produce no proposed understanding change. Do not manufacture a lesson or brainstorming item to justify the meeting.

A low-noise outcome is legitimate: “Work progressed within the existing requirements; no governing change is proposed; these operational follow-ups remain.” Qualify this by the known evidence coverage rather than claiming omniscience.

## 3. Preparation happens before operator judgment

The preparing agent should reconstruct results and original problems, remove superseded attempts from the main account without losing their history, distinguish review targets, retrieve applicable requirements, find earlier operator answers and investigate apparent contradictions.

It should resolve factual questions from available evidence. Asking whether tests passed is unnecessary when the captured result answers that question. Asking whether passing behavior establishes a requirement may be worthwhile.

Preparation can use more computation than presentation, but it must remain incremental and measured. Repeatedly rereading the whole archive to present three questions is not automatically economical. Inspect only changed topics plus the evidence needed to interpret them, with broader consistency checks triggered when useful.

The packet is prepared against a stable checkpoint and baseline version. It states known coverage gaps, relevant unprocessed work and material items omitted from the current agenda. It does not quietly approve omitted items.

## 4. Routing into human attention

| Development | Default treatment to validate in use |
|---|---|
| More support for an unchanged scoped claim | Attach support; no repeated review item solely for corroboration. |
| Routine evidence-backed state transition | Update current work; do not automatically amend project understanding. |
| Potential reusable lesson from a narrow episode | Keep a tentative interpretation and its limits; aggregate or prioritize when meaningful. |
| New evidence challenges a maintained conclusion | Make the challenge visible; prepare a review item when its significance warrants judgment. |
| Proposed requirement, direction or applicability change | Discuss before presenting it as an operator-approved governing change. |
| Current action needs permission not already available | Surface the action-specific uncertainty; do not wait silently for a future general meeting. |

This routing is a starting policy, not a perfect materiality classifier. A model score can assist prioritization but cannot silently suppress consequential challenges. Deferred material remains inspectable with age, scope and practical consequence.

## 5. A natural conversation with typed consequences

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

## 6. Reconciliation and design do not share one meaning

| Conversational outcome | Record it as | Do not silently turn it into |
|---|---|---|
| “That was only an experiment.” | Correction of the historical/current interpretation. | A ban on future experiments. |
| “That exception applies to the migration tool.” | Scope qualification. | A project-wide rule. |
| “This explains the incidents better.” | Accepted retrospective interpretation with its date and support. | The rationale documented at the original decision. |
| “We could move this responsibility.” | Exploratory idea or hypothesis. | An adopted architecture. |
| “Use this approach for this component from now on.” | Explicit scoped direction/decision. | Unlimited execution authorization. |
| “Investigate it, but do not change production.” | Authorized exploration with a boundary. | Permission to deploy. |

A new decision is valuable new evidence. It should not be presented as a fact extracted from the preceding work. A retrospective explanation can improve understanding without rewriting what was known at the time.

## 7. The meeting becomes evidence without another approval loop

Retain the conversation and authenticated operator statements using the existing identity and custody model. Record explicit decisions as decisions made in that conversation. Any agent-authored extension beyond the actual statement remains an interpretation or proposal.

A short closing account makes the semantic result inspectable: what was agreed, what changed direction, what remains exploratory, what next steps were authorized and what is still open. This is meeting-level confirmation, not line-by-line observation acceptance.

When a generated publication patch needs confirmation, it must point to the exact displayed revision. The conversation can itself contain sufficient explicit confirmation; a second mechanical interview is not required. Unclear or undisplayed additions remain pending. Silence does not approve them.

The next consolidation must know which questions were resolved and which evidence was considered. It should not ask the same question again from the same evidence or interpret an exploratory idea as independent corroboration of the agent's earlier suggestion.

## 8. Attention budget and cadence

The operator's initial target is that routine work spanning roughly two days should usually require only a few minutes of reconciliation. A substantial pending knowledge change can justify more attention. This is an acceptance target to evaluate, not a measured result or a guarantee for every workload.

Separate required reconciliation time from elective design discussion. A useful twenty-minute design exploration is different from twenty minutes correcting a poor recap. The system must not relabel difficult reconciliation as design to improve metrics. Timing classification may be approximate and operator-correctable.

Bound the number and detail of questions presented. If substantial independent issues exceed the chosen attention budget, prioritize and split the discussion while exposing what remains pending. The operator can expand evidence or continue design discussion deliberately.

There is no mandatory daily schedule, automatic calendar integration or obligation to hold a meeting when nothing meaningful needs discussion. Ordinary reconciliation can be requested or grouped periodically. Action-relevant uncertainty is surfaced to the affected task when needed.

## 9. New evidence during the meeting

Do not silently mutate the packet under discussion. Identify newer material and determine whether it changes an agenda item. An unaffected explicit decision can be recorded; a changed proposal should be re-prepared or carried forward with the conflict visible.

Before publication, check the baseline version. Concurrent operator decisions or other accepted changes must not be overwritten by stale preparation. A reproducible meeting record links the packet actually shown, not merely whatever the latest packet became.

## 10. Outcomes and evaluation are different records

Project outcomes affect history, task state or understanding. Meeting feedback evaluates Mémoire's support. “Your recap was too detailed” should not become a domain requirement of the target project, nor a global user preference without an appropriate scope.

Collect an optional free-text note during, after or later than the meeting. It is not a prerequisite for closing the meeting. Preserve exact feedback, including positive observations, and link it to the actual packet/conversation. See [measurement and learning](07-measurement-and-learning.md).

## 11. Initial interface boundary

Use the least elaborate existing local surface that can support the interaction: existing UI, CLI plus a local agent, or a repository-compatible conversational integration. Do not build a new chat platform merely to validate the meeting concept.

A static packet and optional feedback are useful first steps, but they are not proof that the conversational experience works. Live use must demonstrate that the agent can retrieve supporting evidence, accept corrections, explore ideas without adopting them prematurely, and leave a faithful closing record.

The [meeting packet](../templates/meeting-packet.md) and [outcome](../templates/meeting-outcomes.md) templates are starting aids, not mandatory scripts or form-filling obligations for the operator.
