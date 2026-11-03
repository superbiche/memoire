#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -uo pipefail

# memoire-capture-muse.sh — native Muse Code store-sweep capture adapter.
#
# Muse Code (a coding-agent CLI) keeps its sessions in a local append-only
# store. This adapter walks the store and queues EVERY logged record as one
# Memoire event — full prompts, assistant text, reasoning (encrypted_content
# kept whole), every tool call with input and result, model completions,
# terminals, subagent control, retained frames, unparseable lines — plus a
# turn-level projection (muse_assistant_turn) and per-session facts
# (muse_session). Capture-all: no truncation, no sampling. Redaction stays at
# the queue-write gate.
#
# Source and reader
# -----------------
# Store root: MEMOIRE_MUSE_SESSIONS_DIR (default ~/.local/share/muse/sessions),
# --sessions-dir DIR. Session files: <root>/YYYY/MM/DD/<sid>/session.jsonl
# (root sessions) and <root>/YYYY/MM/DD/<sid>/subagent/<cid>/session.jsonl
# (children). Nothing else in the store is read (no session-index.db, no
# cron.db, no sqlite, no workflows/, no approval-review/); unknown files are
# ignored. A session id (dir name) must match the UUID form
# [0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}; otherwise the
# dir is refused (one log line, counted rejected, never read, exit unaffected).
#
# Decoding is by the harness binary: `muse export --session <path>/session.jsonl
# --out <workdir file>` (never a directory path: the binary refuses it), under
# `timeout MEMOIRE_MUSE_EXPORT_TIMEOUT_S` (60). The export is read-only on the
# store; the adapter itself NEVER writes, creates, locks or touches anything
# under the store root. Exports are conversation content, so they go to a
# private workdir, never /tmp.
#
# Selection, exclusion, quiet gate
# --------------------------------
# --scan (default): root sessions whose tree (root session.jsonl or any child
# session.jsonl) has an mtime inside the window (--since SPEC, a `date -d`
# spec, default "48 hours ago"). --full drops the window. Explicit arguments
# (a root session dir or its session.jsonl) bypass the window, not the gates.
#
# Exclusion is fail-closed and BEFORE any export: the acpx exclusion file
# ${XDG_CONFIG_HOME:-$HOME/.config}/memoire/acpx-exclude (same file, format and
# matching as the acpx adapter: one absolute prefix per line, # comments,
# path-boundary match, symlinks resolved on both sides, trailing slash
# equivalent). A missing or unreadable file: exit 3 before any session is
# read. The workspace comes from a bounded probe of the ROOT session.jsonl: the
# first runtime.session.metadata record (payload.record.workspace_root) within
# the first 64 lines and 1 MiB. Excluded: no export, no event, children not
# read, counted excluded. No workspace in the bound: the unit is deferred
# (counted no_workspace), not exported, exit unaffected.
#
# Quiet gate (mtime only): a unit is processed when now - newest mtime over
# its tree's session.jsonl files > quiet (--quiet-for SEC,
# MEMOIRE_MUSE_QUIET_S, default 120); otherwise it is deferred without export.
# An open-but-quiet session IS captured incrementally.
#
# Events (one queue line per event; memoire-queue -batch-file)
# ------------------------------------------------------------
# Every export event becomes EXACTLY ONE capture event (capture-all). Line
# fields: project, session_id, source_adapter "muse-store", event_type,
# source_event_id, occurred_at, cwd, payload, and text when the event has text.
# project_root/project_source ride from the resolver like the other adapters.
# session_id is the id of the file that holds the record (child records carry
# the child id); cwd is the root's workspace_root for root and child events.
# project comes from memoire-resolve-project.sh on cwd (honours .memoire
# capture: false -> the unit emits nothing, counted capture_off);
# --project NAME / MEMOIRE_PROJECT override.
#
#   export event / record                       event_type            text
#   retained_frame                              muse_retained_frame   -
#   unparseable_line                            muse_unparseable_line raw_text
#   runtime.session started (run, prompt != "") muse_user_prompt      prompt
#   assistant_message_committed                 muse_assistant_message text
#   reasoning_committed                         muse_reasoning        text if non-empty
#   assistant_tool_calls_committed              muse_tool_calls       <name> <args> per call, \n
#   tool_result_batch_committed                 muse_tool_results     results[].text, \n\n
#   model_completed                             muse_model_completed  -
#   terminal                                    muse_turn_terminal    -
#   session.end                                 muse_session_end      -
#   subagent.control.*                          muse_subagent_control -
#   gap                                         muse_gap              -
#   any other export event kind                 muse_record           -
#
# payload = {envelope | raw_text | event, derived (when present), export_kind,
# root_session_id, parent_session_id, subagent_id, subagent_role, muse_internal,
# schema: "muse-export-v1"}; the envelope is never trimmed.
#
#   source_event_id (records are immutable, so ids are unversioned and stable):
#     record         muse:<session_id>:r:<envelope.id>
#     retained frame muse:<session_id>:frame:<outer_log_ordinal>
#     unparseable    muse:<session_id>:bad:<sha256(raw_text)[0:16]>
#     gap            muse:<session_id>:gap:<k>@<anchor> — positional: anchor =
#                    the envelope id of the nearest preceding record event
#                    (a duplicate record event included, "start" before any
#                    record); k counts the gaps seen so far after that anchor
#                    in this export (a re-appearing anchor continues its
#                    count); duplicate record events are not re-emitted; each
#                    gap is emitted once (cursor).
#
# Projections:
#   muse_assistant_turn — one per run_id with a terminal record and at least
#     one non-empty assistant_message_committed of that run_id; text = those
#     texts joined by \n\n in file order; payload {run_id, terminal, prompt,
#     models (distinct model_completed.model), usage (sums of numeric usage
#     fields), turn_index (derived.turn_count of the terminal record), link
#     fields, schema}; id muse:<session_id>:turn:<run_id>; emitted once ever
#     (cursor), never before the terminal.
#   muse_session — the session facts {session_id, root_session_id,
#     parent_session_id, subagent_id, subagent_role, muse_internal, model_id,
#     provider_id, workspace_root, client_info_name, turn_count,
#     accepted_spawns, session_end, diagnostics, exporter_version,
#     export_schema_version}; id muse:<session_id>:session:<sha256(canonical
#     facts JSON)[0:16]>; emitted when the digest differs from the cursor's.
#
# Parent link (children only; from ENVELOPES, never from accepted_spawns): in
# the root's records, subagent.control.child_session_bound with
# payload.record.child_session_id == the child dir name gives subagent_id; the
# resume_context_recorded with that subagent_id gives subagent_role
# (payload.record.role, null if absent). Bound => muse_internal false; a child
# with no bound record => muse_internal true, subagent_id and role null (still
# captured whole). Root events: parent fields null, muse_internal false.
#
# Cursor, idempotence, failures
# -----------------------------
# Cursor: MEMOIRE_CAPTURE_CURSOR_DIR/muse-capture.json (default
# $MEMOIRE_HOME/cursors/muse-capture.json), 0600, atomic. Per session file:
# size, mtime_ns, hwm (stream id -> highest queued sequence), frames (queued
# outer_log_ordinals), bad (queued unparseable ids), gaps (queued gap ids),
# turns (queued run_ids), facts (last queued digest). A record is emitted iff its sequence > hwm of its
# stream; a unit whose files all match size and mtime_ns (file set unchanged)
# is skipped without export (skipped_unchanged). The cursor is written only
# after a successful queue write; a failed queue write leaves it untouched
# (the next run re-emits the same ids), the unit fails, exit 1, other units
# continue. A cursor write failure after a successful queue write: exit 7.
# An export failure/timeout: log line, unit failed, exit 1, other units
# continue. Missing muse binary: exit 1 with one log line if a unit reaches the
# export step; nothing to read exits 0.
#
# Every run except the kill switch ends with ONE stderr line (the tick parses
# it): memoire-capture-muse: done: units=U events=E failed=F rejected=R
# excluded=X deferred=D skipped=S capture_off=C no_workspace=N. Log lines never
# name an excluded workspace and never print record content.
#
# Workdir: $MEMOIRE_HOME/tmp/muse-capture/run.XXXXXX (0700, files 0600, removed
# at exit and on TERM/INT/HUP), never under /tmp.
#
# Exit codes: 0 ok (including nothing to read, deferred, excluded, rejected);
# 1 a unit failed; 2 usage; 3 exclusion file missing/unreadable; 7 cursor
# persist failure.
#
# Options:
#   --scan               scan mode (default): sessions changed inside the window
#   --since SPEC         window cutoff, a `date -d` spec (default "48 hours ago")
#   --full               no window: every session
#   --sessions-dir DIR   store root (MEMOIRE_MUSE_SESSIONS_DIR)
#   --quiet-for SEC      quiet gate, seconds (MEMOIRE_MUSE_QUIET_S, default 120)
#   --project NAME       project override (MEMOIRE_PROJECT still works)
#   --dry-run            print the queue lines on stdout, write nothing (cursor
#                        included); nothing else is emitted
#   -h, --help           this block
#
# Knobs (all defaulted; a malformed value falls back to the default):
#   MEMOIRE_MUSE_SESSIONS_DIR (default ~/.local/share/muse/sessions),
#   MEMOIRE_MUSE_QUIET_S (120), MEMOIRE_MUSE_EXPORT_TIMEOUT_S (60),
#   MEMOIRE_MUSE_BIN (default: `muse` on PATH, then ~/.local/share/mise/shims/muse,
#   then ~/.local/bin/muse), MEMOIRE_QUEUE_BIN (must understand -batch-file),
#   MEMOIRE_RESOLVE_PROJECT, MEMOIRE_CAPTURE_CURSOR_DIR, MEMOIRE_PROJECT,
#   MEMOIRE_HOME, MEMOIRE_CAPTURE=off|0|false (kill switch).

