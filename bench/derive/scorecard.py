#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Scorecard for the derive bench (docs/DERIVE-BENCH-DESIGN.md §5).

Unblinds the filled ballots against the sealed mapping and aggregates, per
candidate: axis-by-axis wins/ties/losses versus the incumbent across sessions,
run metadata, the sanity probe, and the alignment counts. The scorecard plus
the packs is the evidence a REPLACE ruling cites.

Pure files-to-files, like pack.py: no store, no network.

Usage:
  scorecard.py --set my-eval                    # today's results dir
  scorecard.py --set my-eval --check-ballots    # parse and validate only

Outputs land beside the packs, under the user's custody:
  <out>/scorecard-<candidate>.json
  <out>/scorecard-<candidate>.md
"""

import argparse
import datetime
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import benchlib  # noqa: E402
import pack as packlib  # noqa: E402

BENCH_DIR = os.path.dirname(os.path.abspath(__file__))
REPO_ROOT = os.path.dirname(os.path.dirname(BENCH_DIR))


class BallotError(Exception):
    """One ballot file the scorecard refuses, named with its reason."""


def parse_args(argv):
    parser = argparse.ArgumentParser(
        prog="scorecard.py", description="Unblind ballots and score a derive-bench run."
    )
    parser.add_argument("--set", dest="set_name", default="default",
                        help="frozen eval set the runs were made against")
    parser.add_argument("--data-dir", default=os.path.join(BENCH_DIR, "data"),
                        help="fixture + incumbent root (gitignored)")
    parser.add_argument("--out",
                        help="results dir (default tmp/derive-bench/<UTC date>)")
    parser.add_argument("--check-ballots", action="store_true",
                        help="parse and validate the ballots, write no scorecard")
    return parser.parse_args(argv)


def parse_ballot(text, sides, pack_id):
    """Read one filled ballot under a strict grammar.

    The grammar is deliberately unforgiving: a judge's ballot is evidence, and
    a silently mis-parsed axis is a wrong ruling that looks like a right one. A
    ballot whose axes are ALL blank is not malformed, it is unfilled — that
    distinction keeps a mid-flight bench from reading as broken.
    """
    if text.count(packlib.BALLOT_BEGIN) != 1 or text.count(packlib.BALLOT_END) != 1:
        raise BallotError("exactly one BALLOT-BEGIN/BALLOT-END marker pair is required")
    body = text.split(packlib.BALLOT_BEGIN, 1)[1].split(packlib.BALLOT_END, 1)[0]
    if "```" not in body:
        raise BallotError("the answer block between the markers is not a fenced block")
    fenced = body.split("```", 2)
    if len(fenced) < 3:
        raise BallotError("the fenced answer block is not closed")
    payload = fenced[1]
    if "\n" in payload:
        payload = payload.split("\n", 1)[1]  # drop the fence's info string
    try:
        ballot = json.loads(payload)
    except ValueError as err:
        raise BallotError(f"the answer block is not valid JSON: {err}")

    if not isinstance(ballot, dict):
        raise BallotError("the answer block must be a JSON object")
    if ballot.get("ballot_version") != benchlib.BALLOT_VERSION:
        raise BallotError(
            f"ballot_version is {ballot.get('ballot_version')!r}, this scorecard "
            f"speaks {benchlib.BALLOT_VERSION}"
        )
    # Bind the ballot to the pack it was written for. Without this the parser
    # attributes a ballot to whatever pack its FILE happens to sit in, so a
    # copied or misplaced ballot silently votes on a comparison it never saw —
    # and the sealed record it unblinds against is the wrong one.
    if ballot.get("pack_id") != pack_id:
        raise BallotError(
            f"pack_id is {ballot.get('pack_id')!r} but this ballot belongs to "
            f"{pack_id!r} — a ballot cannot be moved between packs"
        )
    axes = ballot.get("axes")
    if not isinstance(axes, dict):
        raise BallotError("`axes` is missing or not an object")
    unknown = sorted(set(axes) - set(packlib.AXES))
    if unknown:
        raise BallotError(f"unknown axes {unknown}")
    missing = [axis for axis in packlib.AXES if axis not in axes]
    if missing:
        raise BallotError(f"missing axes {missing}")

    allowed = set(sides) | {"tie"}
    verdicts, blanks = {}, 0
    for axis in packlib.AXES:
        entry = axes[axis]
        if not isinstance(entry, dict):
            raise BallotError(f"axis {axis!r} is not an object")
        prefer = entry.get("prefer", "")
        evidence = entry.get("evidence", "")
        if not isinstance(prefer, str) or not isinstance(evidence, str):
            raise BallotError(f"axis {axis!r}: `prefer` and `evidence` must be strings")
        if "\n" in evidence:
            raise BallotError(f"axis {axis!r}: `evidence` must be a single line")
        if prefer == "":
            blanks += 1
            verdicts[axis] = {"prefer": "", "evidence": evidence}
            continue
        if prefer not in allowed:
            raise BallotError(
                f"axis {axis!r}: `prefer` is {prefer!r}, which is neither a side "
                f"label of this pack nor \"tie\""
            )
        verdicts[axis] = {"prefer": prefer, "evidence": evidence}

    if blanks == len(packlib.AXES):
        # Still untouched: the template ships `judge` empty, so an unfilled
        # ballot is not yet required to name one.
        return {"status": "pending", "judge": ballot.get("judge", ""), "axes": verdicts}
    if blanks:
        raise BallotError(
            f"{blanks} axis/axes left blank — a ballot is filled completely or not at all"
        )
    # A ballot carrying verdicts must say who cast them: the panel protocol
    # rules at the aggregate over independent judges, and an unattributable
    # ballot cannot be audited back to one.
    judge = ballot.get("judge")
    if not isinstance(judge, str) or not judge.strip():
        raise BallotError(
            f"`judge` is {judge!r} — a filled ballot must name the judge who cast it"
        )
    return {"status": "filled", "judge": judge.strip(), "axes": verdicts}


def load_sealed(out_dir):
    sealed_dir = os.path.join(out_dir, "sealed")
    if not os.path.isdir(sealed_dir):
        raise benchlib.BenchError(
            f"no sealed mapping under {sealed_dir} — run pack.py first"
        )
    out = []
    for name in sorted(os.listdir(sealed_dir)):
        if name.endswith(".json"):
            out.append(benchlib.read_json(os.path.join(sealed_dir, name)))
    if not out:
        raise benchlib.BenchError(f"sealed mapping {sealed_dir} is empty")
    return out


def ballot_path(out_dir, pack_id):
    return os.path.join(out_dir, "packs", *pack_id.split("/"), "ballot.md")


def unblind(verdicts, sides):
    """Map each axis's nonce preference onto incumbent / candidate / tie."""
    out = {}
    for axis, entry in verdicts.items():
        prefer = entry["prefer"]
        out[axis] = {
            "winner": "tie" if prefer == "tie" else sides.get(prefer, "unknown"),
            "evidence": entry["evidence"],
        }
    return out


