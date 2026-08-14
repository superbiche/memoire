-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

BEGIN;

-- memoire:D152/D153: the one-sided future-skew alert on /v1/health counts
-- events whose occurred_at_epoch is AHEAD of received_at_epoch (producer
-- clock fault). Health is a cheap monitoring endpoint and must not scan the
-- events table, so this partial index covers the clock-fault candidates only
-- (normally zero rows). The predicate must textually match the literal WHERE
-- conjunct in CountFutureSkewEvents — bound parameters cannot prove a
-- partial index predicate (0008/0011 lesson).
CREATE INDEX IF NOT EXISTS idx_events_future_skew
ON events(occurred_at_epoch, received_at_epoch)
WHERE occurred_at_epoch > received_at_epoch;

INSERT OR IGNORE INTO schema_version(version, applied_at, note)
VALUES (12, datetime('now'), 'partial index for one-sided future clock-skew count');

COMMIT;
