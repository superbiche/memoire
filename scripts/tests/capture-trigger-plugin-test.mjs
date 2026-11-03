// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// capture-trigger-plugin-test.mjs — the `event` hook of the OpenCode plugin
// (session.idle -> scripts/memoire-trigger-capture-opencode.sh, detached, never
// awaited). Plain node, a fake wrapper script, a scratch MEMOIRE_HOME: no
// opencode, no Bun, no network.

import path from "node:path";
import fs from "node:fs";
import os from "node:os";
import { pathToFileURL } from "node:url";

const here = path.dirname(new URL(import.meta.url).pathname);
const pluginPath = process.env.MEMOIRE_PLUGIN_PATH ? path.resolve(process.env.MEMOIRE_PLUGIN_PATH) : path.resolve(here, "../opencode-plugin/memoire.js");
let failures = 0;
const check = (cond, label) => { if (!cond) { failures += 1; console.error(`FAIL: ${label}`); } };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const root = fs.mkdtempSync(path.join(process.env.TMPDIR || os.tmpdir(), "capture-trigger-plugin."));
process.on("exit", () => { try { fs.rmSync(root, { recursive: true, force: true }); } catch { /* */ } });
const home = path.join(root, "mh"); fs.mkdirSync(home, { recursive: true });
process.env.MEMOIRE_HOME = home;
process.env.HOME = path.join(root, "home");
process.env.MEMOIRE_SUBAGENT_LOG = path.join(home, "hooks", "subagent.log");
const envFile = path.join(home, "capture-tick.env");
const calls = path.join(root, "calls.log");
const fake = path.join(root, "wrapper.sh");
// The fake wrapper: records its argument, sleeps (so a host that awaited it would be slow), then leaves a marker.
fs.writeFileSync(fake, `#!/usr/bin/env bash\nprintf '%s\\n' "$1" >> "${calls}"\nsleep "\${FAKE_SLEEP:-0}"\n: > "${root}/done-$1"\n`, { mode: 0o755 });
process.env.MEMOIRE_TRIGGER_OPENCODE_SCRIPT = fake;

const { MemoirePlugin } = await import(pathToFileURL(pluginPath).href);
const hooks = await MemoirePlugin({ $: () => { throw new Error("no shell"); }, directory: root, client: {} });
const idle = (sid) => ({ event: { type: "session.idle", properties: { sessionID: sid } } });
const callsOf = () => (fs.existsSync(calls) ? fs.readFileSync(calls, "utf8").split("\n").filter(Boolean) : []);

check(typeof hooks.event === "function", "the plugin has an event hook");
check(typeof hooks["chat.message"] === "function", "the chat.message hook is still there");

// switch absent / off: no spawn
await hooks.event(idle("ses_a")); await sleep(300);
check(callsOf().length === 0, "no switch -> no spawn");
fs.writeFileSync(envFile, "MEMOIRE_HOOK_CAPTURE=off\n");
await hooks.event(idle("ses_a")); await sleep(300);
check(callsOf().length === 0, "switch off -> no spawn");

// switch on: spawned, hook returns at once, child outlives the hook's return
fs.writeFileSync(envFile, "MEMOIRE_HOOK_CAPTURE=on\n");
process.env.FAKE_SLEEP = "1";
const t0 = Date.now();
await hooks.event(idle("ses_b"));
const dt = Date.now() - t0;
check(dt < 500, `the event hook returns at once while the wrapper sleeps 1 s (${dt} ms)`);
await sleep(400);
check(callsOf().includes("ses_b"), "session.idle -> wrapper spawned with the session id");
check(!fs.existsSync(path.join(root, "done-ses_b")), "the wrapper was not awaited (still running)");
await sleep(1200);
check(fs.existsSync(path.join(root, "done-ses_b")), "the wrapper completes after the hook returned");
delete process.env.FAKE_SLEEP;

// other events, bad ids: nothing
const before = callsOf().length;
await hooks.event({ event: { type: "session.updated", properties: { sessionID: "ses_c" } } });
await hooks.event({ event: { type: "message.updated", properties: {} } });
await hooks.event(idle("bad id/../x"));
await hooks.event(idle(undefined));
await hooks.event({});
await hooks.event({ event: null });
await sleep(300);
check(callsOf().length === before, "non-idle events and invalid session ids spawn nothing");

// spawn failure: no throw, one line in the plugin's log
process.env.MEMOIRE_TRIGGER_OPENCODE_SCRIPT = path.join(root, "missing.sh");
const { MemoirePlugin: P2 } = await import(pathToFileURL(pluginPath).href + "?2");
const hooks2 = await P2({ $: () => { throw new Error("no shell"); }, directory: root, client: {} });
let threw = false;
try { await hooks2.event(idle("ses_d")); } catch { threw = true; }
await sleep(300);
check(!threw, "a failed spawn never throws into the host");
const log = fs.existsSync(process.env.MEMOIRE_SUBAGENT_LOG) ? fs.readFileSync(process.env.MEMOIRE_SUBAGENT_LOG, "utf8") : "";
check(/memoire-trigger-opencode-plugin: status=error=spawn session=ses_d code=ENOENT/.test(log), "a failed spawn is logged to the plugin's log");
check(!/forty-two|content/.test(log), "no content in the log");

if (failures) { console.error(`capture-trigger-plugin-test: ${failures} FAILED`); process.exit(1); }
console.log("capture-trigger-plugin-test: OK");
