<!-- SPDX-License-Identifier: AGPL-3.0-only
     Copyright (C) 2026 Michel Tomas -->
<script setup lang="ts">
import { computed } from 'vue'
import { kindLabel, kindStyle } from '../lib/kinds'

const props = defineProps<{ kind: string; proposed?: boolean }>()
const style = computed(() => kindStyle(props.kind))
</script>

<template>
  <span
    class="inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-[11px] font-medium ring-1 ring-inset"
    :class="style.badge"
  >
    <span class="size-1.5 rounded-full" :class="style.dot" />
    {{ kindLabel(kind) }}<span v-if="proposed" class="opacity-70" title="unreviewed (proposed or lapsed)">~</span>
  </span>
</template>
