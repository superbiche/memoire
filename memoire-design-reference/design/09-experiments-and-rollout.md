# 09 — Experiments, rollout and promotion

**Status:** upstream-first direction requested by the operator; stage boundaries, mechanisms and thresholds below are proposed starting points.  
**Basis:** [C1](../sources/README.md#c1), including the edition 1.1 sequencing correction; [A1, lines 71–85, 112–118](../sources/README.md#a1). Sources: [register](../sources/README.md).

## 1. Improve the upstream product before adding the meeting consumer

Edition 1.0 made the meeting loop the first useful implementation and deferred selective derivation. That sequence is superseded. The operator's original problems are avoidable derivation work, noisy observations and unsustainable downstream review. A meeting built over substantially unchanged outputs would not address those problems first.

The first useful implementation is a measured, replayable evidence-to-observation path. It preserves capture custody and improves the model-facing input and extraction contract, rather than deleting source evidence or making the operator reconcile raw observations. Later increments make observations usable over time, maintain topic understanding and provide supported changes before a meeting consumes them.

This is dependency-driven development, not a requirement to finish an ideal engine before real use. Keep narrow downstream probes from the beginning: can a fixed task question be answered with the correct scope, sources and uncertainty? Use existing read/inspection surfaces and opt-in real workload. Do not build a meeting product merely to obtain those tests. An existing capability can satisfy a stage when inspection and evidence demonstrate it.

## 2. Staged path and exit evidence

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

## 3. First-agent scope versus the broader upstream program

The first agent implements a bounded A/B/C path: baseline instrumentation plus a concrete, locally runnable normalization/batching/extraction improvement and its regression evidence. Choose the relevant existing capture adapter and workload first; do not rewrite every harness or implement all later lifecycle stages in one assignment.

Provide the stable evidence links, work/session attribution, processing dispositions and run/version records that later lifecycle operations require. Do not invent the final ontology. Demonstrated existing lifecycle capabilities remain in use and receive targeted tests; a new full consolidation engine is not first-agent scope.

The deliverable is not a meeting UI, autonomous dream scheduler, replacement database or general model bake-off. A diagnostic report and replay command are sufficient interfaces. If the candidate fails a quality/cost gate, deliver the implemented opt-in path and honest results without promoting it or relabeling it an improvement. The bounded initial assignment must not become an indefinite requirement to find the optimum.

## 4. Experiment cards

### EXP-01 — Remove avoidable extraction work

**Hypothesis:** normalization, semantic batching and explicit abstention reduce repeated input and output narration without losing consequential claims.

Compare identical authorized evidence with the same model/template/settings. Isolate normalization and extraction-policy effects where practical; record bundled changes otherwise. Measure calls, input/output tokens, queue/processing duration, retries, important-claim retention and grounding. Sample suppressed/no-op material. Reject a gain that only reduces observation count while losing governing constraints, useful failures or current progress. Measure oldest unprocessed evidence during sustained work as well as offline throughput.

### EXP-02 — Non-thinking and smaller-model extraction

**Hypothesis:** bounded extraction may not require the current reasoning workload or model size.

Record the actual serving settings. Treat a thinking-mode change as its own experiment before adding model substitution. Compare smaller models after the same-model pipeline baseline. Keep downstream evaluation fixed where possible. Measure schema/truncation failures, source faithfulness, scope errors, processing cost and freshness. Do not promote from speed alone or infer local quality from another engine's cloud benchmark.

### EXP-03 — Observation lifecycle and incremental understanding

**Hypothesis:** scope-aware reconciliation and incremental topic updates reduce duplicates, stale state and unnecessary regeneration while preserving history.

Test observation lifecycle separately from understanding synthesis, then together. Include late evidence, repeated capture versus genuine recurrence, parallel task reviews, revision changes, corrected sources, reopened work and unchanged topics. Measure unsupported merges, missed contradictions, stale active claims, needless churn and total processing cost. Resolved work leaving the active view must not vanish from history.

### EXP-04 — Meeting materiality and grouping

**Hypothesis:** supported upstream changes grouped by project question produce fewer, more useful operator decisions.

Run once upstream products are inspectable. Use independently classified routine and structural-change cases; inspect hidden consequential matters as well as agenda relevance. Measure repeated-question burden and required reconciliation separately from elective design. Do not ask the meeting agent to hide faulty inputs with polished prose.

### EXP-05 — Progressive disclosure with replacement

**Hypothesis:** purpose-shaped reads and replacement preserve decisive evidence with lower complete context cost.

Use fixed task/decision probes from early pipeline work, then integrate the real consumer. Measure peak/cumulative input, retrieval turns, stale text retained, handle errors and correctness at the relevant scope. Count actual consumer input where visible, not only service payload. Do not attribute a harness failure to retrieval ranking. [A1](../sources/README.md#a1)

### EXP-06 — Identity enrichment

**Hypothesis:** ledger/coordinator relationships improve multi-agent continuity beyond session-local grouping.

Start with existing identifiers and safe fallback; richer adapters should not block capture cleanup. Compare real available identity sources without manufacturing missing metadata. Evaluate cross-task contamination, review targeting, namespace collisions, late linkage and unnecessary operator interruptions.

### EXP-07 — Feedback-driven improvement

**Hypothesis:** attributable changes motivated by operational failures or exact operator feedback improve targeted behavior without losing known strengths.

Begin with pipeline reports and optional scoped notes. When meetings ship, preserve per-meeting feedback as a first-class source. Investigate the failure stage before changing prompts; define expected benefit, run replay/held-out cases, then use live evidence. Maintain rollback. A note is optional; missing feedback is not success.

## 5. Comparison discipline

Record evidence checkpoint, code revision, normalization/batching/extraction versions, model, endpoint boundary, serving build, quantization, prompt/template/schema and relevant settings. Label unavailable settings and estimated measurements. Record cache state, concurrency and resource contention when they materially affect timing.

Use routine work, decisions, failures, rollbacks, parallel reviews, branch divergence, missing results, misleading assistant claims, operator corrections and no-op cases. Evaluate discarded material and positive cases. Do not use fewer observations, valid JSON or source-handle resolution as substitutes for semantic quality.

Time-faithful replay cannot see future evidence. Later-known truth can assess retrospective correctness, but is a separate evaluation. Respect retention/redaction and do not resurrect intentionally deleted material. Newly authored regression fixtures are not reproductions of the addendum's reported experiments.

## 6. Promotion gates

**Safety/integrity:** no demonstrated regression in source custody, redaction, authorization, scoped identity, reference stability or publication boundaries in tested cases. Failures block affected promotion even when speed improves.

**Semantic:** important outcomes, constraints, uncertainty and useful negative evidence survive. No unsupported upgrades from intention to completion, report to verification, patch to requirement or exploration to decision. State the rubric, assessor and limits.

**Cost/freshness:** compare complete work per bounded corpus and under representative arrival patterns. Include retries, pre/post-processing and work shifted to later stages. Do not exchange low extraction cost for unmeasured downstream re-derivation or unacceptable current-state delay.

**Operational:** explicit candidate/production separation, repository-approved migrations, retry/idempotency, resource controls and disablement. Test relevant supported storage lanes and consumers at the scope claimed.

**Usefulness at the delivered scope:** early pipeline changes need representative workload/replay and task-probe evidence; they do not require an interview. Conversational usefulness and sustainable reconciliation time require actual meetings once stage G ships.

Set workload-specific targets after baseline and before judging the candidate. A test gate is not a promise of zero future defects. Report failed cases and leave a candidate unpromoted when results are inconclusive. No fixed calendar or universal model threshold is established here.

## 7. Non-blocking choices and false milestones

Batch thresholds, topic granularity, detailed ranking, model allocation, interview cadence and exact representations remain empirical. Minimal identity and evidence contracts are needed early; full ledger integration and automatic semantic linking are not. Unresolved privacy, production retention, source-contract or destructive-migration permission blocks only the affected operation.

“Added a dreamer,” “produced fewer observations,” “all JSON validates” and “built an interview UI” are not sufficient milestones. Neither is a source-preserving archive with no demonstrated improvement in the data used by consumers.

**Initial progress:** a replayable local source-to-observation increment with measured noise/cost behavior, inspectable omissions and protected source/scope semantics. **Pre-meeting progress:** lifecycle-correct work accounts and evidence-backed supported changes over representative work. **Whole-system success:** actual meetings and task use show that better memory saves attention without hiding significance.
