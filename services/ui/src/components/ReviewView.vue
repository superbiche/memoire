<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import type { Observation, ReviewedEntry } from '../lib/types'
import { can } from '../lib/verdicts'
import {
  elevateFromQueue,
  evictReviewedEntry,
  lapseFromQueue,
  loadMore,
  loadQueue,
  review,
  rewriteFromQueue,
  setElevation,
  state,
} from '../stores/review'
import FilterBar from './FilterBar.vue'
import ObservationCard from './ObservationCard.vue'
import DetailPanel from './DetailPanel.vue'

// True while the Review tab is the visible one — see App.vue: this
// component stays mounted across tabs, so key handling gates on it.
const props = defineProps<{ active: boolean }>()

const project = ref('')
const kind = ref('')
const includeNoise = ref(false)
// Sort on occurred_at — the date the cards display. Default: recent first.
const sortDir = ref<'desc' | 'asc'>('desc')
const selectedIndex = ref(0)
const rejecting = ref(false)
// Lifted flow states for the panel (harmony pass 2026-07-31): e opens the
// elevation friction box, w the rewrite editor. Mutually exclusive with
// rejecting and with each other.
const elevating = ref(false)
const rewriting = ref(false)
// The observation an open flow was opened FOR (round-1 review MAJOR 1): a
// flow submits against this id or not at all — the selection can drift
// underneath an open editor (filter change, list mutation), and resolving
// the target at submit time rewrote/elevated the WRONG observation. The
// watcher below closes any flow the moment the selection stops matching.
const flowFor = ref<number | null>(null)
// -1 = live queue; >= 0 = walking back through this session's reviews (u key).
const historyCursor = ref(-1)

function closeFlows() {
  rejecting.value = false
  elevating.value = false
  rewriting.value = false
  flowFor.value = null
}

const filtered = computed(() =>
  state.items
    .filter(
      (o) =>
        (includeNoise.value || !o.noise_candidate) &&
        (!project.value || o.project === project.value) &&
        (!kind.value || o.kind === kind.value),
    )
    .sort((a, b) =>
      sortDir.value === 'desc'
        ? b.occurred_at_epoch - a.occurred_at_epoch || b.id - a.id
        : a.occurred_at_epoch - b.occurred_at_epoch || a.id - b.id,
    ),
)

function facetCounts(pick: (o: Observation) => string, skip: 'project' | 'kind') {
  const counts = new Map<string, number>()
  for (const o of state.items) {
    if (!includeNoise.value && o.noise_candidate) continue
    if (skip !== 'project' && project.value && o.project !== project.value) continue
    if (skip !== 'kind' && kind.value && o.kind !== kind.value) continue
    const key = pick(o)
    counts.set(key, (counts.get(key) ?? 0) + 1)
  }
  return [...counts.entries()]
    .map(([name, count]) => ({ name, count }))
    .sort((a, b) => b.count - a.count || a.name.localeCompare(b.name))
}

const projects = computed(() => facetCounts((o) => o.project, 'project'))
const kinds = computed(() => facetCounts((o) => o.kind, 'kind'))
// Unknown until the noise superset has been fetched.
const noiseCount = computed(() =>
  state.noiseLoaded ? state.items.filter((o) => o.noise_candidate).length : null,
)

// Each (direction × noise) combination is a distinct capped server window;
// the store dedupes windows it already holds. Both toggle directions fetch:
// filtering noise out of a capped noisy window is NOT the non-noise window
// (noise can crowd out non-noise rows past the cap).
watch(includeNoise, (on) => {
  void loadQueue(sortDir.value, on)
})
watch(sortDir, (dir) => {
  void loadQueue(dir, includeNoise.value)
  // A new direction is a new reading order: start at its top, don't chase
  // the previously selected card down the resorted list.
  backToQueue()
  selectedIndex.value = 0
  listEl.value?.scrollTo({ top: 0 })
})

const activeWindow = computed(() => state.windows[`${sortDir.value}:${includeNoise.value}`])
const canLoadMore = computed(() => Boolean(activeWindow.value) && !activeWindow.value!.exhausted)

