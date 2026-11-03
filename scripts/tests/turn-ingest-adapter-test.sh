#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail

# turn-ingest-adapter-test.sh — regression test for the codex/qwen
# assistant-turn adapters (turn-ingest campaign Phase 1, slice 2). Pure
# --dry-run over the committed smoke fixtures: no API, no queue writes, no
# provider. The live end-to-end path is covered by the foreign tests
# (make codex-rollout-capture-test / qwen-rollout-smoke).
#
# Covered (codex, capture-all): session_meta → thread facts
# only; legacy event_msg agent_message → codex_record with legacy_ui (no
# turn); a task_started … task_complete pair with an assistant response_item
# → one codex_assistant_turn whose text is the joined assistant text;
# deterministic codex:<thread>:<ordinal> ids with the ln<line> fallback;
# capture-all has NO text cap (MEMOIRE_BACKFILL_TEXT_MAX is ignored); the
# session cwd resolves through the registry.
#
# Covered (qwen, unchanged): agent_message → qwen_assistant_turn; non-thought
# text parts only; deterministic source_event_ids; the MEMOIRE_BACKFILL_TEXT_MAX
# cap truncates the event text and flags text_truncated in the payload.
#
# Part 2 (streaming capture, Fibery #342) covers the CC transcript adapter's
# byte cursor against a stub queue writer in a scratch MEMOIRE_HOME: first
# pass full, second pass silent, partial trailing lines held back, truncate
# rescan, the --quiet-for/--since scan windows, --emit-sessions, cursor-seeded
# session-home attribution across a roaming chunk, and the invariant that a
# flagless pass writes exactly the queue calls it always did.

# Part 2 also covers sub-agent text capture (operator ruling 2026-10-02): the
# subagents/ tree at any depth, inline sidechain lines, meta-derived payload
# keys, compaction agents as claude_compact_summary, hook-context attachment
# lines never queued, parent-home attribution in every pass order, the fork
# skip under <forkId>/subagents/, --tail on a sub-agent file, the per-file scan
# windows and --emit-sessions counting sub-agent turns under the parent session.
#
# Part 4 covers the long-turn rule (operator ruling 2026-10-02: a turn over 6000
# characters is a first event identical to the one the reader at origin/main
# b383646 emitted, pinned by golden skeletons, plus ONE
# continuation event cc-*:<sid>:<uuid>:rest sent through -text-file), the fixed
# split point, --from-start repair and its idempotence, --emit-sessions counting
# turns only, the --dry-run marking, and the sub-agent home repair once the
# parent transcript appears.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CODEX_SCRIPT="$ROOT/scripts/memoire-capture-codex-rollout.sh"
QWEN_SCRIPT="$ROOT/scripts/memoire-capture-qwen-rollout.sh"
CODEX_FIXTURE="$ROOT/tests/foreign/fixtures/codex-rollout-sample.jsonl"
QWEN_FIXTURE="$ROOT/tests/foreign/fixtures/qwen-rollout-sample.jsonl"

command -v jq >/dev/null || { echo "SKIP: jq not available"; exit 0; }

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

# The codex adapter keeps its working copies under $MEMOIRE_HOME/tmp: point it
# at a scratch home, never at the operator's.
SCRATCH_MH="$(mktemp -d)"
export MEMOIRE_HOME="$SCRATCH_MH"

# --- codex: capture-all mapping and the assistant-turn projection ------------
# Session meta → facts; every record with an event class → a capture-all
# event; the task_started … task_complete pair with an assistant message →
# exactly one codex_assistant_turn.
# The committed fixture is an old ordinal-less file (legacy UI events only);
# the turn fixture is built here in the shape of a current file.
codex_out="$("$CODEX_SCRIPT" --dry-run "$CODEX_FIXTURE" 2>/dev/null)"
[[ "$(jq -s '[.[] | select(.event_type=="codex_thread_facts")] | length' <<<"$codex_out")" -ge 1 ]] \
  || fail "codex fixture must yield a thread facts event"
jq -e '.payload.parent_thread_id == null' \
  <<<"$(jq -c 'select(.event_type=="codex_thread_facts")' <<<"$codex_out")" >/dev/null \
  || fail "codex facts must give parent_thread_id null when the file records none"
CODEX_THREAD="$(jq -r 'select(.type=="session_meta") | .payload.id' "$CODEX_FIXTURE")"

# Legacy event_msg agent_message → codex_record flagged legacy_ui, never a
# turn; the ordinal-less legacy file falls back to ln<line> ids.
jq -e 'select(.source_event_id=="codex:'"$CODEX_THREAD"':ln3")
       | .event_type == "codex_record" and .payload.legacy_ui == true' <<<"$codex_out" >/dev/null \
  || fail "legacy agent_message must be a codex_record with legacy_ui and an ln<line> id"
[[ "$(jq -s '[.[] | select(.event_type=="codex_assistant_turn")] | length' <<<"$codex_out")" == 0 ]] \
  || fail "a legacy file without turn markers must yield no assistant turn"

TURN_FIXTURE="$(mktemp)"
DUP_FIXTURE="$(mktemp)"
trap 'rm -f "$TURN_FIXTURE" "$DUP_FIXTURE"; rm -rf "$SCRATCH_MH"' EXIT
{
  jq -c 'select(.type=="session_meta")' "$CODEX_FIXTURE"
  printf '%s\n' '{"timestamp":"2026-07-02T00:13:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn_smoke"},"ordinal":1}'
  printf '%s\n' '{"timestamp":"2026-07-02T00:13:30.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"codexturnneedle applied the patch; alpha added and beta moved to gamma."}]},"ordinal":2}'
  printf '%s\n' '{"timestamp":"2026-07-02T00:14:00.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn_smoke","last_agent_message":"codexturnneedle applied the patch"},"ordinal":3}'
} > "$TURN_FIXTURE"
codex_turns="$("$CODEX_SCRIPT" --dry-run "$TURN_FIXTURE" 2>/dev/null | jq -c 'select(.event_type=="codex_assistant_turn")')"
[[ "$(grep -c . <<<"$codex_turns")" == 1 ]] || fail "codex turn fixture must yield exactly 1 assistant turn"
grep -q "codexturnneedle" <<<"$codex_turns" || fail "codex turn text missing from dry-run output"
[[ "$(jq -r '.source_event_id' <<<"$codex_turns")" == "codex:${CODEX_THREAD}:t3" ]] \
  || fail "codex turn id must be codex:<thread>:t<ordinal of the closing record>"
jq -e '.payload.closed_by == "terminal"' <<<"$codex_turns" >/dev/null \
  || fail "a task_complete-closed turn must carry closed_by terminal"
jq -e '.payload.inherited != true' <<<"$codex_turns" >/dev/null \
  || fail "own records must not be flagged inherited"

# Determinism: a second dry-run emits the identical source_event_id.
codex_id1="$(jq -r '.source_event_id' <<<"$codex_turns")"
codex_id2="$("$CODEX_SCRIPT" --dry-run "$TURN_FIXTURE" 2>/dev/null \
  | jq -r 'select(.event_type=="codex_assistant_turn") | .source_event_id')"
[[ "$codex_id1" == "$codex_id2" ]] || fail "codex turn source_event_id not deterministic"

# Duplicate turns stay distinct: the closing record's ordinal is part of the
# identity — two identical turns must yield two events with different
# source_event_ids.
{
  jq -c 'select(.type=="session_meta")' "$CODEX_FIXTURE"
  printf '%s\n' '{"timestamp":"2026-07-02T00:10:30.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn_d1"},"ordinal":1}'
  printf '%s\n' '{"timestamp":"2026-07-02T00:10:31.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"same turn twice"}]},"ordinal":2}'
  printf '%s\n' '{"timestamp":"2026-07-02T00:10:32.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn_d1"},"ordinal":3}'
  printf '%s\n' '{"timestamp":"2026-07-02T00:11:30.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn_d2"},"ordinal":4}'
  printf '%s\n' '{"timestamp":"2026-07-02T00:11:31.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"same turn twice"}]},"ordinal":5}'
  printf '%s\n' '{"timestamp":"2026-07-02T00:11:32.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn_d2"},"ordinal":6}'
} > "$DUP_FIXTURE"
dup_ids="$("$CODEX_SCRIPT" --dry-run "$DUP_FIXTURE" 2>/dev/null \
  | jq -r 'select(.event_type=="codex_assistant_turn") | .source_event_id')"
[[ "$(grep -c . <<<"$dup_ids")" == 2 ]] || fail "duplicate turns must yield two turn events"
[[ "$(sort -u <<<"$dup_ids" | grep -c .)" == 2 ]] || fail "identical duplicate turns must get distinct source_event_ids"

# Capture-all has NO text cap: MEMOIRE_BACKFILL_TEXT_MAX must be ignored, not
# inherited from the backfill adapter.
codex_uncapped="$(MEMOIRE_BACKFILL_TEXT_MAX=5 "$CODEX_SCRIPT" --dry-run "$TURN_FIXTURE" 2>/dev/null \
  | jq -c 'select(.event_type=="codex_assistant_turn")')"
grep -q "codexturnneedle applied the patch" <<<"$codex_uncapped" \
  || fail "codex capture-all must ignore MEMOIRE_BACKFILL_TEXT_MAX"

# --- qwen: non-thought text parts become qwen_assistant_turn ------------------
qwen_out="$("$QWEN_SCRIPT" --dry-run "$QWEN_FIXTURE" 2>/dev/null)"
qwen_turns="$(jq -c 'select(.event_type=="qwen_assistant_turn")' <<<"$qwen_out")"
[[ "$(grep -c . <<<"$qwen_turns")" == 1 ]] || fail "qwen fixture must yield exactly 1 assistant turn"
grep -q "qwenturnneedle" <<<"$qwen_turns" || fail "qwen turn text missing from dry-run output"
grep -q "qwenthoughtneedle" <<<"$qwen_turns" && fail "qwen thought part must never emit"
[[ "$(jq -r '.source_event_id' <<<"$qwen_turns")" == "qwen-turn:qwen-smoke-session-20260707:qwen-line-0002" ]] \
  || fail "qwen turn source_event_id must be deterministic per (session, line uuid)"
jq -e '.payload.backfill_generation | length > 0' <<<"$qwen_turns" >/dev/null \
  || fail "qwen turn payload missing backfill_generation"
jq -e '.payload.text_truncated == false' <<<"$qwen_turns" >/dev/null \
  || fail "short qwen turn must not be flagged truncated"

qwen_capped="$(MEMOIRE_BACKFILL_TEXT_MAX=14 "$QWEN_SCRIPT" --dry-run "$QWEN_FIXTURE" 2>/dev/null \
  | jq -c 'select(.event_type=="qwen_assistant_turn")')"
[[ "$(jq -r '.text' <<<"$qwen_capped")" == "qwenturnneedle" ]] \
  || fail "qwen turn text not capped at MEMOIRE_BACKFILL_TEXT_MAX"
jq -e '.payload.text_truncated == true' <<<"$qwen_capped" >/dev/null \
  || fail "capped qwen turn must be flagged truncated"

# --- resolver wiring: session cwd resolves through the registry ---------------
# Proves both rollout adapters run the shared resolver on the session cwd
# (codex fixture cwd = /tmp/codex-smoke-project, qwen = /tmp/qwen-smoke-project)
# rather than taking a bare basename.
REG="$(mktemp --suffix=.json)"
trap 'rm -f "$TURN_FIXTURE" "$DUP_FIXTURE" "$REG"; rm -rf "$SCRATCH_MH"' EXIT
cat > "$REG" <<'JSON'
{
  "schema": "liaison-memoire-projects/1",
  "projects": {
    "codex-mapped": { "display": "codex-smoke-project", "paths": ["/tmp/codex-smoke-project"] },
    "qwen-mapped":  { "display": "qwen-smoke-project",  "paths": ["/tmp/qwen-smoke-project"] }
  }
}
JSON

codex_proj="$(MEMOIRE_PROJECTS_REGISTRY="$REG" "$CODEX_SCRIPT" --dry-run "$TURN_FIXTURE" 2>/dev/null \
  | jq -r 'select(.event_type=="codex_assistant_turn") | .project' | head -1)"
[[ "$codex_proj" == "codex-mapped" ]] \
  || fail "codex adapter must resolve session cwd through the registry (got '$codex_proj')"

qwen_proj="$(MEMOIRE_PROJECTS_REGISTRY="$REG" "$QWEN_SCRIPT" --dry-run "$QWEN_FIXTURE" 2>/dev/null \
  | jq -r 'select(.event_type=="qwen_assistant_turn") | .project' | head -1)"
[[ "$qwen_proj" == "qwen-mapped" ]] \
  || fail "qwen adapter must resolve session cwd through the registry (got '$qwen_proj')"

# --project override still beats the registry on both adapters.
codex_forced="$(MEMOIRE_PROJECTS_REGISTRY="$REG" "$CODEX_SCRIPT" --dry-run --project forced "$TURN_FIXTURE" 2>/dev/null \
  | jq -r 'select(.event_type=="codex_assistant_turn") | .project' | head -1)"
[[ "$codex_forced" == "forced" ]] || fail "codex --project override must beat the registry"

