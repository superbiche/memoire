// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// pi-extension-test.mjs — the Pi extension (scripts/pi-extension/memoire.js).
// Plain node, a fake Pi API and session manager, a fake injector, resolver and
// queue writer under a scratch MEMOIRE_HOME: no Pi, no network, no live state.
// Covers: injection computed once per session and set as the `memoire` system
// prompt section of every run; a failing, empty or hanging injector sets
// nothing and releases the prompt within the deadline; capture on
// agent_settled of new entries only (pi_entry + pi_assistant_turn), turn text
// rules, image stripping, own-section removal from system messages and
// compaction checkpoints; history present before the extension is not queued;
// a failed queue write is retried at the next settle and after a restart (the
// per-session cursor); capture-off by env and by resolver; session_shutdown
// waits for writes in flight.

import path from "node:path";
import fs from "node:fs";
import os from "node:os";
import { pathToFileURL } from "node:url";

const here = path.dirname(new URL(import.meta.url).pathname);
const extPath = process.env.MEMOIRE_PI_EXTENSION_PATH
  ? path.resolve(process.env.MEMOIRE_PI_EXTENSION_PATH)
  : path.resolve(here, "../pi-extension/memoire.js");
let failures = 0;
const check = (cond, label) => { if (!cond) { failures += 1; console.error(`FAIL: ${label}`); } };

const root = fs.mkdtempSync(path.join(process.env.TMPDIR || os.tmpdir(), "pi-extension."));
process.on("exit", () => { try { fs.rmSync(root, { recursive: true, force: true }); } catch { /* */ } });
const home = path.join(root, "mh"); fs.mkdirSync(home, { recursive: true });
process.env.MEMOIRE_HOME = home;
process.env.HOME = path.join(root, "home");
process.env.MEMOIRE_INJECT_WRAPPER_TIMEOUT = "2";
delete process.env.MEMOIRE_CAPTURE;

const script = (name, body) => {
  const p = path.join(root, name);
  fs.writeFileSync(p, `#!/usr/bin/env bash\n${body}\n`, { mode: 0o755 });
  return p;
};
const modeFile = (name) => path.join(root, `${name}.mode`);
const setMode = (name, mode) => fs.writeFileSync(modeFile(name), mode);
const injectorCalls = path.join(root, "injector.log");
process.env.MEMOIRE_INJECT_SCRIPT = script("inject.sh", `
cat >> "${injectorCalls}"; printf '\\n' >> "${injectorCalls}"
case "$(cat "${modeFile("inject")}" 2>/dev/null)" in
  fail) echo garbage; exit 1 ;;
  empty) exit 0 ;;
  hang) sleep 30 ;;
  *) printf '# [memoire] recent context\\n- obs é日本\\n' ;;
esac`);
process.env.MEMOIRE_RESOLVE_PROJECT = script("resolve.sh", `
if [[ "$1" == "--capture-enabled" ]]; then
  [[ "$(cat "${modeFile("resolve")}" 2>/dev/null)" == off ]] && { echo off; exit 1; }
  echo on; exit 0
fi
printf 'proj-key\\t%s\\tregistry\\n' "$2"`);
const batches = path.join(root, "batches"); fs.mkdirSync(batches);
process.env.MEMOIRE_QUEUE_BIN = script("queue.sh", `
[[ "$1" == "-batch-file" ]] || exit 9
[[ "$(cat "${modeFile("queue")}" 2>/dev/null)" == fail ]] && exit 1
cp "$2" "${batches}/$(date +%s%N).jsonl"`);

const injectorCount = () => (fs.existsSync(injectorCalls) ? fs.readFileSync(injectorCalls, "utf8").split("\n").filter(Boolean).length : 0);
const readBatches = () => fs.readdirSync(batches).sort().map((f) => fs.readFileSync(path.join(batches, f), "utf8")
  .split("\n").filter(Boolean).map((l) => JSON.parse(l)));
const clearBatches = () => { for (const f of fs.readdirSync(batches)) fs.rmSync(path.join(batches, f)); };

