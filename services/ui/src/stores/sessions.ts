// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Slice E (memoire:D146, E3): the session review surface. One page = one
// session's story; the backlog is a deposit to skim (catchup), not a debt
// repaid one row at a time.
import { computed, reactive } from 'vue'
import { api, ApiError, preconditionedCall, type ConflictCurrent, type Expected } from '../lib/api'
import type { Observation, SessionSummary } from '../lib/types'
import { needsConfirmChain } from '../lib/verdicts'
import { pushToast } from './toast'

export const sessionsState = reactive({
  sessions: [] as SessionSummary[],
  loaded: false,
  loading: false,
  project: '' as string,
  withWorkOnly: true,
  selected: '' as string,
  story: [] as Observation[],
  storyLoading: false,
  // True when the story hit the page-loop safety cap: bulk lapse is
  // disabled because the server would mutate rows the operator never saw.
  storyTruncated: false,
  // ids with an elevate chain in flight — double-click protection for the
  // two-call confirm+elevate sequence (round-1 review finding 2: without
  // it, two clicks wrote duplicate confirm rows and a contradictory
  // success+409 pair).
  elevationPending: {} as Record<number, true>,
  // Story row the keyboard cursor sits on (null = no cursor). Lives in the
  // store so id-jump (TopBar #id) can place it before the view mounts.
  cursorId: null as number | null,
  // ids with any verdict call in flight — keyboard repeat/duplicate guard
  // for a/c/r/l (review 2026-07-30 finding 2; elevate has its own chain
  // guard below).
  verdictPending: {} as Record<number, true>,
  // Noise visibility (operator ask 2026-07-31): the story FETCH always
  // includes noise — complete membership is what keeps refreshCounts and the
  // bulk-lapse guard honest — but flagged rows are hidden from the page by
  // default. Store-level so the choice survives tab hops.
  showNoise: false,
})

// What the story page actually renders. Everything that mutates or counts
// (refreshCounts, lapseRemaining, storyGen identity) stays on the FULL
// story; only display and cursor movement use this projection.
export const visibleStory = computed(() =>
  sessionsState.showNoise
    ? sessionsState.story
    : sessionsState.story.filter((o) => !o.noise_candidate),
)

export const storyNoiseCount = computed(
  () => sessionsState.story.filter((o) => o.noise_candidate).length,
)

// Hidden-but-proposed noise: "lapse remaining" acts server-side on the whole
// session, so the operator is told when rows they cannot see would lapse.
export const hiddenProposedNoise = computed(() =>
  sessionsState.showNoise
    ? 0
    : sessionsState.story.filter((o) => o.noise_candidate && o.review_state === 'proposed')
        .length,
)

export const projects = computed(() => {
  const set = new Set<string>()
  for (const s of sessionsState.sessions) set.add(s.project)
  return [...set].sort()
})

// Last-request-wins identity for summary loads: mutation-triggered refetches
// and filter changes can overlap, and an older snapshot committing late must
// not overwrite a newer one (review round-5).
let summariesSeq = 0

export async function loadSessions(): Promise<void> {
  const seq = ++summariesSeq
  sessionsState.loading = true
  try {
    const res = await api.listSessions({
      project: sessionsState.project || undefined,
      proposedOnly: sessionsState.withWorkOnly,
      limit: 100,
    })
    if (seq !== summariesSeq) return
    sessionsState.sessions = res.sessions
    sessionsState.loaded = true
  } catch (e) {
    // A superseded request publishes no terminal UI effects — including
    // this toast (review round-6 minor).
    if (seq === summariesSeq && !(e instanceof ApiError && e.status === 401))
      pushToast('error', 'failed to load sessions')
  } finally {
    if (seq === summariesSeq) sessionsState.loading = false
  }
}

// Monotonic identity for NAVIGATION INTENTS: session clicks, n/p hops, and
// #id jumps all race, and the LAST GESTURE must win — a slow earlier load
// (or a jump's pre-story by-id lookup, review round-2 residual) finishing
// late must never overwrite where the operator last asked to be.
let navSeq = 0

