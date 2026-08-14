// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Shared verdict availability — one truth for every review surface (harmony
// pass, operator ask 2026-07-31): the Sessions story rows and the Review
// queue's detail panel must never drift on which actions a row offers.
// Mirrors the store state machine server-side (0007 enforces the elevation
// invariant).
import type { Observation } from './types'

export type VerdictAction = 'accept' | 'confirm' | 'reject' | 'lapse' | 'rewrite' | 'elevate'

export function can(o: Observation, verdict: VerdictAction): boolean {
  switch (verdict) {
    case 'accept':
    case 'confirm':
    case 'reject':
      return o.review_state === 'proposed' || o.review_state === 'lapsed'
    case 'lapse':
      return o.review_state === 'proposed'
    case 'rewrite':
      return ['proposed', 'lapsed', 'disputed'].includes(o.review_state)
    case 'elevate':
      // One-gesture elevation everywhere: a proposed/lapsed row chains
      // confirm first (memoire:D192); an accepted row elevates directly.
      return o.can_instruct !== 1 && ['accepted', 'proposed', 'lapsed'].includes(o.review_state)
  }
}

// True when elevating this row must chain a confirm first: not accepted yet
// (the elevate endpoint 409s below accepted), or accepted without vouched
// provenance (elevation requires user_confirmed/imported, memoire:D192).
export function needsConfirmChain(o: Observation): boolean {
  return (
    o.review_state !== 'accepted' ||
    (o.provenance !== 'user_confirmed' && o.provenance !== 'imported')
  )
}
