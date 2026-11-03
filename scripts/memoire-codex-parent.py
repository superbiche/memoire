#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Resolve the immediate parent of a Codex 0.165 SubagentStart rollout."""

import json
import os
from pathlib import Path
import re
import stat
import sys
import time


class Unavailable(Exception):
    pass


def identity(value):
    if not isinstance(value, str) or not re.fullmatch(
        r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}", value
    ):
        raise Unavailable("identity")
    return value


def directory(value):
    if not isinstance(value, str) or not os.path.isabs(value) or any(
        ord(c) < 32 or 127 <= ord(c) <= 159 for c in value
    ):
        raise Unavailable("cwd")
    return value


def confined(path, roots):
    if any(ord(c) < 32 or 127 <= ord(c) <= 159 for c in str(path)):
        raise Unavailable("path")
    path = Path(os.path.abspath(path))
    for configured, root in roots:
        try:
            parts = path.relative_to(configured).parts
        except ValueError:
            try:
                parts = path.relative_to(root).parts
            except ValueError:
                continue
        current = root
        for part in parts:
            current /= part
            if current.is_symlink():
                raise Unavailable("symlink")
        if current.resolve(strict=True).is_relative_to(root):
            return current
    raise Unavailable("path")


def metadata(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise Unavailable("file-type")
        line = stream.readline(65537)
    if len(line) > 65536 or not line.endswith(b"\n"):
        raise Unavailable("metadata-size")
    record = json.loads(line)
    if not isinstance(record, dict) or record.get("type") != "session_meta":
        raise Unavailable("metadata")
    value = record.get("payload")
    if not isinstance(value, dict):
        raise Unavailable("metadata")
    identity(value.get("id"))
    directory(value.get("cwd"))
    return value


def resolve(child_path, agent_id):
    home = Path(os.environ.get("CODEX_HOME", str(Path.home() / ".codex")))
    # Accept the configured spelling and canonical spelling of each root;
    # symlinks below that root remain forbidden.
    roots = list(dict.fromkeys((Path(os.path.abspath(p)), Path(p).resolve()) for p in (
        os.environ.get("CODEX_SESSIONS_DIR", str(home / "sessions")),
        os.environ.get("CODEX_ARCHIVED_SESSIONS_DIR", str(home / "archived_sessions")),
    )))
    child_path = confined(child_path, roots)
    child = metadata(child_path)
    if child["id"] != identity(agent_id):
        raise Unavailable("child-identity")
    try:
        parent_id = identity(child["source"]["subagent"]["thread_spawn"]["parent_thread_id"])
    except (KeyError, TypeError):
        raise Unavailable("parent-link") from None
    if parent_id == agent_id:
        raise Unavailable("parent-cycle")

    # Enumerate names only; unrelated rollout bodies are never opened.
    deadline = time.monotonic() + 0.5
    pending = [(root, 0) for root in dict.fromkeys(r for _, r in roots) if root.is_dir()]
    matches = []
    visited = 0
    suffix = "-" + parent_id + ".jsonl"
    while pending:
        folder, depth = pending.pop()
        with os.scandir(folder) as entries:
            for entry in entries:
                visited += 1
                if visited > 100000 or time.monotonic() > deadline:
                    raise Unavailable("lookup-bound")
                if entry.is_symlink():
                    continue
                if entry.is_dir(follow_symlinks=False) and depth < 3:
                    pending.append((Path(entry.path), depth + 1))
                elif (entry.is_file(follow_symlinks=False)
                      and entry.name.startswith("rollout-") and entry.name.endswith(suffix)):
                    matches.append(Path(entry.path))
                    if len(matches) > 1:
                        raise Unavailable("ambiguous-parent")
    if not matches:
        raise Unavailable("parent-missing")
    parent_path = confined(matches[0], roots)
    parent = metadata(parent_path)
    if parent["id"] != parent_id:
        raise Unavailable("parent-identity")
    return {
        "path": str(parent_path), "id": parent_id, "cwd": parent["cwd"],
        "child_cwd": child["cwd"],
    }


if __name__ == "__main__":
    try:
        result = resolve(sys.argv[1], sys.argv[2])
    except Unavailable as error:
        result = {"error": str(error)}
    except (OSError, ValueError, TypeError, IndexError):
        result = {"error": "metadata-unavailable"}
    print(json.dumps(result, ensure_ascii=True))
