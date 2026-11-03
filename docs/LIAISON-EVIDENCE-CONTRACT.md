# Liaison Evidence Adapter Contract

Date: 2026-07-18
Authority: liaison:D218 (first-class ingest, atomic cross-repo landing),
memoire:D136 R-2 (contractual redaction) + R-3 (queue-accept ack), liaison:D242
(canonical `liaison` key). This document is the Memoire-side contract of record
for the `liaison-evidence` source adapter. The liaison-side counterpart is
`liaison/EVIDENCE-ECHO-CONTRACT.md` (its `Memoire fact:` marker path is retired
by dated correction).

## Purpose

Liaison's graduation finalize echoes each appended `EvidenceObservation` into
Memoire as a first-class `liaison_evidence` event (envelope v2, below). This
replaces the retired v1 convention that impersonated
`source_adapter=claude-code` with a `Memoire fact:` prompt marker consumed by
R001.

> Status (2026-11-01, liaison L0005): the read-back half of this seam is
> retired. The event is still written into Memoire's custody, but its R005
> derivation into an accepted `fact` ended with L0010 (Hindsight owns derived
> memory), and Liaison's `memoire_feed` adapter — the reader of that derived
> row — was deleted (liaison L0005). There is no derived row left to read back.

Authority ruling (liaison:D218): the Liaison evidence store is authoritative
for evidence. A Memoire-side rejection or skip of an echoed observation
affects memory only, never graduation.

## Event contract (envelope v2)

Both transports MUST produce a queue envelope with exactly these values:

- `schema_version`: `1`
- `project`: `liaison` by default (canonical top-level key, liaison:D242);
  overridable only by finalize `--project` when non-empty.
- `session_id`: `liaison-evidence-echo-<task-ref-slug>` — slug lowercases
  `task_ref`, collapses non-alphanumeric runs to `-`, trims `-`, falls back to
  `untitled`.
- `source_adapter`: exactly `liaison-evidence`.
- `event_type`: exactly `liaison_evidence`.
- `source_event_id`: `liaison-evidence-echo:<hash16>` — first 16 lowercase hex
  chars of SHA-256 over the compact envelope JSON (the exact `text` bytes).
- `cwd`: the finalize repo root.
- `text`: exactly the compact envelope JSON. Single-line UTF-8 JSON object,
  sorted keys, compact separators, parseable by `json.loads`. NO marker
  prefix of any kind.
- `payload`: the envelope as a JSON object (same content as `text`, parsed).
- `metadata`: JSON object with at least:
  - `liaison_evidence_echo`: `true`
  - `schema_version`: `1`
  - `actual_source`: `liaison.graduation.finalize`
  - `liaison_redaction_rules_version`: `"redact-v1"` — liaison's declaration
    that it pre-redacted this envelope to that ruleset (see Redaction).
  - `liaison_redactions`: OPTIONAL, present only when liaison's pre-redaction
    rewrote something: array of `{"rule": <id>, "count": <n>}`, sorted by rule.

Envelope JSON shape (unchanged from v1; D41 feed shape untouched):

```json
{"liaison_evidence_observation": {"artifact_ref": "...", "evidence_type": "...",
 "judge_verdict": "...", "supersedes": [], "task_ref": "...",
 "task_shape": {"operation_kind": "...", "oracle": "...", "risk_tier": "..."},
 "timestamp": "...", "ttl_review": ""}, "schema_version": 1}
```

## Derivation (Memoire side, rule R005)

> RETIRED (2026-11-01, L0010): R005 no longer runs — Memoire's derivation lane
> is retired and Hindsight owns derived memory, so the `liaison_evidence` event
> is captured as raw but no longer derived into a `fact`. The spec below is the
> historical R005 rule, kept for the event-shape record; the event contract
> above is unchanged and still written by Liaison's echo.

`services/api/internal/derive/rules.go` R005 "Liaison Evidence Ingest" v1 (historical):

- Gate: `source_adapter == "liaison-evidence"` AND `event_type` equals
  `liaison_evidence` (case-insensitive) AND `text` parses as a JSON object
  containing the key `liaison_evidence_observation`.