const cwd = path.join(root, "proj"); fs.mkdirSync(cwd);
function makePi() {
  const handlers = {};
  return { handlers, on: (name, fn) => { (handlers[name] ??= []).push(fn); return () => {}; } };
}
function makeCtx(sessionId, entries) {
  return {
    cwd,
    sessionManager: {
      getSessionId: () => sessionId,
      getSessionFile: () => `/sessions/${sessionId}.jsonl`,
      getEntries: () => entries,
    },
  };
}
const fire = async (pi, name, event, ctx) => {
  let last;
  for (const fn of pi.handlers[name] ?? []) last = await fn(event, ctx);
  return last;
};
const startEvent = () => ({ type: "before_agent_start", prompt: "p", systemPrompt: "", systemPromptOptions: { sections: {} } });
const settle = async (pi, ctx) => { await fire(pi, "agent_settled", { type: "agent_settled" }, ctx); await fire(pi, "session_shutdown", {}, ctx); };

const ts = "2026-10-07T10:00:00.000Z";
const msg = (id, message) => ({ type: "message", id, parentId: null, timestamp: ts, message });
const user = (id, text) => msg(id, { role: "user", content: text, timestamp: 1 });
const assistant = (id, content, stopReason = "stop") => msg(id, { role: "assistant", content, stopReason, provider: "conductor", model: "coder", timestamp: 1 });

const { default: memoire, buildEvents } = await import(pathToFileURL(extPath).href);
check(typeof memoire === "function" && typeof buildEvents === "function", "exports: default factory and buildEvents");

// --- injection -------------------------------------------------------------
{
  const pi = makePi(); memoire(pi);
  const entries = [user("old1", "before load")];
  const ctx = makeCtx("s-inject", entries);
  await fire(pi, "session_start", { type: "session_start", reason: "startup" }, ctx);
  setMode("inject", "ok");
  const e1 = startEvent(); await fire(pi, "before_agent_start", e1, ctx);
  check(e1.systemPromptOptions.sections.memoire?.includes("obs é日本"), "injection: first run carries the memoire section");
  const e2 = startEvent(); await fire(pi, "before_agent_start", e2, ctx);
  check(e2.systemPromptOptions.sections.memoire === e1.systemPromptOptions.sections.memoire, "injection: later runs carry the same section");
  check(injectorCount() === 1, `injection: injector ran once per session (ran ${injectorCount()})`);
  check(JSON.parse(fs.readFileSync(injectorCalls, "utf8").split("\n")[0]).cwd === cwd, "injection: hook JSON carries the session cwd");
  await fire(pi, "session_start", { type: "session_start", reason: "new" }, makeCtx("s-inject-2", []));
  const e3 = startEvent(); await fire(pi, "before_agent_start", e3, makeCtx("s-inject-2", []));
  check(injectorCount() === 2 && e3.systemPromptOptions.sections.memoire, "injection: a new session runs the injector again");
}
for (const mode of ["fail", "empty", "hang"]) {
  const pi = makePi(); memoire(pi);
  const ctx = makeCtx(`s-${mode}`, []);
  await fire(pi, "session_start", { type: "session_start", reason: "startup" }, ctx);
  setMode("inject", mode);
  const t0 = Date.now();
  const e = startEvent(); await fire(pi, "before_agent_start", e, ctx);
  check(!("memoire" in e.systemPromptOptions.sections), `injection ${mode}: no section`);
  check(Date.now() - t0 < 6000, `injection ${mode}: released within the deadline (${Date.now() - t0} ms)`);
}
setMode("inject", "ok");

