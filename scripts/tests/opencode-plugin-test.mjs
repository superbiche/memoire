// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// opencode-plugin-test.mjs — hook-level regression test for the opencode
// injection plugin (ortho review 2026-07-13, MAJOR: unbounded injector).
// Runs under plain node with a fake Bun-shell `$`: no opencode, no Bun, no
// network, no live state. Covers: happy-path part shape, per-session dedupe,
// nonzero-exit suppression, empty-output suppression, and the JS deadline
// race (a hung pipeline must not leave the hook pending).

import { pathToFileURL } from "node:url";
import path from "node:path";

// MEMOIRE_PLUGIN_PATH lets the release lane run this same behavioral suite
// against the EXTRACTED tarball copy (R-10.5 round-2 finding 3).
const pluginPath = process.env.MEMOIRE_PLUGIN_PATH
  ? path.resolve(process.env.MEMOIRE_PLUGIN_PATH)
  : path.resolve(
      path.dirname(new URL(import.meta.url).pathname),
      "../opencode-plugin/memoire.js",
    );

let failures = 0;
function check(cond, label) {
  if (!cond) {
    failures += 1;
    console.error(`FAIL: ${label}`);
  }
}

// Fake Bun shell: tagged template returning a chainable thenable. `behavior`
// receives the interpolated expressions and returns a promise (or a
// never-resolving one for the hang case).
function makeShell(behavior) {
  return (strings, ...exprs) => {
    const p = behavior(exprs);
    p.cwd = () => p;
    p.nothrow = () => p;
    p.quiet = () => p;
    return p;
  };
}

const ok = (text) => Promise.resolve({ exitCode: 0, text: () => text });
const failExit = () => Promise.resolve({ exitCode: 1, text: () => "garbage" });
const hang = () => new Promise(() => {});

async function makeHook(behavior) {
  const { MemoirePlugin } = await import(pathToFileURL(pluginPath).href + `?v=${Math.random()}`);
  const hooks = await MemoirePlugin({ $: makeShell(behavior), directory: "/tmp/proj" });
  return hooks["chat.message"];
}

function freshOutput() {
  return { message: { id: "msg_1", sessionID: "ses_1" }, parts: [] };
}

// --- happy path: full TextPart pushed -----------------------------------------
{
  const hook = await makeHook(() => ok("# [memoire] recent context\n- obs"));
  const out = freshOutput();
  await hook({ sessionID: "ses_1", messageID: "msg_1" }, out);
  check(out.parts.length === 1, "happy path must push exactly one part");
  const part = out.parts[0] ?? {};
  check(part.type === "text" && part.synthetic === true, "part must be synthetic text");
  check(typeof part.id === "string" && part.id.startsWith("prt_memoire_"), "part id must carry the prt_memoire_ prefix");
  check(part.sessionID === "ses_1" && part.messageID === "msg_1", "part must carry sessionID and messageID");
  check(part.text.includes("# [memoire] recent context"), "part text must carry the injected block");

  // --- dedupe: same session never injects twice -------------------------------
  const out2 = freshOutput();
  await hook({ sessionID: "ses_1", messageID: "msg_2" }, out2);
  check(out2.parts.length === 0, "second message in the same session must not inject");
}

// --- nonzero exit: no injection ------------------------------------------------
{
  const hook = await makeHook(failExit);
  const out = freshOutput();
  await hook({ sessionID: "ses_err", messageID: "msg_1" }, out);
  check(out.parts.length === 0, "nonzero injector exit must not inject");
}

// --- empty output: no injection ------------------------------------------------
{
  const hook = await makeHook(() => ok("  \n "));
  const out = freshOutput();
  await hook({ sessionID: "ses_empty", messageID: "msg_1" }, out);
  check(out.parts.length === 0, "whitespace-only injector output must not inject");
}

