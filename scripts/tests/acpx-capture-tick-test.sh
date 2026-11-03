#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Michel Tomas
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
export ACPX_TEST_ROOT="$ROOT"
exec python3 - <<'PYTHON'
import fcntl
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import unittest

ROOT=Path(os.environ['ACPX_TEST_ROOT'])
TICK=ROOT/'scripts/memoire-capture-acpx-tick.sh'
INSTALLER=ROOT/'scripts/capture-acpx-tick-timer.sh'
SUMMARY='memoire-capture-acpx: done: records=2 skipped=5 failed_records=0 failed_events=0'

class AcpxTickTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory(dir=os.environ.get('TMPDIR'))
        self.work=Path(self.temp.name)
        self.home=self.work/'home';self.home.mkdir()
        self.mh=self.work/'memoire'
        self.sessions=self.work/'sessions dir';self.sessions.mkdir()
        self.archive=self.work/'archive';self.archive.mkdir()
        self.adapter=self.work/'adapter'
        self.adapter.write_text(r'''#!/usr/bin/env python3
import json,os,sys,time
from pathlib import Path
p=Path(os.environ['CALLS'])
p.write_text((p.read_text() if p.exists() else '')+json.dumps(sys.argv[1:])+'\n')
print('synthetic stdout sentinel')
if os.environ.get('HANG'):
 import signal
 signal.signal(signal.SIGTERM,signal.SIG_IGN)
 time.sleep(20)
print('memoire-capture-acpx: skip (unchanged): synthetic private diagnostic',file=sys.stderr)
if not os.environ.get('NO_SUMMARY'):
 print(os.environ.get('SUMMARY_LINE','memoire-capture-acpx: done: records=2 skipped=5 failed_records=0 failed_events=0'),file=sys.stderr)
 if os.environ.get('DOUBLE_SUMMARY'):
  print('memoire-capture-acpx: done: records=9 skipped=9 failed_records=0 failed_events=0',file=sys.stderr)
sys.exit(int(os.environ.get('RC','0')))
''')
        self.adapter.chmod(0o755)
        self.env={k:v for k,v in os.environ.items() if not k.startswith('MEMOIRE_') and not k.startswith('ACPX_')}
        self.env.update(HOME=str(self.home),MEMOIRE_HOME=str(self.mh),
                        MEMOIRE_ACPX_SESSIONS_DIR=str(self.sessions),
                        MEMOIRE_ACPX_ARCHIVE_DIR=str(self.archive),
                        MEMOIRE_CAPTURE_ACPX_BIN=str(self.adapter),CALLS=str(self.work/'calls'),
                        MEMOIRE_CAPTURE_ACPX='on')

    def tearDown(self):
        self.temp.cleanup()

    def run_tick(self, **changes):
        env={**self.env,**changes}
        for k in [k for k,v in changes.items() if v is None]:
            env.pop(k,None)
        logfile=self.mh/'log/capture-acpx-tick.log'
        before=logfile.stat().st_size if logfile.exists() else 0
        r=subprocess.run(['bash',str(TICK)],env=env,capture_output=True,text=True,timeout=8)
        self.assertEqual(r.returncode,0)
        self.assertEqual(r.stdout,'')
        self.assertEqual(r.stderr,'')
        with logfile.open('rb') as f:
            f.seek(before);lines=f.read().decode().splitlines()
        self.assertEqual(len(lines),1)
        return lines[0].split(' ',1)[1]

    def calls(self):
        p=self.work/'calls'
        return [json.loads(l) for l in p.read_text().splitlines()] if p.exists() else []

    def test_default_and_disabled_switch(self):
        self.env.pop('MEMOIRE_CAPTURE_ACPX')
        self.assertEqual('tick_acpx skipped=acpx_switch_off',self.run_tick())
        for value in ['off','0','false','']:
            self.assertEqual('tick_acpx skipped=acpx_switch_off',self.run_tick(MEMOIRE_CAPTURE_ACPX=value))
        self.assertEqual(self.calls(),[])

    def test_gate_file_is_never_sourced(self):
        self.mh.mkdir()
        (self.mh/'capture-tick.env').write_text('MEMOIRE_CAPTURE_ACPX=on\n')
        self.env.pop('MEMOIRE_CAPTURE_ACPX')
        self.assertEqual('tick_acpx skipped=acpx_switch_off',self.run_tick())
        self.assertEqual(self.calls(),[])

    def test_global_kill_and_unrecognised(self):
        self.assertEqual('tick_acpx skipped=capture_disabled',self.run_tick(MEMOIRE_CAPTURE='off'))
        self.assertEqual('tick_acpx skipped=acpx_switch_unrecognised',self.run_tick(MEMOIRE_CAPTURE_ACPX='typo'))
        self.assertEqual(self.calls(),[])

    def test_success_invocation_and_log_line(self):
        self.assertEqual('tick_acpx records=2 skipped=5 failed_records=0 failed_events=0 summary=1 archive=1 timed_out=0 rc=0',
                         self.run_tick())
        self.assertEqual(self.calls(),[['--scan','--sessions-dir',str(self.sessions),
                                        '--archive-dir',str(self.archive),
                                        '--since','172800 seconds ago','--quiet-for','3600']])
        text=(self.mh/'log/capture-acpx-tick.log').read_text()
        self.assertNotIn('private diagnostic',text)
        self.assertNotIn('stdout sentinel',text)

    def test_one_adapter_call_per_tick_no_own_markers(self):
        self.run_tick();self.run_tick()
        self.assertEqual(len(self.calls()),2)
        self.assertFalse((self.mh/'state').exists())

    def test_archive_absent_or_empty_is_omitted(self):
        self.assertIn(' archive=0 ',self.run_tick(MEMOIRE_ACPX_ARCHIVE_DIR=str(self.work/'absent')))
        self.assertIn(' archive=0 ',self.run_tick(MEMOIRE_ACPX_ARCHIVE_DIR=''))
        for call in self.calls():
            self.assertNotIn('--archive-dir',call)

    def test_default_archive_is_host_scoped(self):
        host=socket.gethostname().split('.')[0]
        default=self.home/'Sync/archives/acpx-history'/host/'sessions'
        self.assertIn(' archive=0 ',self.run_tick(MEMOIRE_ACPX_ARCHIVE_DIR=None))
        default.mkdir(parents=True)
        self.assertIn(' archive=1 ',self.run_tick(MEMOIRE_ACPX_ARCHIVE_DIR=None))
        self.assertEqual(self.calls()[-1][3:5],['--archive-dir',str(default)])

    def test_default_sessions_dir_fallbacks(self):
        self.env.pop('MEMOIRE_ACPX_SESSIONS_DIR')
        self.assertEqual('tick_acpx skipped=no_sessions_dir',self.run_tick())
        alt=self.work/'alt';alt.mkdir()
        self.run_tick(ACPX_SESSIONS_DIR=str(alt))
        self.assertEqual(self.calls()[-1][1:3],['--sessions-dir',str(alt)])
        default=self.home/'.acpx/sessions';default.mkdir(parents=True)
        self.run_tick()
        self.assertEqual(self.calls()[-1][1:3],['--sessions-dir',str(default)])

    def test_window_overrides_and_invalid_values(self):
        self.run_tick(MEMOIRE_TICK_SINCE_S='600',MEMOIRE_ACPX_TICK_QUIET_S='0')
        self.assertEqual(self.calls()[-1][-4:],['--since','600 seconds ago','--quiet-for','0'])
        self.run_tick(MEMOIRE_TICK_SINCE_S='0',MEMOIRE_ACPX_TICK_QUIET_S='x')
        self.assertEqual(self.calls()[-1][-4:],['--since','172800 seconds ago','--quiet-for','3600'])

    def test_adapter_failure_is_reported(self):
        log=self.run_tick(RC='1',SUMMARY_LINE='memoire-capture-acpx: done: records=1 skipped=0 failed_records=1 failed_events=2')
        self.assertEqual('tick_acpx records=1 skipped=0 failed_records=1 failed_events=2 summary=1 archive=1 timed_out=0 rc=1',log)

    def test_missing_or_ambiguous_summary(self):
        self.assertEqual('tick_acpx records=0 skipped=0 failed_records=0 failed_events=0 summary=0 archive=1 timed_out=0 rc=3',
                         self.run_tick(NO_SUMMARY='1',RC='3'))
        self.assertIn(' summary=0 ',self.run_tick(DOUBLE_SUMMARY='1'))
        self.assertIn(' summary=0 ',self.run_tick(SUMMARY_LINE='prefix '+SUMMARY))

    def test_independent_lock(self):
        self.mh.mkdir()
        with (self.mh/'capture-acpx-tick.lock').open('w') as f:
            fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)
            self.assertEqual('tick_acpx skipped=locked',self.run_tick())
        self.assertEqual(self.calls(),[])

    def test_timeout_term_kill(self):
        start=time.monotonic()
        log=self.run_tick(HANG='1',MEMOIRE_ACPX_TICK_TIMEOUT='1',MEMOIRE_ACPX_TICK_KILL_S='1')
        self.assertLess(time.monotonic()-start,5)
        self.assertIn(' summary=0 ',log)
        self.assertTrue(log.endswith('timed_out=1 rc=137'),log)

    def test_invalid_bounds_fall_back(self):
        self.assertIn(' rc=0',self.run_tick(MEMOIRE_ACPX_TICK_TIMEOUT='0',MEMOIRE_ACPX_TICK_KILL_S='invalid'))

    def test_missing_adapter_skips(self):
        self.assertEqual('tick_acpx skipped=capture_bin_missing',
                         self.run_tick(MEMOIRE_CAPTURE_ACPX_BIN=str(self.work/'absent')))
        self.adapter.chmod(0o644)
        self.assertEqual('tick_acpx skipped=capture_bin_missing',self.run_tick())

    def test_private_log(self):
        self.run_tick()
        self.assertEqual((self.mh/'log/capture-acpx-tick.log').stat().st_mode & 0o077,0)

    def test_installer_interface_and_regeneration(self):
        bindir=self.work/'bin';bindir.mkdir()
        systemctl=bindir/'systemctl'
        systemctl.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$SYSTEMCTL_LOG"\n')
        systemctl.chmod(0o755)
        env={**self.env,'PATH':str(bindir)+':'+self.env['PATH'],
             'XDG_CONFIG_HOME':str(self.work/'config'),
             'SYSTEMCTL_LOG':str(self.work/'systemctl.log'),
             'MEMOIRE_ACPX_TICK_TIMEOUT':'2','MEMOIRE_ACPX_TICK_KILL_S':'1'}
        r=subprocess.run(['bash',str(INSTALLER),'install'],env=env,capture_output=True,text=True)
        self.assertEqual(r.returncode,0,r.stderr)
        units=self.work/'config/systemd/user'
        service=(units/'memoire-capture-acpx-tick.service').read_text()
        timer=(units/'memoire-capture-acpx-tick.timer').read_text()
        self.assertIn('Type=oneshot',service)
        self.assertIn('TimeoutStartSec=63s',service)
        self.assertIn('EnvironmentFile=-%h/.memoire/capture-tick.env',service)
        self.assertIn('ExecStart='+str(TICK),service)
        self.assertIn('Nice=10',service)
        self.assertIn('OnCalendar=*:3/10',timer)
        self.assertIn('RandomizedDelaySec=60',timer)
        self.assertIn('Persistent=true',timer)
        self.assertIn('WantedBy=timers.target',timer)
        log=(self.work/'systemctl.log').read_text()
        self.assertIn('--user daemon-reload',log)
        self.assertIn('--user enable --now memoire-capture-acpx-tick.timer',log)
        for command in ['start','stop','status','run-once','uninstall']:
            r=subprocess.run(['bash',str(INSTALLER),command],env=env,capture_output=True,text=True)
            self.assertEqual(r.returncode,0,command)
        self.assertFalse((units/'memoire-capture-acpx-tick.service').exists())
        self.assertFalse((units/'memoire-capture-acpx-tick.timer').exists())
        env['MEMOIRE_ACPX_TICK_TIMEOUT']='0'
        r=subprocess.run(['bash',str(INSTALLER),'install'],env=env,capture_output=True,text=True)
        self.assertEqual(r.returncode,0)
        self.assertIn('TimeoutStartSec=301s',(units/'memoire-capture-acpx-tick.service').read_text())
        r=subprocess.run(['bash',str(INSTALLER),'bogus'],env=env,capture_output=True,text=True)
        self.assertNotEqual(r.returncode,0)

    def test_installer_refuses_symlinked_unit(self):
        units=self.work/'config/systemd/user';units.mkdir(parents=True)
        (units/'memoire-capture-acpx-tick.service').symlink_to(self.work/'elsewhere')
        bindir=self.work/'bin';bindir.mkdir()
        (bindir/'systemctl').write_text('#!/bin/sh\nexit 0\n');(bindir/'systemctl').chmod(0o755)
        env={**self.env,'PATH':str(bindir)+':'+self.env['PATH'],'XDG_CONFIG_HOME':str(self.work/'config')}
        r=subprocess.run(['bash',str(INSTALLER),'install'],env=env,capture_output=True,text=True)
        self.assertNotEqual(r.returncode,0)
        self.assertFalse((self.work/'elsewhere').exists())

unittest.main()
PYTHON
