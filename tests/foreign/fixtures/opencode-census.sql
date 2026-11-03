-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Michel Tomas

-- opencode-census.sql — a synthetic OpenCode store for the census test
-- (tests/foreign/opencode-capture.sh). Every name, path and sentence is
-- invented. One row per record type and per case of the mapping table in the
-- adapter header: text (user, assistant, synthetic, escape-heavy, empty),
-- reasoning, tool (completed, error, running, truncated with a spill file,
-- truncated with a missing spill, truncated without a spill), patch,
-- compaction, file, step-start, step-finish, an unknown part type, a
-- non-JSON part, a message with an error and no parts, a message that is not
-- completed, a message with a running tool, todo, session_input,
-- session_message, session_context_epoch, permission, project_directory, and
-- the never-read tables carrying canary values. The test replaces
-- __TOOL_OUT__ with the scratch tool-output directory before loading.
--
-- Expected events per type (the test asserts these numbers):
--   opencode_session_facts 4, opencode_todo 1, opencode_permission 4,
--   opencode_session_input 1, opencode_context_epoch 1, opencode_message 13,
--   opencode_user_prompt 3, opencode_assistant_message 9,
--   opencode_reasoning 1, opencode_item 3, opencode_file 1,
--   opencode_tool_call 6, opencode_tool_result 5, opencode_tool_failure 1,
--   opencode_patch 1, opencode_compaction 1, opencode_assistant_turn 3
--   (58 in all; step-start and step-finish rows produce none, they fold into
--   the message).

CREATE TABLE `project` (`id` text PRIMARY KEY, `worktree` text NOT NULL, `vcs` text, `name` text, `icon_url` text, `icon_color` text, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `time_initialized` integer, `sandboxes` text NOT NULL, `commands` text, `icon_url_override` text);
CREATE TABLE `project_directory` (`project_id` text NOT NULL, `directory` text NOT NULL, `type` text, `strategy` text, `time_created` integer NOT NULL, PRIMARY KEY(`project_id`, `directory`));
CREATE TABLE `session` (`id` text PRIMARY KEY, `project_id` text NOT NULL, `parent_id` text, `slug` text NOT NULL, `directory` text NOT NULL, `title` text NOT NULL, `version` text NOT NULL, `share_url` text, `summary_additions` integer, `summary_deletions` integer, `summary_files` integer, `summary_diffs` text, `revert` text, `permission` text, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `time_compacting` integer, `time_archived` integer, `workspace_id` text, `path` text, `agent` text, `model` text, `cost` real DEFAULT 0 NOT NULL, `tokens_input` integer DEFAULT 0 NOT NULL, `tokens_output` integer DEFAULT 0 NOT NULL, `tokens_reasoning` integer DEFAULT 0 NOT NULL, `tokens_cache_read` integer DEFAULT 0 NOT NULL, `tokens_cache_write` integer DEFAULT 0 NOT NULL, `metadata` text);
CREATE TABLE `message` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL);
CREATE TABLE `part` (`id` text PRIMARY KEY, `message_id` text NOT NULL, `session_id` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL);
CREATE TABLE `todo` (`session_id` text NOT NULL, `content` text NOT NULL, `status` text NOT NULL, `priority` text NOT NULL, `position` integer NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, PRIMARY KEY(`session_id`, `position`));
CREATE TABLE `session_message` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `type` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL, `seq` integer NOT NULL);
CREATE TABLE `session_input` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `prompt` text NOT NULL, `delivery` text NOT NULL, `admitted_seq` integer NOT NULL, `promoted_seq` integer, `time_created` integer NOT NULL);
CREATE TABLE `session_context_epoch` (`session_id` text PRIMARY KEY, `baseline` text NOT NULL, `snapshot` text NOT NULL, `baseline_seq` integer NOT NULL);
CREATE TABLE `permission` (`id` text PRIMARY KEY, `project_id` text NOT NULL, `action` text NOT NULL, `resource` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL);
CREATE INDEX `part_session_idx` ON `part` (`session_id`);
CREATE INDEX `part_message_id_id_idx` ON `part` (`message_id`,`id`);
CREATE INDEX `message_session_time_created_id_idx` ON `message` (`session_id`,`time_created`,`id`);
-- never read by the adapter: a canary in each, the test fails if one surfaces
CREATE TABLE `credential` (`id` text PRIMARY KEY, `value` text NOT NULL);
CREATE TABLE `account` (`id` text PRIMARY KEY, `access_token` text NOT NULL);
CREATE TABLE `account_state` (`id` integer PRIMARY KEY, `active_account_id` text);
CREATE TABLE `control_account` (`email` text NOT NULL, `access_token` text NOT NULL);
INSERT INTO credential VALUES ('cred1', 'CANARY-CREDENTIAL-7Q1');
INSERT INTO account VALUES ('acc1', 'CANARY-ACCOUNT-7Q2');
INSERT INTO account_state VALUES (1, 'CANARY-ACCOUNTSTATE-7Q3');
INSERT INTO control_account VALUES ('nobody@example.invalid', 'CANARY-CONTROL-7Q4');

