# Qwen Rollout Capture Contract

## Purpose

`scripts/memoire-capture-qwen-rollout.sh` captures qwen-code transcript events
into Memoire with the same queue-first, transcript-driven shape as Codex
rollout capture. It parses qwen CLI JSONL session files from disk and emits
events only through `bin/memoire-queue`.

This contract is the oracle for the implementer. The implementer may edit only
the files listed under `## Fence allowlist`. If a test seems wrong, the test is
right and your code is wrong.

Do not implement opencode in this adapter. opencode stores sessions in a live
SQLite database with WAL, not append-only JSONL files, so it needs separate
recon and a read-only DB-snapshot adapter.

## Exact transcript format

Use synthetic fixtures for tests. Never read real `~/.qwen` in tests.

qwen-code v0.17.0 stores one JSONL file per session at:

```text
~/.qwen/projects/<cwd-slug>/chats/<sessionId>.jsonl
```

`<cwd-slug>` is the session cwd with `/` replaced by `-`.

Every line has these top-level fields:

```text
uuid
parentUuid
sessionId
timestamp
type
cwd
version
gitBranch
```

`timestamp` is ISO-8601/RFC3339Nano, for example
`2026-07-06T22:50:06.190Z`.

Line `type` values:

- `user`: `message.role=="user"` and `message.parts[].text` contain the
  human/worker prompt.
- `assistant`: `message.parts[]` is an array. Each part is one of `{text}`,
  `{text, thought}`, or `{functionCall}`. A function call part has
  `{id, name, args}` under `functionCall`.
- `tool_result`: `toolCallResult={callId,resultDisplay,status}` where
  `status` is `success` or `error`. `message.parts[].functionResponse` carries
  result payload content; ignore that content.
- `system`: `subtype=="ui_telemetry"` and `systemPayload.uiEvent`; ignore
  entirely.

File-mutating tools are the only tools that produce file-edit capture events:

- `edit`: `args={file_path,old_string,new_string}`. Derive
  `change_type="update"` from the tool name.
- `write_file`: `args={file_path,content}` or rarely `{file_path}` only.
  Derive `change_type="write"` from the tool name.

`change_type` is never read from `args`. qwen tool arguments do not carry a
`change_type` field.

Non-mutating tools must be skipped:

```text
read_file
grep_search
glob
run_shell_command
todo_write
agent
```

Errored edits must be skipped. A file-mutating `functionCall` counts only when
its matching `tool_result` has `status=="success"`, joined by:

```text
functionCall.id == toolCallResult.callId
```

## CLI contract

The script path is:

```text
scripts/memoire-capture-qwen-rollout.sh
```

It must not collide with `scripts/memoire-capture-qwen.sh`, which is a
text-note wrapper.

Supported flags:

```text
--scan
--since SPEC
--sessions-dir DIR
--project NAME
--include-prompts
--dry-run
-h
--help
```

Default sessions dir:

```text
${QWEN_SESSIONS_DIR:-$HOME/.qwen/projects}
```

With explicit file arguments, ingest those JSONL files. With `--scan`, find
JSONL files under the sessions dir matching:

```text
*/chats/*.jsonl
```

and modified since `--since`, whose default is:

```text
24 hours ago
```

`--scan` and explicit files are mutually exclusive. If no files are found,
exit 0 after logging:

```text
memoire-capture-qwen-rollout: no rollout files to ingest
```

Use only `bash`, `jq`, `sha256sum`, `find`, and the existing queue writer
`bin/memoire-queue` via `MEMOIRE_QUEUE_BIN`. Do not add dependencies. Do not
write pending files, raw transcript copies, or DB rows yourself.

## Exact parsing algorithm

For each readable transcript file:

1. Read session metadata from the first valid JSON line with `.sessionId`.
2. `session_id = .sessionId`.
3. `cwd = .cwd`.
4. `project = --project` if present; otherwise the final path component of
   `cwd` using Bash parameter expansion; otherwise `qwen-unknown`.
5. `transcript_file` is the transcript path with `$HOME` replaced by `~`.
6. Build a success map from tool results:

```jq
map(
  select(.type=="tool_result" and .toolCallResult.status=="success")
  | {key:.toolCallResult.callId, value:.timestamp}
) | from_entries
```

7. Extract candidate file mutations from assistant function calls:

```jq
select(.type=="assistant") as $line
| $line.message.parts[]?
| select(.functionCall?)
| .functionCall as $call
| select($call.name=="edit" or $call.name=="write_file")
| {
    session_id: $line.sessionId,
    cwd: $line.cwd,
    call_id: $call.id,
    tool_name: $call.name,
    file_path: ($call.args.file_path // ""),
    change_type: (if $call.name=="edit" then "update" else "write" end)
  }
```

8. Keep a candidate only when `file_path` is non-empty and the success map has
   `call_id`.
9. Use the matched `tool_result.timestamp` as `occurred_at`. Do not use ingest
   time and do not use the assistant line timestamp for file mutation events.
