#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# liaison:D239 session-level kill switch: honored directly (not via the resolver) so
# it works even if the resolver script is missing or broken.
case "${MEMOIRE_CAPTURE:-}" in off|0|false) exit 0 ;; esac

# Capture-all adapter for Codex CLI rollout transcripts.
#
# One JSONL rollout per thread under ~/.codex/sessions/ (and
# archived_sessions/); line 0 is session_meta; every record carries top-level
# type, timestamp and (except 7 legacy files) an ordinal equal to its line
# index. Every record that maps to an event class is queued WHOLE - no
# truncation, no sampling, no prompt flag. Payload = the record's payload
# object verbatim plus provenance (thread_id, ordinal, line, rollout_file,
# record_type, record_payload_type), every other top-level key of the record
# (`metadata`, ...) under record_metadata / record_extra, with ONE deliberate
# transformation: every nested `encrypted_content` string becomes
# {encrypted: true, bytes: N} (provisional: the opaque blob is a large,
# unreadable string; flipping it back is deleting the encsize filter). A
# provenance key that the payload also carries with another value keeps the
# payload's value under source_<key>.
#
# Requirements: bash, GNU coreutils/sed/find, flock, and jq >= 1.7 (the chunk
# state travels through jq's `stderr`, which writes strings raw from 1.7 on;
# older jq JSON-encodes them and every file would fail). The version is
# checked once there is a file to read: below 1.7 the adapter logs one line and
# exits 1 without reading any file; with nothing to read it exits 0 whatever
# the jq version. Tested with jq 1.8.1.
#
# Characters: a record is never handed to jq through its raw-input mode
# (`jq -R`), which in jq 1.8.1 decodes every 4 KiB read buffer on its own and
# replaces a multibyte character that straddles a boundary with U+FFFD. Each
# line is turned into a JSON string by a byte-level sed (C locale: backslash,
# quote and control characters escaped, every other byte untouched) and jq
# reads the stream in JSON mode, whose reader keeps a character whole across
# any buffer boundary. What reaches the queue is byte-for-byte the source
# text. A line that is not valid UTF-8 has its invalid bytes replaced by U+FFFD
# (the same as `jq -R` would do); a valid line is never altered.
#
# Not captured: event_msg/token_count (a rate-limit poll), world_state,
# realtime_item, and the line-0 session_meta (it feeds the facts event). A
# second session_meta on a later line is a copied parent record and is
# captured as codex_record. A line that is not JSON is captured raw as a
# codex_record {unparsed_line: true} - it never stops a file.
#
# Identity: source_event_id = "codex:<thread>:<ordinal>" where <thread> is the
# line-0 session_meta id (for segment files, history_base.thread_id); the
# fallback "codex:<thread>:ln<line>" applies only to ordinal-less lines.
# Deterministic, so a re-scan, a cursor loss or a second machine re-queues the
# same ids and the hub collapses them (first-write-wins on
# sha256(adapter + ":" + source_event_id)). One codex_thread_facts event per
# thread (id codex:<thread>:facts) is queued whenever a pass starts at line 0;
# sub-agent inherited records (ordinals < subagent_history_start_ordinal) ARE
# captured (provisional) flagged inherited: true, and never join a turn. The
# ids use the ordinal; the cursor uses LINES of the file, never ordinals (a
# segment file's ordinals start high).
#
# Turn projection: each task_started ... task_complete / turn_aborted pair
# sharing turn_id is a turn. Assistant messages belong to the innermost open
# turn (turns nest; a task_started does NOT close an open turn; only the
# turn's own completion or abort closes it). A turn with assistant text emits
# one codex_assistant_turn event whose TEXT (the exact concatenation of the
# messages' text, joined by a blank line, nothing trimmed) is what derivation
# reads.
#   - A terminal turn event is emitted when the record that closes the turn
#     has been read, never before; id codex:<thread>:t<ordinal of the closing
#     record>.
#   - A turn spanning several runs is assembled from its start: the cursor
#     keeps the open turns (turn id, line and ordinal of the first record,
#     partial id) and the next run re-reads the file from the earliest open
#     turn's first line, rebuilding the turns WITHOUT emitting the records
#     again (only lines from the cursor line on emit).
#   - The cursor never moves past the closing record of a turn whose turn
#     event was not written; a failed write is retried at the next run and the
#     rerun produces the same id.
#   - A turn with no closing record is emitted by the adapter only when the
#     file has not been written for MEMOIRE_CODEX_STUCK_S seconds (default
#     3600): a PARTIAL turn event, closed_by:"adapter", id
#     codex:<thread>:t<ordinal of the last record of the turn>:partial (the
#     last record read while the turn was the innermost open one). The
#     ":partial" suffix keeps the two forms apart: a partial id can never equal
#     the id of a terminal turn event, even when another turn closes on the
#     record the partial turn last saw. If the turn later continues and closes,
#     the full turn is emitted under its own terminal id with
#     supersedes:"<partial id>" in the payload; a second partial of the same
#     turn also carries supersedes. BOTH events are then visible to derivation,
#     which reads every codex_assistant_turn; nothing here removes the partial.
#     Two open turns never share a partial id: every record stamps one turn only.
#   - Equality of passes: a full pass and any sequence of tail passes over the
#     same file produce the same ids and the same content for every capture-all
#     event and for every closed turn (tests/foreign/codex-rollout-capture.sh,
#     T-equiv). It does NOT hold for partial turn events: a partial exists only
#     when a run sees the file idle past the stuck bound, so which partials
#     exist, and the terminal turn's supersedes link to them, depend on the
#     time of the runs. This is an accepted exception.
# Capture-all event types are NOT derivation-admitted, so codex_assistant_turn
# is the only Codex input derivation receives. CONSEQUENCE OF NO CAP: a turn
# larger than the sweep ceiling becomes a derivation window of one event.
#
# Time: every event's occurred-at is its record's own timestamp - never the
# run's wall clock.
#
# Attribution: session home = line-0 session_meta.cwd, resolved through the
# shared memoire-resolve-project.sh (registry longest-prefix, scratch-tmp
# included); -cwd, -project-root and -project-source ride every queue write.
# No adapter gate: scratch is a registry entry (operator-owned), not code.
# The per-directory capture kill switch is honored, fail-open.
#
# Writing: the adapter does not start one queue process per event. A chunk of
# up to MEMOIRE_CODEX_BATCH_RECORDS records (default 2000; halved when a chunk
# took over half of MEMOIRE_CODEX_BATCH_SECONDS, default 20) is classified by
# one jq process and written by one `memoire-queue -batch-file` process, one
# queue item per event, the queue format unchanged. The queue prints one path
# per DURABLE item, so a failed write is counted exactly.
#
# Temporary files: conversation text is copied into a chunk file and an events
# file while a chunk is processed. They live in a private work directory
# $MEMOIRE_HOME/tmp/codex-capture/run.XXXXXX (mode 0700, files 0600, umask
# 077), never under /tmp: TMPDIR is pointed at it for every child. The run
# holds a flock on <workdir>/lock for its whole life. At start, under a lock
# on the parent directory, the adapter removes every earlier work directory
# whose lock no live process holds (a run killed by SIGKILL leaves one; the
# next run clears it), then creates its own. The work directory is removed at
# exit and on TERM/INT/HUP.
#
# Cursor (--tail): one JSON file per rollout path under
# ${MEMOIRE_CAPTURE_CURSOR_DIR:-$MEMOIRE_HOME/capture-cursors/codex-rollouts},
# named sha256(abs path): offset and line (of the next record), the file
# identity (file_id, head_len, head_sha, tail_sha, and dev_inode for a
# rollback to the older reader), the open turns, thread. Identity is the Claude
# transcript adapter's rule: file_id = <statfs f_fsid>:<inode> (st_dev is
# renumbered at every mount on btrfs; f_fsid derives from the filesystem UUID
# and subvolume), head_sha / tail_sha = sha256 of the first min(4096, offset)
# bytes / of the (up to) 4096 bytes ending at the offset. An idle file with the
# same file_id is a stat-only no-op; a file with new bytes, or another file_id,
# must still hash the same in both windows; a cursor from before file_id is
# accepted on its inode and rewritten; the offset must also start a line. A rejected cursor
# means a full pass, logged ("cursor does not match the file"). An archive mv
# (sessions/Y/M/D/<name> to archived_sessions/<name>, or back) keeps the
# inode and the mtime: a cursorless file under one of the two directories
# adopts the cursor of its name under the other, when that path no longer
# exists, and the identity check still applies. It is written after EVERY chunk
# and at every clean stop, so a kill or a timeout loses at most the chunk in
# flight (re-queued with identical ids). The cursor NEVER advances past a
# record whose queue write failed (unlike the Claude transcript adapter): on a
# partial write it moves to the last complete record whose events are all
# durable. Every cursor problem degrades to a full pass. A non-tail full pass
# (the sweep pre-pass's shape) starts at line 0 and also keeps the cursor
# current.
#
# Bounds: --max-seconds N (or MEMOIRE_CODEX_MAX_SECONDS) stops at a chunk
# boundary once N seconds have passed since the start of the run: the cursor is
# written, one log line gives the file, the line reached and the lines left,
# and the exit code is 0. Files are processed oldest change first, so a file
# that did not finish keeps its place at the next run and a large file cannot
# starve the others for ever. --max-events N bounds a run the same way.
#
# Options:
#   --scan                scan dirs (args, else sessions + archived dirs)
#   --since SPEC          find -newermt cutoff for --scan (default "48 hours ago")
#   --quiet-for SEC       skip files younger than SEC (the tick passes 120)
#   --tail                per-file cursor
#   --sessions-dir DIR    default CODEX_SESSIONS_DIR or ~/.codex/sessions
#   --archived-sessions-dir DIR   default CODEX_ARCHIVED_SESSIONS_DIR or ~/.codex/archived_sessions
#   --project NAME        project override (MEMOIRE_PROJECT still wins via resolver)
#   --max-seconds N       stop at a chunk boundary after N s (MEMOIRE_CODEX_MAX_SECONDS)
#   --max-events N        stop at a chunk boundary after N events (MEMOIRE_CODEX_TICK_MAX_EVENTS)
#   --summary             print one TSV line on stdout at exit:
#                         files, events, failed_files, stopped(0|1), lines_left
#   --dry-run             print events as JSON lines; write nothing
#   -h, --help            this block
# Environment: MEMOIRE_CODEX_STUCK_S, MEMOIRE_CODEX_BATCH_RECORDS,
# MEMOIRE_CODEX_BATCH_SECONDS, MEMOIRE_QUEUE_BIN, MEMOIRE_RESOLVE_PROJECT,
# MEMOIRE_CAPTURE_CURSOR_DIR, MEMOIRE_HOME.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QUEUE_BIN="${MEMOIRE_QUEUE_BIN:-$ROOT/bin/memoire-queue}"
RESOLVER="${MEMOIRE_RESOLVE_PROJECT:-$ROOT/scripts/memoire-resolve-project.sh}"
SESSIONS_DIR="${CODEX_SESSIONS_DIR:-$HOME/.codex/sessions}"
ARCHIVED_DIR="${CODEX_ARCHIVED_SESSIONS_DIR:-$HOME/.codex/archived_sessions}"
MEMOIRE_HOME_DIR="${MEMOIRE_HOME:-$HOME/.memoire}"
CURSOR_DIR="${MEMOIRE_CAPTURE_CURSOR_DIR:-$MEMOIRE_HOME_DIR/capture-cursors/codex-rollouts}"
MAX_EVENTS="${MEMOIRE_CODEX_TICK_MAX_EVENTS:-0}"
MAX_SECONDS="${MEMOIRE_CODEX_MAX_SECONDS:-0}"
STUCK_S="${MEMOIRE_CODEX_STUCK_S:-3600}"
BATCH_RECORDS="${MEMOIRE_CODEX_BATCH_RECORDS:-2000}"
BATCH_SECONDS="${MEMOIRE_CODEX_BATCH_SECONDS:-20}"

