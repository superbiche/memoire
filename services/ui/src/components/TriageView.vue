<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
// Triage lane v1 (R-C): the fast override surface.
//
// Rows arrive pre-sorted by the judge's suggested verdict; the operator
// glances at each and stakes a verdict with one key; ONE bulk apply commits
// the staked rows as sequential per-row preconditioned POSTs. Nothing
// reaches the server before that apply.
//
// Left = accept, right = LAPSE. The asymmetry is mistake-tolerant on
// purpose: lapse is reversible on encounter, reject is not. Reject is
// staked one row at a time, from its own explicit control, and never rides
// a swipe.
import { onBeforeUnmount, onMounted, nextTick, ref, watch } from 'vue'
import { fmtDate, fmtDateTime, pct } from '../lib/format'
import type { TriageRow, TriageStake } from '../lib/types'
import KindBadge from './KindBadge.vue'
import {
  applyReject,
  applyStakes,
  clearStakes,
  denoiseGenre,
  denoiseMode,
  isDoubt,
  loadTriage,
  moveCursor,
  setNoiseGenre,
  sortedRows,
  stake,
  toggleKeep,
  rejectStakeIDs,
  stakedBreakdown,
  stakedCount,
  totalStaked,
  suggestedConfidence,
  suggestedGenre,
  suggestedReason,
  suggestedVerdict,
  triageState as t,
} from '../stores/triage'

const listEl = ref<HTMLElement | null>(null)
const noiseOpen = ref<number | null>(null)
const noiseText = ref('')

function currentRow(): TriageRow | undefined {
  if (t.cursorId == null) return undefined
  return sortedRows.value.find((r) => r.observation.id === t.cursorId)
}

function openNoise(id: number) {
  noiseOpen.value = id
  noiseText.value = t.noiseGenres[id] ?? suggestedGenre(sortedRows.value.find((r) => r.observation.id === id)!) ?? ''
  void nextTick(() => {
    ;(listEl.value?.querySelector(`[data-noise-input="${id}"]`) as HTMLInputElement | null)?.focus()
  })
}

function commitNoise(id: number) {
  setNoiseGenre(id, noiseText.value)
  noiseOpen.value = null
  moveCursor(1)
}

function stakeTone(v: TriageStake | undefined): string {
  switch (v) {
    case 'accept':
      return 'border-emerald-400 bg-emerald-50/60 dark:border-emerald-600 dark:bg-emerald-500/10'
    case 'lapse':
      return 'border-zinc-400 bg-zinc-50 dark:border-zinc-600 dark:bg-zinc-800/60'
    case 'noise':
      return 'border-amber-400 bg-amber-50/60 dark:border-amber-600 dark:bg-amber-500/10'
    case 'reject':
      return 'border-rose-400 bg-rose-50/60 dark:border-rose-600 dark:bg-rose-500/10'
    case 'keep':
      return 'border-sky-400 bg-sky-50/60 dark:border-sky-500 dark:bg-sky-500/10'
    default:
      return 'border-zinc-200 dark:border-zinc-800'
  }
}

// In the de-noise pool the row's tone carries the VERDICT, not the stake: a
// condemned row is visibly dimmed and a kept one visibly is not. Reading the
// batch has to be possible at a glance, or the operator is re-reading text
// to find out what the page already decided.
function denoiseTone(id: number): string {
  return t.stakes[id] === 'keep'
    ? 'border-sky-400 bg-sky-50/70 opacity-100 dark:border-sky-500 dark:bg-sky-500/10'
    : 'border-zinc-200 bg-zinc-50/40 opacity-55 grayscale dark:border-zinc-800 dark:bg-zinc-900/40'
}

function rowTone(id: number): string {
  return denoiseMode.value ? denoiseTone(id) : stakeTone(t.stakes[id])
}

function suggestionTone(v: string): string {
  switch (v) {
    case 'accept':
      return 'text-emerald-600 dark:text-emerald-400'
    case 'reject':
      return 'text-rose-600 dark:text-rose-400'
    case 'noise-lapse':
      return 'text-amber-600 dark:text-amber-400'
    case 'lapse':
      return 'text-zinc-500 dark:text-zinc-400'
    default:
      return 'text-zinc-400'
  }
}

