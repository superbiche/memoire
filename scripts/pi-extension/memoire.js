// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// memoire.js — Pi extension: Memoire injection and live capture.
//
// Payload lives memoire-side, like the opencode plugin; liaison's
// `memoire link` installs it to ~/.pi/agent/extensions/memoire.js (Pi loads
// plain JS extensions through jiti, no build step).
//
// Injection. At the first prompt of each session the extension runs
// memoire-inject-claude.sh with {"cwd": <session cwd>} on stdin (the script's
// hook-JSON contract) under `timeout -k 2 <deadline>` plus a JS race, and keeps
// its stdout when it exits 0. Every run of the session then carries that text
// as the `memoire` section of the system prompt (before_agent_start,
// systemPromptOptions.sections): stable for the session, so the prompt cache
// holds, and it survives compaction. Pi records the rendered system prompt in
// the session (system messages, and the `systemMessage` of a compaction
// checkpoint), so capture strips the section from both (below). Session-start
// injection is off by
// default (MEMOIRE_INJECT, read by the injector): the injector then prints
// nothing and no section is set. Fail-open: a failing, empty or hanging
// injector sets nothing and never holds the prompt past the deadline.
//
// Capture (liaison 2026-10-07, operator choice (a): live hook, no transcript
// reader). On agent_settled — Pi will not continue on its own — the session
// entries not yet queued are written through `memoire-queue -batch-file`:
//   * pi_entry for every new session entry (messages with reasoning and tool
//     calls, tool results, compactions, model and thinking changes, custom
//     entries, the system prompt messages): capture-all, the entry JSON in
//     the payload, image data replaced by a marker. A system message and a
//     compaction checkpoint's `systemMessage` lose their `memoire` section,
//     and the injected text where their content carries it: the store never
//     re-ingests the context it injected;
//   * pi_assistant_turn for every new assistant message that ended normally
//     (stopReason stop, length or toolUse) with non-empty text: the text parts
//     joined by "\n\n" in the event TEXT, no cap. This is the derivation input.
// Ids: pi:entry:<session id>:<entry id> and pi:turn:<session id>:<entry id>.
// Delivery: a per-session cursor, $MEMOIRE_HOME/capture-cursors/pi/<session id>.json,
// keeps two sets apart: `settled` (entries whose batch was written to the queue: only a
// successful write adds to it) and `excluded` (entries skipped by an explicit capture
// rule: MEMOIRE_CAPTURE off|0|false, or a project the resolver reports capture-disabled;
// an excluded entry is not queued later, even with capture back on, as long as the cursor
// survives). An entry stays out of both until its batch is written, so a failed write is
// retried at the next settle and after a restart. A session without a cursor is NOT
// assumed captured: all its entries are queued again. The queue's keys are idempotent
// (pi:entry:<session id>:<entry id>; the hub keeps one row per key), so a delivered entry
// is absorbed and an undelivered one is finally captured; a lost cursor costs a replay,
// never a loss. The capture switches are evaluated again at replay time. When two cursors
// exist (the current one and the older pi-capture/ one) and disagree, only what both
// settled counts (the smaller set replays more, and a replay is harmless) while an entry
// either cursor excluded stays excluded.
// session_shutdown waits (bounded) for writes in flight. Redaction
// stays at the queue-write gate (MEMOIRE_REDACT). Capture is skipped when
// MEMOIRE_CAPTURE is off|0|false or the resolver reports the cwd's project as
// capture-disabled (`--capture-enabled` prints "off").
//
// Overrides: MEMOIRE_INJECT_SCRIPT, MEMOIRE_RESOLVE_PROJECT, MEMOIRE_QUEUE_BIN,
// MEMOIRE_INJECT_WRAPPER_TIMEOUT (seconds, positive integer, default 10),
// MEMOIRE_HOME (log under hooks/pi-capture.log).

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFile } from "node:child_process";

const memoireHome = () => process.env.MEMOIRE_HOME || `${process.env.HOME}/.memoire`;
const injectScript = () => process.env.MEMOIRE_INJECT_SCRIPT
  || `${process.env.HOME}/dev/personal/memoire/scripts/memoire-inject-claude.sh`;
const resolverScript = () => process.env.MEMOIRE_RESOLVE_PROJECT
  || path.join(path.dirname(injectScript()), "memoire-resolve-project.sh");
const queueBin = () => process.env.MEMOIRE_QUEUE_BIN
  || path.join(path.dirname(injectScript()), "..", "bin", "memoire-queue");
