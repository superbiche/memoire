#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# liaison:D239 session-level kill switch: honored directly (not via the resolver) so
# it works even if the resolver script is missing or broken.
case "${MEMOIRE_CAPTURE:-}" in off|0|false) exit 0 ;; esac

# Ingest acpx worker sessions (Agent Client Protocol, driven through the `acpx`
# CLI) into Memoire's file queue so foreign (no-native-adapter) sessions have a
# queryable turn record: one boundary event per prompt, the turn result carrying
# the worker's final message, and — for agents without a native Memoire adapter —
# the intermediate assistant messages.
#
# acpx keeps one record (<record_id>.json) and a journal stream per session:
#   live:    ~/.acpx/sessions/<id>.json  + <id>.stream.ndjson (+ .stream.<N>.ndjson)
#   archive: <archive>/<id>.json         + <id>.stream.ndjson (+ segments/<id>/<sha256>.ndjson)
# The stream is a mix of journal meta lines (schema acpx.session.journal.v1:
# segment / turn_started / turn_result) and the raw ACP JSON-RPC traffic. JSON-RPC
# ids restart at every reconnect; the journal request_id is the stable turn key.
#
# Known limitation: the journal carries no per-turn timestamp, so every event of a
# record uses the record's created_at as occurred_at.
#
# Re-running over the same record + stream files is idempotent: source_event_id is
# deterministic per (session, request_id, kind) and a state file under
# $MEMOIRE_HOME/cursors skips unchanged records (sizes + mtimes + capture mode).
# The cursor records the mode actually used for each record ("full" or
# "boundaries"): a stored "full" covers a later "boundaries" run, a stored
# "boundaries" matches only a "boundaries" request, and a legacy entry without a
# mode is dropped at load (re-captured when next selected, then re-stamped).
# A record that still holds a turn deferred
# by the quiet window is not fully captured: the state file remembers that (a
# non-zero deferred count) so a later run over unchanged files re-emits the
# deferred turn once it is quiet. The cursor is persisted only after a successful
# queue write; a failed persistence exits non-zero (see exit codes) so nothing is
# marked captured that the queue did not take.
#
# Exit codes: 0 ok; 1 a record failed to read/parse/extract or a queue write
# failed; 2 bad CLI usage; 3 the exclusion file is missing or unreadable
# (fail-closed); 7 the cursor (state file) could not be persisted after a
# successful queue write — rerun to heal (idempotent, nothing is lost).

SCRIPT_PATH="${BASH_SOURCE[0]}"
SCRIPT_DIR="${SCRIPT_PATH%/*}"
if [[ "$SCRIPT_DIR" == "$SCRIPT_PATH" ]]; then
  SCRIPT_DIR="."
fi
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
QUEUE_BIN="${MEMOIRE_QUEUE_BIN:-$ROOT/bin/memoire-queue}"
RESOLVER="${MEMOIRE_RESOLVE_PROJECT:-$ROOT/scripts/memoire-resolve-project.sh}"
MEMOIRE_HOME_DIR="${MEMOIRE_HOME:-$HOME/.memoire}"
STATE_DIR="$MEMOIRE_HOME_DIR/cursors"
STATE_FILE="$STATE_DIR/acpx-capture.json"

SESSIONS_DIR="${ACPX_SESSIONS_DIR:-$HOME/.acpx/sessions}"
ARCHIVE_DIRS=()
SINCE="24 hours ago"
QUIET_FOR=3600
FULL=0
SCAN=0
DRY_RUN=0
FILES=()
AUTO_MODE=1  # full mode decided per-record from the agent unless --full/--boundaries-only

usage() {
  printf '%s\n' \
    "Usage: memoire-capture-acpx.sh [options] [record.json ...]" \
    "" \
    "Reads acpx session records and their journal streams (live ~/.acpx/sessions or" \
    "the synced archive layout) and queues one boundary event per prompt, the turn" \
    "result carrying the worker's final message, and — for agents without a native" \
    "Memoire adapter — the intermediate assistant messages." \
    "With --scan it ingests every record under the sessions dir (and each archive" \
    "dir) modified since a cutoff; with explicit record arguments it ingests those." \
    "Re-runs are idempotent (deterministic source_event_id + state file)." \
    "" \
    "Options:" \
    "  --scan               Scan the sessions dir (and each --archive-dir) instead of" \
    "                       taking record arguments." \
    "  --since SPEC         Cutoff for --scan, as a \`find -newermt\` spec." \
    "                       Default: \"24 hours ago\"." \
    "  --sessions-dir DIR   acpx sessions dir. Default: ~/.acpx/sessions" \
    "                       (ACPX_SESSIONS_DIR)." \
    "  --archive-dir DIR    Repeatable. A synced archive sessions dir." \
    "  --quiet-for SECONDS  An incomplete (no turn_result) turn emits nothing while" \
    "                       the record's newest stream file is younger than this." \
    "                       Default: 3600." \
    "  --full               Emit intermediate messages for every agent." \
    "  --boundaries-only    Emit no intermediate messages for any agent." \
    "  --dry-run            Print the batch lines to stdout, write nothing (and" \
    "                       never read or write the state file)." \
    "  -h, --help           Show this help." >&2
}

