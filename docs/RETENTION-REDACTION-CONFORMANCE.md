# Retention & Redaction Policy — Conformance Table

Slice D4 of memoire:D136 (liaison:D97 hygiene). Maps every decision of the
policy of record — `liaison/RETENTION-REDACTION-POLICY.md` (RULED
2026-07-07, liaison:D97) — to its implementation status in this repo.

Status enum: **implemented** (live behavior), **manual-tooling** (built and
tested, execution is an operator action), **absent** (nothing implements it
yet). Statuses are drift-guarded by
`scripts/tests/retention-conformance-test.sh`: every decision and ruling row
gets its status cell asserted, and rows whose status names a mechanical
artifact (a tool, code path, or schema object) additionally get a reality
probe — implementing a row without updating it (or vice versa) breaks
`make test`. Rows whose status is pure policy posture (6, R4) are guarded on
the table side only; nothing in code can prove them.

Last verified: 2026-08-23.

## Decision table conformance

| # | D97 decision | Status | Evidence / refs |
|---|---|---|---|
| 1 | Retro-redaction sweep of the pre-2026-07-02 corpus | **manual-tooling** | `services/api/cmd/sweep` (`memoire-retro-sweep`, `services/api/internal/sweep/sweep.go`): redacted-generation output, age-encrypted quarantine of originals, JSON report with per-rule hit counts, reconciliation invariant. Caged + oracle-tested. COPY-run 2026-07-18 (memoire:D143), then LIVE run same night on operator GO (memoire:D144): 277,430 files, 670 with hits, 2,066 redactions, reconciliation balanced; generation swapped, originals shredded after archive verification (quarantine `~/.memoire/quarantine-presweep-2026-07-18.tar.age`, key `~/.memoire/age-quarantine-key.txt` = crypto-shred handle). RAW side done. Note this is a distinct rebuild from Standing gap 1's `memoire-redact-derived` (D145): that tool redacts existing derived rows in place; the policy's derived-store REBUILD step — re-deriving observations from scratch off the now-redacted raw generation — still has no tooling. |
| 2 | Raw keep-all, no TTL; monthly `.tar.zst` cold archives | **implemented** (keep-all) / **implemented, deviating** (cold archives) | Keep-all holds: no rotation or TTL touches `raw/`; only purge destroys. Raw custody is one file per FIRST write of each envelope: an exact duplicate, whose idempotency key is already stored and whose session is known (ingest writes nothing), gets no raw copy. First write wins (operator ruling a65, `records/history/L0007-raw-custody-skips-no-op-duplicates.md`; drainer probe `Store.IsNoOpDuplicate`). A duplicate under a new session, a malformed item and every first write keep their copy. Cold-archive compaction is `memoire-raw-pack` (`services/api/internal/rawstore`): closed UTC days are packed into immutable parts `raw/pack-YYYY-MM-DD.NNN.tar.zst` that REPLACE the flat files (only purge rewrites a part, always with a custody-generation bump); Replay, purge and the retro-sweep read packs through `rawstore.Walk`, with flat/packed equivalence oracles. Period: DAILY parts, superseding the D97 monthly period (operator ruling a66 (a), 2026-10-12; `records/history/L0006-raw-cold-archive-daily-packs.md`). Encryption: plaintext parts on the LUKS custody volume, superseding age-at-creation (operator ruling a67 (a), 2026-10-12; decision 5 below). The snapshot (`scripts/memoire-snapshot.sh`, D155) captures packs by name as `.tar.zst.age` MIRROR artifacts. |
| 3 | Derived stores: compaction/rebuild lever, not retention promise | **implemented** (soft-delete visibility) / **manual-tooling** (compaction lever) | Soft-delete columns + default `deleted_at_epoch IS NULL` filters: `services/api/internal/db/store.go` (`observationWhere`). Compaction lever: `memoire-purge --vacuum` (WAL `TRUNCATE` checkpoint + `VACUUM` after hard deletes; FTS stays consistent via the 0001 delete-triggers). |
| 4 | Hard purge (`memoire purge`) | **manual-tooling** | `services/api/cmd/purge` (`memoire-purge`, `services/api/internal/purge`): hard delete ignoring soft-delete visibility; selectors project / event-ids / observation-ids; cascades raw-projection + source-linked observations, review log, source links, and (project selector) sessions, derivation_runs, project_roots, project_identity_alerts; all four file surfaces scanned (raw/, queue pending/inflight/failed + sidecars) with envelope matching through the drainer's own normalization (derived source_event_id matches); quarantined (age) then removed all-or-error; phased crash-safe intent journal (`pre-commit`/`committed`, DB-probe reconciliation) with `--resume-intent` gated identically to apply; corrupt custody manifest stops destructive runs; audit tombstone `purge.apply`; custody generation bump. Dry-run default, `--apply` double-gated (`MEMOIRE_PURGE_APPLY=yes`) + quiesce guard (refuses beside live memoire-api/memoire-derive). memoire:D147 slice 1. Secret deletion is now solved at application level; physical-media caveat documented in the package. |
| 5 | Local plaintext under OS permissions; cold archives age-encrypted | **implemented** (permissions) / **deviating** (cold archives are plaintext on the LUKS custody volume, superseding age-at-creation by operator ruling a67 (a), 2026-10-12; the snapshot and quarantine copies are age-encrypted, the HL6 DR mirror is plaintext on its own LUKS volume, as for flat raw/) | `0o700` home dirs (`services/api/internal/config/config.go:104`), `0o600` key files (`redact.LoadOrCreateKey`). The sweep's quarantine archive IS age-encrypted at creation (`services/api/internal/sweep/sweep.go`) — the pattern exists as manual-tooling. |
| 6 | Non-local custody gated on (i) sweep done, (ii) age-encrypted artifacts, (iii) purge exists + mirrored | **absent** (no enforcement mechanism) | Gate is currently held by absence: no sync, no remote custody of any Memoire artifact exists. Conditions stand at (i) ✓ sweep executed end-to-end (raw 2026-07-18, derived 2026-07-19; memoire:D144/D145), (ii) ✗, (iii) purge ✓ (`memoire-purge`, memoire:D147 slice 1) + mirror contract defined (`docs/MIRROR-PROTOCOL.md`; enforcement lands with slice 2's sync tooling) — so the gate must stay closed until (ii) and the protocol enforcement exist. Nothing enforces this in code; it is policy + this table. |
| 7 | Assistant-turn capture via transcript ingest, default-deny projection | **implemented** — with a sequencing drift, see below | Transcript/rollout ingest: `scripts/memoire-capture-claude-transcripts.sh`, codex/qwen/opencode rollout capture; `*_assistant_turn` event types in `services/api/internal/modelderive/guard.go:91`. Default-deny projection holds structurally: raw events have NO FTS surface (only `observations_fts` exists, `schema/0001_initial.sql:121`), and derived observations start `review_state=proposed`, excluded from default reads. |
| 8 | `queue/failed/` 30-day TTL | **implemented** | Drainer reaper `ReapFailed` (`services/api/internal/drainer/drainer.go`): hourly, mtime-based, deletes expired items + `.error.json` sidecars, audit `queue.failed_reap`. Default 30 days; `MEMOIRE_FAILED_TTL_DAYS` overrides, `0` disables. `queue_failed_depth` on `/v1/health` remains the visibility surface (memoire:D138). |

## Ruling conformance (D97 §Rulings)

| # | Ruling | Status |
|---|---|---|
| R1 | Sweep = quarantine generation (age-encrypted originals, shred later) | **manual-tooling** — implemented by `memoire-retro-sweep` exactly as ruled. |
| R2 | Assistant-turn ingest unblocks AFTER the sweep completes | **Sequencing drift retroactively accepted (operator ruling, 2026-07-18; memoire:D144).** The drift: ~115k `*_assistant_turn` events went live (claude 78,579 / codex 29,459 / qwen 6,258 / opencode 661) before the live sweep ran. Accepted because defense-at-source guarded every one of those NEW writes (live since 2026-07-02; the copy-run proved zero post-activation leak-through), so the exposure was always the pre-07-02 corpus the sweep targets — unchanged by the ingest. The live sweep was ordered in the same ruling. |
| R3 | Cold layer = monthly age-encrypted archives (`.tar` + zstd, age at creation) | **implemented, deviating** — `memoire-raw-pack` packs DAILY: the monthly period is superseded by operator ruling a66 (a), 2026-10-12. Parts are plaintext on the LUKS custody volume: age-at-creation is superseded by operator ruling a67 (a), 2026-10-12 (`records/history/L0006-raw-cold-archive-daily-packs.md`). The D155 snapshot artifacts remain additive `.tar.zst.age` mirrors under docs/MIRROR-PROTOCOL.md and carry the packs age-encrypted. |
| R4 | HL3 waits entirely | **held** — nothing Memoire-shaped leaves the operator's machine; no sync exists. |

## Quarantine key custody

The quarantine age identity lives at `~/.memoire/age-quarantine-key.txt`
(0600) and, durably, in the operator's password vault (resolved on demand via
operator-side tooling — interactive only, never ambient; this repo never
reads the vault). **Release note:** the vault wiring is operator-personal
infrastructure, not part of Memoire. If this project is ever released, users
get the sweep's `--age-recipient` flag and nothing else — quarantine key
generation, custody, and backup are explicitly left to them, and the docs
accompanying a release must say so.

## Standing gaps, in policy-sequencing order

1. ~~Live retro-sweep execution~~ — RAW side executed 2026-07-18 (memoire:D144); derived side executed 2026-07-19 (memoire:D145, `memoire-redact-derived`: 773 rows — 670 events + 103 observations — convergence-verified to 0, identity-path preflight clean). Decision 1 is closed end-to-end. Standing caveat (D145, operator-ratified): `derivation_runs.input_hash` not recomputed — historical re-derives can mint new runs + proposed duplicates.
2. ~~Purge command + failed-queue TTL~~ — executed 2026-07-19 (memoire:D147
   slice 1): `memoire-purge` (decisions 3, 4) + drainer failed-queue reaper
   (decision 8) + `docs/MIRROR-PROTOCOL.md` (decision 6 condition (iii)
   contract; enforcement rides slice 2).
3. Cold-archive compaction + age encryption (decisions 2, 5 — third slice;
   slice 2 of memoire:D147 absorbs the encrypted-artifact half).
4. ~~R2 sequencing ruling~~ — ruled 2026-07-18: retroactively accepted (memoire:D144).
