<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
import { onMounted, ref } from 'vue'

// The elevation friction box, extracted (harmony pass, 2026-07-31) so the
// live-queue elevate and the history/browse elevate/demote share one moment.
// This is the one place in this app where friction is a feature: an elevated
// observation rides every future session (memoire:D191/D192).
const props = defineProps<{
  mode: 'elevate' | 'demote'
  project: string
  // elevate only: announce the chained confirm (memoire:D192)
  chain?: boolean
  busy?: boolean
}>()

const emit = defineEmits<{
  confirm: [reason: string]
  cancel: []
}>()

const reason = ref('')
const input = ref<HTMLInputElement | null>(null)

onMounted(() => input.value?.focus())

function submit() {
  emit('confirm', reason.value.trim())
}
</script>

<template>
  <div
    class="rounded-lg border px-3 py-2.5"
    :class="
      mode === 'elevate'
        ? 'border-violet-300 bg-violet-50/60 dark:border-violet-500/40 dark:bg-violet-500/5'
        : 'border-amber-300 bg-amber-50/60 dark:border-amber-500/40 dark:bg-amber-500/5'
    "
  >
    <p class="text-xs leading-relaxed text-zinc-600 dark:text-zinc-300">
      <template v-if="mode === 'elevate'">
        Elevated observations ride the operating notes of
        <strong>every future {{ project }} session</strong> until demoted. Rare by design — is
        this one of them?
        <template v-if="props.chain">
          <br />This will <strong>confirm first</strong> (provenance → user_confirmed, "I
          vouch"), then elevate — two review-log rows.
        </template>
      </template>
      <template v-else>
        Demote takes this observation out of the operating notes: future sessions stop seeing
        it. It stays accepted in memory.
      </template>
    </p>
    <div class="mt-2 flex items-center gap-2">
      <input
        ref="input"
        v-model="reason"
        type="text"
        placeholder="reason (optional)"
        class="min-w-0 flex-1 rounded-lg border border-zinc-200 bg-white px-3 py-1.5 text-sm outline-none focus:ring-2 focus:ring-violet-400/30 dark:border-zinc-700 dark:bg-zinc-900"
        @keydown.enter.prevent="submit"
        @keydown.esc.prevent="emit('cancel')"
      />
      <button
        :disabled="busy"
        class="shrink-0 rounded-lg px-3 py-1.5 text-sm font-medium text-white transition disabled:opacity-50"
        :class="mode === 'elevate' ? 'bg-violet-600 hover:bg-violet-500' : 'bg-amber-600 hover:bg-amber-500'"
        @click="submit"
      >
        {{ mode === 'elevate' ? 'Confirm elevate' : 'Confirm demote' }}
      </button>
      <button
        class="rounded-lg px-2 py-1.5 text-sm text-zinc-500 transition hover:text-zinc-700 dark:hover:text-zinc-300"
        @click="emit('cancel')"
      >
        Cancel
      </button>
    </div>
  </div>
</template>
