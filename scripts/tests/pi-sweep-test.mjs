// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// pi-sweep-test.mjs — the host-side Pi sweep (scripts/memoire-capture-pi.mjs).
// Synthetic session files in scratch stores laid out like each kind of Pi session (agent, seat,
// caged worker), a fake resolver and a fake queue writer under a scratch MEMOIRE_HOME: no Pi,
// no network, no live state, no write to the live hub.
// Covers: every kind found and the probe stores left out; symbolic links never followed; the
// header id (not the file name) is the session; the extension's event shape and keys; one shared
// cursor (a second run queues nothing; entries the extension settled are not queued; entries the
// sweep settled are not re-queued by the extension's cursor reader); a session still being written
// is left and picked up later without loss (incomplete last line dropped); a failed write settles
// nothing; capture-disabled projects are excluded (own set), exclusions union across cursors;
// kill switch; dry-run writes nothing; rejects; the memoire section is scrubbed; the older cursor
// is migrated away; the summary line.

import path from "node:path";
import fs from "node:fs";
import os from "node:os";
import { spawnSync } from "node:child_process";

const here = path.dirname(new URL(import.meta.url).pathname);
const sweep = path.resolve(here, "../memoire-capture-pi.mjs");
let failures = 0;
const check = (cond, label) => { if (!cond) { failures += 1; console.error(`FAIL: ${label}`); } };

const root = fs.mkdtempSync(path.join(process.env.TMPDIR || os.tmpdir(), "pi-sweep."));
process.on("exit", () => { try { fs.rmSync(root, { recursive: true, force: true }); } catch { /* */ } });
const home = path.join(root, "home"); fs.mkdirSync(home, { recursive: true });
const mh = path.join(root, "mh"); fs.mkdirSync(mh, { recursive: true });
const batches = path.join(root, "batches"); fs.mkdirSync(batches);
const script = (name, body) => { const p = path.join(root, name); fs.writeFileSync(p, `#!/usr/bin/env bash\n${body}\n`, { mode: 0o755 }); return p; };
const modeFile = (n) => path.join(root, `${n}.mode`);
const setMode = (n, m) => fs.writeFileSync(modeFile(n), m);
const queue = script("queue.sh", `
[[ "$1" == "-batch-file" ]] || exit 9
[[ "$(cat "${modeFile("queue")}" 2>/dev/null)" == fail ]] && exit 1
cp "$2" "${batches}/$(date +%s%N).jsonl"`);
const resolver = script("resolve.sh", `
if [[ "$1" == "--capture-enabled" ]]; then
  [[ "$2" == *private* ]] && { echo off; exit 1; }
  echo on; exit 0
fi
printf 'proj-key\\t%s\\tregistry\\n' "$2"`);

