#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// memoire-capture-pi.mjs — host-side sweep of Pi sessions (store-sweep lane, like OpenCode's).
//
// The in-session extension (scripts/pi-extension/memoire.js) captures what a Pi session writes
// while it is loaded and can reach the queue. It misses what it cannot see: a Conclave seat (its
// own Pi home, no Memoire mounts), a caged worker (the run directory), entries written before the
// extension loaded or after a failed write whose cursor was lost, and sessions that died before
// their first settle. This sweep reads the session FILES on the host and queues what is missing.
//
// One shape, one cursor, one key. It builds events with the extension's own `buildEvents` (same
// event types, same `pi:entry:<session id>:<entry id>` keys, same removal of the injected
// `memoire` section), reads and writes the extension's own cursor
// ($MEMOIRE_HOME/capture-cursors/pi/<session id>.json, `settled` and `excluded` sets), and writes
// through the same queue writer (`memoire-queue -batch-file`), so redaction stays at the
// queue-write gate (MEMOIRE_REDACT). The hub keeps one row per key, so a session captured by the
// extension AND swept is absorbed once; the shared cursor only saves the replay. A session is
// identified by its HEADER id (not its file name).
//
// Where each kind of session lives (read from the launchers and the floor code, 2026-11-14):
//   agent   ~/.pi/agent/sessions/<cwd key>/*.jsonl        ordinary, sandboxed, T3 Code, ACP
//           ($PI_CODING_AGENT_DIR/sessions when set; every launcher mounts this real directory)
//   seat    $LIAISON_PI_SEAT_STATE | ~/.local/share/liaison/pi-seat-state
//             /sessions/<seat>/<cockpit key>/<cwd key>/*.jsonl      Conclave coordinator seat
//   cage    <conclave runs>/<run id>/acpx/pi-sessions/<cwd key>/*.jsonl   caged worker
//           (runs: $MEMOIRE_PI_RUNS_DIR | ($LIAISON_CONCLAVE_HOME | ~/dev/conclave)/runs)
// Explicitly NOT swept, with the reason: the floor's probe stores and test homes
// (~/.local/share/liaison/pi-floor-probe, pi-seat-probe, k22c-*: synthetic probe runs, no
// operator work); `--no-session` runs (no file exists). $MEMOIRE_PI_SESSION_ROOTS (colon list)
// replaces the default roots for a test or a relocated store.
//
// Rules:
//   * A session still being written is left for a later run: a file modified within --quiet-for
//     seconds is not read (counted `left`); an incomplete last line is dropped; entries are
//     tracked by id in the cursor, so nothing is lost and nothing is done twice.
//   * Explicit exclusion is its own rule, as in the extension: MEMOIRE_CAPTURE=off|0|false ends the
//     run; a project the resolver reports capture-disabled is recorded in the cursor's `excluded`
//     set (not `settled`), and exclusions resolve by union across cursors.
//   * Worker-writable stores (seat, cage) are untrusted data: symbolic links (files and
//     directories) are never followed, a file over 256 MiB is rejected, the header's cwd is taken
//     as reported (reported, not attested), and the content only ever becomes event payload.
//   * Only a successful queue write settles entries; a failed chunk is retried by the next run.
//
// Usage: memoire-capture-pi.mjs [--scan | file.jsonl ...] [--root DIR]... [--since SPEC]
//          [--quiet-for S] [--max-seconds S] [--summary] [--dry-run] [--help]
//   --since   epoch seconds, `@epoch`, or an ISO time: only files modified since (default: all)
//   --summary last stdout line: sessions<TAB>events<TAB>failed<TAB>rejected<TAB>stopped<TAB>left<TAB>blobs<TAB>source
//   --dry-run print the events that would be queued as JSON lines on stdout; write nothing, no cursor
//
// Environment: MEMOIRE_HOME, MEMOIRE_QUEUE_BIN, MEMOIRE_RESOLVE_PROJECT (the extension's knobs),
//   MEMOIRE_PI_SESSION_ROOTS, MEMOIRE_PI_RUNS_DIR, LIAISON_PI_SEAT_STATE, LIAISON_CONCLAVE_HOME,
//   PI_CODING_AGENT_DIR, MEMOIRE_PI_EXTENSION_PATH (the extension file to import; tests).

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