const deadlineSeconds = () => {
  const v = String(process.env.MEMOIRE_INJECT_WRAPPER_TIMEOUT ?? "");
  return /^[1-9][0-9]{0,8}$/.test(v) ? Number(v) : 10;
};
const captureOff = () => /^(off|0|false)$/.test(String(process.env.MEMOIRE_CAPTURE ?? ""));
const SHUTDOWN_WAIT_MS = 10000;
const QUEUE_TIMEOUT_MS = 30000;
const RESOLVER_TIMEOUT_MS = 3000;
const TURN_STOP_REASONS = new Set(["stop", "length", "toolUse"]);

function logLine(fields) {
  try {
    const file = path.join(memoireHome(), "hooks", "pi-capture.log");
    fs.mkdirSync(path.dirname(file), { recursive: true });
    const clean = (v) => Array.from(String(v).replace(/[\u0000-\u001f\u007f-\u009f\u2028\u2029]/g, " ")).slice(0, 200).join("");
    const kvs = Object.entries(fields).map(([k, v]) => `${k}=${clean(v)}`).join(" ");
    fs.appendFileSync(file, `${new Date().toISOString()} memoire-pi: ${kvs}\n`);
  } catch { /* logging never blocks */ }
}

// Run a file; resolve {code, stdout} and never reject. `input` goes to stdin.
function run(file, args, { cwd, timeoutMs, input } = {}) {
  return new Promise((resolve) => {
    let done = false;
    const finish = (r) => { if (!done) { done = true; resolve(r); } };
    const timer = setTimeout(() => finish({ code: -1, stdout: "" }), timeoutMs + 500);
    timer.unref?.();
    try {
      const child = execFile(file, args, { cwd, timeout: timeoutMs, env: process.env, maxBuffer: 16 << 20 },
        (err, stdout) => {
          clearTimeout(timer);
          finish({ code: err ? (typeof err.code === "number" ? err.code : -1) : 0, stdout: String(stdout ?? "") });
        });
      child.on("error", () => finish({ code: -1, stdout: "" }));
      child.stdin?.on("error", () => { /* a child that exits early closes stdin */ });
      child.stdin?.end(input ?? "");
    } catch {
      clearTimeout(timer);
      finish({ code: -1, stdout: "" });
    }
  });
}

async function injectorText(cwd) {
  const deadline = deadlineSeconds();
  const r = await run("timeout", ["-k", "2", String(deadline), injectScript()],
    { cwd, timeoutMs: (deadline + 2) * 1000, input: JSON.stringify({ cwd }) });
  return r.code === 0 && r.stdout.trim() ? r.stdout : "";
}

// The resolver prints "off" for a capture-disabled project; its exit code does
// not count. --verbose prints key<TAB>root<TAB>source.
async function resolveProject(cwd) {
  const enabled = await run(resolverScript(), ["--capture-enabled", cwd], { timeoutMs: RESOLVER_TIMEOUT_MS });
  if (enabled.stdout.trim() === "off") return null;
  const r = await run(resolverScript(), ["--verbose", cwd], { timeoutMs: RESOLVER_TIMEOUT_MS });
  const [key = "", root = "", source = ""] = r.code === 0 ? r.stdout.trim().split("\t") : [];
  return { key: key || path.basename(cwd) || cwd, root, source };
}

// Image data is replaced by a marker; everything else of the entry is kept.
function stripImages(value) {
  if (Array.isArray(value)) return value.map(stripImages);
  if (value && typeof value === "object") {
    if (value.type === "image" && typeof value.data === "string") {
      const { data, ...rest } = value;
      return { ...rest, data_omitted: true, data_chars: data.length };
    }
    return Object.fromEntries(Object.entries(value).map(([k, v]) => [k, stripImages(v)]));
  }
  return value;
}

// The system prompt messages Pi records carry the injected section: drop it.
function scrubSystemMessage(m, injectedText) {
  if (!m || typeof m !== "object" || m.role !== "system") return m;
  const out = { ...m };
  if (out.sections && typeof out.sections === "object" && "memoire" in out.sections) {
    const { memoire: _own, ...rest } = out.sections;
    out.sections = { ...rest, memoire_omitted: true };
  }
  if (typeof out.content === "string") {
    // Pi renders a named section as <name>…</name>; an injection made by an
    // earlier process is only recognisable by its tag.
    out.content = out.content.replace(/<memoire>[\s\S]*?<\/memoire>/g, "[memoire section omitted]");
    if (injectedText && out.content.includes(injectedText)) {
      out.content = out.content.split(injectedText).join("[memoire section omitted]");
    }
  }
  return out;
}

