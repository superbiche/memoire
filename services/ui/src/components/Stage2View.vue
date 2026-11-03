<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
// Triage lane v1, STAGE 2 (R-C): the confirm/elevation glance over the
// HUMAN-validated set. Rules-born accepted rows never appear — nobody
// validated them, so they are not material for a vouch or a standing order.
//
// Confirm and elevation stay structurally human: the judge suggests, the
// operator stakes per row, and elevation on an unvouched row chains the
// confirm first (memoire:D192's double gesture, two calls, two log rows).
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { fmtDate, fmtDateTime } from '../lib/format'
import type { TriageRow, TriageStage2Stake } from '../lib/types'
import KindBadge from './KindBadge.vue'
import {
  applyStage2,
  clearStage2Stakes,
  loadStage2,
  stage2MoveCursor,
  stage2Rows,
  stage2Stake,
  stage2StakedCount,
  stage2State as t,
  stage2Suggestion,
} from '../stores/triage2'

const listEl = ref<HTMLElement | null>(null)

function currentRow(): TriageRow | undefined {
  if (t.cursorId == null) return undefined
  return stage2Rows.value.find((r) => r.observation.id === t.cursorId)
}

function stakeTone(v: TriageStage2Stake | undefined): string {
  switch (v) {
    case 'confirm':
      return 'border-emerald-400 bg-emerald-50/60 dark:border-emerald-600 dark:bg-emerald-500/10'
    case 'elevate':
      return 'border-violet-400 bg-violet-50/60 dark:border-violet-600 dark:bg-violet-500/10'
    case 'decline':
      return 'border-zinc-400 bg-zinc-50 dark:border-zinc-600 dark:bg-zinc-800/60'
    default:
      return 'border-zinc-200 dark:border-zinc-800'
  }
}

function onKeydown(e: KeyboardEvent) {
  const target = e.target as HTMLElement
  if (e.metaKey || e.ctrlKey || e.altKey || target.tagName === 'INPUT' || target.isContentEditable) return
  if (e.repeat && ['c', 'e', 'd'].includes(e.key)) return
  const row = currentRow()
  switch (e.key) {
    case 'j':
    case 'ArrowDown':
      e.preventDefault()
      stage2MoveCursor(1)
      break
    case 'k':
    case 'ArrowUp':
      e.preventDefault()
      stage2MoveCursor(-1)
      break
    case 'c':
      if (row) {
        e.preventDefault()
        stage2Stake(row.observation.id, 'confirm')
      }
      break
    case 'e':
      if (row) {
        e.preventDefault()
        stage2Stake(row.observation.id, 'elevate')
      }
      break
    case 'd':
      if (row) {
        e.preventDefault()
        stage2Stake(row.observation.id, 'decline')
      }
      break
  }
}

watch(
  () => t.cursorId,
  (id) => {
    if (id == null) return
    void nextTick(() => {
      listEl.value?.querySelector(`[data-s2-id="${id}"]`)?.scrollIntoView({ block: 'nearest', behavior: 'smooth' })
    })
  },
)

onMounted(() => {
  if (!t.loaded) void loadStage2()
  window.addEventListener('keydown', onKeydown)
})
onBeforeUnmount(() => window.removeEventListener('keydown', onKeydown))
</script>

