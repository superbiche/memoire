-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

-- opencode-v2-census.sql — a synthetic OpenCode 2.0 store for the v2 census
-- test (tests/foreign/opencode-v2-capture.sh). Every name, path and sentence
-- is invented. The store holds the frozen v1 tables of a migrated session
-- (session, message, part: the migration floor is max(message.time_updated) =
-- 1700000100000) and the v2 tables: session_v2, session_message (one row per
-- message type and per case of the v2 mapping in the adapter header: system,
-- user with a file, assistant with reasoning + tool + text, synthetic,
-- assistant with a failed tool, an assistant not completed, one with a tool
-- still running, truncated tools with a spill file / a missing spill / no
-- spill, reasoning only, compaction, idle, a user without files, empty and
-- non-empty text items, an unknown content type, a non-JSON row, a BLOB row),
-- todo, permission, project_directory, the queued-prompt tables and the
-- never-read tables carrying canary values. The test replaces __TOOL_OUT__
-- with the scratch tool-output directory before loading.
--
-- Expected events per type with the derived floor (the test asserts these):
--   opencode_session_facts 2, opencode_todo 1, opencode_permission 2,
--   opencode_message 18, opencode_user_prompt 2, opencode_file 1,
--   opencode_item 3, opencode_reasoning 2, opencode_assistant_message 7,
--   opencode_tool_call 5, opencode_tool_result 4, opencode_tool_failure 1,
--   opencode_compaction 1, opencode_assistant_turn 5   (54 in all).
-- ses_v01 (migrated): its three rows at or below the floor emit nothing; the
-- one row edited after the migration (time_updated 1700000105000) emits
-- opencode_message + opencode_assistant_message + opencode_assistant_turn.

