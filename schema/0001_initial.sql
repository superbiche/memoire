-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA foreign_keys=OFF;

CREATE TABLE IF NOT EXISTS sessions (
  session_id        TEXT PRIMARY KEY,
  project           TEXT NOT NULL,
  client            TEXT NOT NULL,
  started_at_epoch  INTEGER NOT NULL,
  ended_at_epoch    INTEGER,
  principal_id      TEXT,
  meta_json         TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_project_started
  ON sessions(project, started_at_epoch DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_principal
  ON sessions(principal_id);

CREATE TABLE IF NOT EXISTS events (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
  idempotency_key     TEXT NOT NULL UNIQUE,
  session_id          TEXT NOT NULL,
  content_session_id  TEXT,
  project             TEXT NOT NULL,
  source_adapter      TEXT NOT NULL,
  source_event_id     TEXT,
  event_type          TEXT NOT NULL,
  cwd                 TEXT,
  payload_json        TEXT NOT NULL,
  text                TEXT,
  occurred_at_epoch   INTEGER NOT NULL,
  received_at_epoch   INTEGER NOT NULL,
  deleted_at_epoch    INTEGER,
  principal_id        TEXT,
  credential_id       TEXT,
  request_id          TEXT,
  meta_json           TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_session
  ON events(session_id);
CREATE INDEX IF NOT EXISTS idx_events_project_occurred
  ON events(project, occurred_at_epoch DESC);
CREATE INDEX IF NOT EXISTS idx_events_type_occurred
  ON events(event_type, occurred_at_epoch DESC);
CREATE INDEX IF NOT EXISTS idx_events_source_event
  ON events(source_adapter, source_event_id)
  WHERE source_event_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_events_principal
  ON events(principal_id);
CREATE INDEX IF NOT EXISTS idx_events_deleted
  ON events(deleted_at_epoch)
  WHERE deleted_at_epoch IS NOT NULL;

CREATE TABLE IF NOT EXISTS observations (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id            INTEGER,
  project             TEXT NOT NULL,
  session_id          TEXT,
  kind                TEXT NOT NULL,
  text                TEXT NOT NULL,
  source_adapter      TEXT,
  event_type          TEXT,
  occurred_at_epoch   INTEGER NOT NULL,
  created_at_epoch    INTEGER NOT NULL,
  deleted_at_epoch    INTEGER,
  principal_id        TEXT,
  supersedes_id       INTEGER,
  ttl_review_epoch    INTEGER,
  confidence          REAL,
  meta_json           TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_obs_event_kind
  ON observations(event_id, kind)
  WHERE event_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_obs_session
  ON observations(session_id);
CREATE INDEX IF NOT EXISTS idx_obs_project_occurred
  ON observations(project, occurred_at_epoch DESC);
CREATE INDEX IF NOT EXISTS idx_obs_kind_occurred
  ON observations(kind, occurred_at_epoch DESC);
CREATE INDEX IF NOT EXISTS idx_obs_supersedes
  ON observations(supersedes_id);
CREATE INDEX IF NOT EXISTS idx_obs_ttl
  ON observations(ttl_review_epoch)
  WHERE ttl_review_epoch IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_obs_principal
  ON observations(principal_id);
CREATE INDEX IF NOT EXISTS idx_obs_deleted
  ON observations(deleted_at_epoch)
  WHERE deleted_at_epoch IS NOT NULL;

CREATE TABLE IF NOT EXISTS credentials (
  id                  TEXT PRIMARY KEY,
  token_hash          TEXT NOT NULL UNIQUE,
  prefix              TEXT NOT NULL,
  label               TEXT,
  scopes_json         TEXT NOT NULL,
  revoked_at_epoch    INTEGER,
  created_at_epoch    INTEGER NOT NULL,
  last_used_at_epoch  INTEGER
);

CREATE TABLE IF NOT EXISTS audit_log (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
  principal_id        TEXT,
  credential_id       TEXT,
  request_id          TEXT,
  action              TEXT NOT NULL,
  resource_type       TEXT,
  resource_id         TEXT,
  details_json        TEXT,
  created_at_epoch    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS schema_version (
  version    INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL,
  note       TEXT
);

CREATE VIRTUAL TABLE IF NOT EXISTS observations_fts USING fts5(
  text,
  source_adapter UNINDEXED,
  event_type UNINDEXED,
  project UNINDEXED,
  content='observations',
  content_rowid='id',
  tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER IF NOT EXISTS observations_ai AFTER INSERT ON observations BEGIN
  INSERT INTO observations_fts(rowid, text, source_adapter, event_type, project)
  VALUES (new.id, new.text, new.source_adapter, new.event_type, new.project);
END;
CREATE TRIGGER IF NOT EXISTS observations_ad AFTER DELETE ON observations BEGIN
  INSERT INTO observations_fts(observations_fts, rowid, text, source_adapter, event_type, project)
  VALUES('delete', old.id, old.text, old.source_adapter, old.event_type, old.project);
END;
CREATE TRIGGER IF NOT EXISTS observations_au AFTER UPDATE ON observations BEGIN
  INSERT INTO observations_fts(observations_fts, rowid, text, source_adapter, event_type, project)
  VALUES('delete', old.id, old.text, old.source_adapter, old.event_type, old.project);
  INSERT INTO observations_fts(rowid, text, source_adapter, event_type, project)
  VALUES (new.id, new.text, new.source_adapter, new.event_type, new.project);
END;

INSERT OR IGNORE INTO schema_version(version, applied_at, note)
VALUES (1, datetime('now'), 'initial event store');

