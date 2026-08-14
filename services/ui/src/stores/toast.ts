// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

import { reactive } from 'vue'

export interface Toast {
  id: number
  tone: 'success' | 'danger' | 'error' | 'info'
  message: string
}

let nextId = 1

export const toasts = reactive<Toast[]>([])

export function pushToast(tone: Toast['tone'], message: string, ttlMs = 2600): void {
  const id = nextId++
  toasts.push({ id, tone, message })
  setTimeout(() => {
    const i = toasts.findIndex((t) => t.id === id)
    if (i >= 0) toasts.splice(i, 1)
  }, ttlMs)
}
