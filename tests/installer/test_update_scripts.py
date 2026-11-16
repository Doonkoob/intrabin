"""Exercise the real deployment helpers against a Docker stub: never contact a daemon."""
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class SerialUpdateTest(unittest.TestCase):
    def run_update(self, mode="build", fail="", services="intrabin\nrunner\ncaddy"):
        with tempfile.TemporaryDirectory() as tmp:
            task = Path(tmp)
            docker = task / "docker"
            docker.write_text("""#!/usr/bin/env python3
import json, os, pathlib, sys, time
root=pathlib.Path(os.environ['FAKE_DIR'])
a=sys.argv[1:]
assert a[:3] == ['compose','--parallel','1'], a
assert os.environ['COMPOSE_PARALLEL_LIMIT'] == '1'
a=a[3:]
if a == ['config','--services']:
    print(os.environ['FAKE_SERVICES']); sys.exit(0)
lock=root/'active'
try: fd=os.open(lock,os.O_CREAT|os.O_EXCL|os.O_WRONLY)
except FileExistsError: sys.exit(88)
try:
    with open(root/'calls','a') as f: f.write(json.dumps(a)+'\\n')
    time.sleep(.02)
    if ' '.join(a)==os.environ.get('FAKE_FAIL'): sys.exit(42)
finally:
    os.close(fd); lock.unlink()
""")
            docker.chmod(0o755)
            # Load only function definitions: the root-only management entrypoint is never run.
            cli = (ROOT / "scripts/intrabin-cli.sh").read_text()
            names = ["json_str", "now_utc", "update_status", "update_progress",
                     "compose_serial_progress", "finish_agent_update"]
            functions = []
            for name in names:
                # json_str and now_utc fit on a single line; the other function bodies close
                # at column zero, while their nested blocks are indented.
                match = re.search(r"^" + name + r"\(\) \{.*?^\}", cli, re.M | re.S)
                if name in ("json_str", "now_utc"):
                    match = re.search(r"^" + name + r"\(\) \{[^\n]*", cli, re.M)
                self.assertIsNotNone(match, name)
                functions.append(match.group())
            script = '\n'.join([
                'set -euo pipefail',
                'source "$REPO_ROOT/scripts/compose-serial.sh"',
                *functions,
                'write_shared() { cat >> "$FAKE_DIR/status"; }',
                'UPDATE_TARGET=v0.2.0; UPDATE_FROM=v0.1.0; UPDATE_STARTED_AT=2026-11-16T12:00:00Z',
                'UPDATE_PROGRESS=0; UPDATE_STAGE=queued; UPDATE_SERVICE=""',
                "trap 'finish_agent_update \"$?\"' EXIT",
                'update_progress fetching 5',
                'update_progress backup 15',
                'update_progress backup 30',
                f'compose_serial_images {mode}',
                'compose_serial_up',
                'update_progress health 95',
            ])
            env = dict(os.environ, REPO_ROOT=str(ROOT), FAKE_DIR=tmp,
                       FAKE_SERVICES=services, FAKE_FAIL=fail,
                       PATH=tmp + os.pathsep + os.environ['PATH'])
            result = subprocess.run(['bash', '-c', script], env=env, capture_output=True, text=True)
            calls = [json.loads(row) for row in (task / 'calls').read_text().splitlines()]
            status = [json.loads(row) for row in (task / 'status').read_text().splitlines()]
            return result, calls, status

    def test_source_builds_and_restarts_one_service_at_a_time(self):
        result, calls, status = self.run_update()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([c[-1] for c in calls if c[0] == 'build'], ['intrabin', 'runner', 'caddy'])
        up = [c for c in calls if c[0] == 'up']
        self.assertEqual([c[-1] for c in up], ['intrabin', 'runner', 'caddy'])
        self.assertTrue(all('--no-build' in c and c[c.index('--pull') + 1] == 'never' for c in up))
        self.assertEqual(status[-1]['state'], 'succeeded')
        self.assertEqual(status[-1]['progress'], 100)
        self.assertEqual(status[-1]['stage'], 'complete')
        self.assertEqual([s['progress'] for s in status], sorted(s['progress'] for s in status))
        self.assertTrue(any(s['service'] == 'runner' and s['stage'] == 'building' for s in status))
        self.assertEqual(next(s['progress'] for s in status if s['stage'] == 'health'), 95)

    def test_release_pulls_without_building(self):
        result, calls, _ = self.run_update(mode='pull')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(any(c[0] == 'build' for c in calls))
        self.assertEqual([c[-1] for c in calls if c[0] == 'pull'], ['intrabin', 'runner', 'caddy'])
        self.assertEqual([c[0] for c in calls], ['pull'] * 3 + ['up'] * 3)

    def test_failed_build_stops_before_other_images_or_restarts(self):
        result, calls, status = self.run_update(fail='build runner')
        self.assertEqual(result.returncode, 42, result.stderr)
        self.assertFalse(any(c[-1] == 'caddy' or c[0] == 'up' for c in calls))
        self.assertEqual(status[-1]['state'], 'failed')
        self.assertEqual(status[-1]['service'], 'runner')
        self.assertEqual(status[-1]['stage'], 'building')
        self.assertLess(status[-1]['progress'], 100)
        self.assertEqual(status[-1]['progress'], status[-2]['progress'])

    def test_disabled_profile_services_are_not_enabled_by_the_helpers(self):
        result, calls, _ = self.run_update(services='intrabin')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(all(c[-1] == 'intrabin' for c in calls))


if __name__ == '__main__':
    unittest.main()
