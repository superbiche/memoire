#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-capture-opencode.sh — native OpenCode capture-all adapter. It reads
# the OpenCode SQLite store and queues EVERY conversation record — full
# prompts, assistant text, reasoning, every tool call with input and result
# (spill files attached when they are safe to read), patches, compactions,
# todos, message-level errors, context epochs and permissions — plus a
# turn-level projection for derivation. No truncation, no sampling. Redaction
# stays at the queue-write gate (config.go MEMOIRE_REDACT).
#
# Reading the database (the harness is writing it)
# ------------------------------------------------
# The store is a live WAL SQLite database. The adapter reads a database another
# process is writing, and an open reader stops that process from cleaning its
# write-ahead log, so:
#   * one read = ONE sqlite3 process running ONE statement whose output goes to
#     a scratch file; the connection is closed before anything else happens (jq,
#     queue writes, project resolution). No read transaction ever waits on a
#     pipe or on a queue write.
#   * the JSON of every row is built IN SQL (json_object, json(), json_valid),
#     never by `sqlite3 -json` (superlinear on escaped characters). The cost is
#     linear in the row size.
#   * every read is bounded. Selection: pages of MEMOIRE_OPENCODE_BATCH sessions
#     (50) walked by rowid, each session costing two index lookups (see "Which
#     sessions"); the `part` and `message` tables are never scanned whole. A
#     chunk: at most MEMOIRE_OPENCODE_ROW_CHUNK rows (500), at most a byte
#     budget that adapts to keep a read near MEMOIRE_OPENCODE_READ_TARGET_MS
#     (200 ms; a single row larger than the budget is read alone, computed from
#     octet_length(), the record header only), and only rows whose time_updated
#     is not newer than now - MEMOIRE_OPENCODE_QUIET_MS (the quiet slack): a
#     session still being written is read up to the slack and no further, the
#     rest waits for the next run. Every statement runs under `timeout`.
#   * every sqlite3 call is `-safe`; no value taken from a row is ever
#     concatenated into SQL (see "Values from rows").
#   * every read ends with a completeness marker line. sqlite3 ignores write
#     errors on its output (a full scratch directory truncates it and exits 0),
#     so an output without the marker is refused.
#   * source: MEMOIRE_OPENCODE_DB, a PATH (a `file:` URI is refused). A path is
#     opened mode=ro when its -wal and -shm exist beside it (a running writer);
#     otherwise immutable=1, so the adapter NEVER creates a file in the harness
#     data directory. Fallback: MEMOIRE_OPENCODE_DB_FALLBACK (the archive copy,
#     always immutable=1), taken at ANY read when the primary cannot be opened,
#     is busy or locked, is malformed or hits a read-only recovery error, and
#     logged. The adapter never writes to the database and never checkpoints.
#     The archive copy is refreshed at most once a day (~/.claude/backup-history.sh,
#     backup_opencode, lines 110-113: `find "$out" -mmin +1380`, 23 h, by an hourly
#     timer): a read of it lags the live store by up to about 25 hours. A fallback
#     is visible: the summary carries source=primary|archive (the tick logs it and
#     one extra line naming the reason) and the run logs the reason.
#     mode=ro writes read marks into the existing -shm, like every WAL reader.
#   * never read, by name: `credential`, every table starting with `account`,
#     `control_account`. In a v1 store `session_message` (then an empty
#     successor table) is guarded by a warning and `session_input` rows are
#     captured (+ warning). Deleted or reverted rows leave no tombstone: a
#     vanishing row is invisible here.
#
# Two store schemas (OpenCode v1 and v2)
# --------------------------------------
# OpenCode 2.0 (fleet cutover 2026-10-06) writes sessions to `session_v2` and
# their records to `session_message` (one row per message, `type` in
# user|assistant|system|synthetic|compaction|idle, `seq` per session, the whole
# message in `data`: a user row carries `text` and `files`, an assistant row a
# `content` array of text, reasoning and tool items with the tool output
# inline). The v1 tables `session`, `message`, `part` stay in the file, frozen
# at the migration. The schema is detected from sqlite_master at every run
# (MEMOIRE_OPENCODE_SCHEMA=auto, or --schema v1|v2 to force): v2 when
# `session_v2` and `session_message` exist, v1 otherwise. Everything below
# (bounded reads, cursor, quiet gate, fallback, spill files, versioned ids)
# holds for both; only the statements and the row -> event mapping differ.
#   * the v2 stream of a session is its `session_message` rows in (time_updated,
#     's', hex(id)) order (one kind only), the cursor is the same (v: 3) file:
#     a session migrated from v1 keeps its cursor and the v2 run resumes after
#     the v1 position (k 'm' or 'p' sorts before 's'; `schema` in the cursor is
#     informative).
#   * the migration floor: the migrated rows of `session_message` are the v1
#     rows already captured by the v1 mapping (same ids, same times). A v2 run
#     emits only the rows of a session with time_updated > max(time_updated)
#     of THAT session's rows in the frozen v1 `message` table (one range of
#     its (session_id, time_created, id) index, read in the session-level
#     statement; 0 when the table is absent or the session has no v1 rows, so
#     a store or a session born in v2 is read whole;
#     MEMOIRE_OPENCODE_V2_FLOOR_MS replaces it for every session). A migrated
#     message edited in v2 is above the floor and re-emits under the v2
#     mapping (a new version); a row at or below it is never read.
#   * a fallback to the archive copy in the middle of a run reads the schema
#     again from the copy (a fallback during schema discovery does not retry
#     the failed statement on the copy: discovery restarts there); a copy whose
#     schema differs from the primary's (a v1 archive behind a v2 primary) is
#     refused: one log line, exit 1. A group signal at any of these points is
#     a clean stop (exit 0, one log line), never a failure.
#   * `session_pending` and `session_inbox` (queued prompts, promoted into user
#     rows) are not read; `session_input` and `session_context_epoch` no longer
#     exist in v2. `todo` and `permission` are read as in v1.
#   * a v2 session is quiet when now - max(session_v2.time_updated, newest
#     session_message.time_updated) is more than the quiet slack; a tool item
#     of its newest message in pending|running|streaming state younger than
#     the stuck bound keeps it gated.
#   * requires sqlite3 >= 3.44 (octet_length, aggregate ORDER BY) and jq >= 1.7
#     (below 1.7 jq re-formats number literals, so "every key verbatim" would not
#     hold). Both are checked once there is a database to read (nothing to read
#     exits 0); a missing or too old tool exits 1 with one log line.
#
# Values from rows
# ----------------
# A path, id or name taken from a row is an INPUT. Session, message and part ids
# are validated against [A-Za-z0-9_.-]+ (at most 100 bytes, no NUL): a session
# that fails is refused with a log line, a message or part row that fails is
# logged (by the hex of its id) and skipped, and both are counted (`rejected`,
# `rejected_rows` in the summary). The cursor position is stored and compared as
# hex(id), so no id from a row is ever inlined into SQL; every number inlined is
# checked ^[0-9]+$. Spill paths are handled by spill_probe / spill_copy (below).
# A BLOB in message.data or part.data does not stop its session: the row is
# captured with the blob as base64 under `raw_blob` and `blob: true` in place of
# the JSON, and counted (`blobs` in the summary). Text that is not valid UTF-8
# becomes U+FFFD (the queue format cannot hold it), as in the Codex adapter.
#
# Which sessions (bounded selection)
# ----------------------------------
# The database has no index on time_updated and session.time_updated does not
# follow the changes of its children (measured on a real store: children newer
# than their session by up to 45 minutes). So: the session table is walked in
# pages by rowid; each session is summarised by its own time_updated and by
# the time_updated of its NEWEST message (message_session_time_created_id_idx:
# one index descent and one row). A session is a candidate when that time is
# inside the window (OPENCODE_SINCE_S, 48 h, or --since) minus a slack
# (MEMOIRE_OPENCODE_SELECT_SLACK_S, 6 h); --full and explicit ids drop the
# window. Only candidates are read further: one probe statement (counts through
# the covering indexes, the newest message and its parts, the small tables) that
# gives the quiet gate and the change signature. A session whose signature is
# unchanged since it was last fully read is skipped without reading a row
# (skipped_unchanged). Residual, stated: an in-place edit of an old row with no
# other activity in its session (no new message, no count change) is not seen
# by the signature; `--verify` (implies --full) re-reads every session from its
# cursor and finds it. The tick never passes --verify and nothing schedules it:
# ACCEPTED RESIDUAL. Measured exposure on the archive
# structure: 1 session of 689 had a row updated 2-8 minutes after the last
# signature-visible change (632 of 633 within 2 minutes, covered by the quiet
# gate). It is an item for the operator: run --verify by hand now and then.
#
# The stream and the cursor
# -------------------------
# One session = one ordered stream of changes: every message and part row, in
# (time_updated, kind, id) order, kind m<p, id compared as hex. A MESSAGE sits
# at its family position: max(message.time_updated, time_updated of any of its
# parts), so a message is processed after everything it depends on. The cursor
# is one file per session under ${MEMOIRE_CAPTURE_CURSOR_DIR:-$MEMOIRE_HOME/
# capture-cursors/opencode}, mode 0600. It holds, PER SOURCE (p = primary, f =
# fallback), the position durably queued (mu, k, idhex), the versions of the
# session-level items already emitted (facts_mu, todo, inputs_mu, epoch_id,
# perm_mu) and done_sig (the signature at which the session was last read to
# its end); and, shared by both sources, `turns` (message ids whose turn event
# is queued). Rows read from the fallback advance the fallback slot ONLY: the
# next run that reads the primary again starts from the last PRIMARY position,
# so a stale archive can never mark a row of the live store as read (rows read
# twice carry identical ids and collapse at the hub). done_sig is only ever
# written for a primary read. The cursor is written after EVERY batch (<= 500
# rows, one read) and at every clean stop, never past a row whose queue write
# failed. Sessions are processed oldest change first. A run stops cleanly at a
# batch limit on --max-seconds / MEMOIRE_OPENCODE_RUN_BUDGET_S (wall time),
# MEMOIRE_OPENCODE_MAX_EVENTS (event cap) or SIGTERM/SIGINT/SIGHUP, whether it
# reached the adapter alone or with its whole process group (the queue writer
# then aborts its batch and prints the durable prefix; the cursor keeps that
# prefix): it logs ONE line, exits 0, and the next run resumes at the cursor.
# A session is not marked done while a row of it is newer than the quiet slack.
# A poison row (one whose queue write fails every time) is NOT skipped: capture
# is complete or loud, so its session fails on every run, exit 1, until the
# operator acts; other sessions are unaffected (the cursor is per session).
#
# Versioned ids
# -------------
#   opencode:part:<part id>:u<time_updated 16 digits>            call / part
#   opencode:part:<part id>:u<16>:outcome                        tool outcome
#   opencode:part:<part id>:u<16>:deferred                       outcome whose spill was deferred
#   opencode:message:<id>:u<max(message.time_updated, step-finish max)>
#   opencode:session:<sid>:facts:u<session.time_updated>
#   opencode:session:<sid>:todo:u<max time_updated>n<row count>
#   opencode:context_epoch:<sid>:u<baseline_seq>:<sha256[0:16] of the row>
#   opencode:permission:<id>:<sid>:u<time_updated>
#   opencode:session_input:<id>:u<time_created>
#   opencode:turn:<message id>                                    unversioned
# v2 rows (one session_message row = one record, its content items addressed
# by their position; <u> = time_updated of the row):
#   opencode:message:<id>:u<16>                                   the row minus its content fields
#   opencode:message:<id>:u<16>:text                              user prompt text / synthetic or system text
#   opencode:message:<id>:u<16>:f<i>                              a user file, whole
#   opencode:message:<id>:u<16>:c<i>                              assistant content item i (text, reasoning, tool call)
#   opencode:message:<id>:u<16>:c<i>:outcome | :deferred          tool outcome
#   opencode:message:<id>:u<16>:compaction                        a compaction row, whole
#   opencode:turn:<message id>                                    unversioned, as in v1
# v2 mapping (payload `schema: "v2"`): every row -> opencode_message (the row
# with `text`/`files`/`content`/`summary`+`recent` taken out, counts in their
# place: nothing is stored twice); user -> opencode_user_prompt (+ one
# opencode_file per file); assistant content items -> opencode_assistant_message,
# opencode_reasoning, opencode_tool_call + opencode_tool_result|_failure (spill
# rules as v1, keyed by the item, the row being the unit of the batch cap: a
# row deferred past the cap emits nothing and is read again at the head of
# the next batch, so no :deferred marker exists in v2); synthetic|system -> opencode_item with the
# text; compaction -> opencode_compaction; idle -> opencode_message only; one
# opencode_assistant_turn per completed assistant row with a non-empty text item
# and no tool item pending, running or streaming.
# Every emission of a part or a message is versioned, the first one included;
# there is no bare id and no counter suffix, so a re-scan or a cursor loss emits
# the same ids for the same content. An id is never emitted for two contents:
# the step-finish fold reads ALL step-finish parts of the message whatever the
# cursor is; the outcome sorts AFTER its call (greatest id = final state) and
# the deferred outcome sorts before the final one; the permission id carries the
# session it is attributed to; the epoch id carries a content digest. Versions
# of capture-all events are visible to every reader until a read-side "latest
# version" projection exists.
#
# Parts written by the Memoire plugin
# -----------------------------------
# A part whose id starts with MEMOIRE_PART_PREFIX (`prt_memoire_`, the
# synthetic parts the OpenCode plugin injects: session-start timeline, parent
# context of a sub-agent) is READ like every row but emits no event: the store
# does not re-ingest its own injected context. The filter is in jq, after the
# read, so the row is in the batch, its position counts for the cursor and the
# batch limits, and the cursor passes it exactly like a step-start part. It
# is also left out of the turn text. Nothing else changes: any other synthetic
# text part (harness "continue" and compaction messages, ...) is still queued
# as an `opencode_item` with `synthetic: true`. The message-level event of the
# message that carries it is unchanged (message JSON and step-finish fold only;
# a user message has no turn).
#
# Turn projection
# ---------------
# One opencode_assistant_turn per assistant message, ONLY when the message has
# time.completed, at least one non-empty text part and no part in a pending or
# running state; text = the text parts joined by "\n\n" in (time_created, id)
# order (a part with the Memoire plugin prefix left out), in the event TEXT
# (queue field `text`), no cap. It is evaluated at the
# message's family position. The cursor remembers every queued turn, so a later
# edit of a text part re-emits the versioned part and message events but NEVER a
# second turn under the same id. Residual, accepted: a turn edited after its
# turn event was queued, with the cursor lost, yields a second content under
# opencode:turn:<id>; the versioned capture-all events (opencode:part:...,
# opencode:message:...) hold the final text, the turn event holds the text at
# the time of its emission. A turn larger than the sweep ceiling becomes a
# derivation window of one event.
#
# Quiet gate
# ----------
# A session is imported when now - max(session.time_updated, newest message
# time_updated, time_updated of the newest message's parts) is more than
# MEMOIRE_OPENCODE_QUIET_MS (120000; --quiet-for SEC), and a tool part of its
# newest message still pending or running whose start is younger than
# MEMOIRE_OPENCODE_STUCK_MS (3600000) keeps its session gated. Explicit session
# ids bypass the window, not the gates (MEMOIRE_OPENCODE_QUIET_MS=0 drops them).
# Independently, no chunk reads a row newer than the quiet slack (see above).
#
# Spill files
# -----------
# state.metadata.outputPath is attached (spill_attached, spill_bytes = bytes,
# spill_output) only if the real path, after resolving links, is a regular file
# inside MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR (default: the OpenCode data dir's
# tool-output; a file missing there is looked up by name in
# MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR_FALLBACK, the archive copy) and it is copied
# with dd iflag=nofollow,nonblock under `timeout`, at most
# MEMOIRE_OPENCODE_SPILL_MAX_BYTES (64 MiB) per file and
# MEMOIRE_OPENCODE_SPILL_BATCH_BYTES (64 MiB) per batch. Anything else: the event
# is emitted with the output stored in the row, truncated_by_harness: true and
# spill_rejected: "<reason>" (missing, outside_dir, not_absolute,
# not_regular_file, too_large, read_failed, no_tool_output_dir). Past the batch
# cap the event carries truncated_by_harness: true and spill_deferred: true, the
# batch ends at that part, and the part is re-read (and its spill attached) at
# the next batch. Never an empty payload.
#
# Work directory
# --------------
# Rows, items and spill copies (conversation content) live in a private work
# directory $MEMOIRE_HOME/tmp/opencode-capture/run.XXXXXX (mode 0700, files
# 0600, umask 077), never under /tmp: TMPDIR is pointed at it for every child.
# The run holds a flock on <workdir>/lock for its whole life. At start, under a
# lock on the parent directory, the adapter removes every earlier work directory
# whose lock no live process holds (a run killed by SIGKILL leaves one; the next
# run clears it), then creates its own. It is removed at exit and on
# TERM/INT/HUP. A full or unwritable scratch directory fails loudly: one log
# line, exit 1.
#
# Attribution, scheduling
# -----------------------
# session.directory is the working directory passed to the project resolver
# (memoire-resolve-project.sh) and rides every event as -cwd. Default mode is a
# scan of sessions changed inside the window; --full drops the window
# (backfill). Events go to the queue writer in batches (`memoire-queue
# -batch-file`, one process per batch, stops at the first failure and prints the
# written prefix). The tick is scripts/memoire-capture-opencode-tick.sh (own
# lock, bound and timer; disabled by default).
#
# Exit code: 1 when ANY session of the run failed (a read or a queue write), the
# source is unusable, the selection failed, the scratch directory is full or a
# required tool is missing or too old; 0 otherwise, including nothing to read, a
# clean stop on a bound, the cap or a signal, and sessions refused for an
# invalid id.
#
# Options:
#   --scan               scan mode (default): sessions changed inside the window
#   --since SPEC         window cutoff, a `date -d` spec (default 48 h ago)
#   --full               no window: every session; done sessions are still skipped
#   --verify             --full, and re-read every session from its cursor
#   --db PATH            OpenCode database (MEMOIRE_OPENCODE_DB)
#   --schema auto|v1|v2  store schema (MEMOIRE_OPENCODE_SCHEMA, default auto: detected)
#   --project NAME       project override (MEMOIRE_PROJECT still works)
#   --quiet-for SEC      quiet gate and slack, seconds (MEMOIRE_OPENCODE_QUIET_MS)
#   --max-seconds N      stop at a batch boundary after N s (MEMOIRE_OPENCODE_RUN_BUDGET_S)
#   --summary            print one TSV line on stdout at exit: sessions, events,
#                        failed_sessions, rejected, stopped(0|1), sessions_left
#   --dry-run            print the events that would be queued; write nothing,
#                        cursors included
#   -h, --help           this block
#
# Knobs (all defaulted; a malformed value falls back to the default, it never
# kills capture): MEMOIRE_OPENCODE_DB, _DB_FALLBACK, _SQLITE3 (binary),
# _TOOL_OUTPUT_DIR, _TOOL_OUTPUT_DIR_FALLBACK, _SPILL_MAX_BYTES, _SPILL_BATCH_BYTES,
# _QUIET_MS (120000), _STUCK_MS (3600000), _SELECT_SLACK_S (21600), _BATCH
# (sessions per selection page, 50), _ROW_CHUNK (rows per batch, at most 500),
# _READ_BYTES (initial byte budget of one read, 2 MiB, adapting to
# _READ_TARGET_MS = 200), _READ_TIMEOUT_S (60), _BUSY_MS (5000), _MAX_EVENTS (0 =
# none), _RUN_BUDGET_S (0 = none; the wall-time bound never fires before the
# first batch is durable), _TIMING=1 (phase timers in the summary), _SCHEMA
# (auto|v1|v2), _V2_FLOOR_MS (the migration floor, ms; default: derived);
# OPENCODE_SINCE_S (172800); MEMOIRE_CAPTURE=off|0|false (kill switch);
# MEMOIRE_PROJECT; MEMOIRE_BACKFILL_GENERATION; MEMOIRE_CAPTURE_CURSOR_DIR;
# MEMOIRE_QUEUE_BIN (must understand -batch-file); MEMOIRE_RESOLVE_PROJECT;
# MEMOIRE_HOME. Each MEMOIRE_OPENCODE_* name above is written without its prefix.

