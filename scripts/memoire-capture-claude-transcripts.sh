#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
#
# Backfill AND stream assistant turns from Claude Code session transcripts into
# Memoire's file queue (Lane 3 turn-ingest step 1, ruled 2026-07-10; streaming
# cursor 2026-09-08).
#
# Usage:
#   memoire-capture-claude-transcripts.sh [OPTIONS] <transcript.jsonl|dir> ...
#
# Options:
#   --dry-run          print what would be queued; writes nothing at all (no cursor, no lock)
#   --tail             per-transcript byte cursor: process only the bytes appended
#                      since the last pass. A cursorless file is read WHOLE — a
#                      cursor is NEVER seeded at EOF.
#   --quiet-for <sec>  directory scans only: skip files whose mtime is younger
#                      than <sec> seconds (the capture tick uses 120)
#   --since <sec>      directory scans only: consider only files whose mtime is
#                      within the last <sec> seconds (the tick uses 172800 = 48h)
#   --emit-sessions    print "<project>\t<session_id>\t<new_turns>" on STDOUT for
#                      every file that queued new turns (a continuation event is
#                      not a turn); logging stays on stderr
#   --from-start       repair mode: read each given file from offset 0 whatever
#                      its cursor says, then leave the cursor at the end of the
#                      last complete line (implies the --tail cursor and flock
#                      machinery; with --dry-run nothing is written). Re-queues
#                      every id: the hub collapses what it already holds, and an
#                      old truncated turn gains its continuation event.
#   -h, --help         print this block
#
# Without the streaming flags the pass is the retro backfill it was born as.
#
# Scope per the operator ruling: assistant `text` content blocks only (no
# thinking, no tool_use, no user or attachment line — tool events are already
# captured live). Sub-agent text is captured since 2026-10-02 (reversing the
# main-session-only scope of 2026-07-10): directory scans include subagents/ at
# any depth (subagents/agent-<id>.jsonl, subagents/workflows/wf_<id>/...) and
# sidechain lines are kept, in sub-agent files and inline in a main transcript.
# A sidechain event keeps the parent's session id and adds sidechain: true and
# agent_id to its payload, plus agent_type, parent_tool_use_id and spawn_depth
# when the sibling agent-<id>.meta.json is readable (a missing or malformed meta
# file never blocks capture). Compaction agents (agent-acompact-*) write a
# model-made summary of the session, not original work: it is queued as
# claude_compact_summary (source_event_id cc-compact:<sid>:<uuid>), never as a
# turn, and is not counted by --emit-sessions. A sub-agent is attributed to the
# PARENT session's home (see below), even when it ran in another cwd.
# Events carry source_adapter=claude_code_transcript_backfill and a
# backfill_generation marker in the payload so D97 retro-sweep tooling can
# target exactly this generation. Idempotent: source_event_id is deterministic
# per (session, line uuid) and the hub collapses duplicates on it (memoire:D153),
# so a re-captured range costs queue churn, never duplicate events.
#
# Directories are scanned for *.jsonl. Project is
# resolved per line via the shared paths->id resolver, attributed to the
# session home (first cwd seen for that session id on a main-session line; for a
# sub-agent file the parent transcript's home: its cursor's session_homes, else
# its first non-sidechain cwd for that session id; the sub-agent's own first cwd
# only when the parent transcript is absent, for that run, never recorded),
# with basename as the fail-open fallback (MEMOIRE_PROJECT still overrides).
#
# Long turns (operator ruling 2026-10-02: truncation is a capture failure).
# A turn of at most 6000 characters is ONE event. A longer turn is a
# deterministic sequence: the first event is exactly what this reader always
# emitted (id cc-turn:<sid>:<uuid>, the first 6000 characters with their
# trailing newlines stripped, text_truncated: true), followed by ONE
# continuation event carrying all the remaining text, starting where the first
# event's text actually ends (so the stripped newlines open it and first +
# continuation is the source text; text_offset is that character offset):
# same event type, adapter, session, cwd and occurred_at, id
# cc-turn:<sid>:<uuid>:rest (cc-compact:<sid>:<uuid>:rest for a compaction
# summary), payload = the first event's payload plus continues_line_uuid and
# text_offset, text_truncated: false. The hub keeps the first write of an
# id, so a turn ingested truncated by an older reader is repaired by re-reading
# its file with --from-start: the first event collapses, the continuation is
# new. Where first + continuation cannot equal the source: a NUL inside the
# first 6000 characters is dropped from the first event (bash, as ever); a turn
# of at most 6000 characters loses its trailing newlines (as ever); a turn whose
# first 6000 characters are all newlines (never ingested before) has its
# leading newlines dropped, text_offset still counting them.
# The split point is FIXED at 6000 (slice by jq characters) and is not
# read from the environment: MEMOIRE_BACKFILL_TEXT_MAX no longer applies to this
# reader (the Qwen reader keeps it), because a moved boundary would break the
# convergence with events already on the hub. The continuation text travels
# through memoire-queue -text-file (a file under the run's temp dir, removed
# after the write), never argv.
#
# Cursors (--tail): one JSON file per transcript under
# ${MEMOIRE_CAPTURE_CURSOR_DIR:-$MEMOIRE_HOME/capture-cursors/cc-transcripts}
# (dir 0700, files 0600, written tmp+mv), named <sha256 of abs path>.json and
# carrying path, file_id, head_len, head_sha, tail_sha, dev_inode, offset,
# session_homes and updated_at. file_id is <statfs f_fsid>:<inode>: st_dev is
# renumbered at every mount on btrfs, f_fsid derives from the filesystem UUID
# and subvolume. head_sha is the sha256 of the first head_len = min(4096,
# offset) bytes, tail_sha that of the (up to) 4096 bytes ending at the offset.
# The cursor describes the file when the size is at least the offset and either
# the file is idle (size = offset) with the same file_id, or both windows still
# hash the same — so a file replaced by an appended copy (a Syncthing receiver
# renames every update over the old inode) resumes at its offset, while a
# reused inode or a copy whose content changed inside either window (a sync conflict
# winner) is read again. A change confined between the two windows, or a
# same-size rewrite of an idle file, is not detected: the writers append. A cursor written
# before file_id (dev_inode only) is accepted when its inode still matches and
# is then rewritten in the current shape. Anything else means a full pass from
# 0, logged. dev_inode is still written for a rollback to the older reader.
# Only bytes [offset, last_newline) are consumed: a transcript can
# be appended to WHILE it is read, so a trailing partial line is always left for
# the next pass. session_homes is a MAP {sessionId: first cwd seen}, persisted
# because a tail chunk has no first line of its own — each session must keep
# attributing to its OWN launch project (liaison:D238), including the second
# session id a resumed transcript carries. A non-blocking per-cursor flock
# serializes the checkpoint hook against the timer tick; a contended file is
# skipped this pass and logged.
# Every cursor problem degrades to a full pass plus one stderr line — hub dedup
# keeps that free of duplicate events, not of hub raw/ files (one per POST).
#
# What is skipped is counted and logged. A queue write that fails (first event,
# continuation, text file, temp dir, payload) is not counted as queued, and the
# file's cursor is left at (or put back to) the offset where this pass started
# reading it (--from-start and a non-tail pass start at 0: the cursor is
# removed): the next pass over that file re-reads from there and the hub
# collapses what was already sent. The timer tick only scans files modified in
# its --since window, so after an exit 2 of a manual --from-start or non-tail
# run, re-run the same command until it exits 0. The log line and the final
# "done:" line say "queue writes FAILED" and the exit code is 2; --emit-sessions
# prints only the turns actually queued in that pass (replay reports the queued
# ones again). A line that is not a JSON object is skipped ALONE and counted
# ("N unparseable lines skipped"); every valid line after it is read. A line
# that fails to parse is retried once with lone UTF-16 surrogate escapes
# replaced by U+FFFD (valid pairs untouched); if it then parses it is captured
# and counted ("N lines repaired (lone surrogate)"); one that still cannot be
# parsed is skipped, counted and logged. An
# assistant text line without cwd is captured with an empty cwd (project from
# the session home); without timestamp, or with one that is not an RFC 3339
# string the queue writer accepts (Go RFC3339Nano), it takes the nearest previous line's
# timestamp in the range, else the file's mtime, and the payload carries
# occurred_at_inferred: true; without uuid or sessionId it cannot get an id and
# is counted ("N lines without uuid or sessionId skipped"). Only main-session
# lines and the parent lookup set a session's home: a sidechain line never does.
# --dry-run writes nothing at all (no cursor directory, no lock file, no temp
# directory); with --tail it only READS an existing cursor.
# A NON-tail full pass also refreshes the cursor (best effort) so a SessionEnd
# capture marks the file consumed and the next tick is a stat-only no-op.
#
# The --quiet-for trade: mtime is refreshed by every transcript write, so a long
# tool-heavy turn keeps a file permanently "fresh" and is then captured only at
# the next PreCompact/SessionEnd checkpoint. The last-newline rule already makes
# a mid-write read safe, so the gate only avoids churning a file the session is
# actively writing — it can stay small.