- Emit: `kind=fact`, `review_state=accepted`, `confidence=1.0`, text = the
  event `text` (trimmed). Derived rows get `can_evidence=1`,
  `noise_candidate=0` via the standard derived-observation defaults, so they
  are feed-eligible exactly like the retired R001 path.
- A gated-out or malformed event produces NO candidate (skip, not error) —
  memory-side rejection affects memory only (liaison:D218).
- R001's marker table is untouched: `Memoire fact:` remains a legitimate
  operator marker in real Claude prompts. What ends is liaison's use of it.

## Redaction (memoire:D136 R-2)

- The contract ruleset is `redact-v7` — the shared fleet contract
  `~/.agents/shared/contracts/redact-v7.md`, implemented by the central Go
  core Caviar (`github.com/superbiche/caviar/redact` v0.3.0, the public
  module; operator rulings a18, a81/a82), which
  `services/api/internal/redact` forwards to (`RulesVersion`, pin
  `redact: 7`). Correction 2026-10-12 (rulings a81/a82): `redact-v6` →
  `redact-v7`, the over-redaction fix from the hub sample, consumed from the
  public Caviar module. Correction 2026-10-10 (rulings a18/a28): `redact-v5` →
  `redact-v6`, the v5 consumers' over-redaction fix; Memoire carries no
  redaction logic of its own. Correction 2026-10-08 (operator rulings
  Q1–Q8, langfuse-fleet session; Memoire migrates first): Memoire went
  straight from `redact-v2` to `redact-v5` (v3 and v4 were superseded
  unadopted). Correction 2026-10-07 (Fibery #616 decision 3): `redact-v1` →
  `redact-v2` after adding `json-secret-field`, `url-credentials` and the
  marker-ownership predicate. Liaison still declares
  `liaison_redaction_rules_version: "redact-v1"` until it adopts
  `redact-v7` through Caviar's Python distribution (the `caviar-redact`
  wheel, exact version; skipping v2 to v6); that mismatch is tolerated — the
  violation stamp below is hit-based, never a version comparison.
  Distribution is the CONTRACT, not keys: liaison adopts the contract's
  implementation verbatim and uses plain-SHA-256
  markers on its internal path; it never holds Memoire's keyed-marker HMAC
  secret. Marker format on both
  sides: `[REDACTED:<rule>:<hash8>]`; markers are exempt from re-redaction
  (idempotent), so a liaison-redacted envelope passes Memoire's backstop
  untouched.
- Liaison MUST pre-redact the envelope (all string values and keys) to
  the ruleset it declares (`redact-v1` today, `redact-v7` once adopted) before
  send, and declares it via
  `metadata.liaison_redaction_rules_version`. Pre-redaction REPLACES the v1
  reject-on-risk behavior: secret-shaped content is masked, not refused.
- Memoire's queue-writer backstop stays enabled on both transports (defense
  in depth). If the backstop records ANY redaction hit on an event whose
  `source_adapter` is `liaison-evidence`, it additionally sets
  `metadata.redaction_contract_violation = true`. A backstop rewrite on this
  adapter's path is a detectable contract violation, never a silent mutation.
- Violations are surfaced as `redaction_contract_violations` (a count of
  ingested events carrying the marker) on `GET /v1/health`.

## Ack semantics (memoire:D136 R-3, queue-accept ack)

Echo success means the write has demonstrably reached Memoire's custody — a
QUEUE-ACCEPT ack, not an ingestion ack:

- Local transport: `memoire-queue` exit 0 with the pending-file path on
  stdout. The file is durably in `queue/pending/` (O_EXCL + fsync + rename).
- HTTP transport: `POST /v1/events` returning `202` with
  `{"queued": true, "queue_id": ...}` (the server funnels through the same
  queue writer).