# ---- OPERATOR RULING PENDING: default source ---------------------------------
# Which database a run reads when neither MEMOIRE_OPENCODE_DB nor --db is set.
#   live    -> ~/.local/share/opencode/opencode.db (mode=ro / immutable=1 as above)
#   archive -> ~/Sync/archives/opencode-history/opencode.db (immutable=1)
# This constant is the only place the choice lives.
OPENCODE_DEFAULT_SOURCE="live"
# -----------------------------------------------------------------------------

case "${MEMOIRE_CAPTURE:-}" in off|0|false) exit 0 ;; esac

SCRIPT_PATH="${BASH_SOURCE[0]}"
SCRIPT_DIR="${SCRIPT_PATH%/*}"
if [[ "$SCRIPT_DIR" == "$SCRIPT_PATH" ]]; then
  SCRIPT_DIR="."
fi
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
QUEUE_BIN="${MEMOIRE_QUEUE_BIN:-$ROOT/bin/memoire-queue}"
RESOLVER="${MEMOIRE_RESOLVE_PROJECT:-$ROOT/scripts/memoire-resolve-project.sh}"
SQLITE3="${MEMOIRE_OPENCODE_SQLITE3:-sqlite3}"
LIVE_DB="$HOME/.local/share/opencode/opencode.db"
ARCHIVE_DB="$HOME/Sync/archives/opencode-history/opencode.db"
case "$OPENCODE_DEFAULT_SOURCE" in
  archive) DEFAULT_DB="$ARCHIVE_DB" ;;
  *) DEFAULT_DB="$LIVE_DB" ;;
esac
DB_PARAM="${MEMOIRE_OPENCODE_DB:-${OPENCODE_DB:-$DEFAULT_DB}}"
DB_FALLBACK="${MEMOIRE_OPENCODE_DB_FALLBACK:-$ARCHIVE_DB}"
TOOL_OUT_DIR="${MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR:-$HOME/.local/share/opencode/tool-output}"
TOOL_OUT_FALLBACK="${MEMOIRE_OPENCODE_TOOL_OUTPUT_DIR_FALLBACK:-$HOME/Sync/archives/opencode-history/tool-output}"
MEMOIRE_HOME_DIR="${MEMOIRE_HOME:-$HOME/.memoire}"
CURSOR_DIR="${MEMOIRE_CAPTURE_CURSOR_DIR:-$MEMOIRE_HOME_DIR/capture-cursors/opencode}"
ADAPTER="opencode-native"
# Id prefix of every part the Memoire OpenCode plugin injects into a session
# (scripts/opencode-plugin/memoire.js, pushPart: `prt_memoire_<uuid>`, synthetic).
# The plugin is the writer; this reader never queues such a part (the store
# must not re-ingest its own injected context). The prefix is written here and
# in the plugin only: change one, change the other.
MEMOIRE_PART_PREFIX="prt_memoire_"
GENERATION="${MEMOIRE_BACKFILL_GENERATION:-2026-09-29-opencode-native-capture}"

QUIET_MS="${MEMOIRE_OPENCODE_QUIET_MS:-120000}"
STUCK_MS="${MEMOIRE_OPENCODE_STUCK_MS:-3600000}"
SLACK_S="${MEMOIRE_OPENCODE_SELECT_SLACK_S:-21600}"
SEL_BATCH="${MEMOIRE_OPENCODE_BATCH:-50}"
ROW_CHUNK="${MEMOIRE_OPENCODE_ROW_CHUNK:-500}"
READ_BYTES="${MEMOIRE_OPENCODE_READ_BYTES:-2097152}"
READ_TARGET_MS="${MEMOIRE_OPENCODE_READ_TARGET_MS:-200}"
READ_TIMEOUT_S="${MEMOIRE_OPENCODE_READ_TIMEOUT_S:-60}"
BUSY_MS="${MEMOIRE_OPENCODE_BUSY_MS:-5000}"
MAX_EVENTS="${MEMOIRE_OPENCODE_MAX_EVENTS:-0}"
RUN_BUDGET_S="${MEMOIRE_OPENCODE_RUN_BUDGET_S:-0}"
SPILL_MAX="${MEMOIRE_OPENCODE_SPILL_MAX_BYTES:-67108864}"
SPILL_BATCH="${MEMOIRE_OPENCODE_SPILL_BATCH_BYTES:-67108864}"
SCHEMA_OPT="${MEMOIRE_OPENCODE_SCHEMA:-auto}"
V2_FLOOR_OVERRIDE="${MEMOIRE_OPENCODE_V2_FLOOR_MS:-}"
SINCE_SPEC=""
PROJECT_OVERRIDE="${MEMOIRE_PROJECT:-}"
FULL=0
VERIFY=0
DRY_RUN=0
SUMMARY=0

usage() { sed -n '/^# memoire-capture-opencode.sh/,/^# MEMOIRE_HOME\. Each/p' "$SCRIPT_PATH" | sed 's/^# \{0,1\}//' >&2; }

exec 5>&2   # the log channel; the shell's own stderr is redirected later (see "Work directory")
log() { printf 'memoire-capture-opencode: %s\n' "$*" >&5; }

need_operand() {
  if [[ $# -lt 2 || -z "${2:-}" ]]; then
    usage
    log "missing operand for $1"
    exit 2
  fi
}

SESSION_IDS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --scan) shift ;;
    --full) FULL=1; shift ;;
    --verify) FULL=1; VERIFY=1; shift ;;
    --since) need_operand "$@"; SINCE_SPEC="$2"; shift 2 ;;
    --since=*) SINCE_SPEC="${1#*=}"; shift ;;
    --db) need_operand "$@"; DB_PARAM="$2"; shift 2 ;;
    --db=*) DB_PARAM="${1#*=}"; shift ;;
    --schema) need_operand "$@"; SCHEMA_OPT="$2"; shift 2 ;;
    --schema=*) SCHEMA_OPT="${1#*=}"; shift ;;
    --project) need_operand "$@"; PROJECT_OVERRIDE="$2"; shift 2 ;;
    --quiet-for) need_operand "$@"
      if [[ "$2" =~ ^[0-9]+$ ]]; then QUIET_MS=$(( $2 * 1000 )); else log "unparsable --quiet-for: $2"; exit 2; fi
      shift 2 ;;
    --max-seconds) need_operand "$@"
      if [[ "$2" =~ ^[0-9]+$ ]]; then RUN_BUDGET_S="$2"; else log "unparsable --max-seconds: $2"; exit 2; fi
      shift 2 ;;
    --summary) SUMMARY=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) usage; log "unknown option: $1"; exit 2 ;;
    *) SESSION_IDS+=("$1"); shift ;;
  esac
done

EXPLICIT=0
[[ ${#SESSION_IDS[@]} -gt 0 ]] && EXPLICIT=1

# Fail-open knob validation: a typo must not silently kill capture.
[[ "$QUIET_MS" =~ ^[0-9]+$ ]] || QUIET_MS=120000
[[ "$STUCK_MS" =~ ^[0-9]+$ ]] || STUCK_MS=3600000
[[ "$SLACK_S" =~ ^[0-9]+$ ]] || SLACK_S=21600
[[ "$SEL_BATCH" =~ ^[1-9][0-9]*$ ]] || SEL_BATCH=50
[[ "$ROW_CHUNK" =~ ^[1-9][0-9]*$ ]] || ROW_CHUNK=500
(( ROW_CHUNK > 500 )) && ROW_CHUNK=500
[[ "$READ_BYTES" =~ ^[1-9][0-9]*$ ]] || READ_BYTES=2097152
[[ "$READ_TARGET_MS" =~ ^[1-9][0-9]*$ ]] || READ_TARGET_MS=200
[[ "$READ_TIMEOUT_S" =~ ^[1-9][0-9]*$ ]] || READ_TIMEOUT_S=60
[[ "$BUSY_MS" =~ ^[0-9]+$ ]] || BUSY_MS=5000
[[ "$MAX_EVENTS" =~ ^[0-9]+$ ]] || MAX_EVENTS=0
[[ "$RUN_BUDGET_S" =~ ^[0-9]+$ ]] || RUN_BUDGET_S=0
[[ "$SPILL_MAX" =~ ^[1-9][0-9]*$ ]] || SPILL_MAX=67108864
[[ "$SPILL_BATCH" =~ ^[1-9][0-9]*$ ]] || SPILL_BATCH=67108864
case "$SCHEMA_OPT" in auto|v1|v2) ;; *) log "unrecognised schema '$SCHEMA_OPT', using auto"; SCHEMA_OPT=auto ;; esac
[[ "$V2_FLOOR_OVERRIDE" =~ ^[0-9]*$ ]] || V2_FLOOR_OVERRIDE=""
OPENCODE_SINCE_S="${OPENCODE_SINCE_S:-172800}"
[[ "$OPENCODE_SINCE_S" =~ ^[0-9]+$ ]] || OPENCODE_SINCE_S=172800
BUDGET_MIN=262144
BUDGET_MAX=16777216
(( READ_BYTES < BUDGET_MIN )) && READ_BYTES=$BUDGET_MIN
(( READ_BYTES > BUDGET_MAX )) && READ_BYTES=$BUDGET_MAX

# The source parameters are paths. A `file:` URI would let the caller pick the
# open mode (a read-write open of a WAL database checkpoints and deletes the
# log on close, a write into the harness database), so it is refused.
for _p in "$DB_PARAM" "$DB_FALLBACK"; do
  case "$_p" in
    file:*|FILE:*)
      log "refused the source: a database parameter is a path, not a file: URI ($_p); nothing captured"
      exit 2 ;;
  esac
done

SUMMARY_DONE=0
emit_summary() { # sessions events failed rejected stopped left blobs (then the source: primary|archive)
  (( SUMMARY == 1 && SUMMARY_DONE == 0 )) || return 0
  SUMMARY_DONE=1
  local src=primary
  [[ "${ON_FALLBACK:-0}" == "1" ]] && src=archive
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "${1:-0}" "${2:-0}" "${3:-0}" "${4:-0}" "${5:-0}" "${6:-0}" "${7:-0}" "$src"
}

# Nothing to read: neither the source nor its fallback exists. Success, no
# requirement checked (a machine without OpenCode needs neither jq nor sqlite3).
if [[ ! -f "$DB_PARAM" && ! -f "$DB_FALLBACK" ]]; then
  log "no database to read ($DB_PARAM, fallback $DB_FALLBACK): nothing captured"
  emit_summary 0 0 0 0 0 0 0
  exit 0
fi

# --- requirements, checked once there is something to read --------------------
if ! command -v jq >/dev/null 2>&1; then
  log "jq is required"
  exit 1
fi
if ! command -v "$SQLITE3" >/dev/null 2>&1; then
  log "sqlite3 is required (>= 3.44)"
  exit 1
fi
SQLITE_VERSION="$("$SQLITE3" --version 2>/dev/null)"
if [[ "$SQLITE_VERSION" =~ ^([0-9]+)\.([0-9]+) ]] \
   && { (( BASH_REMATCH[1] > 3 )) || (( BASH_REMATCH[1] == 3 && BASH_REMATCH[2] >= 44 )); }; then
  :
else
  log "sqlite3 >= 3.44 is required, found '${SQLITE_VERSION%% *}': nothing captured"
  exit 1
fi
JQ_VERSION="$(jq --version 2>/dev/null)"
if [[ "$JQ_VERSION" =~ ^jq-([0-9]+)\.([0-9]+) ]] \
   && { (( BASH_REMATCH[1] > 1 )) || (( BASH_REMATCH[1] == 1 && BASH_REMATCH[2] >= 7 )); }; then
  :
else
  log "jq is required (>= 1.7), found '${JQ_VERSION:-unknown}': nothing captured"
  exit 1
fi
# The version alone does not prove the JSON functions were compiled in.
if ! "$SQLITE3" -safe -batch :memory: "SELECT octet_length('x'), json_valid('{}'), (SELECT group_concat(a, ',' ORDER BY a DESC) FROM (SELECT 1 AS a UNION ALL SELECT 2));" >/dev/null 2>&1; then
  log "sqlite3 >= 3.44 with JSON support is required: nothing captured"
  exit 1
