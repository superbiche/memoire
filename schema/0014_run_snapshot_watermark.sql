-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

BEGIN;

-- memoire:D165 round-2 HIGH-1: exact input-snapshot watermark. The
-- orchestrator reads MAX(events.id) BEFORE its input fetch, bounds the
-- fetch by it, and records it here — so "this event was in the run's
-- input" is the exact predicate `e.id <= snapshot_max_event_id` (insert
-- ids are monotonic on both engines), immune to queue-time received_at
-- stamping, fetch-to-insert races, and same-second ties. NULL =
-- unwatermarked (rules path, pre-0014 rows); outstanding-work falls back
-- to the conservative received_at heuristic for those.
ALTER TABLE derivation_runs ADD COLUMN snapshot_max_event_id INTEGER;

INSERT OR IGNORE INTO schema_version(version, applied_at, note)
VALUES (14, datetime('now'), 'derivation_runs input-snapshot watermark (R-10.1, memoire:D165)');

COMMIT;
