<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
import { computed } from 'vue'
import type { Observation } from '../lib/types'
import { kindStyle } from '../lib/kinds'
import { fmtRelative, pct } from '../lib/format'
import KindBadge from './KindBadge.vue'
import ElevatedBadge from './ElevatedBadge.vue'
import UsageBadge from './UsageBadge.vue'

const props = defineProps<{ observation: Observation; selected: boolean }>()
const style = computed(() => kindStyle(props.observation.kind))
</script>

<template>
  <button
    type="button"
    class="group relative w-full overflow-hidden rounded-xl border bg-white text-left transition-all duration-200 dark:bg-zinc-900"
    :class="
      selected
        ? 'border-violet-400/70 border-l-4 border-l-violet-500 shadow-lg shadow-violet-500/10 ring-2 ring-violet-500/70 dark:border-violet-500/50 dark:border-l-violet-400 dark:ring-violet-400/60'
        : 'border-zinc-200 hover:border-zinc-300 hover:shadow-md hover:shadow-zinc-950/5 hover:ring-2 hover:ring-violet-300/50 dark:border-zinc-800 dark:hover:border-zinc-700 dark:hover:ring-violet-500/30'
    "
  >
    <span class="absolute inset-y-0 left-0 w-1" :class="style.accent" />
    <div class="flex flex-col gap-2 py-3 pr-4 pl-4">
      <div class="flex items-center gap-2">
        <KindBadge :kind="observation.kind" proposed />
        <span
          class="rounded-md bg-zinc-100 px-1.5 py-0.5 font-mono text-[11px] text-zinc-500 dark:bg-zinc-800 dark:text-zinc-400"
          >{{ observation.project }}</span
        >
        <ElevatedBadge v-if="observation.can_instruct" />
        <span
          v-if="observation.noise_candidate"
          class="rounded-md bg-zinc-100 px-1.5 py-0.5 text-[10px] font-medium tracking-wide text-zinc-400 uppercase dark:bg-zinc-800"
          >noise</span
        >
        <span
          v-if="observation.needs_confirmation"
          class="rounded-md bg-amber-100 px-1.5 py-0.5 text-[10px] font-medium tracking-wide text-amber-600 uppercase dark:bg-amber-500/10 dark:text-amber-400"
          >confirm</span
        >
        <span class="ml-auto shrink-0 text-[11px] text-zinc-400 dark:text-zinc-500">{{
          fmtRelative(observation.occurred_at_epoch)
        }}</span>
      </div>
      <p class="line-clamp-2 text-sm leading-snug text-zinc-700 dark:text-zinc-300">
        {{ observation.text }}
      </p>
      <div class="flex items-center gap-2 text-[11px] text-zinc-400 dark:text-zinc-500">
        <span class="font-mono">#{{ observation.id }}</span>
        <span class="flex items-center gap-1.5">
          <span class="h-1 w-10 overflow-hidden rounded-full bg-zinc-200 dark:bg-zinc-800">
            <span
              class="block h-full rounded-full"
              :class="style.accent"
              :style="{ width: pct(observation.confidence) }"
            />
          </span>
          {{ pct(observation.confidence) }}
        </span>
        <UsageBadge :usage="observation.usage" />
      </div>
    </div>
  </button>
</template>
