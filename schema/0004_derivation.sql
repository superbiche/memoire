-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

BEGIN;

CREATE TABLE IF NOT EXISTS derivation_runs (
  id                  TEXT PRIMARY KEY,
  project             TEXT NOT NULL,
  session_id          TEXT,
  provider            TEXT NOT NULL CHECK (provider IN ('rules', 'local-qwen', 'openai', 'manual')),
  data_boundary       TEXT NOT NULL CHECK (data_boundary IN ('local', 'external')),
  derivation_version  TEXT NOT NULL,
  prompt_version      TEXT,
  input_hash          TEXT NOT NULL,
  window_start_epoch  INTEGER NOT NULL,
  window_end_epoch    INTEGER NOT NULL,
  status              TEXT NOT NULL,
  created_at_epoch    INTEGER NOT NULL,
  completed_at_epoch  INTEGER,
  meta_json           TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_derivation_runs_idempotency
  ON derivation_runs(
    provider,
    derivation_version,
    project,
    COALESCE(session_id, ''),
    window_start_epoch,
    window_end_epoch,
    input_hash
  );
CREATE INDEX IF NOT EXISTS idx_derivation_runs_project_window
  ON derivation_runs(project, window_end_epoch DESC, window_start_epoch DESC);
CREATE INDEX IF NOT EXISTS idx_derivation_runs_status_created
  ON derivation_runs(status, created_at_epoch DESC);

ALTER TABLE observations ADD COLUMN review_state TEXT NOT NULL DEFAULT 'accepted';
ALTER TABLE observations ADD COLUMN derivation_run_id TEXT;
ALTER TABLE observations ADD COLUMN content_hash TEXT;

CREATE INDEX IF NOT EXISTS idx_observations_review_state
  ON observations(review_state);
CREATE INDEX IF NOT EXISTS idx_observations_derivation_run
  ON observations(derivation_run_id)
  WHERE derivation_run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_observations_project_kind_content_hash
  ON observations(project, kind, content_hash)
  WHERE content_hash IS NOT NULL;

CREATE TABLE IF NOT EXISTS observation_source_events (
  observation_id  INTEGER NOT NULL,
  event_id        INTEGER NOT NULL,
  created_at_epoch INTEGER NOT NULL,
  PRIMARY KEY (observation_id, event_id)
);
CREATE INDEX IF NOT EXISTS idx_observation_source_events_event
  ON observation_source_events(event_id, observation_id);

INSERT OR IGNORE INTO schema_version(version, applied_at, note)
VALUES (4, datetime('now'), 'derivation runs and observation review metadata');

COMMIT;