def sanity_probe(incumbent, candidate_observations):
    """Weak positive labels: operator-accepted observations are real ground truth.

    Every accepted incumbent observation is checked mechanically against the
    candidate side with the SAME near-dup gate the pack aligned with. A
    candidate missing gems the incumbent found is flagged before any ballot is
    read. Lapsed rows are TTL noise (memoire:D136 R-6) and are never probed —
    nothing is scored against them.
    """
    gems = [row for row in incumbent["observations"]
            if row.get("review_state") == "accepted"]
    texts = [row.get("text", "") for row in candidate_observations]
    missed = []
    for gem in gems:
        best = benchlib.neardup_best(gem.get("text", ""), texts)
        if best["hit_index"] < 0:
            missed.append({
                "kind": gem.get("kind", ""),
                "text": gem.get("text", ""),
                "best_score": best["best_score"],
            })
    return {
        "accepted_gems": len(gems),
        "matched": len(gems) - len(missed),
        "missed": missed,
        "flagged": bool(missed),
    }


def load_summaries(out_dir):
    """Every candidate's run summary, keyed by candidate label."""
    summaries = {}
    for run_dir in packlib.discover_run_dirs(out_dir):
        summary = benchlib.read_json(os.path.join(run_dir, "_summary.json"))
        summaries[summary.get("candidate", os.path.basename(run_dir))] = summary
    return summaries