function loadMoreActive() {
  // The store serializer QUEUES concurrent callers rather than dropping
  // them — an unguarded held-down j would drain page after page (review
  // round-2 new finding 1). One in-flight fetch at a time from the UI.
  if (state.loading) return
  void loadMore(sortDir.value, includeNoise.value)
}

const viewingHistory = computed<ReviewedEntry | null>(() =>
  historyCursor.value >= 0
    ? (state.reviewed[state.reviewed.length - 1 - historyCursor.value] ?? null)
    : null,
)

const selected = computed<Observation | null>(
  () => viewingHistory.value?.observation ?? filtered.value[selectedIndex.value] ?? null,
)

watch(filtered, (list) => {
  if (selectedIndex.value >= list.length) selectedIndex.value = Math.max(0, list.length - 1)
})

function openFlow(which: 'rejecting' | 'elevating' | 'rewriting') {
  const obs = filtered.value[selectedIndex.value]
  if (!obs || viewingHistory.value) return
  closeFlows()
  flowFor.value = obs.id
  if (which === 'rejecting') rejecting.value = true
  else if (which === 'elevating') elevating.value = true
  else rewriting.value = true
}

// The flow's target must stay the selected card; any drift closes the flow
// (and with it the panel's stale editor buffer) instead of letting a
// submit land on whoever slid into the index (round-1 review MAJOR 1).
watch(
  () => filtered.value[selectedIndex.value]?.id,
  (id) => {
    if (flowFor.value != null && id !== flowFor.value) closeFlows()
  },
)

// Keep the selected card in view while navigating.
const listEl = ref<HTMLElement | null>(null)
watch(
  () => selected.value?.id,
  async () => {
    const id = selected.value?.id
    if (id == null || viewingHistory.value) return
    requestAnimationFrame(() => {
      listEl.value
        ?.querySelector(`[data-obs-id="${id}"]`)
        ?.scrollIntoView({ block: 'nearest', behavior: 'smooth' })
    })
  },
)

function backToQueue() {
  historyCursor.value = -1
  closeFlows()
}

function move(delta: number) {
  backToQueue()
  if (!filtered.value.length) {
    // The a/c…j rhythm must keep working on an emptied window: j pulls
    // the next page instead of dead-ending against an empty list.
    if (delta > 0 && canLoadMore.value) loadMoreActive()
    return
  }
  // j past the last card pulls the next page of the active window.
  if (selectedIndex.value + delta >= filtered.value.length && canLoadMore.value) {
    loadMoreActive()
  }
  selectedIndex.value = Math.min(
    Math.max(selectedIndex.value + delta, 0),
    filtered.value.length - 1,
  )
}

async function verdict(v: 'accepted' | 'rejected' | 'confirmed', reason?: string) {
  const obs = filtered.value[selectedIndex.value]
  if (!obs || viewingHistory.value) return
  // A reject submitted from the reason input targets the flow's owner; the
  // drift watcher closes the flow on selection change, so a mismatch here
  // means the gesture raced the close — drop it (round-1 review MAJOR 1).
  if (v === 'rejected' && flowFor.value != null && obs.id !== flowFor.value) return
  closeFlows()
  await review(obs, v, reason || undefined)
}

function lapseSelected() {
  const obs = filtered.value[selectedIndex.value]
  if (!obs || viewingHistory.value || !can(obs, 'lapse')) return
  closeFlows()
  void lapseFromQueue(obs)
}

async function onRewrite(text: string, reason: string) {
  const obs = filtered.value[selectedIndex.value]
  // A flow submits against the observation it was opened for or not at all
  // (round-1 review MAJOR 1); the drift watcher makes a mismatch unreachable
  // in practice — this is the belt to its suspenders.
  if (!obs || viewingHistory.value || obs.id !== flowFor.value) return
  if (await rewriteFromQueue(obs, text, reason || undefined)) closeFlows()
}

async function onElevateProposed(reason: string) {
  const obs = filtered.value[selectedIndex.value]
  if (!obs || viewingHistory.value || obs.id !== flowFor.value) return
  closeFlows()
  await elevateFromQueue(obs, reason || undefined)
}