# session-level kill switch (honored directly, before anything else).
case "${MEMOIRE_CAPTURE:-}" in off|0|false) exit 0 ;; esac

umask 077
SCRIPT_PATH="${BASH_SOURCE[0]}"
SCRIPT_DIR="${SCRIPT_PATH%/*}"
[[ "$SCRIPT_DIR" == "$SCRIPT_PATH" ]] && SCRIPT_DIR="."
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
export MEMOIRE_MUSE_ROOT="$ROOT"

exec python3 -I - "$@" <<'PYEOF'
import sys, os, json, hashlib, subprocess, signal, shutil, tempfile, re, time
from pathlib import Path
import datetime

UUID_RE = re.compile(r'^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$')
ROOT = os.environ.get('MEMOIRE_MUSE_ROOT', '.')


def log(msg):
    print('memoire-capture-muse: ' + msg, file=sys.stderr)


USAGE = '''Usage: memoire-capture-muse.sh [options] [session-dir | session.jsonl ...]

Reads the Muse Code session store and queues every logged record as one event,
plus a turn projection and per-session facts. With --scan it ingests the root
sessions under the store root changed since a cutoff; with explicit arguments
it ingests those (bypassing the window, not the gates). Re-runs are idempotent
(deterministic source_event_id + cursor).

Options:
  --scan               Scan the sessions dir (default).
  --since SPEC         Cutoff for --scan, a `date -d` spec (default "48 hours ago").
  --full               No window: every session.
  --sessions-dir DIR   Store root (MEMOIRE_MUSE_SESSIONS_DIR, default
                       ~/.local/share/muse/sessions).
  --quiet-for SEC      Quiet gate, seconds (MEMOIRE_MUSE_QUIET_S, default 120).
  --project NAME       Project override (MEMOIRE_PROJECT still works).
  --dry-run            Print the queue lines on stdout, write nothing.
  -h, --help           Show this help.'''