// --- new sessions: setup entries are real; cursor location ---------------------
{
  clearBatches();
  const pi = makePi(); memoire(pi);
  const entries = [
    { type: "model_change", id: "setup1", parentId: null, timestamp: ts, provider: "conductor", modelId: "coder" },
    { type: "thinking_level_change", id: "setup2", parentId: "setup1", timestamp: ts, thinkingLevel: "medium" },
  ];
  const ctx = makeCtx("s-new", entries);
  await fire(pi, "session_start", { type: "session_start", reason: "startup" }, ctx);
  entries.push(msg("sys9", { role: "system", content: "x", sections: {}, timestamp: 1 }), user("u9", "hi"));
  await settle(pi, ctx);
  const ids = readBatches().flat().filter((e) => e.event_type === "pi_entry").map((e) => e.source_event_id.split(":").pop());
  check(JSON.stringify(ids) === JSON.stringify(["setup1", "setup2", "sys9", "u9"]), `new session: setup entries present at start are captured (${ids})`);
  check(fs.existsSync(path.join(home, "capture-cursors", "pi", "s-new.json")), "cursor: written under capture-cursors/pi");
  check(!fs.existsSync(path.join(home, "pi-capture", "s-new.json")), "cursor: nothing written at the old place");
  // a cursor left at the old place is still honoured: its settled ids are not queued again
  fs.mkdirSync(path.join(home, "pi-capture"), { recursive: true });
  fs.writeFileSync(path.join(home, "pi-capture", "s-legacy.json"), JSON.stringify({ schema: "pi-capture-cursor/1", settled: ["l1"] }));
  clearBatches();
  const pi2 = makePi(); memoire(pi2);
  const legacy = [user("l1", "queued by an older extension"), user("l2", "new")];
  const lctx = makeCtx("s-legacy", legacy);
  await fire(pi2, "session_start", { type: "session_start", reason: "resume" }, lctx);
  await settle(pi2, lctx);
  const got = readBatches().flat().filter((e) => e.event_type === "pi_entry").map((e) => e.source_event_id.split(":").pop());
  check(JSON.stringify(got) === JSON.stringify(["l2"]), `legacy cursor: only the unsettled entry is queued (${got})`);
}

// --- capture -----------------------------------------------------------------
{
  clearBatches();
  const pi = makePi(); memoire(pi);
  const entries = [user("old1", "resumed history")];
  const ctx = makeCtx("s-cap", entries);
  await fire(pi, "session_start", { type: "session_start", reason: "resume" }, ctx);
  const ev = startEvent(); await fire(pi, "before_agent_start", ev, ctx);
  const injected = ev.systemPromptOptions.sections.memoire;
  entries.push(
    msg("sys1", { role: "system", content: `PREAMBLE\n${injected.trim()}\nRULES`, sections: { preamble: "PREAMBLE", memoire: injected }, timestamp: 1 }),
    user("u1", [{ type: "text", text: "look" }, { type: "image", data: "AAAABBBB", mimeType: "image/png" }]),
    assistant("a1", [{ type: "thinking", thinking: "hmm" }, { type: "text", text: "Checking." }, { type: "toolCall", id: "t1", name: "bash", arguments: { command: "ls" } }], "toolUse"),
    msg("r1", { role: "toolResult", toolCallId: "t1", toolName: "bash", content: [{ type: "text", text: "a b" }], isError: false, timestamp: 1 }),
    assistant("a2", [{ type: "text", text: "Done." }, { type: "text", text: "  " }, { type: "text", text: "Two files." }]),
    assistant("a3", [{ type: "text", text: "partial" }], "aborted"),
    assistant("a4", [{ type: "toolCall", id: "t2", name: "read", arguments: {} }], "toolUse"),
    { type: "model_change", id: "mc1", parentId: null, timestamp: ts, provider: "conductor", modelId: "coder" },
  );
  await settle(pi, ctx);
  const all = readBatches().flat();
  const ids = all.map((e) => e.source_event_id);
  check(!ids.some((id) => id.endsWith(":old1")), "capture: entries present at session start are not queued");
  const entryIds = all.filter((e) => e.event_type === "pi_entry").map((e) => e.source_event_id.split(":").pop());
  check(JSON.stringify(entryIds) === JSON.stringify(["sys1", "u1", "a1", "r1", "a2", "a3", "a4", "mc1"]), `capture: one pi_entry per new entry in order (${entryIds})`);
  const turns = all.filter((e) => e.event_type === "pi_assistant_turn");
  check(JSON.stringify(turns.map((t) => t.source_event_id)) === JSON.stringify(["pi:turn:s-cap:a1", "pi:turn:s-cap:a2"]), `capture: turns for normal ends with text only (${turns.map((t) => t.source_event_id)})`);
  check(turns[1]?.text === "Done.\n\nTwo files.", "capture: turn text joins non-blank text parts with a blank line");
  check(turns[0]?.text === "Checking.", "capture: thinking and tool calls stay out of the turn text");
  check(turns[0]?.payload?.model === "coder" && turns[0]?.payload?.stop_reason === "toolUse", "capture: turn payload carries model and stop reason");
  const first = all[0];
  check(first.project === "proj-key" && first.project_root === cwd && first.project_source === "registry"
    && first.source_adapter === "pi" && first.session_id === "s-cap" && first.cwd === cwd && first.occurred_at === ts,
  "capture: envelope fields (project, root, source, adapter, session, cwd, occurred_at)");
  const sys = all.find((e) => e.source_event_id === "pi:entry:s-cap:sys1").payload.entry.message;
  check(!("memoire" in sys.sections) && sys.sections.memoire_omitted === true && !sys.content.includes("obs é日本"),
    "capture: the system message loses its memoire section and the injected text");
  const img = all.find((e) => e.source_event_id === "pi:entry:s-cap:u1").payload.entry.message.content[1];
  check(img.data === undefined && img.data_omitted === true && img.data_chars === 8, "capture: image data replaced by a marker");

  clearBatches();
  await settle(pi, ctx);
  check(readBatches().length === 0, "capture: a second settle with nothing new writes nothing");

  setMode("queue", "fail");
  entries.push(user("u2", "again"));
  await settle(pi, ctx);
  check(readBatches().length === 0, "capture: failed write writes nothing");
  check(fs.readFileSync(path.join(home, "hooks", "pi-capture.log"), "utf8").includes("error=queue"), "capture: failed write is logged");
  setMode("queue", "ok");
  entries.push(user("u3", "more"));
  await settle(pi, ctx);
  const retried = readBatches().flat().map((e) => e.source_event_id.split(":").pop());
  check(JSON.stringify(retried) === JSON.stringify(["u2", "u3"]), `capture: failed entries are retried at the next settle (${retried})`);
}