function onKeydown(e: KeyboardEvent) {
  const target = e.target as HTMLElement
  if (
    e.metaKey ||
    e.ctrlKey ||
    e.altKey ||
    target.tagName === 'INPUT' ||
    target.tagName === 'TEXTAREA' ||
    target.isContentEditable
  )
    return
  // One physical gesture per stake: auto-repeat on a held key must not walk
  // the queue staking row after row unseen — which is the exact "N rows
  // without per-row attention" failure this surface is built to avoid.
  if (e.repeat && ['ArrowLeft', 'ArrowRight', 'n', 'x', 'a', ' '].includes(e.key)) return
  const row = currentRow()
  // The DE-NOISE pool has its own, smaller keyboard. Everything on screen is
  // already condemned, so the surface offers exactly two ways out and no
  // fast axis at all: `space` keeps, `a` accepts. Left/right are inert here
  // on purpose — left = accept would put the escape hatch back on a swipe,
  // and an accept reached by rhythm is exactly what making it a deliberate
  // key was for. Reject is absent from this pool entirely.
  if (denoiseMode.value) {
    switch (e.key) {
      case 'j':
      case 'ArrowDown':
        e.preventDefault()
        moveCursor(1)
        break
      case 'k':
      case 'ArrowUp':
        e.preventDefault()
        moveCursor(-1)
        break
      case ' ':
        if (row) {
          e.preventDefault()
          toggleKeep(row.observation.id)
          moveCursor(1)
        }
        break
      case 'a':
        if (row) {
          e.preventDefault()
          stake(row.observation.id, 'accept')
          moveCursor(1)
        }
        break
    }
    return
  }
  switch (e.key) {
    case 'j':
    case 'ArrowDown':
      e.preventDefault()
      moveCursor(1)
      break
    case 'k':
    case 'ArrowUp':
      e.preventDefault()
      moveCursor(-1)
      break
    case 'ArrowLeft':
      if (row) {
        e.preventDefault()
        stake(row.observation.id, 'accept')
        moveCursor(1)
      }
      break
    case 'ArrowRight':
      if (row) {
        e.preventDefault()
        stake(row.observation.id, 'lapse')
        moveCursor(1)
      }
      break
    case 'n':
      if (row) {
        e.preventDefault()
        openNoise(row.observation.id)
      }
      break
    case 'x':
      // Reject is deliberately NOT on the left/right axis and does not
      // advance the cursor: a rejected row 404s off the read surface, so it
      // gets a look, not a rhythm.
      if (row) {
        e.preventDefault()
        stake(row.observation.id, 'reject')
      }
      break
    case ' ':
      if (row) {
        e.preventDefault()
        // Space clears whatever is staked on this row.
        const cur = t.stakes[row.observation.id]
        if (cur) stake(row.observation.id, cur)
      }
      break
    case 'Escape':
      if (noiseOpen.value != null) noiseOpen.value = null
      break
  }
}

watch(
  () => t.cursorId,
  (id) => {
    if (id == null) return
    void nextTick(() => {
      listEl.value?.querySelector(`[data-triage-id="${id}"]`)?.scrollIntoView({ block: 'nearest', behavior: 'smooth' })
    })
  },
)

watch(
  () => t.pool,
  () => void loadTriage(),
)

onMounted(() => {
  if (!t.loaded) void loadTriage()
  window.addEventListener('keydown', onKeydown)
})
onBeforeUnmount(() => window.removeEventListener('keydown', onKeydown))
</script>

