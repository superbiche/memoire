#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// memoire-capture-pi.mjs — host-side sweep of Pi sessions (store-sweep lane, like OpenCode's).
//
// The in-session extension (scripts/pi-extension/memoire.js) captures what a Pi session writes
// while it is loaded and can reach the queue. It misses what it cannot see or reach: a Conclave
// seat (its own Pi home, no Memoire mounts), a caged worker (the run directory), entries written
// before the extension loaded or after a failed write whose cursor was lost, sessions that died
// before their first settle. This sweep reads the session FILES on the host and queues what is
// missing, with the extension's own `buildEvents`, queue writer and scrub, so redaction stays at
// the queue-write gate (MEMOIRE_REDACT).
//
// TRUST. A seat or a caged worker is an agent that may be working on a client repository and can
// write its own store. Nothing it writes decides what is captured, under which project, or whose
// cursor moves:
//   * Attribution comes from the HOST. A caged worker: `<run dir>/state.json` (engine truth,
//     bound read-only inside the cage) names the workspace (`target.cage`) and the project
//     repository (`target.repo`); a seat: `<seat state>/cockpits/<seat>/<cockpit key>.json`
//     (written by the launcher outside everything the seat can write) names the cockpit. The
//     session header's cwd must lie inside that workspace; the project and the capture switch are
//     resolved from the host's path, never from the file's. No host record, or disagreement:
//     QUARANTINED (not queued, counted, named in the log, never settled; retried each run).
//   * The ordinary store (~/.pi/agent/sessions) is trusted as the live extension is, which already
//     runs with the user's rights there: the header's cwd is used, and must agree with the
//     directory key the session lies in (quarantine otherwise). A missing cwd is quarantined
//     everywhere: there is no capture-enabled fallback.
//   * Every byte comes from INSIDE the store: from a host-owned root, every component down to the file
//     is opened without following a link (descriptor-relative through /proc/self/fd, Linux), the
//     type and size are checked on the descriptor that is then read, line by line (memory bounded by
//     the longest permitted line and one chunk), discovery is bounded in depth and breadth.
//   * A store cannot speak for another store's session. Seat and cage sessions are bound to their
//     origin (kind, run id, or seat and cockpit) in the session id, the event key and the cursor
//     (`capture-cursors/pi-origin/`); only the ordinary store shares the extension's cursor and
//     plain keys. A header id claimed by two origins (or by two files of one origin) quarantines
//     every claimant, and is reported.
//
// Where each kind of session lives (read from the launchers and the floor code, 2026-11-14):
//   agent   ~/.pi/agent/sessions/<cwd key>/*.jsonl        ordinary, sandboxed, T3 Code, ACP
//           ($PI_CODING_AGENT_DIR/sessions when set)
//   seat    $LIAISON_PI_SEAT_STATE | ~/.local/share/liaison/pi-seat-state
//             /sessions/<seat>/<cockpit key>/<cwd key>/*.jsonl      Conclave coordinator seat
//   cage    <conclave runs>/<run id>/acpx/pi-sessions/<cwd key>/*.jsonl   caged worker
//           (runs: $MEMOIRE_PI_RUNS_DIR | ($LIAISON_CONCLAVE_HOME | ~/dev/conclave)/runs)
// Not swept, with the reason: the floor's probe stores and test homes (synthetic runs);
// `--no-session` runs (no file). $MEMOIRE_PI_SESSION_ROOTS / --root: ordinary-layout stores.
//
// Rules: a file changed within --quiet-for seconds is left for later; an incomplete last line is
// dropped; only a successful queue write settles. A file whose (size, mtime) equals the mark left by
// a complete earlier sweep is not read again, so there is no lookback window: a pending file is
// never stranded. Exclusion is its own rule (the cursor's `excluded` set, union across cursors);
// the cursor is updated under one lock shared with the extension. Accepted loss: an entry removed
// from a file before it was captured cannot be recovered, and an entry rewritten in place under the
// same id keeps the version first captured (the hub keeps one row per key).
//
// Usage: memoire-capture-pi.mjs [--scan | file.jsonl ...] [--root DIR]... [--since SPEC]
//          [--quiet-for S] [--max-seconds S] [--summary] [--dry-run] [--help]
//   --summary last stdout line: sessions<TAB>events<TAB>failed<TAB>rejected<TAB>stopped<TAB>left<TAB>blobs<TAB>source<TAB>quarantined
//   --dry-run print the events that would be queued as JSON lines on stdout; write nothing, no cursor
//
// Environment: MEMOIRE_HOME, MEMOIRE_QUEUE_BIN, MEMOIRE_RESOLVE_PROJECT, MEMOIRE_PI_SESSION_ROOTS,
//   MEMOIRE_PI_RUNS_DIR, LIAISON_PI_SEAT_STATE, LIAISON_CONCLAVE_HOME, PI_CODING_AGENT_DIR,
//   MEMOIRE_PI_EXTENSION_PATH (the extension file to import; tests).

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import crypto from "node:crypto";
import { pathToFileURL } from "node:url";

