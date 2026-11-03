# 5. The seams
*Great Book v0.7 edition — 2026-11-02.*

Memoire was not built alone, and pretending otherwise would make several of its
design choices look arbitrary. This chapter is about where it touches other
systems, and it is deliberately a chapter of pointers: seam content that is
duplicated on both sides of a seam is a defect — the copies drift, and then two
documents disagree about a contract that has exactly one true version. So where
a contract exists, this chapter names it and stops.

Memoire has five kinds of neighbour: **Hindsight**, the memory layer that reads
its record; **Liaison** and **Conclave**, its siblings in a three-project braid;
**Caviar**, the redaction core it consumes; the **infrastructure layer** that
hosts its hub; and the **agent harnesses** it captures from.

## Sibling under contract, not subtree

The governing rule is the first thing to state, because everything else follows
from it: **Memoire is a sibling under contract, not a subtree.** Nothing in
Memoire's product runtime depends on Liaison, Conclave or Hindsight. The service
binary, the container images and the deployment target gain no dependency on
any of them (D169, D171). You can clone this repository, build it, and run it
with no awareness that the other projects exist — and that is a property
maintained on purpose, not a coincidence of the current state.

The three-project braid is collectively called **Tresse**, French for the braid:
**Liaison** carries the shared instruction layer, the skill library, the
model-routing policy and the evidence-and-graduation loop; **Conclave** is the
multi-model dispatcher that runs work through a role graph; Memoire keeps the
record (liaison:D253). The name is a documentary designation only. Nothing is
renamed, no repository merges, and each project keeps its own identity, its own
journal and its own version.

Memoire has no build-time dependency on Liaison. The documentation-state
generator `greffe` that once rendered the decision ledgers is retired; nothing in
this tree invokes it.

## Hindsight: the memory layer

Hindsight is the newest seam and the one that changed the shape of the others. It
is a separate agent-memory system that retains sessions, consolidates what they
say into knowledge, and answers memory questions. The operator stopped
developing Memoire's own derivation on 2026-10-03 and adopted Hindsight in its
place; on 2026-11-01 Memoire's defaults followed (L0010).

The division of labour is one sentence: **Hindsight answers what was decided or
learned; Memoire answers what happened, exactly, and when.** The seam is
asymmetric and thin:

- **Memoire does not call Hindsight, and Hindsight is not required to run
  Memoire.** There is no Hindsight client in Memoire's code. The only runtime
  trace is a sentence in the MCP tool declarations that points agents at it.
- **Hindsight reads Memoire's record read-only.** According to L0010, the
  historical import read the `events` table and finished in October 2026. That importer lives outside this repository, and its scope and its limits
  (a fixed adapter list; trivial, worker and out-of-scope sessions skipped; tool
  output and hook events rendered into conversation documents) are stated in
  L0010 and are not Memoire's to change.
- **Live sessions reach Hindsight directly**, through Hindsight's own hooks in
  each harness, not by way of Memoire. Memoire is therefore the record, not the
  pipe. Headless worker sessions are deliberately not retained into Hindsight;
  they are still captured into Memoire if their harness has a lane.
- **When the two disagree, neither overrides the other.** Memoire's tool
  declarations tell the agent that a hit is "dated evidence, not current truth",
  that it corroborates and dates, and that the agent should cite the hit's id and
  verify it against the files. The temporal-conflict literature that L0010
  surveyed finds that outdated versions mislead models and that time-qualified
  retrieval helps; the date on every line is that finding in practice.

If you run Memoire without Hindsight, you have a capture service with a dated
text search. That is a complete system; it is just not a memory.

## The boundary rule

