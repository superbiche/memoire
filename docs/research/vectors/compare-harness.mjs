// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// D183 — comparison harness (KILL-CRITERIA §4). Runs the frozen
// reference set verbatim against both read surfaces:
//   FTS:    GET  $MEMOIRE_BASE_URL/v1/search  (include_proposed=true —
//           the real MCP surface; project/limit exactly as issued)
//   hybrid, one of two modes:
//     pilot  (default): POST $PILOT_BASE/agentmemory/smart-search  (raw
//            query string, includeLessons:false; project passed but the
//            engine does not scope results by it — engine behavior,
//            judged as-is)
//     native (HYBRID_NATIVE=1, D218 acceptance gate): the SAME
//            $MEMOIRE_BASE_URL/v1/search call as the FTS lane plus
//            hybrid=1 — the real client path with RRF fusion forced on
//            per-request, so the flag can stay off while measuring.
// Per query per engine: 1 warm-up + ITER timed calls (client-measured);
// the final call's body is kept as the result list. Errors and empty
// results are recorded as-is (syntax honesty). Output: one raw JSON.
// Usage:
//   MEMOIRE_TOKEN_FILE=... node compare-harness.mjs > compare-raw.json
//   MEMOIRE_TOKEN_FILE=... HYBRID_NATIVE=1 node compare-harness.mjs > compare-raw.json
import { createInterface } from "node:readline";
import { createReadStream, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const BASE = process.env.MEMOIRE_BASE_URL || "https://memoire.superbiche.me";
const PILOT = process.env.PILOT_BASE || "http://127.0.0.1:3111";
const ITER = Number(process.env.ITER || 10);
const TOKEN = readFileSync(process.env.MEMOIRE_TOKEN_FILE, "utf8").trim();

async function timedCall(fn) {
  const t0 = performance.now();
  try {
    const res = await fn();
    const body = await res.json().catch(() => null);
    return { ms: Math.round(performance.now() - t0), http: res.status, body };
  } catch (e) {
    return { ms: Math.round(performance.now() - t0), http: 0, error: String(e).slice(0, 200) };
  }
}

const NATIVE = process.env.HYBRID_NATIVE === "1";

function ftsCall(q) {
  const u = new URL(`${BASE}/v1/search`);
  u.searchParams.set("q", q.query);
  u.searchParams.set("include_proposed", "true");
  // Native mode: pin the control lane to FTS explicitly — once the
  // deployment default flips on, an unpinned control would silently
  // compare hybrid against hybrid (round-1 F6).
  if (NATIVE) u.searchParams.set("hybrid", "0");
  if (q.project) u.searchParams.set("project", q.project);
  if (q.limit) u.searchParams.set("limit", q.limit);
  return fetch(u, {
    headers: { authorization: `Bearer ${TOKEN}` },
    signal: AbortSignal.timeout(15000),
  });
}

function hybridCall(q) {
  if (NATIVE) {
    const u = new URL(`${BASE}/v1/search`);
    u.searchParams.set("q", q.query);
    u.searchParams.set("include_proposed", "true");
    u.searchParams.set("hybrid", "1");
    if (q.project) u.searchParams.set("project", q.project);
    if (q.limit) u.searchParams.set("limit", q.limit);
    return fetch(u, {
      headers: { authorization: `Bearer ${TOKEN}` },
      signal: AbortSignal.timeout(15000),
    });
  }
  const payload = { query: q.query, includeLessons: false };
  if (q.project) payload.project = q.project;
  if (q.limit) payload.limit = Number(q.limit);
  return fetch(`${PILOT}/agentmemory/smart-search`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(payload),
    signal: AbortSignal.timeout(15000),
  });
}

const tsv = join(dirname(fileURLToPath(import.meta.url)), "reference-queries.tsv");
const queries = [];
const rl = createInterface({ input: createReadStream(tsv), crlfDelay: Infinity });
for await (const line of rl) {
  const [id, category, gt, , , target, limit, query] = line.split("\t");
  if (id === "id" || !query) continue;
  queries.push({
    id: Number(id),
    category,
    gt,
    project: target !== "-" ? target : null,
    limit: limit !== "-" ? limit : null,
    query,
  });
}

const out = [];
for (const q of queries) {
  const rec = { ...q, engines: {} };
  for (const [name, call] of [["fts", ftsCall], ["hybrid", hybridCall]]) {
    await timedCall(() => call(q)); // warm-up, untimed
    const timings = [];
    let last = null;
    for (let i = 0; i < ITER; i++) {
      last = await timedCall(() => call(q));
      timings.push({ ms: last.ms, http: last.http });
    }
    const hits =
      name === "fts" || NATIVE
        ? (last.body?.hits ?? []).map((h) => ({
            id: String(h.id),
            kind: h.kind,
            project: h.project,
            date: h.captured_at_epoch,
            score: h.rank,
            text: h.text,
          }))
        : (last.body?.results ?? []).map((r) => ({
            id: String(r.obsId).replace(/^memoire-/, ""),
            kind: r.type,
            date: r.timestamp,
            score: r.score,
            title: r.title,
          }));
    rec.engines[name] = {
      timings,
      http: last.http,
      error: last.error ?? last.body?.error ?? null,
      hits,
    };
    console.error(
      `#${q.id} ${name}: http=${last.http} hits=${hits.length} ms=[${timings.map((t) => t.ms).join(",")}]`,
    );
  }
  out.push(rec);
}

function pct(sorted, p) {
  return sorted[Math.min(sorted.length - 1, Math.ceil((p / 100) * sorted.length) - 1)];
}
const summary = {};
for (const name of ["fts", "hybrid"]) {
  const all = out
    .flatMap((r) => r.engines[name].timings)
    .filter((t) => t.http === 200)
    .map((t) => t.ms)
    .sort((a, b) => a - b);
  summary[name] = {
    calls: all.length,
    p50: pct(all, 50),
    p90: pct(all, 90),
    p95: pct(all, 95),
    p99: pct(all, 99),
    max: all[all.length - 1],
  };
}
console.error("latency summary:", JSON.stringify(summary));
console.log(JSON.stringify({ ranAt: "see-filename", iter: ITER, base: BASE, pilot: NATIVE ? "native-hybrid" : PILOT, summary, queries: out }, null, 1));