# ============================================================================
# Part 2 — Claude Code transcript adapter: the --tail byte cursor (Fibery #342)
# ============================================================================
CC_SCRIPT="$ROOT/scripts/memoire-capture-claude-transcripts.sh"
command -v flock >/dev/null || {
  echo "SKIP: flock not available (cursor cases)"
  echo "turn-ingest-adapter-test: OK"
  exit 0
}

WORK="$(mktemp -d)"
trap 'rm -f "$TURN_FIXTURE" "$DUP_FIXTURE" "$REG"; rm -rf "$WORK" "$SCRATCH_MH"' EXIT
mkdir -p "$WORK/bin" "$WORK/home" "$WORK/scan" "$WORK/tf"
QLOG="$WORK/queue.log"
: > "$QLOG"

# Stub queue writer. argv is recorded NUL-SEPARATED: a -text value can carry
# newlines, so a line-per-call log could not be compared byte for byte.
# Like the real writer it rejects -text together with -text-file (the rejection
# is also written to $WORK/stub-violation, which the test asserts absent at the
# end). A pattern in $WORK/qfail makes every call whose argv contains it fail
# (exit 1, nothing recorded) — the failed-write cases.
cat > "$WORK/bin/queue" <<FAKE
#!/usr/bin/env bash
has_text= has_file=
for a in "\$@"; do
  [[ "\$a" == -text ]] && has_text=1
  [[ "\$a" == -text-file ]] && has_file=1
done
if [[ -n "\$has_text" && -n "\$has_file" ]]; then
  echo "use only one of -text or -text-file" >&2
  echo violation >> "$WORK/stub-violation"
  exit 1
fi
if [[ -s "$WORK/qfail" ]] && printf '%s\\0' "\$@" | grep -aqF -- "\$(cat "$WORK/qfail")"; then
  echo "stub: forced failure" >&2
  exit 1
fi
{ printf 'CALL'; printf '\\0%s' "\$@"; printf '\\n'; } >> "$QLOG"
# A -text-file is removed by the reader after the write: keep a copy by name.
prev=
for a in "\$@"; do
  [[ "\$prev" == -text-file ]] && cp -- "\$a" "$WORK/tf/\$(basename "\$a")"
  prev="\$a"
done
FAKE
chmod +x "$WORK/bin/queue"

cat > "$WORK/registry.json" <<'JSON'
{
  "schema": "liaison-memoire-projects/1",
  "projects": {
    "cursor-home": { "display": "cursor-home", "paths": ["/tmp/memoire-cursor-home"] },
    "cursor-roam": { "display": "cursor-roam", "paths": ["/tmp/memoire-cursor-roam"] },
    "sa-parent":   { "display": "sa-parent",   "paths": ["/tmp/memoire-sa-parent"] },
    "sa-wt":       { "display": "sa-wt",       "paths": ["/tmp/memoire-sa-wt"] },
    "sa-other":    { "display": "sa-other",    "paths": ["/tmp/memoire-sa-other"] }
  }
}
JSON

