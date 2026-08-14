// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

import type { Observation, SessionSummary, Stats, TimelineHit } from './types'

const TOKEN_KEY = 'memoire.token'

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? ''
}

export function setToken(token: string): void {
  localStorage.setItem(TOKEN_KEY, token.trim())
}

export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY)
}

// Metadata-only snapshot of a row's review state, as carried by the
// server's unified 409 (code: state_conflict) — never text or payload, so
// it is safe even for states the read surface hides (memoire:D207).
export interface ConflictCurrent {
  id: number
  review_state: string
  provenance: string
  can_instruct: number
}

// Optional compare-and-set snapshot sent with a verdict: the server
// conflicts (writes nothing) unless every provided field matches the row.
export interface Expected {
  review_state?: string
  provenance?: string
  can_instruct?: number
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    // set when the server answered with the unified state-conflict shape
    public code?: string,
    public current?: ConflictCurrent,
  ) {
    super(message)
  }
}

type UnauthorizedHandler = () => void
let onUnauthorized: UnauthorizedHandler | null = null

export function setUnauthorizedHandler(fn: UnauthorizedHandler): void {
  onUnauthorized = fn
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  const token = getToken()
  if (token) headers.set('Authorization', `Bearer ${token}`)
  if (init.body) headers.set('Content-Type', 'application/json')
  const res = await fetch(path, { ...init, headers })
  if (res.status === 401) {
    onUnauthorized?.()
    throw new ApiError(401, 'unauthorized')
  }
  if (!res.ok) {
    const text = await res.text().catch(() => '')
    // The unified 409 carries machine-readable current state (D207): parse
    // it into the error so callers never string-match prose.
    try {
      const parsed = JSON.parse(text) as {
        error?: string
        code?: string
        current?: ConflictCurrent
      }
      if (parsed && parsed.code === 'state_conflict' && parsed.current) {
        throw new ApiError(res.status, parsed.error || res.statusText, parsed.code, parsed.current)
      }
    } catch (e) {
      if (e instanceof ApiError) throw e
    }
    throw new ApiError(res.status, text || res.statusText)
  }
  return (await res.json()) as T
}

// Outcome of a preconditioned verdict call, retried once across a transport
// failure. This IS the reconciliation protocol (memoire:D207): every call
// carries the caller's snapshot as a precondition, so of the original and
// the retry at most ONE can ever commit — the other reports the row's
// current metadata through the 409. No truth-reads, no grace waits.
export type VerdictOutcome =
  | { kind: 'ok'; observation: Observation }
  | { kind: 'conflict'; current: ConflictCurrent }
  | { kind: 'gone' } // row absent server-side (purged)
  | { kind: 'failed'; error: ApiError } // server answered with a non-conflict error
  | { kind: 'unknown' } // transport failed twice — state genuinely unverified

export async function preconditionedCall(
  fn: () => Promise<{ observation: Observation }>,
): Promise<VerdictOutcome> {
  for (let attempt = 0; ; attempt++) {
    try {
      return { kind: 'ok', observation: (await fn()).observation }
    } catch (e) {
      if (e instanceof ApiError) {
        if (e.code === 'state_conflict' && e.current) return { kind: 'conflict', current: e.current }
        if (e.status === 404) return { kind: 'gone' }
        return { kind: 'failed', error: e }
      }
      // Transport failure: the POST may have committed without a response —
      // retrying the SAME preconditioned call is safe by construction.
      if (attempt >= 1) return { kind: 'unknown' }
    }
  }
}

