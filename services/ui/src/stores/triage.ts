// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Triage lane v1 (R-C): the fast override surface.
//
// The shape of the gesture, and why it is safe. The operator STAKES a
// verdict per row — one glance, one key — and nothing reaches the server
// until ONE bulk apply, which commits the staked rows as SEQUENTIAL
// per-row preconditioned POSTs. There is no batch verdict route
// (memoire:D146 stands) and each POST carries its own D208 `expected`
// snapshot, so a row another surface moved conflicts instead of being
// silently overwritten.
//
// The D194-safe distinction of record: the danger is "N rows WITHOUT
// per-row attention", not "N rows in one click". Every staked row was
// individually glanced and individually keyed; the apply is bookkeeping.
//
// Asymmetry is deliberate and mistake-tolerant: left = accept,
// right = LAPSE. Lapse is reversible on encounter; reject is not — it 404s
// the row off the read surface — so reject rides neither the fast gesture
// nor the bulk apply: it is staked AND applied one row at a time, through
// its own explicit act.
import { computed, reactive } from 'vue'
import { api, ApiError, preconditionedCall, type Expected } from '../lib/api'
import type { Observation, TriageCounts, TriageRow, TriageStake } from '../lib/types'
import { pushToast } from './toast'

export const triageState = reactive({
  rows: [] as TriageRow[],
  counts: null as TriageCounts | null,
  loaded: false,
  loading: false,
  applying: false,
  project: '' as string,
  // A pool is a FILTER over the scope, never a state of its own (R-A): all
  // four settings read the same rows through the same predicate, three of
  // them narrowed. `de-noise` is the inverted pool — see denoiseMode.
  pool: '' as '' | 'proposed' | 'lapsed' | 'de-noise',
  cursorId: null as number | null,
  // Known genre slugs (judge's and operator's, frequency-first) feeding the
  // manual noise input's autocomplete: convergence at input time, so a
  // genre never fragments across spellings on its way to the graduation
  // evidence count.
  genres: [] as string[],
  // Staked verdicts, keyed by observation id. Nothing here has reached the
  // server; the bulk apply is what commits them.
  stakes: {} as Record<number, TriageStake>,
  // Operator-typed genre slugs for rows the judge did not flag. They ride
  // the lapse verdict's reason_text as `noise:<slug>` — an existing audited
  // field — rather than widening the closed review-action vocabulary.
  noiseGenres: {} as Record<number, string>,
  // Rows whose apply conflicted or failed, kept visible with the reason so
  // an apply never reports a success it did not have.
  failures: [] as { id: number; reason: string }[],
})

const t = triageState

// Suggested verdict for a row, or '' when the judge has not opined. Stage-1
// suggestions only: a stage-2 confirm/elevate suggestion is a different
// question and must not pre-sort this screen.
// Stage-1 suggestions only, and never a COVERAGE row: verdict 'none' means
// the judge evaluated the row and nominated nothing. It exists so re-sweeps
// can skip evaluated rows; rendering it as a suggested verdict would put a
// word in the judge's mouth it did not say.
function stage1Suggestion(row: TriageRow) {
  const s = (row.suggestions ?? []).filter((x) => x.stage === 1 && x.suggested_verdict !== 'none')
  return s.length ? s[s.length - 1] : null
}

export function suggestedVerdict(row: TriageRow): string {
  return stage1Suggestion(row)?.suggested_verdict ?? ''
}

export function suggestedGenre(row: TriageRow): string {
  return stage1Suggestion(row)?.reason_genre ?? ''
}

export function suggestedReason(row: TriageRow): string {
  return stage1Suggestion(row)?.reason_text ?? ''
}

export function suggestedConfidence(row: TriageRow): number | null {
  return stage1Suggestion(row)?.confidence ?? null
}

// --- The DE-NOISE pool: the inversion ---
//
// De-noise is a cheap bulk gesture that runs BEFORE triage, not more rows
// inside it. Every LOADED row arrives condemned — default-staked `noise`
// with the judge's genre — and the operator's work is rescuing the ones that
// are not noise. That is why the stakes are MATERIALIZED on load rather than
// computed: the breakdown, the clear affordance, the visible-rows-only
// pruning and the apply loop are all written against `stakes`, and a
// computed default would have to be threaded through every one of them,
// leaving four places where inverted mode could disagree with itself.
export const denoiseMode = computed(() => t.pool === 'de-noise')

