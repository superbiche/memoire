// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// memoire.js — opencode global plugin: inject recent Memoire observations as
// a synthetic text part on the first message of each session.
//
// Payload lives memoire-side (ratified 2026-07-12, liaison
// docs/memoire-foreign-injection-proposals.md § opencode); liaison's
// `memoire link` installs it to ~/.config/opencode/plugin/memoire.js.
//
// Shells out to memoire-inject-claude.sh, passing {"cwd": directory} on stdin
// (the script's hook-JSON contract — explicit, no reliance on its $PWD
// fallback). Fail-open everywhere: injection must never block a session.
// The message path AWAITS this hook, so the injector runs under a hard
// deadline — `timeout -k 2` (TERM→KILL, same discipline as the qwen envelope
// wrapper) plus a JS race as the second belt, so even a missing timeout
// binary or a stuck pipeline cannot leave the hook pending (ortho review
// 2026-07-13, MAJOR). Stdout is injected only on a clean exit.
//
// Future upgrade: `experimental.chat.system.transform` would place the block
// in the system prompt and survive compaction, but rides an explicitly
// experimental API — stay on chat.message until that stabilizes.

export const MemoirePlugin = async ({ $, directory }) => {
  // MEMOIRE_INJECT_SCRIPT overrides for tarball installs (R-10.5); the
  // default stays the fleet checkout path this plugin is installed from.
  const script = process.env.MEMOIRE_INJECT_SCRIPT
    || `${process.env.HOME}/dev/personal/memoire/scripts/memoire-inject-claude.sh`;
  // Whole-injector deadline (seconds). Positive integers only — zero or junk
  // must fall back, never disable the ceiling.
  const rawDeadline = Number(process.env.MEMOIRE_INJECT_WRAPPER_TIMEOUT);
  const deadline = Number.isInteger(rawDeadline) && rawDeadline > 0 ? rawDeadline : 10;
  const seen = new Set();
  return {
    "chat.message": async (input, output) => {
      try {
        const sid = input.sessionID ?? output.message.sessionID;
        if (seen.has(sid)) return;
        seen.add(sid);
        const hookJson = JSON.stringify({ cwd: directory });
        const run = $`printf '%s' ${hookJson} | timeout -k 2 ${deadline} ${script}`
          .cwd(directory)
          .nothrow()
          .quiet();
        const result = await Promise.race([
          run,
          new Promise((resolve) => setTimeout(() => resolve(null), (deadline + 2) * 1000)),
        ]);
        if (!result || result.exitCode !== 0) return;
        const text = result.text();
        if (!text.trim()) return;
        // Parts are persisted rows since opencode 1.17.x: id, sessionID and
        // messageID are required (text PK) — a bare {type, text} part crashes
        // message processing with an opaque server error.
        // Random id: the seen-Set is per-process, so a resumed session in a
        // fresh process injects again — a deterministic id would collide on
        // the part PK.
        const mid = input.messageID ?? output.message.id;
        output.parts.push({
          id: `prt_memoire_${crypto.randomUUID()}`,
          sessionID: sid,
          messageID: mid,
          type: "text",
          text,
          synthetic: true,
        });
      } catch {
        // fail-open: never block the message on injection errors
      }
    },
  };
};