const env = (extra = {}) => ({
  ...process.env, HOME: home, MEMOIRE_HOME: mh, MEMOIRE_QUEUE_BIN: queue, MEMOIRE_RESOLVE_PROJECT: resolver,
  LIAISON_PI_SEAT_STATE: path.join(root, "seat-state"), MEMOIRE_PI_RUNS_DIR: path.join(root, "runs"),
  PI_CODING_AGENT_DIR: path.join(root, "agent"), ...extra,
});
delete process.env.MEMOIRE_CAPTURE;
const run = (args = ["--scan"], extra = {}) => spawnSync("node", [sweep, "--summary", ...args], { env: env(extra), encoding: "utf8", timeout: 60000 });
const summary = (r) => { const [sessions, events, failed, rejected, stopped, left] = r.stdout.trim().split("\n").pop().split("\t").map(Number); return { sessions, events, failed, rejected, stopped, left }; };
const readBatches = () => fs.readdirSync(batches).sort().map((f) => fs.readFileSync(path.join(batches, f), "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l)));
const queued = () => readBatches().flat().filter((e) => e.event_type === "pi_entry").map((e) => e.source_event_id.split(":").pop());
const clearBatches = () => { for (const f of fs.readdirSync(batches)) fs.rmSync(path.join(batches, f)); };
const cursorFile = (sid) => path.join(mh, "capture-cursors", "pi", `${sid}.json`);
const legacyFile = (sid) => path.join(mh, "pi-capture", `${sid}.json`);
const readCursor = (sid) => JSON.parse(fs.readFileSync(cursorFile(sid), "utf8"));

const ts = "2026-11-14T06:00:00.000Z";
const header = (id, cwd) => ({ type: "session", version: 3, id, timestamp: ts, cwd });
const msg = (id, message) => ({ type: "message", id, parentId: null, timestamp: ts, message });
const user = (id, text) => msg(id, { role: "user", timestamp: 1, content: [{ type: "text", text }] });
const assistant = (id, text, stopReason = "stop") => msg(id, { role: "assistant", provider: "conductor", model: "coder", stopReason, timestamp: 1, content: [{ type: "text", text }] });
const session = (file, id, cwd, entries, { age = 3600 } = {}) => {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, [header(id, cwd), ...entries].map((r) => JSON.stringify(r)).join("\n") + "\n");
  const t = (Date.now() / 1000) - age; fs.utimesSync(file, t, t);
  return file;
};
const ids = (list) => list.slice().sort();

// --- every kind found; probes left out; links never followed ------------------------------
{
  const f1 = session(path.join(root, "agent/sessions/--work-a--/2026-11-14T00-00-00-000Z_name-chosen-by-caller.jsonl"), "agent-sess-1", "/work/a", [user("a1", "hi"), assistant("a2", "yo")]);
  session(path.join(root, "seat-state/sessions/conclave-coordinator/cockpit-key/--work-seat--/2026-11-14T00-00-00-000Z_seat-sess-1.jsonl"), "seat-sess-1", "/work/seat", [user("s1", "plan")]);
  session(path.join(root, "runs/run-1/acpx/pi-sessions/--work-cage--/2026-11-14T00-00-00-000Z_cage-sess-1.jsonl"), "cage-sess-1", "/work/cage", [user("c1", "task"), assistant("c2", "done")]);
  session(path.join(home, ".local/share/liaison/pi-floor-probe/x/pi-home/sessions/--p--/2026-11-14T00-00-00-000Z_probe-1.jsonl"), "probe-sess-1", "/work/p", [user("p1", "probe")]);
  // a worker plants links: one to a file outside, one to a directory outside
  const outside = session(path.join(root, "outside/real.jsonl"), "outside-sess", "/work/o", [user("o1", "outside")]);
  fs.symlinkSync(outside, path.join(root, "runs/run-1/acpx/pi-sessions/--work-cage--/link.jsonl"));
  fs.symlinkSync(path.join(root, "outside"), path.join(root, "runs/run-1/acpx/pi-sessions/linkdir"));
  const r = run();
  const s = summary(r);
  check(r.status === 0 && s.sessions === 3 && s.failed === 0, `kinds: agent, seat and cage sessions queued, summary line (${r.stdout.trim()})`);
  const got = queued();
  check(ids(got).join() === ids(["a1", "a2", "s1", "c1", "c2"]).join(), `kinds: exactly their entries are queued (${got})`);
  check(!got.includes("p1") && !got.includes("o1"), "kinds: the probe store and linked files/directories are not swept");
  const events = readBatches().flat();
  check(events.some((e) => e.source_event_id === "pi:entry:agent-sess-1:a1" && e.session_id === "agent-sess-1" && e.source_adapter === "pi" && e.project === "proj-key"),
    "shape: header id is the session (file named for another id), adapter pi, project resolved");
  check(events.some((e) => e.event_type === "pi_assistant_turn" && e.source_event_id === "pi:turn:agent-sess-1:a2" && e.text === "yo"), "shape: assistant turn event as the extension builds it");
  check(readCursor("seat-sess-1").settled.join() === "s1", "cursor: the shared cursor records what was written");
  check(fs.existsSync(f1), "sanity: sources untouched");
  // second run: nothing
  clearBatches();
  const s2 = summary(run());
  check(s2.events === 0 && queued().length === 0, "second run queues nothing");
}

// --- shared cursor with the extension ------------------------------------------------------
{
  clearBatches();
  const file = path.join(root, "agent/sessions/--work-b--/f_shared.jsonl");
  session(file, "shared-1", "/work/b", [user("e1", "one"), user("e2", "two"), user("e3", "three")]);
  fs.mkdirSync(path.dirname(cursorFile("shared-1")), { recursive: true });
  fs.writeFileSync(cursorFile("shared-1"), JSON.stringify({ schema: "pi-capture-cursor/1", settled: ["e1"], excluded: [] }));   // what the extension delivered
  run();
  check(ids(queued()).join() === "e2,e3", `shared cursor: entries the extension settled are not queued again (${queued()})`);
  check(ids(readCursor("shared-1").settled).join() === "e1,e2,e3", "shared cursor: the sweep adds to it, keeping the extension's");
  // the extension's reader sees the sweep's work
  const ext = await import(new URL("../pi-extension/memoire.js", import.meta.url).href);
  process.env.MEMOIRE_HOME = mh;
  check(ids([...ext.readCursor("shared-1").settled]).join() === "e1,e2,e3", "shared cursor: the extension reads what the sweep settled");
}

// --- a session still being written ----------------------------------------------------------
{
  clearBatches();
  const file = path.join(root, "agent/sessions/--work-c--/f_live.jsonl");
  const lines = [header("live-1", "/work/c"), user("l1", "one"), user("l2", "two")].map((r) => JSON.stringify(r));
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, lines.join("\n") + "\n" + lines[2].slice(0, 20));    // an incomplete last line
  let t = Date.now() / 1000 - 3600; fs.utimesSync(file, t, t);
  run();
  check(ids(queued()).join() === "l1,l2", `live: an incomplete last line is dropped, the complete entries queued (${queued()})`);
  // fresh file: not read at all while within --quiet-for
  clearBatches();
  fs.appendFileSync(file, "");
  fs.writeFileSync(file, lines.join("\n") + "\n" + JSON.stringify(user("l3", "three")) + "\n");   // the writer finished the line and added one
  const now = Date.now() / 1000; fs.utimesSync(file, now, now);
  const r = run(["--scan", "--quiet-for", "120"]);
  check(summary(r).left === 1 && queued().length === 0, "live: a file changed within --quiet-for is left for later, not read");
  t = Date.now() / 1000 - 3600; fs.utimesSync(file, t, t);
  run(["--scan", "--quiet-for", "120"]);
  check(ids(queued()).join() === "l3", `live: later it is picked up, only what is new (${queued()})`);
}

