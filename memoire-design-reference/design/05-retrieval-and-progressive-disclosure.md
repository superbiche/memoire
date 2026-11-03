# 05 — Retrieval and progressive disclosure

**Status:** required read behavior; transport and ranking implementation remain open.  
**Owns:** DR-08.  
**Basis:** [C1](../sources/README.md#c1); [A1, lines 71–99, 112–118](../sources/README.md#a1). Source IDs: [register](../sources/README.md).

## 1. A two-sided contract

The memory service must supply the right representation and honest evidence coverage. The consumer must request relevant memory, preserve its meaning, use valid handles and manage the context it assembles. Neither side alone guarantees useful continuation.

The addendum reports a continuation that never requested retrieval and later generated an invalid source reference. That is not a ranking test. It is a reason to evaluate consumer behavior separately. [A1, lines 80–85](../sources/README.md#a1)

Compaction is one consumer. The same read principles should support ordinary task continuation, historical explanation, project orientation and meeting preparation.

## 2. Purpose, scope and budget

A request should express or establish purpose, applicable project/work/environment scope and an output budget. These need not be rigid enumerated endpoints. Mixed purposes are valid.

| Purpose | Primary representation | Necessary qualifications |
|---|---|---|
| Resume this task | Current objective, constraints, blockers, latest supported state and changes since last seen. | Correct work item, revision scope, outstanding reviews, evidence lag and relevant challenges. |
| Explain a past decision | Original problem, contemporaneous rationale, alternatives, outcomes and subsequent corrections. | Do not replace historical reasons with a plausible later explanation. |
| Orient in a project | Compact maintained understanding and important unresolved questions. | Publication status, freshness and current evidence that materially challenges it. |
| Prepare an alignment meeting | Baseline, relevant outcomes and proposed implications requiring judgment. | Stable checkpoint, omitted/pending matters, available counterevidence and known prior decisions. |

For current work, recent relevant observations are first-class regardless of human review. For governing knowledge, applicable decisions and requirements remain primary while challenges are visible. An implementation must not reduce all purposes to semantic similarity plus one global trust or recency score.

## 3. The response identifies what it supplied

A read product should identify its selected views, resolved scope, version/checkpoint, evidence handles and limitations. It should indicate consequential omissions and whether the response is partial.

Conceptually, a response may contain a bounded overview, active constraints/state, relevant live changes, supported understanding, unresolved conflicts and expansion pointers. This is not a requirement to include every section in every call; include what serves the stated purpose within budget.

When scope is ambiguous, use the narrowest supported identity rather than silently combining tasks. Broader related material can be shown as related, not attributed to the active task without evidence.

## 4. Total context, not individually small chunks

The relevant budget includes summaries, state, knowledge, evidence excerpts, metadata, source handles, tool wrappers and any retained trace included in the consumer's prompt. Measure the entire assembled product, including consumer-side overhead where observable.

Do not claim efficiency because one summary is small while appending a full trace beside it. The supplied V15 finding specifically concerns this combined-cost failure; its trace growth and aggressive initial clipping were lab errors, not demonstrated Mémoire behavior. [A1, lines 80–85](../sources/README.md#a1)

Use diversity and deduplication so repeated versions of an old decision do not consume all available slots. Similar wording alone does not establish equivalence: differences in scope, time or contradiction can be decisive.

## 5. Progressive disclosure includes replacement

Start with a compact purpose-shaped representation and expose precise expansion paths. Expansions can recover exact source detail and bounded ordered groups: command, result, correction and later verification.

Do not keep appending refreshed state forever. A newer view can replace the older representation it supersedes, while the evidence remains in the archive. Active constraints deserve protection because they remain applicable, not because their original slots are permanently reserved.

Consumer capabilities matter. Some harnesses cannot remove earlier tool messages until compaction. In that case, logical supersession is not physical token removal. Report actual accumulated context and apply replacement at the next supported assembly/compaction boundary. Never count a logical “retired” marker as reclaimed tokens when the bytes remain in the prompt.

When essential applicable constraints exceed budget, expose insufficiency and request a larger budget or a narrower task scope. Silent clipping must not decide which governing constraint the agent forgets.

## 6. Stable references and precise expansion

The service issues source handles; models do not invent them. A handle should resolve to the intended evidence or version, subject to access control and retention. An expansion response identifies the exact range supplied and whether more remains.

Unknown, inaccessible, deliberately deleted and temporarily unavailable sources require meaningful distinct outcomes where security allows that distinction. Error responses must not reveal protected content or fabricate a replacement excerpt. The consumer can recover by using returned alternatives or rerunning a supported query.

Same-run sibling pointers can help navigate related evidence, but do not prove causality, correction or resolution. The addendum says existing documentation already describes provenance/review metadata and derivation-run sibling pointers; inspect and reuse those contracts where applicable. [A1, line 99](../sources/README.md#a1)

## 7. Freshness and coverage are observable

| Outcome | Meaning |
|---|---|
| No matching evidence in the searched scope | Search completed over the stated available coverage; no supported match was found. |
| Relevant source unavailable | The referenced evidence could not be supplied; state the permitted reason or recovery path. |
| Budget clipping | Material was omitted to meet the response budget; expose its category or expansion route. |
| Derivation/index lag | Received evidence exists beyond what the chosen view/search can currently represent. |
| Capture uncertainty | The system does not know whether all relevant harness activity reached it. |

An approved understanding can be stale. Return a relevant newer challenge rather than hiding it until the next interview. A current-state answer can also be incomplete because of missing results; first-class treatment does not justify pretending the missing evidence exists.

## 8. Consumer obligations

Consumers supply or establish task identity and purpose; use returned handles; expand evidence when a consequential conclusion depends on omitted detail; preserve qualifications; and distinguish memory content from active instructions.

They should request updates at meaningful boundaries such as resumption, compaction, a new task assignment or a decision requiring current evidence. Exact triggering is an experiment. A harness should not require a model to remember an undocumented ritual before any retrieval occurs.

The consumer must not turn “reported complete” into “verified,” old authorization into new permission, or one reproduced failure into an explanation of every failure in a log. Relevant retrieval cannot compensate for those interpretation errors.

## 9. Evaluation boundaries

Test the service and consumer independently, then together. Service tests cover scope, budgets, source resolution, omissions and status honesty. Consumer tests cover triggering, valid handles, qualification, total context and correct next action.

Evaluate both peak context and cumulative continuation input. A smaller peak can still require many costly retrieval turns; fewer turns can still overwhelm context. Decision quality is evaluated alongside cost, not replaced by it.

The foundational pipeline increment should reuse existing inspection/search/expansion for fixed task probes rather than replace ranking. Generalized lifecycle-aware reads follow observation/state and consolidation work under the repository's compatibility policy. When the later meeting increment ships, it consumes those scoped, source-linked products and still reports coverage; it must not substitute fresh transcript interpretation for missing upstream contracts.