// Below this, a judge's stated confidence is DOUBT: the row is pinned into
// the block the operator reads first rather than riding the batch. The
// number is judgment, not measurement — high enough that a hedged verdict
// gets a human look, low enough that the doubt block stays a block rather
// than becoming the page.
export const DOUBT_CONFIDENCE = 0.75

// A row is DOUBT when the two noise signals disagree, or when the judge
// hedged its own noise call.
//
// A MISSING confidence is not doubt. Every suggestion written before the
// stage-1 v2 prompt carries none, and reading absence as a low number would
// pin the entire existing backlog into the doubt block on the first load —
// which would leave the block saying nothing at all.
export function isDoubt(row: TriageRow): boolean {
  const judgedNoise = suggestedVerdict(row) === 'noise-lapse'
  const flagged = row.observation.noise_candidate === 1
  if (judgedNoise !== flagged) return true
  const c = suggestedConfidence(row)
  return judgedNoise && c !== null && c < DOUBT_CONFIDENCE
}

// The genre a de-noise row is condemned under. Flag-only rows have no judge
// genre, so they are condemned as `unnamed` — honestly nameless rather than
// borrowed from a neighbour.
export function denoiseGenre(row: TriageRow): string {
  return suggestedGenre(row) || 'unnamed'
}

// Rows interleaved ROUND-ROBIN across genres, doubt pinned at the top.
//
// Consecutive rows sharing a genre is what turns a de-noise pass into
// rubber-stamping: twenty `status-narration` rows in a row are read as one
// row, and the twenty-first — which is not noise — rides the batch with
// them. Interleaving forces the eye to re-engage. The rotation is
// DETERMINISTIC, not shuffled: a reload that reorders the page under the
// operator would move the cursor's row out from under a keystroke already
// being aimed.
function roundRobinByGenre(rows: TriageRow[]): TriageRow[] {
  const buckets = new Map<string, TriageRow[]>()
  for (const row of [...rows].sort((a, b) => a.observation.id - b.observation.id)) {
    const genre = denoiseGenre(row)
    const bucket = buckets.get(genre)
    if (bucket) bucket.push(row)
    else buckets.set(genre, [row])
  }
  // Biggest genre first, ties broken by name: both halves are properties of
  // the page, so the same page always renders the same order.
  const ordered = [...buckets.entries()].sort((a, b) => b[1].length - a[1].length || a[0].localeCompare(b[0]))
  const out: TriageRow[] = []
  for (let i = 0; ordered.some(([, b]) => i < b.length); i++) {
    for (const [, bucket] of ordered) if (i < bucket.length) out.push(bucket[i])
  }
  return out
}

// Rows PRE-SORTED by suggested verdict (R-C). The order is the reading
// order the surface wants: the noise the lane is confident about first, so
// the bulk-lapse lane is skimmed and cleared, then the ordinary lapses,
// then the keepers, then the rejects — which are read slowly — and last
// the rows nobody has judged, which need the most attention and get it when
// the cheap ones are gone.
const verdictOrder: Record<string, number> = {
  'noise-lapse': 0,
  lapse: 1,
  accept: 2,
  reject: 3,
  '': 4,
}

// The lane a row sorts into. R-B's default death point starts with the
// EXISTING noise_candidate flag: a row derivation already flagged belongs in
// the bulk-lapse lane whether or not the judge has opined on it. Sorting it
// into "unjudged" would bury the flagged population at the bottom of the
// queue — the exact rows the flag exists to surface.
function laneOf(row: TriageRow): number {
  const v = suggestedVerdict(row)
  if (v) return verdictOrder[v] ?? 4
  return row.observation.noise_candidate ? verdictOrder['noise-lapse'] : 4
}

export const sortedRows = computed(() => {
  if (denoiseMode.value) {
    // Doubt first, visibly marked, then the confident rows interleaved.
    const doubt: TriageRow[] = []
    const confident: TriageRow[] = []
    for (const row of t.rows) (isDoubt(row) ? doubt : confident).push(row)
    return [...roundRobinByGenre(doubt), ...roundRobinByGenre(confident)]
  }
  const rows = [...t.rows]
  rows.sort((a, b) => {
    const la = laneOf(a)
    const lb = laneOf(b)
    if (la !== lb) return la - lb
    return a.observation.id - b.observation.id
  })
  return rows
})

// The BULK apply covers accept / lapse / noise only. Reject is excluded by
// construction: the ruling makes applying a reject an explicit slow per-row
// act, because a rejected row 404s off the read surface and no encounter
// brings it back. Staking a reject is per-row already; bundling the APPLY
// would put the irreversible verdict on the same button as the reversible
// ones, which is the distinction the ruling draws.
export const bulkStakeIDs = computed(() =>
  Object.entries(t.stakes)
    .filter(([, v]) => v !== 'reject')
    .map(([id]) => Number(id)),
)

