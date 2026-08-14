# Embed Worker

The D218 vector-lane worker: one node image, two duties (design:
`docs/research/vectors/PORT-DESIGN.md`).

- `node src/index.mjs sweep` — freshness sweep (cdk8s CronJob, 15 min):
  `GET /v1/embeddings/missing` → embed (MiniLM q8, 384 dims, batched) →
  `PUT /v1/embeddings`, repeating until caught up or the deadline. No
  queue, no subscriptions: missed work is always re-discovered from the
  store, so silent embed death cannot strand rows.
- `node src/index.mjs serve` — cluster-local query-embedding service
  (cdk8s Deployment): `POST /embed {"text"}` → `{"model","dim","vector"}`,
  bearer-token guarded; `GET /healthz` for probes.

Model identity (`all-MiniLM-L6-v2-q8`, 384) must match
`services/api/internal/db/vector.go`. Model files are baked into the image
(`scripts/model-tool.mjs fetch` + offline `verify` at build); the runtime
is pinned offline — no HF fetches on the hub.

Env: `MEMOIRE_ADDR` (API, sweep mode), `MEMOIRE_TOKEN_FILE` or
`MEMOIRE_HOME` (bearer token), `MEMOIRE_EMBED_PORT` (default 37779),
`MEMOIRE_EMBED_BATCH` (500), `MEMOIRE_EMBED_DEADLINE_S` (3300),
`MEMOIRE_EMBED_MODEL_DIR`, `MEMOIRE_EMBED_LOCAL_ONLY`.