<template>
  <div class="mx-auto flex h-full max-w-5xl flex-col gap-3 px-4 py-4 sm:px-6">
    <!-- Scope header. These counters are the triage surface's OWN: lapsed
         rows are excluded from the queue counters by law, so reusing those
         would under-report exactly the TTL pool this lane works through. -->
    <div class="flex flex-wrap items-center gap-3 text-xs">
      <div class="flex items-center gap-1 rounded-lg bg-zinc-100 p-1 dark:bg-zinc-900">
        <button
          v-for="p in ([['', 'all'], ['proposed', 'proposed'], ['lapsed', 'TTL-lapsed'], ['de-noise', 'de-noise']] as const)"
          :key="p[0]"
          class="rounded-md px-2.5 py-1 font-medium transition"
          :class="
            t.pool === p[0]
              ? 'bg-white text-zinc-900 shadow-sm dark:bg-zinc-700 dark:text-zinc-50'
              : 'text-zinc-500 hover:text-zinc-700 dark:text-zinc-400'
          "
          @click="t.pool = p[0]"
        >
          {{ p[1] }}
        </button>
      </div>
      <!-- The counts SPLIT: proposed and TTL-lapsed are main-pool numbers
           and exclude identified noise, which is now its own pool and its
           own gesture. A backlog figure that quietly counted both would
           misstate each of them. -->
      <span v-if="t.counts" class="text-zinc-500 dark:text-zinc-400">
        {{ t.counts.proposed }} proposed · {{ t.counts.ttl_lapsed }} TTL-lapsed ·
        <span class="font-medium text-amber-600 dark:text-amber-400">{{ t.counts.de_noise }} de-noise</span> ·
        {{ t.counts.suggested }} judged
      </span>
      <span
        v-if="t.pool === 'lapsed'"
        class="rounded-full bg-zinc-200 px-2 py-0.5 text-[11px] text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300"
        title="the TTL-lapsed pool is a filter over the reviewable scope, never a state of its own — rows a human lapsed deliberately are not here"
      >
        filter, not a state
      </span>
      <span
        v-if="denoiseMode"
        class="rounded-full bg-amber-100 px-2 py-0.5 text-[11px] font-medium text-amber-800 dark:bg-amber-500/15 dark:text-amber-300"
        title="every row on this page is already staked as noise. Your work here is rescuing the ones that are not — the apply commits the loaded rows only, never the pool behind the page."
      >
        inverted — everything here is condemned
      </span>

      <div class="ml-auto flex items-center gap-2">
        <span v-if="totalStaked && !denoiseMode" class="font-mono text-[11px] text-zinc-500">
          {{ stakedBreakdown.accept }}a / {{ stakedBreakdown.lapse }}l /
          {{ stakedBreakdown.noise }}n / {{ stakedBreakdown.reject }}x
        </span>
        <button
          v-if="totalStaked"
          class="rounded-md px-2 py-1 text-xs text-zinc-500 hover:text-zinc-700 dark:hover:text-zinc-300"
          :disabled="t.applying"
          :title="denoiseMode ? 'back to the default: everything on this page condemned again' : 'drop every staked verdict'"
          @click="clearStakes()"
        >
          {{ denoiseMode ? 'reset' : 'clear' }}
        </button>
