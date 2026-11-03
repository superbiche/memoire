<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
import { nextTick, ref, watch } from 'vue'
import type { Observation, ReviewedEntry } from '../lib/types'
import { fmtDateTime, pct, shortHash } from '../lib/format'
import { kindStyle } from '../lib/kinds'
import { needsConfirmChain } from '../lib/verdicts'
import KindBadge from './KindBadge.vue'
import ElevatedBadge from './ElevatedBadge.vue'
import ElevationConfirm from './ElevationConfirm.vue'
import VerdictActions from './VerdictActions.vue'
import UsageSection from './UsageSection.vue'

const props = withDefaults(
  defineProps<{
    observation: Observation | null
    // When set, the panel shows an already-reviewed item (u key history).
    reviewedEntry: ReviewedEntry | null
    rejecting: boolean
    // Live-queue flow states, lifted to the parent (harmony pass 2026-07-31)
    // so the keyboard (e/w) and the buttons drive the same refs: while true,
    // the action row is replaced by the elevation friction box / the
    // rewrite editor.
    elevating?: boolean
    rewriting?: boolean
    // True while an elevate/demote request for this observation is in flight;
    // controls are disabled so a double-click cannot fire twice.
    elevationBusy?: boolean
    // 'review' drives the queue (verdicts on live items, elevation on this
    // session's accepted history). 'browse' is the read surface's detail
    // moment: no verdicts ever, elevation on any accepted observation —
    // still operator-pull, still behind the confirm step.
    mode?: 'review' | 'browse'
  }>(),
  { mode: 'review', elevating: false, rewriting: false },
)

const emit = defineEmits<{
  accept: []
  confirm: []
  reject: [reason: string]
  'cancel-reject': []
  'start-reject': []
  lapse: []
  // live-queue rewrite: the parent owns the store call
  'start-rewrite': []
  rewrite: [text: string, reason: string]
  'cancel-rewrite': []
  // live-queue elevation (chained confirm+elevate for proposed rows, D192)
  'start-elevate': []
  'elevate-proposed': [reason: string]
  'cancel-elevate': []
  // history/browse elevation on accepted rows (unchanged contract)
  elevate: [reason: string]
  demote: [reason: string]
}>()

const reason = ref('')
const reasonInput = ref<HTMLInputElement | null>(null)

// History/browse elevate/demote go through the same friction box as the
// live queue (ElevationConfirm): an elevated observation rides every future
// session, so this is the one place where friction is a feature.
const confirming = ref<'elevate' | 'demote' | null>(null)

// Live-queue rewrite editor state; seeded from the observation each time
// the parent opens the editor.
const rewriteText = ref('')
const rewriteReason = ref('')
const rewriteEl = ref<HTMLTextAreaElement | null>(null)

watch(
  () => props.rejecting,
  async (on) => {
    if (on) {
      reason.value = ''
      await nextTick()
      reasonInput.value?.focus()
    }
  },
)

watch(
  () => props.rewriting,
  async (on) => {
    if (on && props.observation) {
      rewriteText.value = props.observation.text
      rewriteReason.value = ''
      await nextTick()
      rewriteEl.value?.focus()
    }
  },
)

watch(
  () => props.observation?.id,
  () => {
    confirming.value = null
  },
)

function confirmReject() {
  emit('reject', reason.value.trim())
}

function submitRewrite() {
  emit('rewrite', rewriteText.value, rewriteReason.value.trim())
}

function confirmHistoryElevation(reasonText: string) {
  if (confirming.value === 'elevate') emit('elevate', reasonText)
  else if (confirming.value === 'demote') emit('demote', reasonText)
  confirming.value = null
}
</script>

