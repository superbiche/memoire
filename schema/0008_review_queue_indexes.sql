-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

BEGIN;

-- Review-queue read path: the UI pages proposed observations ordered by
-- (occurred_at_epoch, id) in two windows keyed on include_noise. Without a
-- matching index the planner falls back to idx_observations_noise_candidate
-- (matches nearly every row) plus a temp sort, which costs seconds at 250k+
-- observations and head-of-line-blocks every verdict write behind the single
-- store connection.
--
-- The indexes are partial on review_state = 'proposed' (not a leading
-- column) so they can never be selected for accepted-state or default
-- reads, which have more selective indexes of their own
-- (idx_obs_project_occurred, idx_observations_derivation_run); a broader
-- (review_state, ...) shape measurably stole those plans. The partial
-- predicate requires the queue query to inline review_state as a validated
-- literal — see observationWhere in services/api/internal/db/store.go.

-- Window 1: include_noise=false.
CREATE INDEX IF NOT EXISTS idx_obs_review_queue
  ON observations(occurred_at_epoch, id)
  WHERE deleted_at_epoch IS NULL
    AND review_state = 'proposed'
    AND noise_candidate = 0;

-- Window 2: include_noise=true.
CREATE INDEX IF NOT EXISTS idx_obs_review_queue_all
  ON observations(occurred_at_epoch, id)
  WHERE deleted_at_epoch IS NULL
    AND review_state = 'proposed';

INSERT OR IGNORE INTO schema_version(version, applied_at, note)
VALUES (8, datetime('now'), 'proposed-only review queue indexes');

COMMIT;
