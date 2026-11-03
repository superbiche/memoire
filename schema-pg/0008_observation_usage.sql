-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

-- schema-pg/0008_observation_usage.sql — PostgreSQL twin of
-- schema/0019_observation_usage.sql. See that file for the design comment;
-- column types are the only translation.
BEGIN;

CREATE TABLE IF NOT EXISTS observation_usage (
  id                 BIGSERIAL PRIMARY KEY,
  observation_id     BIGINT NOT NULL,
  event_id           BIGINT NOT NULL,
  session_id         TEXT NOT NULL,
  project            TEXT NOT NULL,
  verdict            TEXT NOT NULL CHECK (verdict IN ('used', 'unused', 'misapplied', 'contradicted-by-files', 'fetch-failed')),
  reason             TEXT,
  occurred_at_epoch  BIGINT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_observation_usage_obs_event
  ON observation_usage(observation_id, event_id);
CREATE INDEX IF NOT EXISTS idx_observation_usage_obs_occurred
  ON observation_usage(observation_id, occurred_at_epoch DESC);
CREATE INDEX IF NOT EXISTS idx_observation_usage_event
  ON observation_usage(event_id);

INSERT INTO schema_version(version, applied_at, note)
VALUES (8, now()::text, 'observation usage projection of memory_usage events (additive)')
ON CONFLICT (version) DO NOTHING;

COMMIT;
