// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Triage lane v1, STAGE 2 (R-C): confirm/elevation candidates over the
// HUMAN-validated set.
//
// The scope is server-side: rows carrying a human accept in the review log.
// Rules-born accepted rows are excluded there — they were accepted at birth
// by a rule and nobody validated them, so they are not material for a vouch
// or a standing order.
//
// Confirm and elevation stay STRUCTURALLY HUMAN (memoire:D146/D191's double
// gesture). The judge suggests; nothing here promotes provenance or flips
// can_instruct without a per-row human stake, and elevation on a row without
// vouched provenance chains the confirm first (memoire:D192).
//
// `decline` is a real stake, not the absence of one. A suggestion the
// operator read and turned down has to leave "pending" or the override rate
// carries a class it can never read.
import { computed, reactive } from 'vue'
import { api, ApiError, preconditionedCall, type Expected } from '../lib/api'
import type { Observation, TriageRow, TriageStage2Stake } from '../lib/types'
import { pushToast } from './toast'

export const stage2State = reactive({
  rows: [] as TriageRow[],
  loaded: false,
  loading: false,
  applying: false,
  project: '' as string,
  cursorId: null as number | null,
  stakes: {} as Record<number, TriageStage2Stake>,
  failures: [] as { id: number; reason: string }[],
})

const t = stage2State

// Coverage rows (verdict 'none') are excluded: the judge evaluated the row
// and nominated nothing, which is not a suggestion to render or act on.
export function stage2Suggestion(row: TriageRow) {
  const s = (row.suggestions ?? []).filter((x) => x.stage === 2 && x.suggested_verdict !== 'none')
  return s.length ? s[s.length - 1] : null
}

// Suggested rows first — those are the ones the judge has an opinion about
// and the ones whose override rate the A/B measures.
export const stage2Rows = computed(() => {
  const rows = [...t.rows]
  rows.sort((a, b) => {
    const sa = stage2Suggestion(a) ? 0 : 1
    const sb = stage2Suggestion(b) ? 0 : 1
    if (sa !== sb) return sa - sb
    return a.observation.id - b.observation.id
  })
  return rows
})

export const stage2StakedCount = computed(() => Object.keys(t.stakes).length)

export async function loadStage2(): Promise<void> {
  t.loading = true
  try {
    const res = await api.triageStage2Queue({ project: t.project || undefined, limit: 100 })
    t.rows = res.rows
    t.loaded = true
    const present = new Set(res.rows.map((r) => r.observation.id))
    for (const id of Object.keys(t.stakes).map(Number)) {
      if (!present.has(id)) delete t.stakes[id]
    }
    if (t.cursorId != null && !present.has(t.cursorId)) t.cursorId = null
    if (t.cursorId == null && stage2Rows.value.length) t.cursorId = stage2Rows.value[0].observation.id
  } catch (e) {
    if (!(e instanceof ApiError && e.status === 401)) pushToast('error', 'failed to load the validated set')
  } finally {
    t.loading = false
  }
}

export function stage2Stake(id: number, verdict: TriageStage2Stake): void {
  if (t.applying) return
  if (t.stakes[id] === verdict) {
    delete t.stakes[id]
    return
  }
  t.stakes[id] = verdict
}

export function stage2MoveCursor(delta: number): void {
  const rows = stage2Rows.value
  if (!rows.length) return
  const i = t.cursorId == null ? -1 : rows.findIndex((r) => r.observation.id === t.cursorId)
  const next = i < 0 ? (delta > 0 ? 0 : rows.length - 1) : Math.min(Math.max(i + delta, 0), rows.length - 1)
  t.cursorId = rows[next].observation.id
}

function expectedFor(o: Observation): Expected {
  return { review_state: o.review_state, provenance: o.provenance, can_instruct: o.can_instruct }
}

async function applyOne(row: TriageRow, verdict: TriageStage2Stake): Promise<string | null> {
  const o = row.observation
  const id = o.id
  if (verdict === 'decline') {
    // Nothing changes on the row — the entry IS the record that the
    // operator read the suggestion and turned it down.
    const out = await preconditionedCall(() => api.reviewAction(id, 'triage-decline-elevation'))
    return out.kind === 'ok' ? null : failureReason(out)
  }
  if (verdict === 'confirm') {
    // Already vouched: the apply is a no-op on the ROW, but the operator's
    // encounter is not. Recording it is what stops the suggestion sitting
    // pending forever and the row returning on every load.
    if (o.provenance === 'user_confirmed') return affirmCurrent(id)
    const out = await preconditionedCall(() => api.confirm(id, undefined, expectedFor(o)))
    if (out.kind === 'conflict' && out.current.provenance === 'user_confirmed') return affirmCurrent(id)
    return out.kind === 'ok' ? null : failureReason(out)
  }
  // Elevate. memoire:D192's chain: a row without vouched provenance is
  // confirmed FIRST — two calls, two review-log rows, the double-gesture
  // invariant intact — because elevate 409s below a vouched provenance.
  if (o.can_instruct === 1) return affirmCurrent(id)
  if (o.provenance !== 'user_confirmed') {
    const chain = await preconditionedCall(() => api.confirm(id, undefined, expectedFor(o)))
    if (chain.kind === 'ok') {
      o.provenance = chain.observation.provenance
      o.review_state = chain.observation.review_state
    } else if (chain.kind === 'conflict' && chain.current.provenance === 'user_confirmed') {
      o.provenance = chain.current.provenance
    } else {
      return failureReason(chain)
    }
  }
  const out = await preconditionedCall(() => api.elevate(id))
  if (out.kind === 'conflict' && out.current.can_instruct === 1) return affirmCurrent(id)
  return out.kind === 'ok' ? null : failureReason(out)
}

// affirmCurrent records the encounter with a row that already says what the
// judge suggested. Same append-only action path as every other triage
// encounter — no new mechanism.
async function affirmCurrent(id: number): Promise<string | null> {
  const out = await preconditionedCall(() => api.reviewAction(id, 'triage-affirm-current'))
  return out.kind === 'ok' ? null : failureReason(out)
}

function failureReason(out: Awaited<ReturnType<typeof preconditionedCall>>): string {
  switch (out.kind) {
    case 'ok':
      return ''
    case 'conflict':
      return `is ${out.current.review_state}/${out.current.provenance} on the server`
    case 'gone':
      return 'no longer exists on the server'
    case 'failed':
      return out.error.message
    case 'unknown':
      return 'transport failed twice — state unverified'
  }
}

// Bulk apply over individually-glanced rows: sequential per-row calls, each
// preconditioned. No batch route exists and none is used.
export async function applyStage2(): Promise<void> {
  if (t.applying || !stage2StakedCount.value) return
  t.applying = true
  t.failures = []
  const byID = new Map(t.rows.map((r) => [r.observation.id, r]))
  let applied = 0
  try {
    for (const [key, verdict] of Object.entries(t.stakes)) {
      const id = Number(key)
      const row = byID.get(id)
      if (!row) {
        t.failures.push({ id, reason: 'row left the set before apply' })
        continue
      }
      const failure = await applyOne(row, verdict)
      if (failure) {
        t.failures.push({ id, reason: failure })
        continue
      }
      applied++
      delete t.stakes[id]
    }
  } finally {
    t.applying = false
  }
  if (t.failures.length) {
    pushToast('error', `applied ${applied}, ${t.failures.length} did not land — see the failures panel`)
  } else {
    pushToast('success', `applied ${applied} stage-2 verdict${applied === 1 ? '' : 's'}`)
  }
  await loadStage2()
}

export function clearStage2Stakes(): void {
  if (t.applying) return
  t.stakes = {}
}