const here = path.dirname(new URL(import.meta.url).pathname);
const extPath = process.env.MEMOIRE_PI_EXTENSION_PATH
  ? path.resolve(process.env.MEMOIRE_PI_EXTENSION_PATH)
  : path.resolve(here, "pi-extension/memoire.js");
const ext = await import(pathToFileURL(extPath).href);
const { buildEvents, readCursor, readCursorFile, updateCursor, updateCursorAt, cursorPath, resolveProject, writeBatch, captureOff } = ext;

const MAX_FILE_BYTES = 256 * 1024 * 1024;
const MAX_LINE_BYTES = 16 * 1024 * 1024;
const CHUNK_ENTRIES = 400;
const CHUNK_BYTES = 32 * 1024 * 1024;
const MAX_DEPTH = 5;
const MAX_DIR_ENTRIES = 20000;
const MAX_FILES = 50000;
const log = (m) => process.stderr.write(`memoire-capture-pi: ${m}\n`);
const home = os.homedir();
const memoireHome = () => process.env.MEMOIRE_HOME || path.join(home, ".memoire");

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

// --- descriptor-relative, no-follow access (Linux) --------------------------------------------
const C = fs.constants;
const RDONLY_NOFOLLOW = C.O_RDONLY | C.O_NOFOLLOW | (C.O_CLOEXEC ?? 0);
const procFd = (fd) => `/proc/self/fd/${fd}`;
const okName = (n) => typeof n === "string" && n !== "" && n !== "." && n !== ".." && !n.includes("/") && !n.includes("\0");

// A host-owned root: its last component is not followed (its ancestors are the host's own).
function openRoot(dir) { return fs.openSync(dir, RDONLY_NOFOLLOW | C.O_DIRECTORY); }
// A child of an open directory, by single name, link not followed, relative to the descriptor.
function openChild(dirFd, name, extra = 0) {
  if (!okName(name)) throw Object.assign(new Error("bad name"), { code: "EBADNAME" });
  return fs.openSync(`${procFd(dirFd)}/${name}`, RDONLY_NOFOLLOW | extra);
}
const closeQuiet = (fd) => { if (fd >= 0) try { fs.closeSync(fd); } catch { /* */ } };
// Open root/parts[0]/…/parts[n-1]; every component through the previous descriptor, none followed.
// Returns the LAST descriptor (the file or directory) with the intermediates closed.
function openPath(rootDir, parts, lastIsDir = false) {
  let fd = openRoot(rootDir);
  try {
    for (let i = 0; i < parts.length; i++) {
      const last = i === parts.length - 1;
      const next = openChild(fd, parts[i], last && !lastIsDir ? 0 : C.O_DIRECTORY);
      closeQuiet(fd); fd = next;
    }
    return fd;
  } catch (e) { closeQuiet(fd); throw e; }
}
function listDir(fd) {
  try { return fs.readdirSync(procFd(fd)).filter(okName).slice(0, MAX_DIR_ENTRIES); } catch { return []; }
}
function fstatOf(fd) { return fs.fstatSync(fd); }

// Lines of an open descriptor, one at a time, memory bounded by MAX_LINE_BYTES. {text, complete}.
function* lines(fd, size) {
  const buf = Buffer.allocUnsafe(1 << 16);
  let pos = 0, carry = [], carryLen = 0;
  while (pos < size) {
    const n = fs.readSync(fd, buf, 0, Math.min(buf.length, size - pos), pos);
    if (n <= 0) break;
    pos += n;
    let start = 0;
    for (let i = 0; i < n; i++) {
      if (buf[i] !== 10) continue;
      const piece = buf.subarray(start, i);
      carryLen += piece.length;
      if (carryLen > MAX_LINE_BYTES) throw new Reject("line too long");
      carry.push(Buffer.from(piece));
      yield { text: Buffer.concat(carry).toString("utf8"), complete: true };
      carry = []; carryLen = 0; start = i + 1;
    }
    if (start < n) {
      carryLen += n - start;
      if (carryLen > MAX_LINE_BYTES) throw new Reject("line too long");
      carry.push(Buffer.from(buf.subarray(start, n)));
    }
  }
  if (carryLen) yield { text: Buffer.concat(carry).toString("utf8"), complete: false };
}

