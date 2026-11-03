// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// subagent-context-plugin-test.mjs — OpenCode child-session leg of the
// sub-agent parent-context feature. Runs under plain node with a fake Bun
// shell and a fake opencode client: no opencode, no Bun, no network, no live
// database. Four modes; gen, block and swdir are driven by subagent-context-test.sh
// (home lane), run by subagent-context-plugin-test.sh (public core lane):
//   gen <dir> [longanchor]   write one equivalent scenario as a Claude transcript, a Codex
//                rollout and an OpenCode message list (parity fixtures)
//   block <dir>  run the plugin on the OpenCode fixture, print the parent block
//   swdir <dir>  for each numeric subdirectory holding a capture-tick.env, print
//                "<n> on|off": does the plugin's switch parser enable injection
//   run          the plugin suite: switch default-off, opt-in parsing, provider
//                boundary (B5), parent limit and large history (B8, B9f),
//                gates on both directories (B9d), re-delegation, exclusions,
//                deadline, no content in the log

import { pathToFileURL } from "node:url";
import path from "node:path";
import fs from "node:fs";
import os from "node:os";

const here = path.dirname(new URL(import.meta.url).pathname);
const pluginPath = process.env.MEMOIRE_PLUGIN_PATH
  ? path.resolve(process.env.MEMOIRE_PLUGIN_PATH)
  : path.resolve(here, "../opencode-plugin/memoire.js");
const realResolver = path.resolve(here, "../memoire-resolve-project.sh");

let failures = 0;
const check = (cond, label) => { if (!cond) { failures += 1; console.error(`FAIL: ${label}`); } };

// ---- fakes -------------------------------------------------------------------
const okShell = (text) => () => {
  const p = Promise.resolve({ exitCode: 0, text: () => text });
  p.cwd = () => p; p.nothrow = () => p; p.quiet = () => p;
  return p;
};

function mkClient({ sessions, messages }) {
  const calls = { get: [], messages: [], message: [] };
  return {
    calls,
    session: {
      // session.message({path:{id, messageID}}) -> {data:{info, parts}} (SDK 1.14.48 SessionMessageResponses)
      message: async ({ path: p }) => {
        calls.message.push(p.messageID);
        const m = messages[p.id];
        const all = typeof m === "function" ? await m(undefined) : m;
        const hit = (all ?? []).find((x) => x.info.id === p.messageID);
        return hit ? { data: hit } : { data: undefined };
      },
      get: async ({ path: p }) => { calls.get.push(p.id); return sessions[p.id] ? { data: sessions[p.id] } : { data: undefined }; },
      messages: async (args) => {
        calls.messages.push(args);
        const m = messages[args.path.id];
        return { data: typeof m === "function" ? await m(args.query?.limit) : m };
      },
    },
  };
}

async function makeHook({ client, timeline = "", directory = "/tmp/proj" } = {}) {
  const { MemoirePlugin } = await import(pathToFileURL(pluginPath).href + `?v=${Math.random()}`);
  const hooks = await MemoirePlugin({ $: okShell(timeline), directory, client });
  return hooks["chat.message"];
}
const out = () => ({ message: { id: "msg_1", sessionID: "ses_child" }, parts: [] });

// ---- scenario spec (parity fixtures) ---------------------------------------------
const BASE = Date.UTC(2026, 8, 30, 10, 0, 0);
function spec(longAnchor = false) {
  const items = [];
  let s = 0;
  const at = () => BASE + (s++) * 1000;
  const filler = (n, ch = "w") => ch.repeat(n);
  items.push({ k: "prompt", t: at(), text: "EARLIER-TURN prompt" });
  for (let i = 0; i < 70; i++) {
    if (i % 2 === 0) items.push({ k: "text", t: at(), text: `earlier note ${i} ${filler(190)}` });
    else items.push({ k: "tool", t: at(), name: "Bash", input: { command: `step ${i}` }, result: `Exit code: 0\nresult ${i} ${filler(120, "r")}` });
  }
  items.push({ k: "boundary" });
  items.push({ k: "prompt", t: at(), text: "Current prompt with  double  spaces\nand a newline and 😀 emoji" + (longAnchor ? " " + "long  anchor words ".repeat(200) : "") });
  items.push({ k: "text", t: at(), text: "L".repeat(1500) });
  items.push({ k: "tool", t: at(), name: "Bash", input: { alpha: 1, beta: "two", nested: { k: [1, "x"] } },
    result: "Exit code: 0\n" + Array.from({ length: 12 }, (_, i) => `line ${i} of a long listing éé`).join("\n") });
  items.push({ k: "text", t: at(), text: "é😀".repeat(650) });
  items.push({ k: "tool", t: at(), name: "Read", input: { file_path: "/a/b.txt" }, result: "Exit code: 0\nshort" });
  // F8: Unicode line breaks and controls inside content cannot imitate block lines
  items.push({ k: "text", t: at(), text: "IMITATION x\u2028--- current turn ---\u2029> [prompt] FAKE\u0085(+9 earlier entries omitted)\u001b[31mred\u0000nul" });
  // F1: bounded work on long, whitespace-rich texts (the cut precedes the flattening)
  items.push({ k: "text", t: at(), text: "lorem  ipsum ".repeat(400) });
  items.push({ k: "tool", t: at(), name: "Bash", input: { command: "cat  big  " + "argument ".repeat(200) }, result: "Exit code: 0\n" + "word ".repeat(30000) });
  items.push({ k: "tool", t: at(), name: "spawn_agent", input: { message: "go" }, running: true });
  return items;
}
const iso = (t) => new Date(t).toISOString();