// Resolves true only when THIS intent's story committed (still latest, no
// load error) — the #id jump needs that distinction. Callers with an intent
// claimed BEFORE earlier awaits (the jump) pass it in; direct gestures
// (session click, n/p) claim the newest here.
export async function openSession(sessionID: string, intent?: number): Promise<boolean> {
  const seq = intent ?? ++navSeq
  if (seq !== navSeq) return false // a newer gesture already superseded this one
  sessionsState.selected = sessionID
  sessionsState.storyLoading = true
  sessionsState.story = []
  sessionsState.storyTruncated = false
  sessionsState.cursorId = null
  try {
    // Follow cursors until the story is COMPLETE: bulk lapse acts on the
    // whole session server-side, so the page must show the whole session.
    const all: Observation[] = []
    let cursor: string | undefined
    for (let page = 0; page < 25; page++) {
      const res = await api.sessionObservations(sessionID, cursor)
      all.push(...res.observations)
      if (!res.next_cursor) {
        cursor = undefined
        break
      }
      cursor = res.next_cursor
    }
    if (seq !== navSeq) return false // stale: a newer gesture owns the state
    sessionsState.storyTruncated = Boolean(cursor)
    sessionsState.story = all
    // A fresh story is the truth for this session's badges — this is also
    // what lets a verdict that completed after a hop show correctly on
    // return (review round-3: "refreshes on next open" must be real).
    refreshCounts()
    return true
  } catch {
    if (seq === navSeq) pushToast('error', 'failed to load session story')
    return false
  } finally {
    if (seq === navSeq) sessionsState.storyLoading = false
  }
}

function replaceInStory(obs: Observation): void {
  const i = sessionsState.story.findIndex((o) => o.id === obs.id)
  if (i >= 0) sessionsState.story[i] = obs
}

// The story ARRAY REFERENCE is the story's generation identity: openSession
// replaces it on start (empty) and on commit (fresh rows), so a captured
// reference distinguishes "same displayed story" from a hop AND from a
// same-session reload (review round-3: session id alone was not enough —
// a reload mid-verdict recounted an emptied story as zero).
function storyGen(): Observation[] {
  return sessionsState.story
}

// Preconditioned verdicts (memoire:D207): every row mutation sends the
// row's snapshot as an `expected` precondition and runs through
// preconditionedCall (lib/api). The server's unified 409 carries the row's
// current metadata — it replaces the old truth-read/grace-wait
// reconciliation, and works even for states the read surface hides (a
// landed reject reads as current.review_state "rejected").
async function verdictCall(
  id: number,
  fn: () => Promise<{ observation: Observation }>,
  label: string,
  // The REQUESTED postcondition (round-2 review MAJOR 1): a conflict counts
  // as our success only when its current metadata satisfies what THIS call
  // asked for — movement alone proves another actor, not our verdict.
  landedMeta: (c: ConflictCurrent) => boolean,
): Promise<Observation | null> {
  // A verdict completes against the story generation it STARTED in; a
  // completion landing after a hop or reload mutates nothing here — the
  // fresh story commit (openSession) recounts from server truth.
  const gen = storyGen()
  const rowAtCall = sessionsState.story.find((o) => o.id === id)
  const out = await preconditionedCall(fn)
  switch (out.kind) {
    case 'ok':
      if (storyGen() === gen) {
        replaceInStory(out.observation)
        refreshCounts()
      }
      return out.observation
    case 'conflict': {
      // The story shows server truth either way (gen-guarded patch of the
      // metadata fields; content is untouched).
      let patched: Observation | null = null
      if (storyGen() === gen) {
        const cur = sessionsState.story.find((o) => o.id === id)
        if (cur) {
          cur.review_state = out.current.review_state
          cur.provenance = out.current.provenance
          cur.can_instruct = out.current.can_instruct
          patched = cur
        }
        refreshCounts()
      }
      if (landedMeta(out.current)) {
        pushToast('success', `${label} #${id} landed (verified via state conflict)`)
        if (patched) return patched
        return rowAtCall
          ? {
              ...rowAtCall,
              review_state: out.current.review_state,
              provenance: out.current.provenance,
              can_instruct: out.current.can_instruct,
            }
          : null
      }
      pushToast('error', `#${id} is ${out.current.review_state} on the server — ${label} not applied`)
      return null
    }
    case 'gone':
      // The row no longer exists server-side: remove it and recount instead
      // of leaving a proven non-member proposed and actionable (round-5
      // MAJOR 2), with OWNER-guarded cursor repair (round-6 MAJOR 1) — the
      // cursor lands on the row that slid into the removed row's visible
      // position so the j/verdict rhythm continues to the neighbor.
      if (storyGen() === gen) {
        const visIndex = visibleStory.value.findIndex((o) => o.id === id)
        const i = sessionsState.story.findIndex((o) => o.id === id)
        if (i >= 0) sessionsState.story.splice(i, 1)
        refreshCounts()
        if (sessionsState.cursorId === id) {
          const after = visibleStory.value
          sessionsState.cursorId = after.length
            ? after[Math.min(Math.max(visIndex, 0), after.length - 1)].id
            : null
        }
      }
      pushToast('error', `#${id} no longer exists on the server — ${label} not applied`)
      return null
    case 'failed':
      pushToast('error', `${label} failed: ${out.error.message}`)
      return null
    case 'unknown':
      pushToast('error', `${label} failed and #${id} state could not be verified — reload before retrying`)
      return null
  }
}