set -uo pipefail

# liaison:D239 session-level kill switch: honored directly (not via the resolver) so
# it works even if the resolver script is missing or broken.
case "${MEMOIRE_CAPTURE:-}" in off|0|false) exit 0 ;; esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QUEUE_BIN="${MEMOIRE_QUEUE_BIN:-$ROOT/bin/memoire-queue}"
RESOLVER="${MEMOIRE_RESOLVE_PROJECT:-$ROOT/scripts/memoire-resolve-project.sh}"
# Fixed split point; deliberately NOT read from the environment (see header).
readonly TEXT_MAX=6000
RUN_TMP=""
cleanup_run_tmp() { [[ -n "$RUN_TMP" ]] && rm -rf "$RUN_TMP"; }
trap cleanup_run_tmp EXIT
MEMOIRE_HOME_DIR="${MEMOIRE_HOME:-$HOME/.memoire}"
CURSOR_DIR="${MEMOIRE_CAPTURE_CURSOR_DIR:-$MEMOIRE_HOME_DIR/capture-cursors/cc-transcripts}"

# Resolve a project id via the shared paths->id resolver (liaison:D238/liaison:D239),
# fail-open to basename. Sets MRP_KEY/MRP_ROOT/MRP_SOURCE — globals, not
# command substitution (a subshell would drop root/source, which ride the
# queue write for identity-conflict detection, liaison:D239 item 4).
#   memoire_resolve_v <event-dir> [session-home-dir]
MRP_KEY="" MRP_ROOT="" MRP_SOURCE=""
memoire_resolve_v() {
  local out
  MRP_KEY="" MRP_ROOT="" MRP_SOURCE=""
  if out="$("$RESOLVER" --verbose "$1" "${2:-}" 2>/dev/null)" && [[ -n "$out" ]]; then
    IFS=$'\t' read -r MRP_KEY MRP_ROOT MRP_SOURCE <<<"$out"
  fi
  if [[ -z "$MRP_KEY" ]]; then
    local d="${2:-$1}"
    MRP_KEY="$(basename "$d" 2>/dev/null)" || MRP_KEY="$d"
    MRP_ROOT="" MRP_SOURCE=""
  fi
}

# liaison:D239 capture kill switches (`capture: false` in the nearest .memoire, or
# MEMOIRE_CAPTURE=off). Off ONLY on an explicit "off" from the resolver —
# fail-open otherwise. Cached per session id: the gate runs once per session,
# not once per transcript line.
declare -A MRP_CAPTURE_OFF
memoire_capture_off_session() {
  local sid="$1" dir="$2" home="$3"
  if [[ -z "${MRP_CAPTURE_OFF[$sid]:-}" ]]; then
    if [[ "$("$RESOLVER" --capture-enabled "$dir" "$home" 2>/dev/null)" == "off" ]]; then
      MRP_CAPTURE_OFF[$sid]=1
    else
      MRP_CAPTURE_OFF[$sid]=0
    fi
  fi
  [[ "${MRP_CAPTURE_OFF[$sid]}" == "1" ]]
}

# Session-home attribution: first cwd seen per session id becomes the whole
# session's home, so a session that roams across repos still attributes to its
# launch project. Per-event cwd stays stored on each event unchanged. In --tail
# mode the cursor's session_homes map seeds this one per session id: chunk 2
# has no first line of its own.
declare -A MRP_SESSION_HOME
GENERATION="${MEMOIRE_BACKFILL_GENERATION:-2026-07-10-cc-transcript-backfill}"
DRY_RUN=0
TAIL=0
EMIT_SESSIONS=0
FROM_START=0
QUIET_FOR=""
SINCE_S=""

log() { printf 'memoire-capture-claude-transcripts: %s\n' "$*" >&2; }

PATHS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --tail) TAIL=1 ;;
    --emit-sessions) EMIT_SESSIONS=1 ;;
    --from-start) FROM_START=1; TAIL=1 ;;
    --quiet-for) QUIET_FOR="${2:-}"; shift ;;
    --quiet-for=*) QUIET_FOR="${1#*=}" ;;
    --since) SINCE_S="${2:-}"; shift ;;
    --since=*) SINCE_S="${1#*=}" ;;
    -h|--help) sed -n '3,30p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) PATHS+=("$1") ;;
  esac
  shift
done
# Fail-open on knob typos: a nonnumeric window is no window at all.
[[ "$QUIET_FOR" =~ ^[0-9]+$ ]] || { [[ -z "$QUIET_FOR" ]] || log "ignoring non-numeric --quiet-for"; QUIET_FOR=""; }
[[ "$SINCE_S" =~ ^[0-9]+$ ]] || { [[ -z "$SINCE_S" ]] || log "ignoring non-numeric --since"; SINCE_S=""; }