class Reject extends Error {}

// --- stores ----------------------------------------------------------------------------------
const dirKey = (cwd) => "--" + String(cwd).replace(/^[\\/]+/, "").replace(/[\\/:]/g, "-") + "--";
const inside = (cwd, base) => typeof cwd === "string" && typeof base === "string" && base !== ""
  && (path.resolve(cwd) === path.resolve(base) || path.resolve(cwd).startsWith(path.resolve(base) + path.sep));
const ownRoot = (p) => { try { return fs.lstatSync(p).isDirectory() && !fs.lstatSync(p).isSymbolicLink(); } catch { return false; } };

// Each file is {kind, root, parts, origin} : openable by openPath(root, parts).
function discover() {
  const out = [];
  const add = (f) => { if (out.length < MAX_FILES) out.push(f); };
  const agentLayout = (kind, root) => {
    if (!ownRoot(root)) return;
    let rfd = -1;
    try {
      rfd = openRoot(root);
      for (const key of listDir(rfd)) {
        let dfd = -1;
        try {
          dfd = openChild(rfd, key, C.O_DIRECTORY);
          for (const name of listDir(dfd)) if (name.endsWith(".jsonl")) add({ kind, root, parts: [key, name], origin: { kind: "agent" } });
        } catch { /* a link, not a directory, vanished: skipped */ } finally { closeQuiet(dfd); }
      }
    } catch { /* unreadable root */ } finally { closeQuiet(rfd); }
  };
  if (opts.files.length) {
    for (const f of opts.files) {
      const abs = path.resolve(f);
      add({ kind: "custom", root: path.dirname(path.dirname(abs)), parts: [path.basename(path.dirname(abs)), path.basename(abs)], origin: { kind: "agent" } });
    }
    return out;
  }
  const explicit = opts.roots.length ? opts.roots : (process.env.MEMOIRE_PI_SESSION_ROOTS || "").split(":").filter(Boolean);
  if (explicit.length) { for (const r of explicit) agentLayout("custom", r); return out; }
  agentLayout("agent", process.env.PI_CODING_AGENT_DIR ? path.join(process.env.PI_CODING_AGENT_DIR, "sessions") : path.join(home, ".pi/agent/sessions"));
  // seat: <state>/sessions/<seat>/<cockpit key>/<cwd key>/*.jsonl
  const state = process.env.LIAISON_PI_SEAT_STATE || path.join(home, ".local/share/liaison/pi-seat-state");
  if (ownRoot(state)) {
    let sfd = -1;
    try {
      sfd = openRoot(state);
      let sessionsFd = -1;
      try {
        sessionsFd = openChild(sfd, "sessions", C.O_DIRECTORY);
        for (const seat of listDir(sessionsFd)) {
          let seatFd = -1;
          try {
            seatFd = openChild(sessionsFd, seat, C.O_DIRECTORY);
            for (const key of listDir(seatFd)) {
              let kfd = -1;
              try {
                kfd = openChild(seatFd, key, C.O_DIRECTORY);
                for (const cwdKey of listDir(kfd)) {
                  let cfd = -1;
                  try {
                    cfd = openChild(kfd, cwdKey, C.O_DIRECTORY);
                    for (const name of listDir(cfd)) if (name.endsWith(".jsonl")) add({ kind: "seat", root: state, parts: ["sessions", seat, key, cwdKey, name], origin: { kind: "seat", seat, key, id: `seat:${seat}:${key}` } });
                  } catch { /* */ } finally { closeQuiet(cfd); }
                }
              } catch { /* */ } finally { closeQuiet(kfd); }
            }
          } catch { /* */ } finally { closeQuiet(seatFd); }
        }
      } catch { /* no sessions directory */ } finally { closeQuiet(sessionsFd); }
    } catch { /* unreadable state */ } finally { closeQuiet(sfd); }
  }
  // cage: <runs>/<run>/acpx/pi-sessions/<cwd key>/*.jsonl
  const runs = process.env.MEMOIRE_PI_RUNS_DIR || path.join(process.env.LIAISON_CONCLAVE_HOME || path.join(home, "dev/conclave"), "runs");
  if (ownRoot(runs)) {
    let rfd = -1;
    try {
      rfd = openRoot(runs);
      for (const run of listDir(rfd)) {
        let runFd = -1, acpxFd = -1, psFd = -1;
        try {
          runFd = openChild(rfd, run, C.O_DIRECTORY);
          acpxFd = openChild(runFd, "acpx", C.O_DIRECTORY);       // worker-writable: opened without following
          psFd = openChild(acpxFd, "pi-sessions", C.O_DIRECTORY);
          for (const cwdKey of listDir(psFd)) {
            let cfd = -1;
            try {
              cfd = openChild(psFd, cwdKey, C.O_DIRECTORY);
              for (const name of listDir(cfd)) if (name.endsWith(".jsonl")) add({ kind: "cage", root: runs, parts: [run, "acpx", "pi-sessions", cwdKey, name], origin: { kind: "cage", run, id: `cage:${run}` } });
            } catch { /* */ } finally { closeQuiet(cfd); }
          }
        } catch { /* a run without a Pi store, or with a link where a directory belongs */ } finally { closeQuiet(psFd); closeQuiet(acpxFd); closeQuiet(runFd); }
      }
    } catch { /* unreadable runs dir */ } finally { closeQuiet(rfd); }
  }
  return out;
}

