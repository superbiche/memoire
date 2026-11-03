// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

import { computed, reactive } from 'vue'
import { api, preconditionedCall, type ConflictCurrent } from '../lib/api'
import type { Observation, ReviewedEntry, Verdict } from '../lib/types'
import { needsConfirmChain } from '../lib/verdicts'
import { pushToast } from './toast'

// Single fetch of the whole proposed queue (with noise); filtering is
// client-side so chips and the noise toggle respond instantly.
export const state = reactive({
  items: [] as Observation[],
  loading: false,
  loaded: false,
  // Noise candidates are a separate (much larger) population fetched only
  // when the include-noise toggle first flips on.
  noiseLoaded: false,
  // One entry per fetched (order × noise) window, keyed `${order}:${noise}`:
  // the continuation cursor for its next page, and exhausted once the server
  // stops returning one.
  windows: {} as Record<string, { cursor: string | null; exhausted: boolean }>,
  error: '',
  reviewed: [] as ReviewedEntry[],
  // id of a card currently animating out, mapped to its verdict — the leave
  // transition reads it to slide right (accept), left (reject), or down
  // (lapse: parked, not judged).
  leaving: {} as Record<number, Verdict | 'lapsed'>,
  // ids with an elevate/demote request in flight — the detail panel disables
  // its controls so a double-click cannot race a 409 against the first call.
  elevationPending: {} as Record<number, true>,
})

export const acceptedCount = computed(
  () => state.reviewed.filter((r) => r.verdict === 'accepted').length,
)
export const confirmedCount = computed(
  () => state.reviewed.filter((r) => r.verdict === 'confirmed').length,
)
export const rejectedCount = computed(
  () => state.reviewed.filter((r) => r.verdict === 'rejected').length,
)

function windowKey(order: 'desc' | 'asc', withNoise: boolean): string {
  return `${order}:${withNoise}`
}

// Rows this session resolved WITHOUT a queue verdict — lapsed, or superseded
// by a rewrite. They never enter `reviewed` (that set drives the u-history
// and its counters), but a stale page response merging late must not
// resurrect them either: `leaving` only covers the 400ms animation window.
const suppressed = new Set<number>()

// Serialize window fetches: a request arriving while another fetch is in
// flight must wait and then re-check, never be dropped — the direction the
// user selected decides which capped server window we must hold.
let inflight: Promise<void> | null = null

async function serialized(fn: () => Promise<void>): Promise<void> {
  while (inflight) await inflight
  inflight = fn()
  try {
    await inflight
  } finally {
    inflight = null
  }
}

// First page of a (direction × noise) window; no-op if already held.
export async function loadQueue(order: 'desc' | 'asc' = 'desc', withNoise = false): Promise<void> {
  if (state.windows[windowKey(order, withNoise)]) return
  await serialized(async () => {
    if (state.windows[windowKey(order, withNoise)]) return
    await fetchWindow(order, withNoise)
  })
}

// Next page of an already-held window, continuing from its cursor.
export async function loadMore(order: 'desc' | 'asc', withNoise: boolean): Promise<void> {
  await serialized(async () => {
    const win = state.windows[windowKey(order, withNoise)]
    if (!win || win.exhausted || !win.cursor) return
    await fetchWindow(order, withNoise, win.cursor)
  })
}

async function fetchWindow(
  order: 'desc' | 'asc',
  withNoise: boolean,
  cursor?: string,
): Promise<void> {
  state.loading = true
  state.error = ''
  try {
    const res = await api.listProposed(withNoise, order, cursor)
    // An empty result set marshals as JSON null, not [] — never let that
    // reach the render path.
    res.observations ??= []
    // Display order is owned by ReviewView (sort control); the fetch decides
    // which capped window/page we hold, not how it is shown.
    if (state.loaded) {
      // Merge, never replace: every fetch is capped at 200, so a new page
      // must not evict items already in the queue. Exclude items reviewed
      // this session or with a verdict in flight — a response generated
      // before the verdict committed must not resurrect them.
      const have = new Set(state.items.map((o) => o.id))
      const reviewed = new Set(state.reviewed.map((r) => r.observation.id))
      state.items.push(
        ...res.observations.filter(
          (o) =>
            !have.has(o.id) &&
            !reviewed.has(o.id) &&
            !(o.id in state.leaving) &&
            !suppressed.has(o.id),
        ),
      )
    } else {
      state.items = res.observations
    }
    state.loaded = true
    state.windows[windowKey(order, withNoise)] = {
      cursor: res.next_cursor,
      exhausted: !res.next_cursor,
    }
    if (withNoise) state.noiseLoaded = true
  } catch (e) {
    state.error = e instanceof Error ? e.message : String(e)
  } finally {
    state.loading = false
  }
}

