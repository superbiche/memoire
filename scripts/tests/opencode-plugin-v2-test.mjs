// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// opencode-plugin-v2-test.mjs — the OpenCode v2 face of the plugin (default
// export { id, setup(ctx) }). Plain node, a fake v2 host context (session.hook,
// session.get, session.context, event.subscribe, location), a fake injector
// script and a fake trigger wrapper under a scratch MEMOIRE_HOME: no opencode,
// no network, no live state. Covers: the export shape (v1 named export kept);
// the timeline block computed once per session at its first prompt and pushed
// to the system parts of every dispatch; a failing, empty or hanging injector
// pushes nothing and never leaves the prompt hook pending; the parent block of
// a child session (opt-in switch, same-provider rule, pointer when withheld);
// the idle events -> the detached trigger; fail-open on a host whose hooks
// throw.

import path from "node:path";
import fs from "node:fs";
import os from "node:os";
import { pathToFileURL } from "node:url";

const here = path.dirname(new URL(import.meta.url).pathname);
const pluginPath = process.env.MEMOIRE_PLUGIN_PATH ? path.resolve(process.env.MEMOIRE_PLUGIN_PATH) : path.resolve(here, "../opencode-plugin/memoire.js");
let failures = 0;
const check = (cond, label) => { if (!cond) { failures += 1; console.error(`FAIL: ${label}`); } };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const root = fs.mkdtempSync(path.join(process.env.TMPDIR || os.tmpdir(), "opencode-plugin-v2."));
process.on("exit", () => { try { fs.rmSync(root, { recursive: true, force: true }); } catch { /* */ } });
const home = path.join(root, "mh"); fs.mkdirSync(path.join(home, "hooks"), { recursive: true });
process.env.MEMOIRE_HOME = home;
process.env.HOME = path.join(root, "home");
process.env.MEMOIRE_SUBAGENT_LOG = path.join(home, "hooks", "subagent.log");
const envFile = path.join(home, "capture-tick.env");

// The fake injector: records the hook JSON it receives, prints a block (or
// fails, or hangs) by the mode in a file.
const injectorCalls = path.join(root, "injector.log");
const injectorMode = path.join(root, "injector.mode");
const injector = path.join(root, "inject.sh");
fs.writeFileSync(injector, `#!/usr/bin/env bash
cat >> "${injectorCalls}"; printf '\\n' >> "${injectorCalls}"
mode="$(cat "${injectorMode}" 2>/dev/null)"
case "$mode" in
  fail) echo garbage; exit 1 ;;
  empty) exit 0 ;;
  hang) sleep 30 ;;
  *) printf '# [memoire] recent context\\n- obs é日本\\n' ;;
esac
`, { mode: 0o755 });
process.env.MEMOIRE_INJECT_SCRIPT = injector;
// The resolver: capture on everywhere, project = basename.
const resolver = path.join(root, "resolve.sh");
fs.writeFileSync(resolver, `#!/usr/bin/env bash
if [[ "$1" == "--capture-enabled" ]]; then echo on; exit 0; fi
basename "$1"
`, { mode: 0o755 });
process.env.MEMOIRE_RESOLVE_PROJECT = resolver;
// The fake trigger wrapper.
const triggerCalls = path.join(root, "trigger.log");
const trigger = path.join(root, "trigger.sh");
fs.writeFileSync(trigger, `#!/usr/bin/env bash\nprintf '%s\\n' "$1" >> "${triggerCalls}"\n`, { mode: 0o755 });
process.env.MEMOIRE_TRIGGER_OPENCODE_SCRIPT = trigger;
const injectorCount = () => (fs.existsSync(injectorCalls) ? fs.readFileSync(injectorCalls, "utf8").split("\n").filter(Boolean).length : 0);
const triggerCallsOf = () => (fs.existsSync(triggerCalls) ? fs.readFileSync(triggerCalls, "utf8").split("\n").filter(Boolean) : []);

// The session directory must exist: the injector runs with it as cwd.
const projDir = path.join(root, "proj-v2"); fs.mkdirSync(projDir, { recursive: true });
// A fake v2 host context. Sessions: a map id -> Info; contexts: id -> messages.
function makeCtx({ sessions = {}, contexts = {}, events = [], throwOnHook = false } = {}) {
  const hooks = {};
  let release;
  const released = new Promise((r) => { release = r; });
  const ctx = {
    location: { directory: projDir },
    session: {
      hook: async (name, fn) => { if (throwOnHook) throw new Error("no hooks here"); hooks[name] = fn; },
      get: async ({ sessionID }) => sessions[sessionID],
      context: async ({ sessionID }) => contexts[sessionID],
    },
    event: {
      subscribe: ({ signal }) => ({
        async *[Symbol.asyncIterator]() {
          for (const e of events) yield e;
          await Promise.race([released, new Promise((r) => signal?.addEventListener("abort", r, { once: true }))]);
        },
      }),
    },
    hooks,
    release: () => release(),
  };
  return ctx;
}
const dispatch = (sid, provider = "conductor") => ({ sessionID: sid, model: { providerID: provider, id: "coder" }, system: [], messages: [], options: {} });

