#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
export QWEN_TEST_ROOT="$ROOT"
exec python3 - <<'PYTHON'
import fcntl
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import time
import unittest

ROOT=Path(os.environ['QWEN_TEST_ROOT'])
TICK=ROOT/'scripts/memoire-capture-qwen-tick.sh'
INSTALLER=ROOT/'scripts/capture-qwen-tick-timer.sh'

class QwenTickTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory(dir=os.environ.get('TMPDIR'))
        self.work=Path(self.temp.name)
        self.home=self.work/'home';self.home.mkdir()
        self.mh=self.work/'memoire'
        self.sessions=self.work/'sessions'
        chat=self.sessions/'project with spaces/chats';chat.mkdir(parents=True)
        self.file=chat/'synthetic-session.jsonl';self.file.touch()
        os.utime(self.file,(time.time()-300,time.time()-300))
        self.adapter=self.work/'adapter'
        self.adapter.write_text(r'''#!/usr/bin/env python3
import os,sys,time
from pathlib import Path
p=Path(os.environ['CALLS']);entry=Path(sys.argv[2]).name+'\n';p.write_text((p.read_text() if p.exists() else '')+entry)
print('synthetic stdout sentinel')
assert sys.argv[1]=='--include-prompts' and Path(sys.argv[2]).is_file()
if os.environ.get('HANG'):
 import signal
 signal.signal(signal.SIGTERM,signal.SIG_IGN)
 time.sleep(20)
if os.environ.get('DELETE_FIRST') and Path(sys.argv[2]).name=='synthetic-session.jsonl':
 Path(sys.argv[2]).unlink()
if os.environ.get('CHANGE'):
 Path(sys.argv[2]).touch()
print('synthetic private diagnostic must not be logged',file=sys.stderr)
if not os.environ.get('NO_SUMMARY'):
 print('memoire-capture-qwen-rollout: done: files=1 failed_files=0 events=3 failed_events=0',file=sys.stderr)
sys.exit(1 if os.environ.get('FAIL') else 0)
''')
        self.adapter.chmod(0o755)
        self.env={k:v for k,v in os.environ.items() if not k.startswith('MEMOIRE_') and not k.startswith('QWEN_')}
        self.env.update(HOME=str(self.home),MEMOIRE_HOME=str(self.mh),
                        MEMOIRE_QWEN_SESSIONS_DIR=str(self.sessions),
                        MEMOIRE_CAPTURE_QWEN_BIN=str(self.adapter),CALLS=str(self.work/'calls'),
                        MEMOIRE_CAPTURE_QWEN='on')

    def tearDown(self):
        self.temp.cleanup()

    def run_tick(self, **changes):
        env={**self.env,**changes}
        logfile=self.mh/'log/capture-qwen-tick.log'
        before=logfile.stat().st_size if logfile.exists() else 0
        r=subprocess.run(['bash',str(TICK)],env=env,capture_output=True,text=True,timeout=8)
        self.assertEqual(r.returncode,0)
        self.assertEqual(r.stdout,'')
        with logfile.open('rb') as f:
            f.seek(before);lines=f.read().decode().splitlines()
        self.assertEqual(len(lines),1)
        return lines[0].split(' ',1)[1]

    def calls(self):
        p=self.work/'calls'
        return len(p.read_text().splitlines()) if p.exists() else 0

    def test_default_and_disabled_switch(self):
        self.env.pop('MEMOIRE_CAPTURE_QWEN')
        self.assertEqual('tick_qwen skipped=qwen_switch_off',self.run_tick())
        for value in ['off','0','false','']:
            self.assertEqual('tick_qwen skipped=qwen_switch_off',self.run_tick(MEMOIRE_CAPTURE_QWEN=value))
        self.assertEqual(self.calls(),0)

    def test_gate_file_is_never_sourced(self):
        self.mh.mkdir()
        (self.mh/'capture-tick.env').write_text('MEMOIRE_CAPTURE_QWEN=on\n')
        self.env.pop('MEMOIRE_CAPTURE_QWEN')
        self.assertEqual('tick_qwen skipped=qwen_switch_off',self.run_tick())
        self.assertEqual(self.calls(),0)

    def second_file(self):
        second=self.file.with_name('second-session.jsonl');second.touch()
        os.utime(second,(time.time()-200,time.time()-200))
        return second

    def test_two_files_oldest_first(self):
        second=self.second_file()
        self.assertIn(' files=2 ',self.run_tick())
        self.assertEqual((self.work/'calls').read_text().splitlines(),
                         [self.file.name,second.name])

    def test_post_read_stat_race_does_not_abort_later_file(self):
        second=self.second_file()
        self.assertIn('failed_files=1',self.run_tick(DELETE_FIRST='1'))
        self.assertEqual((self.work/'calls').read_text().splitlines(),
                         [self.file.name,second.name])
        self.assertEqual(len(list((self.mh/'state/qwen-tick').glob('*.json'))),1)

    def test_global_kill_and_unrecognised(self):
        self.assertIn('skipped=capture_disabled',self.run_tick(MEMOIRE_CAPTURE='off'))
        self.assertEqual('tick_qwen skipped=qwen_switch_unrecognised',self.run_tick(MEMOIRE_CAPTURE_QWEN='typo'))
        self.assertEqual(self.calls(),0)

    def test_success_unchanged_append_and_private_state(self):
        self.assertIn('files=1 events=3 failed_files=0',self.run_tick())
        self.assertIn('unchanged=1',self.run_tick())
        self.assertEqual(self.calls(),1)
        self.file.write_text('synthetic appended fixture')
        os.utime(self.file,(time.time()-200,time.time()-200))
        self.assertIn('files=1 events=3 failed_files=0',self.run_tick())
        self.assertEqual(self.calls(),2)
        markers=list((self.mh/'state/qwen-tick').glob('*.json'))
        self.assertEqual(len(markers),1)
        self.assertEqual(stat.S_IMODE(markers[0].stat().st_mode),0o600)
        self.assertFalse(list((self.mh/'state/qwen-tick').glob('*.tmp')))
        self.assertNotIn('private diagnostic',(self.mh/'log/capture-qwen-tick.log').read_text())

    def test_quiet_and_since_filters(self):
        self.file.touch()
        self.assertIn(' files=0 ',self.run_tick())
        os.utime(self.file,(time.time()-500000,time.time()-500000))
        self.assertIn(' files=0 ',self.run_tick())
        self.assertEqual(self.calls(),0)

    def test_failed_file_retries(self):
        self.assertIn('failed_files=1',self.run_tick(FAIL='1'))
        self.assertIn('failed_files=0',self.run_tick())
        self.assertEqual(self.calls(),2)

    def test_changed_during_read_remains_eligible(self):
        self.run_tick(CHANGE='1')
        self.assertEqual(list((self.mh/'state/qwen-tick').glob('*.json')),[])

    def test_missing_summary_is_failure_not_success(self):
        self.assertIn('failed_files=1',self.run_tick(NO_SUMMARY='1'))
        self.assertEqual(list((self.mh/'state/qwen-tick').glob('*.json')),[])

    def test_independent_lock(self):
        self.mh.mkdir()
        with (self.mh/'capture-qwen-tick.lock').open('w') as f:
            fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)
            self.assertIn('skipped=locked',self.run_tick())
        self.assertEqual(self.calls(),0)

    def test_timeout_term_kill_retry(self):
        start=time.monotonic()
        log=self.run_tick(HANG='1',MEMOIRE_QWEN_TICK_TIMEOUT='1',MEMOIRE_QWEN_TICK_KILL_S='1')
        self.assertLess(time.monotonic()-start,5)
        self.assertIn('timed_out=1 rc=137',log)
        self.assertEqual(list((self.mh/'state/qwen-tick').glob('*.json')),[])

    def test_invalid_bounds_fall_back(self):
        self.assertIn('events=3',self.run_tick(MEMOIRE_QWEN_TICK_TIMEOUT='0',MEMOIRE_QWEN_TICK_KILL_S='invalid'))

    def test_absent_sessions_skips(self):
        self.assertIn('skipped=no_sessions_dir',self.run_tick(MEMOIRE_QWEN_SESSIONS_DIR=str(self.work/'absent')))

    def test_installer_interface_and_regeneration(self):
        bindir=self.work/'bin';bindir.mkdir()
        systemctl=bindir/'systemctl'
        systemctl.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$SYSTEMCTL_LOG"\n')
        systemctl.chmod(0o755)
        env={**self.env,'PATH':str(bindir)+':'+self.env['PATH'],
             'XDG_CONFIG_HOME':str(self.work/'config'),
             'SYSTEMCTL_LOG':str(self.work/'systemctl.log'),
             'MEMOIRE_QWEN_TICK_TIMEOUT':'2','MEMOIRE_QWEN_TICK_KILL_S':'1'}
        r=subprocess.run(['bash',str(INSTALLER),'install'],env=env,capture_output=True,text=True)
        self.assertEqual(r.returncode,0)
        units=self.work/'config/systemd/user'
        service=(units/'memoire-capture-qwen-tick.service').read_text()
        timer=(units/'memoire-capture-qwen-tick.timer').read_text()
        self.assertIn('TimeoutStartSec=63s',service)
        self.assertIn('EnvironmentFile=-%h/.memoire/capture-tick.env',service)
        self.assertIn(str(TICK),service)
        self.assertIn('OnCalendar=*:9/10',timer)
        self.assertIn('Persistent=true',timer)
        for command in ['start','stop','status','run-once','uninstall']:
            r=subprocess.run(['bash',str(INSTALLER),command],env=env,capture_output=True,text=True)
            self.assertEqual(r.returncode,0)
        self.assertFalse((units/'memoire-capture-qwen-tick.service').exists())
        env['MEMOIRE_QWEN_TICK_TIMEOUT']='0'
        r=subprocess.run(['bash',str(INSTALLER),'install'],env=env,capture_output=True,text=True)
        self.assertEqual(r.returncode,0)
        self.assertIn('TimeoutStartSec=301s',(units/'memoire-capture-qwen-tick.service').read_text())

unittest.main()
PYTHON
