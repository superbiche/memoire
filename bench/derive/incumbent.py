#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Incumbent baseline collector for the derive bench (docs/DERIVE-BENCH-DESIGN.md §3).

The incumbent needs no re-run: its historical derivations for the frozen
sessions are already in the user's store and serve as the baseline side of
every comparison pack. This script materializes that side as a result-shaped
file per session, so pack.py sees two files of the same shape and never has to
know that one came from a hub and the other from a throwaway home.

Read-only against the store: GETs and nothing else. This is the ONLY
hub-touching code in the pack/ballot/scorecard slice.

The set is defined by PROVENANCE, not by "everything the store holds for this
session":

  - a row whose derivation run ran on a MODEL provider is the incumbent's
    answer, whatever its review state — lapsed is TTL noise (memoire:D136 R-6),
    never a negative label, and rejected rows are simply rare;
  - a row from the rules path, a manual marker, or with no reachable run is
    recorded under `unattributed` and annotated. It is never silently mixed
    into the side a judge compares;
  - a SOFT-DELETED row is excluded from the judged side outright, whatever
    derived it (operator ruling, 2026-08-24). See classify_row().

All three buckets are written to the local baseline file, which is the user's
own gitignored custody — so a ruling about any of them can be revisited
without re-collecting from the store.

Usage:
  incumbent.py --set my-eval                    # every session of the set
  incumbent.py --set my-eval --session <id>     # a subset; repeatable