const mod = await import(pathToFileURL(pluginPath).href);
check(typeof mod.default === "object" && typeof mod.default.id === "string" && mod.default.id.length > 0, "default export carries an id");
check(typeof mod.default.setup === "function", "default export carries setup(ctx)");
check(typeof mod.MemoirePlugin === "function", "the v1 named export is still there");
check(mod.default.server === mod.MemoirePlugin, "the v1 face rides the default export as `server` (rtk.js pattern)");

// --- timeline: once per session, pushed to the system parts of every dispatch -----
{
  const ctx = makeCtx();
  const cleanup = await mod.default.setup(ctx);
  check(typeof ctx.hooks.prompt === "function" && typeof ctx.hooks.context === "function", "setup registers the prompt and context hooks");
  await ctx.hooks.prompt({ sessionID: "ses_a", messageID: "msg_1", prompt: { text: "hi" }, delivery: "queued" });
  const d1 = dispatch("ses_a");
  ctx.hooks.context(d1);
  check(d1.system.length === 1 && d1.system[0].type === "text" && d1.system[0].text.includes("# [memoire] recent context") && d1.system[0].text.includes("obs é日本"), `the timeline block is a system part of the dispatch (${JSON.stringify(d1.system).slice(0, 120)})`);
  const hookJson = fs.readFileSync(injectorCalls, "utf8").trim().split("\n")[0];
  check(hookJson === JSON.stringify({ cwd: projDir }), `the injector receives {"cwd": location.directory} on stdin (${hookJson})`);
  await ctx.hooks.prompt({ sessionID: "ses_a", messageID: "msg_2", prompt: { text: "again" }, delivery: "queued" });
  check(injectorCount() === 1, "the injector runs once per session");
  const d2 = dispatch("ses_a");
  ctx.hooks.context(d2);
  check(d2.system.length === 1, "every later dispatch of the session gets the cached block");
  const other = dispatch("ses_other");
  ctx.hooks.context(other);
  check(other.system.length === 0, "a session that never prompted gets nothing");
  await ctx.hooks.prompt({ sessionID: "ses_b", messageID: "msg_3", prompt: { text: "x" }, delivery: "queued" });
  check(injectorCount() === 2, "another session runs the injector again");
  if (typeof cleanup === "function") cleanup();
}

// --- injector failures: nothing pushed, the hook never hangs -------------------------
{
  for (const mode of ["fail", "empty"]) {
    fs.writeFileSync(injectorMode, mode);
    const ctx = makeCtx();
    const cleanup = await mod.default.setup(ctx);
    await ctx.hooks.prompt({ sessionID: `ses_${mode}`, messageID: "m", prompt: { text: "x" }, delivery: "queued" });
    const d = dispatch(`ses_${mode}`);
    ctx.hooks.context(d);
    check(d.system.length === 0, `an injector that ${mode === "fail" ? "exits non-zero" : "prints nothing"} pushes nothing`);
    if (typeof cleanup === "function") cleanup();
  }
  fs.writeFileSync(injectorMode, "hang");
  process.env.MEMOIRE_INJECT_WRAPPER_TIMEOUT = "1";
  const ctx = makeCtx();
  const cleanup = await mod.default.setup(ctx);
  const t0 = Date.now();
  await ctx.hooks.prompt({ sessionID: "ses_hang", messageID: "m", prompt: { text: "x" }, delivery: "queued" });
  const dt = Date.now() - t0;
  check(dt < 3500, `a hanging injector is cut at the deadline (${dt} ms)`);
  const d = dispatch("ses_hang");
  ctx.hooks.context(d);
  check(d.system.length === 0, "a hanging injector pushes nothing");
  delete process.env.MEMOIRE_INJECT_WRAPPER_TIMEOUT;
  fs.writeFileSync(injectorMode, "ok");
  if (typeof cleanup === "function") cleanup();
}

