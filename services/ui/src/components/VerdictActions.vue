<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
import { computed } from 'vue'
import type { Observation } from '../lib/types'
import { can, needsConfirmChain } from '../lib/verdicts'

// The shared action strip (harmony pass, 2026-07-31): availability, look,
// and key hints live in ONE place for both review surfaces. Flow ownership
// (reject reasons, elevation friction, rewrite editors) stays with the
// parent — this component only emits intent. Rewrite is deliberately set
// apart on the right with a distinct ghost/dashed look: it is not a verdict
// (it opens an editor and writes a NEW observation), and its key is w, not
// the r the label suggests.
const props = withDefaults(
  defineProps<{
    observation: Observation
    size?: 'row' | 'panel'
    // disables everything while a verdict call for this row is in flight
    pending?: boolean
    elevationPending?: boolean
  }>(),
  { size: 'row', pending: false, elevationPending: false },
)

const emit = defineEmits<{
  accept: []
  confirm: []
  reject: []
  lapse: []
  rewrite: []
  elevate: []
}>()

// The elevation bar (memoire:D191(b)), stated at the gesture and not only in
// the ledger: elevation is a standing-order subscription, not a truth tier.
// One constant so the chain and plain tooltips cannot drift from each other.
const ELEVATION_BAR =
  'standing order: rides EVERY session in this project when session-start injection is on (can_instruct) — elevate caveats/gotchas/invariants only; an important fact is confirmed, not elevated'

const chain = computed(() => needsConfirmChain(props.observation))
const panel = computed(() => props.size === 'panel')
const btn = computed(() =>
  panel.value
    ? 'flex items-center gap-1.5 rounded-lg px-3.5 py-1.5 text-sm font-medium transition active:scale-[0.98] disabled:opacity-50'
    : 'flex items-center gap-1 rounded-md px-2 py-0.5 text-[11px] font-medium transition disabled:opacity-50',
)
const key = computed(() =>
  panel.value
    ? 'rounded border px-1 font-mono text-[10px]'
    : 'rounded border px-0.5 font-mono text-[9px] leading-3',
)
</script>

<template>
  <div class="flex w-full flex-wrap items-center" :class="panel ? 'gap-2' : 'gap-1.5'">
    <button
      v-if="can(observation, 'accept')"
      :disabled="pending || elevationPending"
      :class="btn"
      class="bg-emerald-600 text-white hover:bg-emerald-500"
      @click="emit('accept')"
    >
      Accept <kbd :class="key" class="border-emerald-400/40 bg-emerald-500/30 text-emerald-50">a</kbd>
    </button>
    <button
      v-if="can(observation, 'confirm')"
      :disabled="pending || elevationPending"
      :class="btn"
      class="bg-emerald-800 text-white hover:bg-emerald-700"
      title="accept + I vouch — promotes provenance to user_confirmed"
      @click="emit('confirm')"
    >
      Confirm <kbd :class="key" class="border-emerald-400/40 bg-emerald-600/40 text-emerald-50">c</kbd>
    </button>
    <button
      v-if="can(observation, 'reject')"
      :disabled="pending || elevationPending"
      :class="btn"
      class="border border-rose-300 text-rose-600 hover:bg-rose-50 dark:border-rose-500/40 dark:text-rose-400 dark:hover:bg-rose-500/10"
      @click="emit('reject')"
    >
      Reject <kbd :class="key" class="border-rose-300/60 text-rose-500 dark:border-rose-500/40 dark:text-rose-400">r</kbd>
    </button>
    <button
      v-if="can(observation, 'lapse')"
      :disabled="pending || elevationPending"
      :class="btn"
      class="border border-zinc-300 text-zinc-500 hover:bg-zinc-100 dark:border-zinc-700 dark:text-zinc-400 dark:hover:bg-zinc-800"
      title="park unreviewed; reversible on encounter"
      @click="emit('lapse')"
    >
      Lapse <kbd :class="key" class="border-zinc-300 text-zinc-400 dark:border-zinc-600">l</kbd>
    </button>
    <button
      v-if="can(observation, 'elevate')"
      :disabled="pending || elevationPending"
      :class="btn"
      class="bg-violet-600 text-white hover:bg-violet-500"
      :title="
        chain ? `${ELEVATION_BAR} — chained: confirms first (I vouch), two review-log rows` : ELEVATION_BAR
      "
      @click="emit('elevate')"
    >
      {{ chain ? 'Confirm+Elevate' : 'Elevate' }}
      <kbd :class="key" class="border-violet-400/40 bg-violet-500/30 text-violet-50">e</kbd>
    </button>
    <button
      v-if="can(observation, 'rewrite')"
      :disabled="pending || elevationPending"
      :class="btn"
      class="ml-auto border border-dashed border-zinc-400 text-zinc-600 hover:border-zinc-500 hover:bg-zinc-50 dark:border-zinc-600 dark:text-zinc-300 dark:hover:bg-zinc-800"
      title="not a verdict: opens an editor — your wording becomes a NEW user_confirmed observation superseding this one"
      @click="emit('rewrite')"
    >
      ✎ Rewrite <kbd :class="key" class="border-zinc-300 text-zinc-400 dark:border-zinc-600">w</kbd>
    </button>
  </div>
</template>