// Core elevate/demote call shared by the review history view and Browse's
// detail panel: pending guard (double-submit protection), toasts, and the
// server's post-action observation as the return value — null on failure or
// when a request for this id is already in flight. Callers own patching
// their local copies with the returned record.
// Outcome shape (round-3 review NEW MAJOR 1): `observation` carries the
// freshest known truth even when `applied` is false — a conflict that
// proves the request did NOT land still told us what the row IS, and the
// caller must be able to repaint/reconcile membership from it instead of
// stranding a stale cache.
export interface ElevationOutcome {
  applied: boolean
  // Definitive server 404 — the row is purged (round-4 review MAJOR 1):
  // callers must drop/reload it rather than keep offering controls that
  // will 404 forever.
  gone?: boolean
  observation: Observation | null
}

export async function applyElevation(
  observation: Observation,
  elevated: boolean,
  reason?: string,
): Promise<ElevationOutcome> {
  const id = observation.id
  if (state.elevationPending[id]) return { applied: false, observation: null }
  state.elevationPending[id] = true
  try {
    // Chained confirm+elevate (memoire:D192): elevation requires
    // user_confirmed/imported provenance. When the row lacks it, confirm
    // first — one UI moment, two API calls, two review-log rows, so the
    // double-human-gesture invariant stays real (the caller's friction box
    // announces the chain before this runs). Each leg runs the
    // preconditioned retry protocol (round-2 review NEW MAJOR 1: raw calls
    // here left a committed-but-lost demote painted elevated forever, and a
    // stale-row retry could double-log the confirm).
    let base = observation
    const needsConfirm =
      elevated &&
      observation.provenance !== 'user_confirmed' &&
      observation.provenance !== 'imported'
    if (needsConfirm) {
      const expected = {
        review_state: observation.review_state,
        provenance: observation.provenance,
        can_instruct: observation.can_instruct,
      }
      const out = await preconditionedCall(() => api.confirm(id, reason, expected))
      if (out.kind === 'ok') {
        base = out.observation
      } else if (
        out.kind === 'conflict' &&
        out.current.review_state === 'accepted' &&
        out.current.provenance === 'user_confirmed'
      ) {
        // the confirm landed (lost response) or an equivalent act did
        base = { ...observation, ...metaPatch(out.current) }
        if (out.current.can_instruct === 1) {
          pushToast('success', `#${id} confirmed + elevated (verified via state conflict)`)
          return { applied: true, observation: base }
        }
      } else {
        pushToast(
          'error',
          out.kind === 'conflict'
            ? `#${id} is ${out.current.review_state} on the server — elevation not applied`
            : out.kind === 'gone'
              ? `#${id} no longer exists on the server — elevation not applied`
              : out.kind === 'unknown'
                ? `#${id} confirm failed and its state could not be verified — reload before retrying`
                : `#${id} confirm failed: ${out.error.message}`,
        )
        return {
          applied: false,
          gone: out.kind === 'gone',
          observation:
            out.kind === 'conflict' ? { ...observation, ...metaPatch(out.current) } : null,
        }
      }
    }
    const out = await preconditionedCall(() => (elevated ? api.elevate(id, reason) : api.demote(id, reason)))
    if (out.kind === 'ok') {
      pushToast(
        'success',
        elevated
          ? `#${id} ${needsConfirm ? 'confirmed + ' : ''}elevated — rides every session when injection is on`
          : `#${id} demoted — memory only again`,
      )
      return { applied: true, observation: out.observation }
    }
    if (out.kind === 'conflict' && out.current.can_instruct === (elevated ? 1 : 0)) {
      // The requested end-state holds — a lost response (or equivalent
      // foreign act): report success and hand back patched truth.
      pushToast(
        'success',
        elevated
          ? `#${id} elevated (verified via state conflict)`
          : `#${id} demoted (verified via state conflict)`,
      )
      return { applied: true, observation: { ...base, ...metaPatch(out.current) } }
    }
    pushToast(
      'error',
      out.kind === 'conflict'
        ? `#${id} is ${out.current.review_state}/${out.current.can_instruct === 1 ? 'elevated' : 'not elevated'} on the server — ${elevated ? 'elevate' : 'demote'} not applied`
        : out.kind === 'gone'
          ? `#${id} no longer exists on the server — ${elevated ? 'elevate' : 'demote'} not applied`
          : out.kind === 'unknown'
            ? `#${id} ${elevated ? 'elevate' : 'demote'} failed and its state could not be verified — reload before retrying`
            : `#${id} ${elevated ? 'elevate' : 'demote'} failed: ${out.error.message}`,
    )
    return {
      applied: false,
      gone: out.kind === 'gone',
      observation:
        out.kind === 'conflict'
          ? { ...base, ...metaPatch(out.current) }
          : out.kind !== 'gone' && base !== observation
            ? // The confirm leg LANDED (base carries its truth) — a
              // definitive or unverifiable elevate failure must not lose it
              // (round-4 review MAJOR 1; queue sibling records the same).
              base
            : null,
    }
  } finally {
    delete state.elevationPending[id]
  }
}

