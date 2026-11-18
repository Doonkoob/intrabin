"""Host-agent repair and lock handoff without touching the host's systemd units."""
import fcntl
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parents[2]


def function(name):
    source = (ROOT / 'scripts/intrabin-cli.sh').read_text()
    return re.search(r'^' + name + r'\(\) \{.*?^\}', source, re.M | re.S).group()


class UpdateAgentTest(unittest.TestCase):
    def test_repair_installs_watcher_and_periodic_recovery_without_consuming_request(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            install_dir = root / 'installation'
            (install_dir / 'run/update').mkdir(parents=True)
            request = install_dir / 'run/update/request'
            request.write_text('version=v0.2.9\n')
            units = root / 'units'; units.mkdir()
            script = '''set -euo pipefail
source "$REPO_ROOT/scripts/update-agent.sh"
chown() { :; }
systemctl() { echo "$*" >> "$TASK_DIR/calls"; }
install_update_agent_units "$TASK_DIR/installation" "$TASK_DIR/units"
'''
            result = subprocess.run(['bash', '-c', script], env=dict(os.environ, REPO_ROOT=str(ROOT), TASK_DIR=tmp), capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(request.read_text(), 'version=v0.2.9\n')
            service = (units / 'intrabin-update.service').read_text()
            timer = (units / 'intrabin-update.timer').read_text()
            watcher = (units / 'intrabin-update.path').read_text()
            self.assertIn(f'Environment="INTRABIN_DIR={install_dir}"', service)
            self.assertIn('ExecStart=/usr/local/bin/intrabin apply-update', service)
            self.assertNotIn('Requires=docker.service', service, 'idle timer checks must not start a stopped Docker daemon')
            self.assertIn('OnUnitInactiveSec=30s', timer)
            self.assertIn('Unit=intrabin-update.service', timer)
            self.assertIn(f'PathExists={request}', watcher)
            calls = (root / 'calls').read_text()
            self.assertIn('reset-failed intrabin-update.path intrabin-update.service', calls)
            self.assertIn('enable --now intrabin-update.path intrabin-update.timer', calls)
            self.assertIn('restart intrabin-update.path', calls)

    def test_repair_rejects_symlinked_shared_directory(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp); (root/'other').mkdir(); (root/'installation').mkdir(); (root/'installation/run').symlink_to(root/'other')
            script='source "$REPO_ROOT/scripts/update-agent.sh"\ninstall_update_agent_units "$TASK_DIR/installation" "$TASK_DIR"'
            result=subprocess.run(['bash','-c',script],env=dict(os.environ,REPO_ROOT=str(ROOT),TASK_DIR=tmp),capture_output=True,text=True)
            self.assertNotEqual(result.returncode,0)
            self.assertFalse((root/'intrabin-update.service').exists())

    def test_agent_info_does_not_claim_missing_units_are_ready(self):
        for ready in (0,1):
            script='\n'.join(['set -euo pipefail',function('write_agent_info'),'''UPDATE_DIR="$TASK_DIR"
get_env() { echo ''; }
update_agent_ready() { [[ "$READY" == 1 ]]; }
write_shared() { cat > "$TASK_DIR/$1"; }
write_agent_info
'''])
            with tempfile.TemporaryDirectory() as tmp:
                result=subprocess.run(['bash','-c',script],env=dict(os.environ,TASK_DIR=tmp,READY=str(ready)),capture_output=True,text=True)
                self.assertEqual(result.returncode,0,result.stderr)
                self.assertEqual(json.loads((Path(tmp)/'agent.json').read_text())['agent'],ready)

    @unittest.skipUnless(shutil.which('flock'), 'real flock handoff is exercised in Linux CI')
    def test_busy_agent_waits_for_manual_update_then_consumes_request_once(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp); shared=root/'run/update';shared.mkdir(parents=True)
            request=shared/'request';request.write_text('version=v0.2.9\n')
            script='\n'.join(['set -euo pipefail',function('cmd_apply_update'),'''INSTALL_DIR="$TASK_DIR"
UPDATE_DIR="$TASK_DIR/run/update"
die() { echo "$*" >&2; exit 1; }
get_env() { echo v0.2.7; }
now_utc() { echo 2026-11-17T00:00:00Z; }
valid_tag() { return 0; }
newer_than() { return 0; }
update_status() { :; }
write_agent_info() { :; }
finish_agent_update() { :; }
write_shared() { cat > "$UPDATE_DIR/$1"; }
update_to_tag() { echo "$1" >> "$TASK_DIR/started"; }
cmd_apply_update
'''])
            with (root/'run/.update.lock').open('w') as lock:
                fcntl.flock(lock,fcntl.LOCK_EX)
                process=subprocess.Popen(['bash','-c',script],env=dict(os.environ,TASK_DIR=tmp),stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
                try:
                    time.sleep(.2)
                    self.assertIsNone(process.poll(),'agent exited immediately while lock was held')
                    self.assertTrue(request.exists())
                    self.assertFalse((root/'started').exists())
                finally:
                    fcntl.flock(lock,fcntl.LOCK_UN)
                    out,err=process.communicate(timeout=5)
                self.assertEqual(process.returncode,0,out+err)
                self.assertFalse(request.exists())
                self.assertEqual((root/'started').read_text().splitlines(),['v0.2.9'])


if __name__ == '__main__':
    unittest.main()
