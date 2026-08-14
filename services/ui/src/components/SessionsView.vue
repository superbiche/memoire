<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
// Slice E (memoire:D146, E3): one page = one session's story, read
// SessionStart -> SessionEnd; verdicts per row; lapse-all for the rest.
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { fmtDateTime, fmtRelative, pct } from '../lib/format'
import type { Observation } from '../lib/types'
import { can, type VerdictAction } from '../lib/verdicts'
import KindBadge from './KindBadge.vue'
import VerdictActions from './VerdictActions.vue'
import {
  acceptRow,
  confirmRow,
  elevateRow,
  hiddenProposedNoise,
  lapseRemaining,
  lapseRow,
  loadSessions,
  openSession,
  projects,
  rejectRow,
  remainingProposed,
  rewriteRow,
  sessionsState as s,
  storyNoiseCount,
  visibleStory,
} from '../stores/sessions'

const rewriteOpen = ref<number | null>(null)
const rewriteText = ref('')
const rewriteReason = ref('')

function startRewrite(o: Observation) {
  rewriteOpen.value = o.id
  rewriteText.value = o.text
  rewriteReason.value = ''
}

async function submitRewrite(id: number) {
  // Rewrite is a row mutation like any verdict (round-4 MAJOR 2): it takes
  // the row's verdictPending slot so it excludes — and is excluded by —
  // in-flight verdicts and elevation on the same row.
  if (s.verdictPending[id] || s.elevationPending[id]) return
  s.verdictPending[id] = true
  try {
    if (await rewriteRow(id, rewriteText.value, rewriteReason.value || undefined)) {
      rewriteOpen.value = null
    }
  } finally {
    delete s.verdictPending[id]
  }
}

function stateTone(state: string): string {
  switch (state) {
    case 'proposed':
      return 'text-amber-600 dark:text-amber-400'
    case 'lapsed':
      return 'text-zinc-400 dark:text-zinc-500'
    case 'accepted':
      return 'text-emerald-600 dark:text-emerald-400'
    case 'rejected':
      return 'text-rose-600 dark:text-rose-400'
    case 'superseded':
      return 'text-zinc-400 line-through dark:text-zinc-500'
    default:
      return 'text-zinc-500'
  }
}

// Verdict availability lives in lib/verdicts (harmony pass 2026-07-31) —
// one truth shared with the Review queue's panel.

function pickProject(p: string) {
  s.project = s.project === p ? '' : p
  void loadSessions()
}

// --- Keyboard triage (operator ask, 2026-07-30): same muscle memory as the
// Review queue — j/k rows, a/c/r verdicts — plus the surface's own verbs
// (l lapse, e elevate) and n/p session hopping. This component mounts only
// while its tab is active (App.vue v-if), so a window listener is safe.
const storyEl = ref<HTMLElement | null>(null)

// Cursor movement walks the VISIBLE projection (noise hidden by default) —
// j/k must never land on a row the page does not render.
function cursorIndex(): number {
  return s.cursorId == null ? -1 : visibleStory.value.findIndex((o) => o.id === s.cursorId)
}

function moveCursor(delta: number) {
  const rows = visibleStory.value
  if (!rows.length) return
  const i = cursorIndex()
  const next = i < 0 ? (delta > 0 ? 0 : rows.length - 1) : Math.min(Math.max(i + delta, 0), rows.length - 1)
  s.cursorId = rows[next].id
}

async function moveSession(delta: number) {
  if (!s.sessions.length) return
  const i = s.sessions.findIndex((x) => x.session_id === s.selected)
  const next = i < 0 ? 0 : Math.min(Math.max(i + delta, 0), s.sessions.length - 1)
  if (s.sessions[next].session_id !== s.selected) await openSession(s.sessions[next].session_id)
}

