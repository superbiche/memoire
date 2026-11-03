-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

BEGIN;

-- Attribution at Stop (docs/rework/retrieval-consumer/attribution-at-stop.md):
-- the per-observation usage projection of `memory_usage` events. One row per
-- (observation, event): the verdict the consuming agent gave the served hit
-- at the end of the turn and its reason line. Filled at ingest, idempotent
-- on replay (primary key), never a lifecycle input by itself: the review UI
-- shows it next to the review state, the operator rules.
--
-- ADDITIVE ONLY: a new table and its indexes. `reason` is agent prose about
-- observation content and is enrolled as a redaction target (redactderived). Ids named by the agent that
-- match no observation row are dropped at projection time (the event keeps
-- them). Verdict vocabulary = the measured grammar; widening is a strict
-- superset change under the schema README carve-out.
CREATE TABLE IF NOT EXISTS observation_usage (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  observation_id     INTEGER NOT NULL,
  event_id           INTEGER NOT NULL,
  session_id         TEXT NOT NULL,
  project            TEXT NOT NULL,
  verdict            TEXT NOT NULL CHECK (verdict IN ('used', 'unused', 'misapplied', 'contradicted-by-files', 'fetch-failed')),
  reason             TEXT,
  occurred_at_epoch  INTEGER NOT NULL
);
-- One verdict per (observation, event): a queue replay is idempotent.
CREATE UNIQUE INDEX IF NOT EXISTS idx_observation_usage_obs_event
  ON observation_usage(observation_id, event_id);
CREATE INDEX IF NOT EXISTS idx_observation_usage_obs_occurred
  ON observation_usage(observation_id, occurred_at_epoch DESC);
CREATE INDEX IF NOT EXISTS idx_observation_usage_event
  ON observation_usage(event_id);

INSERT OR IGNORE INTO schema_version(version, applied_at, note)
VALUES (19, datetime('now'), 'observation usage projection of memory_usage events (additive)');

COMMIT;
