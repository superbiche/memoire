# Mirror protocol — remote custody of Memoire artifacts

Status: contract of record for liaison:D97 decision 6 gate condition (iii),
established by memoire:D147 slice 1. Slice 2 (memoire:D155) implements the
mechanics and RESOLVED rules 4-5 against the real pipeline (see below);
that resolution amends the original bounded-retention promise and was
**RATIFIED by the operator 2026-07-20 (memoire:D156 ruling 1)** — the
merged contract text stands as ratified. *(Dated correction 2026-08-05:
this paragraph carried its pre-ratification "pending" flag for two weeks
after D156 ruled; cleared during the D225 README-vs-code audit, which
proved the flag stale — MISSING #3 consumed in the same act.)* The
D97 gate stays closed until conditions (i)–(iii) all hold: (i) retro-sweep
done (memoire:D144/D145), (ii) age-encrypted artifacts, (iii) purge exists
+ this protocol enforced.

## Definitions

- **Custody artifact**: any Memoire data that leaves the live store as a
  file — DB snapshots, `raw/` archives, quarantine archives. The live
  store itself (SQLite file, `raw/` dir, queue) is never mirrored.
- **Generation**: the monotonic counter in
  `~/.memoire/custody/MANIFEST.json`, bumped by every `memoire-purge`
  apply that deleted anything. It is the purge lineage marker: an
  artifact created at generation G is stale the moment the counter passes
  G.
- **Landing zone**: `/data/backups/memoire/` on HL3, fed by the
  three-phase push (artifact sync with `--delay-updates` → manifest →
  prune). An interrupted TRANSFER leaves the previous complete set
  intact; the one residual window is a crash during rsync's final
  rename batch (sub-second, local-disk), which can leave a
  manifest-listed artifact with newer bytes than its recorded hash.
  **Manifest authority + verify-before-restore**: only listed artifacts
  count, and every restore MUST first run the hash verification
  (`sha256sum -c` over MANIFEST.json); on a mismatch, recover the prior
  artifact bytes from the off-site pipeline's `_archive` (the sweep
  archived them on its next pass) or re-push from staging. Unlisted
  files are push residue. The off-site pipelines sweep the landing zone
  like any other `/data` content.
- **Mirror**: any non-local destination holding custody artifacts.
  Current inventory (D150/D154): the storagebox crypt pipeline
  (`rclone-backup.service`, system scope) and the gdrive crypt remote
  (michel user scope; join proposed in D155 item 6, not yet wired). A
  destination not listed here is a contract violation, not an oversight.

## Rules

1. **Artifacts, never the live store.** Mirrors receive snapshot
   artifacts produced by the custody owner; nothing tails or replicates
   the live SQLite/queue/raw surfaces directly.
2. **Generation-stamped.** Every artifact set records the generation at
   creation time (in its path — `raw/gen-<G>/` — and in the plaintext
   MANIFEST.json). An artifact without a generation stamp must not be
   synced.
3. **Purge propagates by replacement at the landing zone.** A purge bumps
   the generation; the next snapshot run drops the old generation from
   staging and the push (`rsync --delete`) removes it from the landing
   zone. The landing zone never accumulates superseded generations.
4. **Remote retention is NOT deterministic on this pipeline — stated
   honestly (D155 amendment).** The storagebox pipeline is
   `rclone copy --backup-dir _archive/<day>`: files deleted at the landing
   zone are never deleted remotely, overwritten files accumulate under
   `_archive`, and nothing prunes `_archive`. The original rule-4 promise
   (bounded N sets, computable time-to-gone by file deletion) is therefore
   unsatisfiable here. Under this amendment the remote FILE lifecycle is
   unbounded and rule 5 — the key layer — is the purge-propagation
   mechanism that carries time-to-gone: **purged plaintext is gone from
   mirrors when the pre-purge snapshot key is destroyed**, regardless of
   which ciphertext files linger. (Pipeline-level `_archive` pruning would
   restore file-level bounds and is flagged with the operator's pipeline
   repair, but this contract no longer depends on it.)
5. **Memoire-held encryption layer — ADOPTED (D155).** Every artifact is
   age-encrypted to the dedicated snapshot key BEFORE leaving the machine
   (the pipelines' own crypt only protects against the remote; its keys
   give Memoire no shred handle). Key custody per D150/D154: Bitwarden
   entry + printed paper copy, dedicated key independent of the D144
   quarantine key. **Crypto-shred procedure** (the rule-4 lever): mint a
   new keypair, replace the recipient file, run snapshot+push — the
   tooling detects the recipient change and re-encrypts EVERY staged
   artifact (a matching generation never skips re-encryption; state
   tracks the recipient) — verify the push completed (rule 7), and only
   THEN destroy the old key: vault entry AND printed copy. Every
   pre-rotation remote artifact, including `_archive` copies the pipeline
   will never delete, becomes unreadable ciphertext. Run it after any
   purge whose content must not survive in mirrors. Destroying the old
   key before a verified post-rotation push destroys the live restore
   chain — the order is the contract.
6. **Push-only.** Sync direction is from the custody owner outward.
   Mirrors never write back into any Memoire surface. The push identity
   (`memoire-mirror` on HL3) is rrsync-locked to the landing zone.
7. **Verification duties.** BEFORE ANY RESTORE: run the manifest hash
   verification (definition above) — the rename-batch window makes this
   mandatory, not hygiene. AFTER A PURGE: verify the landing zone carries
   only current-generation artifacts (one push cycle), and journal
   whether the crypto-shred lever was exercised. If it was not, the purge
   record states plainly that pre-purge ciphertext remains on mirrors
   under a live key. Until journaled, the purge is complete locally but
   OPEN remotely — report it that way.

## Failure honesty

- Between a purge and the next push, purged data exists in the landing
  zone's previous artifact set; between a purge and a key rotation, purged
  data exists on mirrors as ciphertext under a LIVE key. Rule 7 forces
  both states to be named in the purge record.
- SSD/physical-media caveats apply to remotes exactly as locally: this
  protocol is application-level destruction, not media scrubbing.
- The storagebox pipeline's own health (timer disabled, modtime failure —
  D154 item 4) is the operator's repair; until it runs nightly, the
  landing zone is current but off-site copies are stale-by-outage. The
  D97 (ii) gate needs the sweep live.