const here = path.dirname(new URL(import.meta.url).pathname);
const extPath = process.env.MEMOIRE_PI_EXTENSION_PATH
  ? path.resolve(process.env.MEMOIRE_PI_EXTENSION_PATH)
  : path.resolve(here, "pi-extension/memoire.js");
const ext = await import(pathToFileURL(extPath).href);
const { buildEvents, readCursor, writeCursor, cursorPath, resolveProject, writeBatch, captureOff } = ext;

const MAX_FILE_BYTES = 256 * 1024 * 1024;
const CHUNK_ENTRIES = 400;
const MAX_DEPTH = 5;
const log = (m) => process.stderr.write(`memoire-capture-pi: ${m}\n`);
const home = os.homedir();

function usage() {
  process.stderr.write(fs.readFileSync(new URL(import.meta.url), "utf8").split("\n")
    .filter((l) => l.startsWith("// Usage") || l.startsWith("//   --") || l.startsWith("//          ")).map((l) => l.slice(3)).join("\n") + "\n");
}

// --- arguments -------------------------------------------------------------
const opts = { scan: false, roots: [], since: 0, quietFor: 0, maxSeconds: 0, summary: false, dryRun: false, files: [] };
{
  const a = process.argv.slice(2);
  const num = (v, name) => { const n = Number(v); if (!Number.isFinite(n) || n < 0) { log(`bad ${name}: ${v}`); process.exit(2); } return n; };
  for (let i = 0; i < a.length; i++) {
    const k = a[i];
    if (k === "--scan") opts.scan = true;
    else if (k === "--root") opts.roots.push(a[++i]);
    else if (k === "--since") {
      const v = String(a[++i] ?? "").replace(/^@/, "");
      opts.since = /^\d+(\.\d+)?$/.test(v) ? Number(v) : Date.parse(v) / 1000;
      if (!Number.isFinite(opts.since)) { log(`bad --since: ${v}`); process.exit(2); }
    } else if (k === "--quiet-for") opts.quietFor = num(a[++i], "--quiet-for");
    else if (k === "--max-seconds") opts.maxSeconds = num(a[++i], "--max-seconds");
    else if (k === "--summary") opts.summary = true;
    else if (k === "--dry-run") opts.dryRun = true;
    else if (k === "-h" || k === "--help") { usage(); process.exit(0); }
    else if (k.startsWith("-")) { usage(); log(`unknown option: ${k}`); process.exit(2); }
    else opts.files.push(k);
  }
  if (opts.scan && opts.files.length) { log("--scan and explicit files are mutually exclusive"); process.exit(2); }
  if (!opts.scan && !opts.files.length) { usage(); process.exit(2); }
}

// --- discovery -------------------------------------------------------------
const dirents = (d) => { try { return fs.readdirSync(d, { withFileTypes: true }); } catch { return []; } };

function defaultRoots() {
  const explicit = (process.env.MEMOIRE_PI_SESSION_ROOTS || "").split(":").filter(Boolean);
  if (explicit.length) return explicit.map((d) => ({ kind: "custom", dir: d }));
  const agent = process.env.PI_CODING_AGENT_DIR ? path.join(process.env.PI_CODING_AGENT_DIR, "sessions") : path.join(home, ".pi/agent/sessions");
  const seat = path.join(process.env.LIAISON_PI_SEAT_STATE || path.join(home, ".local/share/liaison/pi-seat-state"), "sessions");
  const runs = process.env.MEMOIRE_PI_RUNS_DIR
    || path.join(process.env.LIAISON_CONCLAVE_HOME || path.join(home, "dev/conclave"), "runs");
  const roots = [{ kind: "agent", dir: agent }, { kind: "seat", dir: seat }];
  for (const e of dirents(runs)) {
    if (e.isDirectory() && !e.isSymbolicLink()) roots.push({ kind: "cage", dir: path.join(runs, e.name, "acpx", "pi-sessions") });
  }
  return roots;
}

