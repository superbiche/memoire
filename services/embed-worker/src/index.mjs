// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// memoire-embed-worker — one image, two duties (PORT-DESIGN §2):
//   node src/index.mjs serve   query-embedding HTTP service (Deployment)
//   node src/index.mjs sweep   freshness sweep, exits when caught up (CronJob)
import { readToken, MemoireAPI } from './api.mjs';
import { sweep } from './sweep.mjs';
import { serve } from './serve.mjs';

const mode = process.argv[2];

try {
  const token = readToken();
  if (mode === 'serve') {
    await serve({ port: Number(process.env.MEMOIRE_EMBED_PORT || 37779), token });
  } else if (mode === 'sweep') {
    await sweep(new MemoireAPI({ token }), {
      batch: Number(process.env.MEMOIRE_EMBED_BATCH || 500),
      deadlineMs: Number(process.env.MEMOIRE_EMBED_DEADLINE_S || 3300) * 1000,
    });
  } else {
    console.error('usage: node src/index.mjs serve|sweep');
    process.exit(2);
  }
} catch (err) {
  console.error(`embed-worker ${mode}:`, err?.message || err);
  process.exit(1);
}
