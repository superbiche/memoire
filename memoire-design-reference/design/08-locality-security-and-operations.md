# 08 — Locality, security and operational boundaries

**Status:** preserve existing repository contracts; refinements below are required boundaries or proposed safeguards, not an audit of current enforcement.  
**Owns:** DR-12 and safe operation across all paths.  
**Basis:** [C1](../sources/README.md#c1); repository checks [R1–R3]. Sources: [register](../sources/README.md).

## 1. Local-only is an end-to-end property

The preferred target is local-only operation using the operator's own infrastructure. Local-first is an explicit fallback policy, not permission for automatic external escalation.

Check every processing stage: extraction, consolidation, meeting conversation, embeddings, reranking, diagnostics, tracing and exported evaluation fixtures. A local database with a remote model is not local-only inference. A locally generated memory snapshot injected into a cloud-backed coding harness is an external disclosure of that snapshot.

“Local” can include operator-controlled machines or LAN infrastructure under the declared boundary; it does not mean every model must run on the same workstation. Exact hosts and trust boundaries come from deployment configuration, not this reference.

The public quickstart explicitly distinguishes local from external derivation and requires per-invocation external allowance. Extend the relevant boundary rather than inventing a silent fallback mechanism. It also documents the API as the sole store writer. Both must be checked in the source checkout before new workers are wired. [R2](../sources/README.md#r2)

## 2. Explicit modes and exports

In local-only mode, unavailable local inference produces a visible failure or backlog while capture continues. Do not retry through a remote provider. Model downloads, external documentation retrieval and operational memory transfer are different actions; none should silently broaden the authorized data boundary.

In a separately enabled local-first mode, an authorized external operation identifies its purpose, selected material, endpoint and redaction policy. Existing repository consent requirements govern. Do not substitute a global “cloud allowed” switch for a stronger existing per-run boundary without an explicit ruling.

Memory reads to an external harness need their own policy. The system cannot honestly call the full workflow local-only merely because derivation stayed local. Record the actual export scope where supported.

## 3. Captured content is data, not instructions

Tool output, source files, retrieved memories and earlier agent messages can contain instruction-like text. Derivation, consolidation and meeting preparation must treat them as evidence, not commands that alter the worker's rules or permissions.

An evidence item saying “mark this accepted” cannot grant publication authority. A model cannot create an authenticated operator decision by generating a line that resembles one. Use existing principal and write authorization rather than trusting role labels inside arbitrary captured text.

Preserve the distinction between an approved explanation, an applicable standing rule and a current authorized action. Retrieval must not reactivate old permissions without checking their scope and applicability.

## 4. Access and sensitive content

Derived representations must not broaden access beyond their source policy. Cross-task or cross-session linking is not a grant to read another project's restricted evidence. Topic scoping and authorization scoping must remain distinct.

Avoid logging secrets, full prompts, personal content or unbounded identifiers in public metrics. Evaluation fixtures and exports can be as sensitive as original sessions. Sanitize fixtures deliberately and record when sanitization changes the semantics relevant to a test.

The first increment should reuse existing authentication, redaction and custody controls. New meeting, feedback and publication surfaces need the same scrutiny as existing memory surfaces, not a weaker “internal tool” exemption.

## 5. Retention, correction and deletion

Relevance pruning removes material from an active view; it does not automatically delete evidence. Deliberate purge, quarantine or redaction follows the repository's policy. Do not introduce automatic raw-evidence deletion as a side effect of condensation or context-budget management.

When a source is unavailable or deliberately deleted, source handles should return the permitted status rather than stale content. Dependent summaries, search caches, exports and replay records require review under the retention policy. A hidden derivative must not restore content the operator deliberately removed.

Version history and tombstones can preserve the fact of correction or deletion without retaining disallowed content. Exact deletion propagation and retention durations are operational choices to inspect and test, not invented legal rules in this document.

## 6. Failure behavior

Capture remains independent of slow inference and absent human review. Derived work uses bounded retries, idempotency and visible failure states. A successful empty batch is not the same as an enumeration failure, refusal or truncated response.

A read with incomplete coverage must identify that limitation. A meeting prepared over a failed source range must not conclude that nothing material changed without qualification. Newer evidence should not silently mutate an already presented packet.

Resource scheduling should protect active use from consolidation contention. Measure queue and read/meeting latency under realistic concurrent load. Do not claim exact GPU attribution from process elapsed time on shared hardware.

## 7. Migration and compatibility

This is an additive redesign. Inspect existing lifecycle, read, injection, state/knowledge, storage and API contracts before adding new objects or changing eligibility.

Current proposed observations becoming first-class for scoped work reads is a **new behavioral interpretation**, not permission to globally mark all proposed rows accepted. Preserve human publication boundaries for governing understanding. Introduce new read behavior through the repository's versioning or feature-gate mechanism and test it explicitly.

Initially keep production injection and published understanding unchanged while exercising candidate normalization, derivation and later lifecycle/consolidation products in shadow or opt-in use. Preserve the distinction between candidate artifacts and active production outputs; a replay must not create duplicate production observations. Promote a tested path deliberately under the source repository's normal policy.

Shadow does not mean current observations are semantically second-class or unavailable to an opt-in current-work read. It means experiments cannot silently change production authority or context policy. Meeting products join the same controlled path when their prerequisites are exercised.

The public README describes storage/lifecycle guarantees across SQLite and PostgreSQL, not identical retrieval features. Do not impose a graph or vector dependency on the intentional SQLite full-text-search lane. Inspect actual source contracts and test the touched supported engines. [R1](../sources/README.md#r1)

## 8. Operational minimum for the first agent

Use isolated local fixtures or a read-only/replay-safe evidence source. Follow the existing writer boundary. Disable new behavior cleanly without losing evidence. Keep configuration versions, source references and baseline comparisons. Do not deploy, bulk-rederive, purge, migrate production data or enable external processing merely because the design is approved.

Approval of the design establishes direction. It is not blanket authorization for destructive operations or production cutover.
