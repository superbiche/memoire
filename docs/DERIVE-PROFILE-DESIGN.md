# Derive profile — per-endpoint context geometry in the model registry

Status: RULED (operator, 2026-08-25) — design accepted; open questions resolved
(see "Open questions — ruled" below). Owner gesture: `derive-observations`.
Origin: the autopsy of the first derive-bench run
(`tmp/derive-bench/2026-08-24/autopsy-empty-content.md`), which traced 19
failed legs to one arithmetic bug and named the fix as per-endpoint data.

## The problem

The first derive-bench run (Qwen3.5-9B-Q8, 51 legs) lost the omission axis
almost entirely to a mechanical failure, not to model incapability. The
autopsy's universal statement across every failed leg: **generation room =
min(max_tokens, ctx − prompt_tokens) was smaller than the model's thinking
need.** 18 legs burned their budget mid-generation (17 ran until the 32,768
context was literally full, 1 stopped exactly at the 16,384 cap) and 1 was
rejected up front by llama-server as `request (32897 tokens) exceeds the
available context size (32768)`. Three symptom classes — empty content,
malformed-EOF JSON, HTTP 400 — one mechanism.

Two numbers produced it, and neither is per-model:

- The **byte-denominated split ceiling**, 70,000 bytes, shared by the
  production sweep (`scripts/memoire-derive-sweep.sh`,
  `MEMOIRE_SWEEP_MAX_PROMPT_BYTES`) and the bench driver
  (`bench/derive/run.py --max-prompt-bytes`). On real session content it
  measured ~2.3 bytes/token, so it admits prompts up to ~31k tokens — into a
  32,768-token context.
- The **fixed `max_tokens = 16384`**
  (`services/api/internal/modelderive/qwen_adapter.go`,
  `defaultMaxCompletionTokens`), sent regardless of the serving context. Its
  own comment already names the failure mode it was raised to avoid.

A byte ceiling shared across models with different tokenizers, contexts, and
thinking behavior is the latent bug class. The numbers must become
per-endpoint data. Prompt SIZE did not separate ok legs from failed ones
(medians 54.6KB vs 57.8KB, ranges fully overlapping) — the arithmetic did.

## The design: a derive profile

An **additive, optional `profile` object on each D75 registry entry**
(`model-endpoints.json`). The registry stays the config home (D56/D75), and
D53's Consequences explicitly bless endpoint-level options without changing
the Provider interface.

```json
{
  "endpoint_id": "hl3",
  "base_url": "https://…",
  "api_key_env": "HL3_QWEN_API_KEY",
  "models": ["hl3-qwen3.6-35b-a3b-iq3_xxs"],
  "profile": {
    "ctx_tokens": 32768,
    "max_completion_tokens": 16384,
    "bytes_per_token": 2.2,
    "margin_tokens": 512,
    "thinking": { "policy": "budget", "budget_tokens": 8192 }
  }
}
```

One derived quantity, computed at the CLI composition root
(`services/api/cmd/derive/main.go` — the D75 pattern, so `config` and
`modelderive` stay import-clean):

```
effective_max_prompt_bytes = floor((ctx_tokens − max_completion_tokens − margin_tokens) × bytes_per_token)
```

The composition root resolves the selected endpoint's profile once, before
wiring both the adapter config and the request limits.

The formula reproduces the autopsy's own remedy from data instead of from a
guess: (32768 − 16384 − 512) × 2.2 ≈ 34.9KB, inside the "lower the ceiling to
~36–40KB" range the autopsy derived by hand.

### 1. Bytes stay the transport unit

No tokenizer dependency enters memoire. `bytes_per_token` is measured or
declared data — the bench measured ~2.3 on real session content; declare
conservatively. The ceiling NUMBER becomes per-endpoint; the byte MECHANISM
is unchanged: `-max-prompt-bytes`, the adaptive window-split recursion, and
D212's headroom yielding all keep working exactly as they do today. The D245
outstanding surface's byte-classified aggregates are untouched.

### 2. No new gates

The existing `MaxPromptBytes` gate and the window-split recursion simply
receive an honest per-endpoint number. `MaxCompletionTokens` — already a
`QwenConfig` field with a CLI flag — is wired from the profile instead of
falling through to the 16384 constant. Nothing new is enforced anywhere; the
invariant holds by construction:

```
prompt_tokens_est + max_completion_tokens + margin_tokens ≤ ctx_tokens
```

### 3. Precedence

Explicit CLI flag > `MEMOIRE_DERIVE_*` / sweep env > profile > built-in
default. An absent profile is today's behavior exactly: old registries parse
unchanged, and every current invocation keeps its current numbers.

### 4. Thinking policy is a DECLARATION in v1