Environment (the repo's usual client knobs):
  MEMOIRE_BASE_URL   store to read from (default http://$MEMOIRE_ADDR)
  MEMOIRE_TOKEN_FILE bearer token file (default $MEMOIRE_HOME/token)
"""

import argparse
import datetime
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import benchlib  # noqa: E402

BENCH_DIR = os.path.dirname(os.path.abspath(__file__))
SOFT_DELETED = "soft_deleted"


def parse_args(argv):
    parser = argparse.ArgumentParser(
        prog="incumbent.py",
        description="Collect the incumbent baseline for a frozen eval set.",
    )
    parser.add_argument("--set", dest="set_name", default="default",
                        help="frozen eval set to collect for")
    parser.add_argument("--session", dest="sessions", action="append", default=[],
                        help="restrict collection to these sessions; repeatable")
    parser.add_argument("--data-dir", default=os.path.join(BENCH_DIR, "data"),
                        help="fixture root (gitignored; default bench/derive/data)")
    parser.add_argument("--base-url", default=benchlib.default_base_url(),
                        help="store base URL (default MEMOIRE_BASE_URL)")
    parser.add_argument("--token-file", default=benchlib.default_token_file(),
                        help="bearer token file (default MEMOIRE_TOKEN_FILE)")
    parser.add_argument("--force", action="store_true",
                        help="re-collect sessions that already have a baseline")
    return parser.parse_args(argv)


class RunCache:
    """Per-run provenance, fetched once per derivation run id.

    A session's observations cluster into a handful of runs, so the run fetch
    is cached; a run that cannot be read (purged, or a row pointing at nothing)
    is remembered as missing rather than re-requested per observation.
    """

    def __init__(self, client):
        self.client = client
        self.runs = {}

    def get(self, run_id):
        if run_id not in self.runs:
            try:
                row = self.client.derivation_run(run_id)
            except benchlib.BenchError:
                row = {}
            self.runs[run_id] = {
                "provider": row.get("provider", ""),
                "model": row.get("model", ""),
                "endpoint_id": row.get("endpoint_id", ""),
                "status": row.get("status", ""),
                "prompt_version": row.get("prompt_version", ""),
                "derivation_version": row.get("derivation_version", ""),
                "input_hash": row.get("input_hash", ""),
                "data_boundary": row.get("data_boundary", ""),
                "created_at_epoch": row.get("created_at_epoch", 0),
                "reachable": bool(row),
            }
        return self.runs[run_id]


def collect_observations(client, project, session_id):
    """Every observation of the session, in every review state, noise included.

    Fetched state by state on purpose: the list surface returns ACCEPTED rows
    when no review_state is given, widens to accepted/proposed/lapsed under
    `include_proposed`, and reaches rejected/superseded/disputed/soft_deleted
    only when the state is named. Rows are keyed by id so a state that overlaps
    another cannot double-count.
    """
    by_id = {}
    for state in benchlib.REVIEW_STATES:
        for record in client.observations(project, session_id, review_state=state):
            by_id[int(record["id"])] = record
    return [by_id[key] for key in sorted(by_id)]


def annotate(record, runs):
    """Project one store row into the baseline shape, with its run provenance."""
    run_id = record.get("derivation_run_id") or ""
    run = runs.get(run_id, {}) if run_id else {}
    return {
        "id": int(record["id"]),
        "kind": record.get("kind", ""),
        "text": record.get("text", ""),
        "confidence": record.get("confidence", 0),
        "review_state": record.get("review_state", ""),
        "content_hash": record.get("content_hash", ""),
        "provenance": record.get("provenance", ""),
        "noise_candidate": record.get("noise_candidate", 0),
        "can_instruct": record.get("can_instruct", 0),
        "source_event_ids": record.get("source_event_ids") or [],
        "created_at_epoch": record.get("created_at_epoch", 0),
        "derivation_run_id": run_id,
        "provider": run.get("provider", ""),
        "model": run.get("model", ""),
        "prompt_version": run.get("prompt_version", ""),
        "derivation_version": run.get("derivation_version", ""),
    }


def classify_row(review_state, run):
    """Which bucket one observation belongs to. Pure, so the law is testable.

    The soft-delete check comes FIRST, ahead of the provenance check, and the
    order is the whole point. Soft-delete is visibility deletion only — this
    repo has no purge/rebuild yet (Working Rules), so a row the operator hid
    was often hidden because of what it CONTAINS. A pack is a judge-visible
    artifact handed to model judge sessions, so resurfacing a deliberately
    hidden row there would republish it to an audience the hiding was meant to
    exclude. No provenance makes that acceptable, which is why no provenance is
    consulted before the exclusion.
    """
    if review_state == SOFT_DELETED:
        return SOFT_DELETED
    if run.get("provider") in benchlib.MODEL_PROVIDERS:
        return "model"
    return "unattributed"


def unattributed_reason(run_id, run):
    if not run_id:
        return "no_derivation_run"
    if not run.get("reachable"):
        return "run_unreachable"
    return "non_model_provider:" + (run.get("provider") or "unknown")


def collect_session(client, fixture_path, session_id):
    fixture = benchlib.load_fixture(fixture_path)
    project = fixture["project"]
    runs = RunCache(client)

    model_side, unattributed, soft_deleted = [], [], []
    by_state, by_provider = {}, {}
    for record in collect_observations(client, project, session_id):
        run_id = record.get("derivation_run_id") or ""
        run = runs.get(run_id) if run_id else {}
        row = annotate(record, {run_id: run} if run_id else {})
        state = row["review_state"] or "unknown"
        by_state[state] = by_state.get(state, 0) + 1
        provider = run.get("provider") or "(none)"
        by_provider[provider] = by_provider.get(provider, 0) + 1
        bucket = classify_row(row["review_state"], run)
        if bucket == "model":
            model_side.append(row)
        elif bucket == SOFT_DELETED:
            row["reason"] = "soft_deleted_never_judged"
            soft_deleted.append(row)
        else:
            row["reason"] = unattributed_reason(run_id, run)
            unattributed.append(row)

    prompt_versions = sorted({r["prompt_version"] for r in model_side if r["prompt_version"]})
    derivation_versions = sorted(
        {r["derivation_version"] for r in model_side if r["derivation_version"]}
    )
    return {
        "incumbent_version": benchlib.INCUMBENT_VERSION,
        "bench": "derive",
        "side": "incumbent",
        "project": project,
        "session_id": session_id,
        "collected_at_utc": datetime.datetime.now(datetime.timezone.utc)
        .replace(microsecond=0).isoformat(),
        "fixture": {
            "path": os.path.relpath(fixture_path, BENCH_DIR),
            "frozen_at_utc": fixture.get("frozen_at_utc", ""),
            "events": fixture["counts"]["events"],
        },
        # The incumbent's prompt_version set is recorded here and compared
        # against each candidate run's by the pack builder: a historical
        # derivation that predates a prompt change is still a legitimate
        # baseline, but the pack must say so (design doc open question 2).
        "prompt_versions": prompt_versions,
        "derivation_versions": derivation_versions,
        "counts": {
            "model": len(model_side),
            "unattributed": len(unattributed),
            "soft_deleted": len(soft_deleted),
            "by_review_state": by_state,
            "by_provider": by_provider,
        },
        "runs": runs.runs,
        # `observations` is the ONLY key the pack builder reads. The two
        # buckets below stay here for the operator and are never packed.
        "observations": model_side,
        "unattributed": unattributed,
        "soft_deleted": soft_deleted,
    }


def main(argv):
    args = parse_args(argv)
    set_dir = os.path.join(args.data_dir, benchlib.safe_name(args.set_name))
    index_path = os.path.join(set_dir, "set.json")
    if not os.path.exists(index_path):
        raise benchlib.BenchError(
            f"eval set {args.set_name!r} is not frozen ({index_path} missing) — "
            "run freeze.py first"
        )
    sessions = args.sessions or benchlib.read_json(index_path).get("sessions") or []
    if not sessions:
        raise benchlib.BenchError(f"eval set {args.set_name!r} lists no sessions")

    client = benchlib.Client(args.base_url, benchlib.read_token(args.token_file))
    health = client.health()
    print(f"incumbent: store {args.base_url} version={health.get('version', '?')} "
          f"set={args.set_name} sessions={len(sessions)}")

    for session_id in sessions:
        path = benchlib.incumbent_path(args.data_dir, args.set_name, session_id)
        if os.path.exists(path) and not args.force:
            print(f"  {session_id}: already collected ({path}) — use --force to redo")
            continue
        fixture_path = benchlib.fixture_path(args.data_dir, args.set_name, session_id)
        if not os.path.exists(fixture_path):
            print(f"  {session_id}: SKIP (no fixture at {fixture_path})")
            continue
        baseline = collect_session(client, fixture_path, session_id)
        benchlib.write_json(path, baseline)
        counts = baseline["counts"]
        print(f"  {session_id}: {counts['model']} model observation(s), "
              f"{counts['unattributed']} unattributed, "
              f"{counts['soft_deleted']} soft-deleted (never judged), "
              f"prompt_versions={baseline['prompt_versions'] or ['(none)']} -> {path}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except benchlib.BenchError as err:
        print(f"incumbent: {err}", file=sys.stderr)
        sys.exit(1)