// --- host records ----------------------------------------------------------------------------
function readHostJson(root, parts) {
  let fd = -1;
  try {
    fd = openPath(root, parts);
    const st = fstatOf(fd);
    if (!st.isFile() || st.size > 1 << 20) return null;
    return JSON.parse(fs.readFileSync(fd, "utf8"));
  } catch { return null; } finally { closeQuiet(fd); }
}

const projects = new Map();   // cwd -> project | null, per run
async function projectOf(cwd) {
  if (!projects.has(cwd)) projects.set(cwd, await resolveProject(cwd));
  return projects.get(cwd);
}

// {ok, cwd, project|null (capture-disabled), ...} or {quarantine: reason}
async function attribute(file, header) {
  const cwd = header.cwd;
  if (typeof cwd !== "string" || !path.isAbsolute(cwd)) return { quarantine: "no usable cwd in the header" };
  const o = file.origin;
  if (o.kind === "agent") {
    if (file.parts[0] !== dirKey(cwd)) return { quarantine: "header cwd disagrees with the directory key" };
    const p = await projectOf(cwd);
    return { cwd, project: p, excluded: !p };
  }
  if (o.kind === "seat") {
    const rec = readHostJson(file.root, ["cockpits", o.seat, `${o.key}.json`]);
    if (!rec || rec.seat !== o.seat || typeof rec.cockpit !== "string") return { quarantine: "no host launch record for this seat and cockpit" };
    if (!inside(cwd, rec.cockpit) || file.parts[3] !== dirKey(cwd)) return { quarantine: "header cwd is not the cockpit the launcher recorded" };
    const p = await projectOf(rec.cockpit);
    return { cwd, project: p, excluded: !p };
  }
  if (o.kind === "cage") {
    const rec = readHostJson(file.root, [o.run, "state.json"]);
    const t = rec && typeof rec === "object" ? rec.target : null;
    if (!t || typeof t.cage !== "string" || typeof t.repo !== "string") return { quarantine: "no host run record for this run" };
    if (!inside(cwd, t.cage) || file.parts[3] !== dirKey(cwd)) return { quarantine: "header cwd is not the workspace the run record names" };
    const pc = await projectOf(t.cage);
    const pr = await projectOf(t.repo);
    return { cwd, project: pc && pr ? pr : null, excluded: !pc || !pr };
  }
  return { quarantine: "unknown origin" };
}

// --- cursors ---------------------------------------------------------------------------------
const sha = (s) => crypto.createHash("sha256").update(s).digest("hex").slice(0, 40);
// Identity of a session in the hub and in cursors: plain for the ordinary store (shared with the
// extension), bound to its origin for seat and cage stores.
const nsId = (origin, headerId) => (origin.kind === "agent" ? headerId : `${origin.id}:${headerId}`);
const originCursorFile = (ns) => path.join(memoireHome(), "capture-cursors", "pi-origin", `${sha(ns)}.json`);
function cursorOps(origin, ns) {
  if (origin.kind === "agent") {
    // the merged view of both cursors for the sets; the sweep mark lives in the current cursor file
    return {
      read: () => {
        const merged = readCursor(ns); const own = readCursorFile(cursorPath(ns));
        return merged || own ? { settled: merged?.settled ?? own.settled, excluded: merged?.excluded ?? own.excluded, extra: own?.extra ?? {} } : null;
      },
      update: (m) => updateCursor(ns, m),
    };
  }
  const file = originCursorFile(ns);
  return { read: () => readCursorFile(file), update: (m) => updateCursorAt(file, m) };
}