// --- failed delivery settles nothing -------------------------------------------------------
{
  clearBatches();
  session(path.join(root, "agent/sessions/--work-d--/f_fail.jsonl"), "fail-1", "/work/d", [user("f1", "one"), user("f2", "two")]);
  setMode("queue", "fail");
  const r = run();
  check(r.status === 1 && summary(r).failed === 1 && queued().length === 0, "failure: a failed write queues nothing and the run says so (exit 1)");
  check(!fs.existsSync(cursorFile("fail-1")) || readCursor("fail-1").settled.length === 0, "failure: nothing is settled without a successful write");
  setMode("queue", "ok");
  run();
  check(ids(queued()).join() === "f1,f2" && readCursor("fail-1").settled.length === 2, "failure: the next run delivers everything, then settles it");
}

// --- explicit exclusion ---------------------------------------------------------------------
{
  clearBatches();
  session(path.join(root, "agent/sessions/--private--/f_priv.jsonl"), "priv-1", "/work/private", [user("x1", "secret")]);
  run();
  check(queued().length === 0 && readCursor("priv-1").excluded.join() === "x1" && readCursor("priv-1").settled.length === 0,
    "exclusion: a capture-disabled project is recorded as excluded, never queued, never settled");
  const r = run([ "--scan" ], { MEMOIRE_CAPTURE: "off" });
  check(r.status === 0 && r.stdout.trim() === "", "exclusion: MEMOIRE_CAPTURE=off ends the run silently");
  // exclusions union across two cursors
  clearBatches();
  session(path.join(root, "agent/sessions/--work-e--/f_union.jsonl"), "union-1", "/work/e", [user("u1", "a"), user("u2", "b"), user("u3", "c")]);
  fs.mkdirSync(path.dirname(legacyFile("union-1")), { recursive: true });
  fs.mkdirSync(path.dirname(cursorFile("union-1")), { recursive: true });
  fs.writeFileSync(legacyFile("union-1"), JSON.stringify({ schema: "pi-capture-cursor/1", settled: ["u1"], excluded: [] }));
  fs.writeFileSync(cursorFile("union-1"), JSON.stringify({ schema: "pi-capture-cursor/1", settled: ["u1"], excluded: ["u2"] }));
  run();
  check(ids(queued()).join() === "u3", `exclusion: the newer exclusion survives the older cursor (${queued()})`);
  check(!fs.existsSync(legacyFile("union-1")), "migration: the older cursor is removed once the current one holds all of it");
}