CREATE TABLE `project` (`id` text PRIMARY KEY, `worktree` text NOT NULL, `vcs` text, `name` text, `icon_url` text, `icon_color` text, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `time_initialized` integer, `sandboxes` text NOT NULL, `commands` text, `icon_url_override` text);
CREATE TABLE `project_directory` (`project_id` text NOT NULL, `directory` text NOT NULL, `type` text, `strategy` text, `time_created` integer NOT NULL, PRIMARY KEY(`project_id`, `directory`));
-- frozen v1 tables
CREATE TABLE `session` (`id` text PRIMARY KEY, `project_id` text NOT NULL, `parent_id` text, `slug` text NOT NULL, `directory` text NOT NULL, `title` text NOT NULL, `version` text NOT NULL, `share_url` text, `summary_additions` integer, `summary_deletions` integer, `summary_files` integer, `summary_diffs` text, `revert` text, `permission` text, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `time_compacting` integer, `time_archived` integer, `workspace_id` text, `path` text, `agent` text, `model` text, `cost` real DEFAULT 0 NOT NULL, `tokens_input` integer DEFAULT 0 NOT NULL, `tokens_output` integer DEFAULT 0 NOT NULL, `tokens_reasoning` integer DEFAULT 0 NOT NULL, `tokens_cache_read` integer DEFAULT 0 NOT NULL, `tokens_cache_write` integer DEFAULT 0 NOT NULL, `metadata` text);
CREATE TABLE `message` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL);
CREATE TABLE `part` (`id` text PRIMARY KEY, `message_id` text NOT NULL, `session_id` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL);
CREATE INDEX `message_session_time_created_id_idx` ON `message` (`session_id`,`time_created`,`id`);
-- v2 tables
CREATE TABLE `session_v2` (`id` text PRIMARY KEY, `project_id` text NOT NULL, `workspace_id` text, `parent_id` text, `fork_session_id` text, `fork_boundary` text, `slug` text NOT NULL, `directory` text NOT NULL, `path` text, `title` text, `version` text NOT NULL, `share_url` text, `summary_additions` integer, `summary_deletions` integer, `summary_files` integer, `summary_diffs` text, `metadata` text, `cost` real DEFAULT 0 NOT NULL, `tokens_input` integer DEFAULT 0 NOT NULL, `tokens_output` integer DEFAULT 0 NOT NULL, `tokens_reasoning` integer DEFAULT 0 NOT NULL, `tokens_cache_read` integer DEFAULT 0 NOT NULL, `tokens_cache_write` integer DEFAULT 0 NOT NULL, `revert` text, `permission` text, `agent` text, `model` text, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `time_compacting` integer, `time_archived` integer, `time_suspended` integer, `resume_attempts` integer DEFAULT 0 NOT NULL, `time_idle` integer, `time_viewed` integer, `idle_outcome` text);
CREATE TABLE `session_message` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `type` text NOT NULL, `seq` integer NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL);
CREATE UNIQUE INDEX `session_message_session_seq_idx` ON `session_message` (`session_id`,`seq`);
CREATE INDEX `session_message_session_time_created_id_idx` ON `session_message` (`session_id`,`time_created`,`id`);
CREATE TABLE `session_pending` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `type` text NOT NULL, `data` text NOT NULL, `delivery` text, `admitted_seq` integer NOT NULL, `time_created` integer NOT NULL);
CREATE TABLE `session_inbox` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `type` text NOT NULL, `payload` text NOT NULL, `delivery` text NOT NULL, `enqueued_seq` integer NOT NULL, `time_created` integer NOT NULL);
CREATE TABLE `todo` (`session_id` text NOT NULL, `content` text NOT NULL, `status` text NOT NULL, `priority` text NOT NULL, `position` integer NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, PRIMARY KEY(`session_id`, `position`));
CREATE TABLE `permission` (`id` text PRIMARY KEY, `project_id` text NOT NULL, `action` text NOT NULL, `resource` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL);
-- never read by the adapter: a canary in each, the test fails if one surfaces
CREATE TABLE `credential` (`id` text PRIMARY KEY, `secret` text NOT NULL);
CREATE TABLE `account` (`id` text PRIMARY KEY, `token` text NOT NULL);
CREATE TABLE `account_state` (`id` text PRIMARY KEY, `state` text NOT NULL);
CREATE TABLE `control_account` (`id` text PRIMARY KEY, `key` text NOT NULL);
INSERT INTO credential VALUES ('cred1', 'CANARY-credential-ñ');
INSERT INTO account VALUES ('acc1', 'CANARY-account-ñ');
INSERT INTO account_state VALUES ('acs1', 'CANARY-account-state-ñ');
INSERT INTO control_account VALUES ('ctl1', 'CANARY-control-ñ');
INSERT INTO session_pending VALUES ('pend1', 'ses_v02', 'prompt', '{"text":"CANARY-pending é"}', 'queued', 1, 1700001000000);
INSERT INTO session_inbox VALUES ('inb1', 'ses_v02', 'prompt', '{"text":"CANARY-inbox é"}', 'queued', 1, 1700001000000);

INSERT INTO project VALUES ('prj_v', '/tmp/oc-v2-census/alpha', 'git', 'alpha', NULL, NULL, 1700000000000, 1700000000000, NULL, '[]', NULL, NULL);
INSERT INTO project_directory VALUES ('prj_v', '/tmp/oc-v2-census/alpha-extra', 'worktree', 'manual', 1700000000000);
INSERT INTO permission VALUES ('perm_v1', 'prj_v', 'bash', 'ls é', 1700000000000, 1700000001000);