// Never follow a symbolic link: a seat or a cage can write its own store.
function walk(dir, depth, out) {
  if (depth > MAX_DEPTH) return;
  for (const e of dirents(dir)) {
    if (e.isSymbolicLink()) continue;
    const full = path.join(dir, e.name);
    if (e.isDirectory()) walk(full, depth + 1, out);
    else if (e.isFile() && e.name.endsWith(".jsonl")) out.push(full);
  }
}

function discover() {
  if (opts.files.length) return opts.files.map((f) => ({ kind: "file", file: path.resolve(f) }));
  const found = [];
  const roots = opts.roots.length ? opts.roots.map((d) => ({ kind: "custom", dir: d })) : defaultRoots();
  for (const r of roots) {
    const list = [];
    try { if (fs.lstatSync(r.dir).isDirectory()) walk(r.dir, 0, list); } catch { /* absent root: nothing to sweep */ }
    for (const f of list) found.push({ kind: r.kind, file: f });
  }
  return found;
}

// --- one file --------------------------------------------------------------
class Reject extends Error {}

function readSession(file) {
  const st = fs.lstatSync(file);
  if (!st.isFile()) throw new Reject("not a regular file");
  if (st.size > MAX_FILE_BYTES) throw new Reject("over 256 MiB");
  const lines = fs.readFileSync(file, "utf8").split("\n").filter((l) => l.trim() !== "");
  if (!lines.length) throw new Reject("empty");
  let header;
  try { header = JSON.parse(lines[0]); } catch { throw new Reject("unparseable header"); }
  if (!header || header.type !== "session" || typeof header.id !== "string" || !header.id) throw new Reject("no session header");
  if (header.version !== 3) throw new Reject(`unsupported version ${header.version}`);
  const entries = [];
  const seen = new Set();
  for (let i = 1; i < lines.length; i++) {
    let e;
    try { e = JSON.parse(lines[i]); } catch {
      if (i === lines.length - 1) break;   // an incomplete last line: the session is still being written
      throw new Reject(`unparseable entry ${i + 1}`);
    }
    if (!e || typeof e !== "object" || typeof e.id !== "string" || !e.id) throw new Reject(`entry ${i + 1} has no id`);
    if (seen.has(e.id)) continue;
    seen.add(e.id);
    entries.push(e);
  }
  return { header, entries, mtimeMs: st.mtimeMs };
}

const projects = new Map();   // cwd -> project | null, per run
async function projectOf(cwd) {
  if (!projects.has(cwd)) projects.set(cwd, await resolveProject(cwd));
  return projects.get(cwd);
}

// The shared cursor, merged with whatever another writer (the extension) settled meanwhile.
function saveCursor(sid, addSettled, addExcluded) {
  const cur = readCursor(sid) || { settled: new Set(), excluded: new Set() };
  for (const id of addSettled) cur.settled.add(id);
  for (const id of addExcluded) cur.excluded.add(id);
  if (!writeCursor(sid, cur.settled, cur.excluded)) return false;
  // Migration: once the current cursor holds everything the older pi-capture/ cursor held, drop the
  // older one (it would otherwise keep limiting the intersection forever). Host side only.
  const legacy = path.join(process.env.MEMOIRE_HOME || path.join(home, ".memoire"), "pi-capture", path.basename(cursorPath(sid)));
  try {
    const old = JSON.parse(fs.readFileSync(legacy, "utf8"));
    const oldSettled = new Set(Array.isArray(old.settled) ? old.settled : []);
    const oldExcluded = new Set(Array.isArray(old.excluded) ? old.excluded : []);
    if ([...oldSettled].every((x) => cur.settled.has(x)) && [...oldExcluded].every((x) => cur.excluded.has(x))) fs.rmSync(legacy, { force: true });
  } catch { /* no older cursor */ }
  return true;
}