10. Never serialize `edit.old_string`, `edit.new_string`,
    `write_file.content`, or any `functionResponse` result payload.

Prompt extraction is enabled only by `--include-prompts`:

```jq
select(.type=="user" and .message.role=="user")
| {
    ts: .timestamp,
    message: ([.message.parts[]?.text // empty] | join("\n"))
  }
```

Skip empty prompt text. Prompt text may be truncated to 4000 bytes, matching the
Codex rollout adapter's conservative search-visible text cap.

## Exact event shape

All writes must go through `MEMOIRE_QUEUE_BIN`, defaulting to:

```text
$ROOT/bin/memoire-queue
```

The queue writer invocation surface is exactly:

```text
-project
-session-id
-source-adapter
-event-type
-cwd
-occurred-at
-source-event-id
-payload-file
-text
```

Use:

```text
source_adapter="qwen-cli"
```

Successful mutation events use:

```text
event_type="qwen_FileEdit"
```

Prompt parity events use:

```text
event_type="qwen_UserMessage"
```

Successful mutation payloads must have only paths and provenance:

```json
{
  "tool_name": "edit",
  "tool_input": {
    "file_path": "/tmp/qwen-smoke/alpha.txt",
    "change_type": "update"
  },
  "call_id": "call_qwen_edit_success",
  "transcript_file": "~/..."
}
```

For `write_file`, `tool_name` must be `write_file` and `change_type` must be
`write`. `tool_input.file_path` is required because `GET /v1/events/tool-window`
derives `paths_touched` from that field.

Prompt payloads must be:

```json
{
  "message": "qwenpromptneedle ...",
  "transcript_file": "~/..."
}
```

Mutation event text format:

```text
Qwen edit update /tmp/qwen-smoke/alpha.txt
Qwen write_file write /tmp/qwen-smoke/beta.md
```

Prompt event text is the prompt text, truncated to the prompt cap.

### Dry-run output

With `--dry-run`, write nothing to the queue. Emit one compact JSON object per
event on stdout using the same wrapper shape as the Codex rollout adapter's
`jq -c` dry-run branch:

```text
project
session_id
event_type
occurred_at
source_event_id
text
payload
```

The mutation event dry-run line must use this compact JSON shape, with no spaces
after colons:

```json
{"project":"qwen-smoke-project","session_id":"qwen-smoke-session-20260707","event_type":"qwen_FileEdit","occurred_at":"2026-07-06T22:50:10.000Z","source_event_id":"qwen-fileedit-<sha256>","text":"Qwen edit update /tmp/qwen-smoke/alpha.txt","payload":{"tool_name":"edit","tool_input":{"file_path":"/tmp/qwen-smoke/alpha.txt","change_type":"update"},"call_id":"call_qwen_edit_success","transcript_file":"~/..."}}
```

The prompt event dry-run line must use this compact JSON shape, with no spaces
after colons:

```json
{"project":"qwen-smoke-project","session_id":"qwen-smoke-session-20260707","event_type":"qwen_UserMessage","occurred_at":"2026-07-06T22:50:00.000Z","source_event_id":"qwen-usermessage-<sha256>","text":"qwenpromptneedle please edit alpha and write beta while leaving the failed file alone","payload":{"message":"qwenpromptneedle please edit alpha and write beta while leaving the failed file alone","transcript_file":"~/..."}}
```

The smoke pins the compact substrings:

```text
"source_event_id":"qwen-fileedit-
"source_event_id":"qwen-usermessage-
```

## Idempotency

Mutation `source_event_id` is deterministic per `(sessionId, callId, path)`:

```bash
fingerprint_input="$session_id:$call_id:$path"
fingerprint="$(printf '%s' "$fingerprint_input" | sha256sum)"
fingerprint="${fingerprint%% *}"
source_event_id="qwen-fileedit-$fingerprint"
```

The exact source-event-id prefix pinned by the smoke test is:

```text
qwen-fileedit-
```

Prompt `source_event_id` is deterministic per `(sessionId, timestamp, message)`:

```bash
fingerprint_input="$session_id:$ts:$message"
fingerprint="$(printf '%s' "$fingerprint_input" | sha256sum)"
fingerprint="${fingerprint%% *}"
source_event_id="qwen-usermessage-$fingerprint"
```

The exact prompt source-event-id prefix pinned by the smoke test is:

```text
qwen-usermessage-
```

Re-scans need no checkpoint. The store dedups on source adapter plus source
event id.

## Failure accounting

Copy the Codex rollout adapter's fail-open and honest-exit behavior in spirit:

- unreadable or unparsable file increments `failed_files`;
- queue write failure increments `failed_events`;
- any file with queue write failures counts as a failed file;
- final exit is nonzero if `failed_files > 0 || failed_events > 0`;
- timers and burns must not read partial capture as success.

Every log line must use this prefix:

```text
memoire-capture-qwen-rollout:
```

