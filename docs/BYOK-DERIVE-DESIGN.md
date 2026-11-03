# BYOK derivation — standalone provider design (v0.7)

Status: RATIFIED (operator, 2026-08-05): Q1 explicit-only dialect
selection — agreed; Q2 required-explicit data boundary, no default —
agreed; Q3 — GO. Plus one in-build ruling: the store gate turned out to
be an inline CHECK constraint (both engines), so §4's "extended
additively" was corrected to a ruled STRICT-WIDENING carve-out to the
additive-only schema law (schema/README.md; SQLite rebuild pattern in
`schema/0016_byok_provider.sql`, PG constraint replacement in
`schema-pg/0005_byok_provider.sql`). Ruling of record for the design
direction: docs/LANE-MATRIX-BYOK-HANDOFF-2026-08-04.md §3.

## 1. What exists, and what the port actually is

Memoire's derive lane already has a provider seam
(`services/api/internal/modelderive/provider.go`): `Provider`
{`Name`, `DataBoundary`, `Generate(ctx, Request) → ProviderResponse`}, a
registry, and an orchestrator that owns prompt build, strict-JSON
parsing, refusal/usage accounting, and every store write. The only
model-backed adapter is `local-qwen` (`qwen_adapter.go`): an
**allowlisted-endpoint registry** speaking **OpenAI-compatible
chat-completions** (`/v1/chat/completions` + `/v1/models` availability
probe), bearer key by env-var name (D73 `EnvLookup` posture), ctx-owned
timeouts, and an error taxonomy the orchestrator already consumes.

Consequence for the ratified "port the provider seam from Reveille
Conductor" direction — refined, not contradicted:

- **OpenAI-compatible v1 dialect: the production Go source is in-repo.**
  The BYOK OpenAI dialect generalizes `qwen_adapter.go`'s
  request/response/usage/refusal code (extracted, not duplicated) rather
  than porting TypeScript that says the same thing. Conductor is not the
  best quarry for a dialect Memoire already speaks in production against
  llama.cpp/vLLM.
- **Anthropic dialect: genuinely new — Conductor is the quarry.**
  Port-not-adopt from `reveille/server/conductor/`: request/response
  shapes and header discipline (`x-api-key`, `anthropic-version`) from
  `http.ts`/`adapters.ts`/`types.ts`, and the provider error-envelope
  taxonomy (`anthropicError` mapping in `http.ts`/`routes.ts`).
  Provenance stated in source comments (internal port, files cited).
- **Explicitly NOT ported** (per the ruling): WOL, prewarm, load/reap,
  scheduler, endpoint registry, proxy surfaces. The BYOK client knows
  one URL, one key, one model — config, nothing else.

## 2. Provider shape

One new provider, `byok`, in `modelderive`, with the dialect as an
internal transport selector — not two providers:

- `Name() = "byok"`. `derivation_runs.model` records the user's model
  name (participates in idempotency, existing rule); `endpoint_id`
  records `"byok"` (or the optional user label, §3); the dialect is
  recorded in run metadata. Rationale: the dialect is transport, not
  model identity — the same model reached over either dialect is the
  same logical derivation, exactly the D-established `endpoint_id`
  stance (audit metadata, not idempotency).
- The **closed set is enforced in code**: the dialect selector accepts
  exactly `openai-v1` and `anthropic`; anything else is a refusal
  naming the closed set and the ruling. Extension is an operator ruling,
  never a PR (D175 closed-set family — journaled in D232/handoff §3).
- Orchestrator contract unchanged: `Generate` returns raw JSON +
  usage + refusal; parsing, validation, provenance, and writes stay
  orchestrator-owned. The boundary law (D231) is untouched — this is a
  Memoire service talking to a user endpoint, no harness involved.

## 3. Config surface

Env-first, with matching `memoire-derive` flags overriding env
(existing convention). Nothing rides a repo config file; the key is
never in argv, never logged (constraint family of record).

| Setting | Env | Notes |
|---|---|---|
| Base URL | `MEMOIRE_BYOK_BASE_URL` | required |
| Model | `MEMOIRE_BYOK_MODEL` | required; recorded in provenance |
| Dialect | `MEMOIRE_BYOK_DIALECT` | required, `openai-v1` \| `anthropic` — **explicit only, no sniffing** (§6 Q1) |
| API key | `MEMOIRE_BYOK_API_KEY_ENV` (name of the env var holding the key) or `MEMOIRE_BYOK_API_KEY_FILE` | one of the two; file must be non-world-readable; empty key allowed only for keyless local endpoints |
| Data boundary | `MEMOIRE_BYOK_DATA_BOUNDARY` | required, `local` \| `external` — no default (§6 Q2) |
| Endpoint label | `MEMOIRE_BYOK_ENDPOINT_ID` | optional, default `byok`; audit metadata only |

## 4. Data boundary and the store gate

`docs/MODEL-PROVIDER.md` §Data Boundary Discipline: external providers
require explicit per-run operator opt-in, and the store rejects
provider/data-boundary mismatches. BYOK cannot infer its boundary — the
same dialect reaches `localhost:8080` and `api.anthropic.com` — so the
user declares it (§3), and:

- `data_boundary=external` requires the existing explicit per-run
  opt-in gesture on `memoire-derive` (the flag the `openai` path
  reserved; the BYOK slice builds it if it is still notional).
- `data_boundary=local` behaves as `local-qwen` does today.
- The store-side mismatch check is extended to admit `byok` with either
  declared boundary. CORRECTED AT BUILD (2026-08-05): the gate is an
  inline CHECK constraint, not additive-extendable — widened under the
  operator-ruled strict-widening carve-out (schema/README.md), realized
  as a SQLite table rebuild (0016) and a PG constraint replacement
  (schema-pg/0005), with a data-preservation regression test
  (`internal/db/byok_migration_test.go`).

## 5. Acceptance (release-blocking, per the ruling)

- **Contract tests** in `modelderive` (default `make test` path, public
  core): per-dialect `httptest` stubs — request-shape assertions
  (headers included: bearer vs `x-api-key`+`anthropic-version`),
  response parsing, usage extraction, refusal mapping, error taxonomy,
  timeout honored, closed-set refusal for an unknown dialect, key
  never appearing in logs or error strings.
- **Smoke**: extend `tests/e2e/standalone-sqlite-smoke.sh` (verify, do
  not duplicate — reuse its store/session fixture): capture → BYOK
  derive `-execute` against a local stub speaking each dialect in turn →
  proposed rows land with `provider=byok`, the stub's model name, the
  declared boundary, correct dialect metadata. No network, no real keys.
  Stub mechanism chosen at build (small Go helper under `tests/`,
  started/stopped by the smoke).
- These two artifacts are what turns the README matrix cell from
  MISSING #9 pointer into a claim.

## 6. Ruling questions

1. **Dialect selection — explicit-only?** Proposed: `MEMOIRE_BYOK_DIALECT`
   is required, no auto-detect ever. The handoff allows "auto-detect if
   unambiguous"; nothing about a bare URL is unambiguous, and "explicit
   beats clever" is the ratified tiebreak. Confirm explicit-only.
2. **Data boundary — required-explicit or default-external?** Proposed:
   required, no default (refuse loudly when absent). Default-external
   would be the safe guess but taxes every localhost user with opt-in
   friction and makes the declaration invisible; a required knob makes
   the user state where their data goes — once, in config.
3. **GO/sequencing:** proposal holds within the stabilization-week
   budget as one reviewed slice (ROUTINE→DEEP call made at review time;
   it touches a store gate, so likely DEEP). Build now, or hold behind
   the soak priorities and take the handoff §4 escape valve at tag time?