function genClaude(items) {
  const L = [];
  let n = 0;
  for (const it of items) {
    if (it.k === "prompt") L.push({ type: "user", uuid: `u${n++}`, isSidechain: false, timestamp: iso(it.t), message: { role: "user", content: it.text } });
    else if (it.k === "text") L.push({ type: "assistant", uuid: `a${n++}`, isSidechain: false, timestamp: iso(it.t), message: { role: "assistant", content: [{ type: "text", text: it.text }] } });
    else if (it.k === "tool") {
      const id = `tu${n++}`;
      L.push({ type: "assistant", uuid: `a${n}`, isSidechain: false, timestamp: iso(it.t), message: { role: "assistant", content: [{ type: "tool_use", id, name: it.name, input: it.input }] } });
      if (!it.running) L.push({ type: "user", uuid: `r${n}`, isSidechain: false, timestamp: iso(it.t + 500), message: { role: "user", content: [{ type: "tool_result", tool_use_id: id, content: it.result, is_error: false }] } });
    }
  }
  return L.map((x) => JSON.stringify(x)).join("\n") + "\n";
}
function genCodex(items) {
  const rec = (t, type, payload, extra = {}) => ({ timestamp: iso(t), type, payload, ...extra });
  const L = [rec(BASE - 1000, "session_meta", { id: "thread-par", cwd: "/x", cli_version: "0.159.1" })];
  let n = 0;
  let turn = 0;
  const startTurn = (t, id) => L.push(rec(t, "event_msg", { type: "task_started", turn_id: id }));
  for (const it of items) {
    if (it.k === "prompt") {
      turn += 1;
      const tid = turn === 1 ? "tprev" : "tcur";
      startTurn(it.t - 1, tid);
      L.push(rec(it.t, "response_item", { type: "message", role: "user", content: [{ type: "input_text", text: it.text }] }, { metadata: { client_authored: true } }));
    } else if (it.k === "text") L.push(rec(it.t, "response_item", { type: "message", role: "assistant", content: [{ type: "output_text", text: it.text }] }));
    else if (it.k === "tool") {
      const id = `call_${n++}`;
      L.push(rec(it.t, "response_item", { type: "function_call", name: it.name, arguments: JSON.stringify(it.input), call_id: id }));
      if (!it.running) L.push(rec(it.t + 500, "response_item", { type: "function_call_output", call_id: id, output: it.result }));
    }
  }
  return L.map((x) => JSON.stringify(x)).join("\n") + "\n";
}
function genOpenCode(items, providerID = "prov") {
  const M = [];
  let n = 0;
  const info = (role, t) => ({ id: `msg_${n++}`, sessionID: "ses_parent", role, time: { created: t }, ...(role === "assistant" ? { providerID, modelID: "m" } : { model: { providerID, modelID: "m" } }) });
  for (const it of items) {
    if (it.k === "prompt") M.push({ info: info("user", it.t), parts: [{ type: "text", text: it.text }, { type: "text", text: "SYNTHETIC-INJECTED", synthetic: true, id: "prt_memoire_x" }] });
    else if (it.k === "text") M.push({ info: info("assistant", it.t), parts: [{ type: "text", text: it.text }] });
    else if (it.k === "tool") M.push({ info: info("assistant", it.t), parts: [{ type: "tool", callID: `c${n}`, tool: it.name,
      state: it.running ? { status: "running", input: it.input, time: { start: it.t } } : { status: "completed", input: it.input, output: it.result, time: { start: it.t, end: it.t + 500 } } }] });
  }
  return M;
}

const mode = process.argv[2] ?? "run";

if (mode === "gen") {
  const dir = process.argv[3];
  // longanchor: the anchor prompt is ~3800 characters (A1: small MAX_CHARS)
  const items = spec(process.argv[4] === "longanchor").filter((x) => x.k !== "boundary");
  fs.writeFileSync(path.join(dir, "claude.jsonl"), genClaude(items));
  fs.writeFileSync(path.join(dir, "codex.jsonl"), genCodex(items));
  fs.writeFileSync(path.join(dir, "opencode.json"), JSON.stringify(genOpenCode(items)));
  process.exit(0);
}

process.env.MEMOIRE_PROJECTS_REGISTRY ||= "/nonexistent/memoire-projects.json";
process.env.MEMOIRE_RESOLVE_PROJECT = realResolver;
process.env.TZ ||= "UTC";

if (mode === "swdir") {
  const dir = process.argv[3];
  const names = fs.readdirSync(dir).filter((n) => /^[0-9]+$/.test(n)).sort((a, b) => Number(a) - Number(b));
  const T0 = 1_000_000_000_000;
  const msgs = [
    { info: { id: "m1", role: "user", time: { created: T0 }, model: { providerID: "prov", modelID: "m" } }, parts: [{ type: "text", text: "P" }] },
    { info: { id: "m2", role: "assistant", time: { created: T0 + 1000 }, providerID: "prov", modelID: "m" }, parts: [{ type: "text", text: "A" }] },
  ];
  for (const n of names) {
    process.env.MEMOIRE_HOME = path.join(dir, n);
    const client = mkClient({
      sessions: { ses_child: { id: "ses_child", parentID: "ses_parent", directory: dir }, ses_parent: { id: "ses_parent", directory: dir } },
      messages: { ses_parent: msgs },
    });
    const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK", directory: dir });
    const o = out();
    await hook({ sessionID: "ses_child", messageID: "msg_1", model: { providerID: "prov", modelID: "m" } }, o);
    console.log(`${n} ${o.parts.length === 2 ? "on" : "off"}`);
  }
  process.exit(0);
}

if (mode === "block") {
  const dir = process.argv[3];
  const messages = JSON.parse(fs.readFileSync(path.join(dir, "opencode.json"), "utf8"));
  const client = mkClient({
    sessions: { ses_child: { id: "ses_child", parentID: "ses_parent", directory: dir }, ses_parent: { id: "ses_parent", directory: dir } },
    messages: { ses_parent: messages },
  });
  const hook = await makeHook({ client, directory: dir });
  const o = out();
  await hook({ sessionID: "ses_child", messageID: "msg_1", model: { providerID: "prov", modelID: "m" } }, o);
  process.stdout.write((o.parts.at(-1)?.text ?? "") + "\n");
  process.exit(0);
}