One sentence governed the Liaison relationship, ratified on the Liaison side
(liaison:D46): **Memoire observes; Liaison derives and produces.** With
derivation retired from Memoire, the sentence now reads even more simply, and it
still has an operational form: agent-written content is evidence, never
instruction, until a human hand crosses the line. Memoire's MCP surface is
read-only — there is no MCP tool that writes, accepts, or elevates anything — and
the HTTP gestures that review or elevate a stored observation need an `admin`
scope, which the tokens minted for capture hooks and thin clients do not carry
(a standalone install's own local token carries all three scopes). That is not a
user-interface choice; it is the boundary rule expressed in code.

## The evidence echo, and the feed that was retired

Two channels used to cross the Liaison seam in opposite directions; one is gone.

**The echo pushes evidence into Memoire, and it still exists.** When Liaison
finalizes a graduation, each appended evidence observation is echoed into
Memoire as a first-class event, through its own named adapter
(`liaison-evidence`) and its own event type (`liaison_evidence`). It is not
disguised as another client's traffic — the retired first version of the
convention had Liaison impersonate a Claude Code prompt, which worked and was
wrong, because provenance you cannot reconstruct from the audit trail is not
provenance. The event is captured and kept like any other. What changed on
2026-11-01 is what Memoire does with it afterwards: nothing. The derivation
rule that used to turn it into an accepted `fact` observation no longer runs.

**The feed that pulled memory back out was retired** (liaison L0005,
2026-11-01). Liaison's Memoire-feed adapter used to read accepted observations
out of Memoire and append them to its own graduation log. With no derived rows
on Memoire's default surfaces and no derivation producing new ones, there was
nothing to read back, and the adapter was deleted on the Liaison side. The echo's
authority rule stands (liaison:D218): Liaison's evidence store is authoritative
for evidence, and a Memoire-side rejection or skip of an echoed observation never
affects graduation.

**Redaction is contractual, and the contract is measured.** Liaison pre-redacts to
the shared ruleset before handing anything over; Memoire's defense-at-source
redactor still runs as a backstop, and a backstop that *fires* on this adapter is
a producer-side contract violation, counted on `GET /v1/health` as
`redaction_contract_violations`. Chapter 3 covers why a producer cannot forge that
stamp. The pattern is the point: a contract nobody measures is a wish, and the
measurement lives on the receiving side.

**The contracts.** Adapter names, event types, envelope keys and failure modes
belong to the contract documents, and restating them here would be a defect
rather than a service. Memoire's side of the echo is the evidence-adapter
contract in its design documents; Liaison's counterpart is its evidence-echo
contract. Both are in the private repositories; both state, in their own words,
what survived the retirement.

## Caviar: the redaction core

Redaction is a fleet-wide contract, `redact-v7`, implemented once in **Caviar**,
a small public Go module with a Python client. Memoire's `internal/redact`
package forwards to it and adds nothing; the module version is pinned in the
build. This is a seam in the ordinary sense — a rule change is a Caviar release
and a version bump here, and the same rules protect every project in the braid
that consumes it, so the same secret is marked the same way everywhere. Chapter 3
describes what the rules do.

## The infrastructure layer

The hub's namespace, its CloudNativePG database, the gateway that gives it a
stable TLS name, the storage nodes and their encrypted volumes, the off-site
sweeps that carry snapshot artifacts away, and the alerts that watch all of it
belong to the infrastructure layer, not to Memoire. Memoire's side of that seam
is what it asks for: a namespace, a database secret, a volume of a stated size
and class, a landing zone for snapshots, and endpoints for health, readiness and
metrics. Chapter 4 describes the ask. The practical consequence for you is that
the manifests in the reference install are an example of expressing that ask,
not a product to adopt.

## Project identity

Every event carries a project id, and getting that id right is what keeps one
person's record from turning into an undifferentiated pile. The mechanism is a
small file.

A `.memoire` file at a repository root carries hand-editable, YAML-shaped lines —
`project: <key>`, an optional `capture: false` repository-level kill switch, and
an optional `inject_content: false` for the session-start injector. This
repository's own file reads `project: memoire`.

Resolution is done by one shared script and walks a precedence chain: an explicit
`MEMOIRE_PROJECT` environment variable wins outright; otherwise the nearest
`.memoire` file found walking *up* from the working directory wins; otherwise a
registry of path prefixes disambiguates same-basename collisions and folds paths
from old home directories into their current project; and if all of that fails,
the directory basename is used. A linked git worktree attributes as its main
checkout (D164).

Two details of that chain encode real lessons. First, **the fallback never
fails** — capture must never break because identity was ambiguous, so an
unrecognized directory captures under its basename rather than being dropped.
Second, **presence is what counts, not commit status**: a freshly created,
not-yet-committed `.memoire` takes effect immediately, and no runtime git check is
performed. Committing the file is the usage convention rather than the mechanism,
which is what lets git worktrees inherit their parent's identity naturally.

## Harness capture adapters

Memoire's relationship with agent harnesses is also a seam, and it is the one most
likely to matter to you, because it determines whether your tools are covered.
Memoire declares, in a small registry, which capture lane serves each harness it
knows about, and a validator checks every declared lane against the live machine —
timer active, hook present and executable, extension byte-equal to its source —
and publishes a coverage file that Liaison's parity check reads. A route is
either observed to work or reported as a gap; nothing in that file is authored.

The lanes today:

| Harness | Lane |
|---|---|
| Claude Code | hooks on prompt and tool use, a checkpoint at compaction and session end, and a ten-minute transcript tick |
| Codex | a timer sweeping its rollout files |
| Qwen | a timer sweeping its chat files (its own implementer contract governs the parser) |
| opencode | a timer sweeping its session store, plus an injection plugin |
| Muse Code | a timer sweeping its session store |
| Pi | an extension that queues entries when the agent settles |
| Antigravity | no native session store; captured only when driven through `acpx` |
| any harness driven through `acpx` | a journal sweep; boundaries-only where the native lane already captures the turns, the full journal where it is the only record |
| anything else | a generic text adapter, best-effort |

**Gemini has no capture adapter.** A `GEMINI.md` instruction bridge exists in this
repository and the design documents name Gemini CLI as a foreign client, but no
capture script and no lane exist for it. A Gemini session can read Memoire — that
is exactly what curl parity and the MCP server are for — but its interactive
sessions are not captured natively; the generic text adapter, or an `acpx`
journal sweep when Gemini is driven through `acpx`, are the only routes in.

The adapters are wired by Liaison rather than authored by it: Liaison holds the
registry of which hook goes where on which harness and projects it into each
harness's settings, and Memoire holds the scripts. That is the seam. It means
Memoire's adapters work without Liaison (they are ordinary scripts you can wire by
hand, and the bootstrap prints the Claude Code wiring) while a Liaison-managed
fleet gets them placed consistently across every harness.