// The gesture's snapshot, captured ONCE and by VALUE (precondition review
// round-1 BLOCKER 2): the closure passed to preconditionedCall runs twice
// on a transport retry, so it must never re-read mutable UI state — and it
// carries ALL THREE fields, because review_state alone cannot serialize
// confirm-on-accepted (the state does not change; the provenance does).
function expectedFor(id: number): Expected | undefined {
  const row = sessionsState.story.find((o) => o.id === id)
  return row
    ? { review_state: row.review_state, provenance: row.provenance, can_instruct: row.can_instruct }
    : undefined
}

export function acceptRow(id: number, reason?: string) {
  const expected = expectedFor(id)
  return verdictCall(
    id,
    () => api.accept(id, reason, expected),
    'accept',
    (c) => c.review_state === 'accepted',
  )
}
export function confirmRow(id: number, reason?: string) {
  const expected = expectedFor(id)
  return verdictCall(
    id,
    () => api.confirm(id, reason, expected),
    'confirm',
    (c) => c.review_state === 'accepted' && c.provenance === 'user_confirmed',
  )
}
export function rejectRow(id: number, reason?: string) {
  const expected = expectedFor(id)
  return verdictCall(
    id,
    () => api.reject(id, reason, expected),
    'reject',
    (c) => c.review_state === 'rejected',
  )
}
export function lapseRow(id: number, reason?: string) {
  const expected = expectedFor(id)
  return verdictCall(
    id,
    () => api.lapse(id, reason, expected),
    'lapse',
    (c) => c.review_state === 'lapsed',
  )
}
export async function elevateRow(id: number, reason?: string) {
  // Chained confirm+elevate (memoire:D192): a row without vouched
  // provenance is confirmed first, then elevated — two review-log rows, the
  // double-gesture invariant intact. Each leg is its own preconditioned
  // call: a confirm that landed without the elevate ever being sent is
  // reported as a confirm, never as an elevation (round-2 review MAJOR 1).
  // Pending-guarded: the chain must never run twice for one id.
  if (sessionsState.elevationPending[id]) return null
  sessionsState.elevationPending[id] = true
  try {
    const row = sessionsState.story.find((o) => o.id === id)
    // Shared chain predicate (lib/verdicts): proposed/lapsed rows need the
    // confirm for the ACCEPT as much as for the vouch — elevate 409s below
    // accepted.
    if (row != null && needsConfirmChain(row)) {
      // Captured once, by value — the retry must send the SAME snapshot
      // (round-1 BLOCKER 2).
      const expected = expectedFor(id)
      const confirmed = await verdictCall(
        id,
        () => api.confirm(id, reason, expected),
        'confirm',
        (c) => c.review_state === 'accepted' && c.provenance === 'user_confirmed',
      )
      if (!confirmed) return null
      if (confirmed.can_instruct === 1) return confirmed // already fully elevated
    }
    return await verdictCall(id, () => api.elevate(id, reason), 'elevate', (c) => c.can_instruct === 1)
  } finally {
    delete sessionsState.elevationPending[id]
  }
}

export async function rewriteRow(id: number, text: string, reason?: string): Promise<boolean> {
  const gen = storyGen()
  try {
    const res = await api.rewrite(id, text, reason)
    // The original is superseded; show the operator's new row in its place —
    // but only in the story generation the rewrite started in (round-3
    // sibling sweep); a later generation gets server truth on its own load.
    if (storyGen() === gen) {
      const i = sessionsState.story.findIndex((o) => o.id === id)
      if (i >= 0) {
        sessionsState.story[i] = { ...sessionsState.story[i], review_state: 'superseded' }
        sessionsState.story.splice(i + 1, 0, res.observation)
      }
      refreshCounts()
    }
    // Summary totals are server-owned: no client-side delta arithmetic —
    // a delta can be lost after a hop or double-applied under a racing
    // list refresh (review round-5). Refetch is read-after-write
    // consistent and seq-guarded against stale overwrites.
    void loadSessions()
    pushToast('success', `rewritten as #${res.observation.id}`)
    return true
  } catch (e) {
    pushToast('error', `rewrite failed${e instanceof ApiError ? `: ${e.message}` : ''}`)
    return false
  }
}