def input_hash_consistency(summaries):
    """The LEG VECTOR must be identical for every candidate over one fixture.

    A session is no longer one derivation but an ordered list of windows, so
    the comparable unit is the ordered `[window_start, window_end, input_hash]`
    vector, not a single hash. Skipped legs ride along with an empty hash on
    purpose.

    Comparing the whole vector buys a second guarantee for free: the split
    boundaries are computed from median canonical-event epochs in a throwaway
    store holding exactly the fixture, so they are a function of the fixture
    alone. Two candidates whose vectors match therefore saw the same input AND
    split it the same way; a mismatch means one of those two failed, and either
    way the session is not comparable.
    """
    by_session = {}
    for candidate, summary in summaries.items():
        for run in summary.get("runs") or []:
            vector = run.get("input_hash_vector") or []
            if vector:
                # Tuples so the vector is hashable for the set comparison; JSON
                # round-trips it back to lists for the report.
                by_session.setdefault(run["session_id"], {})[candidate] = tuple(
                    tuple(leg) for leg in vector
                )
    divergent = {
        session: vectors for session, vectors in by_session.items()
        if len(set(vectors.values())) > 1
    }
    return {"by_session": by_session, "divergent": divergent,
            "consistent": not divergent}


def coverage_gaps(candidate, summaries):
    """Windows this candidate structurally never saw (design §2, memoire:D0048).

    A leg refused at the split-depth cap, or one whose window could not be
    split at all, means those events reached no prompt. In a pack that reads as
    the candidate omitting something — unfairly, because it never had the
    chance, and the incumbent did.

    This is FLAGGED, never excluded: unlike an input-hash divergence, the
    session's remaining legs are honest work and their ballots count. The
    scorecard asterisks the omission axis so a reader weighs it knowingly.
    """
    gaps = []
    for run in (summaries.get(candidate, {}).get("runs") or []):
        if run.get("coverage_gap"):
            gaps.append({
                "session_id": run["session_id"],
                "windows": run.get("coverage_gap_windows") or [],
            })
    return gaps


def score_candidate(candidate, entries, summaries, consistency):
    axes = {axis: {"candidate": 0, "incumbent": 0, "tie": 0} for axis in packlib.AXES}
    sessions, pending, alignment = [], [], {"equivalent": 0, "disputed": 0, "one_side": 0}
    probes, drift_sessions, genres = [], [], {}
    prompt_versions = set()

    excluded = []
    for entry in entries:
        session_id = entry["sealed"]["session_id"]
        # A session whose candidates did not see the same canonical input is
        # NOT COMPARABLE, and input_hash_consistency's own contract says so.
        # Scoring its ballots would let a REPLACE ruling rest on two models
        # answering different questions, so it leaves every aggregate — axes,
        # ballot counts, alignment, run metadata and the probe alike — and is
        # named in `excluded_sessions` instead. Excluded and said out loud, not
        # refused: a divergence is data (memoire:D0048), and the comparable
        # sessions in the same run stay usable.
        if session_id in consistency["divergent"]:
            excluded.append({"session_id": session_id,
                             "pack_id": entry["sealed"]["pack_id"],
                             "reason": "input_hash_divergent"})
            continue
        counts = entry["sealed"]["counts"]
        for key in alignment:
            alignment[key] += counts.get(key, 0)
        genre = entry["sealed"].get("candidate_genre", "")
        genres[genre] = genres.get(genre, 0) + 1
        prompt_versions.update(
            v for v in (entry["sealed"].get("candidate_prompt_versions") or []) if v)
        if entry["drift"]:
            drift_sessions.append(entry["sealed"]["session_id"])
        probes.append({"session_id": entry["sealed"]["session_id"], **entry["probe"]})

        if entry["ballot"]["status"] != "filled":
            pending.append(entry["sealed"]["pack_id"])
            continue
        rulings = unblind(entry["ballot"]["axes"], entry["sealed"]["sides"])
        for axis, ruling in rulings.items():
            axes[axis][ruling["winner"]] = axes[axis].get(ruling["winner"], 0) + 1
        sessions.append({
            "session_id": entry["sealed"]["session_id"],
            "pack_id": entry["sealed"]["pack_id"],
            "judge": entry["ballot"]["judge"],
            "axes": rulings,
        })

    summary = summaries.get(candidate, {})
    # Timings follow the same exclusion: a total that quietly included a
    # session no other number counts would misstate what was measured.
    runs = [run for run in (summary.get("runs") or [])
            if run.get("session_id") not in consistency["divergent"]]
    return {
        "bench": "derive",
        "candidate": candidate,
        "model": summary.get("model", ""),
        "endpoint_id": summary.get("endpoint_id", ""),
        "scored_at_utc": datetime.datetime.now(datetime.timezone.utc)
        .replace(microsecond=0).isoformat(),
        # Totals count COMPARABLE packs only; `excluded_sessions` carries the
        # rest so the gap between them is never silent.
        "packs": len(entries) - len(excluded),
        "packs_excluded": len(excluded),
        "excluded_sessions": excluded,
        "ballots_filled": len(sessions),
        "ballots_pending": pending,
        # Wins/ties/losses are stated FROM THE CANDIDATE'S SIDE: "candidate"
        # counts the axes where the judge preferred it over the incumbent.
        "axes": axes,
        "alignment": alignment,
        "runs": {
            "genres": genres,
            "derive_ms": {
                "total": sum(int(run.get("derive_ms", 0)) for run in runs),
                "by_session": {run["session_id"]: run.get("derive_ms", 0)
                               for run in runs},
            },
            "prompt_versions": sorted(prompt_versions),
            "prompt_version_drift_sessions": sorted(drift_sessions),
            "input_hash_consistent": consistency["consistent"],
            "input_hash_divergent_sessions": sorted(consistency["divergent"]),
        },
        # Flagged, not excluded — the counterpart of excluded_sessions above.
        "coverage_gaps": coverage_gaps(candidate, summaries),
        "sanity_probe": {
            "flagged_sessions": sorted(
                probe["session_id"] for probe in probes if probe["flagged"]
            ),
            "by_session": probes,
        },
        "sessions": sessions,
    }


