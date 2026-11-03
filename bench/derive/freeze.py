#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Eval-set freezer for the derive bench (docs/DERIVE-BENCH-DESIGN.md §1).

Pulls the raw canonical-input events of a handful of operator-picked sessions
out of the user's own store and writes them as replayable local fixtures. The
set is frozen ONCE and reused for every candidate, so the input side of every
bench run is byte-identical.

Read-only against the store: the freezer issues GETs and nothing else.

Fixtures are the user's custody — they hold raw transcript content and stay in
the gitignored data dir. They never reach the repo, and the bearer token never
reaches a fixture.

Usage:
  freeze.py --set my-eval --session <id> [--session <id> ...]
  freeze.py --set my-eval --sessions-file sessions.txt

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


def parse_args(argv):
    parser = argparse.ArgumentParser(
        prog="freeze.py", description="Freeze a derive-bench eval set."
    )
    parser.add_argument("--set", dest="set_name", default="default",
                        help="eval-set name (a directory under the data dir)")
    parser.add_argument("--session", dest="sessions", action="append", default=[],
                        help="session id to freeze; repeatable")
    parser.add_argument("--sessions-file",
                        help="file of session ids, one per line ('#' comments)")
    parser.add_argument("--data-dir",
                        default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "data"),
                        help="fixture root (gitignored; default bench/derive/data)")
    parser.add_argument("--base-url", default=benchlib.default_base_url(),
                        help="store base URL (default MEMOIRE_BASE_URL)")
    parser.add_argument("--token-file", default=benchlib.default_token_file(),
                        help="bearer token file (default MEMOIRE_TOKEN_FILE)")
    parser.add_argument("--force", action="store_true",
                        help="re-freeze sessions that already have a fixture")
    return parser.parse_args(argv)


def collect_sessions(args):
    sessions = list(args.sessions)
    if args.sessions_file:
        with open(args.sessions_file, "r", encoding="utf-8") as fh:
            for line in fh:
                line = line.split("#", 1)[0].strip()
                if line:
                    sessions.append(line)
    seen, ordered = set(), []
    for session in sessions:
        if session not in seen:
            seen.add(session)
            ordered.append(session)
    if not ordered:
        raise benchlib.BenchError("no sessions given (--session or --sessions-file)")
    return ordered


def freeze_session(client, session_id):
    """Pull one session's canonical input events into a fixture payload.

    The tool-window query is the session-scoped event surface, but its record
    shape drops the payload for anything that is not a tool call — so it is
    used to ENUMERATE ids per canonical type, and each event is then fetched
    whole through GET /v1/events/{id}.
    """
    meta = {}
    try:
        meta = client.session(session_id)
    except benchlib.BenchError:
        # A session with events but no session row is still freezable; the
        # metadata block is a convenience, not the fixture's substance.
        meta = {}

    events, by_type = [], {}
    for event_type in benchlib.CANONICAL_INPUT_EVENT_TYPES:
        ids = client.tool_window_ids(session_id, event_type)
        by_type[event_type] = len(ids)
        for event_id in ids:
            record = client.event(event_id)
            if not record:
                raise benchlib.BenchError(
                    f"session {session_id}: event {event_id} vanished mid-freeze"
                )
            events.append(record)

    if not events:
        raise benchlib.BenchError(
            f"session {session_id} has no canonical input events — nothing to "
            "derive, so it cannot be a bench session"
        )

    projects = sorted({e.get("project", "") for e in events})
    if len(projects) > 1:
        raise benchlib.BenchError(
            f"session {session_id} spans projects {projects}; derivation runs "
            "are per project, so this session cannot be frozen as one unit"
        )

    events = benchlib.sort_events(events)
    epochs = [int(e["occurred_at_epoch"]) for e in events]
    return {
        "fixture_version": benchlib.FIXTURE_VERSION,
        "frozen_at_utc": datetime.datetime.now(datetime.timezone.utc)
        .replace(microsecond=0)
        .isoformat(),
        "project": projects[0],
        "session_id": session_id,
        "event_types": list(benchlib.CANONICAL_INPUT_EVENT_TYPES),
        "counts": {"events": len(events), "by_type": by_type},
        "epoch_range": {"start": epochs[0], "end": epochs[-1]},
        "session_meta": {
            "client": meta.get("client", ""),
            "started_at_epoch": meta.get("started_at_epoch", 0),
            "ended_at_epoch": meta.get("ended_at_epoch", 0),
            "store_event_count": meta.get("event_count", 0),
            "store_observation_count": meta.get("observation_count", 0),
        },
        "events": events,
    }


def main(argv):
    args = parse_args(argv)
    sessions = collect_sessions(args)
    client = benchlib.Client(args.base_url, benchlib.read_token(args.token_file))

    health = client.health()
    print(f"freeze: store {args.base_url} version={health.get('version', '?')} "
          f"set={args.set_name}")

    frozen = []
    for session_id in sessions:
        path = benchlib.fixture_path(args.data_dir, args.set_name, session_id)
        if os.path.exists(path) and not args.force:
            print(f"  {session_id}: already frozen ({path}) — use --force to redo")
            frozen.append(session_id)
            continue
        fixture = freeze_session(client, session_id)
        benchlib.write_json(path, fixture)
        by_type = ", ".join(
            f"{t}={n}" for t, n in sorted(fixture["counts"]["by_type"].items()) if n
        )
        print(f"  {session_id}: {fixture['counts']['events']} events "
              f"({by_type}) project={fixture['project']} -> {path}")
        frozen.append(session_id)

    index = os.path.join(args.data_dir, benchlib.safe_name(args.set_name), "set.json")
    benchlib.write_json(index, {
        "set": args.set_name,
        "sessions": frozen,
        "updated_at_utc": datetime.datetime.now(datetime.timezone.utc)
        .replace(microsecond=0)
        .isoformat(),
    })
    print(f"freeze: {len(frozen)} session(s) in set '{args.set_name}' -> {index}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except benchlib.BenchError as err:
        print(f"freeze: {err}", file=sys.stderr)
        sys.exit(1)