export const rejectStakeIDs = computed(() =>
  Object.entries(t.stakes)
    .filter(([, v]) => v === 'reject')
    .map(([id]) => Number(id)),
)

// stakedCount is what the BULK apply would commit; totalStaked includes
// rejects, so the breakdown and the clear affordance stay visible when the
// only outstanding stake is a reject.
export const stakedCount = computed(() => bulkStakeIDs.value.length)

export const totalStaked = computed(() => Object.keys(t.stakes).length)

export const stakedBreakdown = computed(() => {
  const out: Record<TriageStake, number> = { accept: 0, lapse: 0, noise: 0, reject: 0, keep: 0 }
  for (const v of Object.values(t.stakes)) out[v]++
  return out
})

// The stakes each pool admits. De-noise is condemned-by-default with two
// ways out — the keep override and the accept escape hatch — and reject is
// absent from it entirely: an irreversible verdict has no place in a gesture
// whose whole premise is that everything on screen is already condemned.
function stakesFor(denoise: boolean): TriageStake[] {
  return denoise ? ['noise', 'keep', 'accept'] : ['accept', 'lapse', 'noise', 'reject']
}

async function loadGenres(): Promise<void> {
  // Best-effort: an empty autocomplete never blocks flagging noise.
  try {
    const res = await api.triageGenres()
    const counts = new Map<string, number>()
    for (const g of res.judge_genres) counts.set(g.genre, (counts.get(g.genre) ?? 0) + Number(g.occurrences ?? 0))
    for (const g of res.operator_genres) counts.set(g.genre, (counts.get(g.genre) ?? 0) + Number(g.count ?? 0))
    t.genres = [...counts.entries()].sort((a, b) => b[1] - a[1]).map(([slug]) => slug)
  } catch {
    /* keep whatever we had */
  }
}

export async function loadTriage(): Promise<void> {
  t.loading = true
  void loadGenres()
  try {
    const denoise = t.pool === 'de-noise'
    const res = await api.triageQueue({
      project: t.project || undefined,
      reviewState: denoise ? undefined : (t.pool as 'proposed' | 'lapsed') || undefined,
      pool: denoise ? 'de-noise' : undefined,
      limit: 100,
    })
    t.rows = res.rows
    t.counts = res.counts
    t.loaded = true
    // Staking survives a filter change only for rows still on screen: a
    // stake on a row the operator can no longer see is a verdict nobody is
    // looking at, which is exactly what this surface must not commit. A
    // stake the NEW pool does not admit goes too — a `keep` left over in an
    // ordinary pool would commit a not-noise on a row nobody condemned.
    const present = new Set(res.rows.map((r) => r.observation.id))
    const admitted = new Set<string>(stakesFor(denoise))
    for (const id of Object.keys(t.stakes).map(Number)) {
      if (!present.has(id) || !admitted.has(t.stakes[id])) {
        delete t.stakes[id]
        delete t.noiseGenres[id]
      }
    }
    if (denoise) materializeDenoiseStakes()
    if (t.cursorId != null && !present.has(t.cursorId)) t.cursorId = null
    if (t.cursorId == null && sortedRows.value.length) t.cursorId = sortedRows.value[0].observation.id
  } catch (e) {
    if (!(e instanceof ApiError && e.status === 401)) pushToast('error', 'failed to load the triage queue')
  } finally {
    t.loading = false
  }
}

// Condemn every loaded row that carries no stake yet. "No stake yet" is the
// load-bearing half: a mid-session reload — the one the apply itself
// triggers — must not overwrite a keep the operator has already made and not
// yet committed.
//
// APPLY SCOPE IS THE LOADED ROWS, and materializing is what guarantees it.
// The pool behind the page limit is untouched: nothing is ever staked on a
// row the operator has not had on screen.
function materializeDenoiseStakes(): void {
  for (const row of t.rows) {
    const id = row.observation.id
    if (t.stakes[id]) continue
    t.stakes[id] = 'noise'
    t.noiseGenres[id] = denoiseGenre(row)
  }
}