const totals = { sessions: 0, events: 0, failed: 0, rejected: 0, stopped: 0, left: 0 };

async function sweepFile({ kind, file }) {
  let s;
  try { s = readSession(file); } catch (e) {
    if (e instanceof Reject) { totals.rejected++; log(`reject (${e.message}): kind=${kind}`); return; }
    totals.failed++; log(`unreadable: kind=${kind} ${e?.code ?? e?.message ?? e}`); return;
  }
  const sid = s.header.id;
  const cursor = readCursor(sid);
  const settled = cursor?.settled ?? new Set();
  const excluded = cursor?.excluded ?? new Set();
  const todo = s.entries.filter((e) => !settled.has(e.id) && !excluded.has(e.id));
  if (!todo.length) return;
  const cwd = typeof s.header.cwd === "string" ? s.header.cwd : "";
  const project = cwd ? await projectOf(cwd) : { key: path.basename(path.dirname(file)), root: "", source: "" };
  if (!project) {
    // an explicit capture-disabled project: recorded as excluded, never queued
    if (!opts.dryRun) saveCursor(sid, [], todo.map((e) => e.id));
    log(`excluded (capture off for the project): kind=${kind} session=${sid} entries=${todo.length}`);
    return;
  }
  let written = 0;
  for (let i = 0; i < todo.length; i += CHUNK_ENTRIES) {
    const chunk = todo.slice(i, i + CHUNK_ENTRIES);
    const events = buildEvents(chunk, { sessionId: sid, sessionFile: file, cwd, project, injectedText: "" });
    if (opts.dryRun) {
      for (const ev of events) process.stdout.write(JSON.stringify(ev) + "\n");
      written += events.length;
      continue;
    }
    if (!(await writeBatch(events)) ) { totals.failed++; log(`queue write failed: kind=${kind} session=${sid} chunk=${i / CHUNK_ENTRIES}`); break; }
    saveCursor(sid, chunk.map((e) => e.id), []);
    written += events.length;
  }
  if (written) { totals.sessions++; totals.events += written; log(`${opts.dryRun ? "would queue" : "queued"}: kind=${kind} session=${sid} project=${project.key} events=${written}`); }
}

// --- run -------------------------------------------------------------------
if (captureOff()) process.exit(0);
const started = Date.now();
const files = discover()
  .map((f) => { try { return { ...f, mtimeMs: fs.lstatSync(f.file).mtimeMs }; } catch { return null; } })
  .filter((f) => f && f.mtimeMs / 1000 >= opts.since)
  .sort((a, b) => a.mtimeMs - b.mtimeMs);
for (let i = 0; i < files.length; i++) {
  const f = files[i];
  if (opts.maxSeconds && (Date.now() - started) / 1000 >= opts.maxSeconds) { totals.stopped = 1; totals.left += files.length - i; break; }
  if (opts.quietFor && (Date.now() - f.mtimeMs) / 1000 < opts.quietFor) { totals.left++; continue; }   // still being written: later
  await sweepFile(f);
}
log(`done: files=${files.length} sessions=${totals.sessions} events=${totals.events} failed=${totals.failed} rejected=${totals.rejected} left=${totals.left}${totals.stopped ? " stopped=1" : ""}`);
if (opts.summary) process.stdout.write([totals.sessions, totals.events, totals.failed, totals.rejected, totals.stopped, totals.left, 0, "primary"].join("\t") + "\n");
process.exit(totals.failed ? 1 : 0);