def parse_args(argv):
    opts = {'sessions_dir': None, 'since': None, 'full': 0, 'quiet_for': None,
            'dry_run': 0, 'project': None, 'scan': 1, 'scan_explicit': 0, 'explicit': []}
    i = 0
    while i < len(argv):
        a = argv[i]
        if a == '--scan':
            opts['scan'] = 1; opts['scan_explicit'] = 1; i += 1
        elif a == '--full':
            opts['full'] = 1; i += 1
        elif a == '--since':
            opts['since'] = argv[i + 1]; i += 2
        elif a == '--sessions-dir':
            opts['sessions_dir'] = argv[i + 1]; i += 2
        elif a == '--quiet-for':
            opts['quiet_for'] = argv[i + 1]; i += 2
        elif a == '--project':
            opts['project'] = argv[i + 1]; i += 2
        elif a == '--dry-run':
            opts['dry_run'] = 1; i += 1
        elif a in ('-h', '--help'):
            print(USAGE, file=sys.stderr); sys.exit(0)
        elif a.startswith('-'):
            log('unknown option: ' + a); sys.exit(2)
        else:
            opts['explicit'].append(a); i += 1
    if opts['scan_explicit'] and opts['explicit']:
        log('--scan and explicit session arguments are mutually exclusive')
        sys.exit(2)
    return opts


OPTS = parse_args(sys.argv[1:])

HOME = Path(os.environ.get('HOME', str(Path.home())))
MEMOIRE_HOME = Path(os.environ.get('MEMOIRE_HOME', str(HOME / '.memoire')))
SESSIONS_DIR = Path(os.environ.get('MEMOIRE_MUSE_SESSIONS_DIR',
                                  str(HOME / '.local/share/muse/sessions')))
if OPTS['sessions_dir']:
    SESSIONS_DIR = Path(OPTS['sessions_dir'])

# Quiet gate: a malformed value falls back to the default (it never kills capture).
def _int(value, default, zero_ok=False):
    try:
        v = int(value)
    except (TypeError, ValueError):
        return default
    if v < 0:
        return default
    if v == 0 and not zero_ok:
        return default
    return v

QUIET_FOR = _int(OPTS['quiet_for'] if OPTS['quiet_for'] is not None
                 else os.environ.get('MEMOIRE_MUSE_QUIET_S'), 120, zero_ok=True)
SINCE = OPTS['since'] if OPTS['since'] is not None else '48 hours ago'
DRY_RUN = OPTS['dry_run']
FULL = OPTS['full']

MUSE_BIN = os.environ.get('MEMOIRE_MUSE_BIN')
if not MUSE_BIN:
    for cand in (shutil.which('muse'),
                 str(HOME / '.local/share/mise/shims/muse'),
                 str(HOME / '.local/bin/muse')):
        if cand and os.path.isfile(cand) and os.access(cand, os.X_OK):
            MUSE_BIN = cand
            break
MUSE_BIN = MUSE_BIN or ''

QUEUE_BIN = os.environ.get('MEMOIRE_QUEUE_BIN') or str(Path(ROOT) / 'bin' / 'memoire-queue')
RESOLVER = os.environ.get('MEMOIRE_RESOLVE_PROJECT') or str(Path(ROOT) / 'scripts' / 'memoire-resolve-project.sh')
EXPORT_TIMEOUT = _int(os.environ.get('MEMOIRE_MUSE_EXPORT_TIMEOUT_S'), 60)
CURSOR_DIR = Path(os.environ.get('MEMOIRE_CAPTURE_CURSOR_DIR', str(MEMOIRE_HOME / 'cursors')))
CURSOR_FILE = CURSOR_DIR / 'muse-capture.json'
PROJECT_OVERRIDE = OPTS['project'] or os.environ.get('MEMOIRE_PROJECT') or ''

# --- exclusion file (fail-closed, before any session is read) ---------------
EXCLUDE_FILE = Path(os.environ.get('MEMOIRE_MUSE_EXCLUDE_FILE',
                                   str(Path(os.environ.get('XDG_CONFIG_HOME', str(HOME / '.config'))) / 'memoire' / 'acpx-exclude')))
EXCLUDE_PREFIXES = []
if not EXCLUDE_FILE.is_file():
    log('exclusion file missing (fail-closed): ' + str(EXCLUDE_FILE))
    sys.exit(3)
if not os.access(EXCLUDE_FILE, os.R_OK):
    log('exclusion file unreadable (fail-closed): ' + str(EXCLUDE_FILE))
    sys.exit(3)
try:
    for line in EXCLUDE_FILE.read_text(encoding='utf-8').splitlines():
        line = line.strip()
        if not line or line.startswith('#'):
            continue
        if line == '~':
            line = str(HOME)
        elif line.startswith('~/'):
            line = str(HOME / line[2:])
        resolved = os.path.realpath(line)
        if resolved != '/':
            resolved = resolved.rstrip('/')
        if resolved:
            EXCLUDE_PREFIXES.append(resolved)
