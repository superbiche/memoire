# D183 kill criteria — RATIFIED 2026-08-02

Status: RATIFIED (operator, 2026-08-02: thresholds as proposed; query
#11 measured but unscored; durable home docs/research/vectors — this
directory). Journaled as the dated 2026-08-02 amendment to D183. The
D183 start condition is SATISFIED; the pilot may begin.

The reference query set and the baseline/comparison harness in this
directory are first-class artifacts of the vectors lane (D183 permanent
deliverables), independent of the pilot's outcome.

## 1. Reference query set — 29 REAL queries, mined, frozen

Source: every `mcp__memoire__memoire_search` call captured on the hub
(PostToolUse payloads carry `tool_input.q` + project + limit — the D190
minability path, used as designed). Window 2026-07-24 → 2026-08-02,
8 source projects; 24 of 29 issued after the pull-retrieval rule went
live (07-29). Full set with metadata: `reference-queries.tsv` in this
directory (frozen). Raw mining dump: `tmp/d183-kill-criteria/`.
No synthetic queries were added; 29 sits inside the ruled 20-40 band.

Category coverage vs the D183 families:

| category | n | ground truth |
|---|---|---|
| recent-work recall | 10 | blind |
| cold-start context rebuild (safe-gate fan) | 5 | blind |
| cross-project lookup | 4 | 1 derivable (carries a ticket id), 3 blind |
| exact-id (ticket/task number) | 5 | derivable |
| temporal (July retrospective) | 3 | blind |
| decision-recall | 1 | derivable |
| instruction-mining | 1 | measured, UNSCORED (operator-ruled: run it for learning, exclude from all pass/kill arithmetic) |

Two genres the mining surfaced that the D183 text did not name, both
kept in the set deliberately:

- **Exact-id lookups** (US-9248, "#54"/"tâche 54"): FTS is structurally
  strong here, embeddings structurally weak — the sharpest discriminator
  in the set, and ground truth IS derivable (an observation is a hit iff
  it references the ticket).
- **Syntax-bearing queries** (8 of 29 use OR / NOT / NEAR / quoted
  phrases): real sessions speak FTS query language. Policy proposed in
  §3: the hybrid engine receives the raw string; a query it cannot
  execute counts as a LOSS for hybrid on that comparison. Engines are
  judged on the query language users actually emit, not on a sanitized
  paraphrase.

The named **cold-start post-vacances scenario** stays as D183 defines it
(blind side-by-side at resumption week, operator-judged) — it is a
scenario, not a mined query, and is not double-counted here.

## 2. FTS latency baseline — MEASURED

Harness: `fts-baseline.sh` — each of the 29 queries verbatim (project +
limit as issued) against `GET /v1/search` on
`https://memoire.superbiche.me` (the real client path: TLS, envoy,
tailnet), 1 warm-up + 10 timed iterations per query, 290/290 HTTP 200.
Raw timings archived: `tmp/d183-kill-criteria/fts-baseline-timings.tsv`.

- **p50 209 ms · p90 263 ms · p95 300 ms · p99 339 ms · max 439 ms**
- Slowest query by mean: #3 (long SecureHoliday recall string, 308 ms
  mean / 439 ms max); no query's worst call exceeded 439 ms.

## 3. Proposed thresholds (the sign-off)

1. **Recall@10 floor (kill if violated):** on the 7 ground-truth
   queries, hybrid recall@10 >= FTS recall@10, aggregated over the set
   (no per-query cherry-picking; a tie is a pass).
2. **Blind preference (kill if violated):** on the 21 scored blind
   queries (#11 runs but never counts),
   side-by-side top-10 lists, source-blinded, operator-judged: hybrid
   must win >= 60% of comparisons where the operator states a
   preference (ties/abstentions excluded from the denominator).
3. **Latency (kill if violated):** hybrid p95 <= 600 ms — exactly
   2 x the measured FTS p95 (300 ms), same client path, same harness,
   same query set, warm.
4. **Syntax honesty:** raw query strings go to both engines; an
   execution error or empty-because-unparsed result on the hybrid side
   scores as a hybrid loss for that query. (FTS parse errors, if any,
   score symmetrically.)
5. **Operator-attention ceiling (kill, unchanged from D183):** more
   than one engine/daemon incident requiring operator attention per
   active week.
6. **Cold-start post-vacances (unchanged from D183):** blind
   side-by-side at resumption week, operator-judged; vacation freeze
   clause applies; verdict at the fixed date; outcomes remain exactly
   (a) port / (b) adopt / (c) kill — no extension.

## 4. Measurement protocol (so numbers are comparable)

- The comparison harness re-runs THIS query set verbatim against both
  read surfaces from this machine; latency always client-measured over
  the same transport.
- The reference set is FROZEN for the pilot. Queries issued during the
  pilot window accrue to a candidate v2 set for the vectors lane, but
  do not enter scoring mid-flight.
- Recall ground truth for the 7 derivable queries is fixed BEFORE the
  pilot runs (observation ids listed per query, derived by literal
  ticket-reference match, spot-checked by the operator).