def render_markdown(card):
    axes_rows = "\n".join(
        f"| `{axis}` | {card['axes'][axis]['candidate']} | "
        f"{card['axes'][axis]['tie']} | {card['axes'][axis]['incumbent']} |"
        for axis in packlib.AXES
    )
    probe = card["sanity_probe"]
    probe_rows = "\n".join(
        f"| `{row['session_id']}` | {row['matched']}/{row['accepted_gems']} | "
        f"{'FLAGGED' if row['flagged'] else 'ok'} |"
        for row in probe["by_session"]
    ) or "| _(no sessions)_ | | |"
    missed = []
    for row in probe["by_session"]:
        for gem in row["missed"]:
            missed.append(
                f"- `{row['session_id']}` ({gem['kind']}, best near-dup "
                f"{gem['best_score']:.3f}): {gem['text']}"
            )
    runs = card["runs"]
    lines = [
        f"# Derive-bench scorecard — candidate `{card['candidate']}`",
        "",
        f"Model `{card.get('model') or '?'}` · endpoint "
        f"`{card.get('endpoint_id') or '?'}` · scored {card['scored_at_utc']}",
        "",
        f"{card['ballots_filled']}/{card['packs']} ballot(s) filled.",
        "",
    ]
    if card["excluded_sessions"]:
        lines += [
            "> **EXCLUDED FROM EVERY NUMBER BELOW.** These sessions' candidates",
            "> did not see the same canonical input, so their runs answer",
            "> different questions and cannot be compared. Nothing about them —",
            "> ballots, axes, alignment or probe — is counted anywhere in this",
            "> scorecard. Re-freeze the eval set and re-run every candidate",
            "> against it before citing these sessions as evidence.",
            ">",
        ] + [
            f"> - `{row['session_id']}` — {row['reason']} (pack `{row['pack_id']}`)"
            for row in card["excluded_sessions"]
        ] + [""]
    if card.get("coverage_gaps"):
        lines += [
            "> **COVERAGE GAPS — read the `omission` axis with an asterisk.**",
            "> On these sessions some windows never reached the model at all:",
            "> the split hit its depth cap, or a window could not be split. The",
            "> candidate never saw those events while the incumbent did, so any",
            "> omission judged against it there is not the model's fault. These",
            "> sessions ARE scored — the rest of their legs are honest work.",
            ">",
        ] + [
            f"> - `{row['session_id']}` — {len(row['windows'])} window(s): "
            + ", ".join(f"{w[0]}..{w[1]} ({w[2]})" for w in row["windows"])
            for row in card["coverage_gaps"]
        ] + [""]
    lines += [
        "## Axes — candidate vs. incumbent",
        "",
        "| axis | candidate | tie | incumbent |",
        "|---|---|---|---|",
        axes_rows,
        "",
        "## Alignment",
        "",
        f"EQUIVALENT (collapsed, not judged): {card['alignment']['equivalent']} · "
        f"disputed: {card['alignment']['disputed']} · "
        f"present in one side only: {card['alignment']['one_side']}",
        "",
        "## Sanity probe — accepted incumbent observations",
        "",
        "| session | matched | |",
        "|---|---|---|",
        probe_rows,
        "",
    ]
    if missed:
        lines += ["Gems the candidate did not match:", ""] + missed + [""]
    lines += [
        "## Run metadata",
        "",
        f"- Genres: {runs['genres'] or '(none)'}",
        f"- Total derive time: {runs['derive_ms']['total']} ms",
        f"- Candidate prompt versions: {runs['prompt_versions'] or ['(unknown)']}",
        f"- Prompt-version drift vs. incumbent: "
        f"{runs['prompt_version_drift_sessions'] or 'none'}",
        f"- input_hash consistent across candidates: "
        f"{'yes' if runs['input_hash_consistent'] else 'NO — ' + str(runs['input_hash_divergent_sessions'])}",
        "",
    ]
    if card["ballots_pending"]:
        lines += [f"Pending ballots: {card['ballots_pending']}", ""]
    return "\n".join(lines) + "\n"


