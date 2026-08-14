// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Thin client for the admin-scoped /v1/embeddings seam
// (services/api/internal/handlers/handlers.go). The token is read from a
// file (MEMOIRE_TOKEN_FILE, or $MEMOIRE_HOME/token) and only ever placed
// in the Authorization header — never logged, never in argv.
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

export function readToken() {
  const file =
    process.env.MEMOIRE_TOKEN_FILE ||
    (process.env.MEMOIRE_HOME && join(process.env.MEMOIRE_HOME, 'token'));
  if (!file) {
    throw new Error('set MEMOIRE_TOKEN_FILE or MEMOIRE_HOME to locate the bearer token');
  }
  const token = readFileSync(file, 'utf8').trim();
  if (!token) throw new Error(`token file ${file} is empty`);
  return token;
}

export class MemoireAPI {
  constructor({ addr = process.env.MEMOIRE_ADDR || '127.0.0.1:37778', token }) {
    this.base = `http://${addr}`;
    this.token = token;
  }

  async #call(method, path, body) {
    const res = await fetch(this.base + path, {
      method,
      headers: {
        Authorization: `Bearer ${this.token}`,
        ...(body ? { 'Content-Type': 'application/json' } : {}),
      },
      body: body ? JSON.stringify(body) : undefined,
    });
    if (!res.ok) {
      const detail = (await res.text()).slice(0, 500);
      throw new Error(`${method} ${path}: HTTP ${res.status} ${detail}`);
    }
    return res.json();
  }

  // -> {model, missing: [{observation_id, text}], total}
  missing(model, limit) {
    const q = new URLSearchParams({ model, limit: String(limit) });
    return this.#call('GET', `/v1/embeddings/missing?${q}`);
  }

  // batch: [{observation_id, model, vector, text_hash}] -> {written}
  upsert(batch) {
    return this.#call('PUT', '/v1/embeddings', { embeddings: batch });
  }

  // -> {model, missing, oldest_missing_epoch}
  freshness(model) {
    const q = new URLSearchParams({ model });
    return this.#call('GET', `/v1/embeddings/freshness?${q}`);
  }
}
