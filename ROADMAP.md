# Memoire: wanted-later ledger

**Scope — read this before adding anything (memoire:D178, 2026-07-25).** This
file is the MEMOIRE-LOCAL wanted-later ledger, and nothing more. Master
cross-project sequencing for the dual Liaison + Memoire project stays in
liaison `ROADMAP.md` (master plan; Now gate + Reading protocol), per the
standing pointer at the head of `docs/ARCHITECTURE.md` — this repo carries
slice-level detail only, and that statement is reconciled by function here,
not contradicted. If an item decides WHEN work happens across the two repos,
it belongs in liaison `ROADMAP.md`. If an item records that Memoire wants
something LATER, it belongs here.

**What lands here:** items deported from `MISSING.md` (tagged `gap:`, with
provenance and a one-line ruling, memoire:D173) and Memoire-scoped
wanted-later work that no current version's goal requires. A gap re-enters
`MISSING.md` only when a version's goal requires it.

---

## Wanted later (memoire-local)

- **Semantic layer / vectors.** **LANDED hub-side 2026-08-03 (memoire:D218):**
  `services/embed-worker/` is a real service (MiniLM q8 freshness sweep +
  query-embed), and the hub runs hybrid retrieval (vectors + RRF over BM25)
  live. The standalone SQLite lane stays FTS-only by design (memoire:D232);
  the packaged PG full-client path is `gap:pg-fullclient-hybrid` below.
  What remains wanted-later here: nothing — superseded by the gap lines.
- **Graph proposals.** L4 of the same spine, design-only, with inert schema
  sketches and a `services/graph-proposer/` stub. Wanted after the derived
  layer's quality is settled, never before.
- **MCP write surface.** The MCP adapter shipped read-side 2026-07-10 (four
  tools over stdio). Writes stay HTTP-only: `docs/ARCHITECTURE.md` holds that
  the HTTP API is the single write authority and that MCP is a thin adapter
  over it, not a second implementation. A write surface is wanted-later and
  would need that invariant re-ruled first.
- **Standalone-SQLite packaging artifact.** memoire:D165 Q4 ruled "constraint
  now, artifact later": the standalone-SQLite smoke entered the default test
  path immediately (and holds parity today), while the packaging artifact was
  parked for the release milestone. **LANDED (R-10.5, memoire:D188):** the
  release sequence is `make dist` (tarball + `SHA256SUMS`) → `make
  dist-check` (validates the existing artifact) → `make dist-sign`
  (operator-present detached GPG signature); the constraint remains live in
  the default test path. The npm packaging slice (D189) stays wanted-later,
  decoupled from v0.7 (memoire:D225).
- **License posture for public release.** memoire:D174 names the AGPL and
  standalone-SQLite constraints as framing the Great Book must carry where
  relevant. The standalone-SQLite half is grounded (memoire:D165 Q4); the
  license half had NO ratifying entry in either repo's roadmap or journal —
  the only occurrence was memoire:D174's own framing sentence. **RULED
  2026-07-28 (memoire:D180): AGPL-3.0 is the license posture.** What remains
  wanted-later is the ARTIFACT: the `LICENSE` file and per-file headers land
  with the release-track packaging slice R-10.5, not before — the same slice
  that owns the standalone-SQLite packaging artifact above. **LANDED
  (R-10.5):** `LICENSE` and per-file SPDX headers are committed, guarded by
  `scripts/tests/spdx-header-test.sh` in the default test path.
- **Vault integration and autonomous memory agents.** Both named in the
  pre-v0.6 AGENTS.md do-not-jump-ahead list; L5/L6 of the liaison memory
  spine, where the boundary stance is already ruled (L5 reads L2, L6 writes
  L2). Aspirational, kept here so the deferral stays visible.

## Deported gaps (`gap:`)

Deported from `MISSING.md` in the founding sweep (2026-07-25): wanted-later,
not required by the v0.6.0 goal. Both were filed and deported in the same
act, so no archived body exists — the provenance cited on each line is the
record. A deported gap re-enters `MISSING.md` only when a version's goal
requires it.

