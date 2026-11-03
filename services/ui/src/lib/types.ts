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
  // Attribution at Stop aggregate; absent when no memory_usage event names
  // the row — render as "no signal", never as zero use.
  usage?: UsageSummary | null
}

export interface UsageSummary {
  used: number
  unused: number
  misapplied: number
  contradicted_by_files: number
  fetch_failed: number
  sessions: number
  last_at_epoch: number
  last_used_at_epoch?: number
}

export interface UsageLine {
  event_id: number
  session_id: string
  project: string
  verdict: string
  reason?: string
  occurred_at_epoch: number
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

// --- Triage lane v1 (R-C) ---

// A judge SUGGESTION. Metadata, never state: it moves nothing on the row
// and carries no authority — the operator applies or flips it, and the flip
// is the measurement.
export interface TriageSuggestion {
  id: number
  observation_id: number
  stage: number
  prompt_version: string
  model: string
  actor: string
  suggested_verdict: string
  reason_genre?: string
  reason_text?: string
  arm?: string
  // The judge's confidence in ITS OWN verdict, in [0,1]. Optional because a
  // suggestion written before the stage-1 v2 prompt made no confidence
  // claim, and an absent claim is not a low one: the de-noise doubt block
  // reads only present-and-low values, so a v1-era backlog is not pinned
  // into it wholesale.
  confidence?: number
  created_at_epoch: number
}

export interface TriageRow {
  observation: Observation
  suggestions: TriageSuggestion[] | null
  raw_events?: string[] | null
}

// The triage surface computes its OWN counters: lapsed rows are excluded
// from the queue counters by law (memoire:D146 R-6), so reusing those would
// under-report exactly the TTL pool this lane exists to work through.
// `proposed`, `ttl_lapsed` and `total` are MAIN-pool counts and exclude the
// de-noise members; `de_noise` is the fourth pool's own size. The split is
// what the counts strip shows: identified noise is a separate cheap bulk
// gesture, not more rows inside the triage backlog.
export interface TriageCounts {
  proposed: number
  ttl_lapsed: number
  total: number
  de_noise: number
  noise_flagged: number
  suggested: number
}

export interface TriageGenreCandidate {
  genre: string
  occurrences: number
  flip_set: number[] | null
  judged: number
}

export interface TriageOperatorGenre {
  genre: string
  count: number
  judge_knows: boolean
}

// What the operator has staked on a row before the ONE bulk apply commits
// it. Nothing here has touched the server yet.
//
// `keep` is the DE-NOISE pool's override. That pool inverts the gesture:
// every loaded row arrives condemned as noise, and the operator's work is
// rescuing the ones that are not. A keep commits the `not-noise` review
// action — the row leaves the de-noise pool for good and returns to the
// ordinary queue with its accept-or-lapse question still open.
export type TriageStake = 'accept' | 'lapse' | 'noise' | 'reject' | 'keep'

// Stage-2 stakes over the human-validated set. `decline` is a real stake,
// not the absence of one: without it a suggestion the operator read and
// turned down could never leave "pending", and a verdict class that can
// never resolve is a metric nobody can read.
export type TriageStage2Stake = 'confirm' | 'elevate' | 'decline'

export interface TriageOverrideRate {
  prompt_version: string
  stage: number
  verdict: string
  arm?: string
  suggested: number
  applied: number
  flipped: number
  pending: number
}