// Migration: once the current cursor holds everything the older pi-capture/ cursor held, drop the
// older one (it would otherwise keep limiting the intersection forever). Host side only.
function migrateLegacy(ns) {
  const legacy = path.join(memoireHome(), "pi-capture", path.basename(cursorPath(ns)));
  try {
    const old = readCursorFile(legacy);
    const own = readCursorFile(cursorPath(ns));
    if (!old || !own) return;
    if ([...old.settled].every((x) => own.settled.has(x)) && [...old.excluded].every((x) => own.excluded.has(x))) fs.rmSync(legacy, { force: true });
  } catch { /* no older cursor */ }
}

// --- one file --------------------------------------------------------------------------------
const totals = { sessions: 0, events: 0, failed: 0, rejected: 0, stopped: 0, left: 0, quarantined: 0 };
const quarantine = (f, why) => { totals.quarantined++; log(`quarantine: kind=${f.origin.kind} origin=${f.origin.id ?? "agent"} file=${f.parts[f.parts.length - 1]} reason=${why}`); };

function readHeader(fd, size) {
  const it = lines(fd, size);
  const first = it.next();
  it.return?.();
  if (first.done) throw new Reject("empty");
  let header;
  try { header = JSON.parse(first.value.text); } catch { throw new Reject("unparseable header"); }
  if (!header || header.type !== "session" || typeof header.id !== "string" || !header.id) throw new Reject("no session header");
  if (header.version !== 3) throw new Reject(`unsupported version ${header.version}`);
  return header;
}

// First pass over the files: header ids by origin, to find claims of one id by more than one file.
function openFile(f) { return openPath(f.root, f.parts); }
function claims(files) {
  const byId = new Map();
  for (const f of files) {
    let fd = -1;
    try {
      fd = openFile(f);
      const st = fstatOf(fd);
      if (!st.isFile() || st.size > MAX_FILE_BYTES) continue;
      f.stat = st;
      f.header = readHeader(fd, Math.min(st.size, MAX_LINE_BYTES));
      const list = byId.get(f.header.id) ?? [];
      list.push(f); byId.set(f.header.id, list);
    } catch (e) { f.headerError = e instanceof Reject ? e.message : (e?.code ?? "unreadable"); } finally { closeQuiet(fd); }
  }
  for (const list of byId.values()) if (list.length > 1) for (const f of list) f.conflict = list.length;
}