// =============================== run: the plugin suite =================================
const scratch = fs.mkdtempSync(path.join(process.env.TMPDIR || os.tmpdir(), "subagent-plugin-test."));
process.on("exit", () => fs.rmSync(scratch, { recursive: true, force: true }));
process.env.HOME = path.join(scratch, "home");
process.env.MEMOIRE_HOME = path.join(scratch, "mh");
fs.mkdirSync(process.env.MEMOIRE_HOME, { recursive: true });
const envFile = path.join(process.env.MEMOIRE_HOME, "capture-tick.env");
const logFile = path.join(process.env.MEMOIRE_HOME, "hooks", "subagent.log");
const setEnvFile = (text) => (text === null ? fs.rmSync(envFile, { force: true }) : fs.writeFileSync(envFile, text));
const readLog = () => (fs.existsSync(logFile) ? fs.readFileSync(logFile, "utf8") : "");
const lastLog = () => readLog().trim().split("\n").at(-1) ?? "";
const dirOk = path.join(scratch, "ok"); const dirParentOff = path.join(scratch, "poff"); const dirChildOff = path.join(scratch, "coff");
for (const d of [dirOk, dirParentOff, dirChildOff]) fs.mkdirSync(d, { recursive: true });
fs.writeFileSync(path.join(dirParentOff, ".memoire"), "project: pp\ncapture: false\n");
fs.writeFileSync(path.join(dirChildOff, ".memoire"), "project: cc\ncapture: false\n");

const T = 1_000_000_000_000;
const parentMsgs = (provider = "prov", extra = []) => [
  { info: { id: "m1", role: "user", time: { created: T }, model: { providerID: provider, modelID: "m" } }, parts: [{ type: "text", text: "PARENT-PROMPT" }, { type: "text", text: "SYNTH-PART", synthetic: true, id: "prt_memoire_abc" }, { type: "text", text: "IGNORED-PART", ignored: true }] },
  { info: { id: "m2", role: "assistant", time: { created: T + 1000 }, providerID: provider, modelID: "m" }, parts: [
    { type: "text", text: "PARENT-ASSISTANT-TEXT" },
    { type: "tool", callID: "c1", tool: "bash", state: { status: "completed", input: { command: "echo PARENT-MARKER" }, output: "PARENT-MARKER", time: { start: T + 2000, end: T + 2100 } } },
  ] },
  ...extra,
];
const sessions = (dirs = {}) => ({
  ses_child: { id: "ses_child", parentID: "ses_parent", directory: dirs.child ?? dirOk },
  ses_parent: { id: "ses_parent", directory: dirs.parent ?? dirOk },
  ses_plain: { id: "ses_plain", directory: dirOk },
});
const childInput = (provider = "prov", sid = "ses_child") => ({ sessionID: sid, messageID: "msg_1", ...(provider ? { model: { providerID: provider, modelID: "m" } } : {}) });
const parentPart = (o) => o.parts.filter((p) => p.text !== "TIMELINE-BLOCK");

// --- default off: nothing is called, nothing but the timeline is injected ---------
setEnvFile(null);
{
  const client = mkClient({ sessions: sessions(), messages: { ses_parent: parentMsgs() } });
  const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
  const o = out();
  await hook(childInput(), o);
  check(client.calls.get.length === 0 && client.calls.messages.length === 0, "off: no client call is made");
  check(o.parts.length === 1 && o.parts[0].text === "TIMELINE-BLOCK", "off: only the existing timeline part");
  check(readLog() === "", "off: nothing logged");
}

// --- opt-in parsing (same rule as the bash side) -------------------------------------
async function injects(envText) {
  setEnvFile(envText);
  const client = mkClient({ sessions: sessions(), messages: { ses_parent: parentMsgs() } });
  const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
  const o = out();
  await hook(childInput(), o);
  return o.parts.length === 2;
}
for (const [txt, want] of [
  ["MEMOIRE_SUBAGENT_CONTEXT=off\n", false], ["MEMOIRE_SUBAGENT_CONTEXT=\n", false], ["#MEMOIRE_SUBAGENT_CONTEXT=on\n", false],
  ["XMEMOIRE_SUBAGENT_CONTEXT=on\n", false], ["MEMOIRE_SUBAGENT_CONTEXT=on\n", true], ["MEMOIRE_SUBAGENT_CONTEXT=1\n", true],
  ["MEMOIRE_SUBAGENT_CONTEXT=true\n", true], ["MEMOIRE_SUBAGENT_CONTEXT=ON\n", true], ['MEMOIRE_SUBAGENT_CONTEXT="on"\n', true],
  ["export MEMOIRE_SUBAGENT_CONTEXT='on'  # opted in\n", true], ["MEMOIRE_SUBAGENT_CONTEXT=on\nMEMOIRE_SUBAGENT_CONTEXT=off\n", false],
  ["MEMOIRE_SUBAGENT_CONTEXT=off\nMEMOIRE_SUBAGENT_CONTEXT=on\r\n", true],
]) check((await injects(txt)) === want, `switch parsing: ${JSON.stringify(txt)} -> ${want}`);