function withoutOwnSection(entry, injectedText) {
  let out = entry;
  if (entry?.type === "message" && entry.message?.role === "system") {
    out = { ...out, message: scrubSystemMessage(entry.message, injectedText) };
  }
  // Compaction checkpoints keep the system prompt current at compaction time.
  if (entry?.systemMessage) {
    out = { ...out, systemMessage: scrubSystemMessage(entry.systemMessage, injectedText) };
  }
  return out;
}

// The per-session delivery cursor (see the header). A malformed file reads as
// absent; a write failure is logged and capture goes on in memory.
// The cursor lives under capture-cursors/pi, the directory every Pi sandbox profile
// mounts writable (the older pi-capture/ is not mounted: inside the sandbox its cursor
// died with the session, 2026-11-14). A cursor left at the old place is still read.
const safeId = (sessionId) => (/^[A-Za-z0-9_-]{1,128}$/.test(sessionId) ? sessionId : Buffer.from(sessionId).toString("hex").slice(0, 200));
const cursorPath = (sessionId) => path.join(memoireHome(), "capture-cursors", "pi", `${safeId(sessionId)}.json`);
const legacyCursorPath = (sessionId) => path.join(memoireHome(), "pi-capture", `${safeId(sessionId)}.json`);
const idSet = (v) => new Set(Array.isArray(v) ? v.filter((x) => typeof x === "string") : []);
const union = (a, b) => new Set([...a, ...b]);
const intersect = (a, b) => new Set([...a].filter((x) => b.has(x)));
function readCursorFile(file) {
  try {
    const d = JSON.parse(fs.readFileSync(file, "utf8"));
    if (!Array.isArray(d?.settled)) return null;
    return { settled: idSet(d.settled), excluded: idSet(d.excluded) };
  } catch {
    return null; // absent or malformed
  }
}
// Both cursors present and disagreeing: the settled sets resolve by intersection (safe
// because a replay is absorbed by the hub's idempotent keys, while trusting the larger set
// could skip an entry that was never delivered); the excluded sets resolve by union (an
// entry excluded once stays excluded, whichever cursor recorded it).
function readCursor(sessionId) {
  const current = readCursorFile(cursorPath(sessionId));
  const legacy = readCursorFile(legacyCursorPath(sessionId));
  if (current && legacy) return { settled: intersect(current.settled, legacy.settled), excluded: union(current.excluded, legacy.excluded) };
  return current ?? legacy;
}
function writeCursor(sessionId, settled, excluded) {
  try {
    const file = cursorPath(sessionId);
    fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
    const tmp = `${file}.${process.pid}.tmp`;
    fs.writeFileSync(tmp, JSON.stringify({ schema: "pi-capture-cursor/1", settled: [...settled], excluded: [...excluded] }) + "\n", { mode: 0o600 });
    fs.renameSync(tmp, file);
    return true;
  } catch (e) {
    logLine({ status: "error=cursor", session: sessionId, detail: e?.message ?? e });
    return false;
  }
}

function turnText(message) {
  if (message?.role !== "assistant" || !TURN_STOP_REASONS.has(message.stopReason)) return "";
  const parts = Array.isArray(message.content) ? message.content : [];
  return parts.filter((p) => p?.type === "text" && typeof p.text === "string" && p.text.trim() !== "")
    .map((p) => p.text).join("\n\n");
}

const occurredAt = (entry) => {
  const t = Date.parse(entry?.timestamp ?? "");
  return new Date(Number.isFinite(t) ? t : Date.now()).toISOString();
};

export function buildEvents(entries, { sessionId, sessionFile, cwd, project, injectedText = "" }) {
  const base = { project: project.key, session_id: sessionId, source_adapter: "pi", cwd };
  if (project.root) Object.assign(base, { project_root: project.root, project_source: project.source });
  const events = [];
  for (const entry of entries) {
    events.push({
      ...base, event_type: "pi_entry", source_event_id: `pi:entry:${sessionId}:${entry.id}`,
      occurred_at: occurredAt(entry),
      payload: { schema: "pi-entry/1", session_id: sessionId, session_file: sessionFile ?? "", entry: stripImages(withoutOwnSection(entry, injectedText.trim())) },
    });
    const text = entry.type === "message" ? turnText(entry.message) : "";
    if (text) {
      const m = entry.message;
      events.push({
        ...base, event_type: "pi_assistant_turn", source_event_id: `pi:turn:${sessionId}:${entry.id}`,
        occurred_at: occurredAt(entry), text,
        payload: { schema: "pi-turn/1", session_id: sessionId, entry_id: entry.id,
          provider: m.provider ?? "", model: m.model ?? "", stop_reason: m.stopReason },
      });
    }
  }
  return events;
}

