#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
"""Native-shaped SubagentStart fixtures, without live Codex state."""

import json
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch


SCRIPTS = Path(__file__).resolve().parents[1]
PARENT = "11111111-1111-4111-8111-111111111111"
CHILD = "22222222-2222-4222-8222-222222222222"
ROOT = "33333333-3333-4333-8333-333333333333"
SPEC = importlib.util.spec_from_file_location("codex_parent", SCRIPTS / "memoire-codex-parent.py")
RESOLVER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RESOLVER)


class NativeParentTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.codex = self.home / "codex"
        self.sessions = self.codex / "sessions" / "2026" / "10" / "29"
        self.sessions.mkdir(parents=True)
        self.archive = self.codex / "archived_sessions"
        self.archive.mkdir()
        self.mh = self.home / "memoire"
        self.mh.mkdir()
        (self.mh / "capture-tick.env").write_text("MEMOIRE_SUBAGENT_CONTEXT=on\n")
        self.cwd = self.home / "project"
        self.cwd.mkdir()
        resolver = self.home / "resolver"
        resolver.write_text('#!/bin/bash\nif [[ "$1" == --capture-enabled ]]; then\n'
                            ' [[ "$2" == */excluded ]] && echo off || echo on\n'
                            'else echo fixture-project; fi\n')
        resolver.chmod(0o700)
        self.env = {"PATH": os.environ["PATH"], "HOME": str(self.home),
                    "CODEX_HOME": str(self.codex), "MEMOIRE_HOME": str(self.mh),
                    "MEMOIRE_RESOLVE_PROJECT": str(resolver)}
        self.parent = self.rollout(PARENT, source="cli", activity=True)
        self.child = self.rollout(CHILD, source={"subagent": {"thread_spawn": {
            "parent_thread_id": PARENT}}})

    def rollout(self, thread, source, activity=False, folder=None, cwd=None, hist=None):
        payload = {"id": thread, "session_id": ROOT, "cwd": str(cwd or self.cwd),
                   "source": source, "cli_version": "0.165.0"}
        if hist is not None:
            payload["subagent_history_start_ordinal"] = hist
        lines = [{"type": "session_meta", "payload": payload, "ordinal": 0}]
        if activity:
            lines += [
                {"type": "response_item", "ordinal": 1, "timestamp": "2026-10-29T10:00:01Z",
                 "payload": {"type": "message", "role": "assistant",
                             "content": [{"type": "output_text", "text": "OLD-HISTORY"}]}},
                {"type": "event_msg", "ordinal": 2,
                 "payload": {"type": "task_started", "turn_id": "parent-turn"}},
                {"type": "response_item", "ordinal": 3, "timestamp": "2026-10-29T10:00:03Z",
                 "payload": {"type": "message", "role": "assistant",
                             "content": [{"type": "output_text", "text": "PARENT-CANARY"}]}},
                {"type": "response_item", "ordinal": 4, "timestamp": "2026-10-29T10:00:04Z",
                 "payload": {"type": "function_call", "name": "spawn_agent",
                             "call_id": "spawn", "arguments": "{}"}},
            ]
        path = (folder or self.sessions) / ("rollout-2026-10-29T10-00-00-" + thread + ".jsonl")
        path.write_text("".join(json.dumps(line) + "\n" for line in lines))
        return path

    def run_hook(self, **overrides):
        request = {"session_id": ROOT, "agent_id": CHILD, "agent_type": "worker",
                   "cwd": str(self.cwd), "transcript_path": str(self.child),
                   "turn_id": "child-turn", "hook_event_name": "SubagentStart"}
        request.update(overrides)
        result = subprocess.run([str(SCRIPTS / "memoire-subagent-context-codex.sh")],
                                input=json.dumps(request), text=True, capture_output=True,
                                env=self.env, cwd=self.cwd, timeout=4)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stderr, "")
        return result.stdout

    def silent(self, reason, **overrides):
        self.assertEqual(self.run_hook(**overrides), "")
        log = (self.mh / "hooks" / "subagent.log").read_text().splitlines()[-1]
        self.assertIn(reason, log)
        self.assertNotIn("PARENT-CANARY", log)

    def test_native_child_path_reads_immediate_parent_not_shared_root(self):
        self.rollout(ROOT, source="cli", activity=True).write_text("unrelated root must not be read\n")
        output = self.run_hook()
        self.assertIn("PARENT-CANARY", output)
        self.assertIn("Parent session " + PARENT, output)
        self.assertNotIn("Parent session " + ROOT, output)

    def test_nested_forked_parent_excludes_inherited_history(self):
        self.rollout(PARENT, source={"subagent": {"thread_spawn": {"parent_thread_id": ROOT}}},
                     activity=True, hist=2)
        output = self.run_hook()
        self.assertIn("PARENT-CANARY", output)
        self.assertNotIn("OLD-HISTORY", output)
        log = (self.mh / "hooks" / "subagent.log").read_text()
        self.assertIn("session=" + PARENT, log)
        self.assertNotIn("session=" + ROOT, log)

    def test_symlinked_codex_home_accepts_both_path_spellings(self):
        linked = self.home / "linked-codex"
        linked.symlink_to(self.codex, target_is_directory=True)
        self.env["CODEX_HOME"] = str(linked)
        for child in (self.child, linked / self.child.relative_to(self.codex)):
            with self.subTest(child=child):
                self.assertIn("PARENT-CANARY", self.run_hook(transcript_path=str(child)))

    def test_explicit_symlinked_store_and_duplicate_root(self):
        linked = self.home / "linked-store"
        linked.symlink_to(self.codex / "sessions", target_is_directory=True)
        self.env["CODEX_SESSIONS_DIR"] = str(linked)
        self.env["CODEX_ARCHIVED_SESSIONS_DIR"] = str(self.codex / "sessions")
        child = linked / self.child.relative_to(self.codex / "sessions")
        self.assertIn("PARENT-CANARY", self.run_hook(transcript_path=str(child)))

    def test_directory_symlink_below_root_rejected(self):
        linked = self.codex / "sessions" / "alias"
        linked.symlink_to(self.sessions, target_is_directory=True)
        self.silent("error=symlink", transcript_path=str(linked / self.child.name))

    def test_missing_parent_link(self):
        self.rollout(CHILD, source="cli")
        self.silent("error=parent-link")

    def test_parent_cycle(self):
        self.rollout(CHILD, source={"subagent": {"thread_spawn": {"parent_thread_id": CHILD}}})
        self.silent("error=parent-cycle")

    def test_invalid_metadata_cwd(self):
        for cwd in ("relative", "/tmp/control\npath", 123):
            with self.subTest(cwd=cwd):
                lines = self.child.read_text().splitlines()
                meta = json.loads(lines[0])
                meta["payload"]["cwd"] = cwd
                self.child.write_text(json.dumps(meta) + "\n")
                self.silent("error=cwd")

    def test_fifo_child_rejected_without_blocking(self):
        self.child.unlink()
        os.mkfifo(self.child)
        self.silent("error=file-type")

    def test_lookup_time_bound(self):
        with patch.dict(os.environ, self.env, clear=True), \
             patch.object(RESOLVER.time, "monotonic", side_effect=[0.0, 1.0]):
            with self.assertRaisesRegex(RESOLVER.Unavailable, "^lookup-bound$"):
                RESOLVER.resolve(self.child, CHILD)

    def test_lookup_entry_bound(self):
        # Real metadata/confinement, synthetic directory iterator only: no
        # production override and no 100,001-file fixture on disk.
        class Entry:
            def is_symlink(self):
                return True
        class Entries:
            def __enter__(self):
                return iter(Entry() for _ in range(100001))
            def __exit__(self, *_):
                return False
        with patch.dict(os.environ, self.env, clear=True), \
             patch.object(RESOLVER.time, "monotonic", return_value=0.0), \
             patch.object(RESOLVER.os, "scandir", return_value=Entries()):
            with self.assertRaisesRegex(RESOLVER.Unavailable, "^lookup-bound$"):
                RESOLVER.resolve(self.child, CHILD)

    def test_resolver_process_failure_is_silent(self):
        bindir = self.home / "bin"
        bindir.mkdir()
        python = bindir / "python3"
        python.write_text("#!/bin/bash\nexit 127\n")
        python.chmod(0o700)
        self.env["PATH"] = str(bindir) + ":" + self.env["PATH"]
        self.silent("error=parent-resolver")

    def test_native_block_respects_configured_size(self):
        self.parent.write_text(self.parent.read_text().replace("PARENT-CANARY", "X" * 10000))
        for size in (600, 1000, 8000):
            with self.subTest(size=size):
                self.env["MEMOIRE_SUBAGENT_MAX_CHARS"] = str(size)
                block = json.loads(self.run_hook())["hookSpecificOutput"]["additionalContext"]
                self.assertLessEqual(len(block), size)
                self.assertIn("Parent session " + PARENT, block)

    def test_new_parent_empty_is_legitimate(self):
        self.rollout(PARENT, source="cli")
        self.silent("status=empty")

    def test_archived_parent(self):
        self.parent.rename(self.archive / self.parent.name)
        self.assertIn("PARENT-CANARY", self.run_hook())

    def test_missing_parent(self):
        self.parent.unlink()
        self.silent("error=parent-missing")

    def test_ambiguous_parent(self):
        (self.archive / self.parent.name).write_bytes(self.parent.read_bytes())
        self.silent("error=ambiguous-parent")

    def test_parent_metadata_identity_mismatch(self):
        self.parent.write_text(self.parent.read_text().replace(PARENT, ROOT))
        self.silent("error=parent-identity")

    def test_child_identity_mismatch(self):
        self.silent("error=child-identity", agent_id=ROOT)

    def test_malformed_identity(self):
        self.silent("error=identity", agent_id="not-a-uuid")

    def test_first_line_must_be_session_metadata(self):
        for record in ({"type": "turn_context"},
                       {"type": "session_meta", "payload": []}):
            with self.subTest(record=record):
                self.child.write_text(json.dumps(record) + "\n")
                self.silent("error=metadata")

    def test_child_outside_store(self):
        outside = self.home / self.child.name
        self.child.rename(outside)
        self.silent("error=path", transcript_path=str(outside))

    def test_child_symlink(self):
        original = self.child.with_suffix(".original")
        self.child.rename(original)
        self.child.symlink_to(original)
        self.silent("error=symlink")

    def test_parent_symlink_is_not_followed(self):
        original = self.home / self.parent.name
        self.parent.rename(original)
        self.parent.symlink_to(original)
        self.silent("error=parent-missing")

    def test_parent_privacy_gate(self):
        self.rollout(PARENT, source="cli", activity=True, cwd=self.home / "excluded")
        self.silent("status=capture-off")

    def test_child_metadata_privacy_gate(self):
        self.rollout(CHILD, source={"subagent": {"thread_spawn": {"parent_thread_id": PARENT}}},
                     cwd=self.home / "excluded")
        self.silent("status=capture-off")

    def test_child_hook_privacy_gate(self):
        self.silent("status=capture-off", cwd=str(self.home / "excluded"))

    def test_malformed_metadata(self):
        self.child.write_text("{\n")
        self.silent("error=metadata-unavailable")

    def test_metadata_size_bound(self):
        self.child.write_text(" " * 65537 + "\n")
        self.silent("error=metadata-size")

    def test_directory_not_rollout(self):
        self.parent.unlink()
        self.parent.mkdir()
        self.silent("error=parent-missing")

    def test_large_store_names_only_and_timeout_margin(self):
        for n in range(5000):
            (self.sessions / f"rollout-unrelated-{n}.jsonl").touch()
        start = time.monotonic()
        self.assertIn("PARENT-CANARY", self.run_hook())
        self.assertLess(time.monotonic() - start, 2.5)


if __name__ == "__main__":
    unittest.main()
