# Derive bench

Right-size the derivation model on **your own** sessions. The bench answers one
question per candidate model: can it fully replace the incumbent for nightly
observation derivation? Scoring is comparison-based, not label-based.

The ruled design — decision rule, judging protocol, scorecard — is
`docs/DERIVE-BENCH-DESIGN.md`. This directory is its implementation.

The repo ships the **harness only**. Frozen sessions, derived observations,
packs, ballots and scorecards are your custody: fixtures and incumbent
baselines live in the gitignored `bench/derive/data/`, everything a run
produces under `tmp/derive-bench/<date>/`. Nothing in either tree is ever
committed, and transcripts never leave your store.

## What is here

- `freeze.py` — the eval-set freezer: pulls operator-picked sessions out of
  your store and writes replayable fixtures. Read-only; GETs and nothing else.
- `incumbent.py` — collects the incumbent baseline: your store's historical
  MODEL derivations for those same sessions. Also read-only, and the only
  script after the freezer that touches your store at all.
- `run.py` — the run driver: per candidate × session, seeds a throwaway
  standalone-SQLite home from a fixture and derives through the real seam
  against a named endpoint-registry entry.
- `pack.py` — the comparison-pack builder: aligns the two sides, blinds them,
  and writes a pack, a transcript, and a judge ballot per session. Files to
  files; no store, no network.
- `scorecard.py` — unblinds the filled ballots and aggregates the result.
- `benchlib.py` — the shared client, the fixture format, and the Python port
  of the production near-dup gate.
- `tests/` — the bench's own gate (`make test`), a test-only
  OpenAI-compatible model stub, and the near-dup golden vectors.

## Freeze an eval set

Pick ~10 sessions by hand. The selection criteria are law
(`docs/DERIVE-BENCH-DESIGN.md` §1): diversity over project, session type,
language, length and harness, including at least one thin session as an
abstention probe.

```sh
export MEMOIRE_BASE_URL=http://127.0.0.1:37778   # your store
export MEMOIRE_TOKEN_FILE=$MEMOIRE_HOME/token    # bearer, never printed

bench/derive/freeze.py --set my-eval \
  --session 0f3c…  --session 7ab1…  --session 9d20…
```

Fixtures land in `bench/derive/data/my-eval/`. Freeze once: the input side of
every candidate's run is then byte-identical.

## Run a candidate

A candidate is an entry in your model endpoint registry — any
OpenAI-compatible server (`model-endpoints.example.json` documents the shape).
The endpoint's `api_key_env` names the bearer; export the value before running,
and the driver forwards it into the derive process by name.

```sh
export MEMOIRE_MODEL_ENDPOINTS=$MEMOIRE_HOME/model-endpoints.json
export RAINBOW_API_KEY=…                          # whatever api_key_env names

bench/derive/run.py --candidate qwen35-9b \
  --endpoint-id local-gpu --model qwen3.5-9b --set my-eval
```

One invocation is one candidate over the whole set. Per session it writes
`tmp/derive-bench/<UTC date>/<candidate>/<session>.json`. A refused, failed or
skipped run is recorded as-is and the driver moves on — failure is data
(memoire:D0048), never a retry loop.

### A session derives as LEGS, not as one prompt

The driver mirrors the production sweep (`scripts/memoire-derive-sweep.sh`,
`derive_session` and `derive_window`). The session is first cut at its own
RHYTHM — a long pause, or N canonical events since the last cut, read from the
binary's `-cut-points` helper (memoire:D0261) — and each segment is then
dry-run; a segment whose prompt still exceeds `--max-prompt-bytes` is halved on
the median canonical-event epoch and retried, down to `--split-depth`. Halving
is the fallback, never the first splitter. Both byte defaults are production's
own constants (70000 and 6) and the test suite grep-asserts them against the
script — the incumbent's observations came through that exact algorithm, so a
bench that windowed differently would not be measuring the same chain.
Whole-session single-shot was retired in 2026-08-24 after it sent 45k-368k-token
prompts that production never sends.

Legs run oldest window first, sequentially, because the write-time near-dup
gate compares each leg's candidates against what earlier legs already wrote.