// --- enabled, same provider: a second synthetic part carries the block ------------------
setEnvFile("MEMOIRE_SUBAGENT_CONTEXT=on\n");
fs.rmSync(path.dirname(logFile), { recursive: true, force: true });
{
  const client = mkClient({ sessions: sessions(), messages: { ses_parent: parentMsgs() } });
  const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
  const o = out();
  await hook(childInput(), o);
  check(o.parts.length === 2 && o.parts[0].text === "TIMELINE-BLOCK", "on: the timeline part is unchanged and comes first");
  const p = o.parts[1] ?? {};
  check(p.type === "text" && p.synthetic === true && /^prt_memoire_/.test(p.id ?? ""), "on: the parent part is a synthetic prt_memoire_ text part");
  check(p.sessionID === "ses_child" && p.messageID === "msg_1", "on: the part carries sessionID and messageID");
  check(p.text.includes("> [prompt] PARENT-PROMPT") && p.text.includes("tool bash command=echo PARENT-MARKER → ok PARENT-MARKER") && p.text.includes("text: PARENT-ASSISTANT-TEXT"), "on: prompt, tool call with result excerpt, assistant text");
  check(!p.text.includes("SYNTH-PART") && !p.text.includes("IGNORED-PART"), "B7: every synthetic (and ignored) part is excluded, not only prt_memoire_ ids");
  check(p.text.split("\n").at(-1).startsWith("Parent session ses_parent (project ok). This block is the parent's recent activity only."), "on: the pointer is the last line");
  check(client.calls.messages[0].query?.limit === 201, "B8/A5: the parent fetch asks for one message more than the 200-message page (to know whether the page was cut)");
  check(/status=injected session=ses_child parent=ses_parent entries=3 chars=\d+ elapsed_ms=\d+/.test(lastLog()), "on: one log line with ids and sizes");
  check(!readLog().includes("PARENT-MARKER"), "on: the log carries no content");
  // the same child again with nothing new: re-delegation sends nothing
  const o2 = out();
  await hook(childInput(), o2);
  check(o2.parts.length === 0, "resume: nothing new since the last block -> no part (timeline is once per session)");
  check(lastLog().includes("status=nothing-new"), "resume: logged");
}

// --- session-start injection off (the injector prints nothing): the parent part is still pushed ---
{
  const client = mkClient({ sessions: sessions(), messages: { ses_parent: parentMsgs() } });
  const hook = await makeHook({ client, timeline: "" });
  const o = out();
  await hook(childInput(), o);
  check(o.parts.length === 1 && /^prt_memoire_/.test(o.parts[0].id ?? "") && o.parts[0].text.includes("> [prompt] PARENT-PROMPT"),
    "empty timeline (MEMOIRE_INJECT off): the parent-context part is still pushed, alone");
}

// --- re-delegation: only what happened after the last block (task_id path) ---------------
{
  let msgs = parentMsgs();
  const client = mkClient({ sessions: sessions(), messages: { ses_parent: () => msgs } });
  const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
  const o1 = out(); await hook(childInput(), o1);
  msgs = [...msgs,
    { info: { id: "m3", role: "assistant", time: { created: T + 9000 }, providerID: "prov" }, parts: [{ type: "text", text: "NEW-ASSISTANT-TEXT" }, { type: "tool", callID: "c9", tool: "bash", state: { status: "completed", input: { command: "echo NEW-MARKER" }, output: "NEW-MARKER", time: { start: T + 9500 } } }] }];
  const o2 = out(); await hook(childInput(), o2);
  const t2 = o2.parts[0]?.text ?? "";
  check(o2.parts.length === 1 && t2.includes("NEW-ASSISTANT-TEXT") && t2.includes("NEW-MARKER"), "resume: the new block covers the new parent activity");
  check(!t2.includes("PARENT-ASSISTANT-TEXT") && !t2.includes("PARENT-MARKER"), "resume: entries at or before lastInjected are not sent again");
  check(t2.includes("> [prompt] PARENT-PROMPT"), "resume: the anchor still opens the block");
}

// --- provider boundary (B5, B9b) ------------------------------------------------------------
async function providerCase({ parentProv, childProv, envText }) {
  setEnvFile(envText);
  const client = mkClient({ sessions: sessions(), messages: { ses_parent: parentMsgs(parentProv) } });
  const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
  const o = out();
  await hook(childInput(childProv), o);
  return { parts: parentPart(o), log: lastLog() };
}
{
  let r = await providerCase({ parentProv: "anthropic", childProv: "openai", envText: "MEMOIRE_SUBAGENT_CONTEXT=on\n" });
  check(r.parts.length === 1 && r.parts[0].text.startsWith("Parent session ses_parent (project ok).") && !r.parts[0].text.includes("\n"), "B9b: another provider: the pointer line only");
  check(!r.parts[0].text.includes("PARENT-MARKER") && !r.parts[0].text.includes("PARENT-PROMPT"), "B9b: another provider: no parent content");
  check(r.log.includes("xprovider=withheld"), "B9b: logged as xprovider=withheld");
  r = await providerCase({ parentProv: "anthropic", childProv: "openai", envText: "MEMOIRE_SUBAGENT_CONTEXT=on\nMEMOIRE_SUBAGENT_CONTEXT_XPROVIDER=on\n" });
  check(r.parts.length === 1 && r.parts[0].text.includes("PARENT-MARKER"), "B9b: the XPROVIDER switch releases the block");
  r = await providerCase({ parentProv: "anthropic", childProv: "openai", envText: "MEMOIRE_SUBAGENT_CONTEXT=on\nMEMOIRE_SUBAGENT_CONTEXT_XPROVIDER=off\n" });
  check(r.parts[0].text.split("\n").length === 1, "B9b: XPROVIDER=off keeps it withheld");
  r = await providerCase({ parentProv: "anthropic", childProv: "anthropic", envText: "MEMOIRE_SUBAGENT_CONTEXT=on\n" });
  check(r.parts[0].text.includes("PARENT-MARKER"), "B9b: the same provider gets the block without the XPROVIDER switch");
  r = await providerCase({ parentProv: "anthropic", childProv: "", envText: "MEMOIRE_SUBAGENT_CONTEXT=on\n" });
  check(r.parts[0].text.split("\n").length === 1, "B9b: an unknown child provider counts as different (withheld)");
  // XPROVIDER alone (no base switch) injects nothing
  r = await providerCase({ parentProv: "a", childProv: "b", envText: "MEMOIRE_SUBAGENT_CONTEXT_XPROVIDER=on\n" });
  check(r.parts.length === 0, "B9b: XPROVIDER does not enable the feature by itself");
}