- gap:missing-4 — Secret source is still env-var-by-name after four eras (memoire:D58, ratified interim in memoire:D70) — the interim posture works and the hub runs on it; replacing it is design debt no version goal has required yet, and the consolidation batch will restate the rule either way (deported 2026-07-25).
- gap:missing-6 — The AGENTS.md model/effort delegation policy routes to GPT-5.5/5.4 while the repo's actual reviewer has been gpt-5.6-sol at xhigh for a month (memoire:D179) — no product behavior depends on it, and model routing is operator-owned: it is a queued operator ruling, not implementation work, so the release gate must not block on it (deported 2026-07-25). **CONSUMED 2026-07-28 (operator ruling R9):** the table is replaced by a pointer to the fleet routing law plus memoire-specific deltas (takeover-handoff floor, orthogonal-review-before-merge); per-repo model tables are a rot class, retired.
- gap:liveness-surface — `/v1/health` under the kubelet livenessProbe is dependency-bound (five store scans + queue filesystem walks through the request context), so a saturated read pool or hung PVC can outlive the probe timeout and restart the sole pod — the dependency-driven crash loop the readiness/liveness split exists to prevent (honest-surfaces review F4, 2026-08-03; class=ARCHITECTURAL origin=preexisting route=audit, `tmp/honest-surfaces-review/findings.md`). Interim mitigation shipped with the slice: explicit `timeoutSeconds: 10` + `failureThreshold: 3`. The real fix is a dedicated cheap liveness surface (process-alive only, no store touch) — a ruled contract, not a drive-by; audit lane, deadline-free.

- gap:pg-fullclient-hybrid — the PG full-client lane has no packaged path to hybrid retrieval (vectors+RRF): the embed worker + hub wiring exist (memoire:D218) but only as the hub deployment's cdk8s lane; a self-hosted PG full-client needs a compose-style recipe before the matrix cell can say yes (memoire:D232 marks it wanted-later; filed 2026-08-05).
- gap:standalone-derive-scheduler — the standalone BYOK derive lane ships as a documented manual invocation in v0.7 (L3: released artifact, honest manual step); a packaged timer/cron helper for standalone deployments is wanted-later unless it turns out trivially includable in the BYOK slice (handoff 2026-08-04 §3; filed 2026-08-05).

- gap:pg-byok-composed-leg — no packaged test composes the full PG path capture→queue→BYOK-derive→proposed-row the way `tests/e2e/standalone-sqlite-smoke.sh` composes it for SQLite; today's PG evidence is the store-contract battery (`make test-pg`), the cutover parity record, and the hub deployment running the drainer+derive on PG live. Wanted-later: a PG leg of the composed smoke (needs a disposable PG, likely rides the pg-shadow harness) so the matrix's PG column is test-backed end-to-end, not deployment-backed (D225 README audit F2; filed 2026-08-05).

- gap:base-url-unification — `MEMOIRE_BASE_URL` is the ratified read-path single knob (memoire:D165, R-10 §3.2). This line is the ONE inventory of record (D342 SIMPLIFICATION, memoire:D235 chain r2): sweep of 2026-08-05 (`rg -n 'MEMOIRE_BASE_URL|MEMOIRE_BASE\b|MEMOIRE_ADDR|127\.0\.0\.1:37778'` over `scripts/` and `services/`) — knob HONORED by the inject script, the MCP server, the notify tooling (`memoire-derive-auto.sh`), and `scripts/check-health.sh` (where legacy `MEMOIRE_BASE` remains a winning compatibility override when both are set); knob NOT honored by `scripts/smoke.sh` and `scripts/quadlet.sh` (`MEMOIRE_BASE` only — quadlet's rebuild readiness probe included), the embed worker's API client and `scripts/memoire-derive-sweep.sh`'s lapse tail (`MEMOIRE_ADDR` only; both hub-co-located in practice), and the UI dev proxy (loopback). Out of the class: `scripts/memoire-bootstrap.sh`, whose `MEMOIRE_ADDR` is the server bind address it installs, not a read-path base. Unify the residue under the knob or ratify the exceptions explicitly (memoire:D235 review r1 F2 + r2 N1; filed 2026-08-05).