// Elevate/demote from the detail panel's history view. The entry's
// observation record is replaced with the server's post-action state so
// badges stay truthful; failures leave it untouched.
export async function setElevation(
  entry: ReviewedEntry,
  elevated: boolean,
  reason?: string,
): Promise<{ applied: boolean; gone: boolean }> {
  const res = await applyElevation(entry.observation, elevated, reason)
  // Freshest known truth updates the entry even when the request did not
  // land (round-3 review NEW MAJOR 1): a stale cached record must not keep
  // offering an elevation the server would refuse. Eviction of a GONE
  // entry is the VIEW's job (round-6 review): the tail-relative history
  // cursor lives there, and a store-side splice shifted which entry the
  // same offset selects — or left a latent cursor that reactivated later.
  if (res.observation) entry.observation = res.observation
  return { applied: res.applied, gone: res.gone === true }
}

// Evict a purged history entry (round-5/6 reviews): a record whose
// controls fire into a permanent 404 is a trap. The caller repairs its
// cursor around this by entry IDENTITY.
export function evictReviewedEntry(entry: ReviewedEntry): void {
  const i = state.reviewed.indexOf(entry)
  if (i >= 0) state.reviewed.splice(i, 1)
}

// Preconditioned verdicts (memoire:D207): every queue mutation sends the
// card's snapshot as an `expected` precondition and runs through
// preconditionedCall (lib/api) — the server's unified 409 replaces the old
// truth-read/grace-wait reconciliation entirely. Of an original call and
// its transport-failure retry, at most one can ever commit; the other
// reports the row's current metadata, which works even for states the read
// surface hides (a landed reject shows up as `current.review_state:
// "rejected"` in the conflict).
const metaPatch = (c: ConflictCurrent) => ({
  review_state: c.review_state,
  provenance: c.provenance,
  can_instruct: c.can_instruct,
})

// Lapse from the queue = park unreviewed (memoire:D146): not a verdict, so
// no `reviewed` history entry — `suppressed` keeps stale merges from
// resurrecting the card instead.
export async function lapseFromQueue(observation: Observation): Promise<boolean> {
  const index = state.items.findIndex((o) => o.id === observation.id)
  if (index < 0) return false
  state.leaving[observation.id] = 'lapsed'
  state.items.splice(index, 1)
  const out = await preconditionedCall(() =>
    api.lapse(observation.id, undefined, {
      review_state: observation.review_state,
      provenance: observation.provenance,
      can_instruct: observation.can_instruct,
    }),
  )
  let ok = false
  switch (out.kind) {
    case 'ok':
      suppressed.add(observation.id)
      pushToast('success', `#${observation.id} lapsed — parked, revivable on encounter`)
      ok = true
      break
    case 'conflict':
      suppressed.add(observation.id) // whatever it is now, it is not proposed
      if (out.current.review_state === 'lapsed') {
        pushToast('success', `#${observation.id} lapsed (verified via state conflict)`)
        ok = true
      } else {
        pushToast('error', `#${observation.id} is ${out.current.review_state} on the server — lapse not applied`)
      }
      break
    case 'gone':
      suppressed.add(observation.id)
      pushToast('error', `#${observation.id} no longer exists on the server — lapse not applied`)
      break
    case 'failed':
      state.items.splice(Math.min(index, state.items.length), 0, observation)
      pushToast('error', `#${observation.id} lapse failed: ${out.error.message}`)
      break
    case 'unknown':
      state.items.splice(Math.min(index, state.items.length), 0, observation)
      pushToast('error', `#${observation.id} lapse failed and its state could not be verified — reload before retrying`)
      break
  }
  setTimeout(() => delete state.leaving[observation.id], 400)
  return ok
}

