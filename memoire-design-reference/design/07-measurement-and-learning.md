# 07 — Measurement and continuous learning

**Status:** first-class from the first usable increment; most thresholds are empirical.  
**Owns:** DR-11.  
**Basis:** [C1](../sources/README.md#c1); [A1, lines 71–85, 112–116](../sources/README.md#a1). Sources: [register](../sources/README.md).

## 1. Define how the system learns from use

The design establishes goals and behavioral boundaries. It does not presume that paper design can determine the optimum model, prompt, grouping policy, materiality detector or meeting format. Real use and tuning are necessary.

Evaluation therefore ships with every pipeline increment, before a meeting exists. A normalization/derivation evaluation records the input checkpoint, transformations, output claims, relevant versions, costs, errors and targeted quality assessments. The first instrumented artifact can be a local replay report; it does not require an interview UI.

When meetings ship, every meeting can have an evaluation record containing automatic measurements, links to the material actually used and optional operator feedback. Pipeline and meeting records should share traceable lineage without forcing all stages into one evaluation unit. Neither creates a per-observation operator approval requirement.

The improvement process must preserve useful behavior as well as diagnose failures. An attractive aggregate score is not a substitute for specific evidence about what helped, what failed and why a proposed change should address it.

## 2. The evaluation unit

Before meetings, evaluate a bounded source corpus, derivation run or lifecycle update. Link the selected evidence, capture/normalization dispositions, batch ranges, candidate observations, known coverage gaps, code/prompt/model/settings and compared baseline. Later stages reuse these links rather than reconstructing opaque input histories.

Useful foundational records include original and normalized input size, actual calls avoided or added, output tokens, queue and processing duration, retries, failed/truncated/empty responses, invalid references and duplicate-write attempts. Source-handle resolution is deterministic; whether a claim faithfully represents its source and whether a suppressed passage mattered are semantic assessments. Record assessor and limits rather than labeling those judgments deterministic truth.

For each compared corpus, report the cohort, important claims/constraints assessed, omissions, unsupported upgrades and retained examples of useful failure evidence. A smaller observation count is not a quality score. Measure batching delay and oldest unprocessed evidence under ongoing work, not just bulk replay throughput. A faster offline run with unacceptable current-state lag is not a clean improvement.

Optional exact notes can attach to a run, episode, observed failure or later meeting. Preserve positive and negative feedback without requiring the operator to inspect or grade every observation. Automated reports and targeted spot checks start immediately; the dedicated conversational feedback experience arrives with the meeting stage.

A meeting evaluation links to the meeting ID, project/topic scope, prepared packet and version, baseline understanding, evidence checkpoint, actual conversation, resulting outcomes and relevant processing configuration.

Use references to retained artifacts rather than copying the entire archive for each meeting. Preserve enough to reconstruct what the operator saw, what the agent had access to and what was omitted. Reproduction is limited by declared retention/deletion and by nondeterministic model generation; do not claim byte-identical replay merely because configuration was recorded.

The evaluation also identifies whether the meeting was completed, abandoned, partially conducted or not conducted. An abandoned meeting is not silently dropped from the denominator or counted as positive feedback.

## 3. Optional operator feedback

A sufficient initial prompt is:

> What helped, and what should be different next time?

The operator can answer freely, during the meeting, immediately afterward or after downstream work exposes a problem. No mandatory ratings, taxonomy selection or questionnaire are required.

Example: “Good reconstruction of the two parallel tasks. Too much implementation detail. The deployment-policy question helped, but the previous question was already answered yesterday.”

Preserve the exact text and its intended scope. The agent may propose tags or a diagnosis, but those are derived interpretations with their own provenance. The original note must remain available for correction. Multiple notes can attach to one meeting or to a specific item.

Record absent feedback as absent. Accepting a decision, ending the conversation or offering a polite acknowledgment does not establish satisfaction. Silence is not success.

Positive feedback deserves the same treatment: “That comparison helped me decide” describes behavior to preserve in later experiments. Improvements that eliminate a known annoyance must not casually remove a useful preparation pattern.

## 4. Direct observations, estimates and judgments

Every metric or assessment should state which category it belongs to and the source/version that produced it.

| Category | Examples | Required treatment |
|---|---|---|
| Directly recorded | Request duration, number of tool calls, provider-reported token usage, explicit recorded decisions. | Preserve definition, units, scope, missingness and relevant IDs. |
| Computed from direct records | Cumulative input tokens, queue duration, age of deferred proposals. | Preserve formula and denominator; do not count duplicate instrumentation twice. |
| Estimated | Locally estimated token count, approximate active attention, inferred reconciliation segment. | Label estimate and method; do not present as exact. |
| Human judgment | Useful question, unnecessary detail, missed requirement, misleading account. | Preserve exact feedback and attributed evaluation. |
| Model judgment | Proposed materiality, faithfulness score, inferred cause of complaint. | Label assessor/model/version and uncertainty; not ground truth. |

A schema-valid output is measurable. Factual correctness is not established by schema validity. An accepted interpretation is measurable. It is not necessarily objectively correct.

## 5. Initial metric dictionary

### Processing and locality

| Metric | Definition and caution |
|---|---|
| Queue wait | Worker start minus queued-at time, using compatible clocks. Distinguish missing timestamps. |
| Preparation latency | Packet-ready minus preparation-request time for the specific version. Separate retries and waiting. |
| Stage duration | Observed duration of normalization, extraction, retrieval, consolidation and packet preparation where instrumented. |
| Model work | Calls, model IDs, generated/reasoning tokens where exposed, input/cached tokens, retries, refusals and truncations. |
| Total inference workload | Sum attributable call durations or tokens with named scope. Concurrent durations are not elapsed wall time or exact GPU occupancy. |
| Locality outcome | Selected boundary, endpoint classification, blocked external attempts and actual allowed external calls. No prompts or secrets in public metric labels. |
| Backlog | Pending/failed processing ranges and their ages, not merely a total queue length. |

### Retrieval and consumer context

| Metric | Definition and caution |
|---|---|
| Service payload size | Actual bytes/tokens returned under a named tokenizer or provider report, including provenance and handles. |
| Peak assembled context | Maximum observed consumer input/context over a continuation, including injected views and retained trace. Missing consumer visibility stays missing. |
| Cumulative continuation input | Sum of observed input tokens across continuation requests; state whether cached tokens are included. |
| Retrieval turns | Number of actual retrieval/expansion calls; zero is not evidence that retrieval worked. |
| Invalid-reference events | Resolution failures separated by unknown, inaccessible, unavailable or deleted as permitted. |
| Coverage | Included sources/ranges plus known capture, derivation and indexing gaps. Not a claim of complete real-world knowledge. |
| Duplication | Exact/referenced-origin duplicate contribution where detectable; semantic duplicate judgments remain separately labeled. |

### Meeting burden and outcomes

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

## 6. Denominators and missing values

Report sample counts and workload class with comparisons. A handful of meetings does not support confident tail-latency or universal usefulness claims. Percentiles may be descriptive with small samples but should not imply a stable distribution.

Unknown token usage is not zero. Failed preparation is not a zero-latency success. Meetings without feedback are not positively rated. A discarded batch must not disappear from coverage. A consumer that does not expose its complete context prevents a complete context-cost claim.

Avoid using accumulated agent-hours as the primary burden denominator: more parallel workers can make a ratio improve without reducing operator work. Record actual review minutes per work window and distinguish routine from structurally consequential work.

Do not fix all thresholds in advance. Establish an instrumented baseline, then set explicit comparison targets and tolerances before evaluating a candidate change.

## 7. Distinguish failure from legitimate change

A later reversal of a valid earlier decision is not automatically an earlier meeting error. Preserve the difference between new evidence, changed operator direction, corrected historical interpretation, and a presentation that was unhelpful despite factual accuracy.

A complaint such as “too much detail” can originate in extraction, topic grouping, agenda preparation, retrieval selection or conversational pacing. Do not automatically edit the meeting prompt before checking which stage produced the failure.

A useful diagnosis chain asks whether evidence existed, was captured, was available in the relevant scope, was retrieved, was included in the packet, and was interpreted and presented correctly. Missing capture cannot be fixed by a better recap prompt.

## 8. Traceable improvement loop

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

## 9. Replay, holdout and live use

Deterministic tests check properties such as valid references, identity preservation, version conflicts, explicit gaps and budgets. Replay examines bounded semantic behavior against retained inputs and expected distinctions. Live use establishes whether the meeting is natural, proportionate and useful to the operator.

Replay the evidence available at the original checkpoint. Do not give a candidate system future decisions and then claim it would have prepared the earlier meeting correctly. Separate before/after-current-knowledge reconstruction from time-faithful replay.

Keep some representative cases outside the examples used to tune a change. Include successful meetings so improvements preserve useful behavior. Record the assessor and rubric for semantic judgments; a model evaluator assists rather than certifies quality.

No exact source fixture for the reported addendum experiments was supplied. Their failure mechanisms can guide newly authored tests, but their scores cannot be independently reproduced from the addendum alone. [A1, lines 71–85](../sources/README.md#a1)

## 10. Evaluate omissions, not only visible questions

A calm, short meeting may be missing the difficult issue. Sample omitted/discarded material periodically and investigate delayed downstream errors. This is not a return to exhaustive observation review; it is targeted coverage checking.

Track unnecessary inclusions and consequential omissions together. Shorter meetings are not improvements when they hide pending decisions or cause an agent to miss a governing constraint. More accepted changes are not automatically better; routine work can rightly produce none.

Do not let the system grade its own materiality classifier by simply accepting its own routine/structural labels. Use independent fixture design or operator judgments for evaluation cohorts.

## 11. Maturity and reduced feedback burden

Early use should make feedback visible and easy after each meeting, while remaining optional. As behavior stabilizes across representative work, move toward sampled feedback, exception reporting and retrospective checks. A significant model, prompt, retrieval or lifecycle change can temporarily justify closer observation again.

Automatic measurements and traceability continue even when explicit feedback becomes less prominent. The goal is less operator evaluation work when it stops earning its cost, not a permanent quality-audit ritual.

Feedback belongs to Mémoire's improvement process. Apply it to the appropriate meeting, project, interface or configuration scope. Do not turn one complaint into an unreviewed global preference.

## 12. Minimum first-increment implementation

Persist or reliably link source checkpoint, input dispositions, normalization/batch/extraction versions, observations and references, basic timings and available usage, errors/gaps, baseline comparison and optional exact diagnostic notes. Supply a repository-native inspection/export and replay path. Include targeted tests for semantic distinctions, capture duplication versus recurrence, abstention versus failure, and omitted consequential material.

Meeting IDs, packets, conversations, outcomes and meeting-specific notes become additional evaluation records when that capability ships. They are not prerequisites for evaluating or shipping foundational improvements.

A new observability platform, dashboard or autonomous evaluator is not required. Existing local logs and storage can support the first loop if records are attributable, queryable and do not leak sensitive content.

Template: [meeting feedback](../templates/meeting-feedback.md). Improvement template: [experiment record](../templates/improvement-experiment.md).