INSERT INTO project (id, worktree, vcs, name, time_created, time_updated, sandboxes) VALUES ('prj_c1', '/tmp/oc-census/alpha', 'git', 'alpha', 1767225500000, 1767225500000, '[]');
INSERT INTO project_directory VALUES ('prj_c1', '/tmp/oc-census/alpha-extra', 'sandbox', 'worktree', 1767225500000);
INSERT INTO permission VALUES ('perm_c1', 'prj_c1', 'edit', 'src/**', 1767225510000, 1767225511000);

INSERT INTO session (id, project_id, parent_id, slug, directory, title, version, time_created, time_updated, agent, model, cost, tokens_input, tokens_output) VALUES
 ('ses_c01', 'prj_c1', NULL, 'census-one', '/tmp/oc-census/alpha', 'Census session one', '1.0.0', 1767225600000, 1767225690000, 'build', '{"providerID":"acme","modelID":"m1"}', 0.25, 1200, 300),
 ('ses_c02', 'prj_c1', 'ses_c01', 'census-child', '/tmp/oc-census/alpha', 'Census child', '1.0.0', 1767225650000, 1767225651000, 'explore', NULL, 0, 10, 5),
 ('ses_c03', 'prj_c1', NULL, 'census-empty', '/tmp/oc-census/alpha', 'Census empty', '1.0.0', 1767225660000, 1767225660000, NULL, NULL, 0, 0, 0),
 ('ses_c04', 'prj_c1', NULL, 'census-guards', '/tmp/oc-census/alpha', 'Census guards', '1.0.0', 1767225670000, 1767225670000, NULL, NULL, 0, 0, 0);

INSERT INTO todo VALUES ('ses_c01', 'write the census', 'completed', 'high', 0, 1767225600100, 1767225600200);
INSERT INTO todo VALUES ('ses_c01', 'review the census', 'pending', 'low', 1, 1767225600100, 1767225600300);
INSERT INTO session_input VALUES ('inp_c1', 'ses_c04', 'queued invented prompt', 'immediate', 1, NULL, 1767225671000);
INSERT INTO session_message VALUES ('smg_c1', 'ses_c04', 'user', 1767225672000, 1767225672000, '{"invented":true}', 1);
INSERT INTO session_context_epoch VALUES ('ses_c04', 'baseline invented text', 'snapshot invented text', 3);