// --- parent context of a child session (opt-in) ----------------------------------
{
  const T = 1700000000000;
  const parentMessages = [
    { id: "msg_p1", type: "user", text: "Fix the parser — répare", files: [], time: { created: T } },
    { id: "msg_p2", type: "assistant", model: { providerID: "conductor", id: "coder" }, content: [
      { type: "reasoning", text: "thinking" },
      { type: "tool", id: "c1", name: "shell", state: { status: "completed", input: { command: "ls" }, content: [{ type: "text", text: "a.go\nb.go" }] }, time: { created: T + 1000 } },
      { type: "text", text: "Two files — deux" },
    ], time: { created: T + 500, completed: T + 2000 } },
  ];
  const sessions = {
    ses_child: { id: "ses_child", parentID: "ses_parent", location: { directory: projDir } },
    ses_parent: { id: "ses_parent", location: { directory: projDir } },
    ses_root: { id: "ses_root", location: { directory: projDir } },
  };
  const contexts = { ses_parent: parentMessages };
  // switch off: nothing but the timeline
  fs.writeFileSync(envFile, "MEMOIRE_SUBAGENT_CONTEXT=off\n");
  let ctx = makeCtx({ sessions, contexts });
  let cleanup = await mod.default.setup(ctx);
  await ctx.hooks.prompt({ sessionID: "ses_child", messageID: "m", prompt: { text: "go" }, delivery: "queued" });
  let d = dispatch("ses_child");
  ctx.hooks.context(d);
  check(d.system.length === 1 && d.system[0].text.includes("recent context"), "switch off: the timeline only");
  if (typeof cleanup === "function") cleanup();
  // switch on, same provider: the block
  fs.writeFileSync(envFile, "MEMOIRE_SUBAGENT_CONTEXT=on\n");
  ctx = makeCtx({ sessions, contexts });
  cleanup = await mod.default.setup(ctx);
  await ctx.hooks.prompt({ sessionID: "ses_child", messageID: "m", prompt: { text: "go" }, delivery: "queued" });
  d = dispatch("ses_child", "conductor");
  ctx.hooks.context(d);
  const block = d.system.map((p) => p.text).find((t) => t.includes("[memoire] parent context")) ?? "";
  check(d.system.length === 2, `switch on: the timeline and the parent block (${d.system.length} parts)`);
  check(block.includes("> [prompt] Fix the parser — répare"), "the parent's prompt is the anchor line");
  check(block.includes("tool shell command=ls → ok a.go b.go"), `the parent's tool call is rendered (${block.split("\n").find((l) => l.includes("tool")) ?? "-"})`);
  check(block.includes("text: Two files — deux"), "the parent's text is rendered");
  check(block.includes("Parent session ses_parent (project proj-v2)"), "the pointer line names the parent and the project");
  // a different provider: the pointer only, unless XPROVIDER
  d = dispatch("ses_child", "anthropic");
  ctx.hooks.context(d);
  const parts = d.system.map((p) => p.text);
  check(parts.length === 2 && parts[1].startsWith("Parent session ses_parent") && !parts[1].includes("[prompt]"), "another provider gets the pointer only");
  fs.writeFileSync(envFile, "MEMOIRE_SUBAGENT_CONTEXT=on\nMEMOIRE_SUBAGENT_CONTEXT_XPROVIDER=on\n");
  d = dispatch("ses_child", "anthropic");
  ctx.hooks.context(d);
  check(d.system[1].text.includes("> [prompt] Fix the parser"), "XPROVIDER on: the block crosses providers");
  // a root session: nothing from the parent lane
  await ctx.hooks.prompt({ sessionID: "ses_root", messageID: "m", prompt: { text: "go" }, delivery: "queued" });
  d = dispatch("ses_root");
  ctx.hooks.context(d);
  check(d.system.length === 1, "a session without a parent gets the timeline only");
  const log = fs.readFileSync(process.env.MEMOIRE_SUBAGENT_LOG, "utf8");
  check(/status=built session=ses_child parent=ses_parent entries=3/.test(log), `the subagent log records the build (${log.trim().split("\n").slice(-1)[0]})`);
  if (typeof cleanup === "function") cleanup();
}

// --- idle events -> the trigger, detached ---------------------------------------------
{
  fs.writeFileSync(envFile, "MEMOIRE_HOOK_CAPTURE=on\n");
  const events = [
    { type: "session.idle", data: { sessionID: "ses_i1" } },
    { type: "session.execution.succeeded", data: { sessionID: "ses_i2" } },
    { type: "session.updated", data: { sessionID: "ses_no" } },
    { type: "session.idle", data: { sessionID: "bad id/../x" } },
    { type: "session.idle" },
    null,
  ];
  const ctx = makeCtx({ events });
  const cleanup = await mod.default.setup(ctx);
  await sleep(500);
  const calls = triggerCallsOf();
  check(calls.includes("ses_i1") && calls.includes("ses_i2"), `idle and terminal execution events spawn the trigger (${calls.join(",")})`);
  check(!calls.includes("ses_no") && calls.length === 2, "other events and invalid ids spawn nothing");
  fs.writeFileSync(envFile, "MEMOIRE_HOOK_CAPTURE=off\n");
  const ctx2 = makeCtx({ events: [{ type: "session.idle", data: { sessionID: "ses_i3" } }] });
  const cleanup2 = await mod.default.setup(ctx2);
  await sleep(300);
  check(!triggerCallsOf().includes("ses_i3"), "switch off: no spawn");
  if (typeof cleanup === "function") cleanup();
  if (typeof cleanup2 === "function") cleanup2();
  ctx.release(); ctx2.release();
}

// --- fail-open: a host whose hook registration throws, a host without events -----------
{
  let threw = false;
  try { await mod.default.setup(makeCtx({ throwOnHook: true })); } catch { threw = true; }
  check(!threw, "a host whose session.hook throws never breaks setup");
  threw = false;
  try { const c = await mod.default.setup({ location: { directory: projDir }, session: { hook: async () => {} } }); if (typeof c === "function") c(); } catch { threw = true; }
  check(!threw, "a host without event.subscribe is fine (capture falls back to the tick)");
}

if (failures) { console.error(`${failures} failure(s)`); process.exit(1); }
console.log("opencode-plugin-v2-test: ok");
