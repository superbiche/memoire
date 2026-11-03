#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec python3 - "$SCRIPT_DIR" <<'PYTHON'
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import time

os.umask(0o077)
home = Path(os.environ.get('MEMOIRE_HOME', str(Path.home() / '.memoire')))
logfile = home / 'log/capture-qwen-tick.log'

def log(message):
    try:
        logfile.parent.mkdir(parents=True, exist_ok=True)
        with logfile.open('a') as f:
            f.write(time.strftime('%Y-%m-%dT%H:%M:%S%z') + ' tick_qwen ' + message + '\n')
    except OSError:
        pass

def skip(reason):
    log('skipped=' + reason)
    sys.exit(0)

if os.environ.get('MEMOIRE_CAPTURE') in ('off', '0', 'false'):
    skip('capture_disabled')
gate = os.environ.get('MEMOIRE_CAPTURE_QWEN', '')
if gate in ('', 'off', '0', 'false'):
    skip('qwen_switch_off')
if gate not in ('on', '1', 'true'):
    skip('qwen_switch_unrecognised')

def number(name, default, zero=False):
    value = os.environ.get(name, '')
    return int(value) if re.fullmatch(r'[0-9]+', value) and (zero or int(value) > 0) else default

bound = number('MEMOIRE_QWEN_TICK_TIMEOUT', 240)
kill_delay = number('MEMOIRE_QWEN_TICK_KILL_S', 10)
quiet = number('MEMOIRE_TICK_QUIET_S', 120, True)
since = number('MEMOIRE_TICK_SINCE_S', 172800)
sessions = Path(os.environ.get('MEMOIRE_QWEN_SESSIONS_DIR',
                             os.environ.get('QWEN_SESSIONS_DIR', str(Path.home() / '.qwen/projects'))))
adapter = Path(os.environ.get('MEMOIRE_CAPTURE_QWEN_BIN',
                            str(Path(sys.argv[1]) / 'memoire-capture-qwen-rollout.sh')))
if not shutil.which('jq'):
    skip('no_jq')
if not adapter.is_file() or not os.access(adapter, os.X_OK):
    skip('capture_bin_missing')
if not sessions.is_dir():
    skip('no_sessions_dir')

try:
    home.mkdir(parents=True, exist_ok=True)
    lockpath = home / 'capture-qwen-tick.lock'
    fd = os.open(lockpath, os.O_CREAT | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
    lock = os.fdopen(fd, 'w')
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
except BlockingIOError:
    skip('locked')
except OSError:
    skip('lock_unavailable')

state = home / 'state/qwen-tick'
try:
    state.mkdir(parents=True, exist_ok=True)
except OSError:
    skip('state_unavailable')
now, deadline = time.time(), time.monotonic() + bound
files = events = failed = timed_out = stopped = unchanged = 0
last_rc = 0
summary = re.compile(r'^memoire-capture-qwen-rollout: done: files=([0-9]+) failed_files=([0-9]+) events=([0-9]+) failed_events=([0-9]+)$')

try:
    candidates = []
    # Only inspect metadata here; the accepted adapter is the transcript reader.
    for path in sessions.glob('*/chats/*.jsonl'):
        try:
            info = path.stat()
            if path.is_file() and since >= now - info.st_mtime >= quiet:
                candidates.append((info.st_mtime_ns, str(path), info.st_size))
        except OSError:
            failed += 1
    for mtime, filename, size in sorted(candidates):
        try:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                stopped = 1
                break
            fingerprint = [mtime, size]
            marker = state / (hashlib.sha256(filename.encode()).hexdigest() + '.json')
            if marker.exists():
                try:
                    if json.loads(marker.read_text()) == fingerprint:
                        unchanged += 1
                        continue
                except (OSError, ValueError):
                    pass
            proc = subprocess.Popen([str(adapter), '--include-prompts', filename],
                                    stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                    stderr=subprocess.PIPE, start_new_session=True)
            try:
                _, error = proc.communicate(timeout=remaining)
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
                timed_out = stopped = 1
            # Arbitrary adapter diagnostics never reach disk, stdout or the journal.
            rows = [summary.fullmatch(line) for line in error.decode(errors='replace').splitlines()]
            rows = [row for row in rows if row]
            if len(rows) == 1:
                nfiles, nfailed, nevents, eventfailed = map(int, rows[0].groups())
                files += nfiles
                events += nevents
            else:
                nfailed = eventfailed = 1
            if last_rc != 0 or nfailed or eventfailed:
                failed += 1
            else:
                info = Path(filename).stat()
                if [info.st_mtime_ns, info.st_size] == fingerprint:
                    tmp = marker.with_suffix('.tmp')
                    tmp.write_text(json.dumps(fingerprint))
                    os.replace(tmp, marker)
            if stopped:
                break
        except (OSError, ValueError):
            failed += 1
except (OSError, ValueError):
    failed += 1
log(f'files={files} events={events} failed_files={failed} unchanged={unchanged} stopped={stopped} timed_out={timed_out} rc={last_rc}')
PYTHON
