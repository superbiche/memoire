<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
import { kindLabel, kindStyle } from '../lib/kinds'

defineProps<{
  projects: Array<{ name: string; count: number }>
  kinds: Array<{ name: string; count: number }>
  noiseCount: number | null
}>()

const project = defineModel<string>('project', { required: true })
const kind = defineModel<string>('kind', { required: true })
const includeNoise = defineModel<boolean>('includeNoise', { required: true })
const sortDir = defineModel<'desc' | 'asc'>('sortDir', { required: true })
</script>

<template>
  <div class="flex flex-wrap items-center gap-x-4 gap-y-2">
    <div class="flex flex-wrap items-center gap-1.5">
      <span class="text-[11px] font-medium tracking-wide text-zinc-400 uppercase">project</span>
      <button
        v-for="p in projects"
        :key="p.name"
        class="rounded-full border px-2.5 py-0.5 font-mono text-xs transition"
        :class="
          project === p.name
            ? 'border-violet-400/60 bg-violet-50 text-violet-700 dark:bg-violet-500/15 dark:text-violet-300'
            : 'border-zinc-200 text-zinc-500 hover:border-zinc-300 hover:text-zinc-700 dark:border-zinc-800 dark:hover:border-zinc-700 dark:hover:text-zinc-300'
        "
        @click="project = project === p.name ? '' : p.name"
      >
        {{ p.name }} <span class="opacity-60">{{ p.count }}</span>
      </button>
    </div>

    <div class="flex flex-wrap items-center gap-1.5">
      <span class="text-[11px] font-medium tracking-wide text-zinc-400 uppercase">kind</span>
      <button
        v-for="k in kinds"
        :key="k.name"
        class="flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs transition"
        :class="
          kind === k.name
            ? 'border-violet-400/60 bg-violet-50 text-violet-700 dark:bg-violet-500/15 dark:text-violet-300'
            : 'border-zinc-200 text-zinc-500 hover:border-zinc-300 hover:text-zinc-700 dark:border-zinc-800 dark:hover:border-zinc-700 dark:hover:text-zinc-300'
        "
        @click="kind = kind === k.name ? '' : k.name"
      >
        <span class="size-1.5 rounded-full" :class="kindStyle(k.name).dot" />
        {{ kindLabel(k.name) }} <span class="opacity-60">{{ k.count }}</span>
      </button>
    </div>

    <button
      type="button"
      class="ml-auto flex items-center gap-1.5 rounded-full border border-zinc-200 px-2.5 py-0.5 text-xs text-zinc-500 transition hover:border-zinc-300 hover:text-zinc-700 dark:border-zinc-800 dark:hover:border-zinc-700 dark:hover:text-zinc-300"
      :title="sortDir === 'desc' ? 'recent first — click for oldest first' : 'oldest first — click for recent first'"
      @click="sortDir = sortDir === 'desc' ? 'asc' : 'desc'"
    >
      date {{ sortDir === 'desc' ? '↓' : '↑' }}
      <span class="opacity-60">{{ sortDir === 'desc' ? 'recent first' : 'oldest first' }}</span>
    </button>

    <label
      class="flex cursor-pointer items-center gap-2 text-xs text-zinc-500 select-none dark:text-zinc-400"
    >
      <span
        class="relative inline-flex h-4.5 w-8 items-center rounded-full transition"
        :class="includeNoise ? 'bg-violet-500' : 'bg-zinc-300 dark:bg-zinc-700'"
      >
        <span
          class="absolute size-3.5 rounded-full bg-white shadow transition-all"
          :class="includeNoise ? 'left-4' : 'left-0.5'"
        />
      </span>
      <input v-model="includeNoise" type="checkbox" class="sr-only" />
      include noise <span v-if="noiseCount !== null" class="opacity-60">({{ noiseCount }})</span>
    </label>
  </div>
</template>