def main(argv):
    args = parse_args(argv)
    out_dir = args.out or packlib.default_out_dir()
    if not os.path.isdir(out_dir):
        raise benchlib.BenchError(f"results dir {out_dir} does not exist")

    sealed_entries = load_sealed(out_dir)
    summaries = load_summaries(out_dir)
    consistency = input_hash_consistency(summaries)

    by_candidate, errors = {}, []
    for sealed in sealed_entries:
        path = ballot_path(out_dir, sealed["pack_id"])
        if not os.path.exists(path):
            errors.append(f"{path}: missing (pack.py wrote no ballot?)")
            continue
        with open(path, "r", encoding="utf-8") as fh:
            text = fh.read()
        try:
            ballot = parse_ballot(text, list(sealed["sides"]), sealed["pack_id"])
        except BallotError as err:
            errors.append(f"{path}: {err}")
            continue

        candidate_result = os.path.join(REPO_ROOT, sealed["candidate_result"])
        observations = []
        if os.path.exists(candidate_result):
            observations = benchlib.read_json(candidate_result).get("observations") or []
        incumbent_file = benchlib.incumbent_path(
            args.data_dir, args.set_name, sealed["session_id"]
        )
        probe = {"accepted_gems": 0, "matched": 0, "missed": [], "flagged": False}
        if os.path.exists(incumbent_file):
            probe = sanity_probe(benchlib.read_json(incumbent_file), observations)

        drift = packlib.prompt_version_drift(
            sealed.get("incumbent_prompt_versions") or [],
            sealed.get("candidate_prompt_versions") or [],
        ) or False
        by_candidate.setdefault(sealed.get("candidate", "?"), []).append(
            {"sealed": sealed, "ballot": ballot, "probe": probe, "drift": drift}
        )

    for error in errors:
        print(f"scorecard: REFUSED {error}", file=sys.stderr)

    if args.check_ballots:
        total = sum(len(entries) for entries in by_candidate.values())
        filled = sum(1 for entries in by_candidate.values()
                     for entry in entries if entry["ballot"]["status"] == "filled")
        print(f"scorecard: {total} ballot(s) parsed, {filled} filled, "
              f"{len(errors)} refused")
        return 1 if errors else 0

    if errors:
        raise benchlib.BenchError(
            f"{len(errors)} ballot(s) refused — fix them or remove them, then re-run"
        )

    for candidate, entries in sorted(by_candidate.items()):
        card = score_candidate(candidate, entries, summaries, consistency)
        stem = os.path.join(out_dir, "scorecard-" + benchlib.safe_name(candidate))
        benchlib.write_json(stem + ".json", card)
        with open(stem + ".md", "w", encoding="utf-8") as fh:
            fh.write(render_markdown(card))
        print(f"scorecard: {candidate}: {card['ballots_filled']}/{card['packs']} "
              f"ballot(s) -> {stem}.json, {stem}.md")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except benchlib.BenchError as err:
        print(f"scorecard: {err}", file=sys.stderr)
        sys.exit(1)
