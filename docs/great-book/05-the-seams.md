# 5. The seams
*Great Book v0.6 edition — 2026-07-25.*

Memoire was not built alone, and pretending otherwise would make several of its
design choices look arbitrary. It is one strand of three.

The other two are **Liaison**, which carries the shared instruction layer, the
skill library, the model-routing policy, and the evidence-and-graduation loop;
and **Conclave**, the multi-model dispatcher that runs work through a role
graph. When the three are referenced as a whole, the collective name is
**Tresse** — French for the braid: three strands, each held by the interleaving
of the other two (liaison:D253, ruled by the operator on 2026-07-19). The name
is a documentary designation only. Nothing is renamed, no repository merges,
and each project keeps its own identity, its own journal, and its own version.

This chapter is about where Memoire touches the other two, and it is
deliberately a chapter of pointers. Seam content that is duplicated on both
sides of a seam is a defect — the copies drift, and then two documents disagree
about a contract that has exactly one true version. So where a contract exists,
this chapter names the file and stops.

## Sibling under contract, not subtree

The governing rule is the first thing to state, because everything else follows
from it: **Memoire is a sibling under contract, not a subtree.** Nothing in
Memoire's product runtime depends on Liaison. The service binary, the container
images, and the deployment target gain no Liaison dependency of any kind
(D169, D171). You can clone this repository, build it, and run it with no
awareness that the other projects exist — and that is a property maintained on
purpose, not a coincidence of the current state.

There is exactly one cross-repo dependency, and it is fenced. Memoire's
documentation-state generator is Liaison's `decision_state.py`, parameterized
and invoked as a **dev-time** tool from Memoire's doctor and CI. It is not
forked and it is not rewritten in Go, because it is tooling rather than
product, and a Go rewrite would put doctrine tooling inside the shipped
service. It is journaled on both sides — the Liaison-side entry cites the
Memoire one — specifically so that neither repository can later discover the
coupling by surprise (D171).

That is the whole of the mechanical coupling. Everything else in this chapter is
a *contract*: an agreement about the shape of data crossing a boundary, with
each side owning its own implementation.

## The boundary rule

There is one sentence that governs the entire Liaison relationship, ratified on
the Liaison side (liaison:D46) and made physical in Memoire's data layer:
**Memoire observes; Liaison derives and produces.**

Its operational form is the rule chapter 2 spent a section on: agent-written
memory is evidence, never instruction, until a human hand crosses the line.
Capture, derivation, and the evidence echo all produce *proposed* memory freely
and unattended. Only the operator elevates. Liaison feeds the evidence side of
that line and has no button that crosses it — there is no API call, no adapter,
and no MCP tool by which an automated system elevates an observation.

That is why the MCP surface is read-only and why elevation sits behind a
confirm step in a human-facing UI. Those are not UX choices; they are the
boundary rule expressed in code.

## The evidence echo, and the feed that runs the other way

Two channels cross this seam in opposite directions, and confusing them is the
easiest mistake to make about the relationship.

**The echo pushes evidence into Memoire.** When Liaison finalizes a graduation,
each appended evidence observation is echoed into Memoire as a first-class
event, through its own named adapter and its own event type. It is not disguised
as another client's traffic — the retired first version of the convention had
Liaison impersonate a Claude Code prompt, which worked and was wrong, because
provenance you cannot reconstruct from the audit trail is not provenance.

**The feed pulls memory back out.** Liaison's Memoire-feed adapter reads
observations out of Memoire and appends them to its own graduation log, strictly
read-only toward Memoire: no writes, no schema changes. What it takes is every
ACCEPTED, evidence-eligible row, **regardless of provenance** — an accepted
observation a model inferred is as feed-eligible as one a human confirmed,
because acceptance is the gate and confirmation is a separate, later verdict.
The low-trust states are simply not accepted, so they never qualify.

Two judgments govern the pair, and each replaced something worse:

- **Authority is explicit.** Liaison's evidence store is authoritative for
  evidence (liaison:D218). A Memoire-side rejection or skip of an echoed
  observation affects *memory only* and never graduation. Without that ruling, a
  memory-layer hiccup could silently invalidate an evidence chain in a different
  system — the kind of coupling that is invisible right up until the morning it
  matters.
- **Redaction is contractual, and the contract is measured.** Liaison pre-redacts
  to the shared ruleset before handing anything over; Memoire's
  defense-at-source redactor still runs as a backstop, and a backstop that
  *fires* on this adapter is a producer-side contract violation, counted on
  `GET /v1/health` as `redaction_contract_violations`. Chapter 3 covers why a
  producer cannot forge that stamp. The pattern is the point: a contract nobody
  measures is a wish, and the measurement lives on the receiving side.

Neither direction can elevate. The boundary rule above holds across both.

**The contracts.** Adapter names, event types, envelope keys, eligibility
predicates, and failure modes belong to the contract documents, and restating
them here would be a defect rather than a service — a duplicated contract drifts
from the original the first time either side changes. Memoire's side of the echo
is `docs/LIAISON-EVIDENCE-CONTRACT.md`; Liaison's counterpart is
`liaison/EVIDENCE-ECHO-CONTRACT.md`; the reverse feed has its own specification
in `liaison/MEMOIRE-FEED-CONTRACT.md`, which carries the eligibility predicate
and the accept/confirm countersign that fixes it.

## Project identity

Every event carries a project id, and getting that id right is what keeps one
person's memory from turning into an undifferentiated pile. The mechanism is a
small file.

A `.memoire` file at a repository root carries hand-editable, YAML-shaped
lines — `project: <key>`, and an optional `capture: false` repository-level
kill switch. This repository's own file reads `project: memoire`.

Resolution walks a precedence chain: an explicit `MEMOIRE_PROJECT` environment
variable wins outright; otherwise the nearest `.memoire` file found walking *up*
from the attribution directory wins; otherwise a registry of path prefixes
disambiguates same-basename collisions and folds paths from old home
directories into their current project; and if all of that fails, the directory
basename is used.

Two details of that chain encode real lessons. First, **the fallback never
fails** — capture must never break because identity was ambiguous, so an
unrecognized directory captures under its basename rather than being dropped.
Second, **presence is what counts, not commit status**: a freshly created,
not-yet-committed `.memoire` takes effect immediately, and no runtime git check
is performed. Committing the file is the usage convention rather than the
mechanism, which is what lets git worktrees inherit their parent's identity
naturally.

## Harness capture adapters

Memoire's relationship with agent harnesses is also a seam, and it is the one
most likely to matter to you, because it determines whether your tools are
covered.

Capture adapters exist for **Claude Code** (live hooks plus transcript
backfill), **Codex** (live plus rollout-file parsing), **Qwen** (live plus
rollout parsing, governed by its own implementer contract in
`docs/QWEN-ROLLOUT-CAPTURE-CONTRACT.md`), and **opencode** (capture plus an
injection plugin). A generic text adapter covers anything else on a
best-effort basis.

**Gemini has no capture adapter.** A `GEMINI.md` instruction bridge exists in
this repository and the design documents name Gemini CLI as a foreign client,
but no capture script and no rollout parser exist for it. A Gemini session can
read Memoire — that is exactly what curl parity and the MCP server are for —
and cannot be captured into it.

The adapters are installed by Liaison's wiring command rather than authored by
it: Liaison holds the registry of which hook goes where on which harness, and
Memoire holds the scripts. That is the seam. It means Memoire's adapters work
without Liaison (they are ordinary scripts you can wire by hand) while a
Liaison-managed fleet gets them placed consistently across every harness.

## The seam contract set

These are the documents the two repositories keep pointing at rather than
copying. All paths are verified to exist at the time of writing; the first two
are relative to this repository, the rest to the Liaison repository.

- `docs/LIAISON-EVIDENCE-CONTRACT.md` — the Memoire-side evidence adapter
  contract, above.
- `docs/MIRROR-PROTOCOL.md` — remote custody of Memoire artifacts, and the
  contract that holds the non-local-custody gate closed. Chapter 3's subject.