<template>
  <aside
    class="flex h-full min-h-0 flex-col overflow-hidden rounded-xl border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900"
  >
    <template v-if="observation">
      <Transition name="detail" mode="out-in">
        <div :key="observation.id" class="flex min-h-0 flex-1 flex-col">
          <!-- Reviewed banner (history view via u) -->
          <!-- Only a rejection reads red: confirmed is the queue's STRONGER
               green (round-1 review MINOR 1 — rewrite/elevate history
               entries carry verdict 'confirmed'). -->
          <div
            v-if="reviewedEntry"
            class="flex items-center gap-2 border-b px-5 py-2 text-xs font-medium"
            :class="
              reviewedEntry.verdict === 'rejected'
                ? 'border-rose-200 bg-rose-50 text-rose-700 dark:border-rose-500/20 dark:bg-rose-500/10 dark:text-rose-300'
                : 'border-emerald-200 bg-emerald-50 text-emerald-700 dark:border-emerald-500/20 dark:bg-emerald-500/10 dark:text-emerald-300'
            "
          >
            <span
              class="size-1.5 rounded-full"
              :class="
                reviewedEntry.verdict === 'rejected'
                  ? 'bg-rose-500'
                  : reviewedEntry.verdict === 'confirmed'
                    ? 'bg-emerald-700'
                    : 'bg-emerald-500'
              "
            />
            {{ reviewedEntry.verdict }} this session
            <span v-if="reviewedEntry.reason" class="font-normal opacity-80">— {{ reviewedEntry.reason }}</span>
            <span class="ml-auto font-normal opacity-60">read-only · no unreview</span>
          </div>

          <div class="min-h-0 flex-1 overflow-y-auto px-5 py-4">
            <div class="mb-3 flex flex-wrap items-center gap-2">
              <KindBadge :kind="observation.kind" :proposed="observation.review_state === 'proposed' || observation.review_state === 'lapsed'" />
              <span
                class="rounded-md bg-zinc-100 px-1.5 py-0.5 font-mono text-[11px] text-zinc-500 dark:bg-zinc-800 dark:text-zinc-400"
                >{{ observation.project }}</span
              >
              <ElevatedBadge v-if="observation.can_instruct" />
              <span class="ml-auto font-mono text-xs text-zinc-400">#{{ observation.id }}</span>
            </div>

            <p class="text-[15px] leading-relaxed whitespace-pre-wrap text-zinc-800 dark:text-zinc-200">
              {{ observation.text }}
            </p>

            <dl class="mt-6 grid grid-cols-2 gap-x-4 gap-y-3 text-xs">
              <div>
                <dt class="mb-0.5 font-medium tracking-wide text-zinc-400 uppercase">confidence</dt>
                <dd class="flex items-center gap-2 text-zinc-600 dark:text-zinc-300">
                  <span class="h-1.5 w-16 overflow-hidden rounded-full bg-zinc-200 dark:bg-zinc-800">
                    <span
                      class="block h-full rounded-full"
                      :class="kindStyle(observation.kind).accent"
                      :style="{ width: pct(observation.confidence) }"
                    />
                  </span>
                  {{ pct(observation.confidence) }}
                </dd>
              </div>
              <div>
                <dt class="mb-0.5 font-medium tracking-wide text-zinc-400 uppercase">provenance</dt>
                <dd class="text-zinc-600 dark:text-zinc-300">{{ observation.provenance }}</dd>
              </div>
              <div>
                <dt class="mb-0.5 font-medium tracking-wide text-zinc-400 uppercase">occurred</dt>
                <dd class="text-zinc-600 dark:text-zinc-300">{{ fmtDateTime(observation.occurred_at_epoch) }}</dd>
              </div>
              <div>
                <dt class="mb-0.5 font-medium tracking-wide text-zinc-400 uppercase">derived</dt>
                <dd class="text-zinc-600 dark:text-zinc-300">{{ fmtDateTime(observation.created_at_epoch) }}</dd>
              </div>
              <div class="col-span-2">
                <dt class="mb-0.5 font-medium tracking-wide text-zinc-400 uppercase">session</dt>
                <dd class="truncate font-mono text-zinc-500 dark:text-zinc-400">{{ observation.session_id }}</dd>
              </div>
              <div class="col-span-2">
                <dt class="mb-0.5 font-medium tracking-wide text-zinc-400 uppercase">derivation run</dt>
                <dd class="truncate font-mono text-zinc-500 dark:text-zinc-400">{{ observation.derivation_run_id }}</dd>
              </div>
              <div>
                <dt class="mb-0.5 font-medium tracking-wide text-zinc-400 uppercase">content hash</dt>
                <dd class="font-mono text-zinc-500 dark:text-zinc-400">{{ shortHash(observation.content_hash) }}</dd>
              </div>
              <div>
                <dt class="mb-0.5 font-medium tracking-wide text-zinc-400 uppercase">source events</dt>
                <dd class="font-mono text-zinc-500 dark:text-zinc-400">
                  {{ (observation.source_event_ids ?? []).join(', ') || '—' }}
                </dd>
              </div>
              <div class="col-span-2 flex gap-1.5">
                <span
                  v-for="flag in [
                    ['evidence', observation.can_evidence],
                    ['instruct', observation.can_instruct],
                    ['needs confirmation', observation.needs_confirmation],
                    ['noise candidate', observation.noise_candidate],
                  ].filter(([, v]) => v)"
                  :key="String(flag[0])"
                  class="rounded-md bg-zinc-100 px-1.5 py-0.5 text-[10px] font-medium tracking-wide text-zinc-500 uppercase dark:bg-zinc-800 dark:text-zinc-400"
                  >{{ flag[0] }}</span
                >
              </div>
            </dl>
            <UsageSection :observation-id="observation.id" />
          </div>

          <!-- Actions -->
          <div class="border-t border-zinc-200 px-5 py-3 dark:border-zinc-800">
            <div v-if="mode === 'review' && !reviewedEntry">
              <div v-if="rejecting" class="flex items-center gap-2">
                <input
                  ref="reasonInput"
                  v-model="reason"
                  type="text"
                  placeholder="rejection reason (optional) — Enter to confirm, Esc to cancel"
                  class="min-w-0 flex-1 rounded-lg border border-rose-300 bg-rose-50/50 px-3 py-1.5 text-sm outline-none focus:ring-2 focus:ring-rose-400/30 dark:border-rose-500/40 dark:bg-rose-500/5"
                  @keydown.enter.prevent="confirmReject"
                  @keydown.esc.prevent="emit('cancel-reject')"
                />
                <button
                  class="rounded-lg bg-rose-600 px-3 py-1.5 text-sm font-medium text-white transition hover:bg-rose-500"
                  @click="confirmReject"
                >
                  Reject
                </button>
                <button
                  class="rounded-lg px-2 py-1.5 text-sm text-zinc-500 transition hover:text-zinc-700 dark:hover:text-zinc-300"
                  @click="emit('cancel-reject')"
                >
                  Cancel
                </button>
              </div>
              <div v-else-if="rewriting" class="space-y-2">
                <textarea
                  ref="rewriteEl"
                  v-model="rewriteText"
                  rows="4"
                  class="w-full rounded-lg border border-zinc-300 bg-white p-2.5 text-sm outline-none focus:ring-2 focus:ring-violet-400/30 dark:border-zinc-700 dark:bg-zinc-950"
                  @keydown.esc.prevent="emit('cancel-rewrite')"
                />
                <div class="flex items-center gap-2">
                  <input
                    v-model="rewriteReason"
                    type="text"
                    placeholder="reason (optional) — Enter to rewrite, Esc to cancel"
                    class="min-w-0 flex-1 rounded-lg border border-zinc-200 bg-white px-3 py-1.5 text-sm outline-none focus:ring-2 focus:ring-violet-400/30 dark:border-zinc-700 dark:bg-zinc-900"
                    @keydown.enter.prevent="submitRewrite"
                    @keydown.esc.prevent="emit('cancel-rewrite')"
                  />
                  <button
                    class="shrink-0 rounded-lg bg-violet-600 px-3 py-1.5 text-sm font-medium text-white transition hover:bg-violet-500"
                    @click="submitRewrite"
                  >
                    Rewrite
                  </button>
                  <button
                    class="rounded-lg px-2 py-1.5 text-sm text-zinc-500 transition hover:text-zinc-700 dark:hover:text-zinc-300"
                    @click="emit('cancel-rewrite')"
                  >
                    Cancel
                  </button>
                </div>
              </div>
              <ElevationConfirm
                v-else-if="elevating"
                mode="elevate"
                :project="observation.project"
                :chain="needsConfirmChain(observation)"
                :busy="elevationBusy"
                @confirm="(r) => emit('elevate-proposed', r)"
                @cancel="emit('cancel-elevate')"
              />
              <VerdictActions
                v-else
                :observation="observation"
                size="panel"
                :elevation-pending="elevationBusy"
                @accept="emit('accept')"
                @confirm="emit('confirm')"
                @reject="emit('start-reject')"
                @lapse="emit('lapse')"
                @rewrite="emit('start-rewrite')"
                @elevate="emit('start-elevate')"
              />
            </div>
            <div v-else class="space-y-2.5">
              <!-- Elevation is operator-pull on accepted observations only:
                   no queue, no candidate list, no nag. -->
              <template v-if="observation.review_state === 'accepted'">
                <ElevationConfirm
                  v-if="confirming"
                  :mode="confirming"
                  :project="observation.project"
                  :chain="confirming === 'elevate' && needsConfirmChain(observation)"
                  :busy="elevationBusy"
                  @confirm="confirmHistoryElevation"
                  @cancel="confirming = null"
                />
                <div v-else>
                  <button
                    v-if="!observation.can_instruct"
                    :disabled="elevationBusy"
                    class="rounded-lg border border-violet-300 px-4 py-1.5 text-sm font-medium text-violet-600 transition hover:bg-violet-50 disabled:opacity-50 dark:border-violet-500/40 dark:text-violet-300 dark:hover:bg-violet-500/10"
                    title="rides the operating notes of every future session when session-start injection is on — asks to confirm"
                    @click="confirming = 'elevate'"
                  >
                    Elevate ↑
                  </button>
                  <button
                    v-else
                    :disabled="elevationBusy"
                    class="rounded-lg border border-amber-300 px-4 py-1.5 text-sm font-medium text-amber-600 transition hover:bg-amber-50 disabled:opacity-50 dark:border-amber-500/40 dark:text-amber-400 dark:hover:bg-amber-500/10"
                    title="take it out of the operating notes — asks to confirm"
                    @click="confirming = 'demote'"
                  >
                    Demote ↓
                  </button>
                </div>
              </template>
              <p v-if="mode === 'review'" class="text-xs text-zinc-400 dark:text-zinc-500">
                Press <kbd>u</kbd> again for earlier reviews · <kbd>j</kbd>/<kbd>k</kbd> to return to the queue
              </p>
              <p
                v-else-if="observation.review_state === 'proposed'"
                class="text-xs text-zinc-400 dark:text-zinc-500"
              >
                Proposed, unreviewed — verdicts live in the Review queue
              </p>
            </div>
          </div>
        </div>
      </Transition>
    </template>

    <div v-else class="flex flex-1 items-center justify-center p-8 text-center">
      <div class="text-sm text-zinc-400 dark:text-zinc-500">
        <p class="mb-1 text-2xl">✦</p>
        <p v-if="mode === 'review'">
          Select an observation<br />or navigate with <kbd>j</kbd> / <kbd>k</kbd>
        </p>
        <p v-else>Select an observation from the timeline</p>
      </div>
    </div>
  </aside>
</template>
