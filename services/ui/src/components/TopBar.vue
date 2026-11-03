<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
import { ref } from 'vue'
import { acceptedCount, confirmedCount, rejectedCount, state } from '../stores/review'
import { totalStaked } from '../stores/triage'

const tab = defineModel<'review' | 'triage' | 'stage2' | 'browse' | 'sessions'>('tab', { required: true })
const emit = defineEmits<{ jump: [id: number] }>()

// "#id" jump: observation ids arrive from injection blocks and chat as
// [332152]-style references; this is the affordance that makes them landable.
const jumpText = ref('')

function submitJump(e: Event) {
  const id = Number.parseInt(jumpText.value.replace(/[^0-9]/g, ''), 10)
  if (!Number.isFinite(id) || id <= 0) return
  jumpText.value = ''
  // Hand the keyboard back to the story: focus left in the input would
  // swallow the j/k/verdict keys the jump exists to serve.
  ;(e.target as HTMLElement | null)?.blur()
  emit('jump', id)
}
</script>

<template>
  <header
    class="sticky top-0 z-10 border-b border-zinc-200/80 bg-white/70 backdrop-blur-md dark:border-zinc-800/80 dark:bg-zinc-950/70"
  >
    <div class="mx-auto flex h-14 max-w-7xl items-center gap-6 px-4 sm:px-6">
      <div class="flex items-baseline gap-2">
        <span class="text-lg font-semibold tracking-tight">memoire</span>
        <span
          class="rounded-full bg-violet-100 px-2 py-0.5 text-[11px] font-medium text-violet-700 dark:bg-violet-500/15 dark:text-violet-300"
          >review</span
        >
      </div>

      <nav class="flex items-center gap-1 rounded-lg bg-zinc-100 p-1 dark:bg-zinc-900">
        <button
          v-for="t in ['review', 'triage', 'stage2', 'browse', 'sessions'] as const"
          :key="t"
          class="rounded-md px-3 py-1 text-sm font-medium capitalize transition"
          :class="
            tab === t
              ? 'bg-white text-zinc-900 shadow-sm dark:bg-zinc-700 dark:text-zinc-50'
              : 'text-zinc-500 hover:text-zinc-700 dark:text-zinc-400 dark:hover:text-zinc-200'
          "
          @click="tab = t"
        >
          {{ t }}
          <span
            v-if="t === 'review' && state.items.length"
            class="ml-1 rounded-full bg-violet-600/90 px-1.5 text-[10px] font-semibold text-white"
            >{{ state.items.length }}</span
          >
          <!-- Staked-but-unapplied rows are the one thing a tab hop could
               lose sight of: the badge is what keeps them from being
               forgotten on another screen. -->
          <span
            v-if="t === 'triage' && totalStaked"
            class="ml-1 rounded-full bg-amber-500/90 px-1.5 text-[10px] font-semibold text-white"
            >{{ totalStaked }}</span
          >
        </button>
      </nav>

      <div class="ml-auto flex items-center gap-4 text-xs text-zinc-500 dark:text-zinc-400">
        <span v-if="acceptedCount + confirmedCount + rejectedCount" class="hidden items-center gap-2 sm:flex">
          <span class="flex items-center gap-1 text-emerald-600 dark:text-emerald-400">
            <span class="size-1.5 rounded-full bg-emerald-500" />{{ acceptedCount }} accepted
          </span>
          <span v-if="confirmedCount" class="flex items-center gap-1 text-emerald-700 dark:text-emerald-300">
            <span class="size-1.5 rounded-full bg-emerald-700" />{{ confirmedCount }} confirmed
          </span>
          <span class="flex items-center gap-1 text-rose-600 dark:text-rose-400">
            <span class="size-1.5 rounded-full bg-rose-500" />{{ rejectedCount }} rejected
          </span>
        </span>
        <input
          v-model="jumpText"
          placeholder="#id"
          class="w-20 rounded-md border border-zinc-200 bg-white px-2 py-1 font-mono text-xs outline-none focus:ring-2 focus:ring-violet-400/30 dark:border-zinc-700 dark:bg-zinc-900"
          title="jump to an observation by id (opens its session story)"
          @keydown.enter="submitJump"
        />
        <span v-if="tab === 'review'" class="hidden items-center gap-2 font-mono md:flex">
          <kbd>j</kbd><kbd>k</kbd> move <kbd>a</kbd> accept <kbd>c</kbd> confirm <kbd>r</kbd> reject <kbd>l</kbd> lapse
          <kbd>e</kbd> elevate <kbd>w</kbd> rewrite <kbd>u</kbd> back
        </span>
        <span v-else-if="tab === 'triage'" class="hidden items-center gap-2 font-mono md:flex">
          <kbd>j</kbd><kbd>k</kbd> move <kbd>&larr;</kbd> accept <kbd>&rarr;</kbd> lapse <kbd>n</kbd> noise genre
          <kbd>x</kbd> reject <kbd>space</kbd> unstake
        </span>
        <span v-else-if="tab === 'stage2'" class="hidden items-center gap-2 font-mono md:flex">
          <kbd>j</kbd><kbd>k</kbd> move <kbd>c</kbd> confirm <kbd>e</kbd> elevate <kbd>d</kbd> decline
        </span>
        <span v-else-if="tab === 'sessions'" class="hidden items-center gap-2 font-mono md:flex">
          <kbd>j</kbd><kbd>k</kbd> rows <kbd>n</kbd><kbd>p</kbd> sessions <kbd>a</kbd> accept <kbd>c</kbd> confirm
          <kbd>r</kbd> reject <kbd>l</kbd> lapse <kbd>e</kbd> elevate <kbd>w</kbd> rewrite
        </span>
      </div>
    </div>
  </header>
</template>
