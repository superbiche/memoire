<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { clearToken, getToken, setUnauthorizedHandler } from './lib/api'
import TokenGate from './components/TokenGate.vue'
import TopBar from './components/TopBar.vue'
import ReviewView from './components/ReviewView.vue'
import BrowseView from './components/BrowseView.vue'
import SessionsView from './components/SessionsView.vue'
import ToastHost from './components/ToastHost.vue'
import { loadQueue, state } from './stores/review'
import { jumpToObservation } from './stores/sessions'

const authed = ref(Boolean(getToken()))
const tab = ref<'review' | 'browse' | 'sessions'>('review')

// TopBar "#id" jump: land on the observation inside its session story —
// the surface that already carries every verdict affordance.
async function onJump(id: number) {
  tab.value = 'sessions'
  await jumpToObservation(id)
}

setUnauthorizedHandler(() => {
  clearToken()
  authed.value = false
})

function onAuthed() {
  authed.value = true
  void loadQueue()
}

onMounted(() => {
  if (authed.value && !state.loaded) void loadQueue()
})
</script>

<template>
  <TokenGate v-if="!authed" @authed="onAuthed" />
  <div v-else class="flex h-screen flex-col">
    <TopBar v-model:tab="tab" @jump="onJump" />
    <main class="min-h-0 flex-1">
      <!-- ReviewView stays v-show (queue state survives tab hops) but its
           window key listener is gated on `active`: un-gated, a/c/r on the
           Sessions or Browse tab silently applied verdicts to the HIDDEN
           queue's selected card (defect found 2026-07-30). -->
      <ReviewView v-show="tab === 'review'" :active="tab === 'review'" />
      <BrowseView v-if="tab === 'browse'" />
      <SessionsView v-if="tab === 'sessions'" />
    </main>
  </div>
  <ToastHost />
</template>