<button
          class="rounded-md bg-violet-600 px-3 py-1.5 text-xs font-medium text-white transition disabled:opacity-40"
          :disabled="!stakedCount || t.applying"
          title="commits each staked row as its own preconditioned request — no batch verdict route exists, and none is used. Only the rows loaded on this page are committed; the pool behind the limit is untouched."
          @click="applyStakes()"
        >
          <template v-if="t.applying">applying…</template>
          <template v-else-if="denoiseMode">
            mark {{ stakedBreakdown.noise }} as noise · keep {{ stakedBreakdown.keep }}<template
              v-if="stakedBreakdown.accept"
            >
              · accept {{ stakedBreakdown.accept }}</template
            >
          </template>
          <template v-else>apply {{ stakedCount }}</template>
        </button>
        <span
          v-if="rejectStakeIDs.length"
          class="rounded-full bg-rose-100 px-2 py-0.5 text-[11px] font-medium text-rose-700 dark:bg-rose-500/15 dark:text-rose-300"
          title="rejects are applied one at a time, from their own row — the bulk apply never carries them"
          >{{ rejectStakeIDs.length }} reject{{ rejectStakeIDs.length === 1 ? '' : 's' }} pending, per row</span
        >
      </div>
    </div>

    <p v-if="denoiseMode" class="text-[11px] text-zinc-400">
      <kbd>j</kbd><kbd>k</kbd> move · <kbd>space</kbd> keep this row · <kbd>a</kbd> accept it outright.
      Everything on this page is staked as noise until you say otherwise; a keep returns the row to the
      ordinary queue with its accept-or-lapse question still open. Reject is not available here.
    </p>
    <p v-else class="text-[11px] text-zinc-400">
      <kbd>j</kbd><kbd>k</kbd> move · <kbd>←</kbd> accept · <kbd>→</kbd> lapse ·
      <kbd>n</kbd> name a noise genre · <kbd>x</kbd> reject · <kbd>space</kbd> unstake.
      Reject is off the fast axis on purpose: a rejected row leaves the read surface, and that is not undone by an encounter.
    </p>

    <div
      v-if="t.failures.length"
      class="rounded-lg border border-rose-300 bg-rose-50/70 p-3 text-xs dark:border-rose-800 dark:bg-rose-950/30"
    >
      <p class="font-medium text-rose-700 dark:text-rose-300">these did not land:</p>
      <ul class="mt-1 space-y-0.5 text-rose-600 dark:text-rose-400">
        <li v-for="f in t.failures" :key="f.id">#{{ f.id }} — {{ f.reason }}</li>
      </ul>
    </div>

    <div ref="listEl" class="min-h-0 flex-1 space-y-2 overflow-y-auto pr-1">
      <p v-if="t.loading" class="py-8 text-center text-sm text-zinc-400">loading…</p>
      <p v-else-if="!sortedRows.length" class="py-8 text-center text-sm text-zinc-400">
        nothing reviewable in this scope
      </p>
      <article
        v-for="row in sortedRows"
        :key="row.observation.id"
        :data-triage-id="row.observation.id"
        class="cursor-pointer rounded-lg border p-3 transition"
        :class="[
          rowTone(row.observation.id),
          t.cursorId === row.observation.id
            ? 'border-l-4 border-l-violet-500 ring-2 ring-violet-500/70 shadow-md shadow-violet-500/10 dark:border-l-violet-400 dark:ring-violet-400/60'
            : 'hover:ring-2 hover:ring-violet-300/50 dark:hover:ring-violet-500/30',
        ]"
        @click="denoiseMode ? (t.cursorId = row.observation.id, toggleKeep(row.observation.id)) : (t.cursorId = row.observation.id)"
      >
        <div class="flex flex-wrap items-center gap-2">
          <KindBadge :kind="row.observation.kind" proposed />
          <span class="text-[11px] text-zinc-400">{{ row.observation.review_state }}</span>
          <!-- The DOUBT marker. These rows are pinned at the top of the pool
               because the two noise signals disagree about them, or because
               the judge hedged its own call — either way they are the rows a
               bulk condemnation is most likely to get wrong. -->
          <span
            v-if="denoiseMode && isDoubt(row)"
            class="rounded-full bg-violet-100 px-2 py-0.5 text-[10px] font-medium tracking-wide text-violet-800 uppercase dark:bg-violet-500/20 dark:text-violet-300"
            :title="
              suggestedVerdict(row) === 'noise-lapse'
                ? (row.observation.noise_candidate ? 'the judge is not sure of this noise call' : 'the judge says noise, derivation did not flag it')
                : 'derivation flagged this as noise, the judge did not'
            "
            >doubt</span
          >
          <!-- The genre a de-noise row is condemned UNDER: the batch is
               readable only if each row says what it is being killed for. -->
          <span
            v-if="denoiseMode"
            class="rounded bg-amber-100 px-1.5 py-0.5 font-mono text-[10px] font-medium text-amber-800 dark:bg-amber-500/15 dark:text-amber-300"
            >{{ denoiseGenre(row) }}</span
          >
          <span
            v-if="denoiseMode && suggestedConfidence(row) !== null"
            class="font-mono text-[10px] text-zinc-400"
            title="the judge's confidence in its own verdict"
            >conf {{ pct(suggestedConfidence(row) ?? 0) }}</span
          >
          <span
            v-if="row.observation.noise_candidate && !denoiseMode"
            class="rounded bg-zinc-100 px-1.5 text-[10px] font-medium tracking-wide text-zinc-400 uppercase dark:bg-zinc-800"
            >noise</span
          >
          <span v-if="suggestedVerdict(row)" class="text-[11px]" :class="suggestionTone(suggestedVerdict(row))">
            judge: {{ suggestedVerdict(row) }}<template v-if="suggestedGenre(row)"> ({{ suggestedGenre(row) }})</template>
          </span>
          <span v-else class="text-[11px] text-zinc-400 italic">unjudged</span>
          <!-- In de-noise mode the default stake is the whole page, so
               saying "staked: noise" on every row says nothing. Only an
               OVERRIDE is news. -->
          <span
            v-if="t.stakes[row.observation.id] && (!denoiseMode || t.stakes[row.observation.id] !== 'noise')"
            class="rounded bg-violet-100 px-1.5 text-[10px] font-medium text-violet-700 dark:bg-violet-500/15 dark:text-violet-300"
          >
            staked: {{ t.stakes[row.observation.id] }}
          </span>
          <span class="ml-auto text-[11px] text-zinc-400" :title="fmtDateTime(row.observation.occurred_at_epoch)">
            {{ row.observation.project }} · {{ fmtDate(row.observation.occurred_at_epoch) }} · #{{ row.observation.id }} · {{ pct(row.observation.confidence) }}
          </span>
        </div>

        <p class="mt-2 text-sm leading-relaxed">{{ row.observation.text }}</p>
        <p v-if="suggestedReason(row)" class="mt-1 text-[11px] text-zinc-400 italic">{{ suggestedReason(row) }}</p>

        <!-- The de-noise controls: two ways out of a condemnation, and no
             fast axis. Reject is absent by construction — an irreversible
             verdict has no place in a gesture whose premise is that
             everything on screen is already condemned. -->
        <div v-if="denoiseMode" class="mt-2 flex flex-wrap items-center gap-1.5">
          <button
            class="rounded-md px-2.5 py-1 text-xs font-medium ring-1 ring-inset transition"
            :class="t.stakes[row.observation.id] === 'keep'
              ? 'bg-sky-600 text-white ring-sky-600'
              : 'text-sky-700 ring-sky-300 hover:bg-sky-50 dark:text-sky-400 dark:ring-sky-800 dark:hover:bg-sky-500/10'"
            title="this is not noise — the row leaves the de-noise pool for good and returns to the ordinary queue, still unruled"
            @click.stop="t.cursorId = row.observation.id; toggleKeep(row.observation.id)"
          >
            {{ t.stakes[row.observation.id] === 'keep' ? 'keeping' : 'keep' }}
          </button>
          <button
            class="rounded-md px-2.5 py-1 text-xs font-medium ring-1 ring-inset transition"
            :class="t.stakes[row.observation.id] === 'accept'
              ? 'bg-emerald-600 text-white ring-emerald-600'
              : 'text-emerald-700 ring-emerald-300 hover:bg-emerald-50 dark:text-emerald-400 dark:ring-emerald-800 dark:hover:bg-emerald-500/10'"
            title="the escape hatch: rule this row accepted without leaving the de-noise pass"
            @click.stop="t.cursorId = row.observation.id; stake(row.observation.id, 'accept')"
          >
            accept
          </button>
        </div>

        <div v-else-if="noiseOpen === row.observation.id" class="mt-2 space-y-1.5">
          <div class="flex items-center gap-2">
            <input
              v-model="noiseText"
              :data-noise-input="row.observation.id"
              list="triage-genre-slugs"
              placeholder="genre slug (e.g. status-narration)"
              class="flex-1 rounded-md border border-zinc-300 bg-white px-2 py-1 font-mono text-xs dark:border-zinc-700 dark:bg-zinc-950"
              @keydown.enter.prevent="commitNoise(row.observation.id)"
              @keydown.esc.prevent="noiseOpen = null"
            />
            <button class="rounded-md bg-amber-500 px-2.5 py-1 text-xs font-medium text-white" @click="commitNoise(row.observation.id)">
              flag noise
            </button>
            <button class="rounded-md px-2 py-1 text-xs text-zinc-500" @click="noiseOpen = null">cancel</button>
          </div>
          <div v-if="t.genres.length" class="flex flex-wrap items-center gap-1">
            <span class="text-[10px] text-zinc-400 uppercase tracking-wide">known:</span>
            <button
              v-for="slug in t.genres.slice(0, 6)"
              :key="slug"
              class="rounded bg-zinc-100 px-1.5 py-0.5 font-mono text-[10px] text-zinc-600 hover:bg-amber-100 hover:text-amber-800 dark:bg-zinc-800 dark:text-zinc-300 dark:hover:bg-amber-500/20"
              @click="noiseText = slug; commitNoise(row.observation.id)"
            >{{ slug }}</button>
          </div>
        </div>

        <div v-else class="mt-2 flex flex-wrap items-center gap-1.5">
          <button
            class="rounded-md px-2.5 py-1 text-xs font-medium ring-1 ring-inset transition"
            :class="t.stakes[row.observation.id] === 'accept'
              ? 'bg-emerald-600 text-white ring-emerald-600'
              : 'text-emerald-700 ring-emerald-300 hover:bg-emerald-50 dark:text-emerald-400 dark:ring-emerald-800 dark:hover:bg-emerald-500/10'"
            @click.stop="stake(row.observation.id, 'accept')"
          >
            accept
          </button>
          <button
            class="rounded-md px-2.5 py-1 text-xs font-medium ring-1 ring-inset transition"
            :class="t.stakes[row.observation.id] === 'lapse'
              ? 'bg-zinc-600 text-white ring-zinc-600'
              : 'text-zinc-600 ring-zinc-300 hover:bg-zinc-50 dark:text-zinc-300 dark:ring-zinc-700 dark:hover:bg-zinc-800'"
            @click.stop="stake(row.observation.id, 'lapse')"
          >
            lapse
          </button>
          <button
            class="rounded-md px-2.5 py-1 text-xs font-medium ring-1 ring-inset transition"
            :class="t.stakes[row.observation.id] === 'noise'
              ? 'bg-amber-500 text-white ring-amber-500'
              : 'text-amber-700 ring-amber-300 hover:bg-amber-50 dark:text-amber-400 dark:ring-amber-800 dark:hover:bg-amber-500/10'"
            :title="t.noiseGenres[row.observation.id] ? `genre: ${t.noiseGenres[row.observation.id]}` : 'name the genre the judge missed'"
            @click.stop="openNoise(row.observation.id)"
          >
            noise{{ t.noiseGenres[row.observation.id] ? `: ${t.noiseGenres[row.observation.id]}` : '…' }}
          </button>