// --- dry run, rejects, scrubbing -----------------------------------------------------------
{
  clearBatches();
  session(path.join(root, "agent/sessions/--work-f--/f_dry.jsonl"), "dry-1", "/work/f", [user("d1", "one")]);
  const r = run(["--scan", "--dry-run"]);
  const printed = r.stdout.split("\n").filter((l) => l.startsWith("{")).map((l) => JSON.parse(l));
  check(printed.some((e) => e.source_event_id === "pi:entry:dry-1:d1") && queued().length === 0 && !fs.existsSync(cursorFile("dry-1")),
    "dry run: prints the events, queues nothing, writes no cursor");
  clearBatches();
  fs.mkdirSync(path.join(root, "agent/sessions/--work-g--"), { recursive: true });
  fs.writeFileSync(path.join(root, "agent/sessions/--work-g--/f_bad1.jsonl"), "not json\n");
  fs.writeFileSync(path.join(root, "agent/sessions/--work-g--/f_bad2.jsonl"), JSON.stringify({ ...header("bad-2", "/w"), version: 9 }) + "\n");
  fs.writeFileSync(path.join(root, "agent/sessions/--work-g--/f_bad3.jsonl"), [header("bad-3", "/w"), "garbage", JSON.stringify(user("b1", "x"))].map((l) => (typeof l === "string" ? l : JSON.stringify(l))).join("\n") + "\n");
  const rr = run();
  check(summary(rr).rejected === 3 && rr.status === 0, `rejects: unparseable header, unsupported version and a bad middle line are rejected, counted, not fatal (${rr.stdout.trim()})`);
  // the injected memoire section never re-enters the store
  clearBatches();
  session(path.join(root, "agent/sessions/--work-h--/f_scrub.jsonl"), "scrub-1", "/work/h", [msg("sys1", { role: "system", content: "A\n<memoire>INJECTED-CONTEXT</memoire>\nB", sections: { preamble: "A", memoire: "INJECTED-CONTEXT" }, timestamp: 1 })]);
  run();
  const sys = readBatches().flat().find((e) => e.source_event_id === "pi:entry:scrub-1:sys1");
  check(sys && !JSON.stringify(sys).includes("INJECTED-CONTEXT") && sys.payload.entry.message.sections.memoire_omitted === true, "scrub: the injected memoire section is removed from a system message");
}

// --- bounded: left files beyond --max-seconds ----------------------------------------------
{
  clearBatches();
  const r = run(["--scan", "--max-seconds", "0.000001"]);
  const s = summary(r);
  check(s.stopped === 1 && s.left >= 1, `bound: the run stops at a file boundary when the time bound is reached, files counted left (${r.stdout.trim()})`);
}

if (failures) { console.error(`pi-sweep-test: ${failures} failure(s)`); process.exit(1); }
console.log("pi-sweep-test: OK");
