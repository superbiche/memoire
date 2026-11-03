#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Comparison-pack builder for the derive bench (docs/DERIVE-BENCH-DESIGN.md §3/§4).

Pure files-to-files: it reads the incumbent baseline collected by incumbent.py
and the candidate results written by run.py, and writes blinded packs, judge
ballots, session transcripts, and the sealed side mapping. It touches no store
and no network.

One pack is one SESSION against ONE candidate. The scorecard counts axis-by-axis
wins/ties/losses against the incumbent and the ballot grammar is a side
preference or a tie, so two sides is the shape the whole protocol is built on;
N candidates produce N packs per session, each independently blinded.

Layout, and the blinding line it enforces:

  <out>/packs/<session>/transcript.md      judge-visible, session-scoped
  <out>/packs/<session>/<pack>/pack.json   judge-visible
  <out>/packs/<session>/<pack>/ballot.md   judge-visible
  <out>/sealed/<session>-<pack>.json       NEVER shown to a judge

The pack directory is named by a fresh nonce, not by the candidate: a candidate
label in a judge-visible path unblinds the pack before it is opened. Nothing
under packs/ ever references sealed/.

Usage:
  pack.py --set my-eval                          # every candidate under today's dir
  pack.py --set my-eval --run-dir tmp/derive-bench/2026-08-24/qwen35-9b