// --- gates on both directories (B9d) ---------------------------------------------------------
setEnvFile("MEMOIRE_SUBAGENT_CONTEXT=on\n");
{
  for (const [label, dirs] of [["parent", { parent: dirParentOff }], ["child", { child: dirChildOff }]]) {
    const client = mkClient({ sessions: sessions(dirs), messages: { ses_parent: parentMsgs() } });
    const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
    const o = out();
    await hook(childInput(), o);
    check(o.parts.length === 1 && o.parts[0].text === "TIMELINE-BLOCK", `B9d: capture:false on the ${label}'s directory: only the timeline part`);
    check(client.calls.messages.length === 0, `B9d: capture:false on the ${label}'s directory: the parent messages are never read`);
    check(lastLog().includes("status=capture-off"), `B9d: ${label} gate logged`);
  }
}

// --- non-child sessions ----------------------------------------------------------------------
{
  const client = mkClient({ sessions: sessions(), messages: { ses_parent: parentMsgs() } });
  const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
  for (let i = 0; i < 3; i++) { const o = out(); await hook(childInput("prov", "ses_plain"), o); check(parentPart(o).length === 0, "plain session: no parent block"); }
  check(client.calls.get.filter((x) => x === "ses_plain").length === 1, "plain session: the lookup is cached, not repeated per message");
  const o = out(); await hook(childInput("prov", "ses_unknown"), o);
  check(parentPart(o).length === 0, "unknown session: fail-open, no parent block");
}

// --- large history (B8, B9f) --------------------------------------------------------------------
{
  const N = 200_000;
  let asked;
  const big = async (limit) => {
    asked = limit;
    const from = limit ? N - limit : 0;
    const arr = [];
    for (let i = from; i < N; i++) {
      arr.push(i % 3 === 0
        ? { info: { id: `m${i}`, role: "user", time: { created: T + i * 1000 } }, parts: [{ type: "text", text: `prompt ${i}` }] }
        : { info: { id: `m${i}`, role: "assistant", time: { created: T + i * 1000 }, providerID: "prov" }, parts: [{ type: "text", text: `answer ${i} ${"x".repeat(200)}` }] });
    }
    return arr;
  };
  const client = mkClient({ sessions: sessions(), messages: { ses_parent: big } });
  const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
  const t0 = Date.now();
  const o = out();
  await hook(childInput(), o);
  const ms = Date.now() - t0;
  const text = parentPart(o)[0]?.text ?? "";
  check(asked === 201, "B9f: the fetch asked for 201 messages (page + 1), not the whole history");
  check(ms < 5000, `B9f: large history stays inside the injector deadline (${ms} ms)`);
  check(text.includes(`answer ${N - 1} `) || text.includes(`answer ${N - 2} `), "B9f: the block is built from the newest messages");
  check(!text.includes("answer 1 ") && !text.includes("prompt 0"), "B9f: nothing from the far past");
  check(text.includes("or more earlier entries omitted)"), "B9f: a full page means older entries were not read");
  check([...text].length <= 8000, "B9f: within the cap");
}


// --- F2: OpenCode writes one assistant message per model step; a turn of many steps pushes
// the user message that opened it out of the 200-message page. The anchor is fetched from the
// newest assistant message's parentID (AssistantMessage.parentID, SDK 1.14.48).
{
  const N = 450; // assistant steps after ONE prompt
  const all = [{ info: { id: "msg_open", role: "user", time: { created: T }, model: { providerID: "prov", modelID: "m" } }, parts: [{ type: "text", text: "OPENING-PROMPT-OF-THE-TURN" }] }];
  for (let i = 0; i < N; i++) {
    all.push({ info: { id: `msg_step${i}`, role: "assistant", parentID: "msg_open", time: { created: T + 1000 + i * 1000 }, providerID: "prov", modelID: "m" },
      parts: [{ type: "tool", callID: `c${i}`, tool: "bash", state: { status: "completed", input: { command: `step ${i}` }, output: `out ${i}`, time: { start: T + 1000 + i * 1000 } } }] });
  }
  const page = async (limit) => (limit ? all.slice(-limit) : all);
  const client = mkClient({ sessions: sessions(), messages: { ses_parent: page } });
  const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
  const o = out();
  await hook(childInput(), o);
  const text = parentPart(o)[0]?.text ?? "";
  check(client.calls.messages[0].query?.limit === 201, "F2: the page is still limited (200 + 1)");
  check(client.calls.messages.length === 1, "N3: no wide fetch when the opening message was found through parentID");
  check(text.includes("> [prompt] OPENING-PROMPT-OF-THE-TURN"), "F2: the opening prompt is fetched through parentID when the page does not hold it");
  check(!text.includes("(no operator prompt in the recent window)"), "F2: no 'no prompt' note");
  check(client.calls.message.includes("msg_open"), "F2: session.message was asked for the parent's opening message");
  check(text.includes(`step ${N - 1} `) || text.includes(`step ${N - 1}\n`) || /step 449/.test(text), "F2: newest steps kept");
  const lines = text.split("\n");
  check(lines[1].startsWith("> [prompt] OPENING-PROMPT"), "F2: the anchor sits right after the header");
  check(text.includes("or more earlier entries omitted)") || text.includes("earlier entries omitted)"), "F2: omission line present");
  // a failing fetch of the opening message is fail-open: the note, not an error
  const client2 = mkClient({ sessions: sessions(), messages: { ses_parent: page } });
  client2.session.message = async () => { throw new Error("gone"); };
  const hook2 = await makeHook({ client: client2, timeline: "TIMELINE-BLOCK" });
  const o2 = out();
  await hook2(childInput(), o2);
  const t2 = parentPart(o2)[0]?.text ?? "";
  check(t2.includes("(no operator prompt in the recent window)") && t2.includes(`step ${N - 1}`), "F2: a failing opening-message fetch falls back to the note");
}