// The KEEP override, and its undo. Toggling back re-condemns the row rather
// than leaving it unstaked: in this pool an unstaked row would be a row with
// no verdict at all, which is not one of the two answers the gesture offers.
export function toggleKeep(id: number): void {
  if (t.applying || !denoiseMode.value) return
  if (t.stakes[id] === 'keep') {
    const row = t.rows.find((r) => r.observation.id === id)
    t.stakes[id] = 'noise'
    if (row) t.noiseGenres[id] = denoiseGenre(row)
    return
  }
  t.stakes[id] = 'keep'
  // The genre belonged to the condemnation; a kept row is not being lapsed
  // under it and must not carry it into the next stake.
  delete t.noiseGenres[id]
}

export function stake(id: number, verdict: TriageStake): void {
  if (t.applying) return
  // Re-keying the same verdict UNSTAKES it: the fast gesture needs a cheap
  // undo, or a misfire on a moving cursor is a row the operator cannot take
  // back before the apply.
  if (t.stakes[id] === verdict) {
    // In de-noise mode the undo re-CONDEMNS rather than clearing: an
    // unstaked row there would carry no verdict at all, which is not one of
    // the two answers the inverted gesture offers.
    if (denoiseMode.value) {
      const row = t.rows.find((r) => r.observation.id === id)
      t.stakes[id] = 'noise'
      if (row) t.noiseGenres[id] = denoiseGenre(row)
      return
    }
    delete t.stakes[id]
    if (verdict !== 'noise') delete t.noiseGenres[id]
    return
  }
  t.stakes[id] = verdict
  if (verdict === 'accept') delete t.noiseGenres[id]
}

export function setNoiseGenre(id: number, genre: string): void {
  const slug = genre.trim().toLowerCase().replace(/\s+/g, '-')
  if (!slug) {
    delete t.noiseGenres[id]
    return
  }
  t.noiseGenres[id] = slug
  // Naming a genre IS the noise call: typing a slug and then having to also
  // press a key would make the two halves of one judgment separable, and a
  // genre on an unstaked row would never reach the loop it exists to feed.
  t.stakes[id] = 'noise'
}

export function moveCursor(delta: number): void {
  const rows = sortedRows.value
  if (!rows.length) return
  const i = t.cursorId == null ? -1 : rows.findIndex((r) => r.observation.id === t.cursorId)
  const next = i < 0 ? (delta > 0 ? 0 : rows.length - 1) : Math.min(Math.max(i + delta, 0), rows.length - 1)
  t.cursorId = rows[next].observation.id
}

function expectedFor(o: Observation): Expected {
  return { review_state: o.review_state, provenance: o.provenance, can_instruct: o.can_instruct }
}

// The postcondition a staked verdict is asking for. When it ALREADY holds —
// the TTL-lapsed pool is this lane's headline workload and its rows are
// already `lapsed` — the verdict cannot travel the transition route, because
// lapsed -> lapsed is illegal and must stay so. It does NOT follow that
// there is nothing to do: the operator read the row and ruled on it, and
// silently dropping that ruling loses the encounter, leaves the suggestion
// pending forever, and lets the same row re-pin the first page on every
// load. The encounter rides the D125 append-only action path instead.
function targetState(verdict: TriageStake): string {
  switch (verdict) {
    case 'accept':
      return 'accepted'
    case 'lapse':
    case 'noise':
      return 'lapsed'
    case 'reject':
      return 'rejected'
    case 'keep':
      // A keep asks for NO state. It is an append-only encounter that
      // resolves the noise question and hands the row back to the ordinary
      // queue with its accept-or-lapse question still open — which is why
      // it never travels a transition route, from either state.
      return ''
  }
}

