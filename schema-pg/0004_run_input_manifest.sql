-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

-- schema-pg/0004_run_input_manifest.sql — PostgreSQL twin of
-- schema/0015_run_input_manifest.sql (memoire:D165 round-3 HIGH-1). See
-- that file for the design comment.
BEGIN;

CREATE TABLE IF NOT EXISTS run_input_events (
  run_id    TEXT NOT NULL,
  event_id  BIGINT NOT NULL,
  PRIMARY KEY (run_id, event_id)
);
CREATE INDEX IF NOT EXISTS idx_run_input_events_event ON run_input_events(event_id);

INSERT INTO schema_version(version, applied_at, note)
VALUES (4, now()::text, 'run input manifest (R-10.1, memoire:D165)')
ON CONFLICT (version) DO NOTHING;

COMMIT;
