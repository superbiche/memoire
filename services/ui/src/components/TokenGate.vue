<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
import { ref } from 'vue'
import { api, clearToken, setToken } from '../lib/api'

const emit = defineEmits<{ authed: [] }>()

const value = ref('')
const error = ref('')
const checking = ref(false)

async function submit() {
  const token = value.value.trim()
  if (!token || checking.value) return
  checking.value = true
  error.value = ''
  setToken(token)
  try {
    await api.stats()
    emit('authed')
  } catch {
    // Do not let a known-bad token linger in localStorage.
    clearToken()
    error.value = 'Token rejected by the API — check ~/.memoire/token'
  } finally {
    checking.value = false
  }
}
</script>

<template>
  <div class="flex h-screen items-center justify-center p-6">
    <div
      class="w-full max-w-md rounded-2xl border border-zinc-200 bg-white p-8 shadow-xl shadow-zinc-950/5 dark:border-zinc-800 dark:bg-zinc-900 dark:shadow-black/30"
    >
      <div class="mb-1 flex items-baseline gap-2">
        <h1 class="text-xl font-semibold tracking-tight">memoire</h1>
        <span
          class="rounded-full bg-violet-100 px-2 py-0.5 text-xs font-medium text-violet-700 dark:bg-violet-500/15 dark:text-violet-300"
          >review</span
        >
      </div>
      <p class="mb-6 text-sm text-zinc-500 dark:text-zinc-400">
        Paste your bearer token once — it stays in this browser's localStorage.
      </p>
      <form class="space-y-3" @submit.prevent="submit">
        <input
          v-model="value"
          type="password"
          autofocus
          placeholder="contents of ~/.memoire/token"
          class="w-full rounded-lg border border-zinc-300 bg-zinc-50 px-3 py-2 font-mono text-sm outline-none focus:border-violet-400 focus:ring-2 focus:ring-violet-400/30 dark:border-zinc-700 dark:bg-zinc-950"
        />
        <button
          type="submit"
          :disabled="checking || !value.trim()"
          class="w-full rounded-lg bg-violet-600 px-3 py-2 text-sm font-medium text-white transition hover:bg-violet-500 disabled:opacity-50"
        >
          {{ checking ? 'Checking…' : 'Unlock' }}
        </button>
        <p v-if="error" class="text-sm text-rose-500">{{ error }}</p>
      </form>
    </div>
  </div>
</template>
