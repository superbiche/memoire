# The Memoire Great Book

*v0.6 edition — 2026-07-25.*

This is the human-readable manual of Memoire: one written book, one edition per
version, written for the reader who has never seen this system before (D174).
It explains what Memoire is for, how a memory moves through it from a captured
event to a line in front of a model, what custody of that memory actually
means, how to run the service yourself in either of its two shapes, where it
meets its sibling projects, and what its command, HTTP, and MCP surfaces
contain.

What this book is **not**:

- It is not current state. "Where are we" lives in `docs/state/DIGEST.md` (the
  NOW section), regenerated on every ledger mutation.
- It is not a gesture precondition. Agents performing a gesture read their view
  under `docs/views/` — that obligation is not satisfied by this book.
- It is not the audit trail. The ledgers (`DECISIONS.md`, `BURN-DECISIONS.md`)
  are the raw custody; this book cites them by D-number but never replaces
  them.

The edition is written prose, never generated, and versioned with the release
it describes. Edition 1 is the `v0.6.0` snapshot — the state of the system
after the CloudNativePG-on-Kubernetes migration completed on 2026-07-24, which
D172 makes a version boundary. That version's goal, as written in `VERSION`:
*Memoire runs as the ecosystem's hub of record on HL3 — CNPG primary with
standalone-SQLite parity held, capture never blocked from any harness, derive
intake over HTTP, read and injection parity across the fleet.*

Two things about that number were still pending when this edition was drafted,
and are stated here rather than smoothed over. The back-numbering of the six
eras that produced `0.6.0` was a run proposal awaiting the operator's
ratification (D176). And **the `v0.6.0` tag had not been cut** — it was to land
at that same ratification, so the draft described a version that existed in the
working tree and not yet in the tag list. *(Edition correction, 2026-08-05:
both have closed. The operator confirmed the era back-numbering and the
`v0.6.0` goal line on 2026-07-28 (D176), and the tag is in the tag list — this
edition describes a tagged version.)* When the contract changes enough to bump
the version, the next edition is written and this one stays readable at its
tag.

One further honesty note, since this book is written for a reader who may want
to use or redistribute the software: **Memoire's license posture is AGPL-3.0**,
ruled by the operator on 2026-07-28 (memoire:D180). *(Edition correction,
2026-08-05: the artifact caught up with the ruling — the `LICENSE` file and
per-file SPDX headers are committed, landed with the R-10.5 packaging slice.)*
Chapter 5 gives the full status.

## Chapters

1. [Identity & the memory thesis](01-identity-and-the-memory-thesis.md) — the
   founding pain, what Memoire is, and the three commitments the design is
   judged against: sovereignty, foreign-client read parity, capture-first.
2. [The life of a memory](02-the-life-of-a-memory.md) — one event followed all
   the way through: capture, derivation into sealed kinds, governance, review,
   elevation, and injection into a future session.
3. [Custody & redaction](03-custody-and-redaction.md) — what total custody
   means, why deletion is a visibility act, defense-at-source redaction,
   encrypted mirrors, and what is honestly not solved yet.
4. [Operating the service](04-operating-the-service.md) — the two deployment
   shapes, the local⇄hub × SQLite/PostgreSQL axis, tokens and scopes, backups,
   and the scheduled derivation lanes.
5. [The seams](05-the-seams.md) — Memoire as a sibling under contract: the
   Liaison evidence adapter, project identity, the harness capture adapters,
   and the release constraints that shape all of it.
6. [The CLI & MCP surface](06-the-cli-and-mcp-surface.md) — the HTTP read
   surface, the command-line tools, the MCP server's read tools, and the
   capture hooks, verified against the code.