"""

import argparse
import datetime
import json
import os
import secrets
import shutil
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import benchlib  # noqa: E402

BENCH_DIR = os.path.dirname(os.path.abspath(__file__))
REPO_ROOT = os.path.dirname(os.path.dirname(BENCH_DIR))

AXES = ("omission", "grounding", "relevance", "noise", "dedup", "abstention")

AXIS_QUESTIONS = {
    "omission": "did a side miss something the transcript clearly warranted?",
    "grounding": "is any observation unsupported by the transcript?",
    "relevance": "are the deltas worth remembering, or filler?",
    "noise": "proposal volume vs. substance, side by side",
    "dedup": "intra-set near-duplicates that survived the gate",
    "abstention": "on a thin session: did the side stay quiet?",
}

BALLOT_BEGIN = "<!-- BALLOT-BEGIN -->"
BALLOT_END = "<!-- BALLOT-END -->"


def parse_args(argv):
    parser = argparse.ArgumentParser(
        prog="pack.py", description="Build blinded comparison packs and judge ballots."
    )
    parser.add_argument("--set", dest="set_name", default="default",
                        help="frozen eval set the runs were made against")
    parser.add_argument("--data-dir", default=os.path.join(BENCH_DIR, "data"),
                        help="fixture + incumbent root (gitignored)")
    parser.add_argument("--out",
                        help="results dir holding the candidates (default "
                             "tmp/derive-bench/<UTC date>)")
    parser.add_argument("--run-dir", dest="run_dirs", action="append", default=[],
                        help="a candidate's result dir; repeatable. Default: every "
                             "subdirectory of --out carrying a _summary.json")
    parser.add_argument("--session", dest="sessions", action="append", default=[],
                        help="restrict packing to these sessions; repeatable")
    parser.add_argument("--force", action="store_true",
                        help="replace an existing pack for the same candidate and "
                             "session instead of refusing (drops its ballot)")
    parser.add_argument("--disputed-floor", type=float, default=benchlib.DISPUTED_FLOOR,
                        help="bench-local floor under which a pair is not shown "
                             "side by side (NOT calibrated law; see benchlib)")
    return parser.parse_args(argv)


def default_out_dir():
    return os.path.join(
        REPO_ROOT, "tmp", "derive-bench",
        datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d"),
    )


def discover_run_dirs(out_dir):
    """Candidate dirs are the ones run.py left a summary in."""
    found = []
    for name in sorted(os.listdir(out_dir)):
        path = os.path.join(out_dir, name)
        if name in ("packs", "sealed"):
            continue
        if os.path.isdir(path) and os.path.exists(os.path.join(path, "_summary.json")):
            found.append(path)
    return found


def load_candidate(run_dir, session_id):
    path = os.path.join(run_dir, benchlib.safe_name(session_id) + ".json")
    if not os.path.exists(path):
        return None, path
    try:
        return benchlib.read_json(path), path
    except ValueError as err:
        raise benchlib.BenchError(f"candidate result {path} is not valid JSON: {err}")


def existing_pack(out_dir, session_id, candidate_label):
    """The sealed record of a pack already built for this candidate × session.

    Exactly one pack per pair is the invariant the scorecard depends on: it
    reads EVERY record under sealed/, so a second pack for the same pair would
    be counted as a second, independent comparison — inflating the pack count,
    double-weighting whichever ballots were filled, and letting a stale ballot
    from a superseded pack vote alongside its replacement.
    """
    sealed_dir = os.path.join(out_dir, "sealed")
    if not os.path.isdir(sealed_dir):
        return None
    for name in sorted(os.listdir(sealed_dir)):
        if not name.endswith(".json"):
            continue
        path = os.path.join(sealed_dir, name)
        try:
            record = benchlib.read_json(path)
        except (OSError, ValueError):
            continue
        if (record.get("session_id") == session_id
                and record.get("candidate", "") == candidate_label):
            return path, record
    return None


def drop_pack(out_dir, sealed_path, record):
    """Remove a superseded pack: its judge-visible dir AND its sealed record.

    Both, or the invariant breaks in a new way — an orphaned sealed record
    would still be scored with no ballot to read, and an orphaned pack dir
    would leave a fillable ballot that unblinds to nothing. The session's
    transcript.md is shared by every pack of that session and stays.
    """
    pack_dir = os.path.join(out_dir, "packs", *record["pack_id"].split("/"))
    shutil.rmtree(pack_dir, ignore_errors=True)
    os.remove(sealed_path)


def ordinals_from_fixture(fixture):
    """Hub event id -> fixture ordinal, the shared key space of both sides.

    The ordinal is the event's position in `sort_events` order — the order
    derivation itself sorts its input, and the order the transcript numbers.
    """
    return {
        int(event["id"]): n
        for n, event in enumerate(benchlib.sort_events(fixture["events"]), 1)
    }


def ordinals_from_replay(event_ids):
    return {int(event_id): n for n, event_id in enumerate(event_ids, 1)}


def remap_refs(refs, table):
    """Project one side's source refs into fixture ordinals.

    A ref that does not resolve is COUNTED, never fatal: an incumbent
    derivation is historical, and the event it cited may since have been
    purged, soft-deleted, or dropped out of the canonical type list the fixture
    froze. The pack records the count so a judge's grounding read is not
    silently built on a shortened citation list.
    """
    out, unresolved = set(), 0
    for ref in refs or []:
        ordinal = table.get(int(ref))
        if ordinal is None:
            unresolved += 1
            continue
        out.add(ordinal)
    return sorted(out), unresolved


def project_observation(record, table):
    """The judge-visible observation schema — a WHITELIST, never a strip list.

    Both sides are projected through these four fields and nothing else, so a
    field that only one side can carry cannot leak by being forgotten. What
    this kills, concretely: `review_state` (the candidate's rows are all
    `proposed` and only the incumbent's can be `accepted`), `content_hash` and
    `created_at_epoch` (incumbent-only in practice), store observation ids and
    `derivation_run_id` (hub ids are large, throwaway ids are small), and raw
    `source_event_ids` (same tell). A new field on either side is invisible
    here until someone adds it deliberately.
    """
    refs, unresolved = remap_refs(record.get("source_event_ids"), table)
    return {
        "kind": record.get("kind", ""),
        "text": record.get("text", ""),
        "confidence": record.get("confidence", 0),
        "source_refs": refs,
    }, unresolved


def project_side(records, table):
    projected, unresolved_total = [], 0
    for record in records:
        row, unresolved = project_observation(record, table)
        projected.append(row)
        unresolved_total += unresolved
    return projected, unresolved_total


def prompt_version_drift(incumbent_versions, candidate_versions):
    """Whether the two sides were derived under different prompt versions.

    Both sides are SETS: the incumbent's historical derivations may span
    versions, and a candidate session is a vector of legs that could too.

    Partially answers design-doc open question 2: the incumbent's historical
    derivation may predate a prompt change, which is legitimate but must not be
    invisible. The flag says the sides differ, never which side is which, so it
    is safe in a blinded pack; the versions themselves stay sealed.
    """
    if not incumbent_versions or not candidate_versions:
        return None
    return sorted(set(incumbent_versions)) != sorted(set(candidate_versions))


def build_pack(session_id, project, incumbent, candidate, disputed_floor,
               fixture_table, replay_table):
    left, left_unresolved = project_side(incumbent["observations"], fixture_table)
    right, right_unresolved = project_side(candidate.get("observations") or [],
                                           replay_table)

    equivalent, disputed, left_only, right_only = benchlib.align_sets(
        [row["text"] for row in left], [row["text"] for row in right],
        disputed_floor=disputed_floor,
    )

    # Fresh nonces PER PACK: one cracked pack must not unblind the set, so
    # nothing about this labelling is derived from the session, the candidate,
    # or any other pack.
    nonce_incumbent = secrets.token_hex(6)
    nonce_candidate = secrets.token_hex(6)
    sides = [nonce_incumbent, nonce_candidate]
    if secrets.randbits(1):
        sides.reverse()

    def pair_rows(pairs):
        rows = [
            {"score": pair["score"],
             nonce_incumbent: left[pair["left"]],
             nonce_candidate: right[pair["right"]]}
            for pair in pairs
        ]
        shuffle(rows)
        return rows

    one_side = [{"side": nonce_incumbent, "observation": left[i]} for i in left_only]
    one_side += [{"side": nonce_candidate, "observation": right[j]} for j in right_only]
    # Presentation order is randomized, not just the side labels: a list that
    # always ran incumbent-first would hand a judge the mapping for free, and
    # a fixed order is a position bias even when it does not leak.
    shuffle(one_side)

    pack = {
        "pack_version": benchlib.PACK_VERSION,
        "bench": "derive",
        "session_id": session_id,
        "project": project,
        "transcript": "../transcript.md",
        "sides": sides,
        "metadata": {
            "neardup_lo_threshold": benchlib.NEARDUP_LO_THRESHOLD,
            "neardup_hi_threshold": benchlib.NEARDUP_HI_THRESHOLD,
            "disputed_floor": disputed_floor,
            "equivalent_count": len(equivalent),
            "disputed_count": len(disputed),
            "one_side_count": len(one_side),
            # Aggregate on purpose: a per-side count would say which side is
            # the historical one, since only hub refs can go stale.
            "unresolved_source_refs": left_unresolved + right_unresolved,
            "prompt_version_drift": prompt_version_drift(
                incumbent.get("prompt_versions") or [],
                (candidate.get("run") or {}).get("prompt_versions") or [],
            ),
            "transcript_events": incumbent["fixture"]["events"],
        },
        # EQUIVALENT pairs cleared the production near-dup policy and drop out
        # of judgment; they are listed so a judge can see what was collapsed.
        "equivalent": pair_rows(equivalent),
        "disputed": pair_rows(disputed),
        "one_side": one_side,
    }
    return pack, nonce_incumbent, nonce_candidate


def shuffle(items):
    """Fisher-Yates over `secrets`, never `random`.

    `random` is seeded from reproducible state and a recovered seed reproduces
    every ordering decision in the run — which is the whole blinding.
    """
    for i in range(len(items) - 1, 0, -1):
        j = secrets.randbelow(i + 1)
        items[i], items[j] = items[j], items[i]


def render_transcript(fixture):
    """A readability projection of the fixture — no filtering, no truncation.

    Deliberately a SUPERSET of what the derivation prompt renders: the prompt
    projects a few payload fields per event type, while a judge asking "did a
    side miss something the transcript clearly warranted?" must be able to see
    everything that was available to be missed.
    """
    events = benchlib.sort_events(fixture["events"])
    lines = [
        f"# Transcript — session {fixture['session_id']}",
        "",
        f"Project: `{fixture['project']}` · Events: {len(events)} · "
        f"Frozen: {fixture.get('frozen_at_utc', '?')}",
        "",
        "Chronological, in the order derivation sorts its input. The event",
        "numbers below are the key space the pack's `source_refs` use.",
        "",
    ]
    for ordinal, event in enumerate(events, 1):
        epoch = int(event["occurred_at_epoch"])
        stamp = datetime.datetime.fromtimestamp(
            epoch, datetime.timezone.utc
        ).replace(tzinfo=None).isoformat() + "Z"
        lines += [
            "---",
            "",
            f"## Event {ordinal} — {event['event_type']} — epoch {epoch} ({stamp})",
            "",
        ]
        text = (event.get("text") or "").strip()
        if text:
            lines += ["### text", "", text, ""]
        payload = event.get("payload")
        if payload:
            lines += [
                "### payload",
                "",
                "```json",
                json.dumps(payload, indent=2, sort_keys=True, ensure_ascii=False),
                "```",
                "",
            ]
        if not text and not payload:
            lines += ["_(this event carries neither text nor payload)_", ""]
    return "\n".join(lines) + "\n"


def render_ballot(pack, pack_id):
    """The judge ballot — prose for the human, one strict block for the parser.

    The answer block is shown FILLED (in the example) so a model judge copies a
    shape rather than inventing one, and it is delimited by comment markers so
    the parser never has to guess which fenced block is the answer.
    """
    left, right = pack["sides"]
    axes_table = "\n".join(
        f"| `{axis}` | {AXIS_QUESTIONS[axis]} |" for axis in AXES
    )
    example = json.dumps(
        {
            "ballot_version": benchlib.BALLOT_VERSION,
            "pack_id": "example/0000",
            "judge": "example-judge",
            "axes": {
                "omission": {"prefer": "example-aaaa",
                             "evidence": "the other side never mentions the rollback in event 12"},
                "grounding": {"prefer": "tie", "evidence": "both cite events that support them"},
                "relevance": {"prefer": "example-bbbb", "evidence": "example-aaaa's deltas are filler"},
                "noise": {"prefer": "tie", "evidence": ""},
                "dedup": {"prefer": "example-bbbb", "evidence": "example-aaaa repeats itself twice"},
                "abstention": {"prefer": "tie", "evidence": "the session is not thin"},
            },
        },
        indent=2,
    )
    answer = json.dumps(
        {
            "ballot_version": benchlib.BALLOT_VERSION,
            "pack_id": pack_id,
            "judge": "",
            "axes": {axis: {"prefer": "", "evidence": ""} for axis in AXES},
        },
        indent=2,
    )
    return f"""# Judge ballot — pack `{pack_id}`

## What to read

- `pack.json` in this directory — two observation sets, already aligned.
- `../transcript.md` — the session both sets were derived from.

Read those two files and nothing else. Do not open other packs, other
directories, or the store.

## The two sides

This pack's sides are labelled `{left}` and `{right}`. The labels are fresh
random nonces. **Do not attempt to work out which label is which model, and do
not let a guess influence a preference** — judge the content, not its origin,
and not its style. One side's phrasing being more polished is not a reason to
prefer it.

`pack.json` gives you three lists:

- `equivalent` — pairs the production near-dup gate collapsed. They are the
  same observation said twice; they are shown for context and are **not** what
  you are judging.
- `disputed` — pairs that are similar but did not collapse. Judge these.
- `one_side` — an observation only one side produced. Judge these.

Every observation carries `source_refs`: event numbers in `../transcript.md`.

## The six axes

| axis | question |
|---|---|
{axes_table}

**Grounding is disqualifying.** An observation the transcript does not support
is worse than a missing one — a memory layer that invents is not usable at any
price. Weigh that axis accordingly.

For each axis, name the side you prefer by its label, or `"tie"`, plus at most
ONE line of evidence. Leave `evidence` empty rather than padding it.

## How to fill this ballot

Replace the block between the `BALLOT-BEGIN` and `BALLOT-END` markers below.
Keep the markers, keep the fence, keep every axis. `prefer` must be exactly
`"{left}"`, `"{right}"`, or `"tie"`. `evidence` is a single line.

Filled example (fake labels — do not copy these values):

```json
{example}
```

Your ballot:

{BALLOT_BEGIN}
```json
{answer}
```
{BALLOT_END}
"""


def pack_session(args, out_dir, session_id, incumbent, fixture, run_dir):
    candidate, candidate_path = load_candidate(run_dir, session_id)
    if candidate is None:
        return None

    replay = candidate.get("replay") or {}
    event_ids = replay.get("event_ids")
    observations = candidate.get("observations") or []
    if not event_ids and (replay.get("ok") or observations):
        raise benchlib.BenchError(
            f"candidate result {candidate_path} carries no replay.event_ids, so its "
            "observations' source events cannot be mapped onto the fixture — "
            "re-run the candidate with the current run.py"
        )

    candidate_label = candidate.get("candidate", "")
    prior = existing_pack(out_dir, session_id, candidate_label)
    if prior and not args.force:
        raise benchlib.BenchError(
            f"{candidate_label or '(unlabelled candidate)'} already has a pack for "
            f"session {session_id}: {prior[0]}. Packing again would add a SECOND "
            "comparison for the same pair, and the scorecard counts every sealed "
            "record — pass --force to replace it (its ballot is dropped), or use a "
            "fresh --out directory"
        )
    if prior:
        drop_pack(out_dir, *prior)

    pack_nonce = secrets.token_hex(6)
    pack_id = f"{benchlib.safe_name(session_id)}/{pack_nonce}"
    pack, nonce_incumbent, nonce_candidate = build_pack(
        session_id, incumbent["project"], incumbent, candidate,
        args.disputed_floor, ordinals_from_fixture(fixture),
        ordinals_from_replay(event_ids or []),
    )
    pack["pack_id"] = pack_id

    session_dir = os.path.join(out_dir, "packs", benchlib.safe_name(session_id))
    pack_dir = os.path.join(session_dir, pack_nonce)
    benchlib.write_json(os.path.join(pack_dir, "pack.json"), pack)

    transcript_path = os.path.join(session_dir, "transcript.md")
    if not os.path.exists(transcript_path):
        os.makedirs(session_dir, exist_ok=True)
        with open(transcript_path, "w", encoding="utf-8") as fh:
            fh.write(render_transcript(fixture))

    with open(os.path.join(pack_dir, "ballot.md"), "w", encoding="utf-8") as fh:
        fh.write(render_ballot(pack, pack_id))

    # The sealed side of the protocol: everything the scorecard needs to
    # unblind and everything the pack had to omit. It is a SIBLING of packs/,
    # never a child, and no judge-visible file names it.
    benchlib.write_json(
        os.path.join(out_dir, "sealed",
                     f"{benchlib.safe_name(session_id)}-{pack_nonce}.json"),
        {
            "pack_id": pack_id,
            "session_id": session_id,
            "project": incumbent["project"],
            "candidate": candidate_label,
            "model": candidate.get("model", ""),
            "endpoint_id": candidate.get("endpoint_id", ""),
            "sides": {nonce_incumbent: "incumbent",
                      nonce_candidate: "candidate"},
            "candidate_result": os.path.relpath(candidate_path, REPO_ROOT),
            "incumbent_prompt_versions": incumbent.get("prompt_versions") or [],
            "candidate_prompt_versions":
                (candidate.get("run") or {}).get("prompt_versions") or [],
            # Sealed, never packed: only one side can have coverage gaps, so a
            # pack-level marker would unblind. The scorecard asterisks the
            # omission axis from here instead.
            "candidate_coverage_gap":
                (candidate.get("run") or {}).get("coverage_gap", False),
            "candidate_coverage_gap_windows":
                (candidate.get("run") or {}).get("coverage_gap_windows") or [],
            "candidate_genre": candidate.get("genre", ""),
            "counts": {
                "incumbent_observations": len(incumbent["observations"]),
                "incumbent_unattributed": len(incumbent.get("unattributed") or []),
                # Both buckets are counted here and packed nowhere: the
                # operator sees how much of the baseline was held back, while
                # the judge-visible tree never learns they exist.
                "incumbent_soft_deleted": len(incumbent.get("soft_deleted") or []),
                "candidate_observations": len(observations),
                "equivalent": pack["metadata"]["equivalent_count"],
                "disputed": pack["metadata"]["disputed_count"],
                "one_side": pack["metadata"]["one_side_count"],
            },
            "incumbent_by_review_state": incumbent["counts"]["by_review_state"],
        },
    )
    return pack_id, pack_dir


def main(argv):
    args = parse_args(argv)
    out_dir = args.out or default_out_dir()
    if not os.path.isdir(out_dir):
        raise benchlib.BenchError(f"results dir {out_dir} does not exist — run run.py first")
    run_dirs = args.run_dirs or discover_run_dirs(out_dir)
    if not run_dirs:
        raise benchlib.BenchError(
            f"no candidate result dirs under {out_dir} (a candidate dir carries a "
            "_summary.json) — pass --run-dir explicitly"
        )

    set_dir = os.path.join(args.data_dir, benchlib.safe_name(args.set_name))
    index_path = os.path.join(set_dir, "set.json")
    if not os.path.exists(index_path):
        raise benchlib.BenchError(f"eval set {args.set_name!r} is not frozen ({index_path} missing)")
    sessions = args.sessions or benchlib.read_json(index_path).get("sessions") or []

    print(f"pack: set={args.set_name} candidates={len(run_dirs)} "
          f"sessions={len(sessions)} floor={args.disputed_floor} -> {out_dir}")

    built = 0
    for session_id in sessions:
        incumbent_file = benchlib.incumbent_path(args.data_dir, args.set_name, session_id)
        if not os.path.exists(incumbent_file):
            print(f"  {session_id}: SKIP (no incumbent baseline — run incumbent.py)")
            continue
        incumbent = benchlib.read_json(incumbent_file)
        fixture = benchlib.load_fixture(
            benchlib.fixture_path(args.data_dir, args.set_name, session_id)
        )
        for run_dir in run_dirs:
            result = pack_session(args, out_dir, session_id, incumbent, fixture, run_dir)
            if result is None:
                print(f"  {session_id}: no result in {run_dir}")
                continue
            pack_id, pack_dir = result
            built += 1
            print(f"  {session_id}: pack {pack_id} -> {pack_dir}")

    print(f"pack: {built} pack(s); sealed mapping under {os.path.join(out_dir, 'sealed')}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except benchlib.BenchError as err:
        print(f"pack: {err}", file=sys.stderr)
        sys.exit(1)