-- ---- ses_v01: migrated from v1 ----------------------------------------------
-- v1 rows (frozen): the floor is max(message.time_updated) = 1700000100000
INSERT INTO session VALUES ('ses_v01', 'prj_v', NULL, 'old-river', '/tmp/oc-v2-census/alpha', 'Old river é', '1.18.34', NULL, NULL, NULL, NULL, NULL, NULL, NULL, 1700000000000, 1700000100000, NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL);
INSERT INTO message VALUES ('msg_v01u', 'ses_v01', 1700000001000, 1700000001000, '{"role":"user","time":{"created":1700000001000}}');
INSERT INTO part VALUES ('prt_v01u', 'msg_v01u', 'ses_v01', 1700000001000, 1700000001000, '{"type":"text","text":"Migrated prompt — ne pas réémettre"}');
INSERT INTO message VALUES ('msg_v01a', 'ses_v01', 1700000002000, 1700000100000, '{"role":"assistant","time":{"created":1700000002000,"completed":1700000003000}}');
INSERT INTO part VALUES ('prt_v01a', 'msg_v01a', 'ses_v01', 1700000002500, 1700000002500, '{"type":"text","text":"Migrated answer — ne pas réémettre"}');
-- the same session in v2: the migrated rows keep their ids and times
INSERT INTO session_v2 VALUES ('ses_v01', 'prj_v', NULL, NULL, NULL, NULL, 'old-river', '/tmp/oc-v2-census/alpha', NULL, 'Old river é', '1.18.34', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, 'build', '{"id":"coder","providerID":"conductor"}', 1700000000000, 1700000105000, NULL, NULL, NULL, 0, 1700000105000, NULL, 'succeeded');
INSERT INTO session_message VALUES ('msg_v01u', 'ses_v01', 'user', 0, 1700000001000, 1700000001000, '{"text":"Migrated prompt — ne pas réémettre","files":[],"time":{"created":1700000001000}}');
INSERT INTO session_message VALUES ('msg_v01a', 'ses_v01', 'assistant', 1, 1700000002000, 1700000100000, '{"agent":"build","model":{"id":"coder","providerID":"conductor"},"content":[{"type":"text","text":"Migrated answer — ne pas réémettre"}],"finish":"stop","time":{"created":1700000002000,"completed":1700000003000}}');
INSERT INTO session_message VALUES ('msg_v01i', 'ses_v01', 'idle', 2, 1700000003001, 1700000003001, '{"time":{"created":1700000003001},"outcome":"succeeded"}');
-- edited after the migration: above the floor, emitted under the v2 mapping
INSERT INTO session_message VALUES ('msg_v01b', 'ses_v01', 'assistant', 3, 1700000104000, 1700000105000, '{"agent":"build","model":{"id":"coder","providerID":"conductor"},"content":[{"type":"text","text":"Edited after the migration — réémis 日本"}],"finish":"stop","time":{"created":1700000104000,"completed":1700000104500}}');