-- m01: a user message with three parts (prompt, synthetic note, file)
INSERT INTO message VALUES ('msg_c01', 'ses_c01', 1767225600000, 1767225600000, '{"role":"user","time":{"created":1767225600000},"agent":"build","summary":{"title":"invented"}}');
INSERT INTO part VALUES ('prt_c01', 'msg_c01', 'ses_c01', 1767225600000, 1767225600000, '{"type":"text","text":"Please list the invented files — é日本😀","time":{"start":1767225599990,"end":1767225600000}}');
INSERT INTO part VALUES ('prt_c02', 'msg_c01', 'ses_c01', 1767225600001, 1767225600001, '{"type":"text","text":"Synthetic invented note","synthetic":true}');
INSERT INTO part VALUES ('prt_c03', 'msg_c01', 'ses_c01', 1767225600002, 1767225600002, '{"type":"file","mime":"text/plain","filename":"invented.txt","url":"file:///tmp/oc-census/alpha/invented.txt"}');
-- m02: an assistant turn with every part type
INSERT INTO message VALUES ('msg_c02', 'ses_c01', 1767225601000, 1767225605000, '{"role":"assistant","time":{"created":1767225601000,"completed":1767225604000},"modelID":"m1","providerID":"acme","cost":0.1,"tokens":{"input":100,"output":20},"finish":"stop","mode":"build","path":{"cwd":"/tmp/oc-census/alpha","root":"/tmp/oc-census"}}');
INSERT INTO part VALUES ('prt_c10', 'msg_c02', 'ses_c01', 1767225601010, 1767225601010, '{"type":"step-start","snapshot":"abc123"}');
INSERT INTO part VALUES ('prt_c11', 'msg_c02', 'ses_c01', 1767225601020, 1767225601030, '{"type":"reasoning","text":"invented reasoning about the files","time":{"start":1767225601020,"end":1767225601030},"metadata":{"openrouter":{"reasoning_details":[{"kind":"invented"}]}}}');
INSERT INTO part VALUES ('prt_c12', 'msg_c02', 'ses_c01', 1767225601040, 1767225601050, '{"type":"text","text":"I will run ls now — voilà 日本.","time":{"start":1767225601040,"end":1767225601050}}');
INSERT INTO part VALUES ('prt_c13', 'msg_c02', 'ses_c01', 1767225601060, 1767225602000, '{"type":"tool","tool":"bash","callID":"call_c13","state":{"status":"completed","input":{"command":"ls","description":"list"},"output":"a.txt\nb.txt\n","title":"list","metadata":{"exit":0,"description":"list"},"time":{"start":1767225601060,"end":1767225602000}},"metadata":{"openrouter":{"note":"part-level metadata"}}}');
INSERT INTO part VALUES ('prt_c14', 'msg_c02', 'ses_c01', 1767225602010, 1767225602500, '{"type":"tool","tool":"read","callID":"call_c14","state":{"status":"error","input":{"filePath":"/tmp/oc-census/alpha/absent.txt"},"error":"file not found","time":{"start":1767225602010,"end":1767225602500}}}');
INSERT INTO part VALUES ('prt_c15', 'msg_c02', 'ses_c01', 1767225603000, 1767225603100, '{"type":"text","text":"Done: two files — é😀.","time":{"start":1767225603000,"end":1767225603100}}');
INSERT INTO part VALUES ('prt_c16', 'msg_c02', 'ses_c01', 1767225603200, 1767225603200, '{"type":"step-finish","reason":"tool-calls","tokens":{"input":50,"output":10},"cost":0.05,"snapshot":"abc124"}');
INSERT INTO part VALUES ('prt_c17', 'msg_c02', 'ses_c01', 1767225604000, 1767225604000, '{"type":"step-finish","reason":"stop","tokens":{"input":50,"output":10},"cost":0.05,"snapshot":"abc125"}');
INSERT INTO part VALUES ('prt_c18', 'msg_c02', 'ses_c01', 1767225604100, 1767225604100, '{"type":"patch","hash":"h0a1b2c","files":["a.txt","b.txt"]}');
INSERT INTO part VALUES ('prt_c19', 'msg_c02', 'ses_c01', 1767225604200, 1767225604200, '{"type":"compaction","auto":true,"overflow":false,"tail_start_id":"msg_c02"}');
-- m03: an assistant message with an error and no parts
INSERT INTO message VALUES ('msg_c03', 'ses_c01', 1767225606000, 1767225606000, '{"role":"assistant","time":{"created":1767225606000},"error":{"name":"APIError","data":{"message":"invented failure","statusCode":429}}}');
-- m04: assistant not completed, with a text part: no turn
INSERT INTO message VALUES ('msg_c04', 'ses_c01', 1767225607000, 1767225607000, '{"role":"assistant","time":{"created":1767225607000}}');
INSERT INTO part VALUES ('prt_c30', 'msg_c04', 'ses_c01', 1767225607010, 1767225607010, '{"type":"text","text":"unfinished invented answer"}');
-- m05: completed, but a tool part is still running: no turn
INSERT INTO message VALUES ('msg_c05', 'ses_c01', 1767225608000, 1767225608500, '{"role":"assistant","time":{"created":1767225608000,"completed":1767225608400}}');
INSERT INTO part VALUES ('prt_c40', 'msg_c05', 'ses_c01', 1767225608010, 1767225608020, '{"type":"tool","tool":"bash","callID":"call_c40","state":{"status":"running","input":{"command":"sleep 99"},"time":{"start":1767225608010}}}');
INSERT INTO part VALUES ('prt_c41', 'msg_c05', 'ses_c01', 1767225608100, 1767225608100, '{"type":"text","text":"waiting on the invented tool"}');
-- m06: truncated tool output with a spill file that exists
INSERT INTO message VALUES ('msg_c06', 'ses_c01', 1767225609000, 1767225609900, '{"role":"assistant","time":{"created":1767225609000,"completed":1767225609800}}');
INSERT INTO part VALUES ('prt_c50', 'msg_c06', 'ses_c01', 1767225609010, 1767225609700, '{"type":"tool","tool":"bash","callID":"call_c50","state":{"status":"completed","input":{"command":"cat big"},"output":"tail of the invented output","title":"cat","metadata":{"truncated":true,"outputPath":"__TOOL_OUT__/tool_ok"},"time":{"start":1767225609010,"end":1767225609700}}}');
-- m07: truncated, the spill file is gone
INSERT INTO message VALUES ('msg_c07', 'ses_c01', 1767225610000, 1767225610900, '{"role":"assistant","time":{"created":1767225610000,"completed":1767225610800}}');
INSERT INTO part VALUES ('prt_c60', 'msg_c07', 'ses_c01', 1767225610010, 1767225610700, '{"type":"tool","tool":"bash","callID":"call_c60","state":{"status":"completed","input":{"command":"cat gone"},"output":"tail of another invented output","title":"cat","metadata":{"truncated":true,"outputPath":"__TOOL_OUT__/tool_gone"},"time":{"start":1767225610010,"end":1767225610700}}}');
-- m08: truncated with no spill path at all
INSERT INTO message VALUES ('msg_c08', 'ses_c01', 1767225611000, 1767225611900, '{"role":"assistant","time":{"created":1767225611000,"completed":1767225611800}}');
INSERT INTO part VALUES ('prt_c70', 'msg_c08', 'ses_c01', 1767225611010, 1767225611700, '{"type":"tool","tool":"bash","callID":"call_c70","state":{"status":"completed","input":{"command":"cat cut"},"output":"third invented tail","title":"cat","metadata":{"truncated":true},"time":{"start":1767225611010,"end":1767225611700}}}');
-- m09: an unknown part type and a part whose data is not JSON
INSERT INTO message VALUES ('msg_c09', 'ses_c01', 1767225612000, 1767225612900, '{"role":"assistant","time":{"created":1767225612000,"completed":1767225612800}}');
INSERT INTO part VALUES ('prt_c80', 'msg_c09', 'ses_c01', 1767225612010, 1767225612010, '{"type":"weird","payload":{"invented":[1,2,3]}}');
INSERT INTO part VALUES ('prt_c81', 'msg_c09', 'ses_c01', 1767225612020, 1767225612020, 'this is not json {');
-- m10: a user message whose text is escape-heavy (backslashes, quotes, newlines, tabs, unicode)
INSERT INTO message VALUES ('msg_c10', 'ses_c01', 1767225613000, 1767225613000, '{"role":"user","time":{"created":1767225613000}}');
INSERT INTO part VALUES ('prt_c90', 'msg_c10', 'ses_c01', 1767225613010, 1767225613010, json_object('type', 'text', 'text', 'line one' || char(10) || 'back\slash "quoted" ' || char(9) || 'tab é 日本 😀 \\ done' || char(10) || char(10) || 'last'));
-- m11: completed, two text parts and an empty one: the turn joins the two
INSERT INTO message VALUES ('msg_c11', 'ses_c01', 1767225614000, 1767225614900, '{"role":"assistant","time":{"created":1767225614000,"completed":1767225614800}}');
INSERT INTO part VALUES ('prt_c95', 'msg_c11', 'ses_c01', 1767225614010, 1767225614010, '{"type":"text","text":"First invented paragraph — é日本."}');
INSERT INTO part VALUES ('prt_c96', 'msg_c11', 'ses_c01', 1767225614020, 1767225614020, '{"type":"text","text":"Second invented paragraph 😀."}');
INSERT INTO part VALUES ('prt_c97', 'msg_c11', 'ses_c01', 1767225614030, 1767225614030, '{"type":"text","text":""}');
-- m12: the turn text is joined in (time_created, id) order, NOT in id order
-- and NOT in insertion order: prt_c98 sorts before prt_c99 by id and is
-- inserted first, but prt_c99 is the earlier paragraph
INSERT INTO message VALUES ('msg_c12', 'ses_c01', 1767225615000, 1767225615900, '{"role":"assistant","time":{"created":1767225615000,"completed":1767225615800}}');
INSERT INTO part VALUES ('prt_c98', 'msg_c12', 'ses_c01', 1767225615020, 1767225615020, '{"type":"text","text":"Beta comes second by time."}');
INSERT INTO part VALUES ('prt_c99', 'msg_c12', 'ses_c01', 1767225615010, 1767225615010, '{"type":"text","text":"Alpha comes first by time."}');
-- the child session
INSERT INTO message VALUES ('msg_d01', 'ses_c02', 1767225650000, 1767225650000, '{"role":"user","time":{"created":1767225650000}}');
INSERT INTO part VALUES ('prt_d01', 'msg_d01', 'ses_c02', 1767225650010, 1767225650010, '{"type":"text","text":"child invented prompt"}');
