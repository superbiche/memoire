// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Build-time model custody (PORT-DESIGN non-goal: no runtime HF fetch on
// the hub — model files ship in the image).
//   fetch   download MiniLM q8 into MEMOIRE_EMBED_MODEL_DIR (network OK,
//           build stage only)
//   verify  load + embed strictly offline through the same code path the
//           runtime uses — a wrong layout fails the BUILD, not the hub
import { embedTexts, loadExtractor, DIM, HF_MODEL } from '../src/embedder.mjs';

const mode = process.argv[2];
if (mode !== 'fetch' && mode !== 'verify') {
  console.error('usage: node scripts/model-tool.mjs fetch|verify');
  process.exit(2);
}
if (!process.env.MEMOIRE_EMBED_MODEL_DIR) {
  console.error('MEMOIRE_EMBED_MODEL_DIR is required');
  process.exit(2);
}
if (mode === 'verify' && process.env.MEMOIRE_EMBED_LOCAL_ONLY !== '1') {
  console.error('verify must run with MEMOIRE_EMBED_LOCAL_ONLY=1');
  process.exit(2);
}

try {
  await loadExtractor();
  const [v] = await embedTexts(['model custody smoke']);
  if (v.length !== DIM) throw new Error(`got ${v.length} dims, want ${DIM}`);
  console.log(`${mode} ok: ${HF_MODEL} q8, ${DIM} dims, dir ${process.env.MEMOIRE_EMBED_MODEL_DIR}`);
} catch (err) {
  console.error(`${mode} failed:`, err?.message || err);
  process.exit(1);
}