The trailing segment ends at the last event so far, so it is the OPEN TAIL and
its leg is derived with `-provisional` (memoire:D0262): its observations are
"so far" statements a later cut may supersede. A session with no cut is one
open tail, and halving an open tail keeps the marker on the right leg only.

`--no-rhythm-cuts` restores the halving-only lane the bench ran before
2026-09-11, and `--cut-gap-seconds` / `--cut-max-events` /
`--cut-max-segments` override the detector's thresholds. Leave a threshold
unset and the binary governs it, which keeps every rhythm number in one place.

The result therefore carries `legs` — an ordered list of
`{window_start_epoch, window_end_epoch, depth, provisional, segment,
prompt_bytes, genre, derive_ms, outcome, diagnosis, run, coverage_gap}` — with
`observations` aggregated across them in leg order, and `run` a session-level
summary whose `input_hash_vector` is the ordered `[start, end, hash]` of every
leg, beside `rhythm_cuts` and `segments`. A top-level `rhythm` block records
what the detector said: `enabled`, `status` (`ok`, `disabled`, `failed`,
`malformed`), `cuts`, `segments`, the `knobs` that were set, and a `detail`
tail when the pass degraded. A failed or malformed detector never stops the
run — the session derives whole, provisionally, and says so.

`outcome` is what the CLI reported for that leg (`status`, `refused`,
`duplicate`, the written/rejected/unknown counts, and any min-content
`skipped` marker). `diagnosis` is present only when something went wrong and
says WHAT KIND: `refusal` carries the classification, and `failure_stage`
plus a clipped `failure_error` name a provider or parse failure. Nothing
else crosses from the run's metadata — the whitelist is deliberately
narrow, and a new field joins it as a declared schema addition with its
own tests, never as a ride-along.

The refusal classification is the field worth understanding. The CLI's
`-json` emits `refused` as a bare boolean, but the adapter maps two different
events onto it — a `content_filter` finish reason, and a model that answered
with nothing. Those point at opposite conclusions: a prompt adapter plausibly
fixes an empty answer, nothing fixes a safety refusal. So the classification
is read from the run row's `meta_json` instead.

That metadata also holds `raw_response` (memoire:D44) — the model's full reply
to a prompt built from your transcript. The bench copies **only the
classification**, through an explicit whitelist, and the test suite greps every
result file to prove no raw response ever lands in one. A result is a
comparison artifact, not a second home for your transcript. The session `genre` is `ok` only when every
leg is; otherwise it summarizes them (`legs:ok=N,failed=M,skipped=K`) and the
leg genres carry the detail.

A window the split cannot fit — `split_depth_exceeded`, or
`unsplittable_window` when no median divides it — is a **coverage gap**: those
events reached no prompt. The driver refuses such a leg itself, with no CLI
call and therefore no provider request, and records the window. Gaps surface
in the scorecard, never in a pack.

Useful flags: `--session` to run a subset, `--keep-home` to keep each throwaway
home for post-mortem, and `--no-min-content-guard` to derive a window the
production guard would skip. The guard is ON by default — the harness is the
real chain, deviations are opt-in — so a thin abstention-probe session needs
that flag to reach the model at all. The setting is recorded per run.

## Collect the incumbent baseline

The incumbent needs no re-run: its derivations for those sessions are already
in your store.

```sh
export MEMOIRE_TOKEN_FILE=/path/to/an/ADMIN/token    # see below
bench/derive/incumbent.py --set my-eval
```

**This step needs an ADMIN-scoped token, unlike the freezer.** The baseline is
defined by provenance, so every observation's derivation run is fetched through
`GET /v1/derivation-runs/{id}` — an admin route. A client read token gets 403
there and the collection cannot proceed. That split is real and deliberate: a
client token reads memory, an admin token reads how memory was made. Point
`MEMOIRE_TOKEN_FILE` at the admin token for this one command; the freezer is
happy with either.

The baseline is defined by **provenance**, not by review state. Every
observation whose derivation run used a model provider is the incumbent's
answer, whatever state it sits in — a lapsed row is TTL noise, never a negative
label. Rules-path, manual, and unattributable rows are recorded separately
under `unattributed` and never mixed into the compared side.