<button
            class="ml-auto rounded-md px-2.5 py-1 text-xs font-medium ring-1 ring-inset transition"
            :class="t.stakes[row.observation.id] === 'reject'
              ? 'bg-rose-600 text-white ring-rose-600'
              : 'text-rose-700 ring-rose-300 hover:bg-rose-50 dark:text-rose-400 dark:ring-rose-800 dark:hover:bg-rose-500/10'"
            title="reject means the row is FALSE — it leaves the read surface and no encounter brings it back"
            @click.stop="stake(row.observation.id, 'reject')"
          >
            reject
          </button>
          <!-- Applying a reject is its own act, one row at a time: the
               irreversible verdict never shares a button with the
               reversible ones. -->
          <button
            v-if="t.stakes[row.observation.id] === 'reject'"
            class="rounded-md bg-rose-600 px-2.5 py-1 text-xs font-medium text-white transition disabled:opacity-40"
            :disabled="t.applying"
            title="applies this reject alone — it is not carried by the bulk apply"
            @click.stop="applyReject(row.observation.id)"
          >
            apply reject #{{ row.observation.id }}
          </button>
        </div>
      </article>
    </div>

    <datalist id="triage-genre-slugs">
      <option v-for="slug in t.genres" :key="slug" :value="slug" />
    </datalist>
  </div>
</template>
