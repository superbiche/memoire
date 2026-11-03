#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Wrap parser/performance fixtures in the native child-to-parent contract."""

import json
import os
from pathlib import Path
import re
import sys
import uuid


raw = sys.stdin.read()
hook = Path(__file__).resolve().parents[1] / "memoire-subagent-context-codex.sh"
try:
    request = json.loads(raw)
    source = Path(request["transcript_path"]).absolute()
    original = source.read_bytes()
except (ValueError, KeyError, OSError):
    # Invalid input must reach the hook unchanged.
    import subprocess
    raise SystemExit(subprocess.run([str(hook)], input=raw, text=True).returncode)

parent_id = str(uuid.uuid5(uuid.NAMESPACE_URL, str(source)))
child_id = str(uuid.uuid5(uuid.NAMESPACE_URL, "child:" + str(source)))
first, separator, rest = original.partition(b"\n")
try:
    record = json.loads(first)
except ValueError:
    record = {}
if record.get("type") != "session_meta":
    rest = original
    record = {"type": "session_meta", "payload": {}}
record["payload"].update(id=parent_id, cwd=request["cwd"])
updated = json.dumps(record).encode() + b"\n" + rest
if updated != original:
    # Keep the inode: the busy-turn fixture appends through its original path.
    source.write_bytes(updated)
root = source.parent
parent = root / ("rollout-fixture-" + parent_id + ".jsonl")
if not parent.exists():
    os.link(source, parent)
child = root / ("rollout-fixture-" + child_id + ".jsonl")
child.write_text(json.dumps({"type": "session_meta", "payload": {
    "id": child_id, "cwd": request["cwd"],
    "source": {"subagent": {"thread_spawn": {"parent_thread_id": parent_id}}},
}}) + "\n")
request.update(transcript_path=str(child), agent_id=child_id)
os.environ["CODEX_SESSIONS_DIR"] = str(root)
os.environ["CODEX_ARCHIVED_SESSIONS_DIR"] = str(root / "absent-archive")
if os.environ.get("CODEX_FIXTURE_EQUAL_BODY_BUDGET") == "1":
    # Selection parity compares equal body budgets; native pointer lengths differ.
    delta = len(str(parent)) - len(request.pop("fixture_original_path", "xxxxxxxxxxxxxxxxxxxxx.jsonl"))
    delta += len(parent_id) - len(request["session_id"])
    limit = os.environ.get("MEMOIRE_SUBAGENT_MAX_CHARS", "8000")
    limit = int(limit) if re.fullmatch(r"[1-9][0-9]{0,8}", limit) else 8000
    os.environ["MEMOIRE_SUBAGENT_MAX_CHARS"] = str(max(600, limit) + delta)
input_path = root / ("hook-fixture-" + child_id + ".json")
input_path.write_text(json.dumps(request))
fd = os.open(input_path, os.O_RDONLY)
os.dup2(fd, 0)
os.close(fd)
os.execv(str(hook), [str(hook)])
