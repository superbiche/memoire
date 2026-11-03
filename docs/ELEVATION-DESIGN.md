# Elevation — the `can_instruct` gate

Rework boundary: this document describes existing behavior and compatibility.
The approved target direction and agent discretion live in the maintainers'
rework mandate (private ledger, `policies/rework.md`). Existing review gestures and read filters
do not prescribe the replacement architecture. Unrelated evidence, privacy and
published consumer obligations remain applicable.

Ruled by the operator 2026-07-10 (interactive session, design conversation with
Fable). Companion to `GOVERNANCE-DESIGN.md`, which owns the underlying flags.

## Three acts, three words (vocabulary is binding)

- **Accept** — review-queue verdict, `proposed` → `accepted` (`review_state`).
  Lazy, optional, no SLA. The system is fully functional with observations
  sitting proposed forever (they inject with `~`, search, and feed derivation).
  Ignoring the accept queue is a supported lifestyle.
- **Elevate** — setting `can_instruct=1`. Rare, **pull not push**: there is no
  elevation queue, no candidate badge, no daily duty. The operator reaches for
  an observation ("this should ride every session") — the system never reaches
  for the operator. Same gesture as adding a CLAUDE.md line, cheaper, audited.
- **Registry promotion** — Liaison's `EVIDENCE-REGISTRY.md` `proposed`→`promoted`
  flip for capability claims (D6). Unrelated to memory observations. Never call
  the two acts above "promotion"; the collision caused real confusion.

## Rulings

1. **Provenance: reuse `user_confirmed`.** An operator clicking elevate IS user
   confirmation in the sense the schema trigger was written for. No new
   provenance value, no migration. The "confirmed where" audit detail lives in
   the observation review log, not in the provenance vocabulary. Sealed
   vocabularies grow only when a consumer branches on the difference.
2. **Elevate requires accepted.** Elevation is a second gate above accept,
   never a shortcut past it. For a proposed observation the path is two
   deliberate acts: accept, then elevate. The double gesture is intentional at
   this blast radius (a wrong instructive observation pollutes every future
   session).
3. **Graduation confers candidacy only, and quietly.** When a Liaison claim
   graduates, linked Memoire observations MAY be marked as elevation candidates
   (meta_json tag + review-log entry; no schema change) surfaced as a webui
   FILTER — no count, no badge, no nag, zero cost to ignoring it forever.
   Graduation never sets `can_instruct` itself: producer never self-certifies,
   including when the producer is the evidence loop. This slice is optional and
   drops without damage.
4. **Instructive injection is a separate block.** Elevated observations inject
   as their own clearly-tagged section (e.g. `# [memoire] operating notes`),
   distinct from informational recent-context, killable independently.

## Slices (delegable from this contract)

- **P1 — elevate action.** `POST /v1/observations/{id}/elevate`: one
  transaction setting `provenance=user_confirmed` + `can_instruct=1`, review-log
  entry, 409/422 unless `review_state=accepted`. Webui gets a deliberately
  heavier control than accept. Small; natural rider for the review-webui
  branch. *(Addendum 2026-07-10, same day: P1 includes the inverse — demote,
  `can_instruct` back to 0, provenance untouched, review-log entry. Elevation
  at this blast radius must be reversible; demote is operator-pull exactly like
  elevate, no queue.)*
- **P2 — instructive injection.** Inject script (later Liaison-owned wiring)
  emits the separate operating-notes block for `can_instruct=1` observations
  of the current project.
- **P3 — graduation candidacy** (optional, last). Liaison graduation gate marks
  linked observations via the echo seam; webui filter view. Waits for real
  graduated evidence flowing through the echo seam; needs its own thin contract
  when picked up.

Sequencing P1 → P2 → P3. Horizon note (parked, not planned): elevated
observations that prove themselves over weeks are distillation candidates for
the instruction layer (AGENTS.md) — the door faces that way, nothing walks
through it yet.