export async function lapseRemaining(reason?: string): Promise<void> {
  const sessionID = sessionsState.selected
  if (!sessionID) return
  if (sessionsState.storyTruncated) {
    pushToast('error', 'story truncated — bulk lapse disabled (rows you have not seen would lapse)')
    return
  }
  const gen = storyGen()
  try {
    const res = await api.lapseRemaining(sessionID, reason)
    pushToast('success', `${res.lapsed_count} lapsed`)
    // Touch story rows only if the displayed story is still the generation
    // the bulk lapse started in — hopping A→B mid-flight must never paint
    // B's rows lapsed (round-3 sibling sweep). Summary badges refetch below.
    if (storyGen() === gen) {
      for (const o of sessionsState.story) {
        if (o.review_state === 'proposed') o.review_state = 'lapsed'
      }
    }
    // Summary badges come from the server, not from local arithmetic — a
    // racing list refresh could double-count a delta, and a hop could lose
    // one (review round-5). Seq-guarded refetch, read-after-write.
    void loadSessions()
  } catch {
    pushToast('error', 'bulk lapse failed')
  }
}

// Keep the list's workload badges honest after row verdicts without a
// refetch — but ONLY the aggregates the story projection has complete
// membership for (review round-4): the story carries accepted/proposed/
// lapsed rows, so proposed/lapsed counts are recomputable from an
// UNTRUNCATED story; observation_count also spans rejected/superseded/
// disputed and is refreshed only from server snapshots (loadSessions).
function refreshCounts(): void {
  if (sessionsState.storyTruncated) return // a prefix is not a whole-session basis
  const s = sessionsState.sessions.find((x) => x.session_id === sessionsState.selected)
  if (!s) return
  s.proposed_count = sessionsState.story.filter((o) => o.review_state === 'proposed').length
  s.lapsed_count = sessionsState.story.filter((o) => o.review_state === 'lapsed').length
}

export const remainingProposed = computed(
  () => sessionsState.story.filter((o) => o.review_state === 'proposed').length,
)

// Id-jump (TopBar "#id"): land on the observation IN ITS SESSION STORY —
// the surface that already carries every verdict affordance — rather than
// growing a parallel detail surface. Success means the row is actually
// displayed with the cursor on it (review 2026-07-30 finding 3): a failed
// or stale story load, a raw event (stories exclude raw), or a row past
// the truncation cap all report failure with a reason, never a silent
// tab-switch to the wrong place.
// A jump that claimed the navigation intent but failed BEFORE owning a
// story load has silenced any in-flight load without replacing it: the
// superseded load may no longer clear storyLoading, so the view would sit
// on its spinner forever (review round-3 new blocker). If we still hold
// the latest intent and a load is stranded, reload the selected session
// under a fresh intent; otherwise the state belongs to someone newer.
function recoverInterruptedLoad(intent: number): void {
  if (intent !== navSeq) return
  if (sessionsState.storyLoading && sessionsState.selected) {
    void openSession(sessionsState.selected)
  }
}

export async function jumpToObservation(id: number): Promise<boolean> {
  // Claim the navigation intent BEFORE the by-id lookup: two jumps whose
  // lookups resolve out of order, or a session click landing mid-lookup,
  // must leave the LAST gesture in control (round-2 residual of finding 1).
  const intent = ++navSeq
  try {
    const res = await api.getObservation(id)
    if (intent !== navSeq) return false // superseded while looking up
    if (!res.observation.session_id) {
      pushToast('error', `#${id} has no session — not reachable on the sessions surface`)
      recoverInterruptedLoad(intent)
      return false
    }
    if (res.observation.kind === 'raw') {
      pushToast('error', `#${id} is a raw event — session stories show semantic observations only`)
      recoverInterruptedLoad(intent)
      return false
    }
    if (!(await openSession(res.observation.session_id, intent))) return false
    const row = sessionsState.story.find((o) => o.id === id)
    if (!row) {
      pushToast(
        'error',
        `#${id} is not in its session story${sessionsState.storyTruncated ? ' (story truncated at the page cap)' : ''}`,
      )
      return false
    }
    // A jump must land on a VISIBLE row: reveal noise rather than parking
    // the cursor on a hidden one (operator ask 2026-07-31).
    if (row.noise_candidate && !sessionsState.showNoise) sessionsState.showNoise = true
    sessionsState.cursorId = id
    return true
  } catch (e) {
    // A superseded lookup failing late must stay silent — the operator has
    // already moved on (round-3: stale "not found" toast).
    if (intent === navSeq) {
      pushToast('error', `#${id} not found${e instanceof ApiError ? `: ${e.message}` : ''}`)
      recoverInterruptedLoad(intent)
    }
    return false
  }
}