log() {
  printf 'memoire-capture-acpx: %s\n' "$*" >&2
}

# Resolve a project id via the shared paths->id resolver (liaison:D238), fail-open to
# basename. The record cwd is both the event dir and the session home, so it is the
# single attribution dir.
#   memoire_resolve_v <cwd>  (sets MRP_KEY/ROOT/SOURCE; globals, not command
#   substitution — root/source ride the queue write for identity-conflict
#   detection, liaison:D239 item 4, and a subshell would drop them)
MRP_KEY="" MRP_ROOT="" MRP_SOURCE=""
memoire_resolve_v() {
  local out
  MRP_KEY="" MRP_ROOT="" MRP_SOURCE=""
  if out="$("$RESOLVER" --verbose "$1" 2>/dev/null)" && [[ -n "$out" ]]; then
    IFS=$'\t' read -r MRP_KEY MRP_ROOT MRP_SOURCE <<<"$out"
  fi
  if [[ -z "$MRP_KEY" ]]; then
    local d="$1"
    MRP_KEY="${d##*/}"
    MRP_ROOT="" MRP_SOURCE=""
  fi
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --scan) SCAN=1; shift ;;
    --since) SINCE="${2:-}"; shift 2 ;;
    --sessions-dir) SESSIONS_DIR="${2:-}"; shift 2 ;;
    --archive-dir) ARCHIVE_DIRS+=("${2:-}"); shift 2 ;;
    --quiet-for) QUIET_FOR="${2:-3600}"; shift 2 ;;
    --full) FULL=1; AUTO_MODE=0; shift ;;
    --boundaries-only) AUTO_MODE=0; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) usage; log "unknown option: $1"; exit 2 ;;
    *) FILES+=("$1"); shift ;;
  esac
done

if ! command -v jq >/dev/null 2>&1; then
  log "jq is required"
  exit 1
fi

if ! command -v python3 >/dev/null 2>&1; then
  log "python3 is required"
  exit 1
fi

if [[ "$DRY_RUN" == "0" && ! -x "$QUEUE_BIN" ]]; then
  log "queue writer not found: $QUEUE_BIN; run make build"
  exit 1
fi