{
  // R3-1: the route caps `limit` at 200 (answers 200 when asked for 201). The page is full and holds no prompt:
  // the opening prompt is still fetched.
  const N = 450;
  const all = [{ info: { id: "msg_open", role: "user", time: { created: T }, model: { providerID: "prov", modelID: "m" } }, parts: [{ type: "text", text: "OPENING-PROMPT-OF-THE-TURN" }] }];
  for (let i = 0; i < N; i++) {
    all.push({ info: { id: `msg_step${i}`, role: "assistant", parentID: "msg_open", time: { created: T + 1000 + i * 1000 }, providerID: "prov", modelID: "m" },
      parts: [{ type: "tool", callID: `c${i}`, tool: "bash", state: { status: "completed", input: { command: `step ${i}` }, output: `out ${i}`, time: { start: T + 1000 + i * 1000 } } }] });
  }
  const capped = async (limit) => (limit ? all.slice(-Math.min(limit, 200)) : all); // limit undefined: the single-message lookup of the fake
  const client = mkClient({ sessions: sessions(), messages: { ses_parent: capped } });
  const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
  const o = out();
  await hook(childInput(), o);
  const text = parentPart(o)[0]?.text ?? "";
  check(client.calls.messages[0].query?.limit === 201 && client.calls.messages.length === 1, "R3-1: the route capped the answer at 200 (one messages call)");
  check(client.calls.message.includes("msg_open"), "R3-1: capped route, 450-step turn: the opening message is fetched");
  check(text.split("\n")[1]?.startsWith("> [prompt] OPENING-PROMPT-OF-THE-TURN"), "R3-1: capped route: the block opens with the real prompt, right after the header");
  check(!text.includes("(no operator prompt in the recent window)"), "R3-1: capped route: no 'no prompt' note");
}

// --- N3a: the child's provider falls back to the message's own model when input.model is absent -----------------------------
{
  const outM = (prov) => ({ message: { id: "msg_1", sessionID: "ses_child", model: prov ? { providerID: prov, modelID: "m" } : undefined }, parts: [] });
  const run = async ({ inputModel, msgModel, parentProv = "prov" }) => {
    setEnvFile("MEMOIRE_SUBAGENT_CONTEXT=on\n");
    const client = mkClient({ sessions: sessions(), messages: { ses_parent: parentMsgs(parentProv) } });
    const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
    const o = outM(msgModel);
    await hook({ sessionID: "ses_child", messageID: "msg_1", ...(inputModel ? { model: { providerID: inputModel, modelID: "m" } } : {}) }, o);
    return { text: parentPart(o)[0]?.text ?? "", log: lastLog() };
  };
  let r = await run({ inputModel: "", msgModel: "prov" });
  check(r.text.includes("PARENT-MARKER") && r.log.includes("status=injected"), "N3a: no input.model, the message's model is the parent's provider: the block is sent");
  r = await run({ inputModel: "", msgModel: "other" });
  check(r.text.startsWith("Parent session ses_parent") && r.text.split("\n").length === 1 && r.log.includes("xprovider=withheld"), "N3a: no input.model, the message's model is another provider: withheld");
  r = await run({ inputModel: "", msgModel: "" });
  check(r.text.split("\n").length === 1 && r.log.includes("xprovider=withheld"), "N3a: neither known: withheld");
  r = await run({ inputModel: "prov", msgModel: "other" });
  check(r.text.includes("PARENT-MARKER"), "N3a: input.model wins when present");
  r = await run({ inputModel: "other", msgModel: "prov" });
  check(r.text.split("\n").length === 1 && r.log.includes("xprovider=withheld"), "N3a: input.model wins when present (another provider: withheld)");
}