-- ---- ses_v02: born in v2 -------------------------------------------------------
INSERT INTO session_v2 VALUES ('ses_v02', 'prj_v', NULL, NULL, NULL, NULL, 'new-canyon', '/tmp/oc-v2-census/alpha', NULL, 'New canyon 😀', '2.0.22', NULL, 3, 1, 2, NULL, NULL, 0.5, 100, 20, 5, 0, 0, NULL, NULL, 'build', '{"id":"coder","providerID":"conductor"}', 1700001000000, 1700001020000, NULL, NULL, NULL, 0, 1700001020000, NULL, 'succeeded');
INSERT INTO todo VALUES ('ses_v02', 'write the census é', 'completed', 'high', 0, 1700001000000, 1700001001000);
INSERT INTO todo VALUES ('ses_v02', 'read it back 日本', 'pending', 'low', 1, 1700001000000, 1700001001000);
-- seq 0: a system row (instructions)
INSERT INTO session_message VALUES ('msg_v02s0', 'ses_v02', 'system', 0, 1700001000100, 1700001000100, '{"metadata":{"notice":"instructions"},"time":{"created":1700001000100},"text":"Instructions updated — règles é","description":"Instructions updated: core"}');
-- seq 1: a user row with text and one inline file
INSERT INTO session_message VALUES ('msg_v02u1', 'ses_v02', 'user', 1, 1700001000200, 1700001000200, '{"text":"Hola é日本 😀 — run ls","files":[{"data":"QUJDREVG","mime":"image/png","source":{"type":"inline"},"name":"pasted.png"}],"time":{"created":1700001000200}}');
-- seq 2: a completed assistant row: reasoning, a completed tool, text
INSERT INTO session_message VALUES ('msg_v02a2', 'ses_v02', 'assistant', 2, 1700001000300, 1700001000900, '{"agent":"build","model":{"id":"coder","providerID":"conductor"},"content":[{"type":"reasoning","text":"Thinking in français…","time":{"created":1700001000310,"completed":1700001000320}},{"type":"tool","id":"call_v02a2","name":"shell","state":{"status":"completed","input":{"command":"ls é"},"content":[{"type":"text","text":"fichier-un\nfichier-deux 日本"}],"metadata":{"exit":0,"truncated":false}},"time":{"created":1700001000330,"ran":1700001000340,"completed":1700001000350}},{"type":"text","text":"Two files — voilà 😀."}],"finish":"stop","cost":0.1,"tokens":{"input":10,"output":5,"reasoning":1,"cache":{"read":0,"write":0}},"time":{"created":1700001000300,"streamed":1700001000800,"completed":1700001000900}}');
-- seq 3: a synthetic row
INSERT INTO session_message VALUES ('msg_v02y3', 'ses_v02', 'synthetic', 3, 1700001001000, 1700001001000, '{"text":"Continue from where you left — synthétique","time":{"created":1700001001000}}');
-- seq 4: a completed assistant row with a FAILED tool and text
INSERT INTO session_message VALUES ('msg_v02a4', 'ses_v02', 'assistant', 4, 1700001001100, 1700001001500, '{"agent":"build","model":{"id":"coder","providerID":"conductor"},"content":[{"type":"tool","id":"call_v02a4","name":"shell","state":{"status":"error","input":{"command":"cat missing"},"error":{"name":"ToolError","message":"no such file — introuvable"},"metadata":{"exit":1}},"time":{"created":1700001001110,"completed":1700001001200}},{"type":"text","text":"The file is missing — introuvable."}],"finish":"stop","time":{"created":1700001001100,"completed":1700001001500}}');
-- seq 5: an assistant row NOT completed (no turn)
INSERT INTO session_message VALUES ('msg_v02a5', 'ses_v02', 'assistant', 5, 1700001001600, 1700001001700, '{"agent":"build","model":{"id":"coder","providerID":"conductor"},"content":[{"type":"text","text":"Still streaming — en cours"}],"time":{"created":1700001001600}}');
-- seq 6: a completed row whose tool is still running (no turn; stuck gate not armed: old)
INSERT INTO session_message VALUES ('msg_v02a6', 'ses_v02', 'assistant', 6, 1700001001800, 1700001001900, '{"agent":"build","model":{"id":"coder","providerID":"conductor"},"content":[{"type":"tool","id":"call_v02a6","name":"shell","state":{"status":"running","input":{"command":"sleep 100"},"metadata":{}},"time":{"created":1700001001810}}],"time":{"created":1700001001800,"completed":1700001001900}}');
-- seq 7: a truncated tool output with a spill file (attached whole) and text
INSERT INTO session_message VALUES ('msg_v02a7', 'ses_v02', 'assistant', 7, 1700001002000, 1700001002500, '{"agent":"build","model":{"id":"coder","providerID":"conductor"},"content":[{"type":"tool","id":"call_v02a7","name":"shell","state":{"status":"completed","input":{"command":"cat big"},"content":[{"type":"text","text":"first lines only — tronqué"}],"metadata":{"exit":0,"truncated":true,"outputPath":"__TOOL_OUT__/tool_ok"}},"time":{"created":1700001002010,"completed":1700001002020}},{"type":"text","text":"Spilled — voir le fichier."}],"finish":"stop","time":{"created":1700001002000,"completed":1700001002500}}');
-- seq 8: a truncated tool output whose spill file is gone (no text: no turn)
INSERT INTO session_message VALUES ('msg_v02a8', 'ses_v02', 'assistant', 8, 1700001002600, 1700001002700, '{"agent":"build","model":{"id":"coder","providerID":"conductor"},"content":[{"type":"tool","id":"call_v02a8","name":"shell","state":{"status":"completed","input":{"command":"cat gone"},"content":[{"type":"text","text":"head only — disparu"}],"metadata":{"exit":0,"truncated":true,"outputPath":"__TOOL_OUT__/tool_gone"}},"time":{"created":1700001002610,"completed":1700001002620}}],"finish":"tool-calls","time":{"created":1700001002600,"completed":1700001002700}}');
-- seq 9: reasoning only, completed (no text: no turn)
INSERT INTO session_message VALUES ('msg_v02a9', 'ses_v02', 'assistant', 9, 1700001002800, 1700001002900, '{"agent":"build","model":{"id":"coder","providerID":"conductor"},"content":[{"type":"reasoning","text":"Only thinking — réflexion"}],"finish":"stop","time":{"created":1700001002800,"completed":1700001002900}}');
-- seq 10: a completed compaction
INSERT INTO session_message VALUES ('msg_v02c10', 'ses_v02', 'compaction', 10, 1700001003000, 1700001003100, '{"time":{"created":1700001003000},"status":"completed","reason":"manual","model":{"id":"coder","providerID":"conductor"},"summary":"## Objective\n- résumé 日本","recent":"","cost":0,"tokens":{"input":1,"output":1,"reasoning":0,"cache":{"read":0,"write":0}}}');
-- seq 11: idle
INSERT INTO session_message VALUES ('msg_v02i11', 'ses_v02', 'idle', 11, 1700001003200, 1700001003200, '{"time":{"created":1700001003200},"outcome":"succeeded"}');
-- seq 12: a user row without files
INSERT INTO session_message VALUES ('msg_v02u12', 'ses_v02', 'user', 12, 1700001003300, 1700001003300, '{"text":"Second prompt — deuxième","files":[],"time":{"created":1700001003300}}');
-- seq 13: completed, an empty text item and a non-empty one (turn = the non-empty one)
INSERT INTO session_message VALUES ('msg_v02a13', 'ses_v02', 'assistant', 13, 1700001003400, 1700001003500, '{"agent":"build","model":{"id":"coder","providerID":"conductor"},"content":[{"type":"text","text":""},{"type":"text","text":"Only this — seulement ceci 😀"}],"finish":"stop","time":{"created":1700001003400,"completed":1700001003500}}');
-- seq 14: an unknown content type, completed, no text (no turn)
INSERT INTO session_message VALUES ('msg_v02a14', 'ses_v02', 'assistant', 14, 1700001003600, 1700001003700, '{"agent":"build","model":{"id":"coder","providerID":"conductor"},"content":[{"type":"weird","payload":"inconnu é"}],"finish":"stop","time":{"created":1700001003600,"completed":1700001003700}}');
-- seq 15: a row whose data is not JSON (record event only, raw)
INSERT INTO session_message VALUES ('msg_v02x15', 'ses_v02', 'system', 15, 1700001003800, 1700001003800, 'not json at all — brut');
-- seq 16: a row whose data is a BLOB (record event only, base64)
INSERT INTO session_message VALUES ('msg_v02b16', 'ses_v02', 'user', 16, 1700001003900, 1700001003900, X'00FF10');

-- ---- a session whose id is not [A-Za-z0-9_.-]: refused at selection ----------
INSERT INTO session_v2 VALUES ('ses_bad''; DROP TABLE x; --', 'prj_v', NULL, NULL, NULL, NULL, 'bad', '/tmp/oc-v2-census/alpha', NULL, 'bad', '2.0.22', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, 0, 0, 0, 0, NULL, NULL, NULL, NULL, 1700001000000, 1700001000000, NULL, NULL, NULL, 0, NULL, NULL, NULL);
INSERT INTO session_message VALUES ('msg_bad', 'ses_bad''; DROP TABLE x; --', 'user', 0, 1700001000000, 1700001000000, '{"text":"CANARY-bad-session","files":[],"time":{"created":1700001000000}}');