// One row's commit. Every call is preconditioned and its own transaction;
// nothing here batches, so a conflicted row stops exactly itself.
async function applyOne(row: TriageRow, verdict: TriageStake): Promise<string | null> {
  const o = row.observation
  const expected = expectedFor(o)
  const id = o.id
  // KEEP is state-free by construction, from proposed and from lapsed
  // alike: the row's noise signal is answered, nothing about its review
  // state is. It rides the D125 append-only action path — the same path the
  // encounter verbs already use — so no transition is attempted and the
  // legality table is untouched.
  if (verdict === 'keep') {
    return actionOutcome(await preconditionedCall(() => api.reviewAction(id, 'not-noise')))
  }
  // Already where the operator wants it: record the ENCOUNTER as a
  // flag-only action rather than attempting an illegal transition.
  if (o.review_state === targetState(verdict)) {
    if (verdict === 'noise') {
      const slug = t.noiseGenres[id] ?? ''
      return actionOutcome(await preconditionedCall(() =>
        api.reviewAction(id, 'mark-noise', `noise:${slug || 'unnamed'}`)))
    }
    if (verdict === 'lapse') {
      return actionOutcome(await preconditionedCall(() =>
        api.reviewAction(id, 'triage-affirm-lapse')))
    }
    return null
  }
  let call: () => Promise<{ observation: Observation }>
  let landed: (state: string) => boolean
  switch (verdict) {
    case 'accept':
      call = () => api.accept(id, undefined, expected)
      landed = (s) => s === 'accepted'
      break
    case 'lapse':
      call = () => api.lapse(id, undefined, expected)
      landed = (s) => s === 'lapsed'
      break
    case 'noise': {
      // The operator's genre rides the lapse reason as `noise:<slug>`.
      const slug = t.noiseGenres[id] ?? ''
      const reason = slug ? `noise:${slug}` : 'noise:unnamed'
      call = () => api.lapse(id, reason, expected)
      landed = (s) => s === 'lapsed'
      break
    }
    case 'reject':
      call = () => api.reject(id, t.noiseGenres[id] || undefined, expected)
      landed = (s) => s === 'rejected'
      break
  }
  const out = await preconditionedCall(call)
  switch (out.kind) {
    case 'ok':
      return null
    case 'conflict':
      // A conflict whose current state already satisfies what this call
      // asked for is OUR outcome, landed: the response was lost, not the
      // write (memoire:D208's reconciliation protocol).
      return landed(out.current.review_state) ? null : `is ${out.current.review_state} on the server`
    case 'gone':
      return 'no longer exists on the server'
    case 'failed':
      return out.error.message
    case 'unknown':
      return 'transport failed twice — state unverified'
  }
}

// actionOutcome folds a flag-action result. An action changes no
// review_state, so a state conflict here means the row moved under the
// operator — a real failure, not a landed-anyway.
function actionOutcome(out: Awaited<ReturnType<typeof preconditionedCall>>): string | null {
  switch (out.kind) {
    case 'ok':
      return null
    case 'conflict':
      return `moved to ${out.current.review_state} on the server`
    case 'gone':
      return 'no longer exists on the server'
    case 'failed':
      return out.error.message
    case 'unknown':
      return 'transport failed twice — state unverified'
  }
}

// The ONE bulk apply, over the reversible verdicts. SEQUENTIAL by
// construction: the store is a single writer on SQLite, and more
// importantly a failure has to stop being ambiguous — each row's outcome is
// known before the next is attempted.
export async function applyStakes(): Promise<void> {
  if (t.applying || !stakedCount.value) return
  t.applying = true
  t.failures = []
  const byID = new Map(t.rows.map((r) => [r.observation.id, r]))
  let applied = 0
  try {
    // Iterate the STAKES, not the rendered order: a re-sort between staking
    // and applying must not change what gets committed.
    for (const id of bulkStakeIDs.value) {
      const verdict = t.stakes[id]
      const row = byID.get(id)
      if (!row) {
        t.failures.push({ id, reason: 'row left the queue before apply' })
        continue
      }
      const failure = await applyOne(row, verdict)
      if (failure) {
        t.failures.push({ id, reason: failure })
        continue
      }
      applied++
      delete t.stakes[id]
      delete t.noiseGenres[id]
    }
  } finally {
    t.applying = false
  }
  if (t.failures.length) {
    pushToast('error', `applied ${applied}, ${t.failures.length} did not land — see the failures panel`)
  } else {
    pushToast('success', `applied ${applied} verdict${applied === 1 ? '' : 's'}`)
  }
  await loadTriage()
}

// Applying ONE reject. Its own act, one row at a time, never bundled:
// reject means the observation is FALSE, the row leaves the read surface,
// and no later encounter brings it back.
export async function applyReject(id: number): Promise<void> {
  if (t.applying || t.stakes[id] !== 'reject') return
  const row = t.rows.find((r) => r.observation.id === id)
  if (!row) return
  t.applying = true
  try {
    const failure = await applyOne(row, 'reject')
    if (failure) {
      t.failures = [...t.failures.filter((f) => f.id !== id), { id, reason: failure }]
      pushToast('error', `reject #${id} did not land — ${failure}`)
      return
    }
    delete t.stakes[id]
    delete t.noiseGenres[id]
    pushToast('success', `rejected #${id}`)
  } finally {
    t.applying = false
  }
  await loadTriage()
}

export function clearStakes(): void {
  if (t.applying) return
  t.stakes = {}
  t.noiseGenres = {}
  // In the inverted pool, "clear" means back to the default — which is
  // everything condemned, not everything unstaked. An empty de-noise page
  // would silently disarm the apply button and read as a cleared batch.
  if (denoiseMode.value) materializeDenoiseStakes()
}