The one exception is **soft-deleted rows, which never reach a pack** whatever
derived them. Soft-delete is visibility deletion and there is no purge behind
it yet, so a row the operator hid was often hidden for what it contains — and a
pack is a judge-visible artifact handed to model judge sessions. Those rows go
to their own `soft_deleted` bucket, counted but never judged.

Baselines land in `bench/derive/data/my-eval/incumbent/`, beside the fixtures
and under the same custody. All three buckets are written there, so any of
these calls can be revisited without re-collecting from your store.

## Build the comparison packs

```sh
bench/derive/pack.py --set my-eval        # every candidate under today's dir
```

One pack is one session against one candidate; N candidates give N packs per
session. Per pack you get:

```
tmp/derive-bench/<date>/packs/<session>/transcript.md    the frozen session, readable
tmp/derive-bench/<date>/packs/<session>/<nonce>/pack.json
tmp/derive-bench/<date>/packs/<session>/<nonce>/ballot.md
tmp/derive-bench/<date>/sealed/<session>-<nonce>.json    do not open before the ballots are in
```

The two sides are labelled by fresh per-pack nonces and presented in random
order, and the pack directory is named by a nonce rather than by the candidate.
Observations are projected through a four-field whitelist (`kind`, `text`,
`confidence`, `source_refs`), so nothing that identifies a side — review state,
content hash, store ids — can reach a judge by being forgotten. `source_refs`
are event numbers in that session's `transcript.md`: both sides' citations are
remapped into that one key space.

Sides are aligned first with the **production near-dup gate** (the same
thresholds `services/api/internal/modelderive/neardup.go` runs, pinned by
shared golden vectors): pairs that gate are EQUIVALENT and drop out of
judgment, so a judge rules only on genuine deltas.

## Judge, then score

The judging protocol is `docs/DERIVE-BENCH-DESIGN.md` §4: a blinded panel of
independent clean-context model judges fills the ballots, and the operator
rules at the aggregate — the scorecard, never a single ballot. Each judge reads
`pack.json` and `../transcript.md` and nothing else, and fills the block
between the `BALLOT-BEGIN` / `BALLOT-END` markers in `ballot.md`.

```sh
bench/derive/scorecard.py --set my-eval --check-ballots   # validate only
bench/derive/scorecard.py --set my-eval
```

`--check-ballots` parses without scoring: an untouched ballot reads as
*pending*, a malformed one is refused by name. The scorecard unblinds against
`sealed/`, aggregates axis-by-axis wins/ties/losses against the incumbent, and
adds the run metadata and the sanity probe — every operator-accepted incumbent
observation checked mechanically for a near-dup on the candidate side, so a
candidate that misses gems is flagged before a ballot is read.

Two aggregate-level markers are worth knowing apart. A session whose candidates
produced **different leg vectors** did not see the same input (or split it the
same way) and is EXCLUDED from every number, listed under `excluded_sessions`.
A session with **coverage gaps** is still scored — the rest of its legs are
honest work — but the omission axis carries an asterisk, because the candidate
never saw those events while the incumbent did. Output is
`tmp/derive-bench/<date>/scorecard-<candidate>.json` and a readable `.md`
beside it.

## How a run is isolated

Each run builds a fresh `MEMOIRE_HOME` under `bench/derive/data/homes/`, boots
a localhost-only API on it, replays the fixture through `POST /v1/events/batch`,
and derives into that home — the standalone-SQLite posture of memoire:D0247.
Both processes run on a scrubbed environment: `MEMOIRE_DB` is deliberately
absent, so an ambient value can never point the bench at your real store.

Your store is touched read-only, by the freezer, once per eval set.

## Test

```sh
bash bench/derive/tests/derive-bench-test.sh     # also runs in `make test`
```

It stands a scratch memoire column up as a stand-in store, freezes a session
out of it, replays it into a throwaway home, and derives against a local stub
endpoint — no network, no GPU, no real key. It then runs synthetic baselines
and results through the pack builder and the scorecard, asserting the blinding
from the outside: no whitelisted-out field, and no token identifying a side,
may appear anywhere under `packs/`.

The near-dup port has its own gate: `tests/neardup-golden.json` is asserted by
both `tests/neardup-test.py` and `TestNearDupGolden*` in
`services/api/internal/modelderive`. Regenerating those expectations to make a
test pass is a recalibration of the D212 policy, which is an operator ruling.