# EVERY invocation pins a scratch home, cursor dir, registry and queue bin: a
# missing pin would write cursors into the operator's live ~/.memoire and
# resolve against the live registry.
cc_run() { # cursor-dir args...
  local cdir="$1"
  shift
  env MEMOIRE_HOME="$WORK/home" \
    MEMOIRE_CAPTURE_CURSOR_DIR="$cdir" \
    MEMOIRE_QUEUE_BIN="$WORK/bin/queue" \
    MEMOIRE_PROJECTS_REGISTRY="$WORK/registry.json" \
    MEMOIRE_PROJECT= \
    bash "$CC_SCRIPT" "$@"
}
cc_line() { # uuid minute session cwd text
  printf '{"type":"assistant","uuid":"%s","timestamp":"2026-09-08T10:%02d:00Z","sessionId":"%s","cwd":"%s","message":{"content":[{"type":"text","text":"%s"}]}}\n' \
    "$1" "$2" "$3" "$4" "$5"
}
qreset() { : > "$QLOG"; }
qcalls() { grep -ac '^CALL' "$QLOG" 2>/dev/null || true; }
# `|| true`: under pipefail, `qrender | grep -q` fails when grep exits early
# and tr dies of SIGPIPE, for a log that DOES hold the pattern.
qrender() { tr '\0' ' ' < "$QLOG" || true; }
cursor_of() { ls "$1"/*.json 2>/dev/null | head -1; }

# --- (a) first --tail pass on a cursorless file captures EVERYTHING ----------
T="$WORK/t.jsonl"
{
  cc_line u1 1 sess-x /tmp/memoire-cursor-home "turn one"
  cc_line u2 2 sess-x /tmp/memoire-cursor-home "turn two"
} > "$T"
qreset
cc_run "$WORK/cursors/a" --tail "$T" 2>/dev/null || fail "tail first pass must exit 0"
[[ "$(qcalls)" == "2" ]] || fail "first --tail pass must queue every assistant turn (got $(qcalls))"
CUR="$(cursor_of "$WORK/cursors/a")"
[[ -n "$CUR" ]] || fail "first --tail pass must write a cursor"
[[ "$(stat -c '%a' "$CUR")" == "600" ]] || fail "cursor file must be 0600"
[[ "$(stat -c '%a' "$WORK/cursors/a")" == "700" ]] || fail "cursor dir must be 0700"
[[ "$(jq -r '.offset' "$CUR")" == "$(stat -c '%s' "$T")" ]] \
  || fail "cursor offset must end at the last newline of a fully-consumed file"
[[ "$(jq -r '.dev_inode' "$CUR")" == "$(stat -c '%d:%i' "$T")" ]] || fail "cursor must record dev:inode"
[[ "$(jq -r '.session_homes["sess-x"]' "$CUR")" == "/tmp/memoire-cursor-home" ]] \
  || fail "cursor must persist the session home under its own session id"
qrender | grep -q 'cc-turn:sess-x:u1' || fail "first pass missed the first turn"
qrender | grep -q 'cc-turn:sess-x:u2' || fail "first pass missed the second turn"

# --- (b) second pass over an unchanged file queues NOTHING -------------------
BEFORE="$(cat "$CUR")"
qreset
cc_run "$WORK/cursors/a" --tail "$T" 2>/dev/null || fail "second --tail pass must exit 0"
[[ "$(qcalls)" == "0" ]] || fail "second --tail pass must queue nothing (got $(qcalls))"
[[ "$(cat "$CUR")" == "$BEFORE" ]] || fail "an unchanged file must leave the cursor untouched"

# --- (c) one complete + one PARTIAL appended line; (g) roaming attribution ---
cc_line u3 3 sess-x /tmp/memoire-cursor-roam "turn three" >> "$T"
printf '{"type":"assistant","uuid":"u4","timestamp":"2026-09-08T10:04:00Z","sessionId":"sess-x","cwd":"/tmp/memoire-cursor-roam","message":{"content":[{"type":"text","text":"half writ' >> "$T"
qreset
cc_run "$WORK/cursors/a" --tail "$T" 2>/dev/null || fail "third --tail pass must exit 0"
[[ "$(qcalls)" == "1" ]] || fail "only the COMPLETE appended turn may queue (got $(qcalls))"
qrender | grep -q 'cc-turn:sess-x:u3' || fail "the complete appended turn must queue"
qrender | grep -q 'cc-turn:sess-x:u4' && fail "a partial trailing line must never queue"
PARTIAL_LEN=$(( $(stat -c '%s' "$T") - $(jq -r '.offset' "$CUR") ))
[[ "$PARTIAL_LEN" -gt 0 ]] || fail "cursor must stop BEFORE the partial trailing line"
# (g) the roaming chunk carries cwd=cursor-roam but attributes to the cursor's
# session_home, exactly as the first-line rule does inside one pass.
qrender | grep -q -- '-project cursor-home ' || fail "tail chunk must attribute via the cursor's session_home"
qrender | grep -q -- '-cwd /tmp/memoire-cursor-roam ' || fail "the per-event cwd must stay the event's own"

# --- (d) truncation and inode replacement force a full rescan ----------------
cc_line u9 9 sess-x /tmp/memoire-cursor-home "turn nine" > "$T"
qreset
cc_run "$WORK/cursors/a" --tail "$T" 2>/dev/null || fail "rescan pass must exit 0"
[[ "$(qcalls)" == "1" ]] || fail "a file smaller than its cursor must be rescanned whole (got $(qcalls))"
qrender | grep -q 'cc-turn:sess-x:u9' || fail "rescan must capture the rewritten content"
{
  cc_line u10 10 sess-x /tmp/memoire-cursor-home "turn ten"
  cc_line u11 11 sess-x /tmp/memoire-cursor-home "turn eleven"
} > "$WORK/replacement.jsonl"
mv -f "$WORK/replacement.jsonl" "$T"
qreset
cc_run "$WORK/cursors/a" --tail "$T" 2>/dev/null || fail "inode-change pass must exit 0"
[[ "$(qcalls)" == "2" ]] || fail "a new inode must be rescanned whole (got $(qcalls))"

# --- (d2) cursor identity: reboot, replaced copy, legacy cursor, same inode --
T="$WORK/id.jsonl"
{
  cc_line i1 1 sess-id /tmp/memoire-cursor-home "id one"
  cc_line i2 2 sess-id /tmp/memoire-cursor-home "id two"
} > "$T"
cc_run "$WORK/cursors/id" --tail "$T" 2>/dev/null || fail "identity first pass must exit 0"
CUR="$(cursor_of "$WORK/cursors/id")"
INO="$(stat -c %i "$T")"
[[ "$(jq -r '.file_id' "$CUR")" == "$(stat -f -c %i "$T"):$INO" ]] || fail "cursor must record file_id <f_fsid>:<inode>"
[[ "$(jq -r '.head_len' "$CUR")" == "$(stat -c %s "$T")" \
   && "$(jq -r '.head_sha' "$CUR")" == "$(sha256sum < "$T" | cut -d' ' -f1)" ]] \
  || fail "cursor must fingerprint the first min(4096, offset) bytes"
# A reboot: st_dev renumbered, and a filesystem whose f_fsid moved too.
jq -c --arg i "$INO" '.dev_inode = "999:" + $i | .file_id = "ffff:" + $i' "$CUR" > "$CUR.x" && mv -f "$CUR.x" "$CUR"
qreset
cc_run "$WORK/cursors/id" --tail "$T" 2>"$WORK/id.err" || fail "post-reboot pass must exit 0"
[[ "$(qcalls)" == "0" ]] || fail "an idle transcript must queue nothing after a reboot (got $(qcalls))"
[[ "$(jq -r '.file_id' "$CUR")" == "$(stat -f -c %i "$T"):$INO" ]] || fail "a matching cursor with another file_id must be rewritten"
grep -q 'cursor does not match' "$WORK/id.err" && fail "a matching cursor must not be logged as rejected"
# A replaced, appended copy (a new inode, as a Syncthing receiver sees it).
{ cat "$T"; cc_line i3 3 sess-id /tmp/memoire-cursor-home "id three"; } > "$T.new"
mv -f "$T.new" "$T"
qreset
cc_run "$WORK/cursors/id" --tail "$T" 2>/dev/null || fail "replaced-copy pass must exit 0"
[[ "$(qcalls)" == "1" ]] && qrender | grep -q 'cc-turn:sess-id:i3' \
  || fail "a replaced appended copy must queue only the appended turn (got $(qcalls))"
# A cursor written before file_id: accepted on its inode, then rewritten.
INO="$(stat -c %i "$T")"
jq -c --arg i "$INO" 'del(.file_id, .head_len, .head_sha) | .dev_inode = "999:" + $i' "$CUR" > "$CUR.x" && mv -f "$CUR.x" "$CUR"
qreset
cc_run "$WORK/cursors/id" --tail "$T" 2>/dev/null || fail "legacy-cursor pass must exit 0"
[[ "$(qcalls)" == "0" && "$(jq -r '.file_id' "$CUR")" == "$(stat -f -c %i "$T"):$INO" ]] \
  || fail "a legacy cursor whose inode matches must be accepted and rewritten (got $(qcalls))"
jq -c 'del(.file_id, .head_len, .head_sha) | .dev_inode = "999:1"' "$CUR" > "$CUR.x" && mv -f "$CUR.x" "$CUR"
qreset
cc_run "$WORK/cursors/id" --tail "$T" 2>"$WORK/id.err" || fail "legacy-mismatch pass must exit 0"
[[ "$(qcalls)" == "3" ]] && grep -q 'cursor does not match the file, full pass' "$WORK/id.err" \
  || fail "a legacy cursor naming another inode must give a logged full pass (got $(qcalls))"
# Other content under the same inode, longer than the offset.
{
  cc_line j1 1 sess-id /tmp/memoire-cursor-home "other content one, longer"
  cc_line j2 2 sess-id /tmp/memoire-cursor-home "other content two, longer"
  cc_line j3 3 sess-id /tmp/memoire-cursor-home "other content three, longer"
} >| "$T"
[[ "$(stat -c %i "$T")" == "$INO" ]] || fail "setup: an in-place rewrite must keep the inode"
qreset
cc_run "$WORK/cursors/id" --tail "$T" 2>/dev/null || fail "same-inode rewrite pass must exit 0"
[[ "$(qcalls)" == "3" ]] || fail "other content under the same inode must be read whole (got $(qcalls))"
# A copy that shares the first 4 KiB but changed its last consumed turn (a sync
# conflict winner), renamed over the file, then appended: read whole.
PAD="$(printf 'x%.0s' {1..3000})"
{
  cc_line k1 1 sess-id /tmp/memoire-cursor-home "$PAD"
  cc_line k2 2 sess-id /tmp/memoire-cursor-home "$PAD"
  cc_line k3 3 sess-id /tmp/memoire-cursor-home "$PAD"
} >| "$T"
cc_run "$WORK/cursors/id" --tail "$T" 2>/dev/null || fail "padded pass must exit 0"
{
  head -n 2 "$T"
  cc_line k3b 3 sess-id /tmp/memoire-cursor-home "$PAD"
  cc_line k4 4 sess-id /tmp/memoire-cursor-home "after the conflict"
} > "$T.new"
cmp -s <(head -c 4096 "$T") <(head -c 4096 "$T.new") || fail "setup: the conflict copy must share the first 4 KiB"
mv -f "$T.new" "$T"
qreset
cc_run "$WORK/cursors/id" --tail "$T" 2>"$WORK/id.err" || fail "conflict-copy pass must exit 0"
qrender | grep -q 'cc-turn:sess-id:k3b' && grep -q 'cursor does not match' "$WORK/id.err" \
  || fail "a copy that diverged after a shared head must be read whole (got $(qcalls))"

# --- (e) --quiet-for and --since bound a DIRECTORY scan ---------------------
cc_line s1 1 sess-fresh /tmp/memoire-cursor-home "fresh turn" > "$WORK/scan/fresh.jsonl"
cc_line s2 2 sess-old /tmp/memoire-cursor-home "old turn" > "$WORK/scan/old.jsonl"
touch -d '3 days ago' "$WORK/scan/old.jsonl"
qreset
cc_run "$WORK/cursors/e1" --tail --quiet-for 3600 "$WORK/scan" 2>/dev/null || fail "--quiet-for pass must exit 0"
qrender | grep -q 'sess-old' || fail "--quiet-for must still capture a settled file"
qrender | grep -q 'sess-fresh' && fail "--quiet-for must skip a file younger than the window"
qreset
cc_run "$WORK/cursors/e2" --tail --since 86400 "$WORK/scan" 2>/dev/null || fail "--since pass must exit 0"
qrender | grep -q 'sess-fresh' || fail "--since must capture a file inside the window"
qrender | grep -q 'sess-old' && fail "--since must skip a file older than the window"

# --- (f) --emit-sessions prints exactly one TSV row, and nothing else --------
EMIT="$WORK/emit.jsonl"
{
  cc_line f1 1 sess-f /tmp/memoire-cursor-home "emit one"
  cc_line f2 2 sess-f /tmp/memoire-cursor-home "emit two"
} > "$EMIT"
qreset
EMIT_OUT="$(cc_run "$WORK/cursors/f" --tail --emit-sessions "$EMIT" 2>/dev/null)"
[[ "$EMIT_OUT" == "$(printf 'cursor-home\tsess-f\t2')" ]] \
  || fail "--emit-sessions stdout must be exactly one TSV row (got: $EMIT_OUT)"
EMIT_OUT2="$(cc_run "$WORK/cursors/f" --tail --emit-sessions "$EMIT" 2>/dev/null)"
[[ -z "$EMIT_OUT2" ]] || fail "--emit-sessions must print nothing when no turn is new"

# --- (h) a NON-tail full pass refreshes the cursor; --dry-run never does -----
FULL="$WORK/full.jsonl"
cc_line h1 1 sess-h /tmp/memoire-cursor-home "full pass" > "$FULL"
qreset
cc_run "$WORK/cursors/h1" "$FULL" 2>/dev/null || fail "non-tail pass must exit 0"
CURH="$(cursor_of "$WORK/cursors/h1")"
[[ -n "$CURH" ]] || fail "a non-tail full pass must refresh the cursor"
[[ "$(jq -r '.offset' "$CURH")" == "$(stat -c '%s' "$FULL")" ]] \
  || fail "the non-tail cursor must mark the file consumed"
cc_run "$WORK/cursors/h2" --dry-run "$FULL" >/dev/null 2>&1 || fail "dry-run must exit 0"
[[ -z "$(cursor_of "$WORK/cursors/h2")" ]] || fail "--dry-run must never write a cursor"
cc_run "$WORK/cursors/h3" --tail --dry-run "$FULL" >/dev/null 2>&1 || fail "tail dry-run must exit 0"
[[ -z "$(cursor_of "$WORK/cursors/h3")" ]] || fail "--tail --dry-run must never write a cursor"

# --- (j) a transcript with TWO session ids keeps each one's own home (F1) ----
# Review finding F1: one home per FILE gave a resumed second session the FIRST
# session's project. The cursor keeps a MAP, one entry per session id, and a
# session absent from it falls back to the chunk's own first cwd — the rule a
# full pass has always followed.
MULTI="$WORK/multi.jsonl"
{
  cc_line m1 1 session-a /tmp/memoire-cursor-home "a one"
  cc_line m2 2 session-b /tmp/memoire-cursor-roam "b one"
} > "$MULTI"
qreset
cc_run "$WORK/cursors/j" --tail "$MULTI" 2>/dev/null || fail "multi-session first pass must exit 0"
[[ "$(qcalls)" == "2" ]] || fail "multi-session first pass must queue both turns"
CURJ="$(cursor_of "$WORK/cursors/j")"
[[ "$(jq -r '.session_homes["session-a"]' "$CURJ")" == "/tmp/memoire-cursor-home" ]] \
  || fail "cursor map must hold session-a's own home"
[[ "$(jq -r '.session_homes["session-b"]' "$CURJ")" == "/tmp/memoire-cursor-roam" ]] \
  || fail "cursor map must hold session-b's own home"
cc_line m3 3 session-b /tmp/memoire-cursor-roam "b two" >> "$MULTI"
qreset
cc_run "$WORK/cursors/j" --tail "$MULTI" 2>/dev/null || fail "multi-session tail pass must exit 0"
[[ "$(qcalls)" == "1" ]] || fail "only the appended turn may queue on the second pass"
qrender | grep -q -- '-project cursor-roam -session-id session-b ' \
  || fail "a resumed second session must attribute to ITS OWN project, not the first session's: $(qrender)"
[[ "$(jq -r '.session_homes | length' "$CURJ")" == "2" ]] \
  || fail "the cursor map must still carry both sessions after the tail pass"

# --- (i) no new flag → the queue calls are the ones the backfill always made -
BASE="$WORK/base.jsonl"
cc_line i1 1 sess-i /tmp/memoire-cursor-home "turn i1" > "$BASE"
qreset
cc_run "$WORK/cursors/i1" "$BASE" 2>/dev/null || fail "flagless pass must exit 0"
cp "$QLOG" "$WORK/flagless.argv"
qreset
cc_run "$WORK/cursors/i2" --tail "$BASE" 2>/dev/null || fail "tail first pass must exit 0"
cmp -s "$WORK/flagless.argv" "$QLOG" \
  || fail "a first --tail pass must issue byte-identical queue calls to the flagless pass"
{ tr '\0' ' ' < "$WORK/flagless.argv" || true; } | grep -q -- \
  'CALL -project cursor-home -session-id sess-i -cwd /tmp/memoire-cursor-home -event-type claude_assistant_turn -source-adapter claude_code_transcript_backfill -source-event-id cc-turn:sess-i:i1 -occurred-at 2026-09-08T10:01:00Z -text turn i1 -payload-json ' \
  || fail "the flagless queue argv changed shape: $(tr '\0' ' ' < "$WORK/flagless.argv")"


# ============================================================================
# Part 3 — sub-agent text capture (operator ruling 2026-10-02)
# ============================================================================
PARENT_CWD=/tmp/memoire-sa-parent
WT_CWD=/tmp/memoire-sa-wt

# qfield <needle> <flag>: the argv value following <flag> on the queue call
# whose argv contains <needle>.
qfield() {
  grep -a -- "$1" "$QLOG" | tr '\0' '\037' \
    | awk -F'\037' -v k="$2" '{for (i = 1; i < NF; i++) if ($i == k) { print $(i + 1); exit }}'
}
qhas() { grep -qa -- "$1" "$QLOG"; }
qcount_of() { grep -ac -- "$1" "$QLOG" || true; }

# sa_line <type> <uuid> <minute> <sid> <cwd> <agentId|-> <sidechain> <content-json>
sa_line() {
  jq -nc --arg t "$1" --arg u "$2" --argjson m "$3" --arg s "$4" --arg c "$5" --arg a "$6" \
    --argjson sc "$7" --argjson body "$8" \
    '{type: $t, uuid: $u, timestamp: ("2026-10-02T09:" + (if $m < 10 then "0" else "" end) + ($m | tostring) + ":00Z"),
      sessionId: $s, cwd: $c, isSidechain: $sc}
     + (if $a != "-" then {agentId: $a} else {} end) + $body'
}
sa_text() { sa_line assistant "$1" "$2" "$3" "$4" "$5" true "$(jq -nc --arg x "$6" '{message: {content: [{type: "text", text: $x}]}}')"; }

SA="$WORK/sa/proj"
mkdir -p "$SA/S1/subagents/workflows/wf_1"
{
  sa_line assistant m1 1 S1 "$PARENT_CWD" - false '{"message":{"content":[{"type":"text","text":"main turn"}]}}'
  sa_text inl1 2 S1 "$PARENT_CWD" inlineA "inline sidechain turn"
} > "$SA/S1.jsonl"
{
  sa_line user p1 3 S1 "$WT_CWD" a1 true '{"message":{"content":"SUBAGENT-PROMPT-NEEDLE"}}'
  sa_line attachment h1 3 S1 "$WT_CWD" a1 true '{"attachment":{"type":"hook_additional_context","content":"HOOKCTX-MARKER"}}'
  sa_line assistant a1x 4 S1 "$WT_CWD" a1 true '{"message":{"content":[{"type":"thinking","thinking":"THINKING-MARKER"},{"type":"text","text":"sub one text"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"TOOLUSE-MARKER"}}]}}'
} > "$SA/S1/subagents/agent-a1.jsonl"
printf '%s\n' '{"agentType":"Explore","description":"d","model":"m","spawnDepth":2,"toolUseId":"toolu_01","requestNonInteractive":false,"requestShape":"x"}' \
  > "$SA/S1/subagents/agent-a1.meta.json"
sa_text a2x 5 S1 "$WT_CWD" a2 "sub two no meta" > "$SA/S1/subagents/agent-a2.jsonl"
sa_text a3x 6 S1 "$WT_CWD" a3 "sub three bad meta" > "$SA/S1/subagents/agent-a3.jsonl"
printf '{"agentType": "Explo' > "$SA/S1/subagents/agent-a3.meta.json"
sa_text a4x 7 S1 "$WT_CWD" a4 "workflow depth text" > "$SA/S1/subagents/workflows/wf_1/agent-a4.jsonl"
printf '%s\n' '{"agentType":"Plan","spawnDepth":1,"toolUseId":"toolu_04"}' > "$SA/S1/subagents/workflows/wf_1/agent-a4.meta.json"
sa_text cu1 8 S1 "$WT_CWD" acompact-z1 "compaction summary text" > "$SA/S1/subagents/agent-acompact-z1.jsonl"

qreset
SA_OUT="$(cc_run "$WORK/cursors/sa1" --emit-sessions "$SA" 2>/dev/null)" || fail "sub-agent directory scan must exit 0"
[[ "$(qcalls)" == "7" ]] || fail "sub-agent scan must queue 7 events (main, inline, a1-a4, compact), got $(qcalls)"

# main-session payload: exactly today's key set
jq -e 'keys == ["backfill_generation","line_uuid","text_truncated","transcript_file"]' \
  <<<"$(qfield 'cc-turn:S1:m1' -payload-json)" >/dev/null || fail "main-session payload must keep exactly today's keys"

# a1: text only (no thinking, tool_use, user prompt, attachment), meta keys, parent attribution, own cwd
[[ "$(qcount_of 'cc-turn:S1:a1x')" == "1" ]] || fail "sub-agent text line must queue exactly once"
[[ "$(qfield 'cc-turn:S1:a1x' -text)" == "sub one text" ]] || fail "sub-agent text must be the text blocks only"
for marker in THINKING-MARKER TOOLUSE-MARKER HOOKCTX-MARKER SUBAGENT-PROMPT-NEEDLE; do
  qhas "$marker" && fail "$marker must never be queued"
done
[[ "$(qfield 'cc-turn:S1:a1x' -event-type)" == "claude_assistant_turn" ]] || fail "sub-agent text keeps the turn event type"
[[ "$(qfield 'cc-turn:S1:a1x' -source-adapter)" == "claude_code_transcript_backfill" ]] || fail "sub-agent text keeps the adapter"
[[ "$(qfield 'cc-turn:S1:a1x' -session-id)" == "S1" ]] || fail "sub-agent event must carry the parent's session id"
[[ "$(qfield 'cc-turn:S1:a1x' -project)" == "sa-parent" ]] || fail "sub-agent event must attribute to the parent's home"
[[ "$(qfield 'cc-turn:S1:a1x' -cwd)" == "$WT_CWD" ]] || fail "per-event cwd stays the line's own"
jq -e '.sidechain == true and .agent_id == "a1" and .agent_type == "Explore" and .parent_tool_use_id == "toolu_01" and .spawn_depth == 2
       and (.line_uuid == "a1x") and (.text_truncated == false)' \
  <<<"$(qfield 'cc-turn:S1:a1x' -payload-json)" >/dev/null || fail "sub-agent payload with meta wrong"
# a2: no meta file; a3: malformed meta
for u in a2x a3x; do
  jq -e '.sidechain == true and (.agent_id | length > 0) and (has("agent_type") | not) and (has("parent_tool_use_id") | not) and (has("spawn_depth") | not)' \
    <<<"$(qfield "cc-turn:S1:$u" -payload-json)" >/dev/null || fail "$u: missing/malformed meta must give a bare sidechain payload"
done
# a4: workflows depth, with meta
jq -e '.sidechain == true and .agent_id == "a4" and .agent_type == "Plan" and .parent_tool_use_id == "toolu_04" and .spawn_depth == 1' \
  <<<"$(qfield 'cc-turn:S1:a4x' -payload-json)" >/dev/null || fail "workflows-depth sub-agent payload wrong"
[[ "$(qfield 'cc-turn:S1:a4x' -project)" == "sa-parent" ]] || fail "workflows-depth sub-agent must attribute to the parent"
# inline sidechain line in the main transcript
jq -e '.sidechain == true and .agent_id == "inlineA" and (has("agent_type") | not)' \
  <<<"$(qfield 'cc-turn:S1:inl1' -payload-json)" >/dev/null || fail "inline sidechain line must be captured with sidechain payload"
# compaction agent: distinct type and id, never a turn
[[ "$(qfield 'cc-compact:S1:cu1' -event-type)" == "claude_compact_summary" ]] || fail "compaction text must use claude_compact_summary"
[[ "$(qfield 'cc-compact:S1:cu1' -text)" == "compaction summary text" ]] || fail "compaction summary text missing"
jq -e '.sidechain == true and .agent_id == "acompact-z1"' <<<"$(qfield 'cc-compact:S1:cu1' -payload-json)" >/dev/null \
  || fail "compaction payload must carry sidechain and agent_id"
qhas 'cc-turn:S1:cu1' && fail "a compaction summary must never be queued as a turn"

# --emit-sessions: sub-agent turns count under the parent session; the compaction summary does not
SA_SUM="$(awk -F'\t' '$1 == "sa-parent" && $2 == "S1" {n += $3} END {print n + 0}' <<<"$SA_OUT")"
[[ "$SA_SUM" == "6" ]] || fail "--emit-sessions must count 6 turns under the parent session (got $SA_SUM): $SA_OUT"
[[ "$(awk -F'\t' '{print $1 "/" $2}' <<<"$SA_OUT" | sort -u)" == "sa-parent/S1" ]] \
  || fail "--emit-sessions rows must all be the parent's (project, session): $SA_OUT"

# --- attribution in every order -------------------------------------------
# (1) sub-agent file only, first in argv, parent transcript with no cursor → parent's first cwd
qreset
cc_run "$WORK/cursors/sa2" --tail "$SA/S1/subagents/agent-a1.jsonl" "$SA/S1.jsonl" 2>/dev/null || fail "sub-first pass must exit 0"
[[ "$(qfield 'cc-turn:S1:a1x' -project)" == "sa-parent" ]] || fail "sub-agent first in the run must still attribute to the parent"
[[ "$(qfield 'cc-turn:S1:m1' -project)" == "sa-parent" ]] || fail "main after sub-agent must attribute to its own home"
# (2) tail pass reading ONLY the sub-agent file, parent cursor carries another home
mkdir -p "$WORK/cursors/sa3"
qreset
cc_run "$WORK/cursors/sa3" --tail "$SA/S1.jsonl" 2>/dev/null || fail "parent pass must exit 0"
PCUR="$(cursor_of "$WORK/cursors/sa3")"
jq -c '.session_homes.S1 = "/tmp/memoire-sa-other"' "$PCUR" > "$PCUR.new" && mv -f "$PCUR.new" "$PCUR"
qreset
cc_run "$WORK/cursors/sa3" --tail "$SA/S1/subagents/agent-a2.jsonl" 2>/dev/null || fail "sub-only tail pass must exit 0"
[[ "$(qfield 'cc-turn:S1:a2x' -project)" == "sa-other" ]] || fail "sub-only pass must use the parent's cursor home: $(qrender)"
[[ "$(qfield 'cc-turn:S1:a2x' -cwd)" == "$WT_CWD" ]] || fail "sub-only pass keeps the line's own cwd"
# (3) parent absent → the sub-agent's own cwd
mkdir -p "$WORK/sa/orphan/S9/subagents"
sa_text o1 1 S9 "$WT_CWD" ao "orphan sub text" > "$WORK/sa/orphan/S9/subagents/agent-ao.jsonl"
qreset
cc_run "$WORK/cursors/sa4" --tail "$WORK/sa/orphan/S9/subagents/agent-ao.jsonl" 2>/dev/null || fail "orphan pass must exit 0"
[[ "$(qfield 'cc-turn:S9:o1' -project)" == "sa-wt" ]] || fail "absent parent must fall back to the sub-agent's own cwd: $(qrender)"

# --- attribution forks: <forkId>/subagents/ is skipped too ----------------
FK="$WORK/sa/forks"
mkdir -p "$FK/FORK1/subagents" "$FK/KEEP/subagents"
sa_text f0 1 FORK1 "$PARENT_CWD" - "fork main" > "$FK/FORK1.jsonl"
sa_text f1 2 FORK1 "$PARENT_CWD" af "fork sub" > "$FK/FORK1/subagents/agent-af.jsonl"
sa_text k0 1 KEEP "$PARENT_CWD" - "keep main" > "$FK/KEEP.jsonl"
sa_text k1 2 KEEP "$PARENT_CWD" ak "keep sub" > "$FK/KEEP/subagents/agent-ak.jsonl"
printf 'FORK1\n' > "$WORK/fork-ids"
qreset
MEMOIRE_ATTRIB_FORK_IDS="$WORK/fork-ids" cc_run "$WORK/cursors/sa5" "$FK" 2>/dev/null || fail "fork pass must exit 0"
qhas 'FORK1' && fail "a fork's transcript and its sub-agents must be skipped"
qhas 'cc-turn:KEEP:k0' && qhas 'cc-turn:KEEP:k1' || fail "non-fork sessions must still be captured"

# --- --tail on a sub-agent file appended in two steps ----------------------
TS="$WORK/sa/tail/S5/subagents/agent-at.jsonl"
mkdir -p "$(dirname "$TS")"
{
  sa_text t1 1 S5 "$PARENT_CWD" at "tail one"
  sa_text t2 2 S5 "$PARENT_CWD" at "tail two"
} > "$TS"
qreset
cc_run "$WORK/cursors/sa6" --tail "$TS" 2>/dev/null || fail "sub-agent tail pass 1 must exit 0"
[[ "$(qcalls)" == "2" ]] || fail "sub-agent tail pass 1 must queue 2 (got $(qcalls))"
sa_text t3 3 S5 "$PARENT_CWD" at "tail three" >> "$TS"
LINE4="$(sa_text t4 4 S5 "$PARENT_CWD" at "tail four")"
printf '%s' "${LINE4:0:40}" >> "$TS"
qreset
cc_run "$WORK/cursors/sa6" --tail "$TS" 2>/dev/null || fail "sub-agent tail pass 2 must exit 0"
[[ "$(qcalls)" == "1" ]] && qhas 'cc-turn:S5:t3' || fail "sub-agent tail pass 2 must queue only the complete new line"
printf '%s\n' "${LINE4:40}" >> "$TS"
qreset
cc_run "$WORK/cursors/sa6" --tail "$TS" 2>/dev/null || fail "sub-agent tail pass 3 must exit 0"
[[ "$(qcalls)" == "1" ]] && qhas 'cc-turn:S5:t4' || fail "sub-agent tail pass 3 must queue the completed line, once"
qreset
cc_run "$WORK/cursors/sa6" --tail "$TS" 2>/dev/null || fail "sub-agent tail pass 4 must exit 0"
[[ "$(qcalls)" == "0" ]] || fail "sub-agent tail pass 4 must queue nothing"

# --- --quiet-for / --since apply per sub-agent file -------------------------
QD="$WORK/sa/win"
mkdir -p "$QD/S6/subagents"
sa_text w0 1 S6 "$PARENT_CWD" - "win main" > "$QD/S6.jsonl"
sa_text w1 2 S6 "$PARENT_CWD" oldag "win old sub" > "$QD/S6/subagents/agent-oldag.jsonl"
sa_text w2 3 S6 "$PARENT_CWD" newag "win new sub" > "$QD/S6/subagents/agent-newag.jsonl"
touch -d '3 days ago' "$QD/S6/subagents/agent-oldag.jsonl"
touch -d '2 hours ago' "$QD/S6.jsonl"
qreset
cc_run "$WORK/cursors/sa7" --tail --quiet-for 3600 "$QD" 2>/dev/null || fail "sub-agent --quiet-for pass must exit 0"
qhas 'cc-turn:S6:w1' || fail "--quiet-for must capture a settled sub-agent file"
qhas 'cc-turn:S6:w2' && fail "--quiet-for must skip a fresh sub-agent file"
qreset
cc_run "$WORK/cursors/sa8" --tail --since 86400 "$QD" 2>/dev/null || fail "sub-agent --since pass must exit 0"
qhas 'cc-turn:S6:w2' || fail "--since must capture a sub-agent file inside the window"
qhas 'cc-turn:S6:w1' && fail "--since must skip a sub-agent file outside the window"

# --- dry-run writes nothing for sub-agent files -----------------------------
qreset
cc_run "$WORK/cursors/sa9" --dry-run "$SA" >/dev/null 2>&1 || fail "sub-agent dry-run must exit 0"
[[ "$(qcalls)" == "0" && -z "$(cursor_of "$WORK/cursors/sa9")" ]] || fail "dry-run must write neither queue calls nor cursors"


# ============================================================================
# Part 4 — long turns: first event + one continuation (operator ruling 2026-10-02)
# ============================================================================
command -v python3 >/dev/null || { echo "turn-ingest-adapter-test: OK (part 4 skipped: python3 missing)"; exit 0; }

# qf <source-event-id> <flag>: argv value after <flag> on the call whose
# -source-event-id is EXACTLY the id (the :rest id shares a prefix).
qf() {
  tr '\0' '\037' < "$QLOG" | awk -F'\037' -v e="$1" -v k="$2" '
    { hit = 0; for (i = 1; i < NF; i++) if ($i == "-source-event-id" && $(i + 1) == e) hit = 1
      if (hit) for (i = 1; i < NF; i++) if ($i == k) { print $(i + 1); exit } }'
}
# long_text <total-chars> <out-prefix>: writes <out-prefix>.full/.first/.rest
# (UTF-8, no trailing newline). Multi-byte characters straddle the 6000 boundary.
long_text() {
  python3 - "$1" "$2" <<'PY'
import sys
n = int(sys.argv[1]); pre = sys.argv[2]
unit = "abcdéèà日本語🙂xyz"
t = ("a" * 5995 + "é" + "日" + "本" + "🙂" + "語" + unit * (n // len(unit) + 1))[:n]
if n <= 5995:
    t = (unit * (n // len(unit) + 1))[:n]
assert len(t) == n
t = t[:-1] + "Z" if n else t
for suf, v in (("full", t), ("first", t[:6000]), ("rest", t[6000:])):
    open(pre + "." + suf, "w", encoding="utf-8", newline="").write(v)
PY
}
# long_line <uuid> <minute> <sid> <cwd> <textfile> [agentId|-] [sidechain true|false]
long_line() {
  jq -nc --rawfile x "$5" --arg t assistant --arg u "$1" --argjson m "$2" --arg s "$3" --arg c "$4" --arg a "${6:--}" \
    --argjson sc "${7:-false}" \
    '{type: $t, uuid: $u, timestamp: ("2026-10-02T09:" + (if $m < 10 then "0" else "" end) + ($m | tostring) + ":00Z"),
      sessionId: $s, cwd: $c, isSidechain: $sc}
     + (if $a != "-" then {agentId: $a} else {} end)
     + {message: {content: [{type: "text", text: $x}]}}'
}
same_file() { cmp -s -- "$1" "$2"; }

LD="$WORK/long"; mkdir -p "$LD"
LCWD=/tmp/memoire-cursor-home
long_text 6000 "$LD/t6000"; long_text 6001 "$LD/t6001"; long_text 20000 "$LD/t20000"
{
  long_line t6000 1 L1 "$LCWD" "$LD/t6000.full"
  long_line t6001 2 L1 "$LCWD" "$LD/t6001.full"
  long_line t20000 3 L1 "$LCWD" "$LD/t20000.full"
  cc_line tshort 4 L1 "$LCWD" "short turn"
} > "$LD/L1.jsonl"
# --- convergence contract (no git history, no old reader) --------------------
# The first event of every turn must stay what the reader at origin/main
# b383646 produced: the GOLDEN skeletons below were produced by that reader on
# these exact fixtures and are the convergence contract with the events already
# on the hub. A skeleton is the whole queue call with the text replaced by its
# sha256 and the payload's transcript_file replaced by <file>.
cat > "$WORK/qskel.py" <<'PY'
import sys, hashlib, json
data = open(sys.argv[1], "rb").read()
want = sys.argv[2]
for call in data.split(b"\nCALL\0"):
    call = call[5:] if call.startswith(b"CALL\0") else call
    args = call.rstrip(b"\n").split(b"\0")
    if b"-source-event-id" in args and args[args.index(b"-source-event-id") + 1].decode() == want:
        out = []
        for i, a in enumerate(args):
            prev = args[i - 1] if i else b""
            if prev == b"-text":
                out.append("sha256:" + hashlib.sha256(a).hexdigest())
            elif prev == b"-payload-json":
                d = json.loads(a); d["transcript_file"] = "<file>"
                out.append(json.dumps(d, separators=(",", ":"), ensure_ascii=False))
            else:
                out.append(a.decode("utf-8", "replace"))
        print("|".join(out)); break
PY
qskel() { python3 "$WORK/qskel.py" "$QLOG" "$1"; }
# Convergence fixtures, one JSON line per case (session C1, cwd $LCWD).
build_conv_fixtures() { # out-dir cwd   → <dir>/CV.jsonl and <dir>/<case>.full
  python3 - "$1" "$2" <<'PY'
import sys, json
d, cwd = sys.argv[1], sys.argv[2]
cases = [
  ("nl2",    "a" * 5998 + "\n\n" + "tail" * 50),          # characters 5999-6000 are newlines
  ("nl1",    "a" * 5999 + "\n" + "tail" * 50),            # character 6000 is a newline
  ("nlmany", "a" * 5990 + "\n" * 10 + "z" * 30),          # ten newlines end the first slice
  ("mb",     "é" * 7000),
  ("emoji",  "🙂" * 6500),
  ("mixed",  "a" * 5997 + "日本語🙂é" + "xyz" * 500),
  ("trail",  "a" * 6100 + "\n\n"),
  ("nul",    "a" * 3000 + "\x00" + "b" * 4000),          # NUL inside the first slice
]
with open(d + "/CV.jsonl", "w", encoding="utf-8", newline="") as out:
    for i, (name, t) in enumerate(cases):
        open(d + "/" + name + ".full", "w", encoding="utf-8", newline="").write(t)
        out.write(json.dumps({"type": "assistant", "uuid": name, "timestamp": "2026-10-02T09:%02d:00Z" % (i + 10),
            "sessionId": "C1", "cwd": cwd, "isSidechain": False,
            "message": {"content": [{"type": "text", "text": t}]}}, ensure_ascii=False) + "\n")
PY
}

qreset
cc_run "$WORK/cursors/ln" "$LD/L1.jsonl" 2>"$WORK/ln.err" || fail "long-turn pass must exit 0"
grep -q 'queue write failed' "$WORK/ln.err" && fail "long-turn pass must not fail a queue write"
[[ "$(qcalls)" == "6" ]] || fail "4 turns plus 2 continuations must queue 6 calls (got $(qcalls))"
# a turn at the cap is unchanged: one event, not truncated, no continuation
[[ -z "$(qf cc-turn:L1:t6000:rest -text)$(qf cc-turn:L1:t6000:rest -text-file)" ]] || fail "a 6000-char turn must have no continuation"
[[ "$(qf cc-turn:L1:t6000 -payload-json | jq -r .text_truncated)" == "false" ]] || fail "6000-char turn must not be flagged truncated"
printf '%s' "$(qf cc-turn:L1:t6000 -text)" | same_file - "$LD/t6000.full" || fail "6000-char turn text must be whole"
for n in 6001 20000; do
  e="cc-turn:L1:t$n"
  printf '%s' "$(qf $e -text)" | same_file - "$LD/t$n.first" || fail "$n: first event text must be the first 6000 characters"
  [[ "$(qf $e -payload-json | jq -r .text_truncated)" == "true" ]] || fail "$n: first event must be flagged truncated"
  [[ -z "$(qf $e:rest -text)" ]] || fail "$n: continuation must not carry -text (argv)"
  tf="$(qf $e:rest -text-file)"; [[ -n "$tf" && -f "$WORK/tf/$(basename "$tf")" ]] || fail "$n: continuation must ride -text-file"
  [[ ! -e "$tf" ]] || fail "$n: the continuation text file must be removed after the write"
  same_file "$WORK/tf/$(basename "$tf")" "$LD/t$n.rest" || fail "$n: continuation text must be the remaining characters"
  cat "$LD/t$n.first" "$WORK/tf/$(basename "$tf")" | same_file - "$LD/t$n.full" || fail "$n: first + rest must equal the source text"
  for f in -project -session-id -cwd -event-type -source-adapter -occurred-at; do
    [[ "$(qf $e $f)" == "$(qf $e:rest $f)" && -n "$(qf $e $f)" ]] || fail "$n: continuation $f must equal the first event's"
  done
  jq -ne --argjson a "$(qf $e -payload-json)" --argjson b "$(qf $e:rest -payload-json)" --arg u "t$n" \
    '$b == ($a + {continues_line_uuid: $u, text_offset: 6000, text_truncated: false})' >/dev/null \
    || fail "$n: continuation payload must be the first payload + continues_line_uuid, text_offset 6000, text_truncated false"
done
[[ "$(qf cc-turn:L1:t6001 -event-type)" == "claude_assistant_turn" ]] || fail "turn event type"
# The first events are the reader-at-b383646 output, call for call (sha256 of the text, flags).
golden() { # source-event-id expected-skeleton
  local got; got="$(qskel "$1")"
  [[ -n "$got" && "$got" == "$2" ]] || fail "$1: first event differs from the golden b383646 call:
  got:  $got
  want: $2"
}
golden cc-turn:L1:t6000 '-project|cursor-home|-session-id|L1|-cwd|/tmp/memoire-cursor-home|-event-type|claude_assistant_turn|-source-adapter|claude_code_transcript_backfill|-source-event-id|cc-turn:L1:t6000|-occurred-at|2026-10-02T09:01:00Z|-text|sha256:83dec4146965f34a571aa186823ea3143b19178054d5e613ff39ea5b1357071a|-payload-json|{"transcript_file":"<file>","line_uuid":"t6000","backfill_generation":"2026-07-10-cc-transcript-backfill","text_truncated":false}|-project-root|/tmp/memoire-cursor-home|-project-source|registry'
golden cc-turn:L1:t6001 '-project|cursor-home|-session-id|L1|-cwd|/tmp/memoire-cursor-home|-event-type|claude_assistant_turn|-source-adapter|claude_code_transcript_backfill|-source-event-id|cc-turn:L1:t6001|-occurred-at|2026-10-02T09:02:00Z|-text|sha256:700731d68aecfa6017ccc3bcf6e6114917a4bf4a89562343be5c2b33e6c69986|-payload-json|{"transcript_file":"<file>","line_uuid":"t6001","backfill_generation":"2026-07-10-cc-transcript-backfill","text_truncated":true}|-project-root|/tmp/memoire-cursor-home|-project-source|registry'
golden cc-turn:L1:t20000 '-project|cursor-home|-session-id|L1|-cwd|/tmp/memoire-cursor-home|-event-type|claude_assistant_turn|-source-adapter|claude_code_transcript_backfill|-source-event-id|cc-turn:L1:t20000|-occurred-at|2026-10-02T09:03:00Z|-text|sha256:700731d68aecfa6017ccc3bcf6e6114917a4bf4a89562343be5c2b33e6c69986|-payload-json|{"transcript_file":"<file>","line_uuid":"t20000","backfill_generation":"2026-07-10-cc-transcript-backfill","text_truncated":true}|-project-root|/tmp/memoire-cursor-home|-project-source|registry'
golden cc-turn:L1:tshort '-project|cursor-home|-session-id|L1|-cwd|/tmp/memoire-cursor-home|-event-type|claude_assistant_turn|-source-adapter|claude_code_transcript_backfill|-source-event-id|cc-turn:L1:tshort|-occurred-at|2026-09-08T10:04:00Z|-text|sha256:c8d9164a313ed8fd1170387501abc38a0a0f8f2d2e42c1852c4256a99fe9503a|-payload-json|{"transcript_file":"<file>","line_uuid":"tshort","backfill_generation":"2026-07-10-cc-transcript-backfill","text_truncated":false}|-project-root|/tmp/memoire-cursor-home|-project-source|registry'

# Convergence fixtures and first+rest = source (review F4): the first event is
# what it always was (first 6000 characters, trailing newlines stripped by the
# reader); the continuation starts where that text actually ends, so the
# stripped newlines open it and text_offset is that character offset.
CV="$WORK/cv"; mkdir -p "$CV"
build_conv_fixtures "$CV" "$LCWD"
qreset
cc_run "$WORK/cursors/cv" "$CV/CV.jsonl" 2>/dev/null || fail "convergence fixture pass must exit 0"
cat > "$WORK/qget.py" <<'PY'
import sys
data = open(sys.argv[1], "rb").read()
want, flag, out = sys.argv[2], sys.argv[3].encode(), sys.argv[4]
for call in data.split(b"\nCALL\0"):
    call = call[5:] if call.startswith(b"CALL\0") else call
    args = call.rstrip(b"\n").split(b"\0")
    if b"-source-event-id" in args and args[args.index(b"-source-event-id") + 1].decode() == want:
        open(out, "wb").write(args[args.index(flag) + 1]); break
PY
cat > "$WORK/f4check.py" <<'PY'
import sys
full, first, rest, off, nul = open(sys.argv[1], encoding="utf-8", newline="").read(), \
    open(sys.argv[2], "rb").read(), open(sys.argv[3], "rb").read(), int(sys.argv[4]), sys.argv[5] == "nul"
k = len(full[:6000].rstrip("\n"))
assert off == k, "text_offset %d != actual end of the first event %d" % (off, k)
src = full.encode("utf-8")
expect = (full[:6000].replace("\x00", "") + full[6000:]).encode("utf-8") if nul else src
assert first + rest == expect, "first + rest != source (%d + %d vs %d bytes)" % (len(first), len(rest), len(expect))
assert rest == full[k:].encode("utf-8"), "continuation is not source[offset:]"
PY
for c in nl2 nl1 nlmany mb emoji mixed trail nul; do
  qget() { python3 "$WORK/qget.py" "$QLOG" "$1" "$2" "$3"; }
  qget "cc-turn:C1:$c" -text "$WORK/cv.$c.first"
  tfn="$(qf "cc-turn:C1:$c:rest" -text-file)" || true
  off="$(qf "cc-turn:C1:$c:rest" -payload-json | jq -r .text_offset)" || true
  [[ -n "$tfn" ]] || fail "$c: a continuation is expected"
  python3 "$WORK/f4check.py" "$CV/$c.full" "$WORK/cv.$c.first" "$WORK/tf/$(basename "$tfn")" "$off" "$c" \
    || fail "$c: first + continuation must equal the source text (text_offset $off)"
done
# a boundary newline pair moves the offset below 6000: the stripped newlines open the continuation
[[ "$(qf cc-turn:C1:nl2:rest -payload-json | jq -r .text_offset)" == "5998" ]] || fail "nl2: text_offset must be 5998"
[[ "$(qf cc-turn:C1:nlmany:rest -payload-json | jq -r .text_offset)" == "5990" ]] || fail "nlmany: text_offset must be 5990"
golden cc-turn:C1:nl2 '-project|cursor-home|-session-id|C1|-cwd|/tmp/memoire-cursor-home|-event-type|claude_assistant_turn|-source-adapter|claude_code_transcript_backfill|-source-event-id|cc-turn:C1:nl2|-occurred-at|2026-10-02T09:10:00Z|-text|sha256:b0f591b2057c86cfee139d9828fdeff03ed1235299e19d2a4443571443ab6017|-payload-json|{"transcript_file":"<file>","line_uuid":"nl2","backfill_generation":"2026-07-10-cc-transcript-backfill","text_truncated":true}|-project-root|/tmp/memoire-cursor-home|-project-source|registry'
golden cc-turn:C1:nl1 '-project|cursor-home|-session-id|C1|-cwd|/tmp/memoire-cursor-home|-event-type|claude_assistant_turn|-source-adapter|claude_code_transcript_backfill|-source-event-id|cc-turn:C1:nl1|-occurred-at|2026-10-02T09:11:00Z|-text|sha256:d435b9b358d4f4a9ea48aad14fb3b2c37f10c0072557f2eb139839ae40989810|-payload-json|{"transcript_file":"<file>","line_uuid":"nl1","backfill_generation":"2026-07-10-cc-transcript-backfill","text_truncated":true}|-project-root|/tmp/memoire-cursor-home|-project-source|registry'
golden cc-turn:C1:nlmany '-project|cursor-home|-session-id|C1|-cwd|/tmp/memoire-cursor-home|-event-type|claude_assistant_turn|-source-adapter|claude_code_transcript_backfill|-source-event-id|cc-turn:C1:nlmany|-occurred-at|2026-10-02T09:12:00Z|-text|sha256:43897f5b3b6d8137b84eecc0c19e4432e6f03704fe0e17703f7aa742aee0bb5c|-payload-json|{"transcript_file":"<file>","line_uuid":"nlmany","backfill_generation":"2026-07-10-cc-transcript-backfill","text_truncated":true}|-project-root|/tmp/memoire-cursor-home|-project-source|registry'
golden cc-turn:C1:mb '-project|cursor-home|-session-id|C1|-cwd|/tmp/memoire-cursor-home|-event-type|claude_assistant_turn|-source-adapter|claude_code_transcript_backfill|-source-event-id|cc-turn:C1:mb|-occurred-at|2026-10-02T09:13:00Z|-text|sha256:363b6288fc7457e46ac9e754c8d3da06dbb0e4742af54a46e1de4558783fdb1e|-payload-json|{"transcript_file":"<file>","line_uuid":"mb","backfill_generation":"2026-07-10-cc-transcript-backfill","text_truncated":true}|-project-root|/tmp/memoire-cursor-home|-project-source|registry'
golden cc-turn:C1:emoji '-project|cursor-home|-session-id|C1|-cwd|/tmp/memoire-cursor-home|-event-type|claude_assistant_turn|-source-adapter|claude_code_transcript_backfill|-source-event-id|cc-turn:C1:emoji|-occurred-at|2026-10-02T09:14:00Z|-text|sha256:686cf9a0bf2689db4f93ceb00811044e20b9a7679eda0d1b2e54e83f6bb78c36|-payload-json|{"transcript_file":"<file>","line_uuid":"emoji","backfill_generation":"2026-07-10-cc-transcript-backfill","text_truncated":true}|-project-root|/tmp/memoire-cursor-home|-project-source|registry'
golden cc-turn:C1:mixed '-project|cursor-home|-session-id|C1|-cwd|/tmp/memoire-cursor-home|-event-type|claude_assistant_turn|-source-adapter|claude_code_transcript_backfill|-source-event-id|cc-turn:C1:mixed|-occurred-at|2026-10-02T09:15:00Z|-text|sha256:c74fb8775b3e13a207736779c7c6b4ca96ffcd218f43fbe902b2757f770f89f9|-payload-json|{"transcript_file":"<file>","line_uuid":"mixed","backfill_generation":"2026-07-10-cc-transcript-backfill","text_truncated":true}|-project-root|/tmp/memoire-cursor-home|-project-source|registry'
golden cc-turn:C1:trail '-project|cursor-home|-session-id|C1|-cwd|/tmp/memoire-cursor-home|-event-type|claude_assistant_turn|-source-adapter|claude_code_transcript_backfill|-source-event-id|cc-turn:C1:trail|-occurred-at|2026-10-02T09:16:00Z|-text|sha256:1554d4b01d511f5918b739d07993476b80e1ece6c74f447f35064a31062f49f0|-payload-json|{"transcript_file":"<file>","line_uuid":"trail","backfill_generation":"2026-07-10-cc-transcript-backfill","text_truncated":true}|-project-root|/tmp/memoire-cursor-home|-project-source|registry'
golden cc-turn:C1:nul '-project|cursor-home|-session-id|C1|-cwd|/tmp/memoire-cursor-home|-event-type|claude_assistant_turn|-source-adapter|claude_code_transcript_backfill|-source-event-id|cc-turn:C1:nul|-occurred-at|2026-10-02T09:17:00Z|-text|sha256:3a47fa8a9f30cf675f68317dd61be04b7eb2f5c947c582a1c0f25da0fef12b21|-payload-json|{"transcript_file":"<file>","line_uuid":"nul","backfill_generation":"2026-07-10-cc-transcript-backfill","text_truncated":true}|-project-root|/tmp/memoire-cursor-home|-project-source|registry'

# MEMOIRE_BACKFILL_TEXT_MAX no longer moves the split point
qreset
MEMOIRE_BACKFILL_TEXT_MAX=10 cc_run "$WORK/cursors/lenv" "$LD/L1.jsonl" 2>/dev/null || fail "env pass must exit 0"
printf '%s' "$(qf cc-turn:L1:t6001 -text)" | same_file - "$LD/t6001.first" || fail "MEMOIRE_BACKFILL_TEXT_MAX must not move the split"
[[ "$(qcalls)" == "6" ]] || fail "MEMOIRE_BACKFILL_TEXT_MAX must not change the event count"

# --- sidechain turn and compaction summary over the cap ----------------------
long_text 7500 "$LD/sc"; long_text 6500 "$LD/cs"
LS="$WORK/long/proj"; mkdir -p "$LS/L2/subagents"
long_line m1 1 L2 "$PARENT_CWD" "$LD/t6000.full" > "$LS/L2.jsonl"
long_line s1 2 L2 "$WT_CWD" "$LD/sc.full" lsa true > "$LS/L2/subagents/agent-lsa.jsonl"
long_line c1 3 L2 "$WT_CWD" "$LD/cs.full" acompact-q true > "$LS/L2/subagents/agent-acompact-q.jsonl"
qreset
cc_run "$WORK/cursors/lsub" "$LS" 2>/dev/null || fail "long sub-agent pass must exit 0"
[[ "$(qcalls)" == "5" ]] || fail "main + sidechain pair + compaction pair = 5 calls (got $(qcalls))"
printf '%s' "$(qf cc-turn:L2:s1 -text)" | same_file - "$LD/sc.first" || fail "sidechain first event text"
same_file "$WORK/tf/$(basename "$(qf cc-turn:L2:s1:rest -text-file)")" "$LD/sc.rest" || fail "sidechain continuation text"
jq -ne --argjson a "$(qf cc-turn:L2:s1 -payload-json)" --argjson b "$(qf cc-turn:L2:s1:rest -payload-json)" \
  '$a.sidechain == true and $b.sidechain == true and $b.agent_id == "lsa" and $b.continues_line_uuid == "s1" and $b.text_offset == 6000 and $b.text_truncated == false and $a.text_truncated == true' >/dev/null \
  || fail "sidechain continuation payload"
[[ "$(qf cc-compact:L2:c1 -event-type)" == "claude_compact_summary" && "$(qf cc-compact:L2:c1:rest -event-type)" == "claude_compact_summary" ]] \
  || fail "compaction summary and its continuation must both be claude_compact_summary"
printf '%s' "$(qf cc-compact:L2:c1 -text)" | same_file - "$LD/cs.first" || fail "compaction first event text"
same_file "$WORK/tf/$(basename "$(qf cc-compact:L2:c1:rest -text-file)")" "$LD/cs.rest" || fail "compaction continuation text"
[[ "$(qf cc-compact:L2:c1:rest -project)" == "$(qf cc-compact:L2:c1 -project)" ]] || fail "compaction continuation project"

# --- a 300,000-byte text is queued without an argv failure -------------------
python3 - "$LD/big" <<'PY'
import sys
t = ("日本語é🙂abc" * 40000)[:300000]   # 300000 characters, well over 400 KB of UTF-8
open(sys.argv[1] + ".full", "w", encoding="utf-8", newline="").write(t)
open(sys.argv[1] + ".first", "w", encoding="utf-8", newline="").write(t[:6000])
open(sys.argv[1] + ".rest", "w", encoding="utf-8", newline="").write(t[6000:])
PY
[[ "$(stat -c %s "$LD/big.rest")" -gt 300000 ]] || fail "big fixture must exceed 300,000 bytes"
long_line big1 1 L3 "$LCWD" "$LD/big.full" > "$LD/L3.jsonl"
qreset
cc_run "$WORK/cursors/lbig" "$LD/L3.jsonl" 2>"$WORK/big.err" || fail "big pass must exit 0"
grep -q 'queue write failed' "$WORK/big.err" && fail "a 300 KB turn must not fail a queue write: $(cat "$WORK/big.err")"
[[ "$(qcalls)" == "2" ]] || fail "big turn must queue 2 events (got $(qcalls))"
same_file "$WORK/tf/$(basename "$(qf cc-turn:L3:big1:rest -text-file)")" "$LD/big.rest" || fail "big continuation text must be whole"
# and the argv really could not have carried it: a direct exec of a 300 KB argument fails
if "$WORK/bin/queue" -text "$(cat "$LD/big.rest")" 2>/dev/null; then
  fail "sanity: a 300 KB argv argument was expected to fail exec"
fi
qreset

# --- --emit-sessions counts turns, not events ---------------------------------
qreset
EOUT="$(cc_run "$WORK/cursors/lemit" --emit-sessions "$LD/L1.jsonl" 2>/dev/null)" || fail "emit pass must exit 0"
[[ "$(awk -F'\t' '$2 == "L1" {n += $3} END {print n + 0}' <<<"$EOUT")" == "4" ]] \
  || fail "--emit-sessions must count 4 turns, not 6 events: $EOUT"
[[ "$(qcalls)" == "6" ]] || fail "emit pass still queues every event"

# --- --dry-run: one line per event, the continuation marked -------------------
qreset
DOUT="$(cc_run "$WORK/cursors/ldry" --dry-run "$LD/L1.jsonl" 2>/dev/null)" || fail "dry-run must exit 0"
[[ "$(grep -c '^DRY ' <<<"$DOUT")" == "6" ]] || fail "dry-run must print one line per event: $DOUT"
[[ "$(grep -c ' continuation ' <<<"$DOUT")" == "2" ]] || fail "dry-run must mark exactly the 2 continuations: $DOUT"
grep -q ' t6001 chars=6000$' <<<"$DOUT" || fail "dry-run first line shape: $DOUT"
grep -q ' t6001 chars=1 continuation id=cc-turn:L1:t6001:rest text_offset=6000$' <<<"$DOUT" || fail "dry-run continuation line: $DOUT"
[[ "$(qcalls)" == "0" && -z "$(cursor_of "$WORK/cursors/ldry")" ]] || fail "dry-run must write neither queue calls nor cursors"

# --- --from-start repairs turns an older reader ingested truncated -----------
# The cursor the pre-change reader leaves after a --tail pass over L1.jsonl
# (same JSON shape), written here rather than produced by running that reader.
write_old_cursor() { # cursor-dir transcript session home
  local f; f="$(readlink -f "$2")"
  mkdir -p "$1"; chmod 700 "$1"
  jq -cn --arg p "$f" --arg d "$(stat -c '%d:%i' "$f")" --argjson o "$(stat -c %s "$f")" --arg s "$3" --arg h "$4" \
    '{path: $p, dev_inode: $d, offset: $o, session_homes: {($s): $h}, updated_at: "2026-09-30T10:00:00+02:00"}' \
    > "$1/$(printf '%s' "$f" | sha256sum | cut -d' ' -f1).json"
}
write_old_cursor "$WORK/cursors/lfs" "$LD/L1.jsonl" L1 "$LCWD"
qreset
cc_run "$WORK/cursors/lfs" --tail "$LD/L1.jsonl" 2>/dev/null || fail "tail pass after old pass must exit 0"
[[ "$(qcalls)" == "0" ]] || fail "cursor at EOF: a plain --tail pass must queue nothing (got $(qcalls))"
FSIZE="$(stat -c %s "$LD/L1.jsonl")"
qreset
cc_run "$WORK/cursors/lfs" --from-start "$LD/L1.jsonl" 2>/dev/null || fail "--from-start pass must exit 0"
[[ "$(qcalls)" == "6" ]] || fail "--from-start must re-queue the 4 firsts + 2 continuations (got $(qcalls))"
qrender | grep -q 'cc-turn:L1:t6001:rest' && qrender | grep -q 'cc-turn:L1:t20000:rest' || fail "--from-start must queue the continuations"
IDS1="$(tr '\0' '\n' < "$QLOG" | grep -a '^cc-' | sort)"
jq -e --argjson s "$FSIZE" '.offset == $s' "$(cursor_of "$WORK/cursors/lfs")" >/dev/null || fail "--from-start must leave the cursor at the end of the last complete line"
qreset
cc_run "$WORK/cursors/lfs" --from-start "$LD/L1.jsonl" 2>/dev/null || fail "second --from-start pass must exit 0"
IDS2="$(tr '\0' '\n' < "$QLOG" | grep -a '^cc-' | sort)"
[[ "$IDS1" == "$IDS2" && -n "$IDS2" ]] || fail "a second --from-start must queue the same ids and nothing else"
jq -e --argjson s "$FSIZE" '.offset == $s' "$(cursor_of "$WORK/cursors/lfs")" >/dev/null || fail "cursor after the second --from-start"
qreset
cc_run "$WORK/cursors/lfs" --tail "$LD/L1.jsonl" 2>/dev/null
[[ "$(qcalls)" == "0" ]] || fail "after --from-start a --tail pass must queue nothing"
# --from-start on a directory honours --since
touch -d '3 days ago' "$LD/L1.jsonl"
qreset
cc_run "$WORK/cursors/lfs2" --from-start --since 86400 "$LD" 2>/dev/null
qhas 'cc-turn:L1:t6001' && fail "--from-start --since must skip files outside the window"
touch "$LD/L1.jsonl"

# --- sub-agent home repair once the parent transcript appears ----------------
HR="$WORK/long/home"; mkdir -p "$HR/H1/subagents" "$HR/H2/subagents"
sa_text h1a 1 H1 "$WT_CWD" ha "first sub line" > "$HR/H1/subagents/agent-ha.jsonl"
sa_text h2a 1 H2 "$WT_CWD" hb "second sub line" > "$HR/H2/subagents/agent-hb.jsonl"
qreset
cc_run "$WORK/cursors/lhome" --tail "$HR/H1/subagents/agent-ha.jsonl" "$HR/H2/subagents/agent-hb.jsonl" 2>/dev/null || fail "orphan home pass must exit 0"
[[ "$(qf cc-turn:H1:h1a -project)" == "sa-wt" ]] || fail "orphan sub-agent starts at its own cwd"
hcur() { # sub-agent file → its cursor's home for session $2
  local f; f="$(readlink -f "$1")"
  jq -r --arg s "$2" '.session_homes[$s] // empty' "$WORK/cursors/lhome/$(printf '%s' "$f" | sha256sum | cut -d' ' -f1).json"
}
[[ -z "$(hcur "$HR/H1/subagents/agent-ha.jsonl" H1)" ]] || fail "a sidechain line never records a session home: an orphan sub-agent cursor holds none"
# A cursor an earlier reader left with the sub-agent's cwd as home (the repair target).
for ag in H1/subagents/agent-ha H2/subagents/agent-hb; do
  hc="$WORK/cursors/lhome/$(printf '%s' "$(readlink -f "$HR/$ag.jsonl")" | sha256sum | cut -d' ' -f1).json"
  jq -c --arg s "${ag%%/*}" --arg h "$WT_CWD" '.session_homes[$s] = $h' "$hc" > "$hc.new" && mv -f "$hc.new" "$hc"
done
[[ "$(hcur "$HR/H1/subagents/agent-ha.jsonl" H1)" == "$WT_CWD" ]] || fail "fixture: an older cursor records the sub-agent cwd"
# the parent transcripts appear
sa_line assistant hm1 1 H1 "$PARENT_CWD" - false '{"message":{"content":[{"type":"text","text":"parent"}]}}' > "$HR/H1.jsonl"
sa_line assistant hm2 1 H2 "$PARENT_CWD" - false '{"message":{"content":[{"type":"text","text":"parent"}]}}' > "$HR/H2.jsonl"
# H1: the sub-agent file grows; H2: no new bytes at all
sa_text h1b 2 H1 "$WT_CWD" ha "appended sub line" >> "$HR/H1/subagents/agent-ha.jsonl"
qreset
cc_run "$WORK/cursors/lhome" --tail "$HR/H1/subagents/agent-ha.jsonl" "$HR/H2/subagents/agent-hb.jsonl" 2>/dev/null || fail "home repair pass must exit 0"
[[ "$(qcalls)" == "1" && "$(qf cc-turn:H1:h1b -project)" == "sa-parent" ]] || fail "the parent's home must win once the parent is present: $(qrender)"
[[ "$(hcur "$HR/H1/subagents/agent-ha.jsonl" H1)" == "$PARENT_CWD" ]] || fail "the sub-agent cursor must be updated to the parent's home"
[[ "$(hcur "$HR/H2/subagents/agent-hb.jsonl" H2)" == "$PARENT_CWD" ]] || fail "the cursor must be repaired even with no new bytes"
HOFF="$(jq -r .offset "$WORK/cursors/lhome/$(printf '%s' "$(readlink -f "$HR/H2/subagents/agent-hb.jsonl")" | sha256sum | cut -d' ' -f1).json")"
[[ "$HOFF" == "$(stat -c %s "$HR/H2/subagents/agent-hb.jsonl")" ]] || fail "the repair must not move the offset"


# ============================================================================
# Part 5 — round-3 review fixes (failed writes, parent home, sidechain homes,
# --dry-run, unparseable lines, missing cwd/timestamp)
# ============================================================================
R3="$WORK/r3"; mkdir -p "$R3"
qfail() { printf '%s' "$1" > "$WORK/qfail"; }
qfail_off() { : > "$WORK/qfail"; }

# --- F2: a failed write is not counted and does not advance the cursor ------
long_text 7000 "$R3/f2"
{
  cc_line f2a 1 F2 "$LCWD" "first turn"
  long_line f2b 2 F2 "$LCWD" "$R3/f2.full"
  cc_line f2c 3 F2 "$LCWD" "third turn"
} > "$R3/F2.jsonl"
F2SIZE="$(stat -c %s "$R3/F2.jsonl")"
qreset; qfail ':rest'
rc=0; cc_run "$WORK/cursors/f2" --tail --emit-sessions "$R3/F2.jsonl" >"$R3/f2.out" 2>"$R3/f2.err" || rc=$?
[[ "$rc" == "2" ]] || fail "F2: a failed continuation write must exit 2 (got $rc)"
grep -q 'queue writes FAILED' "$R3/f2.err" || fail "F2: the log line must say that writes failed: $(cat "$R3/f2.err")"
grep -q 'continuation events' "$R3/f2.err" && fail "F2: a failed continuation must not be counted"
grep -q 'done: 3 events' "$R3/f2.err" || fail "F2: only the 3 first events were queued: $(cat "$R3/f2.err")"
[[ "$(qcalls)" == "3" ]] || fail "F2: 3 first events queue, the continuation fails (got $(qcalls))"
[[ -z "$(cursor_of "$WORK/cursors/f2")" ]] || fail "F2: a failed write must not advance (create) the file's cursor"
# --emit-sessions: the turns actually queued are reported; the replay at the next pass reports them again
[[ "$(awk -F'\t' '{n += $3} END {print n + 0}' "$R3/f2.out")" == "3" ]] || fail "F2: --emit-sessions counts the queued turns: $(cat "$R3/f2.out")"
qfail_off; qreset
cc_run "$WORK/cursors/f2" --tail "$R3/F2.jsonl" 2>/dev/null || fail "F2: replay pass must exit 0"
[[ "$(qcalls)" == "4" ]] && qhas 'cc-turn:F2:f2b:rest' || fail "F2: the next pass re-reads the file and queues the continuation (got $(qcalls))"
jq -e --argjson s "$F2SIZE" '.offset == $s' "$(cursor_of "$WORK/cursors/f2")" >/dev/null || fail "F2: the cursor advances once every write succeeded"
# a cursor that already exists stays where it was
long_text 8000 "$R3/f2d"
long_line f2d 4 F2 "$LCWD" "$R3/f2d.full" >> "$R3/F2.jsonl"
qreset; qfail ':rest'
rc=0; cc_run "$WORK/cursors/f2" --tail "$R3/F2.jsonl" 2>/dev/null || rc=$?
[[ "$rc" == "2" ]] || fail "F2: appended-range failure must exit 2 (got $rc)"
jq -e --argjson s "$F2SIZE" '.offset == $s' "$(cursor_of "$WORK/cursors/f2")" >/dev/null || fail "F2: a failed range leaves the cursor at its previous offset"
qfail_off; qreset
cc_run "$WORK/cursors/f2" --tail "$R3/F2.jsonl" 2>/dev/null || fail "F2: second replay must exit 0"
[[ "$(qcalls)" == "2" ]] && qhas 'cc-turn:F2:f2d:rest' || fail "F2: only the failed range is re-read (got $(qcalls))"
# a failed FIRST event
qreset; qfail 'cc-turn:F2:f2c'
rc=0; cc_run "$WORK/cursors/f2b" --tail --emit-sessions "$R3/F2.jsonl" >"$R3/f2b.out" 2>"$R3/f2b.err" || rc=$?
[[ "$rc" == "2" && -z "$(cursor_of "$WORK/cursors/f2b")" ]] || fail "F2: a failed first event must exit 2 and leave no cursor (rc $rc)"
qhas 'cc-turn:F2:f2c' && fail "F2: the failed event must not appear as queued"
[[ "$(awk -F'\t' '{n += $3} END {print n + 0}' "$R3/f2b.out")" == "3" ]] || fail "F2: failed turn not counted by --emit-sessions (f2a, f2b, f2d queued): $(cat "$R3/f2b.out")"
qfail_off
# an unusable temp dir fails the continuation the same way
qreset
rc=0; TMPDIR=/nonexistent/none cc_run "$WORK/cursors/f2c" --tail "$R3/F2.jsonl" 2>"$R3/f2c.err" || rc=$?
[[ "$rc" == "2" && -z "$(cursor_of "$WORK/cursors/f2c")" ]] || fail "F2: an unusable TMPDIR must fail the continuation, exit 2, no cursor (rc $rc)"
qhas 'cc-turn:F2:f2b:rest' && fail "F2: no continuation can be queued without a temp dir"

# --- F3: parent home fallback follows the sub-agent's session id ------------
F3="$R3/f3/proj"; mkdir -p "$F3/S2/subagents"
{
  sa_line assistant r1 1 S1 "$PARENT_CWD" - false '{"message":{"content":[{"type":"text","text":"first session"}]}}'
  sa_line assistant r2 2 S2 /tmp/memoire-sa-other - false '{"message":{"content":[{"type":"text","text":"resumed session"}]}}'
} > "$F3/S2.jsonl"
sa_text r3 3 S2 "$WT_CWD" ag1 "sub of the resumed session" > "$F3/S2/subagents/agent-ag1.jsonl"
qreset
cc_run "$R3/cur-f3a" --tail "$F3/S2/subagents/agent-ag1.jsonl" 2>/dev/null || fail "F3: sub-agent alone must exit 0"
[[ "$(qfield 'cc-turn:S2:r3' -project)" == "sa-other" ]] || fail "F3: sub-agent alone (no parent cursor) must follow ITS session id: $(qrender)"
qreset
cc_run "$R3/cur-f3b" --tail "$F3" 2>/dev/null || fail "F3: directory pass must exit 0"
[[ "$(qfield 'cc-turn:S2:r3' -project)" == "sa-other" ]] || fail "F3: directory pass must give the same project: $(qrender)"
[[ "$(qfield 'cc-turn:S1:r1' -project)" == "sa-parent" ]] || fail "F3: the first session keeps its own home"

# --- F4 (cont.): covered by the convergence fixtures above ------------------

# --- F5: a sidechain line never sets or changes the session's home ----------
F5="$R3/f5"; mkdir -p "$F5"
{
  sa_text x1 1 S7 "$WT_CWD" inl1 "inline sidechain first"
  sa_line assistant x2 2 S7 "$PARENT_CWD" - false '{"message":{"content":[{"type":"text","text":"main line"}]}}'
} > "$F5/S7.jsonl"
qreset
cc_run "$R3/cur-f5" --tail "$F5/S7.jsonl" 2>/dev/null || fail "F5: inline sidechain pass must exit 0"
[[ "$(qfield 'cc-turn:S7:x1' -project)" == "sa-parent" && "$(qfield 'cc-turn:S7:x2' -project)" == "sa-parent" ]] \
  || fail "F5: an inline sidechain line must not choose the session home: $(qrender)"
[[ "$(jq -r '.session_homes.S7' "$(cursor_of "$R3/cur-f5")")" == "$PARENT_CWD" ]] || fail "F5: the recorded home is the main session's"
sa_text y1 1 S8 "$WT_CWD" inl2 "only a sidechain line" > "$F5/S8.jsonl"
qreset
cc_run "$R3/cur-f5b" --tail "$F5/S8.jsonl" 2>/dev/null || fail "F5: sidechain-only pass must exit 0"
[[ "$(jq -r '.session_homes | has("S8")' "$(cursor_of "$R3/cur-f5b")")" == "false" ]] || fail "F5: a sidechain-only file records no session home"

# --- F6: --dry-run writes nothing at all -------------------------------------
for flag in --tail --from-start; do
  rm -rf "$R3/cur-dry"
  qreset
  DRYOUT="$(cc_run "$R3/cur-dry/nested" $flag --dry-run "$LD/L1.jsonl" 2>/dev/null)" || fail "F6: $flag --dry-run must exit 0"
  [[ ! -e "$R3/cur-dry" ]] || fail "F6: $flag --dry-run must not create the cursor dir or a lock file: $(find "$R3/cur-dry" 2>/dev/null)"
  [[ "$(grep -c '^DRY ' <<<"$DRYOUT")" == "6" ]] || fail "F6: $flag --dry-run on a cursorless file reads it whole: $DRYOUT"
done
[[ "$(cc_run "$WORK/cursors/lfs" --tail --dry-run "$LD/L1.jsonl" 2>/dev/null | grep -c '^DRY ')" == "0" ]] \
  || fail "F6: --tail --dry-run honours an existing cursor"
[[ -z "$(find "$WORK/cursors/lfs" -newer "$LD/L1.jsonl" -name '*.tmp.*' 2>/dev/null)" ]] || fail "F6: no temp file left"

# --- F7a: an unparseable line is skipped alone ------------------------------
{
  cc_line b1 1 F7 "$LCWD" "before"
  printf '%s\n' '{"type":"assistant","uuid":"bX","sess'
  cc_line b2 2 F7 "$LCWD" "after the truncated line"
  printf 'nul\0inside\n'
  cc_line b3 3 F7 "$LCWD" "after the NUL line"
  printf '%s\n' 'not json at all'
  cc_line b4 4 F7 "$LCWD" "last"
} > "$R3/F7.jsonl"
qreset
cc_run "$WORK/cursors/f7" --tail "$R3/F7.jsonl" 2>"$R3/f7.err" || fail "F7a: pass must exit 0"
[[ "$(qcalls)" == "4" ]] && qhas 'cc-turn:F7:b4' && qhas 'cc-turn:F7:b3' && qhas 'cc-turn:F7:b2' || fail "F7a: every valid line must be read (got $(qcalls))"
grep -q '3 unparseable lines skipped' "$R3/f7.err" || fail "F7a: the log line must report the skipped lines: $(cat "$R3/f7.err")"
jq -e --argjson s "$(stat -c %s "$R3/F7.jsonl")" '.offset == $s' "$(cursor_of "$WORK/cursors/f7")" >/dev/null || fail "F7a: cursor at the end of the file"

# --- F7b: no cwd, no timestamp, no id -----------------------------------------
{
  jq -nc --arg c "$LCWD" '{type:"assistant",uuid:"n1",sessionId:"F8",cwd:$c,message:{content:[{type:"text",text:"no timestamp, first line"}]}}'
  jq -nc '{type:"user",uuid:"nu",sessionId:"F8",timestamp:"2026-10-02T09:05:00Z",message:{content:"hi"}}'
  jq -nc --arg c "$LCWD" '{type:"assistant",uuid:"n2",sessionId:"F8",cwd:$c,message:{content:[{type:"text",text:"no timestamp, after a user line"}]}}'
  jq -nc '{type:"assistant",uuid:"n3",sessionId:"F8",timestamp:"2026-10-02T09:06:00Z",message:{content:[{type:"text",text:"no cwd"}]}}'
  jq -nc --arg c "$LCWD" '{type:"assistant",sessionId:"F8",cwd:$c,timestamp:"2026-10-02T09:07:00Z",message:{content:[{type:"text",text:"no uuid"}]}}'
  jq -nc --arg c "$LCWD" '{type:"assistant",uuid:"n5",cwd:$c,timestamp:"2026-10-02T09:07:00Z",message:{content:[{type:"text",text:"no session id"}]}}'
} > "$R3/F8.jsonl"
touch -d '2026-03-04 05:06:07 UTC' "$R3/F8.jsonl"
qreset
cc_run "$WORK/cursors/f8" --tail "$R3/F8.jsonl" 2>"$R3/f8.err" || fail "F7b: pass must exit 0"
[[ "$(qcalls)" == "3" ]] || fail "F7b: n1, n2, n3 must be captured (got $(qcalls)): $(qrender)"
[[ "$(qfield 'cc-turn:F8:n1' -occurred-at)" == "2026-03-04T05:06:07Z" ]] || fail "F7b: no previous timestamp: the file's mtime: $(qfield 'cc-turn:F8:n1' -occurred-at)"
[[ "$(qfield 'cc-turn:F8:n2' -occurred-at)" == "2026-10-02T09:05:00Z" ]] || fail "F7b: the nearest previous line's timestamp"
for u in n1 n2; do
  jq -e '.occurred_at_inferred == true' <<<"$(qfield "cc-turn:F8:$u" -payload-json)" >/dev/null || fail "F7b: $u must carry occurred_at_inferred"
done
jq -e 'has("occurred_at_inferred") | not' <<<"$(qfield 'cc-turn:F8:n3' -payload-json)" >/dev/null || fail "F7b: a real timestamp carries no inferred marker"
[[ "$(qskel cc-turn:F8:n3)" == *'-project|cursor-home|-session-id|F8|-cwd||-event-type|'* ]] \
  || fail "F7b: no cwd: empty -cwd, project from the session home: $(qskel cc-turn:F8:n3)"
grep -q '2 lines without uuid or sessionId skipped' "$R3/f8.err" || fail "F7b: the log line must report the id-less lines: $(cat "$R3/f8.err")"
grep -q '2 timestamps inferred' "$R3/f8.err" || fail "F7b: the log line must report the inferred timestamps"

# ============================================================================
# Part 6 — round-4 fixes (G1 retry cursor, G2 timestamp the writer accepts,
# G3 lone surrogate escapes)
# ============================================================================
R4="$WORK/r4"; mkdir -p "$R4"

# --- G1: a failed write under --from-start / a non-tail pass is retried by --tail
long_text 8000 "$R4/g1"
{
  cc_line g1a 1 G1 "$LCWD" "first turn"
  long_line g1b 2 G1 "$LCWD" "$R4/g1.full"
} > "$R4/G1.jsonl"
G1SIZE="$(stat -c %s "$R4/G1.jsonl")"
qfail_off; qreset
cc_run "$WORK/cursors/g1" --tail "$R4/G1.jsonl" 2>/dev/null || fail "G1: setup pass must exit 0"
jq -e --argjson s "$G1SIZE" '.offset == $s' "$(cursor_of "$WORK/cursors/g1")" >/dev/null || fail "G1: setup: cursor at EOF"
qreset; qfail ':rest'
rc=0; cc_run "$WORK/cursors/g1" --from-start --tail "$R4/G1.jsonl" 2>/dev/null || rc=$?
[[ "$rc" == "2" ]] || fail "G1: a failing continuation under --from-start must exit 2 (got $rc)"
[[ -z "$(cursor_of "$WORK/cursors/g1")" ]] || fail "G1: --from-start failure must leave the cursor at 0 or none: $(cat "$(cursor_of "$WORK/cursors/g1")")"
qfail_off; qreset
cc_run "$WORK/cursors/g1" --tail "$R4/G1.jsonl" 2>/dev/null || fail "G1: the next --tail pass must exit 0"
qhas 'cc-turn:G1:g1b:rest' || fail "G1: the next --tail pass must queue the missing continuation (got $(qcalls) calls)"
jq -e --argjson s "$G1SIZE" '.offset == $s' "$(cursor_of "$WORK/cursors/g1")" >/dev/null || fail "G1: the cursor advances once every write succeeded"
# a non-tail full pass over a file whose cursor is at EOF
qreset; qfail ':rest'
rc=0; cc_run "$WORK/cursors/g1" "$R4/G1.jsonl" 2>/dev/null || rc=$?
[[ "$rc" == "2" && -z "$(cursor_of "$WORK/cursors/g1")" ]] || fail "G1: a failing non-tail pass must exit 2 and leave no cursor ahead of the failure (rc $rc)"
qfail_off; qreset
cc_run "$WORK/cursors/g1" --tail "$R4/G1.jsonl" 2>/dev/null || fail "G1: replay after a non-tail failure must exit 0"
qhas 'cc-turn:G1:g1b:rest' || fail "G1: the next --tail pass must queue the continuation after a non-tail failure"
# a tail resume keeps its cursor at the previous offset (the F2 rule is unchanged)
long_text 8000 "$R4/g1d"
long_line g1d 3 G1 "$LCWD" "$R4/g1d.full" >> "$R4/G1.jsonl"
qreset; qfail ':rest'
rc=0; cc_run "$WORK/cursors/g1" --tail "$R4/G1.jsonl" 2>/dev/null || rc=$?
jq -e --argjson s "$G1SIZE" '.offset == $s' "$(cursor_of "$WORK/cursors/g1")" >/dev/null || fail "G1: a failed tail resume keeps the cursor at its start offset"
qfail_off

# --- G2: a timestamp the queue writer would refuse is treated as missing -----
{
  cc_line g2a 1 G2 "$LCWD" "good timestamp"
  printf '{"type":"assistant","uuid":"g2b","timestamp":"yesterday-ish","sessionId":"G2","cwd":"%s","message":{"content":[{"type":"text","text":"free-form timestamp"}]}}\n' "$LCWD"
  printf '{"type":"assistant","uuid":"g2c","timestamp":"2026-02-30T10:00:00Z","sessionId":"G2","cwd":"%s","message":{"content":[{"type":"text","text":"impossible date"}]}}\n' "$LCWD"
  printf '{"type":"assistant","uuid":"g2d","timestamp":1790000000,"sessionId":"G2","cwd":"%s","message":{"content":[{"type":"text","text":"numeric timestamp"}]}}\n' "$LCWD"
  printf '{"type":"assistant","uuid":"g2e","timestamp":"2026-09-08T10:05:00.25+02:00","sessionId":"G2","cwd":"%s","message":{"content":[{"type":"text","text":"valid offset and fraction"}]}}\n' "$LCWD"
} > "$R4/G2.jsonl"
# the stub refuses exactly what the writer refuses: a bad -occurred-at
qfail 'yesterday-ish'
qreset
rc=0; cc_run "$WORK/cursors/g2" --tail "$R4/G2.jsonl" 2>"$R4/g2.err" || rc=$?
[[ "$rc" == "0" ]] || fail "G2: a malformed timestamp must not fail the pass (rc $rc): $(cat "$R4/g2.err")"
[[ "$(qcalls)" == "5" ]] || fail "G2: all 5 turns are queued (got $(qcalls))"
for u in g2b g2c g2d; do
  jq -e '.occurred_at_inferred == true' <<<"$(qfield "cc-turn:G2:$u" -payload-json)" >/dev/null || fail "G2: $u must carry occurred_at_inferred"
done
[[ "$(qfield cc-turn:G2:g2b -occurred-at)" == "2026-09-08T10:01:00Z" ]] || fail "G2: inferred from the previous line: $(qfield cc-turn:G2:g2b -occurred-at)"
[[ "$(qfield cc-turn:G2:g2d -occurred-at)" == "2026-09-08T10:01:00Z" ]] || fail "G2: a numeric timestamp is inferred too"
[[ "$(qfield cc-turn:G2:g2e -occurred-at)" == "2026-09-08T10:05:00.25+02:00" ]] || fail "G2: a valid RFC 3339 timestamp is kept as is"
jq -e 'has("occurred_at_inferred") | not' <<<"$(qfield cc-turn:G2:g2e -payload-json)" >/dev/null || fail "G2: a valid timestamp carries no inferred marker"
grep -q '3 timestamps inferred' "$R4/g2.err" || fail "G2: the log counts the inferred timestamps: $(cat "$R4/g2.err")"
qreset
cc_run "$WORK/cursors/g2" --tail "$R4/G2.jsonl" 2>/dev/null || fail "G2: second pass must exit 0"
[[ "$(qcalls)" == "0" ]] || fail "G2: the file must not replay at the next pass (got $(qcalls))"
qfail_off

# --- G3: lone surrogate escapes are repaired, valid pairs are untouched -------
{
  cc_line g3a 1 G3 "$LCWD" "plain"
  printf '{"type":"assistant","uuid":"g3b","timestamp":"2026-09-08T10:02:00Z","sessionId":"G3","cwd":"%s","message":{"content":[{"type":"text","text":"pair \\ud83d\\ude00 lone \\ud800 end"}]}}\n' "$LCWD"
  printf '{"type":"assistant","uuid":"g3c","timestamp":"2026-09-08T10:03:00Z","sessionId":"G3","cwd":"%s","message":{"content":[{"type":"text","text":"low \\udc00 and escaped \\\\ud800 and \\\\\\ud800"}]}}\n' "$LCWD"
  printf '{"type":"assistant","uuid":"g3d","timestamp":"2026-09-08T10:04:00Z","sessionId":"G3","cwd":"%s","message":{"content":[{"type":"text","text":"pair only \\ud83d\\ude00"}]}}\n' "$LCWD"
  printf '{"type":"assistant","uuid":"g3e","timestamp":"2026-09-08T10:05:00Z","sessionId":"G3","cwd":"%s","message":{"content":[{"type":"text","text":"broken \\ud800 and unterminated\n' "$LCWD"
  cc_line g3f 6 G3 "$LCWD" "last"
} > "$R4/G3.jsonl"
qreset
cc_run "$WORK/cursors/g3" --tail "$R4/G3.jsonl" 2>"$R4/g3.err" || fail "G3: pass must exit 0"
[[ "$(qcalls)" == "5" ]] || fail "G3: g3a-g3d and g3f are captured (got $(qcalls)): $(cat "$R4/g3.err")"
grep -q '2 lines repaired (lone surrogate)' "$R4/g3.err" || fail "G3: the log must count 2 repaired lines: $(cat "$R4/g3.err")"
grep -q '1 unparseable lines skipped' "$R4/g3.err" || fail "G3: a line that cannot be parsed even after the repair is counted: $(cat "$R4/g3.err")"
[[ "$(qfield cc-turn:G3:g3b -text)" == "pair 😀 lone � end" ]] || fail "G3: pair kept, lone replaced: $(qfield cc-turn:G3:g3b -text)"
[[ "$(qfield cc-turn:G3:g3c -text)" == 'low � and escaped \ud800 and \�' ]] || fail "G3: an escaped backslash is not a surrogate escape: $(qfield cc-turn:G3:g3c -text)"
[[ "$(qfield cc-turn:G3:g3d -text)" == "pair only 😀" ]] || fail "G3: a valid pair alone is untouched"
[[ "$(qfield cc-turn:G3:g3a -text)" == "plain" ]] || fail "G3: a plain line is untouched"

# --- F10: the stub rejected no -text + -text-file combination ---------------
[[ ! -e "$WORK/stub-violation" ]] || fail "the reader passed -text together with -text-file"

echo "turn-ingest-adapter-test: OK"