except OSError:
    log('exclusion file unreadable (fail-closed): ' + str(EXCLUDE_FILE))
    sys.exit(3)


def is_excluded(cwd):
    if not cwd:
        return False
    real_cwd = os.path.realpath(cwd)
    for prefix in EXCLUDE_PREFIXES:
        if not prefix:
            continue
        # Component-boundary match: cwd equals the prefix, or it lies under
        # prefix/ (a sibling whose name only extends the prefix never matches).
        if real_cwd == prefix or real_cwd.startswith(prefix + '/'):
            return True
    return False


# --- workdir (conversation content: never /tmp) -----------------------------
_MUSE_TMP = MEMOIRE_HOME / 'tmp' / 'muse-capture'
_MUSE_TMP.mkdir(parents=True, exist_ok=True)
try:
    _MUSE_TMP.chmod(0o700)
except OSError:
    pass
WORKDIR = Path(tempfile.mkdtemp(prefix='run.', dir=str(_MUSE_TMP)))


def cleanup(*_):
    try:
        shutil.rmtree(WORKDIR, ignore_errors=True)
    except OSError:
        pass


def _term(*_):
    cleanup()
    sys.exit(0)


for _sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
    try:
        signal.signal(_sig, _term)
    except (ValueError, OSError):
        pass


# --- store enumeration -------------------------------------------------------
def root_of(dirpath):
    """(root_sid, child_sid) for a session.jsonl dir, or None if not a session."""
    if 'session.jsonl' not in os.listdir(dirpath) and not os.path.isfile(os.path.join(dirpath, 'session.jsonl')):
        return None
    base = os.path.basename(dirpath)
    parent = os.path.basename(os.path.dirname(dirpath))
    if parent == 'subagent':
        return (os.path.basename(os.path.dirname(os.path.dirname(dirpath))), base)
    return (base, None)


def find_session_files(store):
    """Yield (root_sid, child_sid, dirpath, path) for every session.jsonl."""
    out = []
    for dirpath, dirnames, filenames in os.walk(store):
        if 'session.jsonl' not in filenames:
            continue
        rd = root_of(dirpath)
        if rd is None:
            continue
        root_sid, child_sid = rd
        out.append((root_sid, child_sid, dirpath, os.path.join(dirpath, 'session.jsonl')))
    return out


# Group files by root session id: each unit = a root session + all its children.
units = {}
if SESSIONS_DIR.is_dir():
    for root_sid, child_sid, dirpath, path in find_session_files(SESSIONS_DIR):
        units.setdefault(root_sid, {'root': (root_sid, dirpath, path), 'children': []})
        if child_sid is not None:
            units[root_sid]['children'].append((child_sid, dirpath, path))

# Rejected: a session dir whose name is not a UUID (root or child) is refused.
REJECTED = 0
for root_sid, u in list(units.items()):
    if not UUID_RE.match(root_sid):
        log('refused (invalid session id): ' + root_sid)
        REJECTED += 1
        del units[root_sid]
        continue
    for child_sid, dirpath, path in u['children']:
        if not UUID_RE.match(child_sid):
            log('refused (invalid child session id): ' + child_sid)
            REJECTED += 1
            u['children'].remove((child_sid, dirpath, path))
    if not UUID_RE.match(u['root'][0]):
        continue


def newest_mtime(u):
    files = [u['root'][2]] + [p for _, _, p in u['children']]
    newest = 0
    for p in files:
        try:
            m = os.stat(p).st_mtime
        except OSError:
            continue
        if m > newest:
            newest = m
    return newest


def probe_workspace(path):
    """First runtime.session.metadata workspace_root within 64 lines and 1 MiB of
    bytes read, else None. Binary reads with a remaining-byte budget: a line that
    does not fit the budget ends the probe without decoding it; JSON is decoded
    only for lines that fit."""
    BUDGET = 1024 * 1024
    MAX_LINES = 64
    try:
        with open(path, 'rb') as fh:
            remaining = BUDGET
            for _ in range(MAX_LINES):
                line = fh.readline(remaining)
                if not line:
                    break
                # A line that does not fit the remaining budget (no newline and
                # exactly `remaining` bytes) ends the probe without decoding it.
                if not line.endswith(b'\n') and len(line) == remaining:
                    break
                remaining -= len(line)
                try:
                    rec = json.loads(line.decode('utf-8', errors='replace'))
                except ValueError:
                    continue
                if rec.get('payload_type') == 'runtime.session.metadata':
                    recd = (rec.get('payload') or {}).get('record') or {}
                    ws = recd.get('workspace_root')
                    if ws:
                        return ws
    except OSError:
        return None
    return None


# --- selection (which units this run processes) ------------------------------
def resolve_cutoff(spec):
    """Return the cutoff epoch for a `date -d` spec, or None on failure."""
    try:
        p = subprocess.run(['date', '-d', spec, '+%s'], capture_output=True, text=True)
        if p.returncode == 0:
            return int(p.stdout.strip())
    except (OSError, ValueError):
        pass
    return None


CUTOFF = None
if not FULL and OPTS['scan'] == 1:
    CUTOFF = resolve_cutoff(SINCE)