`policy: on|off|budget` plus `budget_tokens` document how the serving side is
configured — llama-server `--reasoning` and `--reasoning-budget N`, both
available in the build the bench ran (10396). Memoire records the declaration
in run provenance: additive `meta_json` fields in the D47 derivation block
carrying the effective prompt ceiling, the effective `max_completion_tokens`,
and the thinking policy. It does **not** enforce the policy request-side.
Request-side enforcement (`chat_template_kwargs`) is a named later option, not
v1 — the profile's job in v1 is to make the serving reality legible in the
run record, so a bench result can be attributed to a configuration.

### 5. Dry-run JSON carries the resolved limits

The dry-run JSON gains the resolved effective limits as an additive field. The
sweep script can then drop its hardcoded 70000 once profiles exist, and the
bench driver can stop defaulting to it; the `MEMOIRE_DERIVE_*` env override
(D162's pattern) keeps working for both.

### 6. Scope

Local-qwen endpoints in v1. BYOK (D233) gets the same optional fields later —
its runs are operator-driven manual invocations, so the unattended-yield risk
that motivates this is lower. The profile is per-ENDPOINT in v1; a per-model
override map is an additive later extension, needed only if one endpoint ever
serves models with different geometry.

## Non-goals

- **No tokenizer in memoire.** `bytes_per_token` is declared data, not a
  computed count.
- **No schema migration.** Every provenance addition is `meta_json`.
- **No Provider interface change.** Endpoint-level options only.
- **No auto-probe of the serving context.** Reading llama-server `/props` to
  fill `ctx_tokens` automatically is a later option, not v1.
- **No change to the D245 outstanding surface** or its byte-classified
  aggregates.
- **No change to D212 split mechanics.** The recursion, the median-epoch
  split, and the depth cap are untouched.

## Law compliance

- **D75** — the endpoint registry is the seam; its Consequences invite exactly
  this kind of extension.
- **D56** — config is the home of endpoint data; no hostnames or geometry
  compiled into the binary.
- **D53** — endpoint-level options without a Provider interface change.
- **D212** — split mechanics unchanged; the ceiling number is what moves.
- **D162** — the env-override pattern is preserved, and precedence names it.
- **D231** — this lives in memoire, never harness-side.
- **D137** — local-only auto-derive is untouched.
- **D245** — byte-classified aggregates untouched.

## Bench consumption and re-bench plan

The bench registry (`bench/derive/data/model-endpoints.json`) carries the same
profile shape. `run.py` computes `--max-prompt-bytes` from it instead of
defaulting to 70000, and the candidate's llama-server wrapper carries the
declared thinking flags — so the bench's serving configuration and the run
record agree by construction rather than by operator memory.

Planned re-bench: the **same 9B weights**, profile `{ctx_tokens 32768,
max_completion_tokens tuned to the formula, thinking budget ~8192}`. The
expectation to test: the omission axis flips while the axes the candidate
already won — relevance, noise, dedup — hold. Those axes were judged on the
legs that completed, so the re-bench must confirm they survive a smaller
window, not assume it.

Optionally a second candidate column served with `--reasoning off`, to measure
whether thinking earns its latency (ok legs ran a median 52s; failed legs
97–326s). The autopsy declines to guess this: the won axes may owe something
to thinking, and that is a bench question.

## Open questions — ruled (operator, 2026-08-25)

1. Thinking budget for the 9B re-bench: **8192**.
2. Second `--reasoning off` bench column: **yes** — same weights, same window
   geometry, isolating thinking's contribution.
3. Per-endpoint granularity: **confirmed**; per-model override stays a later
   additive extension.
4. Production hl3 profile: **only when a candidate wins** — no production
   registry change until then.

## Q4 fired (2026-09-07)

A candidate won and the production profile landed — on a NEW endpoint rather
than on `hl3`, which the ruling took out of the scheduled path. Adoption config
(derive-bench, memoire obs 499683): Qwen3.5-9B UD-Q6_K_XL, ctx 32768, one slot,
completion cap 12288, thinking budget 8192 enforced server-side via llama-server
`--reasoning-budget`, MTP draft. Declared on the `hl7-conductor` entry in the
k8s registry ConfigMap (`cdk8s/main.ts`, manifest custody per D0252):

```
effective_max_prompt_bytes = floor((32768 − 12288 − 512) × 2.2) = 43929
```

the same 43929-byte geometry every winning bench column ran. Two §3 precedence
consequences are load-bearing on that lane and pinned by the manifest contract
test: it sets no `MEMOIRE_SWEEP_MAX_PROMPT_BYTES` (an explicit value outranks
the profile) and no reasoning-effort steering (§4 — the policy is a declaration;
the budget is served, not requested).
