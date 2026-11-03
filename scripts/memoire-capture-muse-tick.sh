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
import signal
import subprocess
import sys
import time

os.umask(0o077)
home = Path(os.environ.get('MEMOIRE_HOME', str(Path.home() / '.memoire')))
logfile = home / 'log/capture-muse-tick.log'


def log(message):
    try:
        logfile.parent.mkdir(parents=True, exist_ok=True)
        with logfile.open('a') as f:
            f.write(time.strftime('%Y-%m-%dT%H:%M:%S%z') + ' tick_muse ' + message + '\n')
    except OSError:
        pass


def skip(reason):
    log('skipped=' + reason)
    sys.exit(0)


if os.environ.get('MEMOIRE_CAPTURE') in ('off', '0', 'false'):
    skip('capture_disabled')
gate = os.environ.get('MEMOIRE_CAPTURE_MUSE', '')
if gate in ('', 'off', '0', 'false'):
    skip('muse_switch_off')
if gate not in ('on', '1', 'true'):
    skip('muse_switch_unrecognised')


def number(name, default, zero=False):
    value = os.environ.get(name, '')
    return int(value) if re.fullmatch(r'[0-9]+', value) and (zero or int(value) > 0) else default


bound = number('MEMOIRE_MUSE_TICK_TIMEOUT', 240)
kill_delay = number('MEMOIRE_MUSE_TICK_KILL_S', 10)
quiet = number('MEMOIRE_MUSE_TICK_QUIET_S', 120, True)
since = number('MEMOIRE_TICK_SINCE_S', 172800)
sessions = Path(os.environ.get('MEMOIRE_MUSE_SESSIONS_DIR',
                              str(Path.home() / '.local/share/muse/sessions')))
adapter = Path(os.environ.get('MEMOIRE_CAPTURE_MUSE_ADAPTER',
                              str(Path(sys.argv[1]) / 'memoire-capture-muse.sh')))
if not adapter.is_file() or not os.access(adapter, os.X_OK):
    skip('capture_bin_missing')
if not sessions.is_dir():
    skip('no_sessions_dir')

try:
    home.mkdir(parents=True, exist_ok=True)
    lockpath = home / 'capture-muse-tick.lock'
    fd = os.open(lockpath, os.O_CREAT | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
    lock = os.fdopen(fd, 'w')
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
except BlockingIOError:
    skip('locked')
except OSError:
    skip('lock_unavailable')

# One adapter call per tick: the adapter owns its cursor, so the tick writes no
# per-file markers and no state dir of its own.
argv = [str(adapter), '--scan', '--sessions-dir', str(sessions),
        '--since', str(since) + ' seconds ago', '--quiet-for', str(quiet)]

summary = re.compile(r'^memoire-capture-muse: done: units=(\d+) events=(\d+) failed=(\d+) '
                     r'rejected=(\d+) excluded=(\d+) deferred=(\d+) skipped=(\d+) '
                     r'capture_off=(\d+) no_workspace=(\d+)$')
units = events = failed = rejected = excluded = deferred = skipped = capture_off = no_workspace = 0
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
    # else (session ids, diagnostics) is discarded.
    rows = [summary.fullmatch(line) for line in error.decode(errors='replace').splitlines()]
    rows = [row for row in rows if row]
    if len(rows) == 1:
        summary_ok = 1
        (units, events, failed, rejected, excluded, deferred,
         skipped, capture_off, no_workspace) = map(int, rows[0].groups())
except (OSError, ValueError):
    pass

log(f'units={units} events={events} failed={failed} rejected={rejected} excluded={excluded} '
    f'deferred={deferred} skipped={skipped} capture_off={capture_off} no_workspace={no_workspace} '
    f'summary={summary_ok} timed_out={timed_out} rc={last_rc}')
PYTHON