## Release constraints

Two constraints shape what this system may become, and both are ratified and
mechanically held.

**Standalone SQLite is ratified and enforced.** The operator directive is on the
record in the R10 design: Memoire should run locally with SQLite, locally with
PostgreSQL, or as a remote hub service with either database — remote-hub-service
is what the author needs, and local SQLite is what could make the app
adoption-friendly (D157). The constraint is in the default test path: a
standalone-SQLite smoke boots the full local column on a scratch home, with zero
PostgreSQL, zero credential store and zero second daemon, and runs one
capture-drain-search loop (D247). **The build fails if anyone introduces a hard
dependency that breaks the adoption column.** The shape being defended reduces to
one sentence: one binary, one `MEMOIRE_HOME`, zero external services. The
packaging artifact is a signed tarball: `make dist` builds it with its checksums,
`make dist-check` validates the existing artifact, and `make dist-sign` is the
operator-present step that produces the detached signature.

**The license is AGPL-3.0-only (D180, 2026-07-28).** The `LICENSE` file and
per-file SPDX headers are in the tree. The framing that appeared while this book
was first commissioned — AGPL as a direction not yet ruled — was closed by that
ruling.

## The seam contract set

These are the documents the projects keep pointing at rather than copying. The
first three are in Memoire's source repository; the rest are on the Liaison side.

- The evidence-adapter contract — the Memoire-side evidence echo, above.
- The mirror protocol — remote custody of Memoire artifacts, and the contract that
  holds the non-local-custody gate closed. Chapter 3's subject.
- The Qwen rollout capture contract — the implementer contract for the Qwen lane.
- `docs/API-SURFACE.json` (public) — the versioned HTTP and MCP surface, enforced
  by contract tests. Chapter 6's reference is checked against it.
- Liaison's retention and redaction policy (liaison:D97), against which Memoire
  keeps a conformance table; and Liaison's evidence-echo contract.

---

*Next: chapter 6, [The CLI & MCP surface](06-the-cli-and-mcp-surface.md) — every
endpoint, tool, and command, checked against the code.*
