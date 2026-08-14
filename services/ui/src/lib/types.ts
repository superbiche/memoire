// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

export interface Observation {
  id: number
  project: string
  session_id: string
  kind: string
  text: string
  occurred_at_epoch: number
  created_at_epoch: number
  principal_id: string
  confidence: number
  review_state: string
  derivation_run_id: string
  content_hash: string
  source_event_ids: number[] | null
  provenance: string
  can_instruct: number
  can_evidence: number
  needs_confirmation: number
  noise_candidate: number
}

export interface TimelineHit {
  id: number
  session_id: string
  project: string
  kind: string
  review_state: string
  snippet: string
  captured_at_epoch: number
  noise_candidate: number
  can_instruct: number
}

export interface Stats {
  event_count: number
  observation_count: number
  deleted_event_count: number
  deleted_observation_count: number
  last_write_epoch: number
  db_bytes: number
  wal_bytes: number
}

export interface SessionSummary {
  session_id: string
  project: string
  client: string
  started_at_epoch: number
  ended_at_epoch?: number
  observation_count: number
  proposed_count: number
  lapsed_count: number
  last_observed_at_epoch?: number
}

// 'confirmed' is accept + provenance promotion in one gesture (R-5's
// stronger verdict, keyed `c` in the queue since memoire:D192).
export type Verdict = 'accepted' | 'rejected' | 'confirmed'

export interface ReviewedEntry {
  observation: Observation
  verdict: Verdict
  reason?: string
  at: number
}
