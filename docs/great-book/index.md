# The Memoire Great Book

*v0.7 edition — 2026-11-02.*

This is the human-readable manual of Memoire: one written book, one edition per
version, written for the reader who has never seen this system before (D174).
It explains what Memoire is for, how an event moves through it from a hook
firing to a line a model can read back, what custody of that event actually
means, how to run the service yourself, where it meets its sibling projects,
and what its command, HTTP, and MCP surfaces contain.

## What changed since the last edition

The previous edition, v0.6, described a Memoire that derived memory: a model
read the captured record, proposed observations, a human reviewed them, and the
accepted ones were handed back to future sessions. On 2026-11-01 the operator
ruled that this is no longer Memoire's job (L0010, rulings R1 and R2). A
separate system, Hindsight, is now the memory layer that agents consult.
Memoire is the layer underneath it: it **captures** what happened, keeps every
byte in **custody**, is the **source of record** for Hindsight and for audit,
and answers a narrow question itself — *what exactly was said or run, and
when* — as a dated lookup over raw records. It holds no derived memory on its
default read surfaces. The derive and embed lanes that fed the old design were
retired from the deployment the same day.

This edition is written to that scope. The derivation, review and embedding
machinery gets a short historical account in chapter 2, because the code and
the stored rows still exist and a reader will meet their traces; it is not
described as something the system does.

One thing is **stated direction, not behaviour**. L0010 authorizes an
experiment on whether the searchable copy of old raw records could shrink to a
recent window while the full record stays untouched. The experiment has not
run and no eviction exists. Chapters 3 and 4 are expected to get another pass
once it has.

## What this book is not

- It is not current state. "Where are we right now" lives in `VERSION` (the
  version and its goal) and `MISSING.md` (its punch-list). This book is written
  prose and is never generated.
- It is not the rulebook. Agents follow `AGENTS.md`, the tracked contracts and
  the tests and gates in the code.
- It is not the audit trail. The decision ledgers are the raw custody of why
  things are the way they are. This book cites them by number — `D97`,
  `liaison:D281`, `L0010` — and never replaces them. They are not part of the
  public tree.

A note on citations, since you will see them everywhere. A bare `D<n>` is an
entry in Memoire's own decision journal; `L<nnnn>` is a short historical
architecture note; a cross-repository citation carries its prefix, as in
`liaison:D97`. Paths are relative to the repository root. The public tree
carries this book, `README.md`, `QUICKSTART.md`, `MISSING.md`, `VERSION`, the
code, the schemas and `docs/API-SURFACE.json`; the ledgers, the runbook and
the design documents this book mentions by name live in the private source
repository, and are named so that you know they exist, not so that you can open
them.

## Version, license and honesty notes

The version is in `VERSION`, and its one-line goal is the release goal of the
0.7 line: Memoire ships its first public release — a curated export of the
code, this book, the punch-list of known gaps, and the quickstart, with a
signed distribution tarball and a bootstrap script, and honest read surfaces.
This edition describes the tree as it stood on the date above.

Memoire is licensed AGPL-3.0-only (D180); the `LICENSE` file and per-file SPDX
headers are in the tree.

`MISSING.md` is the public punch-list for the current version. Where this book
says something is not solved, `MISSING.md` is where the open items are filed.

## Chapters

1. [Identity & the custody thesis](01-identity-and-the-custody-thesis.md) — the
   founding pain, what Memoire is now that Hindsight is the memory layer, and
   the three commitments the design is judged against: sovereignty,
   foreign-client read parity, capture-first.
2. [The life of a captured event](02-the-life-of-a-captured-event.md) — one
   event followed all the way through: capture, the queue, custody, the raw
   projection, the dated read, the hand-off to Hindsight — and a short account
   of the derivation lane that was retired.
3. [Custody & redaction](03-custody-and-redaction.md) — what total custody
   means, why deletion is a visibility act, redaction at the source, packs and
   encrypted mirrors, and what is honestly not solved yet.
4. [Operating the service](04-operating-the-service.md) — the two deployment
   shapes, the SQLite/PostgreSQL axis, tokens and scopes, the reference
   Kubernetes deployment, backups, and what to watch.
5. [The seams](05-the-seams.md) — Memoire as a sibling under contract:
   Hindsight, Liaison, Conclave, the redaction core, project identity, and the
   harness capture adapters.
6. [The CLI & MCP surface](06-the-cli-and-mcp-surface.md) — the HTTP surface
   with its current defaults, the command-line tools, the MCP server's four
   read tools, and the capture hooks, verified against the code.