// --- hang: the JS race must unblock the hook within deadline+2s ----------------
{
  process.env.MEMOIRE_INJECT_WRAPPER_TIMEOUT = "1";
  const hook = await makeHook(hang);
  const started = Date.now();
  const out = freshOutput();
  await hook({ sessionID: "ses_hang", messageID: "msg_1" }, out);
  const elapsed = Date.now() - started;
  check(elapsed < 5000, `hung injector must unblock within deadline+2s (took ${elapsed}ms)`);
  check(out.parts.length === 0, "hung injector must not inject");
  delete process.env.MEMOIRE_INJECT_WRAPPER_TIMEOUT;
}

// --- junk deadline env falls back, happy path still works ----------------------
{
  process.env.MEMOIRE_INJECT_WRAPPER_TIMEOUT = "not-a-number";
  const hook = await makeHook(() => ok("block"));
  const out = freshOutput();
  await hook({ sessionID: "ses_junk", messageID: "msg_1" }, out);
  check(out.parts.length === 1, "junk deadline env must fall back to the default, not break injection");
  delete process.env.MEMOIRE_INJECT_WRAPPER_TIMEOUT;
}

// --- MEMOIRE_INJECT_SCRIPT override is the EXECUTED script (R-10.5) -----------
// Behavioral pin, not a source grep: the fake shell captures the interpolated
// expressions, so the assertion fails the moment the override expression stops
// feeding the command.
{
  process.env.MEMOIRE_INJECT_SCRIPT = "/custom/layout/memoire-inject-claude.sh";
  let seen = [];
  const hook = await makeHook((exprs) => {
    seen = exprs;
    return ok("# [memoire] recent context\n- obs");
  });
  const out = freshOutput();
  await hook({ sessionID: "ses_override", messageID: "msg_1" }, out);
  check(out.parts.length === 1, "override run must still inject");
  check(
    seen.some((e) => e === "/custom/layout/memoire-inject-claude.sh"),
    "MEMOIRE_INJECT_SCRIPT must be the executed script, got: " + JSON.stringify(seen),
  );
  delete process.env.MEMOIRE_INJECT_SCRIPT;
}

// --- without the override, the fleet default path is executed -----------------
{
  let seen = [];
  const hook = await makeHook((exprs) => {
    seen = exprs;
    return ok("block");
  });
  const out = freshOutput();
  await hook({ sessionID: "ses_default", messageID: "msg_1" }, out);
  check(
    seen.some((e) => typeof e === "string" && e.endsWith("/dev/personal/memoire/scripts/memoire-inject-claude.sh")),
    "default script path must be the fleet checkout inject script",
  );
}