# Select the units this run processes:
#   explicit args -> bypass the window (gates still apply);
#   --full        -> every unit;
#   --scan        -> units whose tree mtime is inside the window.
selected = {}
if OPTS['explicit']:
    for arg in OPTS['explicit']:
        ap = Path(arg)
        if ap.is_dir():
            rdir = str(ap)
            rsid = os.path.basename(rdir)
        elif ap.is_file() and ap.name == 'session.jsonl':
            d = str(ap.parent)
            if os.path.basename(os.path.dirname(d)) == 'subagent':
                rdir = os.path.dirname(os.path.dirname(d))
            else:
                rdir = d
            rsid = os.path.basename(rdir)
        else:
            continue
        if rsid in units:
            selected[rsid] = units[rsid]
elif FULL:
    selected = units
else:
    now = time.time()
    for rsid, u in units.items():
        newest = newest_mtime(u)
        if CUTOFF is not None and newest < CUTOFF:
            continue
        selected[rsid] = u

# Oldest root first (by the tree's newest mtime), then by id for determinism.
order = sorted(selected, key=lambda s: (newest_mtime(selected[s]), s))


def iso_from_us(us):
    if us is None:
        us = int(time.time() * 1e6)
    dt = datetime.datetime.fromtimestamp(us / 1e6, datetime.timezone.utc)
    return dt.strftime('%Y-%m-%dT%H:%M:%S.') + ('%03dZ' % (dt.microsecond // 1000))


def map_record(pt, ek, pk, evv):
    if pt == 'runtime.session' and ek == 'started' and pk == 'run' and evv.get('prompt'):
        return 'muse_user_prompt', evv['prompt']
    if ek == 'assistant_message_committed':
        return 'muse_assistant_message', evv.get('text')
    if ek == 'reasoning_committed':
        return 'muse_reasoning', (evv.get('text') or None)
    if ek == 'assistant_tool_calls_committed':
        return 'muse_tool_calls', '\n'.join('%s %s' % (c.get('name'), c.get('args'))
                                            for c in (evv.get('tool_calls') or []))
    if ek == 'tool_result_batch_committed':
        return 'muse_tool_results', '\n\n'.join(r.get('text', '') for r in (evv.get('results') or []))
    if ek == 'model_completed':
        return 'muse_model_completed', None
    if ek == 'terminal':
        return 'muse_turn_terminal', None
    if pt == 'session.end':
        return 'muse_session_end', None
    if isinstance(pt, str) and pt.startswith('subagent.control.'):
        return 'muse_subagent_control', None
    return 'muse_record', None


BIN_MISSING_LOGGED = [False]


def run_export(path, tag):
    if not MUSE_BIN or not os.path.isfile(MUSE_BIN) or not os.access(MUSE_BIN, os.X_OK):
        return None, 'no-muse-bin'
    out = str(WORKDIR / ('export-' + tag + '.json'))
    try:
        p = subprocess.run(['timeout', str(EXPORT_TIMEOUT), MUSE_BIN, 'export',
                            '--session', path, '--out', out],
                           stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                           stderr=subprocess.PIPE, text=True)
    except OSError:
        return None, 'no-muse-bin'
    if p.returncode != 0:
        return None, 'export-failed'
    try:
        with open(out, encoding='utf-8') as fh:
            return json.load(fh), None
    except (OSError, ValueError):
        return None, 'export-failed'


def resolve_links(root_export, rsid):
    bound, roles = {}, {}
    for ev in (root_export.get('events') or []):
        env = ev.get('envelope')
        if not isinstance(env, dict):
            continue
        pt = env.get('payload_type')
        rec = (env.get('payload') or {}).get('record')
        if not isinstance(rec, dict):
            continue
        if pt == 'subagent.control.child_session_bound':
            if rec.get('child_session_id'):
                bound[rec['child_session_id']] = rec.get('subagent_id')
        elif pt == 'subagent.control.resume_context_recorded':
            if rec.get('subagent_id'):
                roles[rec['subagent_id']] = rec.get('role')
    links = {}
    for csid, aid in bound.items():
        links[csid] = {'root_session_id': rsid, 'parent_session_id': rsid,
                       'subagent_id': aid, 'subagent_role': roles.get(aid),
                       'muse_internal': False}
    return links


def link_for(sid, rsid, links):
    if sid in links:
        return links[sid]
    if sid == rsid:
        return {'root_session_id': rsid, 'parent_session_id': None, 'subagent_id': None,
                'subagent_role': None, 'muse_internal': False}
    return {'root_session_id': rsid, 'parent_session_id': rsid, 'subagent_id': None,
            'subagent_role': None, 'muse_internal': True}


def build_file(export, sid, rsid, link, ws, project, proot, psrc, unit_cursor, path):
    events = []
    uc = unit_cursor if isinstance(unit_cursor, dict) else {}
    old_hwm = dict((uc.get('hwm') or {}).get(sid, {}))
    old_frames = set((uc.get('frames') or {}).get(sid, []))
    old_bad = set((uc.get('bad') or {}).get(sid, []))
    old_gaps = set((uc.get('gaps') or {}).get(sid, []))
    old_turns = set((uc.get('turns') or {}).get(sid, []))
    old_facts = (uc.get('facts') or {}).get(sid)
    try:
        file_mtime_us = int(os.stat(path).st_mtime * 1e6)
    except OSError:
        file_mtime_us = None

    def occ(ts):
        return iso_from_us(ts if ts is not None else file_mtime_us)

    def make_event(etype, eid, occurred, text, payload):
        e = {'project': project, 'session_id': sid, 'source_adapter': 'muse-store',
             'event_type': etype, 'source_event_id': eid, 'occurred_at': occurred,
             'cwd': ws, 'payload': payload}
        if text:
            e['text'] = text
        if proot:
            e['project_root'] = proot
        if psrc:
            e['project_source'] = psrc
        return e

    link_payload = {'root_session_id': link['root_session_id'],
                    'parent_session_id': link['parent_session_id'],
                    'subagent_id': link['subagent_id'], 'subagent_role': link['subagent_role'],
                    'muse_internal': link['muse_internal'], 'schema': 'muse-export-v1'}

    # Session facts.
    s0 = (export.get('sessions') or [{}])[0] or {}
    model_id = provider_id = workspace_root = client_info_name = None
    for ev in (export.get('events') or []):
        env = ev.get('envelope')
        if not isinstance(env, dict):
            continue
        pt = env.get('payload_type')
        rec = (env.get('payload') or {}).get('record')
        if not isinstance(rec, dict):
            rec = {}
        if pt == 'runtime.session.metadata':
            if model_id is None:
                model_id = rec.get('model_id')
                provider_id = rec.get('provider_id')
            if workspace_root is None:
                workspace_root = rec.get('workspace_root')
        elif pt == 'session.opened.observed' and client_info_name is None:
            client_info_name = rec.get('client_info_name')
    facts = {'session_id': sid, 'root_session_id': link['root_session_id'],
             'parent_session_id': link['parent_session_id'], 'subagent_id': link['subagent_id'],
             'subagent_role': link['subagent_role'], 'muse_internal': link['muse_internal'],
             'model_id': model_id, 'provider_id': provider_id, 'workspace_root': workspace_root,
             'client_info_name': client_info_name, 'turn_count': s0.get('turn_count'),
             'accepted_spawns': s0.get('accepted_spawns'), 'session_end': s0.get('session_end'),
             'diagnostics': export.get('diagnostics'), 'exporter_version': export.get('exporter_version'),
             'export_schema_version': export.get('export_schema_version')}
    facts_digest = hashlib.sha256(
        json.dumps(facts, sort_keys=True, separators=(',', ':')).encode('utf-8')).hexdigest()[:16]

    max_recorded = None
    for ev in (export.get('events') or []):
        env = ev.get('envelope')
        if isinstance(env, dict) and isinstance(env.get('recorded_at'), (int, float)):
            if max_recorded is None or env['recorded_at'] > max_recorded:
                max_recorded = env['recorded_at']

    new_hwm = dict(old_hwm)
    new_frames = set(old_frames)
    new_bad = set(old_bad)
    new_gaps = set(old_gaps)
    new_turns = set(old_turns)
    new_facts = old_facts

    if facts_digest != old_facts:
        events.append(make_event('muse_session', 'muse:%s:session:%s' % (sid, facts_digest),
                                 occ(max_recorded), None, dict(facts, schema='muse-export-v1')))
        new_facts = facts_digest

    runs = {}
    running_max = None
    # Gap ordinals: a gap is named by its position — anchor = the nearest
    # preceding record event's envelope id (a duplicate record event included,
    # "start" before any record); k counts the gaps seen so far after that
    # anchor in this export (a re-appearing anchor continues its count).
    gap_anchor = 'start'
    gap_counts = {}
    for ev in (export.get('events') or []):
        kind = ev.get('kind')
        env = ev.get('envelope')
        der = ev.get('derived')
        if kind == 'retained_frame':
            ordinal = ev.get('outer_log_ordinal')
            if ordinal in old_frames:
                continue
            payload = {'export_kind': 'retained_frame', 'envelope': env}
            if isinstance(der, dict):
                payload['derived'] = der
            payload.update(link_payload)
            events.append(make_event('muse_retained_frame',
                                     'muse:%s:frame:%s' % (sid, ordinal), occ(running_max), None, payload))
            new_frames.add(ordinal)
        elif kind == 'unparseable_line':
            raw = ev.get('raw_text')
            bid = hashlib.sha256((raw or '').encode('utf-8')).hexdigest()[:16]
            if bid in old_bad:
                continue
            payload = {'export_kind': 'unparseable_line', 'raw_text': raw}
            payload.update(link_payload)
            events.append(make_event('muse_unparseable_line', 'muse:%s:bad:%s' % (sid, bid),
                                     occ(running_max), raw, payload))
            new_bad.add(bid)
        elif kind == 'record':
            if not isinstance(env, dict):
                continue
            # Every record event (new, already captured, or a duplicate
            # re-export) re-anchors the gap ordinals that follow it.
            if env.get('id') is not None:
                gap_anchor = env['id']
            # A duplicate re-export of an already-exported record: it only
            # re-anchors the gaps — no queue line, no run state, no high-water
            # mark, no running_max.
            if isinstance(der, dict) and der.get('duplicate'):
                continue
            seq = env.get('sequence')
            stream = (env.get('stream') or {}).get('id') if isinstance(env.get('stream'), dict) else None
            if isinstance(env.get('recorded_at'), (int, float)):
                if running_max is None or env['recorded_at'] > running_max:
                    running_max = env['recorded_at']
            is_new = True
            if isinstance(seq, int) and stream is not None:
                if seq <= old_hwm.get(stream, 0):
                    is_new = False
                elif seq > new_hwm.get(stream, 0):
                    new_hwm[stream] = seq
            pl = env.get('payload') or {}
            evv = pl.get('event') if isinstance(pl, dict) else None
            if not isinstance(evv, dict):
                evv = {}
            ek = evv.get('kind')
            rid = pl.get('run_id') if isinstance(pl, dict) else None
            # The turn is projected from the WHOLE export (all its records,
            # including ones queued in an earlier run), so the run state is
            # updated for every record, new or not.
            if rid is not None:
                r = runs.setdefault(rid, {'prompt': '', 'texts': [], 'terminal': None,
                                          'models': [], 'usages': [], 'turn_index': None,
                                          'has_text': False})
                if ek == 'started' and pl.get('kind') == 'run':
                    if evv.get('prompt'):
                        r['prompt'] = evv['prompt']
                elif ek == 'assistant_message_committed':
                    if evv.get('text'):
                        r['texts'].append(evv['text'])
                        r['has_text'] = True
                elif ek == 'model_completed':
                    if evv.get('model'):
                        r['models'].append(evv['model'])
                    if isinstance(evv.get('usage'), dict):
                        r['usages'].append(evv['usage'])
                elif ek == 'terminal':
                    r['terminal'] = evv.get('terminal')
                    if isinstance(der, dict):
                        r['turn_index'] = der.get('turn_count')
            if is_new:
                etype, text = map_record(env.get('payload_type'), ek,
                                         pl.get('kind') if isinstance(pl, dict) else None, evv)
                payload = {'export_kind': 'record', 'envelope': env}
                if isinstance(der, dict):
                    payload['derived'] = der
                payload.update(link_payload)
                events.append(make_event(etype, 'muse:%s:r:%s' % (sid, env.get('id')),
                                         occ(env.get('recorded_at')), text, payload))
            if rid is not None and ek == 'terminal':
                r = runs[rid]
                if r['terminal'] is not None and r['has_text'] and rid not in old_turns:
                    usage = {}
                    for us in r['usages']:
                        for k, v in us.items():
                            if isinstance(v, (int, float)) and not isinstance(v, bool):
                                usage[k] = usage.get(k, 0) + v
                    models = []
                    for m in r['models']:
                        if m not in models:
                            models.append(m)
                    turn_payload = {'run_id': rid, 'terminal': r['terminal'], 'prompt': r['prompt'],
                                    'models': models, 'usage': usage, 'turn_index': r['turn_index'],
                                    'root_session_id': link['root_session_id'],
                                    'parent_session_id': link['parent_session_id'],
                                    'subagent_id': link['subagent_id'],
                                    'subagent_role': link['subagent_role'],
                                    'muse_internal': link['muse_internal'], 'schema': 'muse-export-v1'}
                    events.append(make_event('muse_assistant_turn', 'muse:%s:turn:%s' % (sid, rid),
                                             occ(running_max), '\n\n'.join(r['texts']), turn_payload))
                    new_turns.add(rid)
        elif kind == 'gap':
            # An omitted record: no id, sequence or time. Named by position —
            # the k-th gap after the nearest preceding record, the count
            # cumulative per anchor (a re-appearing anchor continues its
            # count); emitted once, tracked in the cursor like unparseable
            # lines.
            gap_counts[gap_anchor] = gap_counts.get(gap_anchor, 0) + 1
            gid = 'muse:%s:gap:%d@%s' % (sid, gap_counts[gap_anchor], gap_anchor)
            if gid in old_gaps:
                continue
            payload = {'export_kind': 'gap'}
            for key, val in ev.items():
                if key != 'kind':
                    payload[key] = val
            payload.update(link_payload)
            events.append(make_event('muse_gap', gid, occ(running_max), None, payload))
            new_gaps.add(gid)
        else:
            log('unhandled export event kind %s (session %s)' % (kind, sid))
            eid = 'muse:%s:r:%s' % (sid,
                                    hashlib.sha256(json.dumps(ev, sort_keys=True).encode('utf-8')).hexdigest()[:16])
            payload = dict(ev)
            payload['export_kind'] = kind
            payload.update(link_payload)
            events.append(make_event('muse_record', eid, occ(running_max), None, payload))

    return events, {'hwm': new_hwm, 'frames': sorted(new_frames), 'bad': sorted(new_bad),
                    'gaps': sorted(new_gaps), 'turns': sorted(new_turns), 'facts': new_facts}


def resolve_project(cwd):
    """Return (project, root, source) via the shared resolver (fail-open to basename)."""
    key = root = source = ''
    if cwd and os.path.isfile(RESOLVER) and os.access(RESOLVER, os.X_OK):
        try:
            p = subprocess.run([RESOLVER, '--verbose', cwd], capture_output=True, text=True)
            if p.returncode == 0 and p.stdout.strip():
                parts = p.stdout.split('\t')
                key = parts[0]
                root = parts[1] if len(parts) > 1 else ''
                source = parts[2] if len(parts) > 2 else ''
        except OSError:
            pass
    if not key:
        key = os.path.basename(cwd) if cwd else 'muse-unknown'
        root = source = ''
    return key, root, source


def capture_disabled(cwd):
    if not cwd or not os.path.isfile(RESOLVER) or not os.access(RESOLVER, os.X_OK):
        return False
    try:
        p = subprocess.run([RESOLVER, '--capture-enabled', cwd], capture_output=True, text=True)
        return (p.stdout.strip() == 'off')
    except OSError:
        return False


def unit_files(u):
    files = {}
    rsid, rdir, rpath = u['root']
    try:
        st = os.stat(rpath)
        files[rsid] = [st.st_size, st.st_mtime_ns]
    except OSError:
        return None
    for csid, cdir, cpath in u['children']:
        try:
            st = os.stat(cpath)
        except OSError:
            return None
        files[csid] = [st.st_size, st.st_mtime_ns]
    return files


def load_cursor():
    if DRY_RUN:
        return {}
    try:
        with open(CURSOR_FILE, encoding='utf-8') as fh:
            return json.load(fh)
    except (OSError, ValueError):
        return {}


def save_cursor(cursor):
    try:
        CURSOR_FILE.parent.mkdir(parents=True, exist_ok=True)
        tmp = str(CURSOR_FILE) + '.tmp'
        with open(tmp, 'w', encoding='utf-8') as fh:
            json.dump(cursor, fh, ensure_ascii=False)
        os.chmod(tmp, 0o600)
        os.replace(tmp, CURSOR_FILE)
        return True
    except OSError:
        return False


def process_unit(u, files, ws, project, proot, psrc, cursor):
    rsid, rdir, rpath = u['root']
    all_files = [(rsid, rdir, rpath)] + \
        [(csid, cdir, cpath) for csid, cdir, cpath in sorted(u['children'], key=lambda x: x[0])]
    exports = {}
    for sid, ddir, path in all_files:
        exp, err = run_export(path, str(sid))
        if exp is None:
            if err == 'no-muse-bin':
                if not BIN_MISSING_LOGGED[0]:
                    log('muse binary missing: ' + (MUSE_BIN or '(unresolved)'))
                    BIN_MISSING_LOGGED[0] = True
            else:
                log('export failed for session ' + sid)
            return False, 0, None
        if exp.get('export_schema_version') != 1:
            log('refused (export_schema_version %s): %s' % (exp.get('export_schema_version'), sid))
            return False, 0, None
        diag = exp.get('diagnostics') or {}
        nz = [k for k, v in diag.items() if isinstance(v, (int, float)) and not isinstance(v, bool) and v]
        if nz:
            log('diagnostics (session %s): %s' % (sid, ', '.join('%s=%s' % (k, diag[k]) for k in nz)))
        exports[sid] = exp
    links = resolve_links(exports[rsid], rsid)
    ucur = cursor.get(rsid) or {}
    events = []
    # The fingerprint is the PRE-export snapshot (taken once in the main loop
    # before any export runs), so a record appended to a file while its export
    # runs is not marked captured: the next run sees a changed file and re-reads
    # it (the per-stream sequence marks dedupe the overlap).
    new_state = {'files': files, 'hwm': {}, 'frames': {}, 'bad': {}, 'gaps': {}, 'turns': {}, 'facts': {}}
    for sid, ddir, path in all_files:
        link = link_for(sid, rsid, links)
        file_events, file_meta = build_file(exports[sid], sid, rsid, link, ws, project, proot, psrc, ucur, path)
        events.extend(file_events)
        for k in ('hwm', 'frames', 'bad', 'gaps', 'turns', 'facts'):
            new_state[k][sid] = file_meta[k]
    if not events:
        return True, 0, new_state
    if DRY_RUN:
        for e in events:
            print(json.dumps(e, ensure_ascii=False))
        return True, len(events), new_state
    batch = str(WORKDIR / ('batch-' + rsid + '.jsonl'))
    with open(batch, 'w', encoding='utf-8') as fh:
        for e in events:
            fh.write(json.dumps(e, ensure_ascii=False) + '\n')
    try:
        p = subprocess.run([QUEUE_BIN, '-batch-file', batch], stdin=subprocess.DEVNULL,
                           stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
    except OSError:
        p = None
    if p is None or p.returncode != 0:
        log('queue write failed for unit ' + rsid)
        return False, 0, None
    return True, len(events), new_state


# --- main --------------------------------------------------------------------
cursor = load_cursor()
counters = {'units': 0, 'events': 0, 'failed': 0, 'rejected': REJECTED,
            'excluded': 0, 'deferred': 0, 'skipped': 0, 'capture_off': 0, 'no_workspace': 0}

for rsid in order:
    u = selected[rsid]
    root_sid, rdir, rpath = u['root']
    ws = probe_workspace(rpath)
    if ws is None:
        counters['no_workspace'] += 1
        log('defer (no workspace): ' + rsid)
        continue
    if is_excluded(ws):
        counters['excluded'] += 1
        log('skip (excluded): ' + rsid)
        continue
    newest = newest_mtime(u)
    if time.time() - newest <= QUIET_FOR:
        counters['deferred'] += 1
        log('defer (not quiet): ' + rsid)
        continue
    project, proot, psrc = resolve_project(ws)
    if PROJECT_OVERRIDE:
        project = PROJECT_OVERRIDE
        proot = psrc = ''
    if capture_disabled(ws):
        counters['capture_off'] += 1
        log('skip (capture off): ' + rsid)
        continue
    # One pre-export fingerprint, taken once before any export runs, for dry and
    # real runs alike: it drives the unchanged-skip check and is what the cursor
    # stores (so a file changed during an export is re-read on the next run).
    files = unit_files(u)
    if not DRY_RUN:
        entry = cursor.get(rsid)
        if files is not None and entry and entry.get('files') == files:
            counters['skipped'] += 1
            log('skip (unchanged): ' + rsid)
            continue
    ok, n_events, new_state = process_unit(u, files, ws, project, proot, psrc, cursor)
    if not ok:
        counters['failed'] += 1
        continue
    counters['units'] += 1
    counters['events'] += n_events
    if not DRY_RUN:
        cursor[rsid] = new_state

cleanup()
log('done: units=%d events=%d failed=%d rejected=%d excluded=%d deferred=%d skipped=%d '
    'capture_off=%d no_workspace=%d' % (
        counters['units'], counters['events'], counters['failed'], counters['rejected'],
        counters['excluded'], counters['deferred'], counters['skipped'], counters['capture_off'],
        counters['no_workspace']))

if not DRY_RUN:
    if counters['units'] > 0 and not save_cursor(cursor):
        log('state persistence failed; rerun to heal (exit 7)')
        sys.exit(7)

if counters['failed'] > 0:
    sys.exit(1)
sys.exit(0)
PYEOF