log() { printf 'memoire-capture-codex-rollout: %s\n' "$*" >&2; }
usage() { sed -n '/^# Capture-all adapter/,/^# MEMOIRE_CAPTURE_CURSOR_DIR, MEMOIRE_HOME\./p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

MRP_KEY="" MRP_ROOT="" MRP_SOURCE=""
# Resolve a project id via the shared paths->id resolver (liaison:D238/D239),
# fail-open to basename. Globals, not command substitution: root/source ride
# the queue write for identity-conflict detection and a subshell drops them.
memoire_resolve_v() {
  local out
  MRP_KEY="" MRP_ROOT="" MRP_SOURCE=""
  if out="$("$RESOLVER" --verbose "$1" 2>/dev/null)" && [[ -n "$out" ]]; then
    IFS=$'\t' read -r MRP_KEY MRP_ROOT MRP_SOURCE <<<"$out"
  fi
  if [[ -z "$MRP_KEY" ]]; then
    MRP_KEY="$(basename "$1" 2>/dev/null)" || MRP_KEY="$1"
    MRP_ROOT="" MRP_SOURCE=""
  fi
}

declare -A CAPTURE_OFF # per-cwd cache of the capture kill switch
capture_off_cwd() {
  local cwd="$1"
  if [[ -z "${CAPTURE_OFF[$cwd]:-}" ]]; then
    if [[ "$("$RESOLVER" --capture-enabled "$cwd" 2>/dev/null)" == "off" ]]; then
      CAPTURE_OFF[$cwd]=1
    else
      CAPTURE_OFF[$cwd]=0
    fi
  fi
  [[ "${CAPTURE_OFF[$cwd]}" == "1" ]]
}

SCAN=0
TAIL=0
DRY_RUN=0
SUMMARY=0
PROJECT_OVERRIDE="${MEMOIRE_PROJECT:-}"
SINCE="48 hours ago"
QUIET_FOR=""
FILES=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --scan) SCAN=1 ;;
    --tail) TAIL=1 ;;
    --dry-run) DRY_RUN=1 ;;
    --summary) SUMMARY=1 ;;
    --since) SINCE="${2:-}"; shift ;;
    --quiet-for) QUIET_FOR="${2:-}"; shift ;;
    --sessions-dir) SESSIONS_DIR="${2:-}"; shift ;;
    --archived-sessions-dir) ARCHIVED_DIR="${2:-}"; shift ;;
    --project) PROJECT_OVERRIDE="${2:-}"; shift ;;
    --max-events) MAX_EVENTS="${2:-}"; shift ;;
    --max-seconds) MAX_SECONDS="${2:-}"; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) log "unknown option: $1"; exit 2 ;;
    *) FILES+=("$1") ;;
  esac
  shift
done
# Fail-open on knob typos: a nonnumeric bound is "no bound" (0), a nonnumeric
# size is its default.
[[ "$MAX_EVENTS" =~ ^[0-9]+$ ]] || MAX_EVENTS=0
[[ "$MAX_SECONDS" =~ ^[0-9]+$ ]] || MAX_SECONDS=0
[[ "$STUCK_S" =~ ^[0-9]+$ ]] || STUCK_S=3600
[[ "$BATCH_RECORDS" =~ ^[1-9][0-9]*$ ]] || BATCH_RECORDS=2000
[[ "$BATCH_SECONDS" =~ ^[1-9][0-9]*$ ]] || BATCH_SECONDS=20
[[ -z "$QUIET_FOR" || "$QUIET_FOR" =~ ^[0-9]+$ ]] || { log "ignoring non-numeric --quiet-for"; QUIET_FOR=""; }

