# Memoire

Sovereign, local-first memory for a multi-harness AI workflow. Sessions from
Claude Code, Codex, Qwen, and opencode are captured into one store the
operator owns. Capture adapters for all four harnesses ship here; the
bootstrap emits the Claude Code wiring (`QUICKSTART.md` covers that path),
while the Codex, Qwen, and opencode capture scanners ship as scripts whose
invocation and scheduling are left to you. A model endpoint you provide
derives durable observations; the operator reviews them; the useful ones
greet new sessions through the injection surfaces (Claude Code and opencode
wired today; the envelope adapter serves any harness). Capture comes first
and is never blocked by anything downstream. Memory and context belong to
the operator, not to a provider — **foreign-client read parity is a hard
requirement, not a feature**.

Memoire is the shared-memory half of Liaison's founding pain fork. It is a
sibling under contract in that ecosystem, not a subtree: the two repos share
doctrine and, under one stated fence, dev-time tooling — never runtime
(memoire:D169, memoire:D171).

## Lanes

Memoire runs in three topologies. They differ by equipment, not by nature
(memoire:D232): the core — capture, replayable custody, the review
lifecycle, search — is the same system in every lane, guaranteed by the
dual-engine storage contract (memoire:D157: a storage and lifecycle
guarantee, not a feature-parity guarantee).

| Capability | SQLite full-client | PG full-client | Thin client + hub |
|---|---|---|---|
| Capture, replayable custody, queue | yes | yes | yes (client) |
| Review lifecycle (full, incl. elevation) | yes | yes | yes (hub UI) |
| FTS search | yes | yes | yes |
| Derivation | model endpoint you provide¹ | model endpoint you provide¹ | hub service |
| Hybrid retrieval (vectors + RRF) | no — FTS-only | wanted-later² | yes |

¹ BYOK: bring your own endpoint — exactly two dialects, OpenAI-compatible
v1 and Anthropic (closed set, extended by operator ruling only; see
`QUICKSTART.md` "Optional: derivation"). Evidenced by the two-dialect
stub legs in `tests/e2e/standalone-sqlite-smoke.sh` and the
engine-independent adapter contract tests — a matrix cell is a claim only
when a test or live deployment backs it. The end-to-end composition is
exercised on SQLite; on PG it is an open, ledgered gap (see the evidence
paragraph below).
² Wanted-later, tracked in the source project's roadmap ledger. Hybrid
retrieval is PG-only by ruling (memoire:D218); the SQLite lane is
FTS-only by design, not degradation.

Evidence per column, each layer named separately: the SQLite lane is the
default `make test` engine plus `tests/e2e/standalone-sqlite-smoke.sh`,
which composes capture→queue→BYOK-derive→proposed-row end-to-end. The PG
lane's store and lifecycle layers are backed by the dual-engine
store-contract battery (`make test-pg` in the source repository — the
same store code against a live PostgreSQL), the cutover parity record
(memoire:D158/D159), and the hub's production PG deployment (which runs
its own hub-side derive lane, not BYOK). The composed PG+BYOK
end-to-end leg has NOT been exercised by any test or deployment yet —
the BYOK adapter itself is engine-independent and contract-tested, but
the composition is an open gap, ledgered in the source project's roadmap
ledger. The hub lane is the live deployment this repo operates
(memoire:D186 onward). Derivation is a Memoire service in every lane —
never a harness-adapter behavior (boundary law, memoire:D231).

This file is otherwise human-facing and pointer-only (memoire:D173):
identity and lanes above, pointers below, zero perishable facts.
Anything that changes lives in a surface that regenerates.

The `memoire:D<n>` citations throughout are entries in the decision
ledger, which lives in the private source repository (`memoire-source`)
— the public export carries the code and its honest state, not the
ledger custody (curated-export ruling, memoire:D226).

- **Humans**: the Great Book — `docs/great-book/` — is the readable manual of
  the system, one edition per version. It ships in every checkout.
- **Current state** ("where are we"): `VERSION` carries the version and its
  goal; `MISSING.md` is that version's honest punch-list and ships with every
  release.
- **In the private source repository only** (deliberately excluded from the
  public export): the decision ledgers (`DECISIONS.md`, `BURN-DECISIONS.md`),
  the generated state and views (`docs/state/`, `docs/views/`), the
  wanted-later ledger (`ROADMAP.md`), vocabulary, progress history, and the
  agent instruction surfaces (`AGENTS.md`).
- **Agents working in the source repository: your files are `AGENTS.md` and
  your gesture's view (`docs/views/`) — this file is not for you.**
