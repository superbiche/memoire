-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

BEGIN;

-- memoire:D165 round-3 HIGH-1: the run INPUT MANIFEST — the exact event
-- ids a derivation run's fetch returned, recorded when the run row is
-- claimed (fresh mint, retry claim, or completed-dedup upgrade with the
-- identical input_hash). Coverage becomes exact SET MEMBERSHIP, immune to
-- id-allocation-vs-commit ordering (a PG identity id is allocated before
-- its transaction commits, so a MAX(id) watermark cannot prove snapshot
-- membership — round-3 finding). These rows are run-scoped FACTS like
-- input_hash, not a coverage cache (Q2 ruling holds: outstanding-work is
-- computed by joining events x runs x manifests on every call). Runs
-- without manifest rows (pre-0015) fall back to the conservative
-- received-before-created heuristic.
CREATE TABLE IF NOT EXISTS run_input_events (
  run_id    TEXT NOT NULL,
  event_id  INTEGER NOT NULL,
  PRIMARY KEY (run_id, event_id)
);
CREATE INDEX IF NOT EXISTS idx_run_input_events_event ON run_input_events(event_id);

INSERT OR IGNORE INTO schema_version(version, applied_at, note)
VALUES (15, datetime('now'), 'run input manifest (R-10.1, memoire:D165)');

COMMIT;