RUN_START="$EPOCHSECONDS"
NOW_EPOCH="$EPOCHSECONDS"

# --- file selection, oldest change first ---------------------------------------
LIST=()
if [[ "$SCAN" == "1" ]]; then
  SCAN_ROOTS=(${FILES[@]+"${FILES[@]}"})
  if [[ ${#SCAN_ROOTS[@]} -eq 0 ]]; then
    [[ -d "$SESSIONS_DIR" ]] && SCAN_ROOTS+=("$SESSIONS_DIR")
    [[ -d "$ARCHIVED_DIR" ]] && SCAN_ROOTS+=("$ARCHIVED_DIR")
  fi
  if [[ ${#SCAN_ROOTS[@]} -eq 0 ]]; then
    log "scan: no codex rollout directory found"
    [[ "$SUMMARY" == "1" ]] && printf '0\t0\t0\t0\t0\n'
    exit 0
  fi
  for arg in "${SCAN_ROOTS[@]}"; do
    findargs=(-name 'rollout-*.jsonl')
    [[ -n "$SINCE" ]] && findargs+=(-newermt "$SINCE")
    [[ -n "$QUIET_FOR" ]] && findargs+=(! -newermt "@$((NOW_EPOCH - QUIET_FOR))")
    while IFS= read -r row; do LIST+=("$row"); done \
      < <(find "$arg" "${findargs[@]}" -printf '%T@\t%p\n' 2>/dev/null)
  done
else
  # Explicit files: the quiet gate still applies when asked (a file the
  # session is still writing is left to the next run).
  for f in ${FILES[@]+"${FILES[@]}"}; do
    m="$(stat -c '%Y' -- "$f" 2>/dev/null)" || m=0
    if [[ -n "$QUIET_FOR" ]]; then
      [[ "$m" =~ ^[0-9]+$ && "$m" -le $((NOW_EPOCH - QUIET_FOR)) ]] || continue
    fi
    LIST+=("$m"$'\t'"$f")
  done
fi
FILES=()
if [[ ${#LIST[@]} -gt 0 ]]; then
  while IFS= read -r row; do FILES+=("${row#*$'\t'}"); done \
    < <(printf '%s\n' "${LIST[@]}" | LC_ALL=C sort -t$'\t' -k1,1n -s)
fi

if [[ ${#FILES[@]} -eq 0 ]]; then
  log "no rollout files to ingest"
  [[ "$SUMMARY" == "1" ]] && printf '0\t0\t0\t0\t0\n'
  exit 0
fi

# --- jq, queue writer, work directory: checked only once there is a file to read
# (nothing to read exits 0 above, whatever the jq version or the Memoire home) ---
if ! command -v jq >/dev/null 2>&1; then
  log "jq is required (>= 1.7)"
  exit 1
fi
# jq >= 1.7: the chunk state goes through jq's `stderr`, raw from 1.7 on.
JQ_VERSION="$(jq --version 2>/dev/null)"
if [[ "$JQ_VERSION" =~ ^jq-([0-9]+)\.([0-9]+) ]] \
   && { (( BASH_REMATCH[1] > 1 )) || (( BASH_REMATCH[1] == 1 && BASH_REMATCH[2] >= 7 )); }; then
  :
else
  log "jq >= 1.7 is required, found '${JQ_VERSION:-unknown}': nothing captured"
  exit 1
fi
if [[ "$DRY_RUN" == "0" && ! -x "$QUEUE_BIN" ]]; then
  log "queue writer not found: $QUEUE_BIN; run make build"
  exit 1
fi

# --- private work directory ------------------------------------------------------
# Conversation text is copied here while a chunk is processed. Never /tmp: the
# directory lives under the Memoire home, mode 0700, files 0600. See the
# header for the lock protocol.
umask 077
WORK_BASE="$MEMOIRE_HOME_DIR/tmp/codex-capture"
WORKDIR=""
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
  WORKDIR="$(mktemp -d "$WORK_BASE/run.XXXXXX" 2>/dev/null)" || WORKDIR=""
  if [[ -n "$WORKDIR" ]]; then
    exec 6>"$WORKDIR/lock" && flock -n 6 || { rm -rf -- "$WORKDIR"; WORKDIR=""; }
  fi
  exec 7>&-
fi
[[ -n "$WORKDIR" ]] || { log "cannot create a work directory under $WORK_BASE"; exit 1; }
export TMPDIR="$WORKDIR"
CH="$WORKDIR/chunk" CHJ="$WORKDIR/chunk.json" EV="$WORKDIR/events" PATHS="$WORKDIR/paths" QERR="$WORKDIR/qerr"
ST_A="$WORKDIR/state.a" ST_B="$WORKDIR/state.b" META="$WORKDIR/meta" FACTS="$WORKDIR/facts"
STOP=0
cleanup() { rm -rf -- "$WORKDIR"; }
on_signal() { STOP=1; }
trap cleanup EXIT
trap on_signal TERM INT HUP

# --- cursor plumbing (fail-open everywhere; shape from cc-transcripts) -------
CURSORS_OK=1
command -v sha256sum >/dev/null 2>&1 \
  || { CURSORS_OK=0; log "sha256sum missing: cursors disabled, full passes only"; }

cursor_dir_ready() {
  [[ "$CURSORS_OK" == "1" ]] || return 1
  [[ -d "$CURSOR_DIR" && -w "$CURSOR_DIR" ]] && return 0
  mkdir -p "$CURSOR_DIR" 2>/dev/null || { CURSORS_OK=0; log "cursor dir unusable: $CURSOR_DIR"; return 1; }
  chmod 700 "$CURSOR_DIR" 2>/dev/null
  chmod 700 "$(dirname "$CURSOR_DIR")" 2>/dev/null
  [[ -w "$CURSOR_DIR" ]] || { CURSORS_OK=0; log "cursor dir not writable: $CURSOR_DIR"; return 1; }
  return 0
}

abs_path() {
  local p="$1" r
  if r="$(readlink -f -- "$p" 2>/dev/null)" && [[ -n "$r" ]]; then printf '%s' "$r"; return 0; fi
  case "$p" in /*) printf '%s' "$p" ;; *) printf '%s/%s' "$PWD" "$p" ;; esac
}

SESSIONS_ABS="" ARCHIVED_ABS=""
[[ -n "$SESSIONS_DIR" ]] && SESSIONS_ABS="$(abs_path "$SESSIONS_DIR")"
[[ -n "$ARCHIVED_DIR" ]] && ARCHIVED_ABS="$(abs_path "$ARCHIVED_DIR")"

# Absolute byte offset of the end of the last COMPLETE line inside
# [start, size): walk backwards in windows so one huge line never forces a
# full re-read (cc-transcripts pattern).
last_line_end() { # file start size
  local f="$1" start="$2" size="$3" w=65536 from len n bytes end
  end="$size"
  while (( end > start )); do
    from=$(( end - w ))
    (( from < start )) && from="$start"
    len=$(( end - from ))
    n="$(tail -c "+$((from + 1))" -- "$f" 2>/dev/null | head -c "$len" | wc -l 2>/dev/null)"
    n="${n//[^0-9]/}"
    if [[ -n "$n" ]] && (( n > 0 )); then
      bytes="$(tail -c "+$((from + 1))" -- "$f" 2>/dev/null | head -c "$len" \
        | head -n "$n" | wc -c 2>/dev/null)"
      bytes="${bytes//[^0-9]/}"
      if [[ -n "$bytes" ]]; then printf '%s' "$(( from + bytes ))"; return 0; fi
    fi
    end="$from"
  done
  printf '%s' "$start"
}

# True when byte offset $2 of file $1 starts a line (0, or preceded by \n).
at_line_start() {
  local f="$1" off="$2" b
  (( off == 0 )) && return 0
  b="$(tail -c "+$off" -- "$f" 2>/dev/null | head -c 1 | od -An -tx1 | tr -d ' \n')"
  [[ "$b" == "0a" ]]
}

# json_lines: each input line becomes one JSON string, byte for byte. Byte-level
# (C locale): a multibyte character is never looked at, so nothing can split it.
# Escapes backslash, quote and the control characters JSON forbids raw; every
# other byte, including any UTF-8 sequence, passes through.
json_lines() {
  LC_ALL=C sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/\t/\\t/g' -e 's/\r/\\r/g' \
    -e '/[\x00-\x08\x0b-\x1f]/{s/\x00/\\u0000/g;s/\x01/\\u0001/g;s/\x02/\\u0002/g;s/\x03/\\u0003/g;s/\x04/\\u0004/g;s/\x05/\\u0005/g;s/\x06/\\u0006/g;s/\x07/\\u0007/g;s/\x08/\\b/g;s/\x0b/\\u000b/g;s/\x0c/\\f/g;s/\x0e/\\u000e/g;s/\x0f/\\u000f/g;s/\x10/\\u0010/g;s/\x11/\\u0011/g;s/\x12/\\u0012/g;s/\x13/\\u0013/g;s/\x14/\\u0014/g;s/\x15/\\u0015/g;s/\x16/\\u0016/g;s/\x17/\\u0017/g;s/\x18/\\u0018/g;s/\x19/\\u0019/g;s/\x1a/\\u001a/g;s/\x1b/\\u001b/g;s/\x1c/\\u001c/g;s/\x1d/\\u001d/g;s/\x1e/\\u001e/g;s/\x1f/\\u001f/g}' \
    -e 's/^/"/' -e 's/$/"/'
}

# --- the per-chunk jq classifier ------------------------------------------------
# Reads the lines of a chunk as JSON strings (see json_lines; never `jq -R`, see
# the header) and prints one JSON event per line on stdout,
# each tagged with _l (the line after its record), _x (1 on the last event of a
# record: a valid cursor position) and, on _x=1, _open (the turns open after
# the record). The state {ln, st} (line counter and open turns WITH their text)
# goes to stderr for the next chunk, after a one-line summary. Variables:
# $thread $rel $mts $ssto $baseln $emit_from $final $stuck $partials $sin.
JQ_CHUNK='def encsize:
  if type == "object" then
    with_entries(if .key == "encrypted_content" and (.value | type) == "string"
                 then .value = {encrypted: true, bytes: (.value | utf8bytelength)}
                 else .value |= encsize end)
  elif type == "array" then map(encsize)
  else . end;
def enc($raw): if ($raw | contains("\"encrypted_content\"")) then encsize else . end;
def textjoin:
  if type == "array"
  then ([.[] | select(type == "object" and (.text | type) == "string" and .text != "") | .text] | join("\n\n"))
  else "" end;
def openlist:
  map({t: .id, l: .first_line, o: .first_idkey, p: .partial} | with_entries(select(.value != null)));
def shove($k):
  if has($k) then shove("source_" + $k) | .["source_" + $k] = .[$k] else . end;
def prov_merge($prov):
  . as $c
  | reduce ($prov | keys_unsorted[]) as $k ($c;
      if has($k) and .[$k] != $prov[$k] then shove($k) else . end)
  + $prov;
def mkev($id; $type; $ts; $text; $payload):
  {source_event_id: $id, event_type: $type, occurred_at: $ts, text: $text, payload: $payload};
def tagged($next; $x; $st):
  . + {_l: $next, _x: $x} + (if $x == 1 then {_open: ($st | openlist)} else {} end);
def tagall($next; $st):
  . as $evs | [range(0; length) as $i | $evs[$i] | tagged($next; (if $i == ($evs | length) - 1 then 1 else 0 end); $st)];
def evtype($top; $pt; $role):
  if $top == "response_item" and $pt == "message" then
    (if $role == "assistant" then "codex_assistant_message"
     elif $role == "user" then "codex_prompt"
     elif $role == "developer" then "codex_developer_message"
     else "codex_record" end)
  elif $top == "response_item" then
    (if $pt == "reasoning" then "codex_reasoning"
     elif $pt == "custom_tool_call" then "codex_tool_call"
     elif $pt == "custom_tool_call_output" then "codex_tool_result"
     elif $pt == "function_call" then "codex_tool_call"
     elif $pt == "function_call_output" then "codex_tool_result"
     elif $pt == "web_search_call" then "codex_web_search"
     elif $pt == "tool_search_call" then "codex_tool_search"
     elif $pt == "tool_search_output" then "codex_tool_result"
     elif $pt == "compaction" then "codex_compaction"
     elif $pt == "agent_message" then "codex_agent_message"
     else "codex_record" end)
  elif $top == "event_msg" then
    (if $pt == "item_completed" then "codex_item"
     elif $pt == "task_started" then "codex_turn_started"
     elif $pt == "task_complete" then "codex_turn_complete"
     elif $pt == "turn_aborted" then "codex_turn_aborted"
     elif $pt == "thread_settings_applied" then "codex_settings"
     elif $pt == "thread_goal_updated" then "codex_goal"
     else "codex_record" end)
  elif $top == "turn_context" then "codex_turn_context"
  elif $top == "token_usage_record" then "codex_token_usage"
  elif $top == "compacted" then "codex_compaction"
  elif $top == "inter_agent_communication_metadata" then "codex_agent_trigger"
  else "codex_record" end;

def record_payload($rec; $raw; $p; $top; $pt; $idx; $line; $own; $legacy):
  ($p | enc($raw)) as $clean
  | ($rec | del(.type, .timestamp, .payload, .ordinal)) as $extra
  | ({thread_id: $thread, ordinal: $idx, line: $line, rollout_file: $rel, record_type: $top}
     + (if $pt != "" then {record_payload_type: $pt} else {} end)) as $prov
  | ($clean | prov_merge($prov))
    + (if $own then {} else {inherited: true} end)
    + (if $legacy then {legacy_ui: true} else {} end)
    + (if ($rec.payload != null and ($rec.payload | type) != "object") then {payload_value: $rec.payload} else {} end)
    + (if ($extra | length) > 0
       then ($extra | enc($raw)) as $x
            | (if $x.metadata != null then {record_metadata: $x.metadata} else {} end)
              + (($x | del(.metadata)) as $rest | if ($rest | length) > 0 then {record_extra: $rest} else {} end)
       else {} end);

# One record. Input: the state {ln, st}; output: the state with .ev = the
# events this record yields (only from $emit_from on: earlier lines are
# replayed to rebuild the open turns and emit nothing).
def step($raw):
  . as $s
  | $s.ln as $line
  | ($line >= $emit_from) as $emit
  | ($raw | try {v: fromjson} catch {bad: true}) as $parsed
  | if $parsed.bad or (($parsed.v | type) != "object") then
      if $parsed.bad and (($raw | gsub("[ \t\r]"; "")) == "") then .ev = []
      else
        .ev = (if $emit
               then [mkev("codex:" + $thread + ":ln" + ($line | tostring); "codex_record"; $mts; "";
                     (if $parsed.bad
                      then {unparsed_line: true, thread_id: $thread, line: $line, rollout_file: $rel,
                            bytes: ($raw | utf8bytelength), raw: $raw}
                      else {non_object_line: true, thread_id: $thread, line: $line, rollout_file: $rel,
                            value: $parsed.v} end))]
                    | tagall($line + 1; $s.st)
               else [] end)
      end
    else
      $parsed.v as $rec
      | (if ($rec.ordinal | type) == "number" then $rec.ordinal else null end) as $ord
      | (if $ord != null then ($ord | tostring) else "ln" + ($line | tostring) end) as $idkey
      | ($ord // $line) as $idx
      | (if ($rec.timestamp | type) == "string" and $rec.timestamp != "" then $rec.timestamp else $mts end) as $ts
      | (if ($rec.payload | type) == "object" then $rec.payload else {} end) as $p
      | (if ($p.type | type) == "string" then $p.type else "" end) as $pt
      | (if ($rec.type | type) == "string" then $rec.type else "" end) as $top
      | (if ($p.role | type) == "string" then $p.role else "" end) as $role
      | ($idx >= $ssto) as $own
      | (if ($top == "session_meta" and $line == 0)
            or ($top == "event_msg" and $pt == "token_count")
            or $top == "world_state" or $top == "realtime_item" then "SK"
         elif $top == "response_item" and $pt == "message" and $role == "assistant" and $own then "ASST"
         elif $top == "event_msg" and $pt == "task_started" and $own then "TST"
         elif $top == "event_msg" and ($pt == "task_complete" or $pt == "turn_aborted") and $own then "TEN"
         else "EV" end) as $kind
      | (if ($p.turn_id | type) == "string" then $p.turn_id else "" end) as $turnid
      | (if $own and $top == "event_msg" and ($pt == "user_message" or $pt == "agent_message" or $pt == "agent_reasoning")
         then true else false end) as $legacy
      | (if (.st | length) > 0
         then .st[-1].last_idkey = $idkey | .st[-1].last_ts = $ts else . end)
      | (if $kind == "ASST" and (.st | length) > 0
         then ($p.content | textjoin) as $t
              | if $t != ""
                then .st[-1].text = (if .st[-1].nmsg > 0 then .st[-1].text + "\n\n" + $t else $t end)
                     | .st[-1].nmsg += 1
                else . end
         else . end)
      | (if $kind == "TST"
         then .st += [{id: $turnid, first_line: $line, first_idkey: $idkey, text: "", nmsg: 0,
                       last_idkey: $idkey, last_ts: $ts, partial: ($partials[($line | tostring)] // null)}]
         else . end)
      | (if $kind == "TEN"
         then ([.st | to_entries[] | select(.value.id == $turnid)] | last // null) as $hit
              | if $hit == null then .closed = null
                else .closed = $hit.value | .st = (.st[:$hit.key] + .st[$hit.key + 1:]) end
         else .closed = null end)
      | .closed as $closed
      | .st as $st
      | .ev = (if ($emit and $kind != "SK")
               then ([mkev("codex:" + $thread + ":" + $idkey; evtype($top; $pt; $role); $ts; "";
                          record_payload($rec; $raw; $p; $top; $pt; $idx; $line; $own; $legacy))]
                    + (if $closed != null and $closed.nmsg > 0
                       then [mkev("codex:" + $thread + ":t" + $idkey; "codex_assistant_turn"; $ts; $closed.text;
                                  ($p | enc($raw))
                                  + {closed_by: "terminal", rollout_file: $rel, thread_id: $thread,
                                     turn_start: $closed.first_idkey}
                                  + (if $closed.partial != null then {supersedes: $closed.partial} else {} end))]
                       else [] end))
                    | tagall($line + 1; $st)
               else [] end)
      | del(.closed)
    end
  | .ln += 1;

# End of input: at a clean end of the file, a turn still open on a file that
# has not been written for MEMOIRE_CODEX_STUCK_S is emitted as it stands.
def finish:
  if $final and $stuck then
    reduce range(0; (.st | length)) as $i ({s: ., ev: []};
      .s.st[$i] as $e
      | ("codex:" + $thread + ":t" + $e.last_idkey + ":partial") as $id
      | if $e.nmsg > 0 and $e.partial != $id then
          .s.st[$i].partial = $id
          | . as $acc
          | .ev += [mkev($id; "codex_assistant_turn"; $e.last_ts; $e.text;
                         {turn_id: $e.id, closed_by: "adapter", rollout_file: $rel, thread_id: $thread,
                          turn_start: $e.first_idkey}
                         + (if $e.partial != null then {supersedes: $e.partial} else {} end))
                    | tagged($acc.s.ln; 1; $acc.s.st)]
        else . end)
    | .s + {ev: .ev}
  else .ev = [] end;

foreach ((inputs | {raw: .}), {end: true}) as $in (
  (($sin[-1] // {ln: $baseln, st: []}) | .ev = []);
  if $in.end then finish else step($in.raw) end;
  if $in.end
  then .ev[],
       (({ln: .ln, open: (.st | openlist)} | tojson + "\n") | stderr | empty),
       ((del(.ev) | tojson) | stderr | empty)
  else .ev[] end
)

'

total_files=0
total_events=0
failed_files=0
failed_events=0
rescans=0         # files whose cursor was rejected (read again from line 0)
events_run=0      # events queued across this run (cap accounting)
stopped=0         # a bound (time, events, signal) ended the run early
lines_left=0

time_up() {
  (( STOP == 1 )) && return 0
  (( MAX_SECONDS > 0 && EPOCHSECONDS - RUN_START >= MAX_SECONDS )) && return 0
  (( MAX_EVENTS > 0 && events_run >= MAX_EVENTS )) && return 0
  return 1
}

readonly HEAD_MAX=4096
declare -A FSID_BY_DEV=()
FILE_ID=""
# FILE_ID <- "<f_fsid>:<inode>", or "" when the filesystem reports no fsid.
# f_fsid is looked up once per st_dev per run (st_dev is stable within a boot).
file_id_for() { # file dev inode
  local fsid
  FILE_ID=""
  [[ "$2" =~ ^[0-9]+$ && "$3" =~ ^[0-9]+$ ]] || return 0
  fsid="${FSID_BY_DEV[$2]-}"
  if [[ -z "$fsid" ]]; then
    fsid="$(stat -f -c '%i' -- "$1" 2>/dev/null)" || fsid=""
    [[ "$fsid" =~ ^[0-9a-f]+$ && "$fsid" =~ [1-9a-f] ]] || fsid="-"
    FSID_BY_DEV[$2]="$fsid"
  fi
  [[ "$fsid" == "-" ]] || FILE_ID="$fsid:$3"
}

span_sha() { # file start len
  local s
  s="$(dd if="$1" iflag=skip_bytes,count_bytes skip="$2" count="$3" bs=4096 status=none 2>/dev/null \
    | sha256sum 2>/dev/null)" || return 1
  [[ "${s%% *}" =~ ^[0-9a-f]{64}$ ]] || return 1
  printf '%s' "${s%% *}"
}

# The fingerprints known for the current file (reset per file): taken from a
# cursor that matched, else computed by write_cursor. The tail is the window
# [max(0, offset - HEAD_MAX), offset).
HEAD_LEN="" HEAD_SHA="" TAIL_AT="" TAIL_SHA=""

# Whether the cursor loaded into CUR_* describes this file (rule in the
# header). CUR_STALE=1: it does, but must be rewritten in the current shape.
CUR_OFFSET=0 CUR_DEVINODE="" CUR_FILE_ID="" CUR_HEAD_LEN="" CUR_HEAD_SHA="" CUR_TAIL_SHA="" CUR_STALE=0
cursor_matches() { # file size inode
  CUR_STALE=0
  (( $2 >= CUR_OFFSET )) || return 1
  if [[ "$CUR_HEAD_LEN" =~ ^[0-9]+$ && "$CUR_HEAD_SHA" =~ ^[0-9a-f]{64}$ && "$CUR_TAIL_SHA" =~ ^[0-9a-f]{64}$ ]]; then
    if ! (( $2 == CUR_OFFSET )) || [[ -z "$FILE_ID" || "$CUR_FILE_ID" != "$FILE_ID" ]]; then
      local ts=$(( CUR_OFFSET > HEAD_MAX ? CUR_OFFSET - HEAD_MAX : 0 ))
      (( CUR_HEAD_LEN <= CUR_OFFSET )) || return 1
      [[ "$(span_sha "$1" 0 "$CUR_HEAD_LEN")" == "$CUR_HEAD_SHA" ]] || return 1
      [[ "$(span_sha "$1" "$ts" "$(( CUR_OFFSET - ts ))")" == "$CUR_TAIL_SHA" ]] || return 1
      [[ "$CUR_FILE_ID" == "$FILE_ID" ]] || CUR_STALE=1
    fi
    HEAD_LEN="$CUR_HEAD_LEN" HEAD_SHA="$CUR_HEAD_SHA" TAIL_AT="$CUR_OFFSET" TAIL_SHA="$CUR_TAIL_SHA"
    return 0
  fi
  [[ -n "$CUR_DEVINODE" && "${CUR_DEVINODE##*:}" == "$3" ]] || return 1
  CUR_STALE=1
}

# The same rollout under the other of sessions/Y/M/D/ and archived_sessions/.
moved_twin() { # abs-path
  local base="${1##*/}"
  [[ "$base" =~ ^rollout-([0-9]{4})-([0-9]{2})-([0-9]{2})T.*\.jsonl$ ]] || return 1
  if [[ -n "$ARCHIVED_ABS" && "$1" == "$ARCHIVED_ABS/$base" && -n "$SESSIONS_ABS" ]]; then
    printf '%s' "$SESSIONS_ABS/${BASH_REMATCH[1]}/${BASH_REMATCH[2]}/${BASH_REMATCH[3]}/$base"
  elif [[ -n "$SESSIONS_ABS" && "$1" == "$SESSIONS_ABS/${BASH_REMATCH[1]}/${BASH_REMATCH[2]}/${BASH_REMATCH[3]}/$base" && -n "$ARCHIVED_ABS" ]]; then
    printf '%s' "$ARCHIVED_ABS/$base"
  else
    return 1
  fi
}

# write_cursor OFFSET LINE OPEN_TURNS_JSON SETTLED  (globals: cursor_path,
# THREAD_JSON, FILE_JSON, file_abs, file_devinode, FILE_ID, HEAD_*, TAIL_*)
write_cursor() {
  local tmp want ts
  [[ "$DRY_RUN" == "0" && -n "${cursor_path:-}" ]] || return 0
  want=$(( $1 < HEAD_MAX ? $1 : HEAD_MAX ))
  if [[ "$HEAD_LEN" != "$want" || -z "$HEAD_SHA" ]]; then
    HEAD_LEN="" HEAD_SHA=""
    HEAD_SHA="$(span_sha "$file_abs" 0 "$want")" || return 1
    HEAD_LEN="$want"
  fi
  if [[ "$TAIL_AT" != "$1" || -z "$TAIL_SHA" ]]; then
    TAIL_AT="" TAIL_SHA=""
    ts=$(( $1 > HEAD_MAX ? $1 - HEAD_MAX : 0 ))
    TAIL_SHA="$(span_sha "$file_abs" "$ts" "$(( $1 - ts ))")" || return 1
    TAIL_AT="$1"
  fi
  tmp="$(mktemp "$cursor_path.tmp.XXXXXX" 2>/dev/null)" || return 1
  printf '{"v":2,"offset":%s,"line":%s,"settled":%s,"file_id":"%s","head_len":%s,"head_sha":"%s","tail_sha":"%s","dev_inode":"%s","thread_id":%s,"file":%s,"open_turns":%s,"updated_at":"%(%Y-%m-%dT%H:%M:%S%z)T"}\n' \
    "$1" "$2" "$4" "$FILE_ID" "$HEAD_LEN" "$HEAD_SHA" "$TAIL_SHA" "$file_devinode" "$THREAD_JSON" "$FILE_JSON" "$3" -1 >"$tmp" 2>/dev/null \
    && chmod 600 "$tmp" 2>/dev/null \
    && mv -f "$tmp" "$cursor_path" 2>/dev/null && return 0
  rm -f "$tmp" 2>/dev/null
  return 1
}

# queue_batch FILE -> rc; sets WROTE to the number of durable items
queue_batch() {
  local file="$1" qrc
  : >"$PATHS"
  "$QUEUE_BIN" -project "$CUR_PROJECT" -session-id "$CUR_THREAD" -source-adapter codex-cli \
    -cwd "$CUR_CWD" ${CUR_ROOTARGS[@]+"${CUR_ROOTARGS[@]}"} -batch-file "$file" >"$PATHS" 2>"$QERR"
  qrc=$?
  WROTE="$(wc -l <"$PATHS" 2>/dev/null)"
  WROTE="${WROTE//[^0-9]/}"
  [[ -n "$WROTE" ]] || WROTE=0
  return "$qrc"
}

dry_print() { # events-file
  jq -c --arg project "$CUR_PROJECT" --arg session "$CUR_THREAD" --arg cwd "$CUR_CWD" \
    'del(._l, ._x, ._open) + {project: $project, session_id: $session, cwd: $cwd}' "$1"
}

CUR_PROJECT="" CUR_THREAD="" CUR_CWD="" CUR_ROOTARGS=()

process_file() {
  local file="$1"
  local file_abs file_size="" file_mtime="" file_dev="" file_ino="" file_devinode="" cursor_path="" locked=0 stuck=false
  file_abs="$(abs_path "$file")"
  read -r file_size file_mtime file_dev file_ino < <(stat -c '%s %Y %d %i' -- "$file" 2>/dev/null) || true
  if [[ ! "$file_size" =~ ^[0-9]+$ || ! "$file_mtime" =~ ^[0-9]+$ ]]; then
    log "$(basename "$file"): stat failed, skipped"
    return 1
  fi
  [[ "$file_dev" =~ ^[0-9]+$ && "$file_ino" =~ ^[0-9]+$ ]] && file_devinode="$file_dev:$file_ino"
  file_id_for "$file" "$file_dev" "$file_ino"
  HEAD_LEN="" HEAD_SHA="" TAIL_AT="" TAIL_SHA="" CUR_STALE=0
  (( NOW_EPOCH - file_mtime >= STUCK_S )) && stuck=true

  local c_off=0 c_line=0 c_di="" c_settled=0 c_amin="" c_partials='{}' c_opens='[]' have_cursor=0
  local c_fid="" c_hl="" c_hs="" c_ts=""
  if [[ "$DRY_RUN" == "0" ]] && cursor_dir_ready; then
    cursor_path="$CURSOR_DIR/$(printf '%s' "$file_abs" | sha256sum | cut -d' ' -f1).json"
    # Sibling lock: the cursor itself is replaced by mv (cc-transcripts).
    if touch "$cursor_path.lock" 2>/dev/null && exec 9>"$cursor_path.lock"; then
      if flock -n 9 2>/dev/null; then
        locked=1
      else
        exec 9>&-
        log "$(basename "$file"): skipped=locked"
        return 0
      fi
    else
      log "$(basename "$file"): cursor lock unavailable, no cursor"
      cursor_path=""
    fi
  fi

  local adopted=0 twin twin_cursor
  if [[ "$TAIL" == "1" && -n "$cursor_path" && ! -e "$cursor_path" ]] \
     && twin="$(moved_twin "$file_abs")" && [[ ! -e "$twin" ]]; then
    twin_cursor="$CURSOR_DIR/$(printf '%s' "$twin" | sha256sum | cut -d' ' -f1).json"
    [[ -r "$twin_cursor" ]] && mv -f -- "$twin_cursor" "$cursor_path" 2>/dev/null && adopted=1
  fi

  # The cheap idle check comes before any parsing: a file with nothing new and
  # nothing to settle costs one read of the cursor.
  if [[ "$TAIL" == "1" && -n "$cursor_path" && -r "$cursor_path" ]]; then
    local raw
    raw="$(<"$cursor_path")"
    local r_off="" r_fid="" r_settled=0 r_empty=0
    [[ "$raw" =~ \"offset\":([0-9]+) ]] && r_off="${BASH_REMATCH[1]}"
    [[ "$raw" =~ \"settled\":([0-9]+) ]] && r_settled="${BASH_REMATCH[1]}"
    [[ "$raw" =~ \"file_id\":\"([0-9a-f]+:[0-9]+)\" ]] && r_fid="${BASH_REMATCH[1]}"
    [[ "$raw" == *'"open_turns":[]'* ]] && r_empty=1
    if [[ "$adopted" == "0" && "$r_off" == "$file_size" && -n "$FILE_ID" && "$r_fid" == "$FILE_ID" ]] \
       && { (( r_empty == 1 )) || [[ "$stuck" == false ]] || (( r_settled == file_mtime )); }; then
      [[ "$locked" == "1" ]] && exec 9>&-
      return 0
    fi
    have_cursor=1
  fi

  local meta_ok=0
  head -n 1 -- "$file" 2>/dev/null | jq -j --arg rel "${file/#$HOME/\~}" '
      select(.type == "session_meta") | (.payload // {}) as $p
      | ($p.history_base.thread_id // $p.id // "") as $t
      | (($p.subagent_history_start_ordinal // 0) | if type == "number" and . >= 0 and . == floor then . else 0 end) as $s
      | ([$t, ($p.cwd // ""), ($s | tostring), (.timestamp // ""), ($t | tojson)] | join("\u001f")) + "\n",
        ({source_event_id: ("codex:" + $t + ":facts"), event_type: "codex_thread_facts",
          occurred_at: (.timestamp // ""), text: "",
          payload: ($p + {parent_thread_id: ($p.parent_thread_id // null), thread_id: $t, rollout_file: $rel,
                          inherited_ordinal_range: [0, $s], inherited_record_count: $s})} | tojson) + "\n"
    ' >"$META" 2>/dev/null && meta_ok=1
  local thread_id cwd ssto meta_ts THREAD_JSON
  IFS=$'\x1f' read -r thread_id cwd ssto meta_ts THREAD_JSON <"$META"
  if [[ "$meta_ok" != 1 || -z "$thread_id" ]]; then
    log "skip (no session_meta thread id at line 0): $file"
    [[ "$locked" == "1" ]] && exec 9>&-
    return 1
  fi
  [[ "$ssto" =~ ^[0-9]+$ ]] || ssto=0
  if [[ -z "$meta_ts" ]]; then
    meta_ts="$(date -u -d "@$file_mtime" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null)"
  fi
  tail -n +2 "$META" >"$FACTS"
  CUR_THREAD="$thread_id"
  CUR_CWD="$cwd"
  local rel="${file/#$HOME/\~}" FILE_JSON
  FILE_JSON="$(jq -nr --arg f "$file_abs" '$f | tojson' 2>/dev/null)" || FILE_JSON=''
  [[ -n "$FILE_JSON" ]] || FILE_JSON='""'

  if [[ -n "$cwd" ]] && capture_off_cwd "$cwd"; then
    log "skip (capture off): $file"
    [[ "$locked" == "1" ]] && exec 9>&-
    return 0
  fi
  if [[ -n "$PROJECT_OVERRIDE" ]]; then
    CUR_PROJECT="$PROJECT_OVERRIDE" MRP_ROOT="" MRP_SOURCE=""
  else
    memoire_resolve_v "${cwd:-unknown}"
    CUR_PROJECT="$MRP_KEY"
  fi
  CUR_ROOTARGS=()
  [[ -n "$MRP_ROOT" ]] && CUR_ROOTARGS=(-project-root "$MRP_ROOT" -project-source "$MRP_SOURCE")

  # --- where to start ---------------------------------------------------------
  if [[ "$TAIL" == "1" && "$have_cursor" == "1" ]]; then
    local row
    row="$(jq -j '[(.offset // 0), (.line // 0), (.dev_inode // ""), (.settled // 0),
                   ((.open_turns // []) | map(.l) | min // ""),
                   ((.open_turns // []) | map(select(.p != null) | {key: (.l | tostring), value: .p}) | from_entries | tojson),
                   ((.open_turns // []) | tojson), (.file_id // ""), (.head_len // ""), (.head_sha // ""), (.tail_sha // "")]
                  | map(tostring) | join("\u001f")' "$cursor_path" 2>/dev/null)"
    if [[ -n "$row" ]]; then
      IFS=$'\x1f' read -r c_off c_line c_di c_settled c_amin c_partials c_opens c_fid c_hl c_hs c_ts <<<"$row"
    fi
  fi
  CUR_OFFSET="$c_off" CUR_DEVINODE="$c_di" CUR_FILE_ID="$c_fid" CUR_HEAD_LEN="$c_hl" CUR_HEAD_SHA="$c_hs" CUR_TAIL_SHA="$c_ts"
  if ! [[ "$c_off" =~ ^[0-9]+$ && "$c_line" =~ ^[0-9]+$ && "$c_settled" =~ ^[0-9]+$ ]] \
     || ! cursor_matches "$file" "$file_size" "$file_ino" \
     || ! at_line_start "$file" "$c_off"; then
    # Cursorless, rewritten, truncated, or damaged: a full pass from line 0.
    if [[ "$have_cursor" == "1" ]]; then
      log "$(basename "$file"): cursor does not match the file, full pass"
      rescans=$((rescans + 1))
    fi
    c_off=0 c_line=0 c_amin="" c_partials='{}' c_opens='[]' CUR_STALE=0 HEAD_LEN="" HEAD_SHA="" TAIL_AT="" TAIL_SHA=""
  fi
  (( adopted == 1 )) && CUR_STALE=1
  local range_end
  range_end="$(last_line_end "$file" "$c_off" "$file_size")"
  local a_line="$c_line" a_off="$c_off"
  if [[ "$c_amin" =~ ^[0-9]+$ ]] && (( c_amin < c_line )); then
    a_line="$c_amin"
    a_off="$(head -n "$a_line" -- "$file" 2>/dev/null | wc -c)"
    a_off="${a_off//[^0-9]/}"
    if [[ -z "$a_off" ]] || ! at_line_start "$file" "$a_off"; then
      a_line=0 a_off=0 c_off=0 c_line=0 c_partials='{}' c_opens='[]'
      range_end="$(last_line_end "$file" 0 "$file_size")"
    fi
  fi
  local idle_eval=0
  if (( range_end <= c_off )); then
    # Nothing new. Still worth a pass when open turns wait for the stuck rule.
    if [[ "$c_opens" != '[]' && "$stuck" == true ]] && (( c_settled != file_mtime )); then
      idle_eval=1
    else
      if [[ "$CUR_STALE" == "1" ]]; then
        write_cursor "$c_off" "$c_line" "$c_opens" "$c_settled" || log "$(basename "$file"): cursor write failed"
      fi
      [[ "$locked" == "1" ]] && exec 9>&-
      return 0
    fi
  fi

  local start_events=$events_run failed=0 pass_start_line=$c_line
  # One facts event whenever the pass starts at line 0.
  if (( c_off == 0 )); then
    if [[ "$DRY_RUN" == "1" ]]; then
      dry_print "$FACTS"
    elif ! queue_batch "$FACTS" || (( WROTE != 1 )); then
      failed_events=$((failed_events + 1))
      [[ "$locked" == "1" ]] && exec 9>&-
      log "$(basename "$file"): queue write failed (thread facts)"
      return 1
    else
      events_run=$((events_run + 1))
    fi
  fi

  # --- chunk loop -------------------------------------------------------------
  local cur_off=$a_off cur_line=$a_line chunk_lines=$BATCH_RECORDS
  local ch_size is_final n_ev summary new_line new_off t0 st_in="$ST_A" st_out="$ST_B" tmpst
  : >"$st_in"
  local ended_clean=0
  while (( cur_off < range_end )); do
    if time_up; then stopped=1; break; fi
    t0=$SECONDS
    tail -c "+$((cur_off + 1))" -- "$file" 2>/dev/null | head -c "$((range_end - cur_off))" | head -n "$chunk_lines" >"$CH"
    ch_size="$(stat -c '%s' "$CH" 2>/dev/null)"
    [[ "$ch_size" =~ ^[0-9]+$ ]] && (( ch_size > 0 )) || { log "$(basename "$file"): read failed at byte $cur_off"; failed=1; break; }
    is_final=false
    (( cur_off + ch_size >= range_end )) && is_final=true
    json_lines <"$CH" >"$CHJ"
    jq -n -c --slurpfile sin "$st_in" \
      --arg thread "$thread_id" --arg rel "$rel" --arg mts "$meta_ts" \
      --argjson ssto "$ssto" --argjson baseln "$cur_line" --argjson emit_from "$c_line" \
      --argjson final "$is_final" --argjson stuck "$stuck" --argjson partials "$c_partials" \
      "$JQ_CHUNK" "$CHJ" >"$EV" 2>"$st_out"
    local jqrc=$?
    if (( jqrc != 0 )); then
      if (( STOP == 1 )); then stopped=1; break; fi
      log "$(basename "$file"): record scan failed (rc=$jqrc) at line $cur_line; cursor not advanced"
      failed=1
      break
    fi
    IFS= read -r summary <"$st_out"
    if ! [[ "$summary" =~ ^\{\"ln\":([0-9]+),\"open\":(\[.*\])\}$ ]]; then
      log "$(basename "$file"): classifier state unreadable at line $cur_line; cursor not advanced"
      failed=1
      break
    fi
    new_line="${BASH_REMATCH[1]}"
    local new_open="${BASH_REMATCH[2]}"
    n_ev="$(wc -l <"$EV")"
    n_ev="${n_ev//[^0-9]/}"
    new_off=$((cur_off + ch_size))
    local settled_val=0
    if [[ "$is_final" == true && "$stuck" == true ]]; then settled_val="$file_mtime"; fi

    if (( n_ev > 0 )); then
      if [[ "$DRY_RUN" == "1" ]]; then
        dry_print "$EV"
      else
        queue_batch "$EV"
        local qrc=$?
        if (( qrc != 0 || WROTE != n_ev )); then
          # Only the durable prefix counts. The cursor moves to the last
          # complete record whose events are all inside it.
          failed_events=$((failed_events + n_ev - WROTE))
          events_run=$((events_run + WROTE))
          if (( WROTE > 0 )); then
            local pre pl pnl poff
            pre="$(head -n "$WROTE" "$EV" | jq -c -n 'reduce inputs as $e (null; if $e._x == 1 then {l: $e._l, o: $e._open} else . end)' 2>/dev/null)"
            if [[ "$pre" =~ ^\{\"l\":([0-9]+),\"o\":(\[.*\])\}$ ]]; then
              pl="${BASH_REMATCH[1]}"
              poff="$(head -n "$((pl - cur_line))" "$CH" | wc -c)"
              poff="${poff//[^0-9]/}"
              [[ -n "$poff" ]] && write_cursor "$((cur_off + poff))" "$pl" "${BASH_REMATCH[2]}" 0
            fi
          fi
          if (( STOP == 1 )); then stopped=1; else
            failed=1
            log "$(basename "$file"): queue write failed after $WROTE of $n_ev events of the chunk at line $cur_line: $(head -c 300 "$QERR" 2>/dev/null | tr '\n' ' ')"
          fi
          break
        fi
        events_run=$((events_run + n_ev))
      fi
    fi
    # Cursor after every chunk that got past the old cursor, queued events, or
    # settled a stuck evaluation (so an idle file is not replayed every tick).
    if (( new_line > c_line || n_ev > 0 || settled_val != c_settled )); then
      write_cursor "$new_off" "$new_line" "$new_open" "$settled_val" || log "$(basename "$file"): cursor write failed"
    fi
    tmpst="$st_in"; st_in="$st_out"; st_out="$tmpst"
    cur_off=$new_off
    cur_line=$new_line
    (( cur_off >= range_end )) && ended_clean=1
    # A chunk that took long halves the next one (the 20 s batch bound).
    if (( SECONDS - t0 >= (BATCH_SECONDS + 1) / 2 || ch_size > 134217728 )) && (( chunk_lines > 1 )); then
      chunk_lines=$(( (chunk_lines + 1) / 2 ))
    fi
  done
  if (( idle_eval == 1 && failed == 0 && stopped == 0 )) && [[ "$ended_clean" == 0 ]]; then
    ended_clean=1
  fi
  [[ "$locked" == "1" ]] && exec 9>&-

  if (( failed == 1 )); then
    log "$(basename "$file"): stopped at line $cur_line"
    return 1
  fi
  if (( stopped == 1 )); then
    local left
    left="$(tail -c "+$((cur_off + 1))" -- "$file" 2>/dev/null | head -c "$((range_end - cur_off))" | wc -l)"
    left="${left//[^0-9]/}"
    lines_left=$(( lines_left + ${left:-0} ))
    log "$(basename "$file"): bound reached at line $cur_line, ${left:-0} lines left"
    return 0
  fi
  log "$(basename "$file"): thread=$CUR_THREAD project=$CUR_PROJECT events=$((events_run - start_events)) line=$cur_line"
  return 0
}

for f in "${FILES[@]}"; do
  if time_up; then stopped=1; break; fi
  if [[ ! -r "$f" ]]; then
    log "skip (unreadable): $f"
    failed_files=$((failed_files + 1))
    continue
  fi
  before=$events_run
  if process_file "$f"; then
    total_files=$((total_files + 1))
  else
    failed_files=$((failed_files + 1))
  fi
  total_events=$((total_events + events_run - before))
  if (( stopped == 1 )); then break; fi
done

log "done: files=$total_files failed_files=$failed_files events=$total_events failed_events=$failed_events stopped=$stopped lines_left=$lines_left rescans=$rescans"
[[ "$SUMMARY" == "1" ]] && printf '%s\t%s\t%s\t%s\t%s\n' "$total_files" "$total_events" "$failed_files" "$stopped" "$lines_left"
if (( STOP == 1 )); then exit 143; fi
if (( failed_files > 0 || failed_events > 0 )); then
  exit 1
fi
exit 0