- `docs/contracts/IDENTITY-MODEL.md` — the ecosystem's identity model.
- `docs/contracts/RETENTION-REDACTION-POLICY.md` — the retention and redaction
  policy of record, ruled 2026-07-07 (liaison:D97). Memoire's conformance
  against it is tracked in `docs/RETENTION-REDACTION-CONFORMANCE.md`, which is
  chapter 3's honest scorecard.
- `docs/design/MEMORY-LAYER.md` — the layered memory model this project's name
  was chosen to span, and the origin of the foreign-client read parity
  requirement.
- `liaison/EVIDENCE-ECHO-CONTRACT.md` — the Liaison-side counterpart of the
  evidence echo.
- `liaison/MEMOIRE-FEED-CONTRACT.md` — the reverse channel: which accepted,
  evidence-eligible observations Liaison's feed adapter may read back out of
  Memoire, and the accept/confirm countersign that settled provenance's role
  in that predicate.

Liaison's own Great Book carries a chapter on shared memory
(`docs/great-book/03-life-of-a-memory.md`) which is deliberately the shortest
in that book and explicitly defers here: it says that the store, the schema,
how a raw event becomes a derived observation, and how governance gates it
belong to Memoire, and that a reader who wants the internals should read
Memoire's book instead. This chapter is the reciprocal half of that
arrangement, and chapters 2 through 4 are the internals it points at.

## Release constraints

Two constraints shape what this system may become. When this edition was
first written they stood at very different stages of ratification, and
stating that difference was the point of this section — a manual that
presents a direction as a decision is a manual that will be wrong in public.
*(Edition correction, 2026-08-05: both have since been ratified and both
artifacts have landed; the subsections below carry the dated details.)*

**Standalone-SQLite is ratified and mechanically enforced.** The operator
directive is on the record verbatim in `docs/R10-DESIGN.md`: Memoire should be
able to run locally with SQLite, locally with PostgreSQL, or as a remote hub
service with either database — "remote-hub-service is what I need;
local-SQLite is what could make the app adoption-friendly." Ratified on
2026-07-22, the constraint entered the default test path immediately: a
standalone-SQLite smoke boots the full local column — API, drainer, and store
on a scratch home, with zero PostgreSQL, zero credential store, and zero second
daemon — and runs one capture-drain-search loop. **The build fails if anyone
introduces a hard dependency that breaks the adoption column.** The shape being
defended reduces to one sentence: one binary, one `MEMOIRE_HOME`, zero external
services. That is the difference between a stated intention and a held one.

The packaging *artifact* — a quickstart, a single-binary bootstrap, a
configuration-surface audit confirming everything defaults to local — is a
named and sequenced slice (R-10.5) rather than a vague someday, and it was
deliberately parked for the release milestone on the reasoning that packaging
built before the release decisions exist would simply be rebuilt. *(Edition
correction, 2026-08-05: R-10.5 executed. The ruled sequence is three separate
commands — `make dist` builds the tarball and its `SHA256SUMS`, `make
dist-check` validates the existing artifact, and `make dist-sign` is the
operator-present step that produces the detached GPG signature. The
constraint that preceded the artifact still holds.)*

**The AGPL posture is ratified (memoire:D180, 2026-07-28).** It first appeared
as framing in D174, the entry that commissioned this book, with no ratifying
entry anywhere in either repository; the operator closed that gap at the
2026-07-28 rulings session, and AGPL-3.0 is now the ruled license posture for
the release track. If you are reading this edition and want to know Memoire's
license, the honest answer is that it is AGPL-3.0 — by ruling and, since the
R-10.5 packaging slice landed, by committed artifact: the `LICENSE` file and
per-file SPDX headers are in the tree. *(Edition correction, 2026-08-05; the
original text predicted they would land with that slice, and they did.)*

---

*Next: chapter 6, [The CLI & MCP surface](06-the-cli-and-mcp-surface.md) — every
endpoint, tool, and command, checked against the code.*
