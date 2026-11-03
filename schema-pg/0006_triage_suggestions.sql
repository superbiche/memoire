-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

-- schema-pg/0006_triage_suggestions.sql — PostgreSQL twin of
-- schema/0017_triage_suggestions.sql (triage lane v1, R-C/R-D). See that
-- file for the design comment; column types and the identity column are
-- the only translation.
BEGIN;

CREATE TABLE IF NOT EXISTS observation_triage_suggestions (
  id                 BIGSERIAL PRIMARY KEY,
  observation_id     BIGINT NOT NULL,
  stage              BIGINT NOT NULL,
  prompt_version     TEXT NOT NULL,
  model              TEXT NOT NULL,
  actor              TEXT NOT NULL,
  suggested_verdict  TEXT NOT NULL,
  reason_genre       TEXT,
  reason_text        TEXT,
  arm                TEXT,
  created_at_epoch   BIGINT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_triage_suggestion_idempotency
  ON observation_triage_suggestions(observation_id, prompt_version);

CREATE INDEX IF NOT EXISTS idx_triage_suggestion_stage_verdict
  ON observation_triage_suggestions(stage, suggested_verdict, id);

CREATE INDEX IF NOT EXISTS idx_triage_suggestion_genre
  ON observation_triage_suggestions(reason_genre, id)
  WHERE reason_genre IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_observation_review_log_accepts
  ON observation_review_log(observation_id, actor)
  WHERE to_state = 'accepted';

INSERT INTO schema_version(version, applied_at, note)
VALUES (6, now()::text, 'triage suggestions (metadata, append-only) + feed accept-actor index')
ON CONFLICT (version) DO NOTHING;

COMMIT;
