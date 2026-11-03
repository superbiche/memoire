<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
import { computed } from 'vue'
import type { UsageSummary } from '../lib/types'

// Attribution-at-Stop aggregate on the card. Nothing is rendered when the
// summary is absent: an absent usage signal is absent, not "unused".
const props = defineProps<{ usage?: UsageSummary | null }>()
const warn = computed(() => (props.usage?.misapplied ?? 0) + (props.usage?.contradicted_by_files ?? 0))
const title = computed(() => {
  const u = props.usage
  if (!u) return ''
  return `attribution at Stop: used ${u.used}, unused ${u.unused}, misapplied ${u.misapplied}, contradicted by files ${u.contradicted_by_files}, fetch failed ${u.fetch_failed}, over ${u.sessions} session(s)`
})
</script>

<template>
  <span
    v-if="usage"
    :title="title"
    class="inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 font-mono text-[10px]"
    :class="
      warn > 0
        ? 'bg-rose-100 text-rose-600 dark:bg-rose-500/10 dark:text-rose-400'
        : usage.used > 0
          ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-500/10 dark:text-emerald-400'
          : 'bg-zinc-100 text-zinc-500 dark:bg-zinc-800 dark:text-zinc-400'
    "
  >
    <span>used {{ usage.used }}</span>
    <span class="opacity-60">·</span>
    <span>unused {{ usage.unused }}</span>
    <template v-if="warn > 0">
      <span class="opacity-60">·</span>
      <span>{{ usage.misapplied ? `misapplied ${usage.misapplied}` : '' }}{{ usage.misapplied && usage.contradicted_by_files ? ' ' : '' }}{{ usage.contradicted_by_files ? `contradicted ${usage.contradicted_by_files}` : '' }}</span>
    </template>
  </span>
</template>
