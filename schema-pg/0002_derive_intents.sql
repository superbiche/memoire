-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

-- schema-pg/0002_derive_intents.sql — PostgreSQL twin of
-- schema/0013_derive_intents.sql (memoire:D165, R-10.1). See that file for
-- the design comment; column types are the only translation.
BEGIN;

CREATE TABLE IF NOT EXISTS derive_intents (
  project               TEXT NOT NULL,
  session_id            TEXT NOT NULL,
  first_notified_epoch  BIGINT NOT NULL,
  last_notified_epoch   BIGINT NOT NULL,
  notify_count          BIGINT NOT NULL DEFAULT 1,
  completed_at_epoch    BIGINT,
  completed_run_id      TEXT,
  latency_seconds       BIGINT,
  PRIMARY KEY (project, session_id)
);

INSERT INTO schema_version(version, applied_at, note)
VALUES (2, now()::text, 'derive notify intents (R-10.1, memoire:D165)')
ON CONFLICT (version) DO NOTHING;

COMMIT;