// --- compaction checkpoint and restart delivery ------------------------------
{
  clearBatches();
  const sid = "s-restart";
  const entries = [user("pre1", "history from before the extension")];
  let pi = makePi(); memoire(pi);
  let ctx = makeCtx(sid, entries);
  await fire(pi, "session_start", { type: "session_start", reason: "resume" }, ctx);
  const ev = startEvent(); await fire(pi, "before_agent_start", ev, ctx);
  const injected = ev.systemPromptOptions.sections.memoire;
  // Pi 1.0.4 SessionManager.appendCompaction: the current system message rides
  // the checkpoint as `systemMessage`.
  entries.push({ type: "compaction", id: "cp1", parentId: "pre1", timestamp: ts, summary: "sum",
    firstKeptEntryId: "pre1", tokensBefore: 10,
    systemMessage: { role: "system", content: `PRE\n<memoire>\n${injected.trim()}\n</memoire>\nPOST`, sections: { preamble: "PRE", memoire: injected }, timestamp: 1 } });
  setMode("queue", "fail");
  await settle(pi, ctx);
  check(readBatches().length === 0, "restart: the failed write queued nothing");
  // Pi exits; a new process resumes the same session.
  setMode("queue", "ok");
  pi = makePi(); memoire(pi);
  entries.push(user("u9", "after restart"));
  ctx = makeCtx(sid, entries);
  await fire(pi, "session_start", { type: "session_start", reason: "resume" }, ctx);
  await settle(pi, ctx);
  const got = readBatches().flat().filter((e) => e.event_type === "pi_entry");
  check(JSON.stringify(got.map((e) => e.source_event_id.split(":").pop())) === JSON.stringify(["cp1", "u9"]),
    `restart: entries missed before the restart are queued, pre-extension history is not (${got.map((e) => e.source_event_id)})`);
  const cp = got.find((e) => e.source_event_id.endsWith(":cp1"))?.payload.entry.systemMessage;
  check(cp && !("memoire" in cp.sections) && cp.sections.memoire_omitted === true && !cp.content.includes("obs é日本"),
    "compaction: the checkpoint's systemMessage loses the memoire section and the injected text");
  const cursor = JSON.parse(fs.readFileSync(path.join(home, "capture-cursors", "pi", `${sid}.json`), "utf8"));
  check(JSON.stringify(cursor.settled.sort()) === JSON.stringify(["cp1", "pre1", "u9"]), `cursor: lists the settled ids (${cursor.settled})`);
}

