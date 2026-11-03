# PORT slice design — native hybrid lane (D217 outcome)

Status: RATIFIED (operator, 2026-08-02 — journaled as D218). Open
questions resolved: (1) embed runtime = node + @huggingface/transformers
container; (2) hybrid flips ON for the MCP surface immediately after the
acceptance gate passes; (3) freshness look-line = oldest missing > 2h
(8 missed runs at the 15-min cadence — systemic, never a blip).
Origin: D183 pilot verdict (D216 measurements, D217 PORT ruling).
The design goal in one line: capture the panel-demonstrated ranking win
(71% blind preference) without giving up anything that made FTS win the
recall floor — full corpus, project scoping, exact-id dominance.

## Recon facts this design stands on (verified 2026-08-02)

- **pgvector 0.8.2 is already installed** on the hub CNPG cluster
  (`pg_available_extensions`: installed alongside pg_search 0.24.3).
  Zero extension/infra work.
- `schema/0002_observation_embeddings.sql` is an inert Phase-2 sketch
  (SQLite-shaped, "Not applied by v0.1"); the table does not exist on
  the hub and no Go code references it. It stays untouched (additive
  law) — the PG lane gets its own provisioned shape.
- `services/embed-worker/` is the declared Phase-2 placeholder — this
  slice fills it.
- House precedent D160 (pg_search): capability detection at `Open` via
  `pg_extension`, owner-privileged idempotent provisioning
  (`ProvisionPGSearchIndex`), graceful fallback when absent. The vector
  lane mirrors this exactly; **no shared-chain migration is needed**,
  and the SQLite standalone engine is untouched (no SQLite vector lane
  — same posture as pg_search).

## Architecture — three parts

### 1. Schema (PG-only, provisioned, additive)

`ProvisionVectorSchema` at Open (gated on `HasPGVector`), idempotent:

```sql
CREATE TABLE IF NOT EXISTS observation_embeddings_pg (
  observation_id   BIGINT PRIMARY KEY
                   REFERENCES observations(id) ON DELETE CASCADE,
  model            TEXT   NOT NULL,
  dim              INT    NOT NULL,
  embedding        vector(384) NOT NULL,
  text_hash        TEXT   NOT NULL,
  embedded_at_epoch BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_obs_emb_hnsw
  ON observation_embeddings_pg
  USING hnsw (embedding vector_cosine_ops);
```

- Distinct name (`_pg`) so the inert SQLite sketch never collides.
- `ON DELETE CASCADE` rides soft-delete purge semantics for free when
  purge/rebuild lands; visibility filtering happens at query time by
  joining `observations` with the standard `observationWhere`.
- `model` + `text_hash` make re-embeds (model upgrades, text edits via
  rewrite) detectable: a row is CURRENT iff model matches the active
  model AND text_hash matches the observation text.

### 2. Embed worker (`services/embed-worker`, hub-only container)

Model: `Xenova/all-MiniLM-L6-v2` q8 ONNX, 384 dims — pilot-proven
(quality demonstrated by the panel; ~9-core full-corpus embed of 58k
rows took ~25 min once; incremental loads are trivial).

Two duties, one binary/container:

- **Freshness loop (the anti-silent-death design, D217's constraint):**
  no queue, no fire-and-forget, no event subscription. The worker's
  entire state question is a SQL query: "which non-deleted, kind != raw
  observations lack a CURRENT embedding?" It embeds them in chunks,
  upserts, repeats. Run as a frequent CronJob (interval an operator
  knob, proposed 15 min) — crash-safe and self-healing by construction,
  because missed work is always re-discovered from the table itself.
  The pilot's worst failure mode (silent vector-lane death with no
  backfill path) is structurally impossible here.
- **Query-embedding endpoint:** `POST /embed {"text"} -> {"vector"}`
  on a cluster-local service, used by the API at search time
  (~10-20 ms for q8 MiniLM). Token-guarded like other internal seams.

Freshness is OBSERVABLE: the API's health/metrics surface exposes
`embed_missing_count` and `embed_oldest_missing_age_s` (same SQL the
worker runs); RUNBOOK gets the query + an attention threshold
(proposal: oldest missing > 2h = look).

### 3. Hybrid read path (fusion in the existing search surface)

In `SearchWithFilter`, when `HasPGVector` AND the embed service answers
AND the hybrid flag is on:

1. FTS lane exactly as today (pg_search BM25, full corpus incl. raw,
   all `observationWhere` filters) — top K=50.
2. Vector lane: embed the raw query string (syntax honesty carries
   over; operators embed harmlessly), `ORDER BY embedding <=> $q`
   top K=50, **joined against `observations` with the SAME
   `observationWhere`** — project scoping, review-state, deleted, and
   noise filters apply identically (closes the pilot's scoping gap).
3. RRF fusion (k=60, the piloted constant), take `limit`.

Failure posture: embed service unreachable or over a ~150 ms budget →
FTS-only result, counted and logged (fail-open, observable — never a
search outage). Flag default OFF until the acceptance gate passes.

Vector lane stays derived-only (kind != raw): raw rows keep reaching
results through the FTS lane, which is exactly the division of labor
the D216 genre analysis measured. Raw embeddings are a non-goal.

## Acceptance gate (pre-built, D215/D216)

Re-run the D183 harness verbatim against the hub over the real client
path, FTS vs hybrid-flag-on:

- Criterion 1: hybrid recall@10 >= FTS aggregate on the fixed GT
  (`ground-truth-2026-08-02.tsv`) — must now PASS (full corpus + raw
  via FTS lane + project scoping should close the gap; if it does not,
  the port does not ship).
- Criterion 2: blind preference >= 60% must HOLD — fresh blinded pack
  via `build-pack.mjs`, judged by the D216 panel protocol (clean-context
  panel + orthogonal ballot).
- Criterion 3: p95 <= 600 ms over the real path (TLS/envoy/tailnet),
  query embedding included.

## Slice plan (2 sessions, orthogonal review per fleet law — schema +
search semantics are load-bearing)

1. **Session A:** `HasPGVector` + `ProvisionVectorSchema`; embed worker
   (freshness loop + endpoint + container + CronJob in cdk8s); backfill
   runs; freshness metrics + RUNBOOK. Deliverable: 57k+ current
   embeddings on the hub, lag observable at ~0.
2. **Session B:** fusion in `SearchWithFilter` behind the flag; harness
   re-run = acceptance evidence; review; flag-on ruling for the MCP
   surface; deploy per L6.

## Non-goals (this slice)

- No raw-row embeddings; no SQLite vector lane (D160 posture).
- No further agentmemory pillaging (lessons, consolidation, graph) —
  future quarry runs, each with its own kill criteria per D217 doctrine.
- No new external services beyond the embed worker container; model
  files ship in the image (no runtime HF downloads on the hub).

## Open questions for ratification

1. **Embed runtime:** node + @huggingface/transformers container
   (pilot-proven stack, fastest to land; hub-only so the standalone
   single-binary contract is untouched) vs Go + ONNX via cgo (one
   language, heavier toolchain risk). Proposal: node container.
2. **Post-acceptance default:** flip hybrid ON for the MCP search
   surface immediately after the gate passes, or run a flag-on
   observation window first?
3. **Freshness attention threshold:** oldest-missing > 2h as the
   RUNBOOK "look" line — tighter/looser?
