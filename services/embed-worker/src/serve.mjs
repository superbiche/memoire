// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Query-embedding service (PORT-DESIGN §2): POST /embed {"text"} ->
// {"model","dim","vector"} on a cluster-local port, used by the API at
// search time. Token-guarded like the other internal seams; /healthz is
// probe-only and unauthenticated (it reveals nothing but readiness).
import http from 'node:http';
import { createHash, timingSafeEqual } from 'node:crypto';
import { MODEL_NAME, DIM, embedTexts, loadExtractor } from './embedder.mjs';

function tokenMatches(header, token) {
  const got = header?.startsWith('Bearer ') ? header.slice(7).trim() : '';
  // Compare digests: constant-time and length-independent.
  const a = createHash('sha256').update(got, 'utf8').digest();
  const b = createHash('sha256').update(token, 'utf8').digest();
  return timingSafeEqual(a, b);
}

function readBody(req, limit = 1 << 20) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on('data', (c) => {
      size += c.length;
      if (size > limit) reject(new Error('body too large'));
      else chunks.push(c);
    });
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
  });
}

export async function serve({ port, token }) {
  // Warm the model before accepting traffic: the readiness probe then
  // means "can embed now", not "process exists".
  await loadExtractor();
  await embedTexts(['warmup']);

  const server = http.createServer(async (req, res) => {
    const respond = (code, body) => {
      res.writeHead(code, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(body));
    };
    try {
      if (req.method === 'GET' && req.url === '/healthz') {
        return respond(200, { ok: true, model: MODEL_NAME, dim: DIM });
      }
      if (req.method === 'POST' && req.url === '/embed') {
        if (!tokenMatches(req.headers.authorization, token)) {
          return respond(401, { error: 'unauthorized' });
        }
        const body = JSON.parse(await readBody(req));
        if (typeof body?.text !== 'string' || body.text === '') {
          return respond(400, { error: 'text is required' });
        }
        const [vector] = await embedTexts([body.text]);
        return respond(200, { model: MODEL_NAME, dim: DIM, vector });
      }
      return respond(404, { error: 'not found' });
    } catch (err) {
      return respond(500, { error: String(err?.message || err) });
    }
  });

  await new Promise((resolve) => server.listen(port, '0.0.0.0', resolve));
  console.log(`embed service ready on :${port} (model ${MODEL_NAME}, dim ${DIM})`);
  return server;
}
