<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
import { ref, watch } from 'vue'
import { api } from '../lib/api'
import type { UsageLine, UsageSummary } from '../lib/types'
import { fmtRelative } from '../lib/format'
import UsageBadge from './UsageBadge.vue'

// Attribution at Stop, per observation: the reason line each consuming turn
// wrote next to its verdict, so the operator reads the hit against the claim
// (the campaign's `used`-not-carried errors live at that level). Fetched on
// demand for the selected observation; "no usage signal" when no memory_usage
// event names it.
const props = defineProps<{ observationId: number }>()
const usage = ref<UsageSummary | null>(null)
const lines = ref<UsageLine[]>([])
const loading = ref(false)
const error = ref('')

const verdictClass: Record<string, string> = {
  used: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-500/10 dark:text-emerald-400',
  unused: 'bg-zinc-100 text-zinc-500 dark:bg-zinc-800 dark:text-zinc-400',
  misapplied: 'bg-rose-100 text-rose-600 dark:bg-rose-500/10 dark:text-rose-400',
  'contradicted-by-files': 'bg-rose-100 text-rose-600 dark:bg-rose-500/10 dark:text-rose-400',
  'fetch-failed': 'bg-amber-100 text-amber-600 dark:bg-amber-500/10 dark:text-amber-400',
}

async function load(id: number) {
  loading.value = true
  error.value = ''
  try {
    const res = await api.getUsage(id)
    if (id !== props.observationId) return
    usage.value = res.usage
    lines.value = res.lines
  } catch (e) {
    if (id !== props.observationId) return
    usage.value = null
    lines.value = []
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    if (id === props.observationId) loading.value = false
  }
}

watch(
  () => props.observationId,
  (id) => {
    usage.value = null
    lines.value = []
    void load(id)
  },
  { immediate: true },
)
</script>

<template>
  <div class="mt-6 text-xs">
    <div class="mb-1.5 flex items-center gap-2">
      <span class="font-medium tracking-wide text-zinc-400 uppercase">usage</span>
      <UsageBadge :usage="usage" />
      <span v-if="loading" class="text-zinc-400">…</span>
    </div>
    <p v-if="error" class="text-rose-500">usage unavailable: {{ error }}</p>
    <p v-else-if="!loading && !usage" class="text-zinc-400 dark:text-zinc-500">no usage signal yet</p>
    <ul v-else-if="lines.length" class="flex flex-col gap-1.5">
      <li
        v-for="line in lines"
        :key="line.event_id"
        class="rounded-md border border-zinc-200 px-2 py-1.5 dark:border-zinc-800"
      >
        <div class="flex items-center gap-2">
          <span class="rounded px-1.5 py-0.5 font-mono text-[10px]" :class="verdictClass[line.verdict] ?? verdictClass.unused">{{
            line.verdict
          }}</span>
          <span class="truncate font-mono text-[10px] text-zinc-400" :title="line.session_id">{{ line.session_id }}</span>
          <span class="ml-auto shrink-0 text-[10px] text-zinc-400">{{ fmtRelative(line.occurred_at_epoch) }}</span>
        </div>
        <p v-if="line.reason" class="mt-1 leading-snug text-zinc-600 dark:text-zinc-300">{{ line.reason }}</p>
      </li>
    </ul>
  </div>
</template>
