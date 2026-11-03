#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec python3 - "$SCRIPT_DIR" <<'PYTHON'
import fcntl
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import subprocess
import sys
import time

os.umask(0o077)
home = Path(os.environ.get('MEMOIRE_HOME', str(Path.home() / '.memoire')))
logfile = home / 'log/capture-acpx-tick.log'

def log(message):
    try:
        logfile.parent.mkdir(parents=True, exist_ok=True)
        with logfile.open('a') as f:
            f.write(time.strftime('%Y-%m-%dT%H:%M:%S%z') + ' tick_acpx ' + message + '\n')
    except OSError:
        pass

def skip(reason):
    log('skipped=' + reason)
    sys.exit(0)

if os.environ.get('MEMOIRE_CAPTURE') in ('off', '0', 'false'):
    skip('capture_disabled')
gate = os.environ.get('MEMOIRE_CAPTURE_ACPX', '')
if gate in ('', 'off', '0', 'false'):
    skip('acpx_switch_off')
if gate not in ('on', '1', 'true'):
    skip('acpx_switch_unrecognised')

def number(name, default, zero=False):
    value = os.environ.get(name, '')
    return int(value) if re.fullmatch(r'[0-9]+', value) and (zero or int(value) > 0) else default

bound = number('MEMOIRE_ACPX_TICK_TIMEOUT', 240)
kill_delay = number('MEMOIRE_ACPX_TICK_KILL_S', 10)
quiet = number('MEMOIRE_ACPX_TICK_QUIET_S', 3600, True)
since = number('MEMOIRE_TICK_SINCE_S', 172800)
sessions = Path(os.environ.get('MEMOIRE_ACPX_SESSIONS_DIR',
                              os.environ.get('ACPX_SESSIONS_DIR', str(Path.home() / '.acpx/sessions'))))
adapter = Path(os.environ.get('MEMOIRE_CAPTURE_ACPX_BIN',
                             str(Path(sys.argv[1]) / 'memoire-capture-acpx.sh')))
if os.environ.get('MEMOIRE_ACPX_ARCHIVE_DIR') is not None:
    archive_env = os.environ.get('MEMOIRE_ACPX_ARCHIVE_DIR')
    archive = Path(archive_env) if archive_env else None
else:
    host = socket.gethostname().split('.')[0]
    archive = Path.home() / 'Sync/archives/acpx-history' / host / 'sessions'

if not shutil.which('jq'):
    skip('no_jq')
if not adapter.is_file() or not os.access(adapter, os.X_OK):
    skip('capture_bin_missing')
if not sessions.is_dir():
    skip('no_sessions_dir')

try:
    home.mkdir(parents=True, exist_ok=True)
    lockpath = home / 'capture-acpx-tick.lock'
    fd = os.open(lockpath, os.O_CREAT | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
    lock = os.fdopen(fd, 'w')
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
except BlockingIOError:
    skip('locked')
except OSError:
    skip('lock_unavailable')

# One adapter call per tick: the adapter owns its cursor (sizes + mtimes + mode),
# so the tick writes no per-file markers and no state dir of its own. The archive
# dir is passed only when it exists (an empty or absent value means none).
argv = [str(adapter), '--scan', '--sessions-dir', str(sessions)]
archive_flag = 0
if archive is not None and archive.is_dir():
    archive_flag = 1
    argv += ['--archive-dir', str(archive)]
argv += ['--since', str(since) + ' seconds ago', '--quiet-for', str(quiet)]

summary = re.compile(r'^memoire-capture-acpx: done: records=(\d+) skipped=(\d+) failed_records=(\d+) failed_events=(\d+)$')
records = skipped = failed_records = failed_events = 0
summary_ok = 0
timed_out = 0
last_rc = 0

try:
    proc = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                            stderr=subprocess.PIPE, start_new_session=True)
    try:
        _, error = proc.communicate(timeout=bound)
        last_rc = proc.returncode
    except subprocess.TimeoutExpired:
        os.killpg(proc.pid, signal.SIGTERM)
        try:
            _, error = proc.communicate(timeout=kill_delay)
            last_rc = 124
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGKILL)
            _, error = proc.communicate()
            last_rc = 137
        timed_out = 1
    # Only the adapter's single summary line ever reaches the log; everything
    # else (record ids, diagnostics) is discarded.
    rows = [summary.fullmatch(line) for line in error.decode(errors='replace').splitlines()]
    rows = [row for row in rows if row]
    if len(rows) == 1:
        summary_ok = 1
        records, skipped, failed_records, failed_events = map(int, rows[0].groups())
except (OSError, ValueError):
    pass

log(f'records={records} skipped={skipped} failed_records={failed_records} failed_events={failed_events} summary={summary_ok} archive={archive_flag} timed_out={timed_out} rc={last_rc}')
PYTHON