// --- resume matrix: each case is two processes on one session ---------------
{
  const twoProcesses = async (sid, firstRun, secondRun) => {
    const entries = [];
    let pi = makePi(); memoire(pi);
    let ctx = makeCtx(sid, entries);
    await fire(pi, "session_start", { type: "session_start", reason: "startup" }, ctx);
    entries.push(user("first", "written in process 1"));
    await firstRun(pi, ctx);
    pi = makePi(); memoire(pi);
    ctx = makeCtx(sid, entries);
    await fire(pi, "session_start", { type: "session_start", reason: "resume" }, ctx);
    entries.push(user("second", "written in process 2"));
    clearBatches();
    await secondRun(pi, ctx);
    return readBatches().flat().map((e) => e.source_event_id.split(":").pop());
  };
  clearBatches();
  let got = await twoProcesses("m-off-on",
    async (pi, ctx) => { process.env.MEMOIRE_CAPTURE = "off"; await settle(pi, ctx); delete process.env.MEMOIRE_CAPTURE; },
    async (pi, ctx) => { await settle(pi, ctx); });
  check(JSON.stringify(got) === JSON.stringify(["second"]), `resume off->on: entries written while capture was off are never queued (${got})`);
  got = await twoProcesses("m-proj-off-on",
    async (pi, ctx) => { setMode("resolve", "off"); await settle(pi, ctx); setMode("resolve", "on"); },
    async (pi, ctx) => { await settle(pi, ctx); });
  check(JSON.stringify(got) === JSON.stringify(["second"]), `resume project-off->on: excluded entries are never queued (${got})`);
  got = await twoProcesses("m-fail-retry",
    async (pi, ctx) => { setMode("queue", "fail"); await settle(pi, ctx); setMode("queue", "ok"); },
    async (pi, ctx) => { await settle(pi, ctx); });
  check(JSON.stringify(got) === JSON.stringify(["first", "second"]), `resume failure->restart: the failed entry is retried (${got})`);
  got = await twoProcesses("m-ok-idem",
    async (pi, ctx) => { await settle(pi, ctx); },
    async (pi, ctx) => { await settle(pi, ctx); });
  check(JSON.stringify(got) === JSON.stringify(["second"]), `resume success->restart: nothing is queued twice (${got})`);
}

// --- capture switches --------------------------------------------------------
{
  clearBatches();
  const pi = makePi(); memoire(pi);
  const entries = [];
  const ctx = makeCtx("s-off", entries);
  await fire(pi, "session_start", { type: "session_start", reason: "startup" }, ctx);
  entries.push(user("u1", "x"));
  process.env.MEMOIRE_CAPTURE = "off";
  await settle(pi, ctx);
  check(readBatches().length === 0, "switch: MEMOIRE_CAPTURE=off queues nothing");
  delete process.env.MEMOIRE_CAPTURE;
  setMode("resolve", "off");
  await settle(pi, ctx);
  check(readBatches().length === 0, "switch: a capture-disabled project queues nothing");
  setMode("resolve", "on");
  entries.push(user("u2", "y"));
  await settle(pi, ctx);
  const after = readBatches().flat().map((e) => e.source_event_id.split(":").pop());
  check(JSON.stringify(after) === JSON.stringify(["u2"]), `switch: entries settled while disabled are never queued later (${after})`);
}

if (failures) { console.error(`pi-extension-test: ${failures} failure(s)`); process.exit(1); }
console.log("pi-extension-test: OK");