async function writeBatch(events) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "memoire-pi."));
  const file = path.join(dir, "batch.jsonl");
  try {
    fs.writeFileSync(file, events.map((e) => JSON.stringify(e)).join("\n") + "\n", { mode: 0o600 });
    return (await run(queueBin(), ["-batch-file", file], { timeoutMs: QUEUE_TIMEOUT_MS })).code === 0;
  } finally {
    try { fs.rmSync(dir, { recursive: true, force: true }); } catch { /* */ }
  }
}

export default function memoire(pi) {
  let sessionId = "";
  let injected = null; // null: not computed for this session yet; "" : nothing to inject
  let injecting = null;
  const settled = new Set();   // delivered: the queue write succeeded
  const excluded = new Set();  // skipped by an explicit capture rule
  const done = (id) => settled.has(id) || excluded.has(id);
  const inflight = new Set();
  const pending = new Set();

  const resetSession = (ctx) => {
    try {
      sessionId = ctx.sessionManager.getSessionId();
      injected = null;
      injecting = null;
      settled.clear();
      excluded.clear();
      const cursor = readCursor(sessionId);
      if (cursor) {
        for (const id of cursor.settled) settled.add(id);
        for (const id of cursor.excluded) excluded.add(id);
      }
      // no cursor: nothing is assumed captured; the first settle replays the whole history
    } catch (e) {
      logLine({ status: "error=session", detail: e?.message ?? e });
    }
  };

  pi.on("session_start", async (_event, ctx) => { resetSession(ctx); });

  pi.on("before_agent_start", async (event, ctx) => {
    try {
      if (injected === null) {
        injecting ??= injectorText(ctx.cwd);
        injected = await injecting;
      }
      if (injected) event.systemPromptOptions.sections.memoire = injected;
    } catch {
      injected = "";
    }
  });

  // `delivered`: the batch was written. Otherwise the entries were excluded by an explicit
  // capture rule. Nothing is recorded for a failed write.
  const settle = (entries, sid, delivered) => {
    if (sid !== sessionId) return; // the session changed while the batch ran
    for (const e of entries) (delivered ? settled : excluded).add(e.id);
    writeCursor(sid, settled, excluded);
  };

  const capture = async (ctx) => {
    let entries;
    let meta;
    try {
      if (ctx.sessionManager.getSessionId() !== sessionId) resetSession(ctx);
      entries = ctx.sessionManager.getEntries().filter((e) => !done(e.id) && !inflight.has(e.id));
      if (!entries.length) return;
      if (captureOff()) {
        settle(entries, sessionId, false);
        return;
      }
      meta = { sessionId, sessionFile: ctx.sessionManager.getSessionFile(), cwd: ctx.cwd, injectedText: injected || "" };
    } catch (e) {
      logLine({ status: "error=entries", detail: e?.message ?? e });
      return;
    }
    for (const e of entries) inflight.add(e.id);
    try {
      const project = await resolveProject(meta.cwd);
      if (!project) {
        settle(entries, meta.sessionId, false); // capture-disabled project: excluded, not retried while the cursor lives
        return;
      }
      const ok = await writeBatch(buildEvents(entries, { ...meta, project }));
      if (ok) settle(entries, meta.sessionId, true);
      else logLine({ status: "error=queue", session: meta.sessionId, entries: entries.length });
    } catch (e) {
      logLine({ status: "error=capture", session: meta.sessionId, detail: e?.message ?? e });
    } finally {
      for (const e of entries) inflight.delete(e.id);
    }
  };

  pi.on("agent_settled", async (_event, ctx) => {
    const p = capture(ctx);
    pending.add(p);
    p.finally(() => pending.delete(p));
  });

  pi.on("session_shutdown", async () => {
    if (!pending.size) return;
    await Promise.race([
      Promise.allSettled([...pending]),
      new Promise((r) => setTimeout(r, SHUTDOWN_WAIT_MS).unref?.()),
    ]);
  });
}
