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
// 2026-07-13, MAJOR). Stdout is injected only on a clean exit. Session-start
// injection is off by default (MEMOIRE_INJECT): the injector then prints
// nothing, so the timeline part is pushed only when the injector prints
// something. The sub-agent parent-context part below is independent of it.
//
// Child sessions (opt-in, MEMOIRE_SUBAGENT_CONTEXT=on in
// ~/.memoire/capture-tick.env): the timeline part above is unchanged and a
// second synthetic part carries the PARENT session's recent activity, built
// by the same selection rule as scripts/memoire-subagent-select.jq (bash side;
// scripts/tests/subagent-context-test.sh pins the two to the same block
// structure). The block goes to the child's model; when that is another
// provider than the parent's, only the pointer line is sent unless
// MEMOIRE_SUBAGENT_CONTEXT_XPROVIDER is on. Fail-open, bounded by the same
// injector deadline.
//
// Capture trigger (opt-in, MEMOIRE_HOOK_CAPTURE=on in the same file): the `event`
// hook spawns scripts/memoire-trigger-capture-opencode.sh DETACHED on
// session.idle and returns at once; it never awaits it and a failed spawn is
// logged, never raised. The wrapper re-reads the switches, applies the live-path
// rule, debounces and runs the reader on that one session.
//
// OpenCode v2 (2.0.x, fleet cutover 2026-10-06): dual export, same pattern as
// ~/.agents/rtk/opencode-plugin/rtk.js. v1 loads the named export
// `MemoirePlugin` below (unchanged); v2 loads the default export
// `{ id, setup(ctx) }` at the end of this file. In v2 there is no Bun `$` and no
// chat.message hook: the timeline and the parent-context blocks go to the
// SYSTEM parts of every provider dispatch of the session
// (ctx.session.hook("context")), computed at the session's prompts
// (ctx.session.hook("prompt")). Nothing is persisted in the store, so the
// capture reader has nothing to skip and the block survives compaction — the
// upgrade the paragraph below wished for. session.idle (and the terminal
// execution events) reach the capture trigger through ctx.event.subscribe.
//
// (v1 note) `experimental.chat.system.transform` would place the block in the
// system prompt and survive compaction, but rides an explicitly experimental
// API — the v1 export stays on chat.message.

import fs from "node:fs";
import path from "node:path";
import { execFile, spawn } from "node:child_process";