// --- real injector behind the plugin: off by default (MEMOIRE_INJECT) ---------
// A real shell runs the real memoire-inject-claude.sh with a stub curl that
// records every call and a FIFO as the token file. Unset: no prt_memoire_
// part, no call, no token read. MEMOIRE_INJECT=on: the part is back.
{
  const { spawn, spawnSync } = await import("node:child_process");
  const fs = await import("node:fs");
  const os = await import("node:os");
  const here = path.dirname(new URL(import.meta.url).pathname);
  const realScript = path.resolve(here, "../memoire-inject-claude.sh");
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "inject-off-plugin-"));
  const home = path.join(tmp, "home");
  fs.mkdirSync(path.join(tmp, "bin"), { recursive: true });
  fs.mkdirSync(home, { recursive: true });
  const log = path.join(tmp, "curl.log");
  fs.writeFileSync(log, "");
  fs.writeFileSync(
    path.join(tmp, "bin", "curl"),
    `#!/usr/bin/env bash\nprintf '%s\\n' "$*" >> "${log}"\n` +
      `echo '{"hits":[{"id":7,"kind":"fact","captured_at_epoch":1700000000,"snippet":"hello"}]}'\n`,
    { mode: 0o755 },
  );
  const quote = (v) => `'${String(v).replace(/'/g, `'\\''`)}'`;
  // Real shell with the Bun-shell chain the plugin uses (cwd/nothrow/quiet/text).
  let lastErr = "";
  const realShell = (strings, ...exprs) => {
    const cmd = strings.reduce((acc, str, i) => acc + str + (i < exprs.length ? quote(exprs[i]) : ""), "");
    let dir;
    const api = {
      cwd(d) { dir = d; return api; },
      nothrow() { return api; },
      quiet() { return api; },
      then(res, rej) {
        return new Promise((resolve) => {
          const child = spawn("bash", ["-c", cmd], { cwd: dir, env: process.env, stdio: ["ignore", "pipe", "pipe"] });
          let out = "";
          lastErr = "";
          child.stdout.on("data", (c) => { out += c; });
          child.stderr.on("data", (c) => { lastErr += c; });
          child.on("close", (code) => resolve({ exitCode: code, text: () => out }));
        }).then(res, rej);
      },
    };
    return api;
  };
  const saved = { ...process.env };
  const setEnv = (extra) => {
    for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
    Object.assign(process.env, saved, {
      PATH: `${path.join(tmp, "bin")}:${saved.PATH}`,
      MEMOIRE_HOME: home,
      MEMOIRE_BASE_URL: "http://127.0.0.1:9",
      MEMOIRE_PROJECT: "off-proj",
      MEMOIRE_INJECT_SCRIPT: realScript,
      MEMOIRE_INJECT_WRAPPER_TIMEOUT: "5",
    }, extra);
    delete process.env.MEMOIRE_INJECT;
    if (extra?.MEMOIRE_INJECT !== undefined) process.env.MEMOIRE_INJECT = extra.MEMOIRE_INJECT;
  };
  const realHook = async () => {
    const { MemoirePlugin } = await import(pathToFileURL(pluginPath).href + `?v=${Math.random()}`);
    return (await MemoirePlugin({ $: realShell, directory: tmp }))["chat.message"];
  };

  // Token = FIFO without a writer: a read would block until the 5s deadline.
  spawnSync("mkfifo", [path.join(home, "token")]);
  setEnv({});
  let hook = await realHook();
  let out = freshOutput();
  let t0 = Date.now();
  await hook({ sessionID: "ses_off", messageID: "msg_1" }, out);
  check(out.parts.filter((x) => x.id.startsWith("prt_memoire_")).length === 0, "default (MEMOIRE_INJECT unset): no prt_memoire_ part");
  check(out.parts.length === 0, "default: nothing pushed at all");
  check(fs.readFileSync(log, "utf8") === "", "default: curl must not be called");
  check(Date.now() - t0 < 4000, "default: the injector must not wait on the token");
  check(lastErr === "", `default: the injector prints nothing on stderr: ${lastErr}`);

  setEnv({ MEMOIRE_INJECT: "off" });
  hook = await realHook();
  out = freshOutput();
  await hook({ sessionID: "ses_off2", messageID: "msg_1" }, out);
  check(out.parts.length === 0, "MEMOIRE_INJECT=off: nothing pushed");
  check(fs.readFileSync(log, "utf8") === "", "MEMOIRE_INJECT=off: curl must not be called");
  check(lastErr === "", `MEMOIRE_INJECT=off: the injector prints nothing on stderr: ${lastErr}`);

  fs.rmSync(path.join(home, "token"));
  fs.writeFileSync(path.join(home, "token"), "tok\n");
  setEnv({ MEMOIRE_INJECT: "on" });
  hook = await realHook();
  out = freshOutput();
  await hook({ sessionID: "ses_on", messageID: "msg_1" }, out);
  check(out.parts.length === 1 && out.parts[0].id.startsWith("prt_memoire_"), "MEMOIRE_INJECT=on: one prt_memoire_ part");
  check(out.parts[0]?.text?.includes("recent context"), "MEMOIRE_INJECT=on: the part carries the block");
  check(fs.readFileSync(log, "utf8") !== "", "MEMOIRE_INJECT=on: curl is called");

  for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
  Object.assign(process.env, saved);
  fs.rmSync(tmp, { recursive: true, force: true });
}

if (failures > 0) {
  console.error(`opencode-plugin-test: ${failures} failure(s)`);
  process.exit(1);
}
console.log("opencode-plugin-test: OK");
process.exit(0);