# Full mode: on for agents whose harness has no verified native capture lane
# (auto), forced by --full, off by --boundaries-only. In auto mode the decision
# is made per-record from the agent, DERIVED (harness parity, 2026-10-15): the
# record's agent_command basename -> the acpx registry agent whose command has
# that basename (~/.agents/acpx.json) -> the non-retired harness owning that
# agent (~/.agents/harnesses.json) -> that harness's `acpx` lane in
# registry/capture-lanes.json. Only `boundaries` turns full mode OFF. Anything
# unresolved — a missing or unreadable file, an unknown command, an old
# spelling — keeps full journal capture; kill switches and the exclusion file
# are untouched by this derivation. The former hand-kept NATIVE_ADAPTERS list
# is gone, not moved.
ACPX_REGISTRY="${MEMOIRE_ACPX_REGISTRY:-$HOME/.agents/acpx.json}"
HARNESS_INVENTORY="${MEMOIRE_HARNESS_INVENTORY:-$HOME/.agents/harnesses.json}"
CAPTURE_LANES="${MEMOIRE_CAPTURE_LANES:-$ROOT/registry/capture-lanes.json}"
declare -A BOUNDARY_AGENTS=()
if [[ -r "$ACPX_REGISTRY" && -r "$HARNESS_INVENTORY" && -r "$CAPTURE_LANES" ]]; then
  # Each row: <command basename> <enable flag or ->. Boundaries-only applies
  # only when the native lane that would capture the turns is ENABLED here:
  # a sweep lane whose flag is not on (MEMOIRE_CAPTURE_<X> in the tick env)
  # captures nothing, so the journal keeps full capture for that agent.
  while IFS=$'\t' read -r base flag; do
    [[ -n "$base" ]] || continue
    if [[ -n "$flag" && "$flag" != "-" ]]; then
      case "${!flag:-}" in on|1|true) ;; *) continue ;; esac
    fi
    BOUNDARY_AGENTS["$base"]=1
  done < <(jq -r --slurpfile inv "$HARNESS_INVENTORY" --slurpfile lanes "$CAPTURE_LANES" '
      .agents as $agents
      | [ $inv[0].harnesses | to_entries[] | select(.value.status != "retired")
          | .key as $h | (.value.acpx_agents // [])[] | {agent: ., harness: $h} ]
      | .[] | select(($lanes[0].harnesses[.harness].acpx // "") == "boundaries")
      | [ (($agents[.agent].command // empty) | split(" ")[0] | split("/")[-1]),
          ($lanes[0].harnesses[.harness].native.enable_env // "-") ]
      | select(.[0] != null and .[0] != "") | @tsv
    ' "$ACPX_REGISTRY" 2>/dev/null)
else
  log "capture lanes unresolved (missing acpx registry, harness inventory or capture-lanes): full journal capture for every agent"
fi
FULL_MODE=0
if [[ "$AUTO_MODE" == "0" && "$FULL" == "1" ]]; then
  FULL_MODE=1
fi

# --- exclusion file (fail-closed) ---------------------------------------------
EXCLUDE_FILE="${MEMOIRE_ACPX_EXCLUDE_FILE:-${XDG_CONFIG_HOME:-$HOME/.config}/memoire/acpx-exclude}"
if [[ ! -f "$EXCLUDE_FILE" ]]; then
  log "exclusion file missing (fail-closed): $EXCLUDE_FILE"
  exit 3
fi
if [[ ! -r "$EXCLUDE_FILE" ]]; then
  log "exclusion file unreadable (fail-closed): $EXCLUDE_FILE"
  exit 3
fi

EXCLUDE_PREFIXES=()
while IFS= read -r line || [[ -n "$line" ]]; do
  line="${line#"${line%%[![:space:]]*}"}"
  line="${line%"${line##*[![:space:]]}"}"
  [[ -z "$line" ]] && continue
  [[ "$line" == \#* ]] && continue
  if [[ "$line" == "~" ]]; then
    line="$HOME"
  elif [[ "$line" == "~/"* ]]; then
    line="$HOME/${line#"~/"}"
  fi
  # Canonicalize with the same realpath semantics as record cwds: a trailing
  # slash and a symlink to the excluded root both collapse to the real root. A
  # path that does not resolve (a missing directory) is kept as its normalized
  # lexical form and never dropped, so it still excludes a record whose cwd
  # lies under it.
  resolved="$(realpath -m -- "$line" 2>/dev/null)"
  [[ -n "$resolved" ]] || resolved="$line"
  if [[ "$resolved" != "/" ]]; then
    resolved="${resolved%/}"
  fi
  EXCLUDE_PREFIXES+=("$resolved")
done < "$EXCLUDE_FILE"

is_excluded() {
  local real_cwd="$1" prefix
  for prefix in ${EXCLUDE_PREFIXES[@]+"${EXCLUDE_PREFIXES[@]}"}; do
    [[ -z "$prefix" ]] && continue
    # Component-boundary match: the cwd equals the prefix, or it lies under
    # prefix/ (the trailing slash keeps a sibling whose name only extends the
    # prefix from matching). Quoted so the prefix is matched literally, not as
    # a glob.
    if [[ "$real_cwd" == "$prefix" || "${real_cwd#"$prefix"/}" != "$real_cwd" ]]; then
      return 0
    fi
  done
  return 1
}

# --- record selection ---------------------------------------------------------
declare -A REC_SOURCE=() REC_DIR=() REC_PATH=()
add_record() { # path source dir
  local path="$1" source="$2" dir="$3" id base existing
  base="${path##*/}"
  id="${base%.json}"
  [[ -n "$id" ]] || return 0
  existing="${REC_SOURCE[$id]:-}"
  # Live is collected first; an archive never overrides an existing record
  # (a live copy, or an earlier archive copy — the first archive wins).
  if [[ -n "$existing" && "$source" == "archive" ]]; then
    return 0
  fi
  REC_SOURCE[$id]="$source"
  REC_DIR[$id]="$dir"
  REC_PATH[$id]="$path"
}

collect_records() { # dir source
  local dir="$1" source="$2" f
  [[ -d "$dir" ]] || return 0
  while IFS= read -r f; do
    [[ -n "$f" ]] || continue
    add_record "$f" "$source" "$dir"
  done < <(find "$dir" -maxdepth 1 -type f -name '*.json' \
      ${SINCE:+-newermt "$SINCE"} 2>/dev/null | LC_ALL=C sort)
}

if [[ "$SCAN" == "1" ]]; then
  if [[ ${#FILES[@]} -gt 0 ]]; then
    log "--scan and explicit record arguments are mutually exclusive"
    exit 2
  fi
  collect_records "$SESSIONS_DIR" live
  for d in ${ARCHIVE_DIRS[@]+"${ARCHIVE_DIRS[@]}"}; do
    collect_records "$d" archive
  done
else
  for f in ${FILES[@]+"${FILES[@]}"}; do
    [[ -n "$f" ]] || continue
    dir="${f%/*}"
    [[ "$dir" == "$f" ]] && dir="."
    source="live"
    for d in ${ARCHIVE_DIRS[@]+"${ARCHIVE_DIRS[@]}"}; do
      if [[ "$(cd "$d" 2>/dev/null && pwd)" == "$(cd "$dir" 2>/dev/null && pwd)" ]]; then
        source="archive"
        break
      fi
    done
    add_record "$f" "$source" "$dir"
  done
fi

if [[ ${#REC_SOURCE[@]} -eq 0 ]]; then
  log "no acpx records to ingest"
  exit 0
fi

# --- stream files (invented once per directory) -------------------------------
# A directory-wide find per record is quadratic in a large single-directory
# archive. Instead, inventory each record directory's stream files once and look
# them up per record. Only the selected copy of a record contributes its streams:
# a stream file found in a directory that is not that id's selected source (an
# archive copy of a live record, or a second archive) is ignored, so one copy's
# streams are never joined with another's. The per-file ordering (by the first
# line's segment sequence) is done in the extraction step, so no per-file process
# is spawned here.
declare -A INVENTORY=()
inventory_dir() { # dir source
  local dir="$1" src="$2" f id rel segid
  if [[ "$src" == "archive" ]]; then
    # Top-level <id>.stream.ndjson ...
    while IFS= read -r f; do
      [[ -n "$f" ]] || continue
      id="${f##*/}"; id="${id%%.stream*}"
      [[ "${REC_SOURCE[$id]:-}" == "$src" && "${REC_DIR[$id]:-}" == "$dir" ]] || continue
      INVENTORY[$id]+="$f"$'\n'
    done < <(find "$dir" -maxdepth 1 -type f -name '*.stream.ndjson' 2>/dev/null)
    # ... plus rotated segments/<id>/<sha256>.ndjson.
    while IFS= read -r f; do
      [[ -n "$f" ]] || continue
      rel="${f#"$dir"/}"; segid="${rel#segments/}"; id="${segid%%/*}"
      [[ "${REC_SOURCE[$id]:-}" == "$src" && "${REC_DIR[$id]:-}" == "$dir" ]] || continue
      INVENTORY[$id]+="$f"$'\n'
    done < <(find "$dir/segments" -mindepth 2 -maxdepth 2 -type f -name '*.ndjson' 2>/dev/null)
  else
    # Live: <id>.stream.ndjson plus rotated <id>.stream.<N>.ndjson.
    while IFS= read -r f; do
      [[ -n "$f" ]] || continue
      id="${f##*/}"; id="${id%%.stream*}"
      [[ "${REC_SOURCE[$id]:-}" == "$src" && "${REC_DIR[$id]:-}" == "$dir" ]] || continue
      INVENTORY[$id]+="$f"$'\n'
    done < <(find "$dir" -maxdepth 1 -type f -name '*.stream*.ndjson' 2>/dev/null)
  fi
}

declare -A DIR_SEEN=()
for id in "${!REC_SOURCE[@]}"; do
  d="${REC_DIR[$id]}"
  s="${REC_SOURCE[$id]}"
  key="$d"$'\x1f'"$s"
  [[ -n "${DIR_SEEN[$key]:-}" ]] && continue
  DIR_SEEN[$key]=1
  inventory_dir "$d" "$s"
done

# --- state (skip unchanged records) -------------------------------------------
# The cursor is kept in memory for the whole run (associative arrays) instead of
# being reparsed and rewritten per record, and is written to the state file once,
# atomically, after the queue writes. Each record stores the capture mode it was
# last captured with ("full" or "boundaries"): a stored "full" covers a later
# "boundaries" request, a stored "boundaries" matches only "boundaries", and a
# legacy entry without a mode is dropped at load (re-captured when next selected).
# A record that still holds turns
# deferred by the quiet window is not fully captured: it stores a non-zero
# deferred count, which keeps it from being skipped on a later run over unchanged
# files.
declare -A ST_SOURCE=() ST_FILES=() ST_DEFERRED=() ST_MODE=()
STATE_DIRTY=0

state_matches() { # id source fingerprint requested_mode
  local id="$1" source="$2" fp="$3" req="$4"
  [[ "${ST_SOURCE[$id]:-}" == "$source" ]] || return 1
  [[ "${ST_FILES[$id]:-}" == "$fp" ]] || return 1
  [[ "${ST_DEFERRED[$id]:-}" == "0" ]] || return 1
  # The mode joins the cursor: a stored "full" covers any request, a stored
  # "boundaries" matches only a "boundaries" request, and an entry without a mode
  # (written before the mode joined the cursor) matches nothing.
  local stored="${ST_MODE[$id]:-}"
  if [[ "$stored" == "full" ]]; then
    return 0
  elif [[ "$stored" == "boundaries" && "$req" == "boundaries" ]]; then
    return 0
  fi
  return 1
}

update_state_mem() { # id source fingerprint deferred mode
  ST_SOURCE[$1]="$2"
  ST_FILES[$1]="$3"
  ST_DEFERRED[$1]="$4"
  ST_MODE[$1]="$5"
  STATE_DIRTY=1
}

# Fingerprint of the record + its stream files: {relpath: [size, mtime]}.
build_fingerprint() { # dir path streamfiles...
  local source_dir="$1"; shift
  local f rel size mtime lines=""
  for f in "$@"; do
    rel="${f#"$source_dir"/}"
    size="$(stat -c %s -- "$f" 2>/dev/null)" || continue
    mtime="$(stat -c %Y -- "$f" 2>/dev/null)" || continue
    lines+="$rel"$'\t'"$size"$'\t'"$mtime"$'\n'
  done
  printf '%s' "$lines" | jq -cRn '[inputs | select(length>0) | split("\t") as $p
    | {key: $p[0], value: [$p[1]|tonumber, $p[2]|tonumber]}] | from_entries'
}

load_state() {
  [[ -f "$STATE_FILE" ]] || return 0
  # Only cache entries whose mode is exactly "full" or "boundaries" are loaded. A
  # legacy entry without a mode is dropped: it matches nothing anyway, so treating
  # it as absent keeps recapture semantics (it is re-captured when next selected and
  # disappears from the cursor at the next successful flush). Dropping it is also
  # load-bearing for the round-trip: every field of a loaded entry is non-empty, so
  # `IFS=$'\t' read` cannot collapse a column and misalign the rest (an empty field
  # would send the fingerprint into `mode` and leave `files_json` empty, which breaks
  # the flush's fromjson and exits 7).
  local id source deferred mode files_json
  while IFS=$'\t' read -r id source deferred mode files_json; do
    [[ -n "$id" && -n "$source" && -n "$mode" && -n "$files_json" ]] || continue
    ST_SOURCE[$id]="$source"
    ST_DEFERRED[$id]="${deferred:-0}"
    ST_MODE[$id]="$mode"
    ST_FILES[$id]="$files_json"
  done < <(jq -r 'to_entries[]
    | select(.value.mode == "full" or .value.mode == "boundaries")
    | [.key, (.value.source // "live"), ((.value.deferred // 0) | tostring),
       .value.mode, (.value.files // {} | tojson)] | @tsv' \
    "$STATE_FILE" 2>/dev/null)
}

flush_state() {
  [[ "$STATE_DIRTY" == "1" ]] || return 0
  mkdir -p "$STATE_DIR" 2>/dev/null || { log "state write failed (cannot create cursor dir)"; return 1; }
  local tmp id
  tmp="$(mktemp "${STATE_FILE}.XXXXXX" 2>/dev/null)" || { log "state write failed (cannot create temp cursor file)"; return 1; }
  if {
    for id in "${!ST_SOURCE[@]}"; do
      printf '%s\t%s\t%s\t%s\t%s\n' "$id" "${ST_SOURCE[$id]}" "${ST_DEFERRED[$id]}" "${ST_MODE[$id]}" "${ST_FILES[$id]}"
    done
  } | jq -Rn '[inputs | select(length>0) | split("\t") as $p
    | {key: $p[0], value: {source: $p[1], deferred: ($p[2] | tonumber),
        mode: $p[3], files: ($p[4] | fromjson)}}] | from_entries' > "$tmp" 2>/dev/null; then
    chmod 600 "$tmp" 2>/dev/null
    if mv -f "$tmp" "$STATE_FILE" 2>/dev/null; then
      return 0
    fi
  fi
  rm -f "$tmp" 2>/dev/null
  log "state write failed"
  return 1
}

if [[ "$DRY_RUN" != "1" ]]; then
  load_state
  cleanup_state() { flush_state; }
  trap cleanup_state EXIT
fi

# Extract the batch lines for one record. The record JSON is argv[1]; the stream
# files are argv[2:] (unordered; ordered here by their first line's segment
# sequence). A python3 block does the stream ordering, the turn state machine
# (keyed by the journal request_id), the assistant-message grouping (a new message
# starts when messageId changes, or — for agents whose chunks carry no messageId —
# after an intervening tool_call/tool_call_update), and the 6000-code-point text
# split. It also reports how many turns are deferred (incomplete while the record
# is fresh) so the cursor does not mark such a record as fully captured. jq cannot
# do the message-grouping state machine cleanly, hence python.
extract_events() { # record_file streamfiles...
  local record_file="$1"; shift
  ACPX_MODE="$FULL_MODE_ACPX" ACPX_QUIET="$QUIET_ACPX" ACPX_SOURCE="$SOURCE_ACPX" \
    ACPX_PROJECT="$PROJECT_ACPX" ACPX_PROJECT_ROOT="$MRP_ROOT" \
    ACPX_PROJECT_SOURCE="$MRP_SOURCE" ACPX_DEFERRED_FILE="${ACPX_DEFERRED_FILE:-}" \
    python3 -I - "$record_file" "$@" <<'PYEOF'
import sys, os, json, hashlib

MAX = 6000
SCHEMA = "acpx.session.journal.v1"


def content_text(content):
    if isinstance(content, dict):
        return content.get("text", "") if content.get("type") == "text" else ""
    if isinstance(content, list):
        return "".join(content_text(c) for c in content)
    return ""


def first_sequence(path):
    # The sequence of the file's first line when it is a segment meta line, else
    # None (the file then orders last).
    try:
        with open(path, "r", encoding="utf-8") as f:
            first = f.readline()
    except OSError:
        return None
    first = first.strip()
    if not first:
        return None
    try:
        ln = json.loads(first)
    except ValueError:
        return None
    if isinstance(ln, dict) and ln.get("schema") == SCHEMA and ln.get("type") == "segment":
        seq = ln.get("sequence")
        if isinstance(seq, int) and not isinstance(seq, bool):
            return seq
    return None


def stream_lines(paths):
    # Yield the decoded lines of the ordered stream files, one at a time (no
    # all-lines list).
    for p in paths:
        with open(p, "r", encoding="utf-8") as f:
            for ln in f:
                ln = ln.strip()
                if ln:
                    yield ln


def main():
    rec = json.load(open(sys.argv[1]))
    streams = sys.argv[2:]
    mode = os.environ["ACPX_MODE"]  # "1" = full, "0" = boundaries
    quiet = os.environ["ACPX_QUIET"] == "1"
    source = os.environ["ACPX_SOURCE"]
    project = os.environ["ACPX_PROJECT"]
    proot = os.environ.get("ACPX_PROJECT_ROOT", "")
    psrc = os.environ.get("ACPX_PROJECT_SOURCE", "")
    deferred_file = os.environ.get("ACPX_DEFERRED_FILE", "")

    sid = rec.get("acp_session_id", "")
    rid = rec.get("acpx_record_id", "")
    cwd = rec.get("cwd", "")
    created = rec.get("created_at", "")
    name = rec.get("name", "")
    cmd = rec.get("agent_command", "")
    first_word = cmd.split()[0] if cmd.strip() else ""
    agent = first_word.rsplit("/", 1)[-1] if first_word else ""

    # Order the stream files by the sequence of their first line when that line
    # is a segment meta line; a file without one goes last (then by name). The
    # archive layout names rotated files by content hash, so order never comes
    # from the file name.
    seq = {p: first_sequence(p) for p in streams}
    ordered = sorted(streams, key=lambda p: (seq[p] if seq[p] is not None
                                             else 999999999, p))

    turns = []
    cur = None

    def close_cur():
        nonlocal cur
        if cur is None:
            return
        if cur["_open"] and cur["_text"]:
            cur["messages"].append(cur["_text"])
        cur["_open"] = False
        turns.append(cur)
        cur = None

    for raw in stream_lines(ordered):
        ln = json.loads(raw)
        if ln.get("schema") == SCHEMA:
            t = ln.get("type")
            if t == "turn_started":
                close_cur()
                cur = {"request_id": ln.get("request_id"), "prompt": None,
                       "messages": [], "status": None, "stop": None, "model": None,
                       "complete": False, "_open": False, "_text": "", "_mid": None,
                       "_tool": False}
            elif t == "turn_result":
                if cur is not None and cur["request_id"] == ln.get("request_id"):
                    close_cur()
                    res = ln.get("result", {}) or {}
                    meta = res.get("_meta", {}) or {}
                    usage = (meta.get("quota", {}) or {}).get("model_usage") or [{}]
                    turns[-1]["status"] = res.get("status")
                    turns[-1]["stop"] = res.get("stopReason")
                    turns[-1]["model"] = (usage[0] or {}).get("model")
                    turns[-1]["complete"] = True
            # type == "segment": used only for file ordering, ignored here
        else:
            if cur is None:
                continue  # outside a turn (e.g. session/load history replay)
            m = ln.get("method")
            params = ln.get("params")
            if m == "session/prompt" and params:
                if cur["prompt"] is None:
                    pr = params.get("prompt", []) or []
                    cur["prompt"] = "".join(
                        p.get("text", "") for p in pr
                        if isinstance(p, dict) and p.get("type") == "text")
            elif m == "session/update" and params:
                upd = params.get("update", {}) or {}
                su = upd.get("sessionUpdate")
                if su == "agent_message_chunk":
                    txt = content_text(upd.get("content"))
                    mid = upd.get("messageId")
                    if not cur["_open"]:
                        # First chunk of the turn: open the first message.
                        cur["_text"] = txt
                        cur["_mid"] = mid
                        cur["_open"] = True
                        cur["_tool"] = False
                    else:
                        newmsg = False
                        if mid is not None and mid != cur["_mid"]:
                            newmsg = True
                        elif mid is None and cur["_tool"]:
                            newmsg = True
                        if newmsg:
                            if cur["_text"]:
                                cur["messages"].append(cur["_text"])
                            cur["_text"] = ""
                            cur["_mid"] = mid
                        cur["_text"] += txt
                        cur["_tool"] = False
                elif su in ("tool_call", "tool_call_update"):
                    cur["_tool"] = True
                # agent_thought_chunk and other updates: never emitted
    close_cur()

    # A turn that is incomplete (no turn_result) is deferred while the record is
    # fresh (not quiet): it is not fully captured in that case. Report the count
    # so the cursor keeps the record from being skipped on a later run.
    deferred = 0 if quiet else sum(1 for t in turns if not t["complete"])
    if deferred_file:
        with open(deferred_file, "w") as df:
            df.write(str(deferred))

    def split_e(eid, etype, text, payload):
        if len(text) <= MAX:
            return [(eid, etype, text, payload)]
        head = dict(payload, text_truncated=True)
        rest = dict(payload, continues_event_id=eid, text_offset=MAX)
        return [(eid, etype, text[:MAX], head),
                (eid + ":rest", etype, text[MAX:], rest)]

    out = []
    cbase = {"acpx_record_id": rid, "source": source, "agent": agent, "name": name}
    for ordinal, t in enumerate(turns, start=1):
        complete = t["complete"]
        if not complete and not quiet:
            continue
        base = "acpx:%s:%s" % (sid, t["request_id"])
        common = dict(cbase, request_id=t["request_id"], turn_ordinal=ordinal)
        prompt = t["prompt"] or ""
        psha = hashlib.sha256(prompt.encode("utf-8")).hexdigest()
        out += split_e(base + ":prompt", "acpx_prompt", prompt,
                       dict(common, prompt_sha256=psha))
        if mode == "1":
            msgs = t["messages"] if not complete else t["messages"][:-1]
            for n, msg in enumerate(msgs, start=1):
                out += split_e("%s:msg:%d" % (base, n), "acpx_assistant_message",
                               msg, dict(common, message_ordinal=n))
        if complete:
            final = t["messages"][-1] if t["messages"] else ""
            out += split_e(base + ":result", "acpx_turn_result", final,
                           dict(common, prompt_sha256=psha, status=t["status"],
                                stop_reason=t["stop"], model=t["model"],
                                message_count=len(t["messages"])))

    for eid, etype, text, payload in out:
        line = {"project": project, "session_id": sid,
                "source_adapter": "acpx-journal", "event_type": etype,
                "source_event_id": eid, "cwd": cwd, "occurred_at": created,
                "text": text, "payload": payload}
        if proot:
            line["project_root"] = proot
        if psrc:
            line["project_source"] = psrc
        print(json.dumps(line, ensure_ascii=False))


main()
PYEOF
}

# --- main loop ----------------------------------------------------------------
REC_IDS=()
if [[ ${#REC_SOURCE[@]} -gt 0 ]]; then
  mapfile -t REC_IDS < <(printf '%s\n' "${!REC_SOURCE[@]}" | LC_ALL=C sort)
fi

total_records=0
skipped=0
failed_records=0
failed_events=0

for id in "${REC_IDS[@]}"; do
  path="${REC_PATH[$id]}"
  source="${REC_SOURCE[$id]}"
  dir="${REC_DIR[$id]}"

  if ! record_json="$(cat -- "$path" 2>/dev/null)" || [[ -z "$record_json" ]]; then
    log "skip (unreadable record): $id"
    failed_records=$((failed_records + 1))
    continue
  fi
  if ! jq -c 'empty' <<<"$record_json" >/dev/null 2>&1; then
    log "skip (unparsable record): $id"
    failed_records=$((failed_records + 1))
    continue
  fi

  cwd="$(jq -r '.cwd // empty' <<<"$record_json")"

  # Exclusion is fail-closed and must never open the record's stream files.
  if [[ -n "$cwd" ]]; then
    real_cwd="$(realpath -m -- "$cwd" 2>/dev/null)" || real_cwd="$cwd"
    if is_excluded "$real_cwd"; then
      log "skip (excluded): $id"
      skipped=$((skipped + 1))
      continue
    fi
  fi

  # Session-level / repo-level capture kill switch (as the siblings do).
  if [[ -n "$cwd" && "$("$RESOLVER" --capture-enabled "$cwd" 2>/dev/null)" == "off" ]]; then
    log "skip (capture off): $id"
    skipped=$((skipped + 1))
    continue
  fi

  memoire_resolve_v "$cwd"
  project="$MRP_KEY"
  [[ -n "$project" ]] || project="acpx-unknown"
  agent_cmd="$(jq -r '.agent_command // empty' <<<"$record_json")"
  agent_base="${agent_cmd%%[[:space:]]*}"
  agent_base="${agent_base##*/}"
  if [[ "$AUTO_MODE" == "1" ]]; then
    is_native="${BOUNDARY_AGENTS[$agent_base]:-0}"
    FULL_MODE_ACPX="$((1 - is_native))"
  else
    FULL_MODE_ACPX="$FULL_MODE"
  fi
  mode_acpx="boundaries"
  if [[ "$FULL_MODE_ACPX" == "1" ]]; then mode_acpx="full"; fi

  inv="${INVENTORY[$id]:-}"
  if [[ -z "$inv" ]]; then
    log "skip (no stream): $id"
    skipped=$((skipped + 1))
    continue
  fi
  mapfile -t STREAM_FILES < <(printf '%s' "$inv" | LC_ALL=C sort)

  # Quiet window: open turns emit only once the record's newest stream file is old.
  now="$(date +%s)"
  newest=0
  for f in "${STREAM_FILES[@]}"; do
    mt="$(stat -c %Y -- "$f" 2>/dev/null)" || mt=0
    if (( mt > newest )); then newest="$mt"; fi
  done
  if (( now - newest >= QUIET_FOR )); then QUIET_ACPX=1; else QUIET_ACPX=0; fi
  SOURCE_ACPX="$source"
  PROJECT_ACPX="$project"

  # State: skip unchanged, fully-captured records (non-dry-run only).
  fp=""
  if [[ "$DRY_RUN" != "1" ]]; then
    fp="$(build_fingerprint "$dir" "$path" "${STREAM_FILES[@]}")"
    if state_matches "$id" "$source" "$fp" "$mode_acpx"; then
      log "skip (unchanged): $id"
      skipped=$((skipped + 1))
      continue
    fi
  fi

  perr="$(mktemp "${TMPDIR:-/tmp}/acpx-perr.XXXXXX")"
  pdef="$(mktemp "${TMPDIR:-/tmp}/acpx-pdef.XXXXXX")"
  ACPX_DEFERRED_FILE="$pdef"
  pout="$(extract_events "$path" "${STREAM_FILES[@]}" 2>"$perr")"
  rc=$?
  rm -f "$perr" 2>/dev/null
  deferred="$(cat "$pdef" 2>/dev/null)"
  rm -f "$pdef" 2>/dev/null
  [[ "$deferred" =~ ^[0-9]+$ ]] || deferred=0
  if [[ $rc -ne 0 ]]; then
    log "skip (extraction failed): $id"
    failed_records=$((failed_records + 1))
    continue
  fi

  if [[ -z "$pout" ]]; then
    log "skip (no journal meta): $id"
    skipped=$((skipped + 1))
    continue
  fi

  if [[ "$DRY_RUN" == "1" ]]; then
    printf '%s\n' "$pout"
    total_records=$((total_records + 1))
    continue
  fi

  batch_tmp="$(mktemp "${TMPDIR:-/tmp}/acpx-batch.XXXXXX")"
  printf '%s\n' "$pout" > "$batch_tmp"
  qerr="$(mktemp "${TMPDIR:-/tmp}/acpx-qerr.XXXXXX")"
  if "$QUEUE_BIN" -batch-file "$batch_tmp" >/dev/null 2>"$qerr"; then
    update_state_mem "$id" "$source" "$fp" "$deferred" "$mode_acpx"
    total_records=$((total_records + 1))
  else
    log "queue write failed for record $id"
    failed_events=$((failed_events + 1))
  fi
  rm -f "$batch_tmp" "$qerr" 2>/dev/null
done

log "done: records=$total_records skipped=$skipped failed_records=$failed_records failed_events=$failed_events"

# The cursor must be persisted after the (successful) queue writes. This is the
# checked completion path: a failed persistence is a reporting failure, not a
# capture failure — log it and exit non-zero (7) so the operator reruns. The run
# is idempotent, so nothing is lost and no cursor advances past a successful
# write. The EXIT trap still handles early-exit cleanup (best-effort flush).
if [[ "$DRY_RUN" != "1" ]]; then
  if ! flush_state; then
    log "state persistence failed; rerun to heal (exit 7)"
    STATE_DIRTY=0   # handled here; the EXIT trap's flush is now a no-op
    exit 7
  fi
  STATE_DIRTY=0     # persisted; the EXIT trap's flush is now a no-op
fi

if [[ $failed_records -gt 0 || $failed_events -gt 0 ]]; then
  exit 1
fi
exit 0
