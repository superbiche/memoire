<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../lib/api'
import type { Observation, TimelineHit } from '../lib/types'
import { fmtDate } from '../lib/format'
import { applyElevation, state } from '../stores/review'
import KindBadge from './KindBadge.vue'
import ElevatedBadge from './ElevatedBadge.vue'
import DetailPanel from './DetailPanel.vue'

const hits = ref<TimelineHit[]>([])
const loading = ref(false)
const error = ref('')
const project = ref('')
const limit = ref(100)
// Narrows to already-elevated observations (can_instruct=1). A filter over
// past elevations only — deliberately NOT an elevation-candidate surface.
const elevatedOnly = ref(false)

const expanded = ref<number | null>(null)
const detailCache = reactive(new Map<number, Observation>())

// Browse's detail-panel moment: the selected row's full observation, once
// fetched. This is where the operator elevates a past-accepted observation —
// still detail-panel-only, still operator-pull.
const selectedObs = computed<Observation | null>(() =>
  expanded.value !== null ? (detailCache.get(expanded.value) ?? null) : null,
)

// Monotonic token: only the latest load may commit its response, so a slow
// older request can never overwrite a newer filter's list — with a live
// detail panel a stale list is no longer harmless.
let loadSeq = 0

// Snapshot of the filters the CURRENT hits actually answer (committed only
// on a successful load): the hygiene banner must never label one project's
// hits with another project's name typed into the draft input, nor survive
// a failed refresh (round-1 review finding 3).
const appliedProject = ref('')
const appliedLimit = ref(0)
const lastLoadOk = ref(false)