fi
if [[ "$DRY_RUN" == "0" && ! -x "$QUEUE_BIN" ]]; then
  log "queue writer not found: $QUEUE_BIN; run make build"
  exit 1
fi
command -v flock >/dev/null 2>&1 || log "flock missing: cursor locking disabled"
command -v timeout >/dev/null 2>&1 || { log "timeout (coreutils) is required"; exit 1; }
export LC_ALL=C

now_ms() { date +%s%3N; }
NOW_MS="$(now_ms)"
START_MS="$NOW_MS"

# --- private work directory ----------------------------------------------------
# Conversation content is copied here while a batch is processed. Never /tmp:
# the directory lives under the Memoire home, mode 0700, files 0600. See the
# header for the lock protocol.
umask 077
WORK_BASE="$MEMOIRE_HOME_DIR/tmp/opencode-capture"
WORK_DIR=""
mkdir -p "$WORK_BASE" 2>/dev/null && chmod 700 "$MEMOIRE_HOME_DIR/tmp" "$WORK_BASE" 2>/dev/null
if [[ -d "$WORK_BASE" ]] && { exec 7>"$WORK_BASE/.lock"; } 2>/dev/null && flock 7; then
  for stale in "$WORK_BASE"/run.*; do
    [[ -d "$stale" ]] || continue
    # A live run holds a flock on its lock file; none held (or no lock file:
    # the creator was killed between mkdir and flock) means the run is dead.
    if [[ ! -e "$stale/lock" ]] || ( exec 8<"$stale/lock" && flock -n 8 ) 2>/dev/null; then
      rm -rf -- "$stale"
    fi
  done
  WORK_DIR="$(mktemp -d "$WORK_BASE/run.XXXXXX" 2>/dev/null)" || WORK_DIR=""
  if [[ -n "$WORK_DIR" ]]; then
    exec 6>"$WORK_DIR/lock" && flock -n 6 || { rm -rf -- "$WORK_DIR"; WORK_DIR=""; }
  fi
  exec 7>&-
fi
[[ -n "$WORK_DIR" ]] || { log "scratch write failed: cannot create a work directory under $WORK_BASE"; exit 1; }
export TMPDIR="$WORK_DIR"
# The shell's own diagnostics go to a file: bash reports every child that a group
# signal kills ("Terminated ...") on its stderr, and a stop must leave ONE log line.
# At exit only what is not such a notice is passed on to the log channel.
exec 2>"$WORK_DIR/shell.err"
ROWS_TSV="$WORK_DIR/rows.tsv"
ROWS_JSONL="$WORK_DIR/rows.jsonl"
POS_TSV="$WORK_DIR/pos.tsv"
ITEMS="$WORK_DIR/items.jsonl"
WRITTEN="$WORK_DIR/written.txt"
ERR_FILE="$WORK_DIR/sqlite.err"
SPILL_TSV="$WORK_DIR/spill.tsv"
SPILL_PATCH="$WORK_DIR/spill.jsonl"
SPILL_MAP="$WORK_DIR/spillmap.json"
TURNS_FILE="$WORK_DIR/turns.json"
TURNS_NEW="$WORK_DIR/turns-new.json"
OTHER_SLOT="$WORK_DIR/other-slot.json"
AUX_FILE="$WORK_DIR/aux.jsonl"
SESSIONS_TSV="$WORK_DIR/sessions.tsv"
STOP_REQUESTED=0
cleanup() {
  if [[ -s "$WORK_DIR/shell.err" ]]; then
    # a notice may span lines (the text of a multi-line command follows it): skip up to the next diagnostic
    awk '/(^|[: ])[0-9]* *(Terminated|Killed|Interrupt|Hangup)( |$)/ { skip = 1; next }
         skip && !/^memoire-capture-opencode:|: line [0-9]+:/ { next }
         { skip = 0; print }' "$WORK_DIR/shell.err" 2>/dev/null | head -n 20 >&5
  fi
  rm -rf -- "$WORK_DIR"
}
on_signal() { STOP_REQUESTED=1; }
trap cleanup EXIT
trap on_signal TERM INT HUP

# A full or unwritable scratch directory: one log line, exit 1, nothing else.
# scratch_ok is a real write of 64 KiB (and a free-space floor), because
# sqlite3 and some shell writes fail silently.
scratch_ok() {
  local probe="$WORK_DIR/.probe" avail
  (( STOP_REQUESTED )) && return 0   # a helper killed by the signal proves nothing about the disk
  head -c 65536 /dev/zero >"$probe" 2>/dev/null && [[ "$(stat -c %s -- "$probe" 2>/dev/null)" == "65536" ]] || { rm -f "$probe" 2>/dev/null; return 1; }
  rm -f "$probe" 2>/dev/null
  avail="$(df -B1 --output=avail "$WORK_DIR" 2>/dev/null | tail -n 1 | tr -dc '0-9')"
  [[ -z "$avail" ]] || (( avail >= 1048576 ))
}
scratch_fail() { # what failed
  log "scratch write failed (full or unwritable work directory $WORK_DIR): $1; nothing further captured"
  emit_summary 0 0 0 0 0 0 0
  exit 1
}

# --- database access -------------------------------------------------------------

uri_escape() { # a bare path inside a file: URI: % ? # would be read as syntax
  local s="$1"
  s="${s//%/%25}"; s="${s//\?/%3F}"; s="${s//#/%23}"
  printf '%s' "$s"
}

ON_FALLBACK=0
DB_SOURCE="primary"
SRC="p"            # the cursor slot of the source in use: p (primary) or f (fallback)
# Phase timers (ms), reported when MEMOIRE_OPENCODE_TIMING=1.
T_READ=0 T_MAP=0 T_QUEUE=0

ACTIVE_URI=""
choose_uri() { # sets ACTIVE_URI and DB_SOURCE (no subshell: the globals must survive)
  if [[ "$ON_FALLBACK" == "1" ]]; then
    DB_SOURCE="fallback-archive"; SRC="f"
    ACTIVE_URI="file:$(uri_escape "$DB_FALLBACK")?immutable=1"
    return 0
  fi
  SRC="p"
  # Evaluated at every read: a writer may appear or vanish.
  if [[ -e "$DB_PARAM-wal" && -e "$DB_PARAM-shm" ]]; then
    DB_SOURCE="primary-ro"
    ACTIVE_URI="file:$(uri_escape "$DB_PARAM")?mode=ro"
  else
    # No writer beside the file: immutable=1 creates no -wal/-shm in the
    # harness data directory (a mode=ro open of a WAL database would).
    DB_SOURCE="primary-immutable"
    ACTIVE_URI="file:$(uri_escape "$DB_PARAM")?immutable=1"
  fi
}

# A path that does not exist is an open failure WITHOUT opening it: sqlite3
# creates an empty file for a missing path even with immutable=1, and the
# adapter must never create a file next to the harness database.
missing_source() {
  local p="$DB_PARAM"
  [[ "$ON_FALLBACK" == "1" ]] && p="$DB_FALLBACK"
  [[ ! -f "$p" ]]
}

END_MARK='@@END@@'
# One statement, one process, output to a file; the transaction ends when the
# process exits. -safe: no writefile/load_extension/shell dot-commands. The
# statement is followed by a marker SELECT: sqlite3 ignores write errors on its
# output, so an output without the marker line is incomplete and is refused.
sq_run() { # uri sql outfile
  local sql="$2" rc
  if missing_source; then
    echo "Error: unable to open database file (no such file)" >"$ERR_FILE"
    return 1
  fi
  sql="${sql%"${sql##*[![:space:]]}"}"
  [[ "$sql" == *';' ]] || sql+=';'
  timeout "$READ_TIMEOUT_S" "$SQLITE3" -safe -batch -noheader -nullvalue '' \
    "$1" ".timeout $BUSY_MS" "$sql
SELECT '$END_MARK';" >"$3" 2>"$ERR_FILE"
  rc=$?
  (( rc == 0 )) || return "$rc"
  if [[ "$(tail -n 1 "$3" 2>/dev/null)" != "$END_MARK" ]]; then
    scratch_ok || scratch_fail "a read came back incomplete"
    echo "Error: incomplete output (no end marker)" >"$ERR_FILE"
    return 1
  fi
  truncate -s -$(( ${#END_MARK} + 1 )) "$3" 2>/dev/null || { (( STOP_REQUESTED )) && return 1; scratch_fail "cannot trim a read"; }
  return 0
}

err_class() { # rc -> open|busy|readonly|corrupt|other (from the sqlite error text)
  local rc="$1" e
  [[ "$rc" == "124" ]] && { echo busy; return; }
  e="$(head -c 2000 "$ERR_FILE" 2>/dev/null | tr 'A-Z' 'a-z')"
  case "$e" in
    *malformed*|*corrupt*) echo corrupt ;;
    *"unable to open"*|*"cannot open"*|*"not a database"*|*"no such file"*) echo open ;;
    *locked*|*busy*) echo busy ;;
    *readonly*|*read-only*|*recovery*|*"disk i/o"*) echo readonly ;;
    *) echo other ;;
  esac
}

READ_MS=0
READ_COUNT=0
READ_MAX_MS=0
SLOT_HOOK=""   # a function called after a source change, before a statement is rebuilt
# read_db OUTFILE BUILDER [ARGS...] -> 0 on success, 1 on failure, 2 when a
# signal interrupted it (the statement, the builder or a timer), 3 when the
# source changed and SOURCE_HOOK refused to retry the statement on the new
# source (schema discovery restarts instead). BUILDER prints the SQL (it is
# called again after a source change, so a statement never carries the position
# of the other source). Falls back to the archive copy at ANY read when the
# primary cannot be opened, is busy or locked, is malformed or hits a read-only
# recovery error.
read_db() {
  local out="$1" builder="$2" rc cls t0 t1 sql attempt
  shift 2
  for attempt in 1 2; do
    # The builder and the timers run in subshells: one killed by a group signal
    # is a requested stop (rc 2), not a builder failure and not a read failure.
    sql="$("$builder" "$@")" || { (( STOP_REQUESTED )) && return 2; log "internal error: cannot build a statement"; return 1; }
    choose_uri
    t0="$(now_ms)"; [[ "$t0" =~ ^[0-9]+$ ]] || { (( STOP_REQUESTED )) && return 2; t0=0; }
    sq_run "$ACTIVE_URI" "$sql" "$out"; rc=$?
    t1="$(now_ms)"; [[ "$t1" =~ ^[0-9]+$ ]] || t1=$t0
    READ_MS=$(( t1 - t0 ))
    READ_COUNT=$(( READ_COUNT + 1 ))
    T_READ=$(( T_READ + READ_MS ))
    (( READ_MS > READ_MAX_MS )) && READ_MAX_MS=$READ_MS
    # The stop is classified BEFORE the success: a child that printed valid output
    # and was then killed by the group signal (the ending timer) is still a stop.
    (( STOP_REQUESTED )) && return 2
    (( rc == 0 )) && return 0
    if [[ "$ON_FALLBACK" == "0" && "$attempt" == "1" ]]; then
      cls="$(err_class "$rc")"
      if [[ "$cls" != "other" ]]; then
        log "primary database $cls error (rc=$rc): $(head -c 300 "$ERR_FILE" | tr '\n' ' '); falling back to the archive copy (immutable=1)"
        ON_FALLBACK=1
        choose_uri
        if [[ -n "${SOURCE_HOOK:-}" ]] && ! "$SOURCE_HOOK"; then
          (( STOP_REQUESTED )) && return 2
          return 3
        fi
        [[ -n "$SLOT_HOOK" ]] && "$SLOT_HOOK"
        continue
      fi
    fi
    break
  done
  log "read failed (rc=$rc): $(head -c 300 "$ERR_FILE" | tr '\n' ' ')"
  return 1
}

sql_lit() { printf '%s' "$1"; }   # a builder for a fixed statement

# A group signal before the selection: nothing was read to completion, no cursor
# exists to write; the one clean-stop line of the main driver, then exit 0.
init_stop_exit() {
  log "stopped cleanly on signal: nothing read yet, 0 session(s) left for the next run"
  emit_summary 0 0 0 0 1 0 0
  exit 0
}

# Probe the source (and take the fallback here if the primary is unusable). The
# probe names no table: a v1 store has `session`, a v2 store `session_v2`.
if ! read_db "$WORK_DIR/probe.out" sql_lit "SELECT count(*) FROM sqlite_master WHERE type = 'table';"; then
  (( STOP_REQUESTED )) && init_stop_exit
  log "no usable database (primary and fallback failed); giving up"
  emit_summary 0 0 1 0 0 0 0
  exit 1
fi
if [[ "$ON_FALLBACK" == "1" ]]; then
  log "reading the ARCHIVE copy ($DB_FALLBACK), not the primary"
fi