// Elevate/demote apply to the history entry under the cursor (the detail
// panel only offers them on accepted observations).
async function elevation(elevated: boolean, reason?: string) {
  const entry = viewingHistory.value
  if (!entry) return
  const res = await setElevation(entry, elevated, reason || undefined)
  if (res.gone) {
    // Owner-aware eviction + cursor repair (round-6 review): the
    // tail-relative cursor is repaired around the splice BY IDENTITY —
    // exit history when the evicted entry is the one on screen (also
    // clearing the latent cursor that would silently reactivate on the
    // next reviewed.push); if the operator moved during the await, keep
    // whatever they are looking at selected.
    const viewedNow = viewingHistory.value
    evictReviewedEntry(entry)
    if (viewedNow === entry || viewedNow == null) {
      backToQueue()
    } else {
      const idx = state.reviewed.indexOf(viewedNow)
      if (idx >= 0) historyCursor.value = state.reviewed.length - 1 - idx
      else backToQueue()
    }
  }
}

function selectById(id: number) {
  backToQueue()
  const i = filtered.value.findIndex((o) => o.id === id)
  if (i >= 0) selectedIndex.value = i
}

function onKeydown(e: KeyboardEvent) {
  // This component is v-show-mounted on every tab (queue state survives tab
  // hops), so the listener must gate on visibility: without it, verdict keys
  // pressed on other tabs mutated the hidden queue's selected card.
  if (!props.active) return
  const target = e.target as HTMLElement
  if (
    e.metaKey ||
    e.ctrlKey ||
    e.altKey ||
    e.shiftKey ||
    target.tagName === 'INPUT' ||
    target.tagName === 'TEXTAREA' ||
    target.isContentEditable
  )
    return
  // One physical gesture per mutation (round-1 review MAJOR 3): browser
  // auto-repeat on a held verdict key must not drain card after card —
  // review() removes the card synchronously, so each repeat would resolve
  // the NEXT one. Navigation keys keep repeating.
  if (e.repeat && ['a', 'c', 'r', 'l', 'w', 'e'].includes(e.key)) return
  switch (e.key) {
    case 'j':
    case 'ArrowDown':
      e.preventDefault()
      move(1)
      break
    case 'k':
    case 'ArrowUp':
      e.preventDefault()
      move(-1)
      break
    case 'a':
      e.preventDefault()
      void verdict('accepted')
      break
    case 'c':
      // Confirm = accept + I vouch (provenance -> user_confirmed). The
      // queue's stronger verdict, memoire:D192 — the R-5 split's vouching
      // act finally gets a key on the daily surface.
      e.preventDefault()
      void verdict('confirmed')
      break
    case 'r':
      if (selected.value && !viewingHistory.value) {
        e.preventDefault()
        openFlow('rejecting')
      }
      break
    case 'l':
      e.preventDefault()
      lapseSelected()
      break
    case 'w':
      // Rewrite is w, not r — it is not a verdict (opens the editor).
      if (selected.value && !viewingHistory.value && can(selected.value, 'rewrite')) {
        e.preventDefault()
        openFlow('rewriting')
      }
      break
    case 'e':
      if (selected.value && !viewingHistory.value && can(selected.value, 'elevate')) {
        e.preventDefault()
        openFlow('elevating')
      }
      break
    case 'u':
      e.preventDefault()
      if (historyCursor.value < state.reviewed.length - 1) historyCursor.value++
      break
    case 'Escape':
      backToQueue()
      break
  }
}

onMounted(() => window.addEventListener('keydown', onKeydown))
onBeforeUnmount(() => window.removeEventListener('keydown', onKeydown))
</script>