Ingestion is asynchronous (in-process drainer, ~2s tick; if the API acks, the
queue is drained by definition). Failure is loud on both transports: non-zero
exit / non-2xx maps to a failed echo reason, and finalize reports it
(fail-soft: finalize's exit code never changes). The poison residual — a
malformed envelope moved to `queue/failed/` with an `.error.json` sidecar — is
assumed visible-asynchronous; `GET /v1/health` exposes `queue_failed_depth`.

## Transport selection (liaison side)

By `MEMOIRE_BASE` (default `http://127.0.0.1:37778/v1`): if the host is
`127.0.0.1`, `localhost`, or `::1` (or the variable is unset), use the local
queue binary; otherwise POST the envelope to `{base}/events` with
`Authorization: Bearer` from `MEMOIRE_TOKEN` or `~/.memoire/token`. This is
deliberate pre-work for the extraction question and nothing more (memoire:D136
scope fence).

## Versioning

Any change to the envelope fields, the derivation gate, the redaction ruleset
name, or the ack semantics requires a dated correction here AND in
`liaison/EVIDENCE-ECHO-CONTRACT.md`, plus a journal entry (qualified
D-numbers). The redaction ruleset version bumps in Caviar first and reaches
Memoire as an exact `go.mod` bump forwarded by `services/api/internal/redact`;
liaison follows the pattern set, never the reverse.

Correction log:

- 2026-10-12 — `redact-v6` → `redact-v7` (Memoire; operator rulings a81
  (Caviar's public release) and a82). The shared contract `redact-v7.md` owns
  the table (row 27 `env-secret-assignment` and row 26 `mysql-p-flag`
  narrowed, P1's quoted-value closer bounded; the hub sample's
  over-redaction) and its corpus; marker grammar
  unchanged. Memoire's forwarding package now imports the public module
  `github.com/superbiche/caviar` at exactly v0.3.0, fetched from the public
  proxy; the committed file proxy `third_party/goproxy` and its anti-drift
  test are retired (`records/history/L0008-redact-v7-public-caviar.md`).
  Liaison keeps declaring `redact-v1` until it adopts Caviar (Liaison's
  schedule).

- 2026-10-10 — `redact-v5` → `redact-v6` (Memoire; rulings a18, a28, a33,
  relayed by langfuse-fleet, migration order Memoire → Liaison → gateway).
  The shared contract `redact-v6.md` owns the table (rows 18/20/25/27/28
  changed: the v5 consumers' over-redaction; references are not values after
  any key) and its 507-seed corpus. Memoire's implementation becomes a
  forwarding package over Caviar v0.2.0, served from the committed file proxy
  `third_party/goproxy` (`records/history/L0005-redact-v6-consumes-caviar.md`).
  Marker format unchanged. Liaison keeps declaring `redact-v1` until it adopts
  Caviar (mirror correction in liaison `EVIDENCE-ECHO-CONTRACT.md`, Liaison's
  schedule).

- 2026-10-08 — `redact-v2` → `redact-v5` (Memoire; operator rulings Q1–Q8
  relayed by langfuse-fleet, migration order Memoire → Liaison → gateway).
  The shared contract `redact-v5.md` owns the 28-row table, procedures P0–P4,
  the 8-character floor and the exemption list; the key family gains
  `authorization`, `auth`, `cookie`; new rules: Google, Stripe, Hugging Face,
  npm, PyPI, Tailscale, age, Grafana shapes, `signed-url-param`,
  `header-credential`, `json-schema-default`, `json-secret-object`,
  `cli-flag-secret`, `mysql-p-flag`, `bare-bearer-token`; the v2 assignment
  rule's newline-crossing `\s*` (leak class `std-21`) is gone. Marker format
  unchanged. Liaison keeps declaring `redact-v1` until it adopts the reference
  module (mirror correction in liaison `EVIDENCE-ECHO-CONTRACT.md`, Liaison's
  schedule).
- 2026-10-07 — `redact-v1` → `redact-v2` (Memoire). Added rules:
  `json-secret-field`, `url-credentials`; added the marker-ownership predicate.
  Marker format unchanged. Liaison keeps declaring `redact-v1` until its port
  lands (mirror correction in liaison `EVIDENCE-ECHO-CONTRACT.md`, same date).