# The tables present, the schema in use and the session columns come from the
# schema of the source in use, validated: a name from sqlite_master is an input
# like any other. load_schema reads them (three bounded statements) and sets
# HAVE_TABLES, SCHEMA, ST (the session table), MT (the message table) and
# SESSION_OBJ; a v1-only side table (session_input, session_context_epoch, the
# v1 session_message warning) is consulted only in v1 mode. rc 1: a read
# failed; rc 2: a signal interrupted a read, or the requested schema is not in
# the store (the two are told apart by STOP_REQUESTED); rc 3: the source
# changed during discovery and the statement was not retried.
HAVE_TABLES=" "
SCHEMA="" ST="" MT=""
SESSION_OBJ="json_object('id', s.id)"
have_table() { [[ "$HAVE_TABLES" == *" $1 "* ]]; }
v1_table() { [[ "$SCHEMA" == "v1" ]] && have_table "$1"; }
load_schema() {
  local t c rc
  HAVE_TABLES=" "
  read_db "$WORK_DIR/tables.out" sql_lit "SELECT name FROM sqlite_master WHERE type='table' AND name IN ('session','message','part','session_v2','session_message','todo','session_input','session_context_epoch','permission','project','project_directory');"; rc=$?
  (( rc == 0 )) || return "$rc"
  while IFS= read -r t; do [[ "$t" =~ ^[a-z_0-9]+$ ]] && HAVE_TABLES+="$t "; done <"$WORK_DIR/tables.out"
  SCHEMA="$SCHEMA_OPT"
  if [[ "$SCHEMA" == "auto" ]]; then
    if have_table session_v2 && have_table session_message; then SCHEMA=v2; else SCHEMA=v1; fi
  fi
  if [[ "$SCHEMA" == "v2" ]]; then
    ST=session_v2; MT=session_message
    if ! have_table session_v2 || ! have_table session_message; then
      log "schema v2 requested but session_v2/session_message are missing: nothing captured"
      return 2
    fi
  else
    ST=session; MT=message
    if ! have_table session; then
      log "schema v1 requested but the session table is missing: nothing captured"
      return 2
    fi
  fi
  SESSION_OBJ="json_object('id', s.id)"
  read_db "$WORK_DIR/cols.out" sql_lit "SELECT name FROM pragma_table_info('$ST') ORDER BY cid;"; rc=$?
  if (( rc == 0 )); then
    SESSION_OBJ=""
    while IFS= read -r c; do
      [[ "$c" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || { log "ignoring session column with an unusual name"; continue; }
      SESSION_OBJ+="${SESSION_OBJ:+, }'$c', s.$c"
    done <"$WORK_DIR/cols.out"
    SESSION_OBJ="json_object($SESSION_OBJ"
    have_table project && SESSION_OBJ+=", 'project_worktree', pr.worktree, 'project_name', pr.name"
    SESSION_OBJ+=")"
  else
    return "$rc"
  fi
  return 0
}
# A fallback DURING schema discovery (at the tables read or the columns read)
# would leave a schema assembled from two stores: the failed statement is NOT
# retried on the copy (the hook refuses: read_db returns 3) and discovery
# restarts from the beginning on the source now in use, so no statement of one
# schema runs against the other. A schema already determined from the primary
# before the switch is held to, exactly as after initialization: an archive
# copy of another schema is refused. (A switch at the probe, before discovery,
# reads everything from the fallback and needs no check.)
SCHEMA_AT_SWITCH=""
init_source_hook() { SCHEMA_AT_SWITCH="$SCHEMA"; return 1; }
SOURCE_HOOK=init_source_hook
load_schema; rc=$?
if (( rc == 3 )); then
  SOURCE_HOOK=""
  load_schema; rc=$?
  if (( rc == 0 )) && [[ -n "$SCHEMA_AT_SWITCH" && "$SCHEMA" != "$SCHEMA_AT_SWITCH" ]]; then
    log "fallback copy ($DB_FALLBACK) has schema $SCHEMA but the primary had $SCHEMA_AT_SWITCH; refusing the fallback, nothing further captured"
    emit_summary 0 0 1 0 0 0 0
    exit 1
  fi
fi
SOURCE_HOOK=""
if (( rc != 0 )); then
  (( STOP_REQUESTED )) && init_stop_exit
  (( rc == 1 )) && log "cannot read the schema of the store; giving up"
  emit_summary 0 0 1 0 0 0 0
  exit 1
fi
# After a source switch in the middle of a run the schema is read again from the
# fallback copy: a copy whose schema differs from the primary's (a v1 archive
# behind a v2 primary, in the days after a cutover) is refused with one log line
# and the run fails loudly; cursors never mix the two schemas.
fallback_schema_check() {
  local before="$SCHEMA" st_before="$ST" rc
  SOURCE_HOOK=""   # once per run: there is one fallback
  load_schema; rc=$?
  if (( rc != 0 )); then
    (( STOP_REQUESTED )) && return 1   # a signal during the refresh: read_db reports the stop (rc 2)
    log "fallback copy ($DB_FALLBACK): cannot read its schema; refusing the fallback, nothing further captured"
    emit_summary 0 0 1 0 0 0 0
    exit 1
  fi
  if [[ "$SCHEMA" != "$before" ]]; then
    log "fallback copy ($DB_FALLBACK) has schema $SCHEMA but the primary had $before; refusing the fallback, nothing further captured"
    emit_summary 0 0 1 0 0 0 0
    exit 1
  fi
  [[ "$ST" == "$st_before" ]] || { log "internal error: session table changed without a schema change"; exit 1; }
  return 0
}
SOURCE_HOOK=fallback_schema_check
# A row whose data is text and valid JSON (the only kind json_extract may see).
M_VALID="(typeof(m.data) = 'text' AND json_valid(m.data))"
# The migration floor (v2) is per session: max(time_updated) of the session's
# rows in the frozen v1 message table (one range of its (session_id,
# time_created, id) index), read in the session-level statement; 0 when the
# table is absent or the session has no v1 rows. MEMOIRE_OPENCODE_V2_FLOOR_MS
# replaces it for every session.
SESSION_FLOOR=0

# --- project resolution (shared resolver, fail-open to basename) -------------

MRP_KEY="" MRP_ROOT="" MRP_SOURCE=""
memoire_resolve_v() { # dir  (root/source ride the batch for identity-conflict detection)
  local out
  MRP_KEY="" MRP_ROOT="" MRP_SOURCE=""
  if out="$("$RESOLVER" --verbose "$1" 2>/dev/null)" && [[ -n "$out" ]]; then
    IFS=$'\t' read -r MRP_KEY MRP_ROOT MRP_SOURCE <<<"$out"
  fi
  if [[ -z "$MRP_KEY" ]]; then
    MRP_KEY="${1##*/}"
    MRP_ROOT="" MRP_SOURCE=""
  fi
}

declare -A CAPTURE_OFF_CACHE
capture_off() { # dir — off ONLY on an explicit "off" (fail-open)
  if [[ -z "${CAPTURE_OFF_CACHE[$1]:-}" ]]; then
    if [[ "$("$RESOLVER" --capture-enabled "$1" 2>/dev/null)" == "off" ]]; then
      CAPTURE_OFF_CACHE[$1]=1
    else
      CAPTURE_OFF_CACHE[$1]=0
    fi
  fi
  [[ "${CAPTURE_OFF_CACHE[$1]}" == "1" ]]
}

# --- spill files ---------------------------------------------------------------

TOOL_REAL=""
TOOL_FB_REAL=""
SP_REASON=""
SP_COPY="$WORK_DIR/spill.copy"
SP_SIZE=0
SPILL_TOTAL=0   # bytes attached so far in the batch being prepared
SPILL_CUT=0     # row number of the first part whose spill was deferred (0 = none)

# spill_probe PATH -> 0 with SP_REAL (the resolved file) and SP_SIZE, or 1 with
# SP_REASON (refused). PATH is a value from a row: it is used only after
# realpath and must resolve to a regular file inside the tool-output directory
# (or its archive copy), not larger than SPILL_MAX. No byte is read.
SP_REAL=""
spill_probe() {
  local p="$1" real size base
  SP_REASON="" SP_REAL="" SP_SIZE=0
  [[ "$p" == /* ]] || { SP_REASON=not_absolute; return 1; }
  if [[ -z "$TOOL_REAL" ]]; then
    TOOL_REAL="$(realpath -e -- "$TOOL_OUT_DIR" 2>/dev/null || true)"
    TOOL_FB_REAL="$(realpath -e -- "$TOOL_OUT_FALLBACK" 2>/dev/null || true)"
  fi
  [[ -n "$TOOL_REAL$TOOL_FB_REAL" ]] || { SP_REASON=no_tool_output_dir; return 1; }
  real="$(realpath -e -- "$p" 2>/dev/null)" || real=""
  if [[ -z "$real" ]]; then
    # Missing in the live directory: the archive copy keeps the same name.
    base="${p##*/}"
    if [[ -n "$TOOL_FB_REAL" && "$base" =~ ^[A-Za-z0-9_.-]+$ && "$base" != "." && "$base" != ".." \
          && ( "${p%/*}" == "$TOOL_OUT_DIR" || "${p%/*}" == "$TOOL_REAL" ) ]]; then
      real="$(realpath -e -- "$TOOL_FB_REAL/$base" 2>/dev/null)" || real=""
    fi
    [[ -n "$real" ]] || { SP_REASON=missing; return 1; }
  fi
  # An empty root must never match (an empty variable would make "/*" accept
  # every absolute path): test the roots explicitly.
  if [[ -n "$TOOL_REAL" && "$real" == "$TOOL_REAL"/* ]]; then :
  elif [[ -n "$TOOL_FB_REAL" && "$real" == "$TOOL_FB_REAL"/* ]]; then :
  else SP_REASON=outside_dir; return 1
  fi
  [[ -f "$real" && ! -L "$real" ]] || { SP_REASON=not_regular_file; return 1; }
  size="$(stat -c %s -- "$real" 2>/dev/null)" || { SP_REASON=read_failed; return 1; }
  [[ "$size" =~ ^[0-9]+$ ]] || { SP_REASON=read_failed; return 1; }
  (( size <= SPILL_MAX )) || { SP_REASON=too_large; return 1; }
  SP_REAL="$real" SP_SIZE="$size"
  return 0
}
# spill_copy -> 0 with the probed file ($SP_REAL, $SP_SIZE bytes) copied to
# $SP_COPY, read with dd iflag=nofollow,nonblock under timeout (a swapped-in
# pipe cannot block); 1 with SP_REASON=read_failed.
spill_copy() {
  local want
  rm -f "$SP_COPY"
  timeout 30 dd if="$SP_REAL" of="$SP_COPY" bs=1M iflag=nofollow,nonblock status=none 2>/dev/null \
    || { scratch_ok || scratch_fail "a spill copy"; SP_REASON=read_failed; return 1; }
  want="$(stat -c %s -- "$SP_COPY" 2>/dev/null)" || want=-1
  [[ "$want" == "$SP_SIZE" ]] || { scratch_ok || scratch_fail "a spill copy"; SP_REASON=read_failed; return 1; }
  return 0
}

# spill_prepare ROWS_JSONL -> $SPILL_MAP: {"<key>": {spill_output,spill_bytes}
# | {spill_rejected} | {spill_deferred}}, and SPILL_CUT. rc 1: could not prepare.
# The ROW is the unit of the batch cap: all the admissible spills of one row go
# into the same batch, or the whole row waits. A row alone may exceed the cap
# (it is then read alone, at the head of a batch); a row that would push the
# batch over the cap after rows already carrying spill is deferred (SPILL_CUT =
# its row number) and the batch ends BEFORE it. A v1 part row carries one spill
# (keyed by the part id); a v2 message row carries one per tool item (keyed by
# <message id>:c<item>). The v1 mapping emits the deferred part with the
# :deferred marker (it is a row of its own, re-read next batch); the v2 mapping
# emits nothing of a deferred row (every item of the row waits with it: an id
# is never emitted before its outcome is final).
spill_prepare() {
  local n b64id b64path pid ppath rc rown="" key path lines=() i j rowsum
  : >"$SPILL_PATCH"
  echo '{}' >"$SPILL_MAP"
  SPILL_TOTAL=0 SPILL_CUT=0
  grep -q -E '"spill":"|"spills":\[\{' "$1" 2>/dev/null || return 0
  if ! jq -r '. as $r
      | (select($r.k == "p" and ($r.spill | type) == "string" and $r.spill != "") | [($r.n | tostring), ($r.id | @base64), ($r.spill | @base64)]),
        (select($r.k == "s" and ($r.spills | type) == "array") | $r.spills[] | select((.path | type) == "string" and .path != "")
         | [($r.n | tostring), (($r.id + ":c" + (.i | tostring)) | @base64), (.path | @base64)])
      | @tsv' \
      "$1" >"$SPILL_TSV" 2>/dev/null; then
    (( STOP_REQUESTED )) && return 1
    scratch_ok || scratch_fail "the spill list"
    log "spill list failed"
    return 1
  fi
  mapfile -t lines <"$SPILL_TSV"
  i=0
  while (( i < ${#lines[@]} )); do
    # the group of lines of one row: [i, j)
    IFS=$'\t' read -r rown _ _ <<<"${lines[i]}"
    [[ "$rown" =~ ^[0-9]+$ ]] || { log "spill list: malformed row number"; return 1; }
    j=$i
    while (( j < ${#lines[@]} )); do
      IFS=$'\t' read -r n _ _ <<<"${lines[j]}"
      [[ "$n" == "$rown" ]] || break
      j=$(( j + 1 ))
    done
    # first pass: the admissible bytes of the row (no byte read)
    rowsum=0
    local -a probe_rc=() probe_reason=() probe_real=() probe_size=() probe_key=()
    local idx=0
    for (( k = i; k < j; k++ )); do
      IFS=$'\t' read -r n b64id b64path <<<"${lines[k]}"
      key="$(printf '%s' "$b64id" | base64 -d 2>/dev/null)"
      path="$(printf '%s' "$b64path" | base64 -d 2>/dev/null)"
      spill_probe "$path"; rc=$?
      probe_rc[idx]=$rc; probe_reason[idx]="$SP_REASON"; probe_real[idx]="$SP_REAL"; probe_size[idx]="$SP_SIZE"; probe_key[idx]="$key"
      (( rc == 0 )) && rowsum=$(( rowsum + SP_SIZE ))
      idx=$(( idx + 1 ))
    done
    # the row waits when the batch already carries spill and this row would push it over the cap
    if (( rowsum > 0 && SPILL_TOTAL > 0 && SPILL_TOTAL + rowsum > SPILL_BATCH )); then
      jq -nc --arg id "${probe_key[0]}" '{id: $id, spill_deferred: true}' >>"$SPILL_PATCH" || return 1
      SPILL_CUT="$rown"
      break
    fi
    # second pass: attach the admissible ones, record the refusals
    for (( idx = 0; idx < ${#probe_key[@]}; idx++ )); do
      pid="${probe_key[idx]}"
      if (( probe_rc[idx] == 0 )); then
        SP_REAL="${probe_real[idx]}" SP_SIZE="${probe_size[idx]}"
        if spill_copy; then
          SPILL_TOTAL=$(( SPILL_TOTAL + SP_SIZE ))
          jq -nc --arg id "$pid" --rawfile out "$SP_COPY" --argjson n "$SP_SIZE" \
            '{id: $id, spill_output: $out, spill_bytes: $n}' >>"$SPILL_PATCH" \
            || { (( STOP_REQUESTED )) && return 1; scratch_ok || scratch_fail "the spill patch"; return 1; }
        else
          (( STOP_REQUESTED )) && return 1
          jq -nc --arg id "$pid" --arg r "$SP_REASON" '{id: $id, spill_rejected: $r}' >>"$SPILL_PATCH" || return 1
        fi
      else
        jq -nc --arg id "$pid" --arg r "${probe_reason[idx]}" '{id: $id, spill_rejected: $r}' >>"$SPILL_PATCH" || return 1
      fi
    done
    i=$j
  done
  rm -f "$SP_COPY"
  if ! jq -sc 'map({(.id): .}) | add // {}' "$SPILL_PATCH" >"$SPILL_MAP" 2>/dev/null; then
    (( STOP_REQUESTED )) && return 1
    scratch_ok || scratch_fail "the spill map"
    log "spill map failed"
    return 1
  fi
  return 0
}

# --- the mapping: rows -> queue items (jq) ------------------------------------
# Input: one JSON object per row (built in SQL). Output: one JSON object per
# event, ready for `memoire-queue -batch-file`, with _row = the row's number in
# the batch (the cursor holds at the row before a failed item).

read -r -d '' JQ_LIB <<'JQ' || true
def pad16: tostring | ("0000000000000000" + .)[-16:];
def nz: if type == "number" then . else 0 end;
def iso: . as $ms | (($ms / 1000) | floor) as $s
  | ($s | strftime("%Y-%m-%dT%H:%M:%S")) + "." + (("000" + (($ms % 1000) | tostring))[-3:]) + "Z";
def obj: if type == "object" then . else {raw: .} end;
def hexval: if . >= 97 then . - 87 elif . >= 65 then . - 55 else . - 48 end;
def b64: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/" as $a
  | [explode[] | hexval] as $h
  | [range(0; ($h | length); 2) as $i | $h[$i] * 16 + $h[$i + 1]] as $b
  | [range(0; ($b | length); 3) as $i | $b[$i:$i + 3] as $t
     | ($t[0] * 65536 + ($t[1] // 0) * 256 + ($t[2] // 0)) as $v
     | [$a[($v / 262144 | floor) % 64:][:1], $a[($v / 4096 | floor) % 64:][:1],
        (if ($t | length) > 1 then $a[($v / 64 | floor) % 64:][:1] else "=" end),
        (if ($t | length) > 2 then $a[$v % 64:][:1] else "=" end)] | join("")] | join("");
def rowdata: if .blobhex != null then {blob: true, raw_blob: (.blobhex | b64)} else (.data | obj) end;
def wrap($n; $type; $id; $at; $payload; $text):
  {_row: $n, project: $project, session_id: $sid, source_adapter: $adapter,
   event_type: $type, source_event_id: $id, occurred_at: $at, cwd: $cwd, payload: ($payload + {schema: $schema})}
  + (if $text != null then {text: $text} else {} end)
  + (if $root != "" then {project_root: $root, project_source: $psrc} else {} end);
JQ

read -r -d '' JQ_EVENTS <<'JQ' || true
def message_events:
  . as $r | ($r | rowdata) as $d | ($r.tc | nz) as $tc | ($r.omu | nz) as $omu
  | ([$omu, ($r.sfmu | nz)] | max) as $v
  | wrap($r.n; "opencode_message"; "opencode:message:" + $r.id + ":u" + ($v | pad16); ($tc | iso);
      {session_id: $sid, message_id: $r.id, time_created: $tc, time_updated: $omu,
       message: $d,
       step_finish: (($r.sf // []) | map(if type == "object" then del(.type) else . end)),
       backfill_generation: $g}; null),
    (select($r.turn != null and (($doneset[0] // {})[$r.id] | not))
     | ($d.time.completed // 0 | nz) as $comp
     | wrap($r.n; "opencode_assistant_turn"; "opencode:turn:" + $r.id; ($comp | iso);
         {session_id: $sid, message_id: $r.id, completed: $comp, backfill_generation: $g}; $r.turn));

def part_events:
  . as $r | ($r | rowdata) as $d | ($r.tc | nz) as $tc | ($r.mu | nz) as $mu
  | ($r.role // "") as $role
  | ("opencode:part:" + $r.id + ":u" + ($mu | pad16)) as $pid
  | ($tc | iso) as $at
  | {session_id: $sid, part_id: $r.id, message_id: $r.mid,
     time_created: $tc, time_updated: $mu, backfill_generation: $g} as $base
  | ($d.type // "") as $ty
  | if ($r.id | startswith($mprefix)) then empty   # written by the Memoire plugin: read, cursor moves, no event
    elif $ty == "step-start" or $ty == "step-finish" then empty
    elif $ty == "text" then
      (if ($d.synthetic == true or $d.synthetic == 1) then
         wrap($r.n; "opencode_item"; $pid; $at; $base + {synthetic: true, text: ($d.text // ""), part: $d}; null)
       elif $role == "user" then
         wrap($r.n; "opencode_user_prompt"; $pid; $at; $base + {text: ($d.text // ""), part: $d}; null)
       elif $role == "assistant" then
         wrap($r.n; "opencode_assistant_message"; $pid; $at; $base + {text: ($d.text // ""), part: $d}; null)
       else
         wrap($r.n; "opencode_item"; $pid; $at; $base + {text: ($d.text // ""), part: $d}; null)
       end)
    elif $ty == "reasoning" then
      wrap($r.n; "opencode_reasoning"; $pid; $at; $base + {text: ($d.text // ""), part: $d}; null)
    elif $ty == "patch" or $ty == "compaction" or $ty == "file" then
      wrap($r.n; "opencode_" + $ty; $pid; $at; $base + {part: $d}; null)
    elif $ty == "tool" then
      ($d.state | obj) as $st
      | ($d | del(.state)) as $pk
      | (($spmap[0] // {})[$r.id]) as $sp
      | (if $sp != null and ($sp | has("spill_output")) then
           {spill_attached: true, spill_bytes: $sp.spill_bytes, spill_output: $sp.spill_output}
         elif $sp != null and ($sp | has("spill_deferred")) then
           {truncated_by_harness: true, spill_deferred: true}
         elif $sp != null then
           {truncated_by_harness: true, spill_rejected: $sp.spill_rejected}
         elif (($st.metadata | if type == "object" then .truncated else null end) == true) then
           {truncated_by_harness: true}
         else {} end) as $extra
      | (if $sp != null and ($sp | has("spill_deferred")) then ":deferred" else ":outcome" end) as $osuf
      | wrap($r.n; "opencode_tool_call"; $pid; $at;
          $base + {tool: ($d.tool // ""), callID: ($d.callID // ""),
                   state: {input: $st.input, title: $st.title, status: $st.status, time: $st.time},
                   part: $pk}; null),
        wrap($r.n; (if $st.status == "error" then "opencode_tool_failure" else "opencode_tool_result" end);
          $pid + $osuf; $at;
          $base + {tool: ($d.tool // ""), callID: ($d.callID // ""), status: ($st.status // ""),
                   state_out: $d.state, part: $pk} + $extra; null)
    else
      wrap($r.n; "opencode_item"; $pid; $at; $base + {part: $d}; null)
    end;

(if .bad == 1 then empty elif .k == "m" then message_events else part_events end)
| select($cutrow == 0 or ._row <= $cutrow)
JQ

# The v2 mapping: one session_message row -> the record event (the row minus
# its content fields) plus one event per content field or item, addressed by
# position; see "Versioned ids" in the header. Rows: {n, k:"s", id, type, seq,
# tc, mu, data|blobhex, turn, spills:[{i, path}]}.
read -r -d '' JQ_EVENTS_V2 <<'JQ' || true
def v2_tool($r; $base; $cid; $at; $i; $c):
  ($c.state | obj) as $st | ($c | del(.state)) as $pk
  | (($spmap[0] // {})[$r.id + ":c" + ($i | tostring)]) as $sp
  | (if $sp != null and ($sp | has("spill_output")) then
       {spill_attached: true, spill_bytes: $sp.spill_bytes, spill_output: $sp.spill_output}
     elif $sp != null and ($sp | has("spill_rejected")) then
       {truncated_by_harness: true, spill_rejected: $sp.spill_rejected}
     elif (($st.metadata | if type == "object" then .truncated else null end) == true) then
       {truncated_by_harness: true}
     else {} end) as $extra
  | wrap($r.n; "opencode_tool_call"; $cid; $at;
      $base + {index: $i, tool: ($c.name // ""), callID: ($c.id // ""),
               state: {input: $st.input, status: $st.status, time: $c.time}, part: $pk}; null),
    wrap($r.n; (if $st.status == "error" then "opencode_tool_failure" else "opencode_tool_result" end);
      $cid + ":outcome"; $at;
      $base + {index: $i, tool: ($c.name // ""), callID: ($c.id // ""), status: ($st.status // ""),
               state_out: $c.state, part: $pk} + $extra; null);

def v2_events:
  . as $r | ($r | rowdata) as $d | ($r.tc | nz) as $tc | ($r.mu | nz) as $mu
  | ($r.type // "") as $ty
  | ("opencode:message:" + $r.id + ":u" + ($mu | pad16)) as $mid
  | ($tc | iso) as $at
  | {session_id: $sid, message_id: $r.id, message_type: $ty, seq: $r.seq,
     time_created: $tc, time_updated: $mu, backfill_generation: $g} as $base
  | (($d.blob == true) or ($d | has("raw"))) as $blob   # a BLOB or a non-JSON row: the record event only
  | (if $blob then $d
     elif $ty == "user" then ($d | del(.text, .files)) + {files_count: (($d.files // []) | length)}
     elif $ty == "assistant" then ($d | del(.content)) + {content_items: (($d.content // []) | length)}
     elif $ty == "synthetic" or $ty == "system" then ($d | del(.text))
     elif $ty == "compaction" then ($d | del(.summary, .recent))
     else $d end) as $meta
  | wrap($r.n; "opencode_message"; $mid; $at; $base + {message: $meta}; null),
    (select($blob | not)
     | if $ty == "user" then
         wrap($r.n; "opencode_user_prompt"; $mid + ":text"; $at;
           $base + {text: ($d.text // ""),
                    files: (($d.files // []) | map(if type == "object" then (del(.data) + (if has("data") then {data_bytes: (.data | tostring | length)} else {} end)) else . end))}; null),
         (($d.files // []) | to_entries[]
          | wrap($r.n; "opencode_file"; $mid + ":f" + (.key | tostring); $at; $base + {index: .key, file: .value}; null))
       elif $ty == "assistant" then
         (($d.content // []) | to_entries[] | .key as $i | .value as $c
          | ($mid + ":c" + ($i | tostring)) as $cid
          | ($c | if type == "object" then .type else null end // "") as $ct
          | if $ct == "text" then
              wrap($r.n; "opencode_assistant_message"; $cid; $at; $base + {index: $i, text: ($c.text // ""), part: $c}; null)
            elif $ct == "reasoning" then
              wrap($r.n; "opencode_reasoning"; $cid; $at; $base + {index: $i, text: ($c.text // ""), part: $c}; null)
            elif $ct == "tool" then v2_tool($r; $base; $cid; $at; $i; $c)
            else wrap($r.n; "opencode_item"; $cid; $at; $base + {index: $i, part: $c}; null)
            end),
         (select($r.turn != null and (($doneset[0] // {})[$r.id] | not))
          | ($d.time.completed // 0 | nz) as $comp
          | wrap($r.n; "opencode_assistant_turn"; "opencode:turn:" + $r.id; ($comp | iso);
              {session_id: $sid, message_id: $r.id, completed: $comp, backfill_generation: $g}; $r.turn))
       elif $ty == "synthetic" or $ty == "system" then
         wrap($r.n; "opencode_item"; $mid + ":text"; $at;
           $base + {($ty): true, text: ($d.text // "")} + (if $d.description != null then {description: $d.description} else {} end); null)
       elif $ty == "compaction" then
         wrap($r.n; "opencode_compaction"; $mid + ":compaction"; $at; $base + {part: $d}; null)
       else empty end);

# a deferred row (the cut) emits nothing: it is read again at the head of the next batch
(if .bad == 1 then empty else v2_events end)
| select($cutrow == 0 or ._row < $cutrow)
JQ

# Session-level items (aux read): facts, todo snapshot, session_input, context
# epoch, permission. Last output line: {_state: ...} = the versions to store.
read -r -d '' JQ_AUX <<'JQ' || true
[inputs] as $rows
| ($rows | map(select(.a == "dir")) | .[0]) as $dir
| ($rows | map(select(.a == "todo")) | .[0]) as $todo
| ($rows | map(select(.a == "input"))) as $inputs
| ($rows | map(select(.a == "epoch")) | .[0]) as $epoch
| ($rows | map(select(.a == "perm"))) as $perms
| ($dir.tu | nz) as $tu
| (
    (select($tu != $curfacts)
      | wrap(0; "opencode_session_facts"; "opencode:session:" + $sid + ":facts:u" + ($tu | pad16);
          (($dir.tc | nz) | iso);
          {session_id: $sid, session: $dir.row, project_directories: ($dir.dirs // []), backfill_generation: $g}; null)),
    (select($todo != null and ($todo.count | nz) > 0)
      | (($todo.max | nz | pad16) + "n" + ($todo.count | tostring)) as $ver
      | select($ver != $curtodo)
      | wrap(0; "opencode_todo"; "opencode:session:" + $sid + ":todo:u" + $ver; (($todo.max | nz) | iso);
          {session_id: $sid, todos: $todo.rows, count: $todo.count, backfill_generation: $g}; null)),
    ($inputs[] | select((.row.time_created | nz) > $curinputs)
      | wrap(0; "opencode_session_input"; "opencode:session_input:" + .row.id + ":u" + ((.row.time_created | nz) | pad16);
          ((.row.time_created | nz) | iso);
          {session_id: $sid, input: .row, backfill_generation: $g}; null)),
    (select($epoch != null)
      | ("opencode:context_epoch:" + $sid + ":u" + (($epoch.row.baseline_seq | nz) | pad16) + ":" + $epochhash) as $eid
      | select($eid != $curepoch)
      | wrap(0; "opencode_context_epoch"; $eid; (($dir.tc | nz) | iso);
          {session_id: $sid, epoch: $epoch.row, backfill_generation: $g}; null)),
    ($perms[] | select((.row.time_updated | nz) > $curperm)
      | wrap(0; "opencode_permission";
          "opencode:permission:" + .row.id + ":" + $sid + ":u" + ((.row.time_updated | nz) | pad16);
          ((.row.time_created | nz) | iso);
          {session_id: $sid, permission: .row, backfill_generation: $g}; null)),
    {_state: {
       facts_mu: $tu,
       todo: (if $todo != null and ($todo.count | nz) > 0 then (($todo.max | nz | pad16) + "n" + ($todo.count | tostring)) else $curtodo end),
       inputs_mu: ([$curinputs] + ($inputs | map(.row.time_created | nz)) | max),
       epoch_id: (if $epoch != null then ("opencode:context_epoch:" + $sid + ":u" + (($epoch.row.baseline_seq | nz) | pad16) + ":" + $epochhash) else $curepoch end),
       perm_mu: ([$curperm] + ($perms | map(.row.time_updated | nz)) | max),
       smcount: (($rows | map(select(.a == "smcount")) | .[0].n) // 0)}}
  )
JQ

JQ_COMMON_ARGS=()   # filled per session: --arg project ... (see set_jq_args)
set_jq_args() {
  JQ_COMMON_ARGS=(--arg project "$MRP_KEY" --arg sid "$SID" --arg adapter "$ADAPTER"
                  --arg cwd "$SESS_DIR" --arg root "${MRP_ROOT:-}" --arg psrc "${MRP_SOURCE:-}"
                  --arg g "$GENERATION" --arg mprefix "$MEMOIRE_PART_PREFIX" --arg schema "$SCHEMA")
}

# --- cursor --------------------------------------------------------------------
# One file per session, {v: 3, p: <slot>, f: <slot>, turns: [...]}. A slot is the
# state of one SOURCE (p primary, f fallback); SRC says which one this run reads.

cursor_file() { printf '%s/%s.json' "$CURSOR_DIR" "$1"; }
cursor_lock_file() { printf '%s/%s.lock' "$CURSOR_DIR" "$1"; }

CUR_MU=0 CUR_K="" CUR_IX="" CUR_FACTS=0 CUR_TODO="" CUR_INPUTS=0 CUR_EPOCH="" CUR_PERM=0 CUR_DSIG=""
reset_cursor() {
  CUR_MU=0 CUR_K="" CUR_IX="" CUR_FACTS=0 CUR_TODO="" CUR_INPUTS=0 CUR_EPOCH="" CUR_PERM=0 CUR_DSIG=""
  echo '{}' >"$TURNS_FILE"
  echo '{}' >"$OTHER_SLOT"
}
read_cursor() { # session id; loads the slot of the source in use. A cursor file is an input: every field is validated
  reset_cursor
  local f line
  f="$(cursor_file "$1")"
  [[ -f "$f" ]] || return 0
  # every field is prefixed with x: `read` with a tab IFS collapses empty fields
  line="$(jq -r --arg s "$SRC" 'select(.v == 3) | (.[$s] // {}) as $c
      | ["x" + ($c.mu // 0 | tostring), "x" + ($c.k // ""), "x" + ($c.idhex // ""),
         "x" + ($c.facts_mu // 0 | tostring), "x" + ($c.todo // ""), "x" + ($c.inputs_mu // 0 | tostring),
         "x" + ($c.epoch_id // ""), "x" + ($c.perm_mu // 0 | tostring), "x" + ($c.done_sig // "")] | @tsv' "$f" 2>/dev/null)" || line=""
  if [[ -z "$line" ]]; then
    log "$1: cursor unreadable or of another version, restarting the session (ids are stable)"
    return 0
  fi
  local mu k ix facts todo inputs epoch perm dsig
  IFS=$'\t' read -r mu k ix facts todo inputs epoch perm dsig <<<"$line"
  mu="${mu#x}" k="${k#x}" ix="${ix#x}" facts="${facts#x}" todo="${todo#x}" inputs="${inputs#x}"
  epoch="${epoch#x}" perm="${perm#x}" dsig="${dsig#x}"
  if [[ "$mu" =~ ^[0-9]+$ && "$k" =~ ^[mps]?$ && "$ix" =~ ^[0-9A-F]*$ && "$facts" =~ ^[0-9]+$ \
        && "$todo" =~ ^[0-9n]*$ && "$inputs" =~ ^[0-9]+$ && "$epoch" =~ ^[A-Za-z0-9_.:-]*$ \
        && "$perm" =~ ^[0-9]+$ && "$dsig" =~ ^[0-9A-Za-z./_-]*$ ]]; then
    CUR_MU="$mu" CUR_K="$k" CUR_IX="$ix" CUR_FACTS="$facts" CUR_TODO="$todo" CUR_INPUTS="$inputs"
    CUR_EPOCH="$epoch" CUR_PERM="$perm" CUR_DSIG="$dsig"
    jq -c '(.turns // []) | map(select(type == "string") | {(.): true}) | add // {}' "$f" >"$TURNS_FILE" 2>/dev/null \
      || echo '{}' >"$TURNS_FILE"
    jq -c --arg s "$SRC" 'if $s == "p" then (.f // {}) else (.p // {}) end | if type == "object" then . else {} end' "$f" >"$OTHER_SLOT" 2>/dev/null \
      || echo '{}' >"$OTHER_SLOT"
  else
    log "$1: cursor holds invalid values, restarting the session"
    reset_cursor
  fi
}

CURSOR_FAILED=0
BATCHES_DONE=0
write_cursor() { # session id; state in CUR_* and $TURNS_FILE; the other slot is kept as read. tmp + rename.
  [[ "$DRY_RUN" == "1" ]] && return 0
  local f tmp try
  f="$(cursor_file "$1")"
  for try in 1 2 3; do
    tmp="$(mktemp "$f.tmp.XXXXXX" 2>/dev/null)" || { CURSOR_FAILED=1; return 1; }
    if jq -n --arg sid "$1" --arg src "$SRC" --argjson mu "$CUR_MU" --arg k "$CUR_K" --arg ix "$CUR_IX" \
          --argjson facts "$CUR_FACTS" --arg todo "$CUR_TODO" --argjson inputs "$CUR_INPUTS" \
          --arg epoch "$CUR_EPOCH" --argjson perm "$CUR_PERM" --arg dsig "$CUR_DSIG" \
          --slurpfile other "$OTHER_SLOT" --slurpfile turns "$TURNS_FILE" --arg u "$(date -Is)" --arg schema "$SCHEMA" \
          '{mu: $mu, k: $k, idhex: $ix, facts_mu: $facts, todo: $todo, inputs_mu: $inputs,
            epoch_id: $epoch, perm_mu: $perm, done_sig: $dsig, schema: $schema} as $cur
           | {v: 3, session_id: $sid}
             + (if $src == "p" then {p: $cur, f: ($other[0] // {})} else {f: $cur, p: ($other[0] // {})} end)
             + {turns: (($turns[0] // {}) | keys), updated_at: $u}' >"$tmp" 2>/dev/null \
       && mv -f "$tmp" "$f" 2>/dev/null; then
      BATCHES_DONE=$((BATCHES_DONE + 1))
      return 0
    fi
    rm -f "$tmp" 2>/dev/null
    # A group signal may have killed this jq: the signal is spent, try again.
    (( STOP_REQUESTED )) || break
  done
  CURSOR_FAILED=1
  return 1
}

# --- SQL builders (validated literals only) -----------------------------------

P_TYPE="CASE WHEN (typeof(p.data) = 'text' AND json_valid(p.data)) THEN json_extract(p.data, '\$.type') END"
P_STATUS="CASE WHEN (typeof(p.data) = 'text' AND json_valid(p.data)) THEN json_extract(p.data, '\$.state.status') END"
P_TEXT="CASE WHEN (typeof(p.data) = 'text' AND json_valid(p.data)) THEN json_extract(p.data, '\$.text') END"

# An id taken from a row is used only when it matches [A-Za-z0-9_.-]{1,100}
# (no NUL: octet_length counts every byte, length() stops at a NUL).
idok() { printf "(%s NOT GLOB '*[^A-Za-z0-9_.-]*' AND %s <> '' AND octet_length(%s) = length(%s) AND octet_length(%s) <= 100)" "$1" "$1" "$1" "$1" "$1"; }

read -r -d '' CHUNK_SQL <<'SQL' || true
WITH mp AS MATERIALIZED (
  SELECT m.id AS id,
         max(m.time_updated, COALESCE((SELECT max(p.time_updated) FROM part p WHERE p.message_id = m.id), 0)) AS mu
  FROM message m WHERE m.session_id = '@SID@'),
cand AS MATERIALIZED (
  SELECT mp.mu AS mu, 'm' AS k, mp.id AS id, NULL AS mid,
         (SELECT octet_length(m.data) FROM message m WHERE m.id = mp.id)
           + COALESCE((SELECT sum(octet_length(p.data)) FROM part p WHERE p.message_id = mp.id), 0) AS len
  FROM mp WHERE (mp.mu, 'm', hex(mp.id)) > (@CMU@, '@CK@', '@CIX@') AND mp.mu <= @CUT@
  UNION ALL
  SELECT p.time_updated, 'p', p.id, p.message_id, octet_length(p.data)
  FROM part p WHERE p.session_id = '@SID@' AND (p.time_updated, 'p', hex(p.id)) > (@CMU@, '@CK@', '@CIX@')
    AND p.time_updated <= @CUT@
  ORDER BY 1, 2, 3 LIMIT @LIM@),
sized AS (
  SELECT *, COALESCE(sum(len) OVER (ORDER BY mu, k, id ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING), 0) AS before
  FROM cand),
chosen AS MATERIALIZED (
  SELECT row_number() OVER (ORDER BY mu, k, id) AS n, mu, k, id, mid,
         CASE WHEN k = 'm' THEN @IDOK_C_ID@ ELSE (@IDOK_C_ID@ AND @IDOK_C_MID@) END AS ok
  FROM sized WHERE before < @BUDGET@),
roles AS MATERIALIZED (
  SELECT m.id AS mid, CASE WHEN (typeof(m.data) = 'text' AND json_valid(m.data)) THEN json_extract(m.data, '$.role') END AS role
  FROM message m WHERE m.id IN (SELECT mid FROM chosen WHERE k = 'p' AND ok)),
turns AS MATERIALIZED (
  SELECT c.id AS id,
    CASE WHEN (typeof(m.data) = 'text' AND json_valid(m.data)) THEN
      CASE WHEN json_extract(m.data, '$.role') = 'assistant' AND COALESCE(json_extract(m.data, '$.time.completed'), 0) <> 0 THEN
        CASE WHEN EXISTS (SELECT 1 FROM part p WHERE p.message_id = m.id AND @PTYPE@ = 'tool' AND @PSTATUS@ IN ('pending', 'running'))
          THEN NULL
        ELSE (SELECT group_concat(x.t, char(10) || char(10) ORDER BY x.tc, x.id)
              FROM (SELECT @PTEXT@ AS t, p.time_created AS tc, p.id AS id FROM part p
                    WHERE p.message_id = m.id AND @PTYPE@ = 'text' AND COALESCE(@PTEXT@, '') <> ''
                      AND substr(p.id, 1, @MPLEN@) <> '@MPFX@') x)
        END
      END
    END AS t
  FROM chosen c JOIN message m ON m.id = c.id WHERE c.k = 'm' AND c.ok)
SELECT c.n || char(9) || c.mu || char(9) || c.k || char(9) || hex(c.id) || char(9)
  || CASE WHEN c.k = 'm' AND c.ok AND EXISTS (SELECT 1 FROM turns t WHERE t.id = c.id AND t.t IS NOT NULL) THEN 1 ELSE 0 END || char(9)
  || CASE WHEN c.ok THEN c.id ELSE '' END || char(9)
  || CASE WHEN c.ok AND ((c.k = 'm' AND EXISTS (SELECT 1 FROM message m WHERE m.id = c.id AND typeof(m.data) = 'blob'))
                      OR (c.k = 'p' AND EXISTS (SELECT 1 FROM part p WHERE p.id = c.id AND typeof(p.data) = 'blob'))) THEN 1 ELSE 0 END || char(9)
  || CASE WHEN NOT c.ok THEN json_object('n', c.n, 'k', c.k, 'bad', 1)
     WHEN c.k = 'm' THEN (SELECT json_object('n', c.n, 'k', 'm', 'id', m.id, 'tc', m.time_created,
              'omu', m.time_updated, 'mu', c.mu,
              'blobhex', CASE WHEN typeof(m.data) = 'blob' THEN hex(m.data) END,
              'data', CASE WHEN typeof(m.data) = 'blob' THEN json('null') WHEN (typeof(m.data) = 'text' AND json_valid(m.data)) THEN json(m.data) ELSE json_object('raw', m.data) END,
              'sf', json((SELECT json_group_array(json(x.d) ORDER BY x.tc, x.id)
                          FROM (SELECT p.data AS d, p.time_created AS tc, p.id AS id FROM part p
                                WHERE p.message_id = m.id AND @PTYPE@ = 'step-finish') x)),
              'sfmu', (SELECT max(p.time_updated) FROM part p WHERE p.message_id = m.id AND @PTYPE@ = 'step-finish'),
              'turn', (SELECT t.t FROM turns t WHERE t.id = m.id))
            FROM message m WHERE m.id = c.id)
       ELSE (SELECT json_object('n', c.n, 'k', 'p', 'id', p.id, 'mid', p.message_id,
              'tc', p.time_created, 'mu', p.time_updated,
              'role', (SELECT r.role FROM roles r WHERE r.mid = p.message_id),
              'blobhex', CASE WHEN typeof(p.data) = 'blob' THEN hex(p.data) END,
              'data', CASE WHEN typeof(p.data) = 'blob' THEN json('null') WHEN (typeof(p.data) = 'text' AND json_valid(p.data)) THEN json(p.data) ELSE json_object('raw', p.data) END,
              'spill', CASE WHEN (typeof(p.data) = 'text' AND json_valid(p.data)) THEN json_extract(p.data, '$.state.metadata.outputPath') END)
            FROM part p WHERE p.id = c.id)
     END
FROM chosen c ORDER BY c.n;
SQL

# The v2 chunk: the session_message rows of one session after the cursor, at
# or below the quiet cut and above the migration floor, in (time_updated, 's',
# hex(id)) order; same bounds (rows, bytes) as v1. The turn text and the spill
# list of a row are computed in SQL from the content array of the row.
read -r -d '' CHUNK_SQL_V2 <<'SQL' || true
WITH cand AS MATERIALIZED (
  SELECT m.time_updated AS mu, 's' AS k, m.id AS id, octet_length(m.data) AS len
  FROM session_message m
  WHERE m.session_id = '@SID@' AND (m.time_updated, 's', hex(m.id)) > (@CMU@, '@CK@', '@CIX@')
    AND m.time_updated <= @CUT@ AND m.time_updated > @FLOOR@
  ORDER BY 1, 2, 3 LIMIT @LIM@),
sized AS (
  SELECT *, COALESCE(sum(len) OVER (ORDER BY mu, k, id ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING), 0) AS before
  FROM cand),
chosen AS MATERIALIZED (
  SELECT row_number() OVER (ORDER BY mu, k, id) AS n, mu, k, id, @IDOK_C_ID@ AS ok
  FROM sized WHERE before < @BUDGET@),
turns AS MATERIALIZED (
  SELECT c.id AS id,
    CASE WHEN @MVALID@ AND m.type = 'assistant' AND COALESCE(json_extract(m.data, '$.time.completed'), 0) <> 0 THEN
      CASE WHEN EXISTS (SELECT 1 FROM json_each(m.data, '$.content') j
                        WHERE json_extract(j.value, '$.type') = 'tool'
                          AND json_extract(j.value, '$.state.status') IN ('pending', 'running', 'streaming'))
        THEN NULL
      ELSE (SELECT group_concat(x.t, char(10) || char(10) ORDER BY x.k)
            FROM (SELECT json_extract(j.value, '$.text') AS t, j.key AS k FROM json_each(m.data, '$.content') j
                  WHERE json_extract(j.value, '$.type') = 'text' AND COALESCE(json_extract(j.value, '$.text'), '') <> '') x)
      END
    END AS t
  FROM chosen c JOIN session_message m ON m.id = c.id WHERE c.ok)
SELECT c.n || char(9) || c.mu || char(9) || c.k || char(9) || hex(c.id) || char(9)
  || CASE WHEN c.ok AND EXISTS (SELECT 1 FROM turns t WHERE t.id = c.id AND t.t IS NOT NULL) THEN 1 ELSE 0 END || char(9)
  || CASE WHEN c.ok THEN c.id ELSE '' END || char(9)
  || CASE WHEN c.ok AND EXISTS (SELECT 1 FROM session_message m WHERE m.id = c.id AND typeof(m.data) = 'blob') THEN 1 ELSE 0 END || char(9)
  || CASE WHEN NOT c.ok THEN json_object('n', c.n, 'k', c.k, 'bad', 1)
     ELSE (SELECT json_object('n', c.n, 'k', 's', 'id', m.id, 'type', m.type, 'seq', m.seq,
              'tc', m.time_created, 'mu', m.time_updated,
              'blobhex', CASE WHEN typeof(m.data) = 'blob' THEN hex(m.data) END,
              'data', CASE WHEN typeof(m.data) = 'blob' THEN json('null') WHEN @MVALID@ THEN json(m.data) ELSE json_object('raw', m.data) END,
              'turn', (SELECT t.t FROM turns t WHERE t.id = m.id),
              'spills', CASE WHEN @MVALID@ AND m.type = 'assistant' THEN
                json((SELECT json_group_array(json_object('i', j.key, 'path', json_extract(j.value, '$.state.metadata.outputPath')))
                      FROM json_each(m.data, '$.content') j
                      WHERE json_extract(j.value, '$.type') = 'tool' AND json_extract(j.value, '$.state.metadata.outputPath') IS NOT NULL))
                ELSE json('[]') END)
           FROM session_message m WHERE m.id = c.id)
     END
FROM chosen c ORDER BY c.n;
SQL

chunk_sql() { # sid cmu ck cix lim budget cut
  local q
  if [[ "$SCHEMA" == "v2" ]]; then
    q="$CHUNK_SQL_V2"
    q="${q//@MVALID@/$M_VALID}"; q="${q//@FLOOR@/$SESSION_FLOOR}"
  else
    q="$CHUNK_SQL"
    q="${q//@PTYPE@/$P_TYPE}"; q="${q//@PSTATUS@/$P_STATUS}"; q="${q//@PTEXT@/$P_TEXT}"
    q="${q//@IDOK_C_MID@/$(idok mid)}"
    q="${q//@MPLEN@/${#MEMOIRE_PART_PREFIX}}"; q="${q//@MPFX@/$MEMOIRE_PART_PREFIX}"
  fi
  q="${q//@IDOK_C_ID@/$(idok id)}"
  q="${q//@SID@/$1}"; q="${q//@CMU@/$2}"; q="${q//@CK@/$3}"; q="${q//@CIX@/$4}"
  q="${q//@LIM@/$5}"; q="${q//@BUDGET@/$6}"; q="${q//@CUT@/$7}"
  printf '%s' "$q"
}

# Session-level read (facts row + optional tables), always one statement.
# v1 and v2 share it: the session table differs (ST), the v1-only side tables
# are consulted in v1 mode only.
aux_sql() { # sid
  local sid="$1" join="" dirs="'[]'" q
  have_table project && join="LEFT JOIN project pr ON pr.id = s.project_id"
  have_table project_directory && dirs="json((SELECT json_group_array(json_object('directory', d.directory, 'type', d.type, 'strategy', d.strategy, 'time_created', d.time_created)) FROM project_directory d WHERE d.project_id = s.project_id))"
  q="SELECT json_object('a', 'dir', 'directory', s.directory, 'tu', s.time_updated, 'tc', s.time_created, 'row', $SESSION_OBJ, 'dirs', $dirs) FROM $ST s $join WHERE s.id = '$sid'"
  if have_table todo; then
    q+=" UNION ALL SELECT json_object('a', 'todo', 'max', max(time_updated), 'count', count(*), 'rows', json(COALESCE((SELECT json_group_array(json_object('content', content, 'status', status, 'priority', priority, 'position', position, 'time_created', time_created, 'time_updated', time_updated)) FROM (SELECT * FROM todo WHERE session_id = '$sid' ORDER BY position)), '[]'))) FROM todo WHERE session_id = '$sid'"
  fi
  if v1_table session_input; then
    q+=" UNION ALL SELECT json_object('a', 'input', 'row', json_object('id', id, 'prompt', prompt, 'delivery', delivery, 'admitted_seq', admitted_seq, 'promoted_seq', promoted_seq, 'time_created', time_created)) FROM session_input WHERE session_id = '$sid' AND time_created > $CUR_INPUTS"
  fi
  if v1_table session_message; then
    q+=" UNION ALL SELECT json_object('a', 'smcount', 'n', count(*)) FROM session_message WHERE session_id = '$sid'"
  fi
  if v1_table session_context_epoch; then
    q+=" UNION ALL SELECT json_object('a', 'epoch', 'row', json_object('baseline', baseline, 'snapshot', snapshot, 'baseline_seq', baseline_seq)) FROM session_context_epoch WHERE session_id = '$sid'"
  fi
  if have_table permission; then
    q+=" UNION ALL SELECT json_object('a', 'perm', 'row', json_object('id', id, 'project_id', project_id, 'action', action, 'resource', resource, 'time_created', time_created, 'time_updated', time_updated)) FROM permission WHERE project_id = (SELECT project_id FROM $ST WHERE id = '$sid') AND time_updated > $CUR_PERM"
  fi
  if [[ "$SCHEMA" == "v2" ]] && have_table message; then
    # the migration floor of this session: its rows in the frozen v1 table (index range)
    q+=" UNION ALL SELECT json_object('a', 'floor', 'v', COALESCE((SELECT max(time_updated) FROM message WHERE session_id = '$sid'), 0))"
  fi
  printf '%s;' "$q"
}

# The change signature of a session's small side tables (todo, session_input,
# permission of its project, context epoch): they carry no shared clock, so a
# finished session is skipped only while this string is unchanged. The epoch row
# is summarised by its sequence and the byte sizes of its two texts (header
# reads, no content).
sig_expr() { # session id column expression
  local e="''" c="$1"
  [[ "$SCHEMA" == "v2" ]] && e="'v2/'"   # a v1 signature never matches a v2 one
  if have_table todo; then e+=" || COALESCE((SELECT max(time_updated) || '.' || count(*) FROM todo WHERE session_id = $c), '') || '/'"; else e+=" || '/'"; fi
  if v1_table session_input; then e+=" || COALESCE((SELECT max(time_created) || '.' || count(*) FROM session_input WHERE session_id = $c), '') || '/'"; else e+=" || '/'"; fi
  if have_table permission; then e+=" || COALESCE((SELECT max(time_updated) || '.' || count(*) FROM permission WHERE project_id = (SELECT project_id FROM $ST WHERE id = $c)), '') || '/'"; else e+=" || '/'"; fi
  if v1_table session_context_epoch; then e+=" || COALESCE((SELECT baseline_seq || '.' || octet_length(baseline) || '.' || octet_length(snapshot) FROM session_context_epoch WHERE session_id = $c), '')"; fi
  printf '%s' "$e"
}

# Stage 1: a page of sessions by rowid, each with its own time_updated and the
# time_updated of its newest message (one index descent; v2's
# session_message_session_time_created_id_idx is the same shape). Never
# touches `part`.
select_sql() { # last_rowid explicit_filter
  printf '%s' "WITH b AS MATERIALIZED (SELECT s.rowid AS rid, s.id AS id, s.time_updated AS tu FROM $ST s WHERE s.rowid > $1 $2 ORDER BY s.rowid LIMIT $SEL_BATCH)
SELECT rid || '|' || CASE WHEN $(idok id) THEN id ELSE '' END || '|' || tu || '|'
  || COALESCE((SELECT m.time_updated FROM $MT m WHERE m.session_id = b.id ORDER BY m.time_created DESC, m.id DESC LIMIT 1), 0)
FROM b ORDER BY rid;"
}

# Stage 2, per candidate session: counts through the covering indexes, the
# newest message and its parts, the stuck-tool probe, the small tables. The
# fields are the same in v2 (no parts: the part count and the parts' newest
# time are 0; the stuck probe reads the tool items of the newest row).
probe_sql() { # sid
  local sid="$1" newest
  newest="(SELECT m.id FROM $MT m WHERE m.session_id = '$sid' ORDER BY m.time_created DESC, m.id DESC LIMIT 1)"
  if [[ "$SCHEMA" == "v2" ]]; then
    printf '%s' "SELECT (SELECT count(*) FROM session_message WHERE session_id = '$sid')
  || '|' || 0
  || '|' || COALESCE((SELECT time_updated FROM session_message WHERE id = $newest), 0)
  || '|' || 0
  || '|' || COALESCE((SELECT time_updated FROM session_v2 WHERE id = '$sid'), 0)
  || '|' || COALESCE((SELECT min(json_extract(j.value, '\$.time.created')) FROM session_message m, json_each(m.data, '\$.content') j
        WHERE m.id = $newest AND m.time_updated > $NOW_MS - $STUCK_MS AND m.type = 'assistant' AND $M_VALID
          AND json_extract(j.value, '\$.type') = 'tool' AND json_extract(j.value, '\$.state.status') IN ('pending', 'running', 'streaming')), '')
  || '|' || $(sig_expr "'$sid'");"
    return 0
  fi
  printf '%s' "SELECT (SELECT count(*) FROM message WHERE session_id = '$sid')
  || '|' || (SELECT count(*) FROM part WHERE session_id = '$sid')
  || '|' || COALESCE((SELECT time_updated FROM message WHERE id = $newest), 0)
  || '|' || COALESCE((SELECT max(p.time_updated) FROM part p WHERE p.message_id = $newest), 0)
  || '|' || COALESCE((SELECT time_updated FROM session WHERE id = '$sid'), 0)
  || '|' || COALESCE((SELECT min(json_extract(p.data, '\$.state.time.start')) FROM part p
        WHERE p.message_id = $newest AND p.time_updated > $NOW_MS - $STUCK_MS
          AND $P_TYPE = 'tool' AND $P_STATUS IN ('pending', 'running')), '')
  || '|' || $(sig_expr "'$sid'");"
}

# After a session is read to the end: is there still a row newer than the
# slack? (a session changing while it is read is not done)
tail_sql() { # sid cut
  if [[ "$SCHEMA" == "v2" ]]; then
    printf '%s' "SELECT EXISTS (SELECT 1 FROM session_message WHERE session_id = '$1' AND time_updated > $2);"
    return 0
  fi
  printf '%s' "SELECT EXISTS (SELECT 1 FROM part WHERE session_id = '$1' AND time_updated > $2)
  OR EXISTS (SELECT 1 FROM message WHERE session_id = '$1' AND time_updated > $2);"
}

# --- run state -----------------------------------------------------------------

EMITTED_TOTAL=0
ROWS_TOTAL=0
PROCESSED=0
FAILED_SESSIONS=0
REJECTED=0
REJECTED_ROWS=0
BLOB_ROWS=0
SKIPPED_GATED=0
SKIPPED_UNCHANGED=0
DEFERRED_RECENT=0
STOP_REASON=""
LEFT=0
BUDGET="$READ_BYTES"
WROTE=0
QUEUE_INTERRUPTED=0
SID="" SESS_DIR="" CHUNK_LIM=0 PROBE_SID=""
CUT_MS=$(( NOW_MS - QUIET_MS ))
(( CUT_MS < 0 )) && CUT_MS=0

# The wall-time bound never fires before the first batch is durable: a run
# always makes progress, whatever the budget or the startup cost.
run_deadline_hit() { (( RUN_BUDGET_S > 0 && BATCHES_DONE > 0 && $(now_ms) - START_MS >= RUN_BUDGET_S * 1000 )); }
should_stop() { # sets STOP_REASON
  if (( STOP_REQUESTED )); then STOP_REASON=signal; return 0; fi
  if run_deadline_hit; then STOP_REASON=time; return 0; fi
  if (( MAX_EVENTS > 0 && EMITTED_TOTAL >= MAX_EVENTS )); then STOP_REASON=cap; return 0; fi
  return 1
}

adapt_budget() { # keeps one read near READ_TARGET_MS
  if (( READ_MS > READ_TARGET_MS )); then
    BUDGET=$(( BUDGET / 2 )); (( BUDGET < BUDGET_MIN )) && BUDGET=$BUDGET_MIN
  elif (( READ_MS * 2 < READ_TARGET_MS )); then
    BUDGET=$(( BUDGET * 2 )); (( BUDGET > BUDGET_MAX )) && BUDGET=$BUDGET_MAX
  fi
  return 0
}

# Builders read the globals at call time: after a source change the statement
# is rebuilt from the reloaded cursor slot.
aux_sql_g() { aux_sql "$SID"; }
chunk_sql_g() { chunk_sql "$SID" "$CUR_MU" "$CUR_K" "$CUR_IX" "$CHUNK_LIM" "$BUDGET" "$CUT_MS"; }
probe_sql_g() { probe_sql "$PROBE_SID"; }
tail_sql_g() { tail_sql "$SID" "$CUT_MS"; }
hook_reload_slot() { [[ -n "$SID" ]] && read_cursor "$SID"; return 0; }

# queue_items FILE -> WROTE = items durably queued (a prefix); rc 0 iff all.
# When a signal reached the queue writer too (a group signal), the durable
# prefix is still reported and QUEUE_INTERRUPTED tells the caller it is a stop,
# not a failure.
queue_items() {
  local total rc
  total="$(wc -l <"$1")"
  WROTE=0
  QUEUE_INTERRUPTED=0
  if [[ ! "$total" =~ ^[0-9]+$ ]]; then   # a helper killed by a group signal must not read as "nothing to write"
    (( STOP_REQUESTED )) && { QUEUE_INTERRUPTED=1; return 1; }
    scratch_ok || scratch_fail "the item list"
    log "$SID: cannot count the items of a batch"
    return 1
  fi
  (( total == 0 )) && return 0
  if [[ "$DRY_RUN" == "1" ]]; then
    jq -c '{project, session_id, event_type, source_event_id, occurred_at,
            payload_bytes: (.payload | tojson | length), text_bytes: ((.text // "") | length)}' "$1"
    WROTE=$total
    BATCHES_DONE=$((BATCHES_DONE + 1))
    return 0
  fi
  local q0; q0="$(now_ms)"
  "$QUEUE_BIN" -batch-file "$1" >"$WRITTEN" 2>"$WORK_DIR/queue.err"; rc=$?
  T_QUEUE=$(( T_QUEUE + $(now_ms) - q0 ))
  WROTE="$(wc -l <"$WRITTEN")"
  [[ "$WROTE" =~ ^[0-9]+$ ]] || WROTE=0
  if (( rc != 0 || WROTE != total )); then
    if (( STOP_REQUESTED )); then QUEUE_INTERRUPTED=1; return 1; fi
    scratch_ok || scratch_fail "a queue write"
    log "$SID: queue write failed after $WROTE of $total item(s): $(head -c 300 "$WORK_DIR/queue.err" | tr '\n' ' ')"
    return 1
  fi
  BATCHES_DONE=$((BATCHES_DONE + 1))
  return 0
}

# --- one session ---------------------------------------------------------------

session_lock_held=0
lock_session() { # 0 = lock held (or none needed), 1 = another run has it
  session_lock_held=0
  [[ "$DRY_RUN" == "0" ]] || return 0
  command -v flock >/dev/null 2>&1 || return 0
  touch "$(cursor_lock_file "$1")" 2>/dev/null || return 0
  { exec 9>"$(cursor_lock_file "$1")"; } 2>/dev/null || return 0
  if flock -n 9 2>/dev/null; then session_lock_held=1; return 0; fi
  { exec 9>&-; } 2>/dev/null
  return 1
}
unlock_session() { [[ "$session_lock_held" == "1" ]] && { exec 9>&-; } 2>/dev/null; session_lock_held=0; SLOT_HOOK=""; SID=""; return 0; }

BAD_ROW_LOGS=0
# note_blob_rows POS_TSV: rows whose data is a BLOB (column 7) are captured with
# the blob as base64 (raw_blob, blob: true) and counted (blobs in the summary).
note_blob_rows() {
  local n
  n="$(awk -F'\t' '$7 == 1 {c++} END {print c+0}' "$1")"
  [[ "$n" =~ ^[0-9]+$ ]] && BLOB_ROWS=$(( BLOB_ROWS + n ))
  return 0
}
# note_bad_rows POS_TSV: rows whose id failed validation are counted and logged
# (by the hex of the id, never the raw value), and produce no event.
note_bad_rows() {
  local n hx
  n="$(awk -F'\t' '$6 == "" {c++} END {print c+0}' "$1")"
  (( n > 0 )) || return 0
  REJECTED_ROWS=$(( REJECTED_ROWS + n ))
  while IFS=$'\t' read -r hx; do
    (( BAD_ROW_LOGS < 20 )) || break
    BAD_ROW_LOGS=$(( BAD_ROW_LOGS + 1 ))
    log "$SID: skipping a row whose id is not [A-Za-z0-9_.-]{1,100} (id hex ${hx:0:64})"
  done < <(awk -F'\t' '$6 == "" {print $4}' "$1")
}

# process_session SID TU DSIG -> 0 (ok, held, or stopped) / 1 (failed). One
# session, resumable. Sets SESSION_DONE=1 when the session was read to its end.
process_session() {
  SID="$1"
  local tu="$2" dsig="$3" sess_fail=0 rc
  SESS_DIR=""
  if ! lock_session "$SID"; then
    log "$SID: cursor locked by another run, skipping this pass"
    SID=""
    return 0
  fi
  SLOT_HOOK=hook_reload_slot
  read_cursor "$SID"
  if (( VERIFY == 0 )) && [[ -n "$CUR_DSIG" && "$dsig" == "$CUR_DSIG" ]]; then
    SKIPPED_UNCHANGED=$((SKIPPED_UNCHANGED + 1))
    unlock_session
    return 0
  fi
  PROCESSED=$((PROCESSED + 1))

  # -- session-level read: directory, facts row, optional tables --------------
  read_db "$AUX_FILE" aux_sql_g; rc=$?
  if (( rc == 2 )); then unlock_session; return 0; fi
  if (( rc != 0 )); then
    log "$SID: session read failed"
    unlock_session
    return 1
  fi
  SESS_DIR="$(jq -r 'select(.a == "dir") | .directory // ""' "$AUX_FILE" 2>/dev/null | head -n 1)"
  if ! grep -q '"a":"dir"' "$AUX_FILE" 2>/dev/null; then
    (( STOP_REQUESTED )) || log "$SID: session row vanished, skipping"
    unlock_session
    return 0
  fi
  if [[ -n "$SESS_DIR" ]] && capture_off "$SESS_DIR"; then
    log "$SID: capture disabled for $SESS_DIR"
    unlock_session
    return 0
  fi
  SESSION_FLOOR=0
  if [[ "$SCHEMA" == "v2" ]]; then
    if [[ -n "$V2_FLOOR_OVERRIDE" ]]; then
      SESSION_FLOOR="$V2_FLOOR_OVERRIDE"
    else
      SESSION_FLOOR="$(jq -r 'select(.a == "floor") | .v' "$AUX_FILE" 2>/dev/null | head -n 1)"
      [[ "$SESSION_FLOOR" =~ ^[0-9]+$ ]] || SESSION_FLOOR=0
    fi
  fi
  if [[ -n "$PROJECT_OVERRIDE" ]]; then
    MRP_KEY="$PROJECT_OVERRIDE" MRP_ROOT="" MRP_SOURCE=""
  else
    memoire_resolve_v "${SESS_DIR:-unknown}"
  fi
  # a resolver killed by a group signal falls back to the basename: never queue that
  if (( STOP_REQUESTED )); then unlock_session; return 0; fi
  [[ -z "$MRP_KEY" ]] && MRP_KEY="opencode-unknown"
  set_jq_args

  local ehash="" out st
  if grep -q '"a":"epoch"' "$AUX_FILE" 2>/dev/null; then
    ehash="$(jq -c 'select(.a == "epoch") | .row' "$AUX_FILE" | sha256sum | cut -c1-16)"
    if [[ ! "$ehash" =~ ^[0-9a-f]{16}$ ]]; then
      if (( STOP_REQUESTED )); then unlock_session; return 0; fi
      log "$SID: cannot digest the context epoch"; unlock_session; return 1
    fi
  fi
  out="$WORK_DIR/aux.out"
  if ! jq -nc "${JQ_COMMON_ARGS[@]}" --argjson curfacts "$CUR_FACTS" --arg curtodo "$CUR_TODO" \
        --argjson curinputs "$CUR_INPUTS" --arg curepoch "$CUR_EPOCH" --argjson curperm "$CUR_PERM" \
        --arg epochhash "$ehash" "$JQ_LIB"$'\n'"$JQ_AUX" "$AUX_FILE" >"$out" 2>"$WORK_DIR/jq.err"; then
    if (( STOP_REQUESTED )); then unlock_session; return 0; fi
    scratch_ok || scratch_fail "the session-level mapping"
    log "$SID: session-level mapping failed: $(head -c 200 "$WORK_DIR/jq.err" | tr '\n' ' ')"
    unlock_session
    return 1
  fi
  st="$(tail -n 1 "$out")"
  if [[ -z "$st" ]] || ! head -n -1 "$out" >"$ITEMS"; then
    if (( STOP_REQUESTED )); then unlock_session; return 0; fi
    scratch_ok || scratch_fail "the session-level items"
    log "$SID: cannot split the session-level items"; unlock_session; return 1
  fi
  if (( STOP_REQUESTED )); then unlock_session; return 0; fi
  if [[ "$SCHEMA" == "v1" ]] && (( $(jq -r '._state.smcount // 0' <<<"$st") > 0 )); then
    log "WARNING: session_message holds unmapped row(s) for session $SID — a v2 store read in v1 mode; run with --schema auto"
  fi
  if grep -q '"a":"input"' "$AUX_FILE" 2>/dev/null; then
    log "WARNING: session_input holds rows for session $SID — captured as opencode_session_input"
  fi
  if queue_items "$ITEMS"; then
    EMITTED_TOTAL=$((EMITTED_TOTAL + WROTE))
    local f t i e p
    IFS=$'\t' read -r f t i e p < <(jq -r '._state | ["x" + (.facts_mu | tostring), "x" + .todo, "x" + (.inputs_mu | tostring), "x" + .epoch_id, "x" + (.perm_mu | tostring)] | @tsv' <<<"$st")
    CUR_FACTS="${f#x}" CUR_TODO="${t#x}" CUR_INPUTS="${i#x}" CUR_EPOCH="${e#x}" CUR_PERM="${p#x}"
    write_cursor "$SID" || { (( STOP_REQUESTED )) || log "$SID: cursor write failed"; }
  else
    EMITTED_TOTAL=$((EMITTED_TOTAL + WROTE))
    unlock_session
    (( QUEUE_INTERRUPTED )) && return 0
    return 1
  fi

  # -- the stream ---------------------------------------------------------------
  local lim rem nrows njson fail_row done_rows limit_rows pos row_k row_mu row_ix newer
  while :; do
    if should_stop; then break; fi
    lim="$ROW_CHUNK"
    if (( MAX_EVENTS > 0 )); then
      rem=$(( MAX_EVENTS - EMITTED_TOTAL ))
      (( rem < lim )) && lim=$rem
      (( lim < 1 )) && lim=1
    fi
    CHUNK_LIM="$lim"
    read_db "$ROWS_TSV" chunk_sql_g; rc=$?
    if (( rc == 2 )); then break; fi
    if (( rc != 0 )); then sess_fail=1; break; fi
    adapt_budget
    nrows="$(wc -l <"$ROWS_TSV")"
    if [[ ! "$nrows" =~ ^[0-9]+$ ]]; then   # killed by a group signal: not an empty read
      (( STOP_REQUESTED )) || { scratch_ok || scratch_fail "a read"; log "$SID: cannot count the rows of a read"; sess_fail=1; }
      break
    fi
    if (( nrows == 0 )); then
      if (( STOP_REQUESTED )); then break; fi
      # Caught up to the quiet slack. The session is done only when nothing in
      # it is newer than the slack, and only for a primary read.
      SESSION_DONE=1
      if [[ "$SRC" == "p" && "$ON_FALLBACK" == "0" ]]; then
        read_db "$WORK_DIR/tail.out" tail_sql_g; rc=$?
        if (( rc == 2 )); then break; fi
        if (( rc != 0 )); then sess_fail=1; break; fi
        newer="$(head -n 1 "$WORK_DIR/tail.out")"
        if [[ "$SRC" == "p" && "$newer" == "0" ]]; then
          CUR_DSIG="$dsig"
          write_cursor "$SID" || { (( STOP_REQUESTED )) || { log "$SID: cursor write failed"; sess_fail=1; }; }
        else
          DEFERRED_RECENT=$((DEFERRED_RECENT + 1))
        fi
      fi
      break
    fi
    ROWS_TOTAL=$((ROWS_TOTAL + nrows))
    if ! cut -f1-7 "$ROWS_TSV" >"$POS_TSV" || ! cut -f8- "$ROWS_TSV" >"$ROWS_JSONL"; then
      (( STOP_REQUESTED )) && break
      scratch_ok || scratch_fail "the rows of a batch"
      log "$SID: cannot split a batch"; sess_fail=1; break
    fi
    rm -f "$ROWS_TSV"
    njson="$(wc -l <"$ROWS_JSONL")"
    if [[ "$njson" != "$nrows" ]]; then
      (( STOP_REQUESTED )) && break
      scratch_ok || scratch_fail "the rows of a batch"
      log "$SID: a row did not come back as one line; refusing the batch"
      sess_fail=1; break
    fi
    note_bad_rows "$POS_TSV"
    note_blob_rows "$POS_TSV"
    local m0; m0="$(now_ms)"
    if ! spill_prepare "$ROWS_JSONL"; then
      (( STOP_REQUESTED )) && break
      sess_fail=1; break
    fi
    if (( STOP_REQUESTED )); then break; fi
    local jq_events="$JQ_EVENTS"
    [[ "$SCHEMA" == "v2" ]] && jq_events="$JQ_EVENTS_V2"
    if ! jq -c "${JQ_COMMON_ARGS[@]}" --slurpfile doneset "$TURNS_FILE" --slurpfile spmap "$SPILL_MAP" \
          --argjson cutrow "$SPILL_CUT" "$JQ_LIB"$'\n'"$jq_events" "$ROWS_JSONL" >"$ITEMS" 2>"$WORK_DIR/jq.err"; then
      (( STOP_REQUESTED )) && break
      scratch_ok || scratch_fail "the events of a batch"
      log "$SID: mapping failed: $(head -c 300 "$WORK_DIR/jq.err" | tr '\n' ' ')"
      sess_fail=1; break
    fi
    rm -f "$ROWS_JSONL"
    if (( STOP_REQUESTED )); then break; fi   # any helper killed by the signal is behind us: queue nothing tainted
    T_MAP=$(( T_MAP + $(now_ms) - m0 ))
    limit_rows="$nrows"
    (( SPILL_CUT > 0 )) && limit_rows=$(( SPILL_CUT - 1 ))   # the deferred part is read again
    done_rows="$limit_rows"
    if ! queue_items "$ITEMS"; then
      # the cursor holds at the row before the first item that was not written
      if (( ! QUEUE_INTERRUPTED )); then sess_fail=1; fi
      fail_row="$(sed -n "$((WROTE + 1))p;$((WROTE + 1))q" "$ITEMS" | head -c 64 | sed -n 's/^{"_row":\([0-9][0-9]*\).*/\1/p')"
      if [[ "$fail_row" =~ ^[0-9]+$ ]] && (( fail_row >= 1 && fail_row <= nrows )); then
        done_rows=$(( fail_row - 1 ))
      else
        done_rows=0
      fi
      (( done_rows > limit_rows )) && done_rows="$limit_rows"
    fi
    EMITTED_TOTAL=$((EMITTED_TOTAL + WROTE))
    if (( done_rows >= 1 )); then
      # turns queued in this batch are remembered (no second turn on a later edit)
      if ! { awk -F'\t' -v lim="$done_rows" '$5 == 1 && $6 != "" && $1 <= lim {print $6}' "$POS_TSV" \
               | jq -Rn '[inputs] | map({(.): true}) | add // {}' >"$TURNS_NEW"; } 2>/dev/null \
         || ! jq -c -s '.[0] * .[1]' "$TURNS_FILE" "$TURNS_NEW" >"$TURNS_FILE.tmp" 2>/dev/null \
         || ! mv -f "$TURNS_FILE.tmp" "$TURNS_FILE" 2>/dev/null; then
        # the batch is queued but its turns are not remembered: the cursor stays, the rows are read again
        # (identical ids and content)
        (( STOP_REQUESTED )) || { scratch_ok || scratch_fail "the turn list"; log "$SID: cannot record the turns of a batch"; sess_fail=1; }
        break
      fi
      pos="$(sed -n "${done_rows}p" "$POS_TSV")"
      IFS=$'\t' read -r _ row_mu row_k row_ix _ <<<"$pos"
      if [[ ! "$row_mu" =~ ^[0-9]+$ || ! "$row_k" =~ ^[mps]$ || ! "$row_ix" =~ ^[0-9A-F]+$ ]]; then
        (( STOP_REQUESTED )) && break
        log "$SID: malformed position line; refusing to move the cursor"
        sess_fail=1; break
      fi
      CUR_MU="$row_mu" CUR_K="$row_k" CUR_IX="$row_ix"
      write_cursor "$SID" || { (( STOP_REQUESTED )) || { log "$SID: cursor write failed (rows will be re-read next pass; ids are stable)"; sess_fail=1; }; }
    fi
    (( sess_fail )) && break
    (( QUEUE_INTERRUPTED )) && break
  done
  unlock_session
  (( sess_fail )) && return 1
  return 0
}

# --- selection -------------------------------------------------------------------

SINCE_MS=0
if [[ "$FULL" == "0" && "$EXPLICIT" == "0" ]]; then
  if [[ -n "$SINCE_SPEC" ]]; then
    since_s="$(date -d "$SINCE_SPEC" +%s 2>/dev/null)" \
      || { log "unparsable --since spec: $SINCE_SPEC"; exit 2; }
    SINCE_MS=$(( since_s * 1000 ))
  else
    SINCE_MS=$(( NOW_MS - OPENCODE_SINCE_S * 1000 ))
  fi
  (( SINCE_MS < 0 )) && SINCE_MS=0
fi
# A session is a candidate when its newest activity (own time_updated, newest
# message) is later than the window start minus the slack.
CAND_FLOOR=0
if (( SINCE_MS > 0 )); then
  CAND_FLOOR=$(( SINCE_MS - SLACK_S * 1000 ))
  (( CAND_FLOOR < 0 )) && CAND_FLOOR=0
fi

SEL_FILTER=""
if [[ "$EXPLICIT" == "1" ]]; then
  id_list=""
  for sid in ${SESSION_IDS[@]+"${SESSION_IDS[@]}"}; do
    if [[ ! "$sid" =~ ^[A-Za-z0-9_.-]{1,100}$ ]]; then
      log "refusing suspicious session id: $sid"
      exit 2
    fi
    id_list+="${id_list:+,}'$sid'"
  done
  SEL_FILTER="AND s.id IN ($id_list)"
fi

if [[ "$DRY_RUN" == "0" ]]; then
  mkdir -p "$CURSOR_DIR" 2>/dev/null
  chmod 700 "$CURSOR_DIR" 2>/dev/null
  chmod 700 "$(dirname "$CURSOR_DIR")" 2>/dev/null
fi

: >"$SESSIONS_TSV"
last_rid=0
SEL_FAILED=0
SEL_INTERRUPTED=0
SEL_FILTER_G=""
select_sql_g() { select_sql "$last_rid" "$SEL_FILTER"; }
while :; do
  read_db "$WORK_DIR/sel.out" select_sql_g; rc=$?
  if (( rc == 2 )); then SEL_INTERRUPTED=1; break; fi
  if (( rc != 0 )); then
    log "session selection query failed"
    SEL_FAILED=1
    break
  fi
  n=0
  # '|' is a non-whitespace IFS: an empty id (a refused one) stays an empty field
  while IFS='|' read -r rid sid tu lm; do
    [[ "$rid" =~ ^[0-9]+$ ]] || continue
    n=$((n + 1))
    last_rid="$rid"
    if [[ -z "$sid" ]]; then
      REJECTED=$((REJECTED + 1))
      log "refusing a session whose id is not [A-Za-z0-9_.-]{1,100} (rowid $rid)"
      continue
    fi
    [[ "$tu" =~ ^[0-9]+$ ]] || tu=0
    [[ "$lm" =~ ^[0-9]+$ ]] || lm=0
    s0=$(( tu > lm ? tu : lm ))
    (( CAND_FLOOR > 0 && s0 <= CAND_FLOOR )) && continue
    printf '%s\t%s\t%s\n' "$s0" "$sid" "$tu" >>"$SESSIONS_TSV" || scratch_fail "the candidate list"
  done <"$WORK_DIR/sel.out"
  (( n < SEL_BATCH )) && break
done

# --- driver: oldest change first -------------------------------------------------

finish() { # exit code
  local stopped=0
  [[ -n "$STOP_REASON" ]] && stopped=1
  emit_summary "$PROCESSED" "$EMITTED_TOTAL" "$FAILED_SESSIONS" "$(( REJECTED + REJECTED_ROWS ))" "$stopped" "$LEFT" "$BLOB_ROWS"
  exit "$1"
}
summary_line() {
  local t=""
  [[ "${MEMOIRE_OPENCODE_TIMING:-0}" == "1" ]] && t=" total_ms=$(( $(now_ms) - NOW_MS )) read_ms=$T_READ map_ms=$T_MAP queue_ms=$T_QUEUE"
  printf 'source=%s schema=%s sessions=%s events=%s rows=%s failed_sessions=%s rejected=%s rejected_rows=%s blobs=%s skipped_gated=%s skipped_unchanged=%s candidates=%s reads=%s max_read_ms=%s stopped=%s left=%s dry_run=%s%s' \
    "$DB_SOURCE" "$SCHEMA" "$PROCESSED" "$EMITTED_TOTAL" "$ROWS_TOTAL" "$FAILED_SESSIONS" "$REJECTED" "$REJECTED_ROWS" "$BLOB_ROWS" \
    "$SKIPPED_GATED" "$SKIPPED_UNCHANGED" "${TOTAL_CAND:-0}" "$READ_COUNT" "$READ_MAX_MS" "${STOP_REASON:-none}" "$LEFT" "$DRY_RUN" "$t"
}

TOTAL_CAND=0
if (( SEL_INTERRUPTED )); then
  STOP_REASON=signal
  log "stopped cleanly on signal: cursors written, 0 session(s) left for the next run; $(summary_line)"
  finish 0
fi
if [[ "$SEL_FAILED" == "1" ]]; then
  FAILED_SESSIONS=0
  log "done: $(summary_line) selection=failed"
  finish 1
fi

sort -t$'\t' -k1,1n -k2,2 "$SESSIONS_TSV" >"$WORK_DIR/sessions.sorted" || { scratch_ok || scratch_fail "the candidate list"; }
TOTAL_CAND="$(wc -l <"$WORK_DIR/sessions.sorted")"
SEEN=0
SESSION_DONE=0
while IFS=$'\t' read -r -u 3 s0 sid tu; do
  SEEN=$((SEEN + 1))
  if should_stop; then LEFT=$(( TOTAL_CAND - SEEN + 1 )); break; fi
  # stage 2: the probe of this candidate (counts, newest message, small tables)
  PROBE_SID="$sid"
  read_db "$WORK_DIR/probe.out" probe_sql_g; rc=$?
  if (( rc == 2 )); then STOP_REASON=signal; LEFT=$(( TOTAL_CAND - SEEN + 1 )); break; fi
  if (( rc != 0 )); then
    log "$sid: session read failed"
    FAILED_SESSIONS=$((FAILED_SESSIONS + 1))
    continue
  fi
  IFS='|' read -r nm np lm lp ptu run sig <"$WORK_DIR/probe.out"
  for v in nm np lm lp ptu; do [[ "${!v}" =~ ^[0-9]+$ ]] || printf -v "$v" 0; done
  [[ "$run" =~ ^[0-9]+$ ]] || run=0
  [[ "$sig" =~ ^[0-9A-Za-z./]*$ ]] || sig=""
  mx=$(( ptu > lm ? ptu : lm )); (( lp > mx )) && mx=$lp
  if (( NOW_MS - mx <= QUIET_MS )); then SKIPPED_GATED=$((SKIPPED_GATED + 1)); continue; fi
  if (( run > 0 && NOW_MS - run < STUCK_MS )); then SKIPPED_GATED=$((SKIPPED_GATED + 1)); continue; fi
  SESSION_DONE=0
  process_session "$sid" "$ptu" "$ptu.$lm.$lp.$nm.$np.$sig"
  rc=$?
  (( rc != 0 )) && FAILED_SESSIONS=$((FAILED_SESSIONS + 1))
  if (( rc == 0 && SESSION_DONE == 0 )) && should_stop; then
    LEFT=$(( TOTAL_CAND - SEEN + 1 ))   # this session is unfinished, plus the rest
    break
  fi
done 3<"$WORK_DIR/sessions.sorted"

if [[ -n "$STOP_REASON" ]]; then
  log "stopped cleanly on $STOP_REASON: cursors written, $LEFT session(s) left for the next run; $(summary_line)"
else
  log "done: $(summary_line)"
fi
(( FAILED_SESSIONS > 0 )) && finish 1
finish 0