// --- N3b: a compaction in the middle of the parent's turn ------------------------------------------------------------------
// After a compaction the newest assistant message's parentID names a synthetic "continue" user message (or a compaction
// message, or nothing that exists any more). Such a message is not an operator prompt. The plugin then makes ONE wider
// fetch (limit 1000, same deadline) and anchors on the newest real operator prompt in it; when there is none, or the
// fetch fails, the block opens with the no-prompt note. It never anchors on the synthetic message.
{
  const mk = (preSteps, postSteps) => {
    const all = [{ info: { id: "msg_open", role: "user", time: { created: T }, model: { providerID: "prov", modelID: "m" } }, parts: [{ type: "text", text: "PRE-COMPACTION-PROMPT" }] }];
    let n = 0;
    const step = (parent) => all.push({ info: { id: `msg_s${n}`, role: "assistant", parentID: parent, time: { created: T + 1000 + (n++) * 1000 }, providerID: "prov", modelID: "m" },
      parts: [{ type: "tool", callID: `c${n}`, tool: "bash", state: { status: "completed", input: { command: `step ${n}` }, output: `out ${n}`, time: { start: T + 1000 + n * 1000 } } }] });
    for (let i = 0; i < preSteps; i++) step("msg_open");
    all.push({ info: { id: "msg_comp", role: "user", time: { created: T + 1000 + (n++) * 1000 }, model: { providerID: "prov", modelID: "m" } }, parts: [{ type: "compaction", auto: true }] });
    all.push({ info: { id: "msg_sum", role: "assistant", parentID: "msg_comp", summary: true, time: { created: T + 1000 + (n++) * 1000 }, providerID: "prov", modelID: "m" }, parts: [{ type: "text", text: "SUMMARY-OF-EARLIER-WORK" }] });
    all.push({ info: { id: "msg_cont", role: "user", time: { created: T + 1000 + (n++) * 1000 }, model: { providerID: "prov", modelID: "m" } }, parts: [{ type: "text", text: "Continue if you have next steps", synthetic: true }] });
    for (let i = 0; i < postSteps; i++) step("msg_cont");
    return all;
  };
  const drive = async (all, { failWide = false } = {}) => {
    const page = async (limit) => { if (failWide && limit > 201) throw new Error("wide fetch failed"); return limit ? all.slice(-limit) : all; };
    const client = mkClient({ sessions: sessions(), messages: { ses_parent: page } });
    const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
    const o = out(); await hook(childInput(), o);
    return { text: parentPart(o)[0]?.text ?? "", client };
  };
  // A: the synthetic message is itself outside the page
  let r = await drive(mk(100, 260));
  check(r.text.includes("> [prompt] PRE-COMPACTION-PROMPT") && r.text.split("\n")[1].startsWith("> [prompt] PRE-COMPACTION-PROMPT"), "N3b: compaction, synthetic parent outside the page: anchored on the newest real prompt, right after the header");
  check(!r.text.includes("Continue if you have next steps") && !r.text.includes("(no operator prompt"), "N3b: the synthetic message is never the anchor");
  check(r.client.calls.messages.some((c) => c.query?.limit === 1000) && r.client.calls.messages.length === 2, "N3b: exactly one wide fetch (limit 1000)");
  check(r.text.includes("step 359") && r.text.includes("or more earlier entries omitted)"), "N3b: the newest steps are kept and the omission line says the read was cut");
  // B: the synthetic message is inside the page, the real prompt is not
  r = await drive(mk(230, 20));
  check(r.text.split("\n")[1].startsWith("> [prompt] PRE-COMPACTION-PROMPT"), "N3b: compaction, synthetic parent inside the page, prompt outside: anchored on the newest real prompt");
  // C: the wide fetch fails (or the store has no real prompt): the note, fail-open
  r = await drive(mk(100, 260), { failWide: true });
  check(r.text.includes("(no operator prompt in the recent window)") && r.text.includes("step 359"), "N3b: a failing wide fetch: the no-prompt note, the block still built");
  const noPrompt = mk(100, 260).slice(1);
  r = await drive(noPrompt);
  check(r.text.includes("(no operator prompt in the recent window)") && !r.text.includes("Continue if"), "N3b: no real prompt anywhere: the no-prompt note");
  // D: the message parentID points to is gone
  const gone = mk(0, 260).filter((m) => m.info.id !== "msg_cont");
  r = await drive(gone);
  check(r.text.split("\n")[1].startsWith("> [prompt] PRE-COMPACTION-PROMPT"), "N3b: parentID names a message that no longer exists: anchored on the newest real prompt");
}

// --- A5: 'or more earlier entries omitted' only when the page was cut ----------------------------------------------------
{
  const mkN = (count) => {
    const all = [{ info: { id: "msg_open", role: "user", time: { created: T }, model: { providerID: "prov", modelID: "m" } }, parts: [{ type: "text", text: "A5-PROMPT" }] }];
    for (let i = 1; i < count; i++) all.push({ info: { id: `msg_s${i}`, role: "assistant", parentID: "msg_open", time: { created: T + i * 1000 }, providerID: "prov", modelID: "m" },
      parts: [{ type: "text", text: `a5 step ${i} ${"z".repeat(120)}` }] });
    return all;
  };
  for (const [count, cut] of [[199, false], [200, false], [201, true], [450, true]]) {
    const all = mkN(count);
    const client = mkClient({ sessions: sessions(), messages: { ses_parent: async (limit) => (limit ? all.slice(-limit) : all) } });
    const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
    const o = out(); await hook(childInput(), o);
    const text = parentPart(o)[0]?.text ?? "";
    check(text.includes("earlier entries omitted)"), `A5: ${count} messages: the omission line is there (the budget cuts entries)`);
    check(text.includes("or more earlier entries omitted)") === cut, `A5: ${count} messages: "or more" is ${cut ? "" : "not "}said`);
    check(cut || text.split("\n")[1].startsWith("> [prompt] A5-PROMPT"), `A5: ${count} messages: the anchor is the prompt of the page`);
  }
}

// --- A1: the block fits every accepted MAX_CHARS ---------------------------------------------------------------------------
{
  const long = parentMsgs().map((m) => m.info.role === "user" ? { ...m, parts: [{ type: "text", text: "ANCHOR-A1 " + "long anchor words ".repeat(200) }] } : m);
  for (const mc of ["600", "700", "1000", "3000", "8000"]) {
    process.env.MEMOIRE_SUBAGENT_MAX_CHARS = mc;
    const client = mkClient({ sessions: sessions(), messages: { ses_parent: long } });
    const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
    const o = out(); await hook(childInput(), o);
    const text = parentPart(o)[0]?.text ?? "";
    check(Array.from(text).length <= Number(mc), `A1: MAX_CHARS=${mc}: the block fits (${Array.from(text).length})`);
    check(text.split("\n")[0].startsWith("# [memoire] parent context") && text.split("\n").at(-1).startsWith("Parent session ses_parent"), `A1: MAX_CHARS=${mc}: header first, pointer last`);
  }
  process.env.MEMOIRE_SUBAGENT_MAX_CHARS = "600";
  {
    const client = mkClient({ sessions: sessions(), messages: { ses_parent: long } });
    const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
    const o = out(); await hook(childInput(), o);
    const l = (parentPart(o)[0]?.text ?? "").split("\n").find((x) => x.startsWith("> [prompt] ANCHOR-A1")) ?? "";
    check(l.endsWith("…") && Array.from(l).length >= 120, "A1: MAX_CHARS=600: the anchor is cut with an ellipsis, at least 120 characters");
  }
  delete process.env.MEMOIRE_SUBAGENT_MAX_CHARS;
}