async function sweepFile(f) {
  let fd = -1;
  try {
    fd = openFile(f);                              // every component opened without following a link
    const st = fstatOf(fd);                        // checked on the descriptor that is read
    if (!st.isFile()) throw new Reject("not a regular file");
    if (st.size > MAX_FILE_BYTES) throw new Reject("over 256 MiB");
    const header = readHeader(fd, st.size);
    const att = await attribute(f, header);
    if (att.quarantine) { quarantine(f, att.quarantine); return; }
    if (f.conflict) { quarantine(f, `header id claimed by ${f.conflict} files`); return; }
    const ns = nsId(f.origin, header.id);
    const ops = cursorOps(f.origin, ns);
    const cur = ops.read();
    const swept = cur?.extra?.swept;
    const mark = { size: st.size, mtimeMs: st.mtimeMs };
    if (cur && swept && swept.size === mark.size && swept.mtimeMs === mark.mtimeMs) return;   // complete earlier sweep, unchanged since
    const settled = cur?.settled ?? new Set();
    const excluded = cur?.excluded ?? new Set();
    const label = f.origin.kind;
    let chunk = [], chunkBytes = 0, written = 0, failedHere = false;
    const flush = async () => {
      if (!chunk.length) return true;
      const ids = chunk.map((e) => e.id);
      if (att.excluded) {
        if (!opts.dryRun) ops.update((c) => { for (const id of ids) c.excluded.add(id); });
        log(`excluded (capture off for the project): kind=${label} session=${ns} entries=${ids.length}`);
      } else {
        const events = buildEvents(chunk, { sessionId: ns, sessionFile: path.join(f.root, ...f.parts), cwd: att.cwd, project: att.project, injectedText: "" });
        if (f.origin.kind !== "agent") for (const ev of events) ev.payload = { ...ev.payload, origin: { kind: f.origin.kind, id: f.origin.id, session: header.id } };
        if (opts.dryRun) {
          for (const ev of events) process.stdout.write(JSON.stringify(ev) + "\n");
          written += events.length;
        } else {
          if (!(await writeBatch(events))) { totals.failed++; failedHere = true; log(`queue write failed: kind=${label} session=${ns}`); return false; }
          if (!ops.update((c) => { for (const id of ids) c.settled.add(id); })) { totals.failed++; failedHere = true; log(`cursor update failed: kind=${label} session=${ns}`); return false; }
          written += events.length;
        }
      }
      chunk = []; chunkBytes = 0;
      return true;
    };
    const seen = new Set();
    let first = true;
    const take = async (raw) => {
      let e;
      try { e = JSON.parse(raw.text); } catch {
        if (!raw.complete) return "drop";          // an incomplete last line: the session is still being written
        throw new Reject("unparseable entry");
      }
      if (!e || typeof e !== "object" || typeof e.id !== "string" || !e.id) throw new Reject("entry without an id");
      if (seen.has(e.id)) return "ok";
      seen.add(e.id);
      if (settled.has(e.id) || excluded.has(e.id)) return "ok";
      chunk.push(e); chunkBytes += raw.text.length;
      if (chunk.length >= CHUNK_ENTRIES || chunkBytes >= CHUNK_BYTES) { if (!(await flush())) return "stop"; }
      return "ok";
    };
    for (const raw of lines(fd, st.size)) {
      if (first) { first = false; continue; }      // the header, read above
      const r = await take(raw);
      if (r === "stop") break;
    }
    if (!failedHere) await flush();
    if (written) { totals.sessions++; totals.events += written; log(`${opts.dryRun ? "would queue" : "queued"}: kind=${label} session=${ns} project=${att.project?.key ?? "-"} events=${written}`); }
    if (!failedHere && !opts.dryRun) {
      ops.update((c) => { c.extra = { ...c.extra, swept: mark }; });
      if (f.origin.kind === "agent") migrateLegacy(ns);
    }
  } catch (e) {
    if (e instanceof Reject) { totals.rejected++; log(`reject (${e.message}): kind=${f.origin.kind} file=${f.parts[f.parts.length - 1]}`); }
    else { totals.failed++; log(`unreadable: kind=${f.origin.kind} ${e?.code ?? e?.message ?? e}`); }
  } finally { closeQuiet(fd); }
}

// --- run -------------------------------------------------------------------
if (captureOff()) process.exit(0);
const started = Date.now();
const found = discover();
claims(found);
const files = found
  .map((f) => (f.stat ? f : (() => { let fd = -1; try { fd = openFile(f); f.stat = fstatOf(fd); return f; } catch { return null; } finally { closeQuiet(fd); } })()))
  .filter((f) => f && f.stat.mtimeMs / 1000 >= opts.since)
  .sort((a, b) => a.stat.mtimeMs - b.stat.mtimeMs);
// A test seam for the swap-between-inspection-and-read case: wait here until the file appears.
if (process.env.MEMOIRE_PI_TEST_PAUSE) { const go = process.env.MEMOIRE_PI_TEST_PAUSE; fs.writeFileSync(`${go}.ready`, ""); while (!fs.existsSync(`${go}.go`)) Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 20); }
for (let i = 0; i < files.length; i++) {
  const f = files[i];
  if (opts.maxSeconds && (Date.now() - started) / 1000 >= opts.maxSeconds) { totals.stopped = 1; totals.left += files.length - i; break; }
  if (opts.quietFor && (Date.now() - f.stat.mtimeMs) / 1000 < opts.quietFor) { totals.left++; continue; }
  if (f.headerError) { totals.rejected++; log(`reject (${f.headerError}): kind=${f.origin.kind} file=${f.parts[f.parts.length - 1]}`); continue; }
  await sweepFile(f);
}
log(`done: files=${files.length} sessions=${totals.sessions} events=${totals.events} failed=${totals.failed} rejected=${totals.rejected} quarantined=${totals.quarantined} left=${totals.left}${totals.stopped ? " stopped=1" : ""}`);
if (opts.summary) process.stdout.write([totals.sessions, totals.events, totals.failed, totals.rejected, totals.stopped, totals.left, 0, "primary", totals.quarantined].join("\t") + "\n");
process.exit(totals.failed ? 1 : 0);
