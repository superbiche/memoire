<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
import { toasts } from '../stores/toast'

const tones: Record<string, string> = {
  success:
    'border-emerald-200 bg-emerald-50 text-emerald-800 dark:border-emerald-500/30 dark:bg-emerald-950 dark:text-emerald-200',
  danger:
    'border-rose-200 bg-rose-50 text-rose-800 dark:border-rose-500/30 dark:bg-rose-950 dark:text-rose-200',
  error:
    'border-rose-300 bg-rose-100 text-rose-900 dark:border-rose-500/50 dark:bg-rose-900 dark:text-rose-100',
  info: 'border-zinc-200 bg-white text-zinc-700 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-200',
}
</script>

<template>
  <div class="pointer-events-none fixed right-4 bottom-4 z-50 flex flex-col items-end gap-2">
    <TransitionGroup name="toast">
      <div
        v-for="t in toasts"
        :key="t.id"
        class="rounded-lg border px-3 py-2 text-sm shadow-lg"
        :class="tones[t.tone]"
      >
        {{ t.message }}
      </div>
    </TransitionGroup>
  </div>
</template>
