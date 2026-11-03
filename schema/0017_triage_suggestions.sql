-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

BEGIN;

-- Triage lane v1 (R-C): the judge's suggestions are METADATA, never state.
-- They are append-only and carry no authority: nothing in this table
-- writes review_state, provenance, or can_instruct — those stay behind the
-- human verdict routes and their D208 compare-and-set guards. A suggestion
-- is a proposal a human either applies or flips, and the flip is the
-- measurement (override rate) rather than a correction to be stored back.
--
-- Idempotency is (observation_id, prompt_version): a re-suggestion pass at
-- the same prompt version is a no-op, so re-running the sweep is cheap and
-- safe. The stage rides IN the prompt version string
-- (triage-stage1-v1 / triage-stage2-v1) so that key stays literally the
-- ruled pair while both stages coexist; `stage` is the queryable mirror.
--
-- No outcome column: whether a suggestion was applied or flipped is
-- COMPUTED from observation_review_log at read time (memoire:D245's
-- computed-from-source doctrine — a stored outcome is a cache that can lie
-- the moment a verdict lands outside this table's knowledge).
CREATE TABLE IF NOT EXISTS observation_triage_suggestions (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  observation_id     INTEGER NOT NULL,
  stage              INTEGER NOT NULL,
  prompt_version     TEXT NOT NULL,
  model              TEXT NOT NULL,
  actor              TEXT NOT NULL,
  suggested_verdict  TEXT NOT NULL,
  reason_genre       TEXT,
  reason_text        TEXT,
  arm                TEXT,
  created_at_epoch   INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_triage_suggestion_idempotency
  ON observation_triage_suggestions(observation_id, prompt_version);

CREATE INDEX IF NOT EXISTS idx_triage_suggestion_stage_verdict
  ON observation_triage_suggestions(stage, suggested_verdict, id);

CREATE INDEX IF NOT EXISTS idx_triage_suggestion_genre
  ON observation_triage_suggestions(reason_genre, id)
  WHERE reason_genre IS NOT NULL;

-- Feed actor-gate (R-D) read support: the feed predicate is an anti-join
-- over accept transitions rendered by an `agent:*` actor. The partial
-- index keeps that probe off the full log — accept rows are a small
-- minority of a log dominated by TTL lapses.
CREATE INDEX IF NOT EXISTS idx_observation_review_log_accepts
  ON observation_review_log(observation_id, actor)
  WHERE to_state = 'accepted';

INSERT OR IGNORE INTO schema_version(version, applied_at, note)
VALUES (17, datetime('now'), 'triage suggestions (metadata, append-only) + feed accept-actor index');

COMMIT;