<template>
  <div class="mx-auto flex h-full max-w-7xl flex-col gap-3 px-4 py-4 sm:px-6">
    <FilterBar
      v-model:project="project"
      v-model:kind="kind"
      v-model:include-noise="includeNoise"
      v-model:sort-dir="sortDir"
      :projects="projects"
      :kinds="kinds"
      :noise-count="noiseCount"
    />

    <div class="grid min-h-0 flex-1 grid-cols-1 gap-4 lg:grid-cols-[1fr_minmax(24rem,42%)]">
      <!-- Queue -->
      <div ref="listEl" class="relative min-h-0 overflow-y-auto pb-4">
        <div v-if="state.loading && !state.loaded" class="space-y-2">
          <div
            v-for="i in 5"
            :key="i"
            class="h-24 animate-pulse rounded-xl border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900"
          />
        </div>

        <div
          v-else-if="state.error"
          class="rounded-xl border border-rose-200 bg-rose-50 p-4 text-sm text-rose-700 dark:border-rose-500/30 dark:bg-rose-500/10 dark:text-rose-300"
        >
          Failed to load queue: {{ state.error }}
          <button class="ml-2 font-medium underline" @click="loadQueue(sortDir, includeNoise)">
            retry
          </button>
        </div>

        <!-- The pager must survive an emptied list: reviewing the last
             visible card used to swallow the Load-older button with it
             (operator report 2026-07-30) while thousands sat behind the
             server window. Empty state and list are siblings; the pager
             renders after either. -->
        <template v-else>
          <div
            v-if="!filtered.length"
            class="flex min-h-48 flex-col items-center justify-center gap-2 text-center"
          >
            <!-- "Queue clear" is a claim about the ACTIVE window: while it
                 is absent or still fetching (direction/noise change past the
                 initial load), say loading — exhaustion is not yet proven
                 (review round-2 new finding 2). -->
            <span class="text-3xl">{{ state.loading || !activeWindow ? '…' : '✓' }}</span>
            <p class="text-sm font-medium text-zinc-600 dark:text-zinc-300">
              {{
                state.loading || !activeWindow
                  ? 'Loading…'
                  : state.items.length
                    ? 'No matches for these filters'
                    : canLoadMore
                      ? `Window clear — ${sortDir === 'desc' ? 'older' : 'newer'} proposed remain`
                      : 'Queue clear'
              }}
            </p>
            <p v-if="state.reviewed.length" class="text-xs text-zinc-400">
              {{ state.reviewed.length }} reviewed this session — press <kbd>u</kbd> to look back
            </p>
          </div>

          <TransitionGroup v-else name="queue" tag="ul" class="relative space-y-2">
            <li
              v-for="(obs, i) in filtered"
              :key="obs.id"
              :data-obs-id="obs.id"
              :data-verdict="state.leaving[obs.id]"
              class="relative"
            >
              <ObservationCard
                :observation="obs"
                :selected="!viewingHistory && i === selectedIndex"
                @click="selectById(obs.id)"
              />
            </li>
          </TransitionGroup>
          <div class="py-3 text-center">
            <button
              v-if="canLoadMore"
              :disabled="state.loading"
              class="rounded-full border border-zinc-200 px-4 py-1.5 text-xs font-medium text-zinc-500 transition hover:border-violet-400/60 hover:text-violet-600 disabled:opacity-50 dark:border-zinc-800 dark:hover:border-violet-500/50 dark:hover:text-violet-300"
              @click="loadMoreActive"
            >
              {{ state.loading ? 'Loading…' : `Load ${sortDir === 'desc' ? 'older' : 'newer'}` }}
            </button>
            <p v-else-if="filtered.length" class="text-[11px] text-zinc-400 dark:text-zinc-500">
              {{ filtered.length }} in view — end of the queue
            </p>
          </div>
        </template>
      </div>

      <!-- Detail -->
      <DetailPanel
        class="hidden lg:flex"
        :observation="selected"
        :reviewed-entry="viewingHistory"
        :rejecting="rejecting"
        :elevating="elevating"
        :rewriting="rewriting"
        :elevation-busy="!!selected && state.elevationPending[selected.id] === true"
        @accept="verdict('accepted')"
        @confirm="verdict('confirmed')"
        @start-reject="openFlow('rejecting')"
        @reject="(reason) => verdict('rejected', reason)"
        @cancel-reject="closeFlows()"
        @lapse="lapseSelected"
        @start-rewrite="openFlow('rewriting')"
        @rewrite="onRewrite"
        @cancel-rewrite="closeFlows()"
        @start-elevate="openFlow('elevating')"
        @elevate-proposed="onElevateProposed"
        @cancel-elevate="closeFlows()"
        @elevate="(reason) => elevation(true, reason)"
        @demote="(reason) => elevation(false, reason)"
      />
    </div>
  </div>
</template>
