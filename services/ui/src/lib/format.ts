// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

export function fmtDate(epoch: number): string {
  if (!epoch) return '—'
  const d = new Date(epoch * 1000)
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

export function fmtDateTime(epoch: number): string {
  if (!epoch) return '—'
  const d = new Date(epoch * 1000)
  return d.toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}

export function fmtRelative(epoch: number): string {
  if (!epoch) return '—'
  const delta = Math.round(Date.now() / 1000 - epoch)
  if (delta < 60) return 'just now'
  if (delta < 3600) return `${Math.floor(delta / 60)}m ago`
  if (delta < 86400) return `${Math.floor(delta / 3600)}h ago`
  if (delta < 86400 * 30) return `${Math.floor(delta / 86400)}d ago`
  return fmtDate(epoch)
}

// Legacy and rules-derived rows can lack these fields — render placeholders,
// never crash the panel.
export function shortHash(hash: string | null | undefined): string {
  if (!hash) return '—'
  return hash.replace(/^sha256:/, '').slice(0, 12)
}

export function pct(confidence: number | null | undefined): string {
  if (confidence == null) return '—'
  return `${Math.round(confidence * 100)}%`
}