NOW_EPOCH="$(date +%s)"
FILES=()
for arg in ${PATHS[@]+"${PATHS[@]}"}; do
  if [[ -d "$arg" ]]; then
    findargs=(-name '*.jsonl')
    [[ -n "$SINCE_S" ]] && findargs+=(-newermt "@$((NOW_EPOCH - SINCE_S))")
    [[ -n "$QUIET_FOR" ]] && findargs+=(! -newermt "@$((NOW_EPOCH - QUIET_FOR))")
    while IFS= read -r f; do FILES+=("$f"); done \
      < <(find "$arg" "${findargs[@]}" | sort)
  elif [[ -f "$arg" ]]; then
    FILES+=("$arg")
  else
    log "skipping missing path: $arg"
  fi
done

# Attribution forks (scripts/memoire-attribute-claude.py) are headless copies
# of a session under their own session id: never a session to ingest. The
# worker registers each fork id BEFORE launching it, so the skip also holds
# while the fork is still running and its transcript still sits in the tree.
FORK_IDS_FILE="${MEMOIRE_ATTRIB_FORK_IDS:-$MEMOIRE_HOME_DIR/hooks/attrib/fork-ids}"
if [[ -r "$FORK_IDS_FILE" && ${#FILES[@]} -gt 0 ]]; then
  declare -A FORK_ID_SET=()
  while IFS= read -r fid; do [[ -n "$fid" ]] && FORK_ID_SET["$fid"]=1; done < "$FORK_IDS_FILE"
  KEPT=()
  skipped_forks=0
  for f in "${FILES[@]}"; do
    base="${f##*/}"; base="${base%.jsonl}"
    if [[ -n "${FORK_ID_SET[$base]:-}" ]]; then skipped_forks=$((skipped_forks + 1)); continue; fi
    # A fork's own sub-agents live under <forkId>/subagents/.
    if [[ "$f" == */subagents/* ]]; then
      fdir="${f%%/subagents/*}"; fdir="${fdir##*/}"
      if [[ -n "${FORK_ID_SET[$fdir]:-}" ]]; then skipped_forks=$((skipped_forks + 1)); continue; fi
    fi
    KEPT+=("$f")
  done
  if [[ "$skipped_forks" -gt 0 ]]; then
    log "skipped $skipped_forks attribution fork transcript(s)"
    [[ ${#KEPT[@]} -gt 0 ]] || exit 0
  fi
  FILES=(${KEPT[@]+"${KEPT[@]}"})
fi

if [[ ${#FILES[@]} -eq 0 ]]; then
  log "no transcript files given"
  exit 1
fi
if [[ "$DRY_RUN" == "0" && ! -x "$QUEUE_BIN" ]]; then
  log "queue writer not found: $QUEUE_BIN; run make build"
  exit 1
fi

# --- cursor plumbing (fail-open everywhere) ----------------------------------
CURSORS_OK=1
command -v sha256sum >/dev/null 2>&1 \
  || { CURSORS_OK=0; log "sha256sum missing: cursors disabled, full passes only"; }

cursor_dir_ready() {
  [[ "$CURSORS_OK" == "1" ]] || return 1
  [[ -d "$CURSOR_DIR" && -w "$CURSOR_DIR" ]] && return 0
  mkdir -p "$CURSOR_DIR" 2>/dev/null || {
    CURSORS_OK=0
    log "cursor dir unusable: $CURSOR_DIR"
    return 1
  }
  chmod 700 "$CURSOR_DIR" 2>/dev/null
  chmod 700 "$(dirname "$CURSOR_DIR")" 2>/dev/null
  [[ -w "$CURSOR_DIR" ]] || {
    CURSORS_OK=0
    log "cursor dir not writable: $CURSOR_DIR"
    return 1
  }
  return 0
}

abs_path() {
  local p="$1" r
  if r="$(readlink -f -- "$p" 2>/dev/null)" && [[ -n "$r" ]]; then printf '%s' "$r"; return 0; fi
  case "$p" in /*) printf '%s' "$p" ;; *) printf '%s/%s' "$PWD" "$p" ;; esac
}

cursor_file_for() {
  printf '%s/%s.json' "$CURSOR_DIR" "$(printf '%s' "$1" | sha256sum | cut -d' ' -f1)"
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

CUR_PRESENT=0 CUR_OFFSET=0 CUR_DEVINODE="" CUR_FILE_ID="" CUR_HEAD_LEN="" CUR_HEAD_SHA="" CUR_TAIL_SHA=""
declare -A CURSOR_HOMES
read_cursor() {
  CUR_PRESENT=0 CUR_OFFSET=0 CUR_DEVINODE="" CUR_FILE_ID="" CUR_HEAD_LEN="" CUR_HEAD_SHA="" CUR_TAIL_SHA=""
  CURSOR_HOMES=()
  [[ -r "$1" ]] || return 0
  local line sid home
  line="$(jq -r '[(.offset // 0), (.dev_inode // ""), (.file_id // ""), (.head_len // ""), (.head_sha // ""), (.tail_sha // "")]
    | map(tostring) | join("\u001f")' "$1" 2>/dev/null)" \
    || return 0
  [[ -n "$line" ]] || return 0
  IFS=$'\x1f' read -r CUR_OFFSET CUR_DEVINODE CUR_FILE_ID CUR_HEAD_LEN CUR_HEAD_SHA CUR_TAIL_SHA <<<"$line"
  [[ "$CUR_OFFSET" =~ ^[0-9]+$ ]] || { CUR_OFFSET=0; return 0; }
  CUR_PRESENT=1
  # session_homes is a MAP, one entry per session id (review F1). A transcript
  # can carry a SECOND session id (a resume in place), and each session must
  # keep attributing to its OWN launch project — collapsing the map to one
  # value per FILE gave the second session the first one's project, which a
  # full pass never does. A cursor with no map — or one carrying the older
  # scalar field — reads as an empty map, so the chunk's own first cwd decides,
  # which is exactly the full-pass rule.
  while IFS=$'\t' read -r sid home; do
    [[ -n "$sid" ]] || continue
    CURSOR_HOMES[$sid]="$home"
  done < <(jq -r '(.session_homes // {}) | objects | to_entries[]
    | [.key, (.value | tostring)] | join("\t")' "$1" 2>/dev/null)
}

# Whether the cursor read_cursor loaded describes this file (rule in the
# header). CUR_STALE=1: it does, but must be rewritten in the current shape.
CUR_STALE=0
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

# The merged session-home map travels on STDIN as "<sid><TAB><home>" lines, so
# an unbounded number of sessions never becomes argv and jq stays the only
# JSON writer.
write_cursor() { # cursor-file abs-path dev:inode file-id offset  (map on stdin)
  local tmp want ts
  [[ "$5" =~ ^[0-9]+$ ]] || return 1
  want=$(( $5 < HEAD_MAX ? $5 : HEAD_MAX ))
  if [[ "$HEAD_LEN" != "$want" || -z "$HEAD_SHA" ]]; then
    HEAD_LEN="" HEAD_SHA=""
    HEAD_SHA="$(span_sha "$2" 0 "$want")" || return 1
    HEAD_LEN="$want"
  fi
  if [[ "$TAIL_AT" != "$5" || -z "$TAIL_SHA" ]]; then
    TAIL_AT="" TAIL_SHA=""
    ts=$(( $5 > HEAD_MAX ? $5 - HEAD_MAX : 0 ))
    TAIL_SHA="$(span_sha "$2" "$ts" "$(( $5 - ts ))")" || return 1
    TAIL_AT="$5"
  fi
  tmp="$(mktemp "$1.tmp.XXXXXX" 2>/dev/null)" || return 1
  if jq -Rn --arg p "$2" --arg f "$4" --argjson hl "$HEAD_LEN" --arg hs "$HEAD_SHA" --arg ts "$TAIL_SHA" \
      --arg d "$3" --argjson o "$5" --arg u "$(date -Is)" \
      '{path: $p, file_id: $f, head_len: $hl, head_sha: $hs, tail_sha: $ts, dev_inode: $d, offset: $o,
        session_homes: ([inputs | split("\t")
                         | select((.[0] // "") != "")
                         | {key: .[0], value: (.[1] // "")}] | from_entries),
        updated_at: $u}' >"$tmp" 2>/dev/null; then
    chmod 600 "$tmp" 2>/dev/null
    mv -f "$tmp" "$1" 2>/dev/null && return 0
  fi
  rm -f "$tmp" 2>/dev/null
  return 1
}

# What the cursor already knew, plus every session this pass saw with its own
# first cwd — printed for write_cursor's stdin.
merged_homes() {
  local sid
  # Plain key expansion: `${!arr[@]+...}` is INDIRECT expansion, not a guard,
  # and an empty associative array expands to nothing under set -u anyway.
  for sid in "${!CURSOR_HOMES[@]}"; do
    printf '%s\t%s\n' "$sid" "${CURSOR_HOMES[$sid]}"
  done
}

# A sub-agent file that was first read while its parent transcript was absent
# keeps the sub-agent's own cwd as home in its cursor. When the parent is
# present at a later pass, the parent's home wins and the cursor is rewritten
# even if the file has no new bytes (offset and inode untouched): this sets
# CUR_STALE for the caller's rewrite. Stat-only unless the parent transcript
# exists.
repair_sub_cursor_home() { # file
  local dir sid
  [[ "$1" == */subagents/* ]] || return 0
  dir="${1%%/subagents/*}"
  sid="${dir##*/}"
  [[ -n "${CURSOR_HOMES[$sid]+x}" ]] || return 0
  parent_home "$1" "$sid" || return 0
  [[ "${CURSOR_HOMES[$sid]}" != "$PARENT_HOME" ]] || return 0
  CURSOR_HOMES[$sid]="$PARENT_HOME"
  CUR_STALE=1
}

# Absolute offset of the end of the last COMPLETE line inside [start, size).
# Walks backwards in windows so one huge line never forces a full re-read;
# `head -n N | wc -c` is exact regardless of where the window starts, because N
# is that window's own newline count.
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

# One output row per input line that matters, fields joined by US (0x1f): a
# NON-whitespace separator, so an empty field (no cwd) never collapses under
# `read` and shifts the fields after it. Rows:
#   B   a line that is not a JSON object (skipped alone, counted)
#   N   an assistant text line with no uuid or sessionId (cannot be given an id)
#   D <uuid> <ts> <ts_inferred 0|1> <sid> <cwd> <truncated> <agent|-> <side 0|1> <text_offset> <b64 first> <b64 rest>
# A line without a timestamp takes the timestamp of the nearest PREVIOUS line
# of the range that has one (any line type); with none, ts is empty and the
# shell uses the file's mtime. The first slice is the first $max characters
# (the shell strips its trailing newlines); the rest starts where the stripped
# first slice actually ends, so first + rest is the source text, and
# text_offset is that character offset. Only when the first $max characters
# are ALL newlines (the old reader never emitted such a turn) are the leading
# newlines dropped; text_offset still counts them.
JQ_FILTER='
  def esc: gsub("\u001f"; " ") | [.] | @tsv;
  def nes: type == "string" and . != "";
  # An RFC 3339 string exactly as the queue writer accepts it (Go
  # time.Parse(RFC3339Nano): 4-digit year, T, optional fraction with . or ,
  # and any digit count, Z or +-hh:mm; month 1-12, real day of the month,
  # hour < 24, minute and second < 60, offset hour <= 24, offset minute <= 60).
  def rfc:
    if type != "string" then false
    else (capture("\\A(?<y>[0-9]{4})-(?<mo>[0-9]{2})-(?<d>[0-9]{2})T(?<h>[0-9]{2}):(?<mi>[0-9]{2}):(?<s>[0-9]{2})(?:[.,][0-9]+)?(?:Z|[+-](?<zh>[0-9]{2}):(?<zm>[0-9]{2}))\\z") // null) as $c
      | if $c == null then false
        else ($c.y | tonumber) as $y | ($c.mo | tonumber) as $mo | ($c.d | tonumber) as $d
          | (($y % 4 == 0) and (($y % 100 != 0) or ($y % 400 == 0))) as $leap
          | ([31, (if $leap then 29 else 28 end), 31, 30, 31, 30, 31, 31, 30, 31, 30, 31]) as $dim
          | ($mo >= 1 and $mo <= 12 and $d >= 1 and $d <= $dim[$mo - 1]
             and ($c.h | tonumber) < 24 and ($c.mi | tonumber) < 60 and ($c.s | tonumber) < 60
             and (($c.zh // "0") | tonumber) <= 24 and (($c.zm // "0") | tonumber) <= 60)
        end
    end;
  # Lone UTF-16 surrogate escapes (not part of a valid high+low pair) become
  # U+FFFD; an escaped backslash is consumed first so "\\ud800" is left alone.
  def repair:
    gsub("(?<bs>\\\\\\\\)|(?<pair>\\\\u[dD][89abAB][0-9a-fA-F]{2}\\\\u[dD][c-fC-F][0-9a-fA-F]{2})|(?<lone>\\\\u[dD][89a-fA-F][0-9a-fA-F]{2})";
         if .lone != null then "\\ufffd" elif .pair != null then .pair else .bs end);
  def row($l; $ts; $inf):
    ([$l.message.content[]? | select(.type == "text")
      | .text | if type == "string" then . elif . == null then "" else tojson end] | join("\n\n")) as $t0
    | if ($t0 | length) == 0 then []
      elif (($l.uuid | nes) and ($l.sessionId | nes)) | not then ["N"]
      else
        (if ($t0 | length) > $max and (($t0[0:$max] | explode) | all(. == 10))
         then ($t0 | explode) as $e | (first(range(0; $e | length) | select($e[.] != 10)) // ($e | length)) as $lead
              | {t: ($t0[$lead:]), lead: $lead}
         else {t: $t0, lead: 0} end) as $x
        | $x.t as $t
        | (($t | length) > $max) as $trunc
        | (if $trunc then
             ($t[0:$max] | explode) as $e
             | (first(range($max - 1; -1; -1) | select($e[.] != 10)) // -1) + 1
           else 0 end) as $k
        | [["D",
            ($l.uuid | esc), ($ts | esc), (if $inf then "1" else "0" end), ($l.sessionId | esc),
            (($l.cwd // "") | if type == "string" then . else "" end | esc),
            (if $trunc then "true" else "false" end),
            (if (($l.agentId // "") | type) == "string" and ($l.agentId // "") != "" then ($l.agentId | esc) else "-" end),
            (if $l.isSidechain == true then "1" else "0" end),
            (if $trunc then ($x.lead + $k | tostring) else "0" end),
            ($t[0:$max] | @base64),
            (if $trunc then ($t[$k:] | @base64) else "" end)] | join("\u001f")] end;
  foreach inputs as $raw ({ts: "", out: []};
    . as $st
    | if ($raw | test("^\\s*$")) then {ts: $st.ts, out: []}
      else (try {v: ($raw | fromjson), r: false} catch (try {v: ($raw | repair | fromjson), r: true} catch null)) as $p
        | if $p == null or ($p.v | type) != "object" then {ts: $st.ts, out: ["B"]}
          else $p.v as $l
            | (if ($l.timestamp | rfc) then $l.timestamp else "" end) as $own
            | {ts: (if $own != "" then $own else $st.ts end),
               out: ((if $p.r then ["R"] else [] end)
                     + (try (if $l.type == "assistant"
                             then row($l; (if $own != "" then $own else $st.ts end); $own == "")
                             else [] end) catch ["B"]))}
          end
      end;
    .out[])'

# Each input line becomes one JSON STRING (byte-level escaping, C locale), and
# jq reads those strings as ordinary JSON. Neither `jq -R` (it splits a
# multi-byte character at its 4096-byte read boundary: mojibake in long lines)
# nor a bare JSON stream (an unterminated line swallows the next valid line)
# is safe; this is both line-exact and character-exact.
WRAP_SED=(-e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/\t/\\t/g' -e 's/\r/\\r/g')
for _c in 0 1 2 3 4 5 6 7 8 11 12 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30 31; do
  WRAP_SED+=(-e "$(printf 's/\\x%02x/\\\\u%04x/g' "$_c" "$_c")")
done
WRAP_SED+=(-e 's/^/"/' -e 's/$/"/')
unset _c
wrap_lines() { LC_ALL=C sed "${WRAP_SED[@]}"; }

extract_tsv() { # file start end use-range
  if [[ "$4" == "1" ]]; then
    tail -c "+$(($2 + 1))" -- "$1" 2>/dev/null | head -c "$(($3 - $2))" \
      | wrap_lines | jq -rn --argjson max "$TEXT_MAX" "$JQ_FILTER" 2>/dev/null
  else
    wrap_lines <"$1" 2>/dev/null | jq -rn --argjson max "$TEXT_MAX" "$JQ_FILTER" 2>/dev/null
  fi
}

# Parent session home for a sub-agent file. The sub-agent's lines carry the
# parent's session id and may run in another cwd (a worktree), so the home is
# the PARENT transcript's: <projdir>/<sessionId>.jsonl next to the
# <projdir>/<sessionId>/subagents/ tree. Its cursor's session_homes wins, else
# its first line carrying a cwd. Returns 1 (PARENT_HOME empty) when the parent
# transcript is absent or has no cwd: the caller falls back to the sub-agent's
# own cwd.
# First cwd of a MAIN-session line (not a sidechain line) carrying this session
# id: a resumed transcript holds more than one session id, and only the
# session's own lines name its home. A malformed line is skipped, not fatal.
main_first_cwd() { # transcript session-id
  wrap_lines <"$1" 2>/dev/null | jq -rn --arg s "$2" 'first(inputs | fromjson? | select(type == "object"
      and .sessionId == $s and .isSidechain != true
      and (.cwd | type == "string") and .cwd != "") | .cwd) // empty' 2>/dev/null
}

PARENT_HOME=""
parent_home() { # sub-agent-file session-id
  local sub dir main cur home
  PARENT_HOME=""
  dir="${1%%/subagents/*}"
  main="$dir.jsonl"
  [[ -f "$main" ]] || return 1
  if [[ "$CURSORS_OK" == "1" ]]; then
    cur="$(cursor_file_for "$(abs_path "$main")")"
    if [[ -r "$cur" ]]; then
      home="$(jq -r --arg s "$2" '(.session_homes // {}) | objects | .[$s] // empty | tostring' "$cur" 2>/dev/null)" || home=""
      [[ -n "$home" ]] && { PARENT_HOME="$home"; return 0; }
    fi
  fi
  home="$(main_first_cwd "$main" "$2")" || home=""
  [[ -n "$home" ]] || return 1
  PARENT_HOME="$home"
}

# Readable sub-agent meta as a JSON object of the payload keys it supplies
# (agent_type, parent_tool_use_id, spawn_depth); {} when absent, unreadable or
# malformed. Cached per agent id.
AGENT_META="{}"
declare -A AGENT_META_CACHE
agent_meta() { # file session-id agent-id
  local f="$1" mf out ck="$2/$3"
  if [[ -n "${AGENT_META_CACHE[$ck]:-}" ]]; then AGENT_META="${AGENT_META_CACHE[$ck]}"; return 0; fi
  AGENT_META="{}"
  if [[ "$f" == */subagents/* ]]; then
    mf="${f%.jsonl}.meta.json"
  else
    # Inline sidechain line in a main transcript: the sibling tree, if any.
    mf="${f%.jsonl}/subagents/agent-$3.meta.json"
  fi
  if [[ -r "$mf" ]] && out="$(jq -c 'if type == "object" then
        {agent_type: (if (.agentType | type) == "string" and .agentType != "" then .agentType else null end),
         parent_tool_use_id: (if (.toolUseId | type) == "string" and .toolUseId != "" then .toolUseId else null end),
         spawn_depth: (if (.spawnDepth | type) == "number" then .spawnDepth else null end)}
        | with_entries(select(.value != null)) else {} end' "$mf" 2>/dev/null)" && [[ -n "$out" ]]; then
    AGENT_META="$out"
  fi
  AGENT_META_CACHE[$ck]="$AGENT_META"
}

# parent_home once per (file, session) rather than once per line.
declare -A PARENT_HOME_CACHE
parent_home_cached() { # sub-agent-file session-id ; sets PARENT_HOME, returns 1 when absent
  local ck="$1|$2"
  if [[ -z "${PARENT_HOME_CACHE[$ck]+x}" ]]; then
    if parent_home "$1" "$2"; then PARENT_HOME_CACHE[$ck]="$PARENT_HOME"; else PARENT_HOME_CACHE[$ck]=""; fi
  fi
  PARENT_HOME="${PARENT_HOME_CACHE[$ck]}"
  [[ -n "$PARENT_HOME" ]]
}

# Queue one event. Text rides argv (-text) unless a file path is given: a
# continuation can exceed the 128 KiB single-argument limit.
#   queue_event <project> <sid> <cwd> <etype> <eid> <ts> <payload> <text|-> <textfile|->
queue_event() {
  local tflag=(-text "$8")
  [[ "$9" != "-" ]] && tflag=(-text-file "$9")
  "$QUEUE_BIN" \
    -project "$1" \
    -session-id "$2" \
    -cwd "$3" \
    -event-type "$4" \
    -source-adapter claude_code_transcript_backfill \
    -source-event-id "$5" \
    -occurred-at "$6" \
    "${tflag[@]}" \
    -payload-json "$7" \
    ${rootargs[@]+"${rootargs[@]}"} >/dev/null
}

# Code points on stdin, whatever the locale (--dry-run reporting only): `${#var}`
# and `wc -m` count bytes under LANG=C, which CI runs with, and the dry-run
# shape is asserted in characters.
char_count() { jq -Rs 'length' 2>/dev/null || printf '0'; }

declare -A EMIT_COUNT
declare -A MAIN_HOME_CACHE
declare -A OWN_HOME
COUNT=0
CONT_COUNT=0
# Per-range outcome counters, read by the main loop.
WRITE_FAILED=0     # queue writes that failed (the file's cursor must not advance)
BAD_LINES=0        # lines that are not a JSON object, skipped alone
NOID_LINES=0       # assistant text lines with no uuid or sessionId
REPAIRED_LINES=0   # lines parsed only after replacing lone surrogate escapes
INFERRED_TS=0      # events whose occurred_at was inferred
mark_write_failed() { WRITE_FAILED=$((WRITE_FAILED + 1)); log "queue write failed: $*"; }

process_range() { # file start end use-range
  local file="$1" file_mtime=""
  local kind_row uuid ts inferred sid cwd truncated agent side toff text_b64 rest_b64 project text payload key
  local etype eid kind meta rpayload rest_len tf home
  local in_sub=0
  PARENT_HOME_CACHE=()
  MAIN_HOME_CACHE=()
  OWN_HOME=()
  [[ "$file" == */subagents/* ]] && in_sub=1
  COUNT=0
  CONT_COUNT=0
  WRITE_FAILED=0
  BAD_LINES=0
  NOID_LINES=0
  REPAIRED_LINES=0
  INFERRED_TS=0
  EMIT_COUNT=()
  while IFS=$'\x1f' read -r kind_row uuid ts inferred sid cwd truncated agent side toff text_b64 rest_b64; do
    case "$kind_row" in
      B) BAD_LINES=$((BAD_LINES + 1)); continue ;;
      N) NOID_LINES=$((NOID_LINES + 1)); continue ;;
      R) REPAIRED_LINES=$((REPAIRED_LINES + 1)); continue ;;
      D) ;;
      *) continue ;;
    esac
    [[ "$agent" == "-" ]] && agent=""
    [[ "$in_sub" == "1" ]] && side=1
    if [[ "$side" == "1" && -z "$agent" && "$in_sub" == "1" ]]; then
      agent="${file##*/}"; agent="${agent#agent-}"; agent="${agent%.jsonl}"
    fi
    kind=turn
    if [[ "$side" == "1" && ( "$agent" == acompact-* || "${file##*/}" == agent-acompact-* ) ]]; then
      kind=compact
    fi
    # A line without a timestamp: the range's previous timestamp (jq), else
    # the file's mtime. Marked in the payload either way.
    if [[ -z "$ts" ]]; then
      if [[ -z "$file_mtime" ]]; then
        file_mtime="$(date -u -d "@$(stat -c %Y -- "$file" 2>/dev/null || date +%s)" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null)" \
          || file_mtime="1970-01-01T00:00:00Z"
      fi
      ts="$file_mtime"
    fi
    [[ "$inferred" == "1" ]] && INFERRED_TS=$((INFERRED_TS + 1))
    # Per-SESSION home. Only MAIN-session lines and the parent lookup set it:
    # a sidechain line (in a sub-agent file or inline in a main transcript)
    # uses the home but never sets or changes it. Order: what this run already
    # knows, then the cursor's entry for THIS session id, then (main line) the
    # line's own cwd (the full-pass rule). Never another session's home.
    # A sub-agent file defers to its PARENT's home whenever the parent
    # transcript is present, even over its own cursor.
    if [[ "$in_sub" == "1" ]]; then
      if parent_home_cached "$file" "$sid"; then
        MRP_SESSION_HOME[$sid]="$PARENT_HOME"
      elif [[ -z "${MRP_SESSION_HOME[$sid]:-}" && -n "${CURSOR_HOMES[$sid]:-}" ]]; then
        MRP_SESSION_HOME[$sid]="${CURSOR_HOMES[$sid]}"
      fi
    elif [[ -z "${MRP_SESSION_HOME[$sid]:-}" ]]; then
      if [[ -n "${CURSOR_HOMES[$sid]:-}" ]]; then
        MRP_SESSION_HOME[$sid]="${CURSOR_HOMES[$sid]}"
      elif [[ "$side" != "1" && -n "$cwd" ]]; then
        MRP_SESSION_HOME[$sid]="$cwd"
      fi
    fi
    home="${MRP_SESSION_HOME[$sid]:-}"
    if [[ -z "$home" && "$side" == "1" ]]; then
      # No recorded home. Inline sidechain line: the file's first main-session
      # cwd for this session. Sub-agent file with no parent transcript: the
      # file's own first cwd, for this run only (never recorded).
      if [[ "$in_sub" == "0" ]]; then
        if [[ -z "${MAIN_HOME_CACHE[$sid]+x}" ]]; then MAIN_HOME_CACHE[$sid]="$(main_first_cwd "$file" "$sid")"; fi
        home="${MAIN_HOME_CACHE[$sid]}"
      fi
      if [[ -z "$home" && -n "$cwd" ]]; then
        [[ -n "${OWN_HOME[$sid]:-}" ]] || OWN_HOME[$sid]="$cwd"
        home="${OWN_HOME[$sid]}"
      fi
    fi
    # Persist only a home a main-session line or the parent lookup produced.
    [[ -z "${MRP_SESSION_HOME[$sid]:-}" ]] || CURSOR_HOMES[$sid]="${MRP_SESSION_HOME[$sid]}"
    # Kill-switch gate (even under MEMOIRE_PROJECT: override and toggle are
    # orthogonal, liaison:D239 item 6).
    memoire_capture_off_session "$sid" "${cwd:-}" "$home" && continue
    if [[ -n "${MEMOIRE_PROJECT:-}" ]]; then
      project="$MEMOIRE_PROJECT" MRP_ROOT="" MRP_SOURCE=""
    else
      memoire_resolve_v "${cwd:-unknown}" "$home"
      project="$MRP_KEY"
    fi
    rootargs=()
    [[ -n "$MRP_ROOT" ]] && rootargs=(-project-root "$MRP_ROOT" -project-source "$MRP_SOURCE")
    text="$(printf '%s' "$text_b64" | base64 -d 2>/dev/null)" || { mark_write_failed "$file $uuid (text decode)"; continue; }
    # Text that is only newlines carries no content (the old reader skipped it too).
    [[ -z "$text" ]] && continue
    [[ "$toff" =~ ^[0-9]+$ ]] || toff="$TEXT_MAX"
    if [[ "$side" != "1" ]]; then
      etype=claude_assistant_turn eid="cc-turn:$sid:$uuid"
      payload=$(jq -cn --arg f "$file" --arg u "$uuid" --arg g "$GENERATION" --argjson tr "$truncated" --argjson inf "$([[ "$inferred" == 1 ]] && echo true || echo false)" \
        '{transcript_file: $f, line_uuid: $u, backfill_generation: $g, text_truncated: $tr}
         + (if $inf then {occurred_at_inferred: true} else {} end)')
    else
      if [[ "$kind" == "compact" ]]; then
        etype=claude_compact_summary eid="cc-compact:$sid:$uuid" meta="{}"
      else
        etype=claude_assistant_turn eid="cc-turn:$sid:$uuid"
        agent_meta "$file" "$sid" "$agent"; meta="$AGENT_META"
      fi
      payload=$(jq -cn --arg f "$file" --arg u "$uuid" --arg g "$GENERATION" --argjson tr "$truncated" \
        --arg a "$agent" --argjson m "$meta" --argjson inf "$([[ "$inferred" == 1 ]] && echo true || echo false)" \
        '{transcript_file: $f, line_uuid: $u, backfill_generation: $g, text_truncated: $tr,
          sidechain: true, agent_id: $a} + $m
         + (if $inf then {occurred_at_inferred: true} else {} end)')
    fi
    if [[ -z "$payload" ]]; then mark_write_failed "$file $uuid (payload)"; continue; fi
    if [[ "$DRY_RUN" == "1" ]]; then
      printf 'DRY %s %s %s %s chars=%s\n' "$project" "$sid" "$ts" "$uuid" "$(printf '%s' "$text" | char_count)"
    else
      # A failed first-event write: the turn is not counted and the file's
      # cursor will not advance; the continuation is not attempted (the whole
      # turn replays at the next pass).
      queue_event "$project" "$sid" "${cwd:-}" "$etype" "$eid" "$ts" "$payload" "$text" - \
        || { mark_write_failed "$file $uuid"; continue; }
    fi
    COUNT=$((COUNT + 1))
    # A compaction summary is evidence, not a turn: it never notifies derive.
    if [[ "$kind" != "compact" ]]; then
      key="$project"$'\x1f'"$sid"
      EMIT_COUNT[$key]=$(( ${EMIT_COUNT[$key]:-0} + 1 ))
    fi
    # Continuation: everything after the first event's actual end, one event.
    # The text goes through a file, never argv, and is not decoded into a shell
    # variable (no NUL or trailing-newline loss on the tail).
    if [[ "$truncated" == "true" && -n "$rest_b64" ]]; then
      rpayload="$(jq -c --arg u "$uuid" --argjson o "$toff" \
        '. + {continues_line_uuid: $u, text_offset: $o, text_truncated: false}' <<<"$payload")" \
        || rpayload=""
      if [[ -z "$rpayload" ]]; then mark_write_failed "$file $uuid:rest (payload)"; continue; fi
      if [[ "$DRY_RUN" == "1" ]]; then
        rest_len="$(printf '%s' "$rest_b64" | base64 -d 2>/dev/null | char_count)"
        printf 'DRY %s %s %s %s chars=%s continuation id=%s text_offset=%s\n' \
          "$project" "$sid" "$ts" "$uuid" "${rest_len//[^0-9]/}" "$eid:rest" "$toff"
        CONT_COUNT=$((CONT_COUNT + 1))
      else
        [[ -n "$RUN_TMP" && -d "$RUN_TMP" ]] || RUN_TMP="$(mktemp -d "${TMPDIR:-/tmp}/memoire-cc-capture.XXXXXX" 2>/dev/null)"
        tf=""
        [[ -n "$RUN_TMP" && -d "$RUN_TMP" ]] && tf="$(mktemp "$RUN_TMP/text.XXXXXX" 2>/dev/null)"
        if [[ -n "$tf" ]] && printf '%s' "$rest_b64" | base64 -d >"$tf" 2>/dev/null; then
          if queue_event "$project" "$sid" "${cwd:-}" "$etype" "$eid:rest" "$ts" "$rpayload" - "$tf"; then
            CONT_COUNT=$((CONT_COUNT + 1))
          else
            mark_write_failed "$file $uuid:rest"
          fi
        else
          mark_write_failed "$file $uuid:rest (text file)"
        fi
        [[ -n "$tf" ]] && rm -f "$tf"
      fi
    fi
  done < <(extract_tsv "$file" "$2" "$3" "$4")
}

# One TSV row per (project, session) that gained turns in this file. Stdout is
# otherwise silent outside --dry-run, so a caller can parse it (the tick does).
emit_sessions() {
  [[ "$EMIT_SESSIONS" == "1" ]] || return 0
  [[ ${#EMIT_COUNT[@]} -gt 0 ]] || return 0
  local key
  {
    for key in "${!EMIT_COUNT[@]}"; do
      printf '%s\t%s\t%s\n' "${key%%$'\x1f'*}" "${key#*$'\x1f'}" "${EMIT_COUNT[$key]}"
    done
  } | LC_ALL=C sort
}

total_events=0
total_files=0
total_failed=0
total_bad=0
total_noid=0
total_repaired=0
total_inferred=0
for FILE in "${FILES[@]}"; do
  file_size="" file_dev="" file_ino=""
  read -r file_size file_dev file_ino < <(stat -c '%s %d %i' -- "$FILE" 2>/dev/null) || true
  [[ "$file_size" =~ ^[0-9]+$ ]] || file_size=""
  file_devinode=""
  [[ "$file_dev" =~ ^[0-9]+$ && "$file_ino" =~ ^[0-9]+$ ]] && file_devinode="$file_dev:$file_ino"
  file_id_for "$FILE" "$file_dev" "$file_ino"
  HEAD_LEN="" HEAD_SHA="" TAIL_AT="" TAIL_SHA="" CUR_STALE=0
  file_abs="$(abs_path "$FILE")"
  cursor_path=""
  locked=0
  use_range=0
  ranged=0
  range_start=0
  range_end=0
  CURSOR_HOMES=()

  if [[ "$TAIL" == "1" && "$DRY_RUN" == "1" ]]; then
    # --dry-run writes nothing: no cursor dir, no lock file. The cursor is only
    # READ (when it exists) so the dry pass shows what a real pass would do.
    if [[ "$CURSORS_OK" == "1" ]]; then
      cursor_path="$(cursor_file_for "$file_abs")"
      ranged=1
    fi
  elif [[ "$TAIL" == "1" ]] && cursor_dir_ready; then
    cursor_path="$(cursor_file_for "$file_abs")"
    # The lock is a SIBLING file: the cursor itself is replaced by mv, and a
    # lock held on the replaced inode would be invisible to the next opener.
    if touch "$cursor_path.lock" 2>/dev/null && exec 9>"$cursor_path.lock"; then
      if flock -n 9 2>/dev/null; then
        locked=1
        ranged=1
      else
        exec 9>&-
        log "$(basename "$FILE"): skipped=locked"
        continue
      fi
    else
      log "$(basename "$FILE"): cursor lock unavailable, full pass"
      cursor_path=""
    fi
  fi

  if [[ "$ranged" == "1" ]]; then
    read_cursor "$cursor_path"
    if [[ -z "$file_size" ]]; then
      # stat failed: degrade to a whole-file pass, hub dedup absorbs it.
      log "$(basename "$FILE"): stat failed, full pass"
    else
      use_range=1
      if [[ "$FROM_START" == "0" && "$CUR_PRESENT" == "1" ]] \
          && cursor_matches "$FILE" "$file_size" "$file_ino"; then
        range_start="$CUR_OFFSET"
      else
        # Not a resume (rewritten file, other content, cursorless): the pass
        # starts at 0 and therefore sees every session's own first line — the
        # recorded homes must not pre-empt it.
        [[ "$FROM_START" == "0" && "$CUR_PRESENT" == "1" ]] \
          && log "$(basename "$FILE"): cursor does not match the file, full pass"
        CURSOR_HOMES=() CUR_STALE=0
      fi
      if (( file_size == range_start )); then
        if [[ "$DRY_RUN" == "0" && "$CUR_PRESENT" == "1" ]]; then
          repair_sub_cursor_home "$FILE"
          [[ "$CUR_STALE" == "1" ]] && { merged_homes | write_cursor "$cursor_path" "$file_abs" \
            "$file_devinode" "$FILE_ID" "$CUR_OFFSET" || log "$(basename "$FILE"): cursor write failed"; }
        fi
        exec 9>&-
        continue
      fi
      range_end="$(last_line_end "$FILE" "$range_start" "$file_size")"
      if (( range_end <= range_start )); then
        # Only a partial line so far — leave it for the next pass.
        exec 9>&-
        continue
      fi
    fi
  fi

  process_range "$FILE" "$range_start" "$range_end" "$use_range"

  if [[ "$COUNT" -gt 0 || "$WRITE_FAILED" -gt 0 || "$BAD_LINES" -gt 0 || "$NOID_LINES" -gt 0 || "$REPAIRED_LINES" -gt 0 ]]; then
    msg="$COUNT assistant turns"
    [[ "$CONT_COUNT" -gt 0 ]] && msg="$msg (+$CONT_COUNT continuation events)"
    [[ "$BAD_LINES" -gt 0 ]] && msg="$msg, $BAD_LINES unparseable lines skipped"
    [[ "$NOID_LINES" -gt 0 ]] && msg="$msg, $NOID_LINES lines without uuid or sessionId skipped"
    [[ "$REPAIRED_LINES" -gt 0 ]] && msg="$msg, $REPAIRED_LINES lines repaired (lone surrogate)"
    [[ "$INFERRED_TS" -gt 0 ]] && msg="$msg, $INFERRED_TS timestamps inferred"
    [[ "$WRITE_FAILED" -gt 0 ]] && msg="$msg, $WRITE_FAILED queue writes FAILED (cursor left at the start of this pass; re-run this command, or wait for the next --tail pass if the tick still scans this file)"
    log "$(basename "$FILE"): $msg"
  fi
  if [[ "$COUNT" -gt 0 ]]; then
    total_files=$((total_files + 1))
    total_events=$((total_events + COUNT + CONT_COUNT))
    emit_sessions
  fi
  total_failed=$((total_failed + WRITE_FAILED))
  total_bad=$((total_bad + BAD_LINES))
  total_noid=$((total_noid + NOID_LINES))
  total_repaired=$((total_repaired + REPAIRED_LINES))
  total_inferred=$((total_inferred + INFERRED_TS))

  # Cursor advance. --dry-run never writes one. A full pass records the file's
  # consumed end so the next tick is a stat-only no-op; cursor I/O never fails
  # the pass.
  if [[ "$DRY_RUN" == "0" && "$WRITE_FAILED" -gt 0 ]]; then
    # A failed queue write: the next pass must re-read from where THIS pass
    # started. A tail resume started at the cursor, which stays; a pass that
    # started at 0 (--from-start, non-tail, cursorless) removes any cursor so
    # the next --tail pass starts at 0 too.
    if [[ "$range_start" == "0" && "$CURSORS_OK" == "1" ]]; then
      fail_cursor="$(cursor_file_for "$file_abs")"
      if [[ -e "$fail_cursor" ]]; then
        if [[ "$locked" == "1" ]] \
          || { touch "$fail_cursor.lock" 2>/dev/null && exec 9>"$fail_cursor.lock" && flock -n 9 2>/dev/null; }; then
          rm -f -- "$fail_cursor" 2>/dev/null
          [[ -e "$fail_cursor" ]] && log "$(basename "$FILE"): cursor reset failed"
        else
          log "$(basename "$FILE"): cursor reset skipped (locked)"
        fi
        [[ "$locked" == "1" ]] || exec 9>&-
      fi
    fi
  elif [[ "$DRY_RUN" == "0" && -n "$file_size" ]]; then
    if [[ "$locked" == "1" ]]; then
      new_offset="$range_end"
      [[ "$use_range" == "1" ]] || new_offset="$(last_line_end "$FILE" 0 "$file_size")"
      merged_homes | write_cursor "$cursor_path" "$file_abs" "$file_devinode" "$FILE_ID" "$new_offset" \
        || log "$(basename "$FILE"): cursor write failed"
    elif [[ "$TAIL" == "0" ]] && cursor_dir_ready; then
      cursor_path="$(cursor_file_for "$file_abs")"
      if touch "$cursor_path.lock" 2>/dev/null && exec 9>"$cursor_path.lock" \
        && flock -n 9 2>/dev/null; then
        merged_homes | write_cursor "$cursor_path" "$file_abs" "$file_devinode" "$FILE_ID" \
          "$(last_line_end "$FILE" 0 "$file_size")" \
          || log "$(basename "$FILE"): cursor write failed"
      fi
      exec 9>&-
    fi
  fi
  [[ "$locked" == "1" ]] && exec 9>&-
done
summary="done: $total_events events from $total_files files (dry_run=$DRY_RUN)"
[[ "$total_bad" -gt 0 ]] && summary="$summary, $total_bad unparseable lines skipped"
[[ "$total_noid" -gt 0 ]] && summary="$summary, $total_noid lines without uuid or sessionId skipped"
[[ "$total_repaired" -gt 0 ]] && summary="$summary, $total_repaired lines repaired (lone surrogate)"
[[ "$total_inferred" -gt 0 ]] && summary="$summary, $total_inferred timestamps inferred"
[[ "$total_failed" -gt 0 ]] && summary="$summary, $total_failed queue writes FAILED"
log "$summary"
# Exit 2: some queue writes failed (those files keep their cursors).
[[ "$total_failed" -eq 0 ]] || exit 2
