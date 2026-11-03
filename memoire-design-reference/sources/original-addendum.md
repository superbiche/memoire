# Memoire design addendum: history, task state, and project understanding

Please reconcile this contribution with our ongoing Memoire design discussion. It comes from a parallel series of compaction and retrieval experiments, followed by discussion with Michel. It is self-contained: no access to local repositories or experiment files is required.

**Requested response:** identify what is already covered, what this changes or clarifies, and what remains genuinely undecided. Challenge the distinctions and proposed mechanisms where appropriate. Return a focused design response in this conversation; this is not an implementation request. If something is already covered, point to the relevant part of our discussion. If evidence is insufficient, name the missing evidence rather than treating the suggestion as established.

## 1. Direction and newly agreed distinction

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

## 2. Preserve the problem behind the solution

Michel noted that detailed history can give the solution more weight than the original problem. “We replaced X with Y” records an outcome, but loses why the choice was reasonable and when it might cease to be.

A useful historical episode should retain enough of:

- the original intent and problem;
- constraints, assumptions and environment at that time;
- what was attempted and actually observed;
- why the chosen solution was accepted, including known limits;
- what remains unresolved or would justify revisiting the decision.

An explanation reconstructed later must remain distinguishable from a reason recorded at the time. Otherwise, a workaround can become a supposed architectural principle, and a plausible retrospective story can replace the evidence.

Chronology matters for reconstructing an episode, but retaining the entire chronology in every prompt is costly. Keep the detailed sequence expandable from a bounded account.

## 3. One episode, three different contributions

Consider an imported empty feed that incorrectly deletes existing data:

- **History:** the failure was reproduced; deletion ordering was changed; specific tests demonstrated preservation of existing data.
- **Task state:** implementation is complete; independent review is still pending.
- **Project understanding:** an empty feed is an actionable failure and must preserve existing data—if supported by the project's actual requirements, not inferred merely from one patch.

Completing the review changes task state. It does not erase the history, and it need not change the domain invariant. A later operator decision to accept empty feeds could change that invariant; its consequences and superseded scope would need to remain inspectable.

Multiple concurrent tasks also need distinct state. One task's completed review must not resolve another's pending review merely because they concern the same project.

## 4. Review meaningful changes to understanding

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

## 5. Experimental findings that motivate this

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

## 6. Retrieval should serve the chosen purpose

A caller should be able to express, or confirm, an intent such as:

- **Resume this task:** current objective, constraints, blockers, latest verified state, and relevant changes since last seen.
- **Explain this past decision:** original problem, contemporaneous reasoning, attempts, results, trade-offs and later corrections.
- **Orient me in this project:** compact current understanding, important uncertainties and pointers to its evidence.

These are behavioral examples, not proposed endpoint names. Mixed questions should be supported. The system should reveal which view and scope it supplied rather than silently treating one as interchangeable with another.

Useful shared read properties would include bounded output, diversity and deduplication, stable source handles, precise expansion, explicit omissions, and freshness/coverage information. A reader must distinguish no match, unavailable evidence, clipped evidence, and recent events not yet derived or indexed.

A hit can expand into an ordered evidence group—call, result, correction, later verification—without treating adjacency as causal proof. Current Memoire documentation already describes hybrid retrieval, provenance/review metadata, and derivation-run sibling pointers. Build on those where appropriate; same-run lineage does not itself establish correction, causation or resolution. These capability statements are based on documentation inspection, not a complete audit of the rework or deployed code.

## 7. Questions to resolve in the existing design

1. What makes a statement part of durable project understanding rather than history or task state? How do scope and expected lifetime participate without requiring a perfect classifier?
2. How are updates linked across the views without duplicating ownership or allowing a generated narrative to become its own evidence?
3. Which changes warrant human review? How are repeated corroboration, uncertain proposals and consequential contradictions handled differently?
4. How can a current understanding be challenged, revised and reconstructed as of an earlier point, while preserving the original evidence and reasons?
5. What freshness and coverage guarantees can readers rely on during active work, including capture/derivation/index lag?
6. How should consumers select and combine views within a context budget, especially with simultaneous tasks or conflicting evidence?

Please prioritize any questions that materially alter the existing design, and avoid introducing mechanisms merely to fill out this list. Do not infer that these capabilities are absent just because this addendum asks about them.

## 8. Success criteria worth carrying forward

Evaluate whether a reader can recover the original problem and scope of a solution; resume the right task without reviving resolved work; distinguish established facts from unresolved interpretations; and retrieve decisive evidence within a bounded budget.

For human review, measure useful changes to understanding per review item, duplicate review burden, stale conclusions challenged, and inspectable evidence coverage. For consumers, measure decision quality together with peak and cumulative context, retrieval turns, and derivation/index costs.

Compaction is one consumer of this design. It should not dictate the whole memory architecture. The desired result is a small, useful understanding that remains challengeable through its evidence archive, with a separate reliable account of ongoing work.
