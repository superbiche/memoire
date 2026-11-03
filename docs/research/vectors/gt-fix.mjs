// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// D183 — fix recall ground truth for the 7 derivable queries BEFORE
// judging (KILL-CRITERIA §4). Literal ticket-reference match over the
// projection snapshot (the exact /v1/search corpus at export time),
// scoped by each query's issued project filter. Emits one row per GT
// observation with a derived/raw flag so recall can be computed against
// full-corpus GT and derived-only GT (corpus-asymmetry control).
// Usage: node gt-fix.mjs <projection.jsonl> > ground-truth-<date>.tsv
import { createInterface } from "node:readline";
import { createReadStream } from "node:fs";

// (?!\d) guards #54 against #540; literals are case-exact for ticket ids.
// #2: every US-9299 reference lives in beta-project-vacation — none in the
// query's issued project filter (beta-project-valet). GT is therefore
// UNSCOPED (the ratified law: a hit iff it references the ticket) and
// flagged for an operator ruling on whether the mis-scoped filter makes
// the query drop out of recall arithmetic instead.
// #22 (decision-recall): candidates = decision-kind rows mentioning
// GlitchTip in verdict/decommission/Sentry context; operator prunes to
// the actual verdict decision(s) before scoring.
const GT_QUERIES = [
  { id: 2, project: null, re: /US-9299(?!\d)/, spotCheck: true },
  { id: 7, project: "beta-project-hostrider", re: /US-8666(?!\d)/, spotCheck: false },
  { id: 9, project: null, re: /US-9228(?!\d)/, spotCheck: false },
  { id: 15, project: null, re: /US-9248(?!\d)/, spotCheck: false },
  { id: 17, project: "beta-project-vacation", re: /US-9248(?!\d)/, spotCheck: false },
  { id: 22, project: null, kind: "decision", re: /glitchtip/i, re2: /verdict|decommission|sentry/i, spotCheck: true },
  { id: 29, project: "agentrh-apps", re: /(#|t[âa]che |task )54(?!\d)/i, spotCheck: true },
];

const rl = createInterface({
  input: createReadStream(process.argv[2]),
  crlfDelay: Infinity,
});
const out = [];
for await (const line of rl) {
  if (!line.trim()) continue;
  const row = JSON.parse(line);
  for (const q of GT_QUERIES) {
    if (q.project && row.project !== q.project) continue;
    if (q.kind && row.kind !== q.kind) continue;
    if (!q.re.test(row.text)) continue;
    if (q.re2 && !q.re2.test(row.text)) continue;
    out.push([
      q.id,
      row.id,
      row.kind,
      row.kind === "raw" ? "raw" : "derived",
      row.project,
      row.review_state,
      new Date(row.occurred_at_epoch * 1000).toISOString().slice(0, 10),
      q.spotCheck ? "SPOT-CHECK" : "ok",
      row.text.replace(/\s+/g, " ").slice(0, 160),
    ]);
  }
}
out.sort((a, b) => a[0] - b[0] || a[1] - b[1]);
console.log(
  ["query_id", "obs_id", "kind", "corpus_lane", "project", "review_state", "date", "status", "excerpt"].join("\t"),
);
for (const r of out) console.log(r.join("\t"));
const counts = {};
for (const r of out) {
  const k = r[0];
  counts[k] ??= { full: 0, derived: 0 };
  counts[k].full++;
  if (r[3] === "derived") counts[k].derived++;
}
console.error("GT sizes (full/derived):", JSON.stringify(counts));
