-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

BEGIN;

CREATE TABLE IF NOT EXISTS observation_review_log (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  observation_id     INTEGER NOT NULL,
  from_state         TEXT NOT NULL,
  to_state           TEXT NOT NULL,
  actor              TEXT NOT NULL,
  reason_text        TEXT,
  occurred_at_epoch  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_observation_review_log_observation
  ON observation_review_log(observation_id, occurred_at_epoch DESC, id DESC);

INSERT OR IGNORE INTO schema_version(version, applied_at, note)
VALUES (5, datetime('now'), 'append-only observation review transitions');

COMMIT;