export const api = {
  // 200 is the server-side max for this endpoint (normalizeLimit); anything
  // above silently falls back to 50. The `order` param decides WHICH capped
  // window the server returns (newest-200 vs oldest-200) — client-side
  // sorting alone cannot substitute. Windows are fetched on demand and
  // MERGED into the queue client-side (stores/review.ts) so a capped
  // response can never evict already-loaded items. Deeper queues still
  // need pagination later.
  listProposed(
    includeNoise: boolean,
    order: 'desc' | 'asc' = 'desc',
    cursor?: string,
    limit = 200,
  ): Promise<{ observations: Observation[]; total: number; next_cursor: string | null }> {
    const params = new URLSearchParams({
      review_state: 'proposed',
      limit: String(limit),
    })
    if (includeNoise) params.set('include_noise', 'true')
    if (order === 'asc') params.set('order', 'asc')
    if (cursor) params.set('cursor', cursor)
    return request(`/v1/observations?${params}`)
  },

  getObservation(id: number): Promise<{ observation: Observation }> {
    return request(`/v1/observations/${id}?include_proposed=true&include_noise=true`)
  },

  accept(id: number, reason?: string, expected?: Expected): Promise<{ observation: Observation }> {
    return request(`/v1/observations/${id}/accept`, {
      method: 'POST',
      body: JSON.stringify({ ...(reason ? { reason_text: reason } : {}), ...(expected ? { expected } : {}) }),
    })
  },

  reject(id: number, reason?: string, expected?: Expected): Promise<{ observation: Observation }> {
    return request(`/v1/observations/${id}/reject`, {
      method: 'POST',
      body: JSON.stringify({ ...(reason ? { reason_text: reason } : {}), ...(expected ? { expected } : {}) }),
    })
  },

  // Elevate = can_instruct gate (docs/ELEVATION-DESIGN.md), a second gate
  // above accept: the observation rides the operating-notes block of every
  // future session for its project. 409 unless already accepted.
  elevate(id: number, reason?: string): Promise<{ observation: Observation }> {
    return request(`/v1/observations/${id}/elevate`, {
      method: 'POST',
      body: JSON.stringify(reason ? { reason_text: reason } : {}),
    })
  },

  // Inverse of elevate: can_instruct back to 0. 409 unless currently elevated.
  demote(id: number, reason?: string): Promise<{ observation: Observation }> {
    return request(`/v1/observations/${id}/demote`, {
      method: 'POST',
      body: JSON.stringify(reason ? { reason_text: reason } : {}),
    })
  },

  timeline(opts: {
    project?: string
    limit?: number
    elevatedOnly?: boolean
  }): Promise<{ hits: TimelineHit[]; total: number }> {
    const params = new URLSearchParams({
      exclude_kind: 'raw',
      include_proposed: 'true',
      limit: String(opts.limit ?? 100),
    })
    if (opts.project) params.set('project', opts.project)
    if (opts.elevatedOnly) params.set('can_instruct', 'true')
    return request(`/v1/timeline?${params}`)
  },

  stats(): Promise<Stats> {
    return request('/v1/stats')
  },

  // --- Slice E (memoire:D146): session review surface ---

  // Confirm = deliberate accept + provenance promotion to user_confirmed.
  // Plain accept leaves provenance untouched (R-5 split).
  confirm(id: number, reason?: string, expected?: Expected): Promise<{ observation: Observation }> {
    return request(`/v1/observations/${id}/confirm`, {
      method: 'POST',
      body: JSON.stringify({ ...(reason ? { reason_text: reason } : {}), ...(expected ? { expected } : {}) }),
    })
  },

  // Lapse = park unreviewed: searchable, ~-marked at injection, never
  // feed-eligible. Reversible on-encounter (accept/reject/rewrite).
  lapse(id: number, reason?: string, expected?: Expected): Promise<{ observation: Observation }> {
    return request(`/v1/observations/${id}/lapse`, {
      method: 'POST',
      body: JSON.stringify({ ...(reason ? { reason_text: reason } : {}), ...(expected ? { expected } : {}) }),
    })
  },

  // Rewrite = NOT an edit: a new accepted user_confirmed observation with
  // the operator's text supersedes the original.
  rewrite(id: number, text: string, reason?: string): Promise<{ observation: Observation }> {
    return request(`/v1/observations/${id}/rewrite`, {
      method: 'POST',
      body: JSON.stringify({ text, reason_text: reason ?? '' }),
    })
  },

  listSessions(opts: {
    project?: string
    proposedOnly?: boolean
    limit?: number
  }): Promise<{ sessions: SessionSummary[] }> {
    const params = new URLSearchParams({ limit: String(opts.limit ?? 100) })
    if (opts.project) params.set('project', opts.project)
    if (opts.proposedOnly) params.set('proposed_only', 'true')
    return request(`/v1/sessions?${params}`)
  },

  // One session's story: every semantic observation, oldest first, all
  // review states in the unreviewed band included. Paged: the caller loops
  // next_cursor until exhausted — bulk actions must never run on a
  // partially loaded story (review finding 2).
  sessionObservations(
    sessionID: string,
    cursor?: string,
  ): Promise<{ observations: Observation[]; total: number; next_cursor: string | null }> {
    const params = new URLSearchParams({
      session_id: sessionID,
      exclude_kind: 'raw',
      include_proposed: 'true',
      include_noise: 'true',
      order: 'asc',
      limit: '200',
    })
    if (cursor) params.set('cursor', cursor)
    return request(`/v1/observations?${params}`)
  },

  lapseRemaining(sessionID: string, reason?: string): Promise<{ lapsed_count: number }> {
    return request(`/v1/sessions/${encodeURIComponent(sessionID)}/lapse-remaining`, {
      method: 'POST',
      body: JSON.stringify(reason ? { reason_text: reason } : {}),
    })
  },
}