<template>
  <div class="mx-auto flex h-full max-w-5xl flex-col gap-3 px-4 py-4 sm:px-6">
    <div class="flex flex-wrap items-center gap-3 text-xs">
      <span class="text-zinc-500 dark:text-zinc-400">
        {{ stage2Rows.length }} human-validated rows — rules-born accepts are not here, nobody validated them
      </span>
      <div class="ml-auto flex items-center gap-2">
        <button
          v-if="stage2StakedCount"
          class="rounded-md px-2 py-1 text-xs text-zinc-500 hover:text-zinc-700 dark:hover:text-zinc-300"
          :disabled="t.applying"
          @click="clearStage2Stakes()"
        >
          clear
        </button>
        <button
          class="rounded-md bg-violet-600 px-3 py-1.5 text-xs font-medium text-white transition disabled:opacity-40"
          :disabled="!stage2StakedCount || t.applying"
          title="each staked row is its own preconditioned request; an elevation on an unvouched row chains its confirm first"
          @click="applyStage2()"
        >
          {{ t.applying ? 'applying…' : `apply ${stage2StakedCount}` }}
        </button>
      </div>
    </div>

    <p class="text-[11px] text-zinc-400">
      <kbd>j</kbd><kbd>k</kbd> move · <kbd>c</kbd> confirm · <kbd>e</kbd> elevate · <kbd>d</kbd> decline.
      Elevation is a standing order — "constrain every session in this project" — not a badge of importance; an important fact is confirmed, not elevated.
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
      <p v-else-if="!stage2Rows.length" class="py-8 text-center text-sm text-zinc-400">
        nothing validated waiting on a stage-2 ruling
      </p>
      <article
        v-for="row in stage2Rows"
        :key="row.observation.id"
        :data-s2-id="row.observation.id"
        class="cursor-pointer rounded-lg border p-3 transition"
        :class="[
          stakeTone(t.stakes[row.observation.id]),
          t.cursorId === row.observation.id
            ? 'border-l-4 border-l-violet-500 ring-2 ring-violet-500/70 shadow-md shadow-violet-500/10 dark:border-l-violet-400 dark:ring-violet-400/60'
            : 'hover:ring-2 hover:ring-violet-300/50 dark:hover:ring-violet-500/30',
        ]"
        @click="t.cursorId = row.observation.id"
      >
        <div class="flex flex-wrap items-center gap-2">
          <KindBadge :kind="row.observation.kind" />
          <span class="text-[11px] text-zinc-400">{{ row.observation.provenance }}</span>
          <span
            v-if="row.observation.can_instruct === 1"
            class="rounded bg-violet-100 px-1.5 text-[10px] font-medium text-violet-700 dark:bg-violet-500/15 dark:text-violet-300"
            >elevated</span
          >
          <span v-if="stage2Suggestion(row)" class="text-[11px] text-violet-600 dark:text-violet-400">
            judge: {{ stage2Suggestion(row)!.suggested_verdict }}
            <span class="text-zinc-400">(arm {{ stage2Suggestion(row)!.arm || '?' }})</span>
          </span>
          <span v-else class="text-[11px] text-zinc-400 italic">unjudged</span>
          <span class="ml-auto text-[11px] text-zinc-400" :title="fmtDateTime(row.observation.occurred_at_epoch)">
            {{ row.observation.project }} · {{ fmtDate(row.observation.occurred_at_epoch) }} · #{{ row.observation.id }}
          </span>
        </div>

        <p class="mt-2 text-sm leading-relaxed">{{ row.observation.text }}</p>
        <p v-if="stage2Suggestion(row)?.reason_text" class="mt-1 text-[11px] text-zinc-400 italic">
          {{ stage2Suggestion(row)!.reason_text }}
        </p>

        <div class="mt-2 flex flex-wrap items-center gap-1.5">
          <button
            class="rounded-md px-2.5 py-1 text-xs font-medium ring-1 ring-inset transition"
            :class="t.stakes[row.observation.id] === 'confirm'
              ? 'bg-emerald-600 text-white ring-emerald-600'
              : 'text-emerald-700 ring-emerald-300 hover:bg-emerald-50 dark:text-emerald-400 dark:ring-emerald-800 dark:hover:bg-emerald-500/10'"
            @click.stop="stage2Stake(row.observation.id, 'confirm')"
          >
            confirm
          </button>
          <button
            class="rounded-md px-2.5 py-1 text-xs font-medium ring-1 ring-inset transition"
            :class="t.stakes[row.observation.id] === 'elevate'
              ? 'bg-violet-600 text-white ring-violet-600'
              : 'text-violet-700 ring-violet-300 hover:bg-violet-50 dark:text-violet-400 dark:ring-violet-800 dark:hover:bg-violet-500/10'"
            :title="row.observation.provenance === 'user_confirmed'
              ? 'elevate'
              : 'this row is not vouched yet — applying will chain its confirm first (two gestures, two log rows)'"
            @click.stop="stage2Stake(row.observation.id, 'elevate')"
          >
            elevate{{ row.observation.provenance === 'user_confirmed' ? '' : ' (chains confirm)' }}
          </button>
          <button
            class="ml-auto rounded-md px-2.5 py-1 text-xs font-medium ring-1 ring-inset transition"
            :class="t.stakes[row.observation.id] === 'decline'
              ? 'bg-zinc-600 text-white ring-zinc-600'
              : 'text-zinc-600 ring-zinc-300 hover:bg-zinc-50 dark:text-zinc-300 dark:ring-zinc-700 dark:hover:bg-zinc-800'"
            title="read it, not taking the suggestion — recorded so the suggestion stops sitting pending forever"
            @click.stop="stage2Stake(row.observation.id, 'decline')"
          >
            decline
          </button>
        </div>
      </article>
    </div>
  </div>
</template>
