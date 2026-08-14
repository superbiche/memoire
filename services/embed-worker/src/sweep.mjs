// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Freshness sweep (PORT-DESIGN §2): the worker's entire state question is
// the API's missing list — embed, upsert, repeat until the list is empty
// or the deadline hits. Crash-safe and self-healing by construction:
// missed work is always re-discovered from the table itself, so silent
// embed death cannot strand rows (D217's constraint).
import { createHash } from 'node:crypto';
import { MODEL_NAME, embedTexts } from './embedder.mjs';

// Must equal PG's encode(sha256(convert_to(text,'UTF8')),'hex') — the
// CURRENT-row join in vector.go compares this hash verbatim.
export function textHash(text) {
  return createHash('sha256').update(text, 'utf8').digest('hex');
}

export async function sweep(api, { batch = 500, deadlineMs = 55 * 60 * 1000 } = {}) {
  const started = Date.now();
  let total = 0;
  let lastFirstID = -1;
  for (;;) {
    const { missing } = await api.missing(MODEL_NAME, batch);
    if (!missing?.length) break;
    // Livelock guard: an upsert that does not clear its rows from the
    // missing list (hash mismatch, model mismatch) would spin on the same
    // oldest-first batch forever. Fail loud instead — the freshness stat
    // and the CronJob failure history carry the alarm.
    if (missing[0].observation_id === lastFirstID) {
      throw new Error(
        `no progress: observation ${lastFirstID} still missing after upsert — hash or model mismatch between worker and API`,
      );
    }
    lastFirstID = missing[0].observation_id;
    const vectors = await embedTexts(missing.map((m) => m.text));
    await api.upsert(
      missing.map((m, i) => ({
        observation_id: m.observation_id,
        model: MODEL_NAME,
        vector: vectors[i],
        text_hash: textHash(m.text),
      })),
    );
    total += missing.length;
    console.log(`sweep: embedded ${missing.length} (total ${total})`);
    if (Date.now() - started > deadlineMs) {
      console.log('sweep: deadline reached, next run continues');
      break;
    }
  }
  const fresh = await api.freshness(MODEL_NAME);
  console.log(`sweep: done embedded=${total} missing=${fresh.missing} oldest_missing_epoch=${fresh.oldest_missing_epoch}`);
  return { embedded: total, ...fresh };
}
