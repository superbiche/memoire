// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Model identity contract: MODEL_NAME and DIM must match DefaultEmbedModel
// and EmbedDim in services/api/internal/db/vector.go — the API decides
// whether a stored row is CURRENT by comparing against that name.
import { env, pipeline } from '@huggingface/transformers';

export const MODEL_NAME = 'all-MiniLM-L6-v2-q8';
export const HF_MODEL = 'Xenova/all-MiniLM-L6-v2';
export const DIM = 384;

// MEMOIRE_EMBED_MODEL_DIR points at the baked model files (set in the
// image); MEMOIRE_EMBED_LOCAL_ONLY=1 forbids any runtime HF fetch — on the
// hub a missing file must be a loud load error, never a network call
// (PORT-DESIGN non-goal: no runtime model downloads).
export function configureModelEnv() {
  const dir = process.env.MEMOIRE_EMBED_MODEL_DIR;
  if (dir) {
    env.cacheDir = dir;
    env.localModelPath = dir;
  }
  if (process.env.MEMOIRE_EMBED_LOCAL_ONLY === '1') {
    env.allowRemoteModels = false;
  }
}

let extractorPromise;

export function loadExtractor() {
  if (!extractorPromise) {
    configureModelEnv();
    extractorPromise = pipeline('feature-extraction', HF_MODEL, { dtype: 'q8' });
  }
  return extractorPromise;
}

// Mean-pooled, L2-normalized sentence vectors — the exact shape the pilot
// measured. Sub-batches keep peak memory flat on 500-row sweep batches.
export async function embedTexts(texts, batchSize = 16) {
  const extractor = await loadExtractor();
  const out = [];
  for (let i = 0; i < texts.length; i += batchSize) {
    const chunk = texts.slice(i, i + batchSize);
    const t = await extractor(chunk, { pooling: 'mean', normalize: true });
    const [n, dim] = t.dims;
    if (n !== chunk.length || dim !== DIM) {
      throw new Error(`model produced ${n}x${dim}, want ${chunk.length}x${DIM}`);
    }
    for (let j = 0; j < n; j++) {
      out.push(Array.from(t.data.subarray(j * dim, (j + 1) * dim)));
    }
    t.dispose?.();
  }
  return out;
}