// --- A2: the log fields are stripped of control characters and bounded ----------------------------------------------
{
  const sid = "ses_x\nFORGED status=injected\u001b[31m\u0085\u2028" + "y".repeat(300);
  const sess = sessions(); sess[sid] = { id: sid, parentID: "ses_parent", directory: dirOk };
  const client = mkClient({ sessions: sess, messages: { ses_parent: parentMsgs() } });
  const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
  const before = readLog().split("\n").filter(Boolean).length;
  const o = out(); await hook({ sessionID: sid, messageID: "msg_1", model: { providerID: "prov", modelID: "m" } }, o);
  const lines = readLog().split("\n").filter(Boolean);
  check(lines.length === before + 1, "A2: one new log line, whatever the session id carries");
  const l = lines.at(-1);
  check(!/[\u0000-\u0008\u000b-\u001f\u007f-\u009f\u2028\u2029]/.test(l), "A2: no control character in the log line");
  const f = /session=(.*?) parent=/.exec(l)?.[1] ?? "";
  check(f.length > 0 && Array.from(f).length <= 128 && f.startsWith("ses_xFORGED"), "A2: session is bounded to 128 characters and keeps its printable characters");
}

// --- deadline: a hung parent fetch does not hang the hook -----------------------------------------
{
  process.env.MEMOIRE_INJECT_WRAPPER_TIMEOUT = "1";
  const client = mkClient({ sessions: sessions(), messages: { ses_parent: () => new Promise(() => {}) } });
  const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
  const t0 = Date.now();
  const o = out();
  await hook(childInput(), o);
  check(Date.now() - t0 < 4500, `deadline: a hung messages call unblocks the hook (${Date.now() - t0} ms)`);
  check(o.parts.length === 1 && o.parts[0].text === "TIMELINE-BLOCK", "deadline: no parent part, timeline kept");
  delete process.env.MEMOIRE_INJECT_WRAPPER_TIMEOUT;
}

// --- B6 local time: entries carry local HH:MM:SS (T + 2 s = 01:46:42Z) ------------------------------------------------
setEnvFile("MEMOIRE_SUBAGENT_CONTEXT=on\n");
for (const [tz, want] of [["UTC", "- 01:46:42 tool bash"], ["UTC-2", "- 03:46:42 tool bash"]]) {
  const saved = process.env.TZ;
  process.env.TZ = tz;
  const client = mkClient({ sessions: sessions(), messages: { ses_parent: parentMsgs() } });
  const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
  const o = out();
  await hook(childInput(), o);
  process.env.TZ = saved;
  check((parentPart(o)[0]?.text ?? "").includes(want), `local time: TZ=${tz} renders "${want}"`);
}

// --- B5: an unknown PARENT provider counts as different (withheld), whatever the child says -------------------------------
{
  const noProv = () => [
    { info: { id: "m1", role: "user", time: { created: T } }, parts: [{ type: "text", text: "NOPROV-PROMPT" }] },
    { info: { id: "m2", role: "assistant", time: { created: T + 1000 } }, parts: [{ type: "text", text: "NOPROV-ANSWER" }] },
  ];
  for (const childProv of ["prov", ""]) {
    setEnvFile("MEMOIRE_SUBAGENT_CONTEXT=on\n");
    const client = mkClient({ sessions: sessions(), messages: { ses_parent: noProv() } });
    const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
    const o = out();
    await hook(childInput(childProv), o);
    const t = parentPart(o)[0]?.text ?? "";
    check(t.startsWith("Parent session ses_parent") && !t.includes("NOPROV") && t.split("\n").length === 1, `B5: unknown parent provider (child provider "${childProv}"): pointer only`);
    check(lastLog().includes("xprovider=withheld"), `B5: unknown parent provider (child "${childProv}") logged as withheld`);
  }
  setEnvFile("MEMOIRE_SUBAGENT_CONTEXT=on\nMEMOIRE_SUBAGENT_CONTEXT_XPROVIDER=on\n");
  const client = mkClient({ sessions: sessions(), messages: { ses_parent: noProv() } });
  const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
  const o = out();
  await hook(childInput("prov"), o);
  check((parentPart(o)[0]?.text ?? "").includes("NOPROV-ANSWER"), "B5: the XPROVIDER opt-in releases an unknown-provider parent");
  setEnvFile("MEMOIRE_SUBAGENT_CONTEXT=on\n");
}

// --- F11: ONE deadline for the timeline injection and the parent-context build (they run concurrently) --------------
{
  process.env.MEMOIRE_INJECT_WRAPPER_TIMEOUT = "1";
  const hangShell = () => { const p = new Promise(() => {}); p.cwd = () => p; p.nothrow = () => p; p.quiet = () => p; return p; };
  const client = mkClient({ sessions: sessions(), messages: { ses_parent: () => new Promise(() => {}) } });
  const { MemoirePlugin } = await import(pathToFileURL(pluginPath).href + `?v=${Math.random()}`);
  const hook = (await MemoirePlugin({ $: hangShell, directory: "/tmp/proj", client }))["chat.message"];
  const t0 = Date.now();
  const o = out();
  await hook(childInput(), o);
  const ms = Date.now() - t0;
  // timeline ceiling = deadline + 2 s = 3 s; a sequential build would add its own full 1 s deadline
  check(ms < 3500, `F11: a hung timeline and a hung parent fetch share one deadline (${ms} ms, sequential would be ~4000)`);
  delete process.env.MEMOIRE_INJECT_WRAPPER_TIMEOUT;
}

// --- fail-open: a client that throws --------------------------------------------------------------------
{
  const client = { session: { get: async () => { throw new Error("boom"); }, messages: async () => { throw new Error("boom"); } } };
  const hook = await makeHook({ client, timeline: "TIMELINE-BLOCK" });
  const o = out();
  await hook(childInput(), o);
  check(o.parts.length === 1 && o.parts[0].text === "TIMELINE-BLOCK", "fail-open: a throwing client never blocks the message");
  check(lastLog().includes("status=error"), "fail-open: logged");
}

if (failures > 0) {
  console.error(`subagent-context-plugin-test: ${failures} failure(s)`);
  process.exit(1);
}
console.log("subagent-context-plugin-test: OK");
process.exit(0);
