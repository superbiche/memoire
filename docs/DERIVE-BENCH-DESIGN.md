# Derive bench — right-size the derivation model on your own sessions

Status: DESIGN (2026-08-24). Owner gesture: `derive-observations`.
Origin: operator ruling 2026-08-23/24 — the bench is a first-class citizen
of this repo so any memoire user can bench candidate models on their own
sessions; scoring is comparison-based, not label-based.

## Purpose and decision rule

Nightly observation derivation runs on a brain-class model. The bench
answers one question per candidate: **can this cheaper model fully replace
the incumbent for derivation?** Decision rule, judged from the comparison
packs (below):

- **REPLACE** — the candidate is at parity or better on omission and
  grounding, and not materially worse on noise/dedup/abstention. Full
  swap; escalation tiers are not the goal.
- **STAGE** — parity is near-miss in a way a prompt adapter or a
  specialist model plausibly fixes; park with the measured gap named.
- **KEEP BRAIN** — material omission or grounding loss. Record the pack
  as evidence and stop.

The incumbent needs no re-run: its historical derivations for the frozen
sessions are already in the store and serve as the baseline side of every
pack.

## The harness is memoire's own derivation

No synthetic harness. A candidate is an endpoint-registry entry (D0075,
`model-endpoints.json`; OpenAI-compatible contract D0053; per-endpoint
model allowlist D0065) pointing at any OpenAI-compatible server. A bench
run re-derives frozen sessions through the real seam (D0162/D0245) into a
**throwaway standalone-SQLite home** (D0247 posture, `MEMOIRE_HOME` set to
a scratch dir) — so the measured thing is the production prompt, parser,
dedup gate, and abstention behavior, not a proxy.

## What ships vs what stays local

The repo ships the **harness only**: freezer, run driver, pack builder,
judge-ballot templates, scorecard schema. It never ships data. Frozen
sessions (raw transcripts), derived observations, packs, ballots, and
scorecards are the user's custody: fixtures live under the bench's local
data dir (gitignored), results under `tmp/derive-bench/<date>/`. This is
the same privacy line as the rest of the repo — transcripts do not leave
the user's store.

## Anatomy

### 1. Eval-set freezer

Input: a list of session ids the operator cherry-picks. Selection
criteria (the pick is an operator act at bench time, the criteria are
law): diversity over project, session type (implementation, ops/incident,
review, discussion/governance), language (include non-English if the
corpus has it), length, and harness; include at least one thin session as
an **abstention probe** (a good model derives little or nothing from it).
~10 sessions is the working size.

The freezer pulls each session's raw events from the user's store and
writes them as a replayable local fixture. `GET /v1/events/tool-window` is
the only session-scoped event query on the read surface, so it does the
ENUMERATING — once per canonical input event type, paged by re-anchoring
`start` on the last epoch seen, since it caps at 500 rows and returns no
cursor. Its record shape drops the payload for anything that is not a tool
call, so each enumerated id is then fetched whole through
`GET /v1/events/{id}`. Declared limitation: a session with 500 or more
same-type events inside one epoch second is not freezable — the capped,
cursorless surface cannot tell exactly-500 from truncation, so the freezer
refuses loudly instead of risking a silently incomplete fixture; pick
another session. Frozen once, reused for every candidate — the input
side of every run is byte-identical.

### 2. Run driver

Per candidate × session: seed a fresh throwaway home from the fixture,
run derivation through the seam against the candidate's endpoint
(dry-run/execute per D0076), collect the proposed observation set plus
run metadata (timings, failure genres, unmapped ratio). A failed or
partial run is data, not a retry loop (D0048): it lands in the scorecard
as-is.

**A session is not one prompt.** The driver mirrors how the production
sweep CUTS a session into windows (`scripts/memoire-derive-sweep.sh`,
brief amendment 2026-07-10). Two splitters act, in this order:

1. **Rhythm first** (D0261, Fibery #343, 2026-09-08). The session is cut
   at its own rhythm — a long pause between consecutive canonical events,
   or N canonical events since the last cut — read from the binary's
   `-cut-points` helper, and each segment is derived as its own run.
   Thresholds are calibration values the binary owns; the bench passes a
   `--cut-gap-seconds` / `--cut-max-events` / `--cut-max-segments` knob
   only when it is set, so unset means the binary governs and there is one
   place to change a number. `--no-rhythm-cuts` restores the halving-only
   lane.
2. **The byte ceiling second.** Each window is dry-run first, and any
   window whose `prompt_bytes` still exceeds the ceiling is halved on the
   median canonical-event epoch, recursively to a depth cap, each half its
   own idempotent run. A rhythm cut IS a split, so segments enter the
   halving walk at depth 1. The ceiling default is production's **70000**
   and the depth cap **6** — the same constants, pinned against the script
   by the bench's own gate.

The trailing segment ends at the last event so far, so it is the **open
tail**: its leg carries `-provisional`, which marks the run and its
observations as "so far" statements a later cut may supersede (D0262,
Fibery #344). A session with no cut is one open tail, fail-open paths
included; halving an open tail keeps the marker on the right leg only.
Each leg records both `provisional` (what the driver asked for) and
`run.provisional` (what the store recorded), and the session result
carries a `rhythm` block — status, cuts, segments, knobs — beside its
input-hash vector.

Legs execute oldest window first, sequentially: the write-time near-dup
gate compares each leg's candidates against what earlier legs already
wrote, so leg order is part of the determinism contract.

The law here is unchanged — the driver mirrors the production sweep,
because the incumbent's observations came through exactly the sweep's
algorithm. Only the description of what production does has caught up with
what D0261 already ruled. Bench runs made before 2026-09-11, including the
2026-09-09 reference runs, were **halving-only**: their conclusions about
window composition describe the pre-#343 lane.

Whole-session single-shot derivation is RETIRED as unrepresentative
(2026-08-24, measured): it built 45k-368k-token prompts that production
never sends, and failed outright on 6 of the 11 reference-set sessions.
A window the split cannot fit — depth cap reached, or no median to split
on — is recorded as a coverage gap and surfaced at the scorecard, never in
a pack: only the candidate side can have gaps, so a pack-level marker
would unblind the comparison.

### 3. Comparison-pack builder

One pack is one session against ONE candidate: the incumbent's historical
observation set beside that candidate's set, **blinded** (sides labeled by
nonce, mapping kept sealed until ballots are in — the D183 pilot's
protocol). N candidates therefore produce N packs per session; two sides is
the shape the ballot grammar and the wins/ties/losses-vs-incumbent
scorecard are both built on.

Before judging, observation sets are **aligned** mechanically with the
near-dup gate the store already runs at write time (D212 — lexical trigram
Jaccard with the calibrated two-tier policy, NOT embeddings: the embed
lane is a hub-only retrieval concern and has no bearing here). Pairs that
clear the policy are marked EQUIVALENT and drop out of judgment; the judge
rules only on genuine deltas (present-in-one-side observations and
disputed pairs — similar but below the policy).

### 4. Judge protocol

Session-driven, never script-driven: a brain (the operator, or an agent
session under the fleet's LLM-call law) reads each pack beside the frozen
transcript and fills a ballot per session. Axes, each judged on the
deltas:

| axis | question |
|---|---|
| omission | did a side miss something the transcript clearly warranted? |
| grounding | is any observation unsupported by the transcript? |
| relevance | are the deltas worth remembering, or filler? |
| noise | proposal volume vs. substance, side by side |
| dedup | intra-set near-duplicates that survived the gate |
| abstention | on thin sessions: did the side stay quiet? |

Ballot output: per axis, a side preference (or tie) plus at most one line
of evidence. Judging protocol (operator-ruled 2026-08-24): a **blinded
panel of independent clean-context model judges** fills the ballots (the
D183 shape); the **operator rules at the aggregate** — the scorecard
level, never per-ballot. One careful human judge remains acceptable for a
personal bench outside the reference run.

### 5. Scorecard

Unblind, aggregate per candidate: axis-by-axis wins/ties/losses vs. the
incumbent across sessions, run metadata, and the sanity probe result.
The scorecard plus the packs is the evidence a REPLACE ruling cites.
Everything lands under `tmp/derive-bench/<date>/`.

### Sanity probe (weak positive labels)

Operator-accepted observations are rare but real ground truth: for
sessions in the eval set that carry accepted observations, every side is
mechanically checked for a near-dup match to each accepted gem. A
candidate that misses gems the incumbent found is flagged before any
ballot is read. Lapsed observations are TTL noise, never a negative
label — nothing is scored against them.

## Serving candidates

Any OpenAI-compatible endpoint works — the registry entry is the whole
integration. For the reference setup the candidates run on a workstation
llama.cpp `llama-server` started per bench run and stopped after (the
superbiche-bench repo's `my-floor-endpoint` skill is the lifecycle
cookbook: start unit, warm-poll `/v1/models`, verify, stop — referenced
textually by design, D0235). A model that wins the bench then gets a
proper always-available serving profile in the user's fleet: production
derivation must not depend on a workstation being powered.

## Candidate staging

1. **Qwen3.5-9B** first — generalist, no prompt adaptation, answers the
   calibrating question ("can ~9B do this at all?").
2. Extraction/memory specialists (NuExtract3-4B, MemReader-4B) only if
   stage 1 says small models are in range: each needs a prompt-adapter
   slice (their I/O shapes differ from the derive prompt) — future work,
   deliberately not designed here.

## Placement and classification

Design doc: this file. Implementation home: `bench/derive/` (new
first-class top-level tree — the bench is a user-facing product surface,
not repo plumbing), classified `public bench/` in `export/manifest.txt`
as a prefix grant; its local data dir is gitignored and therefore outside
the manifest. Whether `bench/` scripts join the D188 dist set is a
separate ruling — default no until the bench has run once end-to-end.

## Open questions

1. ~~Fixture→throwaway-home seeding: replay through the queue format vs.
   `POST /v1/events/batch` against a local API.~~ CLOSED at slice 1:
   **batch POST**. It is not the weaker of the two — `CreateEventBatch` is a
   loop over the same `queuewriter.WriteBytes` the file queue uses, so the
   HTTP path exercises the queue AND the ingest validation above it, with no
   queue-file naming knowledge in the bench. One event per request, staggered
   a couple of milliseconds: pending files are named
   `<unix-millis>-<adapter>-<sha256>.json` and drained in sorted-name order,
   so same-millisecond writes could permute event ids — which feed
   `CanonicalInputHash` and the prompt order of events sharing an epoch
   second. The stagger makes id assignment a function of the fixture alone,
   which is what makes two candidates comparable.
2. Does the incumbent baseline need ONE re-run through the frozen fixture
   (same prompt version as candidates) when the historical derivation
   predates a prompt change? Check prompt-version provenance at slice 1.
   PARTIALLY ANSWERED at slice 2, still OPEN. The provenance is now
   mechanized rather than assumed: `incumbent.py` records the incumbent's
   `prompt_version` set per session, and the pack builder raises
   `prompt_version_drift` when it differs from the candidate run's. The flag
   is deliberately blinding-safe — it says the two sides differ, never which
   is which — and the versions themselves stay in the sealed mapping. What
   stays open is the RULING: whether a drifted session is judged as-is with
   the gap named in the scorecard, or whether the incumbent gets one re-run
   at the candidates' prompt version. That call needs a first real bench run
   to say how many sessions actually drift; measuring it is now free.
3. Bench CLI shape: one `bench/derive/run.sh` orchestrator vs. make
   targets — follow scripts/ conventions at implementation.
4. The hub host's serving unit for Qwen3.5-9B: reuse the superbiche-bench floor
   unit pattern or a dedicated `llama-server-derive-bench` unit (VRAM
   contention with the coder floor when both run).