// SHARED verdict dispatch for keys AND the row strip's buttons (round-1
// harmony review MAJOR 2: clicks that bypassed this left verdictPending
// unset, so a double-click double-POSTed — confirm-on-accepted is legal
// server-side and would write a second review-log row). Advances the
// cursor only on SUCCESS, and only when the completion still belongs to
// where it started: same session, cursor still on the initiating row. A
// delayed success must never advance past an unrelated row or reach into a
// newly opened session (review round 2).
async function verdictKey(
  row: Observation,
  verdict: VerdictAction,
  fn: (id: number) => Promise<Observation | null>,
) {
  // Mutually exclusive with an in-flight elevation chain on the same row
  // (round-3 MAJOR 3: a concurrent accept's late response repainted an
  // elevated row backward).
  if (!can(row, verdict) || s.verdictPending[row.id] || s.elevationPending[row.id]) return
  s.verdictPending[row.id] = true
  const sessionAtDispatch = s.selected
  try {
    const ok = await fn(row.id)
    if (ok && s.selected === sessionAtDispatch && s.cursorId === row.id) moveCursor(1)
  } finally {
    delete s.verdictPending[row.id]
  }
}

function onKeydown(e: KeyboardEvent) {
  const target = e.target as HTMLElement
  if (
    e.metaKey ||
    e.ctrlKey ||
    e.altKey ||
    e.shiftKey ||
    target.tagName === 'INPUT' ||
    target.tagName === 'TEXTAREA' ||
    target.isContentEditable
  )
    return
  // One physical gesture per mutation (round-1 harmony review MAJOR 3):
  // auto-repeat on a held verdict key must not walk the story mutating row
  // after row. Navigation keys keep repeating.
  if (e.repeat && ['a', 'c', 'r', 'l', 'e', 'w'].includes(e.key)) return
  const i = cursorIndex()
  const row = i >= 0 ? visibleStory.value[i] : undefined
  switch (e.key) {
    case 'j':
    case 'ArrowDown':
      e.preventDefault()
      moveCursor(1)
      break
    case 'k':
    case 'ArrowUp':
      e.preventDefault()
      moveCursor(-1)
      break
    case 'n':
      e.preventDefault()
      void moveSession(1)
      break
    case 'p':
      e.preventDefault()
      void moveSession(-1)
      break
    case 'a':
      if (row) {
        e.preventDefault()
        void verdictKey(row, 'accept', acceptRow)
      }
      break
    case 'c':
      if (row) {
        e.preventDefault()
        void verdictKey(row, 'confirm', confirmRow)
      }
      break
    case 'r':
      if (row) {
        e.preventDefault()
        void verdictKey(row, 'reject', rejectRow)
      }
      break
    case 'l':
      if (row) {
        e.preventDefault()
        void verdictKey(row, 'lapse', lapseRow)
      }
      break
    case 'e':
      // No cursor advance: elevation deserves a look at what it did.
      // Mutually exclusive with in-flight verdicts on the row (round-3
      // MAJOR 3).
      if (row && can(row, 'elevate') && !s.elevationPending[row.id] && !s.verdictPending[row.id]) {
        e.preventDefault()
        void elevateRow(row.id)
      }
      break
    case 'w':
      // Rewrite is w, not r — it is not a verdict (opens the editor). The
      // editor must not open over an in-flight mutation of the same row
      // (round-4 MAJOR 2 — the strip's button is disabled then; the key
      // matches it).
      if (row && can(row, 'rewrite') && !s.verdictPending[row.id] && !s.elevationPending[row.id]) {
        e.preventDefault()
        startRewrite(row)
      }
      break
    case 'Escape':
      if (rewriteOpen.value != null) rewriteOpen.value = null
      else s.cursorId = null
      break
  }
}

watch(
  () => s.cursorId,
  (id) => {
    if (id == null) return
    void nextTick(() => {
      storyEl.value
        ?.querySelector(`[data-obs-id="${id}"]`)
        ?.scrollIntoView({ block: 'nearest', behavior: 'smooth' })
    })
  },
)

onMounted(() => {
  if (!s.loaded) void loadSessions()
  window.addEventListener('keydown', onKeydown)
})
onBeforeUnmount(() => window.removeEventListener('keydown', onKeydown))
</script>