async function load() {
  const seq = ++loadSeq
  loading.value = true
  error.value = ''
  // Captured BEFORE the await: the applied snapshot must record what the
  // REQUEST asked for, not whatever the draft refs say when the response
  // lands (round-2 finding: an in-flight draft edit relabeled project A's
  // hits as project B). The same captures build the request itself.
  const requestedProject = project.value
  const requestedLimit = limit.value
  try {
    const res = await api.timeline({
      project: requestedProject || undefined,
      limit: requestedLimit,
      elevatedOnly: elevatedOnly.value,
    })
    if (seq !== loadSeq) return
    // An empty result set marshals as JSON null, not [] — never let that
    // reach the render path (round-1 review finding 1; same guard as the
    // review store's window fetch).
    const nextHits = res.hits ?? []
    hits.value = nextHits
    appliedProject.value = requestedProject
    appliedLimit.value = requestedLimit
    lastLoadOk.value = true
    // Drop cached full records the fresh timeline contradicts — a stale
    // review_state/can_instruct must not drive the elevation controls.
    for (const hit of nextHits) {
      const cached = detailCache.get(hit.id)
      if (cached && (cached.review_state !== hit.review_state || cached.can_instruct !== hit.can_instruct)) {
        detailCache.delete(hit.id)
      }
    }
    if (expanded.value !== null) {
      if (!nextHits.some((h) => h.id === expanded.value)) {
        expanded.value = null
      } else if (!detailCache.has(expanded.value)) {
        await fetchDetail(expanded.value)
      }
    }
  } catch (e) {
    if (seq !== loadSeq) return
    lastLoadOk.value = false
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

// Per-id fetch tokens: only the latest GET for an id may commit, so a slow
// older detail fetch can never overwrite a fresher record (including one
// just written by an elevate/demote response, which bumps the token).
const detailSeq = new Map<number, number>()

function commitDetail(id: number, observation: Observation) {
  detailSeq.set(id, (detailSeq.get(id) ?? 0) + 1)
  detailCache.set(id, observation)
}

async function fetchDetail(id: number) {
  const seq = (detailSeq.get(id) ?? 0) + 1
  detailSeq.set(id, seq)
  try {
    const res = await api.getObservation(id)
    if (detailSeq.get(id) === seq) detailCache.set(id, res.observation)
  } catch {
    // Snippet stays as the fallback; nothing to surface here.
  }
}

async function toggle(id: number) {
  if (expanded.value === id) {
    expanded.value = null
    return
  }
  expanded.value = id
  if (!detailCache.has(id)) await fetchDetail(id)
}

async function elevation(elevated: boolean, reason?: string) {
  // A reload in flight means the selection context may be about to change;
  // refuse rather than act on it (controls are also disabled via
  // elevation-busy while loading).
  if (loading.value) return
  const obs = selectedObs.value
  if (!obs) return
  const res = await applyElevation(obs, elevated, reason || undefined)
  // A definitive 404 means the row is purged (round-4 review MAJOR 1):
  // EVICT the local record directly — eviction must not depend on the
  // follow-up reload succeeding (round-5: a transient reload failure left
  // the proven-absent row selected with live controls) — then reload
  // best-effort for surrounding membership.
  if (res.gone) {
    hits.value = hits.value.filter((h) => h.id !== obs.id)
    detailCache.delete(obs.id)
    if (expanded.value === obs.id) expanded.value = null
    await load()
    return
  }
  // The freshest known truth applies even when the request did NOT land
  // (round-3 review NEW MAJOR 1): a conflict told us what the row IS, and
  // a cached accepted row must not keep offering Elevate against a server
  // that would refuse it.
  const updated = res.observation
  if (!updated) return
  commitDetail(updated.id, updated)
  const hit = hits.value.find((h) => h.id === updated.id)
  if (hit) {
    hit.can_instruct = updated.can_instruct
    hit.review_state = updated.review_state
  }
  // Membership: the timeline carries accepted/proposed/lapsed semantic
  // rows; truth outside that band — or a demote under elevated-only — no
  // longer belongs, so reload keeps the list honest (load() also clears a
  // vanished selection).
  if (
    (elevatedOnly.value && !updated.can_instruct) ||
    !['accepted', 'proposed', 'lapsed'].includes(updated.review_state)
  )
    await load()
}

onMounted(load)
</script>

<template>
  <div class="mx-auto flex h-full max-w-7xl flex-col gap-3 px-4 py-4 sm:px-6">
    <div class="flex flex-wrap items-center gap-2">
      <input
        v-model="project"
        type="text"
        placeholder="project filter…"
        class="w-44 rounded-lg border border-zinc-200 bg-white px-3 py-1.5 font-mono text-xs outline-none focus:border-violet-400 focus:ring-2 focus:ring-violet-400/30 dark:border-zinc-800 dark:bg-zinc-900"
        @keydown.enter="load"
      />
      <select
        v-model.number="limit"
        class="rounded-lg border border-zinc-200 bg-white px-2 py-1.5 text-xs dark:border-zinc-800 dark:bg-zinc-900"
        @change="load"
      >
        <option :value="50">50</option>
        <option :value="100">100</option>
        <option :value="200">200</option>
      </select>
      <button
        class="rounded-lg bg-violet-600 px-3 py-1.5 text-xs font-medium text-white transition hover:bg-violet-500"
        @click="load"
      >
        Refresh
      </button>
      <button
        type="button"
        class="rounded-full border px-2.5 py-1 text-xs transition"
        :class="
          elevatedOnly
            ? 'border-violet-400/60 bg-violet-50 text-violet-700 dark:bg-violet-500/15 dark:text-violet-300'
            : 'border-zinc-200 text-zinc-500 hover:border-zinc-300 hover:text-zinc-700 dark:border-zinc-800 dark:hover:border-zinc-700 dark:hover:text-zinc-300'
        "
        title="show only elevated observations (the operating-notes view)"
        @click="((elevatedOnly = !elevatedOnly), load())"
      >
        ↑ elevated only
      </button>
      <span class="ml-auto text-xs text-zinc-400">
        accepted + unreviewed (proposed/lapsed, <span class="font-mono">~</span>), raw excluded — the injection view
      </span>
    </div>

    <!-- Standing-brief hygiene (memoire:D191/D192): the elevated-only view
         is where brief overflow is ACTED on, so the count-vs-cap lives
         here. The injection cap is a client knob (default 10); the newest
         cap rides, older standing orders are benched (memoire:D194).
         Rendered from the APPLIED snapshot only — never the draft filter —
         and only after a successful load; a saturated window states a
         lower bound, not an exact count. -->
    <div
      v-if="elevatedOnly && !loading && lastLoadOk && project === appliedProject"
      class="rounded-lg border px-3 py-1.5 text-xs"
      :class="
        appliedProject && hits.length > 10
          ? 'border-amber-300 bg-amber-50 text-amber-700 dark:border-amber-500/40 dark:bg-amber-500/10 dark:text-amber-300'
          : 'border-zinc-200 text-zinc-500 dark:border-zinc-800 dark:text-zinc-400'
      "
    >
      <template v-if="appliedProject && hits.length >= appliedLimit">
        ⚠ at least {{ hits.length }} elevated in {{ appliedProject }} (window full) — the
        injection cap defaults to 10; older standing orders are benched (not riding
        sessions). Demote until the brief fits.
      </template>
      <template v-else-if="appliedProject && hits.length > 10">
        ⚠ {{ hits.length }} elevated in {{ appliedProject }} — the injection cap defaults to 10,
        so the {{ hits.length - 10 }} oldest standing order{{ hits.length - 10 === 1 ? ' is' : 's are' }}
        benched (not riding sessions). Demote until the brief fits.
      </template>
      <template v-else-if="appliedProject">
        {{ hits.length }} elevated in {{ appliedProject }} — all riding (injection cap defaults
        to 10)
      </template>
      <template v-else>
        {{ hits.length >= appliedLimit ? 'at least ' : '' }}{{ hits.length }} elevated across
        all projects — the cap applies per project; filter to check one brief
      </template>
    </div>

    <div
      v-if="error"
      class="rounded-xl border border-rose-200 bg-rose-50 p-4 text-sm text-rose-700 dark:border-rose-500/30 dark:bg-rose-500/10 dark:text-rose-300"
    >
      {{ error }}
    </div>

    <div v-else class="grid min-h-0 flex-1 grid-cols-1 gap-4 lg:grid-cols-[1fr_minmax(24rem,42%)]">
      <div
        class="min-h-0 overflow-y-auto rounded-xl border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900"
      >
        <div v-if="loading" class="p-6 text-sm text-zinc-400">Loading timeline…</div>
        <ul v-else class="divide-y divide-zinc-100 dark:divide-zinc-800/70">
          <li v-for="hit in hits" :key="hit.id">
            <button
              type="button"
              class="flex w-full items-baseline gap-2.5 px-4 py-2 text-left transition hover:bg-zinc-50 dark:hover:bg-zinc-800/40"
              :class="expanded === hit.id ? 'bg-violet-50/60 dark:bg-violet-500/5' : ''"
              @click="toggle(hit.id)"
            >
              <span class="w-14 shrink-0 text-right font-mono text-[11px] text-zinc-400">{{ hit.id }}</span>
              <KindBadge :kind="hit.kind" :proposed="hit.review_state === 'proposed' || hit.review_state === 'lapsed'" />
              <ElevatedBadge v-if="hit.can_instruct" class="shrink-0" />
              <span
                v-if="!project"
                class="shrink-0 rounded-md bg-zinc-100 px-1.5 py-0.5 font-mono text-[10px] text-zinc-500 dark:bg-zinc-800 dark:text-zinc-400"
                >{{ hit.project }}</span
              >
              <span class="shrink-0 text-[11px] text-zinc-400">{{ fmtDate(hit.captured_at_epoch) }}</span>
              <span
                class="min-w-0 text-sm text-zinc-600 dark:text-zinc-300"
                :class="expanded === hit.id ? '' : 'truncate'"
                >{{ hit.snippet }}</span
              >
            </button>
            <!-- Small screens keep the inline read-only expansion; the
                 detail panel (with elevation) takes over on lg+. -->
            <div
              v-if="expanded === hit.id && detailCache.get(hit.id)"
              class="border-t border-dashed border-zinc-200 bg-zinc-50/60 px-4 py-3 pl-[5.25rem] lg:hidden dark:border-zinc-800 dark:bg-zinc-950/40"
            >
              <p class="text-sm leading-relaxed whitespace-pre-wrap text-zinc-700 dark:text-zinc-300">
                {{ detailCache.get(hit.id)!.text }}
              </p>
              <p class="mt-2 font-mono text-[11px] text-zinc-400">
                session {{ detailCache.get(hit.id)!.session_id }} · confidence
                {{ detailCache.get(hit.id)!.confidence }}
              </p>
            </div>
          </li>
        </ul>
        <p v-if="!loading && !hits.length" class="p-6 text-sm text-zinc-400">No timeline hits.</p>
      </div>

      <DetailPanel
        class="hidden lg:flex"
        mode="browse"
        :observation="selectedObs"
        :reviewed-entry="null"
        :rejecting="false"
        :elevation-busy="loading || (!!selectedObs && state.elevationPending[selectedObs.id] === true)"
        @elevate="(reason) => elevation(true, reason)"
        @demote="(reason) => elevation(false, reason)"
      />
    </div>
  </div>
</template>
