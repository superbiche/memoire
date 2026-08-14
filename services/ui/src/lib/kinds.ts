// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Sealed observation kinds (docs/GOVERNANCE-DESIGN.md) mapped to a fixed
// visual identity. Full literal class strings so Tailwind's scanner sees them.
export interface KindStyle {
  badge: string
  accent: string
  dot: string
}

const STYLES: Record<string, KindStyle> = {
  fact: {
    badge:
      'bg-sky-100 text-sky-700 ring-sky-600/20 dark:bg-sky-500/10 dark:text-sky-300 dark:ring-sky-400/25',
    accent: 'bg-sky-500',
    dot: 'bg-sky-500',
  },
  decision: {
    badge:
      'bg-violet-100 text-violet-700 ring-violet-600/20 dark:bg-violet-500/10 dark:text-violet-300 dark:ring-violet-400/25',
    accent: 'bg-violet-500',
    dot: 'bg-violet-500',
  },
  preference: {
    badge:
      'bg-pink-100 text-pink-700 ring-pink-600/20 dark:bg-pink-500/10 dark:text-pink-300 dark:ring-pink-400/25',
    accent: 'bg-pink-500',
    dot: 'bg-pink-500',
  },
  constraint: {
    badge:
      'bg-amber-100 text-amber-800 ring-amber-600/20 dark:bg-amber-500/10 dark:text-amber-300 dark:ring-amber-400/25',
    accent: 'bg-amber-500',
    dot: 'bg-amber-500',
  },
  gotcha: {
    badge:
      'bg-rose-100 text-rose-700 ring-rose-600/20 dark:bg-rose-500/10 dark:text-rose-300 dark:ring-rose-400/25',
    accent: 'bg-rose-500',
    dot: 'bg-rose-500',
  },
  open_question: {
    badge:
      'bg-cyan-100 text-cyan-700 ring-cyan-600/20 dark:bg-cyan-500/10 dark:text-cyan-300 dark:ring-cyan-400/25',
    accent: 'bg-cyan-500',
    dot: 'bg-cyan-500',
  },
  task: {
    badge:
      'bg-emerald-100 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-500/10 dark:text-emerald-300 dark:ring-emerald-400/25',
    accent: 'bg-emerald-500',
    dot: 'bg-emerald-500',
  },
}

const FALLBACK: KindStyle = {
  badge:
    'bg-zinc-100 text-zinc-600 ring-zinc-500/20 dark:bg-zinc-500/10 dark:text-zinc-300 dark:ring-zinc-400/25',
  accent: 'bg-zinc-400',
  dot: 'bg-zinc-400',
}

export const SEALED_KINDS = Object.keys(STYLES)

export function kindStyle(kind: string): KindStyle {
  return STYLES[kind] ?? FALLBACK
}

export function kindLabel(kind: string): string {
  return kind.replace(/_/g, ' ')
}
