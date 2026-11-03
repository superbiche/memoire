# 10 — Acceptance scenarios and traceability

**Status:** proposed acceptance catalogue; cases are specifications, not passing test results.  
**Basis:** [C1](../sources/README.md#c1), [A1](../sources/README.md#a1). Source IDs: [register](../sources/README.md).

## 1. How to use this catalogue

Turn relevant cases into repository-native fixtures and tests as the owning capability ships. Do not require the first agent to implement every future feature just to make the entire catalogue pass.

**D** means deterministic or mechanically assertable with a controlled fixture. **S** means semantic judgment is required. **L** means live use is needed. Several cases require more than one kind. Model evaluators can assist S cases, but their verdicts are labeled assessments.

The first-increment subset is identified in the handoff. Missing original experiment artifacts must not be invented; newly authored fixtures are labeled as such.

## 2. Evidence, meaning and view separation

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

## 3. Identity, time and concurrency

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

## 4. Reads and consumers

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

## 5. Meetings, publication and authority

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

## 6. Feedback, learning and safe operation

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

## 7. Supplied experimental observations

The following are reported by the supplied addendum, not independently reproduced in this work:

| Reported case | Exact limitation to preserve | Related cases |
|---|---|---|
| V11 | Critical preservation constraint lost while three automatic hits concerned review procedures. | AC-24 |
| V13 | All six protected slots filled at the first checkpoint; later constraints never entered. | AC-25 |
| V7 | Selected passages recovered facts, but all three repeats generalized one demonstrated cause to a 33-failure log. | AC-29 |
| V15 cost/criteria | Ten completed pairs used 2.24× baseline continuation input and met 27/40 criteria versus 30/40. | AC-26, AC-30 |
| V15 consumer | Retrieval was never requested and a generated nonexistent source reference later caused an abort. | AC-27, AC-28 |
| Narrow live search | Five near-identical lapsed observations illustrated duplicate budget consumption. | AC-24 |

The cases were small and repeatedly studied; factual scoring came from the coordinating model. Independent reviewers did not inspect the private answers. V15 trace growth and initial-prior clipping were lab-design errors. No candidate described in the addendum was promoted. Original inputs, checkpoints, complete outputs and scoring artifacts are needed to reproduce those results. [A1, lines 71–85](../sources/README.md#a1)

## 8. Requirement-to-evidence map

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

## 9. When to say it works

A capability works at its claimed scope when its relevant deterministic contracts pass, semantic evaluation preserves the required distinctions, operational behavior is observable and reversible, and live use supports the intended benefit where applicable.

Always state the tested scope, sample size, remaining failures and unmeasured aspects. This catalogue is not a claim that fifty passing unit tests alone establish a useful memory system. The whole-system goal requires representative operator use and sustained evidence that lower burden did not come from missed significance.
