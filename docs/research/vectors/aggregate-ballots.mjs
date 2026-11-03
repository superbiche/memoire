// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// D183 — ballot aggregation. Reads panel ballots (JSON arrays of
// {id, preference: A|B|tie, reason}), unblinds via the key, and computes
// criterion-2 arithmetic on the scored blind queries (hybrid must win
// >= 60% of comparisons where a preference is stated; ties/abstentions
// leave the denominator). Majority ballot = per-query majority across
// judges (tie-broken to "tie").
// Usage: node aggregate-ballots.mjs <key.json> <queries.tsv> <ballot.json...>
import { readFileSync } from "node:fs";
import { basename } from "node:path";

const [keyPath, tsvPath, ...ballotPaths] = process.argv.slice(2);
const key = JSON.parse(readFileSync(keyPath, "utf8")).queries;

const lanes = {};
for (const line of readFileSync(tsvPath, "utf8").split("\n").slice(1)) {
  if (!line.trim()) continue;
  const [id, , gt] = line.split("\t");
  lanes[id] = id === "11" ? "unscored" : gt === "recall" ? "recall" : "blind";
}

function parseBallot(path) {
  const txt = readFileSync(path, "utf8");
  const m = txt.match(/\[[\s\S]*\]/);
  const arr = JSON.parse(m ? m[0] : txt);
  const byId = {};
  for (const b of arr) byId[b.id] = { pref: b.preference, reason: b.reason };
  return byId;
}

const ballots = ballotPaths.map((p) => ({ name: basename(p, ".json"), votes: parseBallot(p) }));

function toEngine(qid, pref) {
  if (pref === "tie" || pref == null) return "tie";
  return key[qid][pref];
}

// Per-query table with per-judge engine votes + majority.
const rows = [];
for (const qid of Object.keys(key).sort((a, b) => a - b)) {
  const votes = ballots.map((b) => toEngine(qid, b.votes[qid]?.pref));
  const counts = { fts: 0, hybrid: 0, tie: 0 };
  for (const v of votes) counts[v]++;
  const majority =
    counts.hybrid > counts.fts && counts.hybrid > counts.tie
      ? "hybrid"
      : counts.fts > counts.hybrid && counts.fts > counts.tie
        ? "fts"
        : counts.fts === counts.hybrid && counts.fts > 0
          ? "tie"
          : counts.tie >= Math.max(counts.fts, counts.hybrid)
            ? "tie"
            : "tie";
  rows.push({ qid, lane: lanes[qid], votes, majority });
}

function crit2(pick) {
  let h = 0, f = 0, t = 0;
  for (const r of rows) {
    if (r.lane !== "blind") continue;
    const v = pick(r);
    if (v === "hybrid") h++;
    else if (v === "fts") f++;
    else t++;
  }
  const denom = h + f;
  return { hybrid: h, fts: f, tie: t, denom, hybridPct: denom ? Math.round((100 * h) / denom) : null };
}

console.log(`judges: ${ballots.map((b) => b.name).join(", ")}`);
console.log(`\nq\tlane\t${ballots.map((b) => b.name).join("\t")}\tmajority`);
for (const r of rows) console.log(`#${r.qid}\t${r.lane}\t${r.votes.join("\t")}\t${r.majority}`);

console.log(`\ncriterion 2 (21 blind queries; kill line: hybrid < 60% of stated preferences):`);
console.log(`  majority ballot: ${JSON.stringify(crit2((r) => r.majority))}`);
for (let i = 0; i < ballots.length; i++)
  console.log(`  ${ballots[i].name}: ${JSON.stringify(crit2((r) => r.votes[i]))}`);

console.log(`\nall 29 (context only):`);
const all = { fts: 0, hybrid: 0, tie: 0 };
for (const r of rows) all[r.majority]++;
console.log(`  majority: ${JSON.stringify(all)}`);
