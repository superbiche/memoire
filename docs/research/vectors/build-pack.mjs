// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// D183 — blinded judging pack builder. Consumes the compare-harness raw
// JSON + the projection snapshot + the fixed GT tsv and emits:
//   pack.md   — side-by-side top-10 lists per query, engine identity
//               hidden behind a per-query random List A/List B flip,
//               UNIFORM rendering (kind · project · date · excerpt
//               resolved from the projection snapshot for BOTH sides —
//               no engine-native snippet formats to leak identity).
//   key.json  — per-query side assignment + ranked observation ids.
//               DO NOT OPEN before judging is done.
//   recall.md — recall@10 table on the GT queries, full-corpus GT and
//               derived-only GT variants. Objective arithmetic, but read
//               it AFTER judging the pack (it names engines).
// Usage: node build-pack.mjs <raw.json> <projection.jsonl> <gt.tsv> <outdir>
import { createInterface } from "node:readline";
import { createReadStream, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { randomInt } from "node:crypto";

const [rawPath, projPath, gtPath, outDir] = process.argv.slice(2);
const raw = JSON.parse(readFileSync(rawPath, "utf8"));

// GT: query_id -> { full:Set, derived:Set }
const gt = {};
for (const line of readFileSync(gtPath, "utf8").split("\n").slice(1)) {
  if (!line.trim()) continue;
  const [qid, obsId, , lane] = line.split("\t");
  gt[qid] ??= { full: new Set(), derived: new Set() };
  gt[qid].full.add(obsId);
  if (lane === "derived") gt[qid].derived.add(obsId);
}

// Resolve every hit id against the projection snapshot (two-pass stream).
const needed = new Set();
for (const q of raw.queries)
  for (const e of Object.values(q.engines)) for (const h of e.hits) needed.add(h.id);
const resolved = new Map();
const rl = createInterface({ input: createReadStream(projPath), crlfDelay: Infinity });
for await (const line of rl) {
  if (!line.trim()) continue;
  const r = JSON.parse(line);
  const id = String(r.id);
  if (needed.has(id))
    resolved.set(id, {
      kind: r.kind,
      project: r.project,
      date: new Date(r.occurred_at_epoch * 1000).toISOString().slice(0, 10),
      text: r.text,
    });
}

function renderItem(h, i) {
  const r = resolved.get(h.id);
  // Post-snapshot rows (possible on the live FTS side only) fall back to
  // the fields the engine returned; rendering shape stays identical.
  const kind = r?.kind ?? h.kind ?? "?";
  const project = r?.project ?? h.project ?? "?";
  const date =
    r?.date ??
    (typeof h.date === "number"
      ? new Date(h.date * 1000).toISOString().slice(0, 10)
      : String(h.date ?? "?").slice(0, 10));
  const text = (r?.text ?? h.text ?? h.title ?? "").replace(/\s+/g, " ");
  const excerpt = text.length > 400 ? text.slice(0, 400) + "…" : text;
  return `${i + 1}. \`${kind} · ${project} · ${date}\` ${excerpt}`;
}

const scoredBlind = (q) => q.gt === "blind";
const pack = [];
const key = { note: "DO NOT OPEN before the pack is judged.", queries: {} };

// NATIVE mode (D218 acceptance gate): both engines are the SAME hub API
// over the SAME transport and corpus — the pilot pack's asymmetry
// caveats would misinform judges, so the header switches with the mode.
// STAMP names the run's artifacts (defaults to the pilot date so the
// 2026-08-02 artifacts stay reproducible byte-for-byte).
const NATIVE = process.env.HYBRID_NATIVE === "1";
const STAMP = process.env.PACK_STAMP || "2026-08-02";

pack.push(NATIVE ? `# D218 acceptance — blinded comparison pack — ${STAMP}

Two ranking modes of the SAME production API, identity hidden per query
behind a random List A / List B flip (key in \`blind-key-${STAMP}.json\`
— do not open until judged). Top-10 shown per side; rendering is
uniform for both sides.

**Read before judging:**

- **Same corpus, same scoping, same transport:** both sides are
  \`/v1/search\` on the hub over the real client path, full corpus,
  identical project/visibility filters. The ONLY difference is the
  ranking function (one side fuses in a semantic lane). Lists may be
  near-identical on some queries — \`preference: tie\` is the honest
  call there.
- **Syntax honesty:** raw query strings went to both sides, OR/NOT/
  NEAR/quotes included.
- **Scoring lanes:** \`[blind-preference]\` queries count toward
  criterion 2 (state A / B / tie per query); \`[recall/GT]\` queries are
  scored by recall@10 arithmetic (preference welcome, not counted);
  #11 is \`[measured-unscored]\` by operator ruling.

State one line per query at the bottom of each section, e.g.
\`preference: A\` / \`preference: B\` / \`preference: tie\`.
` : `# D183 blinded comparison pack — ${STAMP}

Two engines, identity hidden per query behind a random List A / List B
flip (key in \`blind-key-${STAMP}.json\` — do not open until judged).
Top-10 shown per side; rendering is uniform (kind · project · date ·
excerpt resolved from the same corpus snapshot for both sides).

**Read before judging:**

- **Corpus asymmetry:** one engine indexes the FULL /v1/search corpus
  (300,741 rows incl. raw event mirrors); the other indexes the
  DERIVED-ONLY subset (57,908 rows, kind != raw — the 300k scale wall,
  see D214). Exact-id queries especially can surface raw rows on one
  side only.
- **Project scoping:** one engine applies the query's project filter;
  the other cannot scope results by project (its project param only
  scopes an unrelated lessons lane). Queries whose issued filter names a
  nonexistent project (#18, #19 \`beta-project/infra\`; #27 \`personal-safe\`)
  return ZERO on the scoping engine — shown as-is (verbatim policy).
- **Transport:** one engine is reached over the real client path (TLS,
  envoy, tailnet), the other over loopback. Latency is scored separately
  (criterion 3), not part of this judging.
- **Syntax honesty:** raw query strings went to both engines, OR/NOT/
  NEAR/quotes included.
- **Scoring lanes:** \`[blind-preference]\` queries count toward
  criterion 2 (state A / B / tie per query); \`[recall/GT]\` queries are
  scored by recall@10 arithmetic (preference welcome, not counted);
  #11 is \`[measured-unscored]\` by operator ruling.

State one line per query at the bottom of each section, e.g.
\`preference: A\` / \`preference: B\` / \`preference: tie\`.
`);

for (const q of raw.queries) {
  const lane = q.id === 11 ? "measured-unscored" : q.gt === "recall" ? "recall/GT" : "blind-preference";
  const flip = randomInt(2) === 1;
  const sides = flip ? ["hybrid", "fts"] : ["fts", "hybrid"];
  key.queries[q.id] = {
    A: sides[0],
    B: sides[1],
    ids: {
      A: q.engines[sides[0]].hits.slice(0, 10).map((h) => h.id),
      B: q.engines[sides[1]].hits.slice(0, 10).map((h) => h.id),
    },
  };
  const meta = [
    q.project ? `project=${q.project}` : null,
    q.limit ? `limit=${q.limit}` : null,
  ].filter(Boolean).join(" · ");
  pack.push(`---\n\n## #${q.id} \`[${lane}]\` (${q.category}${meta ? " · " + meta : ""})\n`);
  pack.push(`> ${q.query}\n`);
  for (const label of ["A", "B"]) {
    const eng = q.engines[key.queries[q.id][label]];
    pack.push(`### List ${label}\n`);
    if (eng.error) pack.push(`*(error: ${eng.error})*\n`);
    else if (eng.hits.length === 0) pack.push(`*(no results)*\n`);
    else pack.push(eng.hits.slice(0, 10).map(renderItem).join("\n") + "\n");
  }
  pack.push(`preference: \n`);
}

// Recall sheet (objective; names engines — read after judging).
function recallAt10(hits, set) {
  if (set.size === 0) return null;
  const top = hits.slice(0, 10).map((h) => h.id);
  const found = top.filter((id) => set.has(id)).length;
  return found / Math.min(set.size, 10);
}
const recall = [
  `# D183 recall@10 — GT queries (read AFTER judging the blind pack)\n`,
  `Formula: |top10 ∩ GT| / min(|GT|, 10). GT fixed 2026-08-02 from the`,
  `projection snapshot (\`ground-truth-2026-08-02.tsv\`); #2/#22/#29 carry`,
  `SPOT-CHECK flags — operator prunes/ratifies before the verdict uses`,
  `these numbers. Two GT variants: full corpus (the real /v1/search`,
  `contract) and derived-only (controls for the pilot's 300k-wall import).\n`,
  `| q | |GT| full/derived | FTS full | hybrid full | FTS derived | hybrid derived |`,
  `|---|---|---|---|---|---|`,
];
const agg = { fts: { full: [], derived: [] }, hybrid: { full: [], derived: [] } };
for (const q of raw.queries) {
  if (q.gt !== "recall") continue;
  const g = gt[q.id] ?? { full: new Set(), derived: new Set() };
  const row = [`| #${q.id} | ${g.full.size}/${g.derived.size} |`];
  for (const lane of ["full", "derived"])
    for (const eng of ["fts", "hybrid"]) {
      const r = recallAt10(q.engines[eng].hits, g[lane]);
      if (r !== null) agg[eng][lane].push(r);
      // column order in header is fts-full, hybrid-full, fts-derived, hybrid-derived
    }
  const cells = [
    recallAt10(q.engines.fts.hits, g.full),
    recallAt10(q.engines.hybrid.hits, g.full),
    recallAt10(q.engines.fts.hits, g.derived),
    recallAt10(q.engines.hybrid.hits, g.derived),
  ].map((v) => (v === null ? "n/a" : v.toFixed(2)));
  recall.push(row[0] + " " + cells.join(" | ") + " |");
}
const mean = (a) => (a.length ? (a.reduce((x, y) => x + y, 0) / a.length).toFixed(3) : "n/a");
recall.push(
  `| **aggregate (mean)** | | **${mean(agg.fts.full)}** | **${mean(agg.hybrid.full)}** | **${mean(agg.fts.derived)}** | **${mean(agg.hybrid.derived)}** |`,
);
recall.push(
  `\nCriterion 1 (kill if violated): hybrid aggregate >= FTS aggregate (tie passes).`,
  `Which GT variant governs is an operator ruling at verdict — the`,
  `corpus-asymmetry caveat rides either way (#9's GT is 100% raw rows:`,
  `structurally invisible to the derived-only pilot).`,
);

writeFileSync(join(outDir, `pack-${STAMP}.md`), pack.join("\n"));
writeFileSync(join(outDir, `blind-key-${STAMP}.json`), JSON.stringify(key, null, 1));
writeFileSync(join(outDir, `recall-${STAMP}.md`), recall.join("\n"));
console.error(`pack: ${raw.queries.length} queries, resolved ${resolved.size}/${needed.size} ids from snapshot`);