<template>
  <div class="mx-auto flex h-full max-w-7xl gap-4 px-4 py-4 sm:px-6">
    <!-- Session list (catchup navigation) -->
    <aside class="flex w-80 shrink-0 flex-col gap-3 overflow-hidden">
      <div class="flex flex-wrap items-center gap-1.5">
        <button
          v-for="p in projects"
          :key="p"
          class="rounded-full px-2 py-0.5 font-mono text-[11px] ring-1 ring-inset transition"
          :class="
            s.project === p
              ? 'bg-violet-600 text-white ring-violet-600'
              : 'text-zinc-500 ring-zinc-300 hover:text-zinc-700 dark:ring-zinc-700 dark:hover:text-zinc-300'
          "
          @click="pickProject(p)"
        >
          {{ p }}
        </button>
        <label class="ml-auto flex items-center gap-1.5 text-[11px] text-zinc-500">
          <input v-model="s.withWorkOnly" type="checkbox" @change="loadSessions()" />
          with work only
        </label>
      </div>

      <div class="min-h-0 flex-1 space-y-2 overflow-y-auto pr-1">
        <p v-if="s.loading" class="py-8 text-center text-sm text-zinc-400">loading…</p>
        <p v-else-if="!s.sessions.length" class="py-8 text-center text-sm text-zinc-400">
          no sessions{{ s.withWorkOnly ? ' with review work' : '' }}
        </p>
        <button
          v-for="sess in s.sessions"
          :key="sess.session_id"
          class="w-full rounded-lg border p-3 text-left transition"
          :class="
            s.selected === sess.session_id
              ? 'border-violet-400 bg-violet-50/60 dark:border-violet-600 dark:bg-violet-500/10'
              : 'border-zinc-200 bg-white hover:border-zinc-300 dark:border-zinc-800 dark:bg-zinc-900 dark:hover:border-zinc-700'
          "
          @click="openSession(sess.session_id)"
        >
          <div class="flex items-center gap-2">
            <span class="rounded bg-zinc-100 px-1.5 py-0.5 font-mono text-[10px] text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300">{{ sess.project }}</span>
            <span class="text-[11px] text-zinc-400">{{ fmtRelative(sess.started_at_epoch) }}</span>
            <span
              v-if="sess.proposed_count"
              class="ml-auto rounded-full bg-amber-500/90 px-1.5 text-[10px] font-semibold text-white"
              >{{ sess.proposed_count }}</span
            >
          </div>
          <div class="mt-1 truncate font-mono text-[11px] text-zinc-500">{{ sess.session_id }}</div>
          <div class="mt-1 text-[11px] text-zinc-400">
            {{ sess.observation_count }} observations · {{ sess.lapsed_count }} lapsed · {{ sess.client }}
          </div>
        </button>
      </div>
    </aside>

    <!-- Session story -->
    <section class="flex min-w-0 flex-1 flex-col overflow-hidden rounded-xl border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900">
      <div v-if="!s.selected" class="flex flex-1 items-center justify-center text-sm text-zinc-400">
        pick a session — skim its story, keep the gems, lapse the rest
      </div>
      <template v-else>
        <div class="flex items-center gap-3 border-b border-zinc-200 px-4 py-2.5 dark:border-zinc-800">
          <span class="truncate font-mono text-xs text-zinc-500">{{ s.selected }}</span>
          <label
            v-if="storyNoiseCount"
            class="ml-auto flex shrink-0 items-center gap-1.5 text-[11px] text-zinc-500"
            title="the story always fetches noise-flagged rows; this only toggles their visibility"
          >
            <input v-model="s.showNoise" type="checkbox" />
            noise ({{ storyNoiseCount }})
          </label>
          <span v-if="s.storyTruncated" :class="storyNoiseCount ? '' : 'ml-auto'" class="shrink-0 text-[11px] text-amber-600 dark:text-amber-400" title="the story exceeded the page-loop cap; bulk lapse is disabled">
            story truncated — bulk lapse disabled
          </span>
          <button
            v-else-if="remainingProposed"
            :class="storyNoiseCount ? '' : 'ml-auto'"
            class="shrink-0 rounded-md bg-zinc-100 px-2.5 py-1 text-xs font-medium text-zinc-600 transition hover:bg-zinc-200 dark:bg-zinc-800 dark:text-zinc-300 dark:hover:bg-zinc-700"
            :title="hiddenProposedNoise ? `includes ${hiddenProposedNoise} hidden noise row${hiddenProposedNoise === 1 ? '' : 's'}` : undefined"
            @click="lapseRemaining()"
          >
            lapse remaining ({{ remainingProposed }})
          </button>
        </div>

        <div ref="storyEl" class="min-h-0 flex-1 space-y-3 overflow-y-auto p-4">
          <p v-if="s.storyLoading" class="py-8 text-center text-sm text-zinc-400">loading…</p>
          <p v-else-if="!s.story.length" class="py-8 text-center text-sm text-zinc-400">
            no semantic observations in this session
          </p>
          <p v-else-if="!visibleStory.length" class="py-8 text-center text-sm text-zinc-400">
            all {{ storyNoiseCount }} observations here are noise-flagged — toggle noise to see them
          </p>
          <article
            v-for="o in visibleStory"
            :key="o.id"
            :data-obs-id="o.id"
            class="rounded-lg border p-3 transition"
            :class="
              s.cursorId === o.id
                ? 'border-violet-400 ring-1 ring-violet-400/60 dark:border-violet-600 dark:ring-violet-500/40'
                : 'border-zinc-200 dark:border-zinc-800'
            "
            @click="s.cursorId = o.id"
          >
            <div class="flex items-center gap-2">
              <KindBadge :kind="o.kind" :proposed="o.review_state === 'proposed' || o.review_state === 'lapsed'" />
              <span class="text-[11px]" :class="stateTone(o.review_state)">{{ o.review_state }}</span>
              <span v-if="o.provenance" class="text-[11px] text-zinc-400">{{ o.provenance }}</span>
              <span v-if="o.noise_candidate" class="rounded bg-zinc-100 px-1.5 text-[10px] font-medium tracking-wide text-zinc-400 uppercase dark:bg-zinc-800">noise</span>
              <span v-if="o.can_instruct === 1" class="rounded bg-violet-100 px-1.5 text-[10px] font-medium text-violet-700 dark:bg-violet-500/15 dark:text-violet-300">elevated</span>
              <span class="ml-auto text-[11px] text-zinc-400" :title="fmtDateTime(o.occurred_at_epoch)">
                #{{ o.id }} · {{ pct(o.confidence) }}
              </span>
            </div>

            <p class="mt-2 text-sm leading-relaxed" :class="o.review_state === 'superseded' ? 'text-zinc-400 line-through' : ''">
              {{ o.text }}
            </p>

            <div v-if="rewriteOpen === o.id" class="mt-2 space-y-2">
              <textarea
                v-model="rewriteText"
                rows="3"
                class="w-full rounded-md border border-zinc-300 bg-white p-2 text-sm dark:border-zinc-700 dark:bg-zinc-950"
                @keydown.esc.prevent="rewriteOpen = null"
              />
              <div class="flex items-center gap-2">
                <input
                  v-model="rewriteReason"
                  placeholder="reason (optional)"
                  class="flex-1 rounded-md border border-zinc-300 bg-white px-2 py-1 text-xs dark:border-zinc-700 dark:bg-zinc-950"
                  @keydown.esc.prevent="rewriteOpen = null"
                />
                <button class="rounded-md bg-violet-600 px-2.5 py-1 text-xs font-medium text-white" @click="submitRewrite(o.id)">rewrite</button>
                <button class="rounded-md px-2 py-1 text-xs text-zinc-500" @click="rewriteOpen = null">cancel</button>
              </div>
            </div>

            <!-- Buttons dispatch through verdictKey, the same guarded path
                 as the keyboard: verdictPending set for the call's lifetime
                 (double-click protection) and success-gated cursor advance. -->
            <VerdictActions
              v-else
              class="mt-2"
              :observation="o"
              size="row"
              :pending="!!s.verdictPending[o.id]"
              :elevation-pending="!!s.elevationPending[o.id]"
              @accept="verdictKey(o, 'accept', acceptRow)"
              @confirm="verdictKey(o, 'confirm', confirmRow)"
              @reject="verdictKey(o, 'reject', rejectRow)"
              @lapse="verdictKey(o, 'lapse', lapseRow)"
              @rewrite="startRewrite(o)"
              @elevate="elevateRow(o.id)"
            />
          </article>
        </div>
      </template>
    </section>
  </div>
</template>