// Rewrite from the queue: the operator's wording becomes a NEW accepted
// user_confirmed observation superseding the original — a confirm-class
// outcome, so history carries the NEW row under the confirmed verdict.
// Per-row pending guard (round-5 MINOR 1): a double-submit must not send a
// second rewrite POST that then fails against the superseded original.
const rewritePending = new Set<number>()

export async function rewriteFromQueue(
  observation: Observation,
  text: string,
  reason?: string,
): Promise<boolean> {
  if (rewritePending.has(observation.id)) return false
  rewritePending.add(observation.id)
  try {
    const res = await api.rewrite(observation.id, text, reason)
    const i = state.items.findIndex((o) => o.id === observation.id)
    if (i >= 0) {
      state.leaving[observation.id] = 'confirmed'
      state.items.splice(i, 1)
    }
    suppressed.add(observation.id)
    state.reviewed.push({
      observation: res.observation,
      verdict: 'confirmed',
      reason: reason || `rewrote #${observation.id}`,
      at: Date.now(),
    })
    pushToast('success', `#${observation.id} rewritten as #${res.observation.id}`)
    return true
  } catch (e) {
    pushToast('error', `#${observation.id} rewrite failed: ${e instanceof Error ? e.message : e}`)
    return false
  } finally {
    rewritePending.delete(observation.id)
    setTimeout(() => delete state.leaving[observation.id], 400)
  }
}

// One-gesture elevation from the live queue (operator ask 2026-07-31): a
// proposed row chains confirm (accept + I vouch) then elevate — one UI
// moment behind the friction box, two review-log rows, the memoire:D192
// double-gesture invariant intact.
export async function elevateFromQueue(
  observation: Observation,
  reason?: string,
): Promise<boolean> {
  const id = observation.id
  if (state.elevationPending[id]) return false
  state.elevationPending[id] = true
  // Optimistic removal, review()-style: the card animates out as confirmed.
  const index = state.items.findIndex((o) => o.id === id)
  if (index >= 0) {
    state.leaving[id] = 'confirmed'
    state.items.splice(index, 1)
  }
  try {
    // Leg 1 — confirm (accept + vouch), preconditioned on the queue
    // snapshot. A conflict whose current is already vouched means the
    // confirm landed without its response (or an equivalent foreign act,
    // same outcome) — the chain continues.
    let confirmed = observation
    if (needsConfirmChain(observation)) {
      // Full snapshot, captured once (round-1 BLOCKER 2).
      const expected = {
        review_state: observation.review_state,
        provenance: observation.provenance,
        can_instruct: observation.can_instruct,
      }
      const out = await preconditionedCall(() => api.confirm(id, reason, expected))
      if (out.kind === 'ok') {
        confirmed = out.observation
      } else if (
        out.kind === 'conflict' &&
        out.current.review_state === 'accepted' &&
        out.current.provenance === 'user_confirmed'
      ) {
        confirmed = { ...observation, ...metaPatch(out.current) }
        if (out.current.can_instruct === 1) {
          // fully elevated already (our lost chain, or equivalent)
          state.reviewed.push({ observation: confirmed, verdict: 'confirmed', reason, at: Date.now() })
          pushToast('success', `#${id} confirmed + elevated (verified via state conflict)`)
          return true
        }
      } else if (out.kind === 'conflict') {
        // Moved, but not by our chain: keep it out of the queue (it is not
        // proposed), record nothing in this session's history.
        suppressed.add(id)
        pushToast('error', `#${id} is ${out.current.review_state} on the server — elevation not applied`)
        return false
      } else if (out.kind === 'gone') {
        suppressed.add(id)
        pushToast('error', `#${id} no longer exists on the server — elevation not applied`)
        return false
      } else {
        if (index >= 0) state.items.splice(Math.min(index, state.items.length), 0, observation)
        pushToast(
          'error',
          out.kind === 'unknown'
            ? `#${id} elevate failed and its state could not be verified — reload before retrying`
            : `#${id} elevate failed: ${out.error.message}`,
        )
        return false
      }
    }
    // Leg 2 — elevate. Its intrinsic guards conflict with current metadata:
    // can_instruct=1 in the conflict IS the landed signal.
    const out2 = await preconditionedCall(() => api.elevate(id, reason))
    if (out2.kind === 'ok') {
      state.reviewed.push({ observation: out2.observation, verdict: 'confirmed', reason, at: Date.now() })
      pushToast('success', `#${id} confirmed + elevated — rides every session when injection is on`)
      return true
    }
    if (out2.kind === 'conflict' && out2.current.can_instruct === 1) {
      state.reviewed.push({
        observation: { ...confirmed, ...metaPatch(out2.current) },
        verdict: 'confirmed',
        reason,
        at: Date.now(),
      })
      pushToast('success', `#${id} confirmed + elevated (verified via state conflict)`)
      return true
    }
    // Elevation really did not land: the row is accepted+vouched
    // server-side and no longer belongs in the proposed queue — record it
    // as reviewed and surface the elevation failure alone.
    state.reviewed.push({ observation: confirmed, verdict: 'confirmed', reason, at: Date.now() })
    pushToast(
      'error',
      out2.kind === 'failed' || out2.kind === 'conflict'
        ? `#${id} confirmed, but elevate failed: ${out2.kind === 'failed' ? out2.error.message : 'state conflict'}`
        : `#${id} confirmed, but the elevate's state could not be verified — it is under u`,
    )
    return false
  } finally {
    delete state.elevationPending[id]
    setTimeout(() => delete state.leaving[id], 400)
  }
}

