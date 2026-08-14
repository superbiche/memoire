-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

BEGIN;

-- memoire:D165 (R-10.1): derive notify intents. A client SessionEnd marks
-- intent over HTTP instead of running a local derive (R-10 Q1 ruling:
-- sweep-only — an intent NEVER triggers immediate derivation). Rows are
-- SOURCE DATA (client statements with timestamps), not a coverage cache
-- (Q2 ruling: outstanding-work is computed from events x derivation_runs).
-- The timestamps exist to journal notify->completion latency (Q1's
-- mandatory instrumentation); current-state only — reopening a completed
-- intent resets it, history lives in the derive log + derivation_runs.
CREATE TABLE IF NOT EXISTS derive_intents (
  project               TEXT NOT NULL,
  session_id            TEXT NOT NULL,
  first_notified_epoch  INTEGER NOT NULL,
  last_notified_epoch   INTEGER NOT NULL,
  notify_count          INTEGER NOT NULL DEFAULT 1,
  completed_at_epoch    INTEGER,
  completed_run_id      TEXT,
  latency_seconds       INTEGER,
  PRIMARY KEY (project, session_id)
);

INSERT OR IGNORE INTO schema_version(version, applied_at, note)
VALUES (13, datetime('now'), 'derive notify intents (R-10.1, memoire:D165)');

COMMIT;