The smoke test pins these queue-failure log fragments:

```text
memoire-capture-qwen-rollout: queue write failed for /tmp/qwen-smoke/alpha.txt
memoire-capture-qwen-rollout: queue write failed for /tmp/qwen-smoke/beta.md
failed_events=2
```

The final accounting log line must use this shape:

```text
memoire-capture-qwen-rollout: done: files=<n> failed_files=<n> events=<n> failed_events=<n>
```

## Smoke oracle

The exact command is:

```bash
command make qwen-rollout-smoke
```

The direct test path is:

```bash
./tests/foreign/qwen-rollout-capture-smoke.sh
```

The synthetic fixture is:

```text
tests/foreign/fixtures/qwen-rollout-sample.jsonl
```

The corrupt-after-first-line fixture is:

```text
tests/foreign/fixtures/qwen-rollout-corrupt-after-first-line.jsonl
```

The smoke runs on port:

```text
37792
```

The smoke-pinned session, project, event types, paths, prompt needle, and
timestamps are:

```text
SESSION_ID="qwen-smoke-session-20260707"
project="qwen-smoke-project"
event_type="qwen_FileEdit"
event_type="qwen_UserMessage"
source_adapter="qwen-cli"
/tmp/qwen-smoke/alpha.txt
/tmp/qwen-smoke/beta.md
/tmp/qwen-smoke/should-not-appear.txt
qwenpromptneedle
must not be queued
2026-07-06T22:50:10Z
```

The successful mutation tool names pinned by the smoke are:

```text
edit,write_file
```

The successful mutation change types pinned by the smoke are:

```text
update,write
```

The cage stub must initially fail the smoke for this reason:

```text
Memoire qwen rollout smoke failed: expected 2 qwen_FileEdit events, got:
```

That initial failure is correct until the implementer fills in the parser. It
must be a 0-event failure, not a jq, API, shell, or harness error.

The smoke also pins these assertion strings verbatim:

```text
corrupt transcript ingest exited 0
corrupt transcript summary mismatch:
paths_touched mismatch:
payload leaked file content
search leaked file content
failed edit was ingested
occurred_at not preserved from transcript:
bounded window should contain 2 events, got
window before mutation time should be empty, got
tool_name mismatch:
change_type mismatch:
re-ingest not idempotent:
expected 1 prompt event, got
prompt text not search-visible
dry-run missing qwen-fileedit source_event_id prefix
dry-run missing qwen-usermessage source_event_id prefix
ingester exited 0 despite every queue write failing
missing alpha queue-failure log
missing beta queue-failure log
missing failed_events=2 accounting
capture script uses commands outside allowlist:
memoire qwen rollout capture smoke passed
```

The corrupt-after-first-line oracle runs the valid fixture and the corrupt
fixture together. The script must process the valid file, queue its two
`qwen_FileEdit` events and one `qwen_UserMessage` event, detect the corrupt
second transcript as unparsable, log this exact summary line, and exit nonzero:

```text
memoire-capture-qwen-rollout: done: files=1 failed_files=1 events=3 failed_events=0
```

The dependency allowlist oracle rejects command-position invocations outside
the contracted set. The smoke currently pins these forbidden command names
because they are the judge J1 drift examples:

```text
sort
mktemp
rm
awk
head
basename
```

The smoke also rejects future command-position drift for common external
utilities outside the allowlist, including:

```text
sed
cut
tail
xargs
python
python3
perl
node
ruby
go
date
tr
wc
tee
readlink
realpath
```

If the implementation truly needs any external command beyond `bash`, `jq`,
`sha256sum`, `find`, and `memoire-queue`, the contract allowlist must be
amended explicitly before the implementation may use it.

The implementer must write verification evidence to:

```text
tmp/qwen-rollout-capture-evidence.txt
```

The evidence file must include the exact `command make qwen-rollout-smoke`
result and enough output to prove success or a real blocker.

## Mapping decision

The cage-owned mapping is:

- successful `edit` and `write_file` transcript mutations become
  `qwen_FileEdit`;
- prompt parity events emitted with `--include-prompts` become
  `qwen_UserMessage`;
- `source_adapter` is `qwen-cli`;
- payload `tool_name` is the real qwen tool name, `edit` or `write_file`;
- `edit` uses `change_type="update"`;
- `write_file` uses `change_type="write"` because the transcript does not prove
  whether the file existed before the call;
- mutation `occurred_at` is the matched successful `tool_result.timestamp`
  because that is the success boundary equivalent to Codex
  `patch_apply_end.success==true`.

This mapping is intentionally parallel to Codex's `codex_PatchApply` and
`codex_UserMessage`, while preserving qwen's native tool names in payload.

## Fence allowlist

The implementer may modify ONLY:

- `scripts/memoire-capture-qwen-rollout.sh`
- `tmp/qwen-rollout-capture-evidence.txt`

Any diff touching any other file is a fence breach and must fail review.