// ---- opt-in switches: the single key of ~/.memoire/capture-tick.env, last
// assignment wins, never sourced; on|1|true enables (same rule as the bash side).
const memoireHome = () => process.env.MEMOIRE_HOME || `${process.env.HOME}/.memoire`;
function switchOn(key) {
  try {
    const text = fs.readFileSync(`${memoireHome()}/capture-tick.env`, "utf8");
    // ASCII whitespace only (the bash side runs under LC_ALL=C): a BOM or a
    // no-break space is not whitespace to either parser.
    const re = new RegExp(`^[ \\t\\n\\v\\f\\r]*(export[ \\t\\n\\v\\f\\r]+)?${key}[ \\t\\n\\v\\f\\r]*=`);
    const lines = text.split("\n").filter((l) => re.test(l));
    if (!lines.length) return false;
    let v = lines[lines.length - 1].replace(/^[^=]*=/, "").replace(/#.*$/, "").replace(/\r/g, "").replace(/^[ \t\n\v\f\r]+|[ \t\n\v\f\r]+$/g, "");
    v = v.replace(/^["']/, "").replace(/["']$/, "").replace(/[A-Z]/g, (c) => c.toLowerCase());
    return v === "on" || v === "1" || v === "true";
  } catch {
    return false;
  }
}
// Same validation as the bash side (sa_int): junk falls back to the default.
const intEnv = (v, d) => (/^[1-9][0-9]{0,8}$/.test(String(v ?? "")) ? Number(v) : d);

// ---- the selection rule (mirror of memoire-subagent-select.jq) ----------------
const POINTER_HEADER = "# [memoire] parent context — what the parent session did just before this sub-agent started";
const cp = (s) => Array.from(s);
// Whitespace, C0/C1 controls (NUL, ESC, U+0085 included) and the Unicode line
// breaks U+2028/U+2029 collapse to one space: parent content cannot imitate the
// block's own lines. Every text is cut to a bounded prefix BEFORE it is
// flattened (head), exactly as the jq rule does.
const flat = (s) => String(s ?? "").replace(/[\u0000-\u0020\u007f-\u009f\u2028\u2029]+/g, " ").replace(/^ /, "").replace(/ $/, "");
// first n code points (2n+1 UTF-16 units always cover them)
const head = (s, n) => cp(String(s ?? "").slice(0, 2 * n + 1)).slice(0, n).join("");
const cap = (s, n) => { const a = cp(s); return a.length > n ? a.slice(0, n - 1).join("") + "…" : s; };
const pre = (s, n) => {
  s = String(s ?? "");
  if (s.length <= 2 * n) return s;
  const a = cp(s);
  return a.length > 2 * n ? a.slice(0, n).join("") + " " + a.slice(a.length - n).join("") : s;
};
const excerpt = (s) => {
  const a = cp(flat(pre(s, 600)));
  return a.length > 220 ? a.slice(0, 140).join("") + "…" + a.slice(a.length - 79).join("") : a.join("");
};
function entryLine(e) {
  if (e.kind === "prompt") return "> [prompt] " + cap(flat(head(e.text, 3000)), 1500);
  if (e.kind === "text") return "- " + e.ts + " text: " + cap(flat(head(e.text, 2400)), 1200);
  const name = cap(flat(head(e.name ?? "", 200)), 60);
  const inp = cap(flat(head(e.input ?? "", 400)), 200);
  const hd = [name, inp].filter((x) => x !== "").join(" ");
  return "- " + e.ts + " tool " + hd + " → " + e.status + (e.result ? " " + excerpt(e.result) : "");
}
function selectBlock(entries, windowCut, pointer, maxc = 8000, maxe = 60) {
  const n = entries.length;
  if (n === 0) return "";
  const a = entries.findIndex((e) => e.anchor === true);
  // Fit (round 3), same rule as memoire-subagent-select.jq: the anchor line is cut to the room
  // left after header, pointer, separators and the 80-character reserve, never below 120
  // characters; otherwise the block is the header and the pointer only.
  const room = maxc - cp(POINTER_HEADER).length - cp(pointer).length - 4 - 80;
  const top0 = a < 0 ? "(no operator prompt in the recent window)" : entryLine(entries[a]);
  const top0n = cp(top0);
  let top;
  if (top0n.length <= room) top = top0;
  else if (room >= 120) top = top0n.slice(0, room - 1).join("") + "…";
  else return [POINTER_HEADER, pointer].join("\n");
  const budget = maxc - cp(POINTER_HEADER).length - cp(pointer).length - cp(top).length - 4 - 80;
  const order = [];
  if (a < 0) for (let i = n - 1; i >= 0; i--) order.push(i);
  else { for (let i = n - 1; i > a; i--) order.push(i); for (let i = a - 1; i >= 0; i--) order.push(i); }
  const kept = []; // lines are rendered lazily, only for the entries the walk visits
  let used = 0;
  for (const i of order) {
    if (kept.length >= maxe) break;
    const l = entryLine(entries[i]);
    const len = cp(l).length + 1;
    if (used + len > budget) break;
    used += len; kept.push({ i, l });
  }
  kept.sort((x, y) => x.i - y.i);
  const preL = kept.filter((k) => a >= 0 && k.i < a).map((k) => k.l);
  const cur = kept.filter((k) => a < 0 || k.i > a).map((k) => k.l);
  const omit = n - (a < 0 ? 0 : 1) - kept.length;
  const out = [POINTER_HEADER, ...preL];
  if (preL.length) out.push("--- current turn ---");
  out.push(top, ...cur);
  if (omit > 0) out.push(`(+${omit} ${windowCut ? "or more " : ""}earlier entries omitted)`);
  else if (windowCut) out.push("(earlier entries not read)");
  out.push(pointer);
  return out.join("\n");
}
const pointerLine = (id, project, where) =>
  `Parent session ${id} (project ${project}). This block is the parent's recent activity only. Older context: search Memoire (memoire_search) under project ${project}, or read ${where}.`;

// ---- extractor: OpenCode messages -> normalized entries -------------------------
const hms = (ms) => {
  const d = new Date(Number(ms));
  if (!Number.isFinite(d.getTime())) return "--:--:--";
  const p = (x) => String(x).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
};
// Same as the jq side: keys in the order the store wrote them, non-strings as
// compact JSON.
const kv = (v) => v && typeof v === "object" && !Array.isArray(v)
  ? Object.keys(v).map((k) => `${k}=${typeof v[k] === "string" ? v[k] : JSON.stringify(v[k])}`).join(" ")
  : typeof v === "string" ? v : v == null ? "" : JSON.stringify(v);
const usable = (p) => p && p.type === "text" && p.synthetic !== true && p.ignored !== true && typeof p.text === "string" && p.text !== "";
function extractOpenCode(messages) {
  const ms = [...messages].sort((x, y) => (x.info?.time?.created ?? 0) - (y.info?.time?.created ?? 0));
  const entries = [];
  for (const m of ms) {
    const t0 = m.info?.time?.created ?? 0;
    const parts = m.parts ?? [];
    if (m.info?.role === "user") {
      const text = parts.filter(usable).map((p) => p.text).join("\n");
      if (text !== "") entries.push({ kind: "prompt", ts: hms(t0), t: t0, text });
    } else if (m.info?.role === "assistant") {
      for (const p of parts) {
        if (usable(p)) entries.push({ kind: "text", ts: hms(t0), t: t0, text: p.text });
        else if (p?.type === "tool") {
          const st = p.state ?? {};
          const t = st.time?.start ?? t0;
          entries.push({
            kind: "tool", ts: hms(t), t, name: p.tool ?? "", input: kv(st.input),
            status: st.status === "completed" ? "ok" : st.status === "error" ? "error" : "running",
            result: st.status === "completed" ? String(st.output ?? "") : st.status === "error" ? String(st.error ?? "") : "",
          });
        }
      }
    }
  }
  let a = -1;
  entries.forEach((e, i) => { if (e.kind === "prompt") a = i; });
  if (a >= 0) entries[a].anchor = true;
  return entries;
}
const lastAssistantProvider = (messages) => {
  const ms = [...messages].sort((x, y) => (x.info?.time?.created ?? 0) - (y.info?.time?.created ?? 0));
  for (let i = ms.length - 1; i >= 0; i--) if (ms[i].info?.role === "assistant" && ms[i].info?.providerID) return ms[i].info.providerID;
  return "";
};

// The resolver exits 1 with "off" on stdout when capture is disabled: stdout
// counts, the exit code does not.
const runFile = (file, args, timeoutMs) => new Promise((resolve) => {
  try {
    execFile(file, args, { timeout: timeoutMs, env: process.env }, (err, stdout) => resolve(err?.killed ? "" : String(stdout ?? "").trim()));
  } catch { resolve(""); }
});
const PAGE = 200; // parent page (B8)
// The messages route takes {limit} and nothing else (SDK 1.14.48: SessionMessagesData.query has directory
// and limit, no cursor, no total). To know whether the page was cut, the plugin asks for PAGE + 1 and keeps
// the newest PAGE: a parent with exactly PAGE messages and nothing older is not "cut". Only the wording depends
// on the route honouring 201: if it caps the answer at PAGE, a long parent shows no "or more" (the omission line
// still counts what the budget dropped). The opening prompt does not depend on it: it is fetched whenever the
// page is full (PAGE messages) and holds no prompt. Live check O4.
const WIDE = 1000; // one wider fetch when a compaction hides the operator's prompt (N3b)
const withDeadline = (p, ms) => Promise.race([p, new Promise((resolve) => setTimeout(() => resolve(undefined), ms).unref?.())]);

// session.idle -> the capture trigger wrapper, detached and unawaited. Shared by
// the v1 and v2 faces (one validation, one switch check, one spawn, one error
// line); each face registers it with its own host event mechanism.
function makeIdleTrigger(triggerScript, logLine) {
  return (sid) => {
    try {
      if (typeof sid !== "string" || !/^[A-Za-z0-9_-]{1,128}$/.test(sid)) return;
      if (!switchOn("MEMOIRE_HOOK_CAPTURE")) return; // the wrapper re-reads it; this only spares a process
      const child = spawn(triggerScript, [sid], { detached: true, stdio: "ignore", env: process.env });
      child.on("error", (e) => logLine({ status: "error=spawn", session: sid, code: e?.code ?? "" }, "memoire-trigger-opencode-plugin"));
      child.unref();
    } catch (e) {
      logLine({ status: "error=spawn", session: String(sid), code: e?.code ?? "" }, "memoire-trigger-opencode-plugin");
    }
  };
}

// MEMOIRE_INJECT_SCRIPT overrides for tarball installs (R-10.5); the default
// stays the fleet checkout path this plugin is installed from. One definition
// for both faces: the dist leak gate exempts this exact line exactly once.
const injectScriptPath = () => process.env.MEMOIRE_INJECT_SCRIPT
    || `${process.env.HOME}/dev/personal/memoire/scripts/memoire-inject-claude.sh`;

export const MemoirePlugin = async ({ $, directory, client }) => {
  const script = injectScriptPath();
  const resolver = process.env.MEMOIRE_RESOLVE_PROJECT || path.join(path.dirname(script), "memoire-resolve-project.sh");
  // Whole-injector deadline (seconds). Positive integers only — zero or junk
  // must fall back, never disable the ceiling.
  const rawDeadline = Number(process.env.MEMOIRE_INJECT_WRAPPER_TIMEOUT);
  const deadline = Number.isInteger(rawDeadline) && rawDeadline > 0 ? rawDeadline : 10;
  const seen = new Set();
  const notChild = new Set();
  const lastInjected = new Map(); // child sessionID -> newest parent entry time already sent

  const logLine = (fields, tag = "memoire-subagent-context-opencode") => {
    try {
      const file = process.env.MEMOIRE_SUBAGENT_LOG || `${memoireHome()}/hooks/subagent.log`;
      fs.mkdirSync(path.dirname(file), { recursive: true });
      // ids come from the harness: control characters stripped, 128 characters at most (A2)
      const clean = (v) => Array.from(String(v).replace(/[\u0000-\u001f\u007f-\u009f\u2028\u2029]/g, "")).slice(0, 128).join("");
      const kvs = Object.entries(fields).map(([k, v]) => `${k}=${clean(v)}`).join(" ");
      fs.appendFileSync(file, `${new Date().toISOString()} ${tag}: ${kvs}\n`);
    } catch { /* logging never blocks */ }
  };

  // Timeline injection: the first message of each session, unchanged. Returns
  // the text (the caller pushes the parts in order).
  const timeline = async (sid) => {
    try {
      if (seen.has(sid)) return "";
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
      if (!result || result.exitCode !== 0) return "";
      const text = result.text();
      return text.trim() ? text : "";
    } catch {
      // fail-open: never block the message on injection errors
      return "";
    }
  };

  // Parts are persisted rows since opencode 1.17.x: id, sessionID and
  // messageID are required (text PK) — a bare {type, text} part crashes
  // message processing with an opaque server error.
  // Random id: the seen-Set is per-process, so a resumed session in a
  // fresh process injects again — a deterministic id would collide on
  // the part PK.
  const pushPart = (sid, input, output, text) => {
    const mid = input.messageID ?? output.message.id;
    // The `prt_memoire_` prefix is how the capture reader recognises this plugin's own
    // parts and does not queue them (MEMOIRE_PART_PREFIX in scripts/memoire-capture-opencode.sh).
    // Both parts (timeline, parent context) go through here; keep the two in step.
    output.parts.push({
      id: `prt_memoire_${crypto.randomUUID()}`,
      sessionID: sid,
      messageID: mid,
      type: "text",
      text,
      synthetic: true,
    });
  };

  // Parent context for child sessions (opt-in). Every chat.message of a child
  // session injects, so a child resumed through task_id gets what the parent
  // did since the last block (lastInjected); the first one gets the full block.
  // Returns the block text ("" when there is nothing to add); t0 is the start of
  // the whole chat.message hook, so the timeline injection and this build share
  // ONE deadline (they run concurrently).
  const parentContext = async (sid, input, output, t0) => {
    let status = "";
    let block = "";
    const f = { session: sid, parent: "-", entries: 0, chars: 0 };
    try {
      if (!client?.session?.get || !client?.session?.messages) return "";
      if (notChild.has(sid) || !switchOn("MEMOIRE_SUBAGENT_CONTEXT")) return "";
      // One deadline for the whole parent-context build (same ceiling as the injector).
      const ms = () => Math.max(1, t0 + deadline * 1000 - Date.now());
      const child = await withDeadline(client.session.get({ path: { id: sid } }), ms());
      const cdata = child?.data ?? child;
      if (!cdata) { status = "error=child-lookup"; return ""; }
      const parentID = cdata.parentID;
      if (!parentID) { notChild.add(sid); status = ""; return ""; }
      f.parent = parentID;
      const parent = await withDeadline(client.session.get({ path: { id: parentID } }), ms());
      const pdata = parent?.data ?? parent;
      const parentDir = pdata?.directory ?? directory;
      const childDir = cdata.directory ?? directory;
      for (const d of [parentDir, childDir]) {
        if ((await runFile(resolver, ["--capture-enabled", d], Math.min(3000, ms()))) === "off") { status = "capture-off"; return ""; }
      }
      // Newest PAGE messages of the parent (the messages route pages newest first and returns the
      // page oldest to newest; entries are re-sorted anyway). One more is asked for: it tells
      // whether anything older exists, and is dropped.
      const res = await withDeadline(client.session.messages({ path: { id: parentID }, query: { limit: PAGE + 1 } }), ms());
      let messages = res?.data ?? res;
      if (!Array.isArray(messages)) { status = "error=messages"; return ""; }
      let cut = messages.length > PAGE;
      if (cut) messages = [...messages].sort((x, y) => (x.info?.time?.created ?? 0) - (y.info?.time?.created ?? 0)).slice(-PAGE);
      const project = (await runFile(resolver, [parentDir], Math.min(3000, ms()))) || path.basename(parentDir);
      const pointer = pointerLine(parentID, project, `OpenCode session ${parentID}`);
      const parentProvider = lastAssistantProvider(messages);
      // The child's provider: the hook input's model, else the message's own (UserMessage.model,
      // always set); unknown only when neither is there (then withheld).
      const childProvider = input.model?.providerID || output?.message?.model?.providerID || "";
      if ((!parentProvider || parentProvider !== childProvider) && !switchOn("MEMOIRE_SUBAGENT_CONTEXT_XPROVIDER")) {
        block = pointer;
        status = "xprovider=withheld";
      } else {
        let entries = extractOpenCode(messages);
        // OpenCode writes one assistant message per model step, so a turn of
        // more than ~200 steps pushes the user message that opened it out of
        // the page. The newest assistant message names it (parentID): fetch it.
        if (messages.length >= PAGE && !entries.some((e) => e.anchor === true) && client.session.message) {
          const opener = await openingPrompt(parentID, messages, ms);
          if (opener) { entries = [opener, ...entries]; cut = true; }
        }
        const last = lastInjected.get(sid);
        if (last !== undefined) entries = entries.filter((e) => e.anchor === true || e.t > last);
        const fresh = entries.filter((e) => e.anchor !== true);
        if (last !== undefined && fresh.length === 0) { status = "nothing-new"; return ""; }
        f.entries = entries.length;
        block = selectBlock(entries, cut, pointer, Math.max(600, intEnv(process.env.MEMOIRE_SUBAGENT_MAX_CHARS, 8000)), intEnv(process.env.MEMOIRE_SUBAGENT_MAX_ENTRIES, 60));
        const newest = entries.reduce((m, e) => Math.max(m, e.t ?? 0), 0);
        lastInjected.set(sid, newest);
        status = "injected";
      }
      if (!block) { status = "empty"; return ""; }
      f.chars = cp(block).length;
      return block;
    } catch {
      status = "error=exception";
      return "";
    } finally {
      if (status) logLine({ status, ...f, elapsed_ms: Date.now() - t0 });
    }
  };

  // The operator's prompt of the parent's current turn, when the fetched page does not hold one
  // (the page was cut). 1) The newest assistant message names the user message it answers
  // (AssistantMessage.parentID, SDK 1.14.48): read it with session.message. 2) After a compaction in
  // the middle of the turn that message is a synthetic "continue" or a compaction message (no operator
  // text), or it no longer exists: then ONE wider fetch (limit WIDE, same deadline) and the newest
  // real operator prompt in it is the anchor; the synthetic message is never the anchor. When there is
  // none, or a fetch fails, the block opens with the no-prompt note (fail-open).
  const openingPrompt = async (parentID, messages, ms) => {
    try {
      const ordered = [...messages].sort((x, y) => (x.info?.time?.created ?? 0) - (y.info?.time?.created ?? 0));
      const newest = [...ordered].reverse().find((m) => m.info?.role === "assistant" && m.info?.summary !== true && m.info?.parentID);
      const pid = newest?.info?.parentID;
      if (pid && !messages.some((m) => m.info?.id === pid)) {
        const res = await withDeadline(client.session.message({ path: { id: parentID, messageID: pid } }), ms());
        const m = res?.data ?? res;
        const p = m?.info?.role === "user" ? extractOpenCode([m]).find((e) => e.kind === "prompt") : undefined;
        if (p) return { ...p, anchor: true };
      }
      const wide = await withDeadline(client.session.messages({ path: { id: parentID }, query: { limit: WIDE } }), ms());
      const all = wide?.data ?? wide;
      if (!Array.isArray(all)) return null;
      const p = extractOpenCode(all).filter((e) => e.kind === "prompt").at(-1);
      return p ? { ...p, anchor: true } : null;
    } catch {
      return null;
    }
  };

  // session.idle -> the capture trigger wrapper (shared helper above).
  const triggerScript = process.env.MEMOIRE_TRIGGER_OPENCODE_SCRIPT
    || path.join(path.dirname(script), "memoire-trigger-capture-opencode.sh");
  const onIdle = makeIdleTrigger(triggerScript, logLine);

  return {
    event: async ({ event }) => {
      try { if (event?.type === "session.idle") onIdle(event.properties?.sessionID); } catch { /* fail-open */ }
    },
    "chat.message": async (input, output) => {
      let sid;
      try { sid = input.sessionID ?? output.message.sessionID; } catch { return; }
      const t0 = Date.now();
      const [tl, pc] = await Promise.all([timeline(sid), parentContext(sid, input, output, t0)]);
      try {
        if (tl) pushPart(sid, input, output, tl);
        if (pc) pushPart(sid, input, output, pc);
      } catch { /* fail-open */ }
    },
  };
};

// ---- OpenCode v2 -----------------------------------------------------------------
// Shared pieces (switches, selection rule, pointer line, trigger spawn) are the
// v1 ones above. What differs: the injector runs through node:child_process
// (no Bun `$`), the parent's messages come from ctx.session.context (v2
// message shape: a user row {text, files}, an assistant row {content: [text |
// reasoning | tool]}), and the blocks ride the system parts of every dispatch.

// The injector, as v1 runs it: `timeout -k 2 <deadline> <script>` with
// {"cwd": directory} on stdin, cwd = directory; a JS race is the second belt.
// Resolves "" on any failure, non-zero exit or empty output.
function runInjector(script, directory, deadline) {
  return new Promise((resolve) => {
    let done = false;
    const finish = (v) => { if (!done) { done = true; resolve(v); } };
    try {
      const child = spawn("timeout", ["-k", "2", String(deadline), script], {
        cwd: directory, env: process.env, stdio: ["pipe", "pipe", "ignore"],
      });
      let out = "";
      const timer = setTimeout(() => { try { child.kill("SIGKILL"); } catch { /* */ } finish(""); }, (deadline + 2) * 1000);
      timer.unref?.();
      child.stdout.on("data", (d) => { out += d; });
      child.on("error", () => { clearTimeout(timer); finish(""); });
      child.on("close", (code) => { clearTimeout(timer); finish(code === 0 && out.trim() ? out : ""); });
      child.stdin.on("error", () => { /* the script may exit before reading */ });
      child.stdin.end(JSON.stringify({ cwd: directory }));
    } catch {
      finish("");
    }
  });
}

// v2 messages -> normalized entries (same entries as extractOpenCode).
const toolText = (c) => Array.isArray(c)
  ? c.filter((x) => x && x.type === "text" && typeof x.text === "string").map((x) => x.text).join("\n")
  : typeof c === "string" ? c : "";
function extractOpenCode2(messages) {
  const ms = (Array.isArray(messages) ? messages : []).filter((m) => m && typeof m === "object")
    .sort((x, y) => (x.time?.created ?? 0) - (y.time?.created ?? 0));
  const entries = [];
  for (const m of ms) {
    const t0 = m.time?.created ?? 0;
    if (m.type === "user") {
      if (typeof m.text === "string" && m.text !== "") entries.push({ kind: "prompt", ts: hms(t0), t: t0, text: m.text });
    } else if (m.type === "assistant") {
      for (const c of Array.isArray(m.content) ? m.content : []) {
        if (!c || typeof c !== "object") continue;
        if (c.type === "text" && typeof c.text === "string" && c.text !== "") {
          entries.push({ kind: "text", ts: hms(t0), t: t0, text: c.text });
        } else if (c.type === "tool") {
          const st = c.state ?? {};
          const t = c.time?.created ?? t0;
          entries.push({
            kind: "tool", ts: hms(t), t, name: c.name ?? "", input: kv(st.input),
            status: st.status === "completed" ? "ok" : st.status === "error" ? "error" : "running",
            result: st.status === "completed" ? toolText(st.content) : st.status === "error" ? String(st.error?.message ?? st.error ?? "") : "",
          });
        }
      }
    }
  }
  let a = -1;
  entries.forEach((e, i) => { if (e.kind === "prompt") a = i; });
  if (a >= 0) entries[a].anchor = true;
  return entries;
}
const lastAssistantProvider2 = (messages) => {
  const ms = (Array.isArray(messages) ? messages : []).filter((m) => m && typeof m === "object")
    .sort((x, y) => (x.time?.created ?? 0) - (y.time?.created ?? 0));
  for (let i = ms.length - 1; i >= 0; i--) if (ms[i].type === "assistant" && ms[i].model?.providerID) return ms[i].model.providerID;
  return "";
};
// ctx.session.context returns the loaded messages; accept an array or {messages}.
const contextMessages = (r) => Array.isArray(r) ? r : Array.isArray(r?.messages) ? r.messages : Array.isArray(r?.data) ? r.data : null;
const IDLE_EVENTS = new Set(["session.idle", "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted"]);

export async function setupV2(ctx) {
  const directory = ctx?.location?.directory || ctx?.directory || process.cwd();
  const script = injectScriptPath();
  const resolver = process.env.MEMOIRE_RESOLVE_PROJECT || path.join(path.dirname(script), "memoire-resolve-project.sh");
  const triggerScript = process.env.MEMOIRE_TRIGGER_OPENCODE_SCRIPT
    || path.join(path.dirname(script), "memoire-trigger-capture-opencode.sh");
  const rawDeadline = Number(process.env.MEMOIRE_INJECT_WRAPPER_TIMEOUT);
  const deadline = Number.isInteger(rawDeadline) && rawDeadline > 0 ? rawDeadline : 10;
  const logLine = (fields, tag = "memoire-subagent-context-opencode") => {
    try {
      const file = process.env.MEMOIRE_SUBAGENT_LOG || `${memoireHome()}/hooks/subagent.log`;
      fs.mkdirSync(path.dirname(file), { recursive: true });
      const clean = (v) => Array.from(String(v).replace(/[\u0000-\u001f\u007f-\u009f\u2028\u2029]/g, "")).slice(0, 128).join("");
      fs.appendFileSync(file, `${new Date().toISOString()} ${tag}: ${Object.entries(fields).map(([k, v]) => `${k}=${clean(v)}`).join(" ")}\n`);
    } catch { /* logging never blocks */ }
  };
  // sid -> {timeline, parent: {block, pointer, provider}}
  const blocks = new Map();
  const timelineStarted = new Set();
  const notChild = new Set();

  // The parent block of a child session at this prompt: the parent's recent
  // activity by the shared selection rule (v1 parentContext, v2 sources).
  const parentContext2 = async (sid, t0) => {
    let status = "";
    const f = { session: sid, parent: "-", entries: 0, chars: 0 };
    try {
      if (!ctx.session?.get || !ctx.session?.context) return null;
      if (notChild.has(sid) || !switchOn("MEMOIRE_SUBAGENT_CONTEXT")) return null;
      const ms = () => Math.max(1, t0 + deadline * 1000 - Date.now());
      const child = await withDeadline(ctx.session.get({ sessionID: sid }), ms());
      if (!child) { status = "error=child-lookup"; return null; }
      const parentID = child.parentID;
      if (!parentID) { notChild.add(sid); return null; }
      f.parent = parentID;
      const parent = await withDeadline(ctx.session.get({ sessionID: parentID }), ms());
      const parentDir = parent?.location?.directory ?? directory;
      const childDir = child.location?.directory ?? directory;
      for (const d of [parentDir, childDir]) {
        if ((await runFile(resolver, ["--capture-enabled", d], Math.min(3000, ms()))) === "off") { status = "capture-off"; return null; }
      }
      const messages = contextMessages(await withDeadline(ctx.session.context({ sessionID: parentID }), ms()));
      if (!messages) { status = "error=messages"; return null; }
      const project = (await runFile(resolver, [parentDir], Math.min(3000, ms()))) || path.basename(parentDir);
      const pointer = pointerLine(parentID, project, `OpenCode session ${parentID}`);
      const entries = extractOpenCode2(messages);
      f.entries = entries.length;
      const block = selectBlock(entries, false, pointer, Math.max(600, intEnv(process.env.MEMOIRE_SUBAGENT_MAX_CHARS, 8000)), intEnv(process.env.MEMOIRE_SUBAGENT_MAX_ENTRIES, 60));
      if (!block) { status = "empty"; return null; }
      f.chars = cp(block).length;
      status = "built";
      return { block, pointer, provider: lastAssistantProvider2(messages) };
    } catch {
      status = "error=exception";
      return null;
    } finally {
      if (status) logLine({ status, ...f, elapsed_ms: Date.now() - t0 });
    }
  };

  try {
    await ctx.session.hook("prompt", async (input) => {
      try {
        const sid = input?.sessionID;
        if (typeof sid !== "string" || sid === "") return;
        const t0 = Date.now();
        const cur = blocks.get(sid) ?? {};
        const jobs = [];
        if (!timelineStarted.has(sid)) {
          timelineStarted.add(sid);
          jobs.push(runInjector(script, directory, deadline).then((t) => { if (t.trim()) cur.timeline = t; }));
        }
        jobs.push(parentContext2(sid, t0).then((p) => { if (p) cur.parent = p; }));
        await Promise.all(jobs);
        blocks.set(sid, cur);
      } catch { /* fail-open */ }
    });
    await ctx.session.hook("context", (input) => {
      try {
        const b = blocks.get(input?.sessionID);
        if (!b || !Array.isArray(input.system)) return;
        if (b.timeline) input.system.push({ type: "text", text: b.timeline });
        if (b.parent) {
          const childProvider = input.model?.providerID ?? "";
          const same = b.parent.provider !== "" && b.parent.provider === childProvider;
          const text = same || switchOn("MEMOIRE_SUBAGENT_CONTEXT_XPROVIDER") ? b.parent.block : b.parent.pointer;
          if (!same && text === b.parent.pointer) logLine({ status: "xprovider=withheld", session: input.sessionID, parent: "-" });
          if (text) input.system.push({ type: "text", text });
        }
      } catch { /* fail-open */ }
    });
  } catch { /* a host without these hooks: injection off, capture trigger below still wired */ }

  // the idle events -> the capture trigger wrapper (shared helper).
  const onIdle = makeIdleTrigger(triggerScript, logLine);
  const events = new AbortController();
  if (typeof ctx?.event?.subscribe === "function") {
    (async () => {
      for await (const e of ctx.event.subscribe({ signal: events.signal })) {
        try {
          if (!e || !IDLE_EVENTS.has(e.type)) continue;
          onIdle(e.data?.sessionID ?? e.properties?.sessionID);
        } catch { /* fail-open */ }
      }
    })().catch(() => { /* the stream ended or failed: capture falls back to the tick */ });
  }
  return () => events.abort();
}

export default {
  id: "superbiche.memoire",
  setup: setupV2,
  server: MemoirePlugin,
};