export async function review(
  observation: Observation,
  verdict: Verdict,
  reason?: string,
): Promise<boolean> {
  const index = state.items.findIndex((o) => o.id === observation.id)
  if (index < 0) return false
  state.leaving[observation.id] = verdict
  state.items.splice(index, 1)
  // Preconditioned on the card's FULL snapshot (memoire:D207; round-1
  // BLOCKER 2: review_state alone cannot serialize confirm-on-accepted).
  // The requested verdict's postcondition is judged from the conflict's
  // metadata — a landed reject reads as current.review_state "rejected"
  // here even though the read surface hides rejected rows.
  const expected = {
    review_state: observation.review_state,
    provenance: observation.provenance,
    can_instruct: observation.can_instruct,
  }
  const out = await preconditionedCall(() =>
    verdict === 'accepted'
      ? api.accept(observation.id, reason, expected)
      : verdict === 'confirmed'
        ? api.confirm(observation.id, reason, expected)
        : api.reject(observation.id, reason, expected),
  )
  const landedMeta = (c: ConflictCurrent) =>
    verdict === 'rejected'
      ? c.review_state === 'rejected'
      : verdict === 'confirmed'
        ? c.review_state === 'accepted' && c.provenance === 'user_confirmed'
        : c.review_state === 'accepted'
  let ok = false
  switch (out.kind) {
    case 'ok':
      state.reviewed.push({ observation: out.observation, verdict, reason, at: Date.now() })
      pushToast(
        verdict === 'rejected' ? 'danger' : 'success',
        `#${observation.id} ${verdict}${reason ? ` — ${reason}` : ''}`,
      )
      ok = true
      break
    case 'conflict':
      if (landedMeta(out.current)) {
        // Our verdict landed but its response was lost (an identical
        // foreign verdict in the same window is outcome-equivalent).
        state.reviewed.push({
          observation: { ...observation, ...metaPatch(out.current) },
          verdict,
          reason,
          at: Date.now(),
        })
        pushToast(
          verdict === 'rejected' ? 'danger' : 'success',
          `#${observation.id} ${verdict} (verified via state conflict)`,
        )
        ok = true
      } else {
        // Moved by another actor: not proposed, not ours — drop without a
        // history entry.
        suppressed.add(observation.id)
        pushToast(
          'error',
          `#${observation.id} is ${out.current.review_state} on the server — ${verdict} not applied`,
        )
      }
      break
    case 'gone':
      suppressed.add(observation.id)
      pushToast('error', `#${observation.id} no longer exists on the server — ${verdict} not applied`)
      break
    case 'failed':
      state.items.splice(Math.min(index, state.items.length), 0, observation)
      pushToast('error', `#${observation.id} ${verdict} failed: ${out.error.message}`)
      break
    case 'unknown':
      state.items.splice(Math.min(index, state.items.length), 0, observation)
      pushToast(
        'error',
        `#${observation.id} ${verdict} failed and its state could not be verified — reload before retrying`,
      )
      break
  }
  if (ok) {
    // Belt-and-suspenders: drop the id again in case a concurrent queue
    // merge re-added it while the verdict was in flight.
    const stale = state.items.findIndex((o) => o.id === observation.id)
    if (stale >= 0) state.items.splice(stale, 1)
  }
  setTimeout(() => delete state.leaving[observation.id], 400)
  return ok
}
