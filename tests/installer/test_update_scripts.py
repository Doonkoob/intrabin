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
        self.assertTrue(all('--policy' in c and c[c.index('--policy') + 1] == 'missing' for c in calls if c[0] == 'pull'))

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


class ReleaseModeTest(unittest.TestCase):
    def run_release(self, fail=False, mode="prebuilt", image="", runner="", remote="https://github.com/Doonkoob/intrabin"):
        with tempfile.TemporaryDirectory() as tmp:
            task = Path(tmp)
            envfile = task / '.env'
            original = f'INTRABIN_VERSION=v0.1.0\nINTRABIN_IMAGE={image}\nINTRABIN_RUNNER_IMAGE={runner}\n'
            envfile.write_text(original)
            cli = (ROOT / 'scripts/intrabin-cli.sh').read_text()
            names = ['get_env', 'set_env', 'valid_tag', 'update_rollback', 'update_to_tag']
            definitions = []
            for name in names:
                match = re.search(r'^' + name + r'\(\) \{.*?^\}', cli, re.M | re.S)
                if name in ('get_env', 'valid_tag'):
                    match = re.search(r'^' + name + r'\(\) \{[^\n]*', cli, re.M)
                self.assertIsNotNone(match, name)
                definitions.append(match.group())
            script = '\n'.join(['set -euo pipefail', *definitions, r'''
ENV_FILE="$TASK_DIR/.env"
cd "$TASK_DIR"
mkdir scripts
printf '#!/bin/bash\n' > scripts/stub.sh
die() { echo "$*" >&2; exit 1; }
warn() { :; }; ok() { :; }; update_progress() { :; }; write_caddyfile() { :; }
running() { return 0; }
cmd_backup() { cp "$ENV_FILE" backup.env; }
git() {
  case "$1 $2" in
    'remote get-url') echo "$FAKE_REMOTE" ;;
    'rev-parse HEAD') echo oldcommit ;;
    'rev-parse --abbrev-ref') echo main ;;
  esac
}
compose_serial_images() {
  echo "$1" >> operations
  cp "$ENV_FILE" prepared.env
  [[ "$FAIL_PULL" != yes ]]
}
compose_serial_up() { echo restart >> operations; }
wait_healthy() { echo "$1" >> operations; }
if [[ -n "$UPDATE_MODE" ]]; then update_to_tag v0.2.4 yes "$UPDATE_MODE"; else update_to_tag v0.2.4; fi
'''])
            result = subprocess.run(['bash', '-c', script], env=dict(os.environ, TASK_DIR=tmp, FAIL_PULL='yes' if fail else 'no', UPDATE_MODE=mode, FAKE_REMOTE=remote), text=True, capture_output=True)
            return result, envfile.read_text(), (task / 'prepared.env').read_text(), (task / 'backup.env').read_text(), (task / 'operations').read_text(), original

    def test_prebuilt_switch_backs_up_and_pulls_both_release_images(self):
        result, current, prepared, backup, operations, original = self.run_release()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(backup, original)
        self.assertIn('INTRABIN_IMAGE=ghcr.io/doonkoob/intrabin:v0.2.4', current)
        self.assertIn('INTRABIN_RUNNER_IMAGE=ghcr.io/doonkoob/intrabin-runner:v0.2.4', prepared)
        self.assertEqual(operations.splitlines(), ['pull', 'restart', 'v0.2.4'])

    def test_failed_pull_restores_source_settings_without_restart(self):
        result, current, prepared, backup, operations, original = self.run_release(fail=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(current, original)
        self.assertEqual(backup, original)
        self.assertEqual(operations.splitlines(), ['pull'])

    def test_existing_source_install_defaults_to_prebuilt_without_extra_flags(self):
        result, current, _, backup, operations, original = self.run_release(mode='')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(backup, original)
        self.assertIn('INTRABIN_IMAGE=ghcr.io/doonkoob/intrabin:v0.2.4', current)
        self.assertEqual(operations.splitlines(), ['pull', 'restart', 'v0.2.4'])

    def test_default_pull_failure_keeps_old_source_install_running(self):
        result, current, _, backup, operations, original = self.run_release(mode='', fail=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(current, original)
        self.assertEqual(backup, original)
        self.assertEqual(operations.splitlines(), ['pull'])

    def test_explicit_build_switches_back_from_published_images(self):
        result, current, _, backup, operations, original = self.run_release(mode='build', image='ghcr.io/doonkoob/intrabin:v0.1.0', runner='ghcr.io/doonkoob/intrabin-runner:v0.1.0')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(backup, original)
        self.assertIn('INTRABIN_IMAGE=\n', current)
        self.assertIn('INTRABIN_RUNNER_IMAGE=\n', current)
        self.assertEqual(operations.splitlines(), ['build', 'restart', 'v0.2.4'])

    def test_custom_registry_is_preserved(self):
        result, current, _, _, operations, _ = self.run_release(mode='', image='registry.example:5000/custom/app:v0.1.0', runner='registry.example:5000/custom/runner:v0.1.0')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('INTRABIN_IMAGE=registry.example:5000/custom/app:v0.2.4', current)
        self.assertIn('INTRABIN_RUNNER_IMAGE=registry.example:5000/custom/runner:v0.2.4', current)
        self.assertEqual(operations.splitlines(), ['pull', 'restart', 'v0.2.4'])

    def test_automatic_source_switch_preserves_custom_runner_registry(self):
        result, current, _, _, operations, _ = self.run_release(mode='', runner='registry.example/custom/runner:v0.1.0')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('INTRABIN_IMAGE=ghcr.io/doonkoob/intrabin:v0.2.4', current)
        self.assertIn('INTRABIN_RUNNER_IMAGE=registry.example/custom/runner:v0.2.4', current)
        self.assertEqual(operations.splitlines(), ['pull', 'restart', 'v0.2.4'])

    def test_failed_explicit_build_restores_published_image_settings(self):
        result, current, _, backup, operations, original = self.run_release(mode='build', fail=True, image='ghcr.io/doonkoob/intrabin:v0.1.0', runner='ghcr.io/doonkoob/intrabin-runner:v0.1.0')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(current, original)
        self.assertEqual(backup, original)
        self.assertEqual(operations.splitlines(), ['build'])

    def test_non_github_source_keeps_build_mode(self):
        result, current, _, _, operations, _ = self.run_release(mode='', remote='https://git.example/team/intrabin')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('INTRABIN_IMAGE=\n', current)
        self.assertEqual(operations.splitlines(), ['build', 'restart', 'v0.2.4'])


class ArchiveFallbackTest(unittest.TestCase):
    def test_public_release_archive_is_loaded_and_verified(self):
        cli = (ROOT / 'scripts/intrabin-cli.sh').read_text()
        function = re.search(r'^compose_release_fallback\(\) \{.*?^\}', cli, re.M | re.S).group()
        with tempfile.TemporaryDirectory() as tmp:
            script = '\n'.join(['set -euo pipefail', function, r'''
get_env() {
  case "$1" in
    INTRABIN_IMAGE) echo ghcr.io/doonkoob/intrabin:v0.2.4 ;;
    INTRABIN_VERSION) echo v0.2.4 ;;
  esac
}
valid_tag() { return 0; }
git() { echo https://github.com/Doonkoob/intrabin; }
docker() {
  echo "$*" >> "$TASK_DIR/calls"
  case "$1 $2" in
    'info --format') echo aarch64 ;;
    'image load') cat > "$TASK_DIR/loaded" ;;
  esac
}
curl() {
  echo "$*" >> "$TASK_DIR/calls"
  while [[ "$1" != -o ]]; do shift; done
  printf 'saved-image' | gzip > "$2"
}
compose_release_fallback intrabin
if compose_release_fallback caddy; then exit 99; fi
'''])
            result = subprocess.run(['bash', '-c', script], env=dict(os.environ, TASK_DIR=tmp), capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((Path(tmp) / 'loaded').read_text(), 'saved-image')
            calls = (Path(tmp) / 'calls').read_text()
            self.assertIn('https://github.com/Doonkoob/intrabin/releases/download/v0.2.4/intrabin-linux-arm64.tar.gz', calls)
            self.assertIn('image inspect ghcr.io/doonkoob/intrabin:v0.2.4', calls)

    def test_bootstrap_runs_tagged_updater_without_checking_out_installation(self):
        with tempfile.TemporaryDirectory() as tmp:
            task = Path(tmp)
            install = task / 'install'; install.mkdir()
            (install / '.env').write_text('old-settings')
            (install / 'compose.yaml').write_text('services: {}')
            fake_git = task / 'git'
            fake_git.write_text(r'''#!/usr/bin/env bash
set -eu
[[ "$1" == -C ]]; shift 2
case "$1" in
  rev-parse) echo tagged-commit ;;
  fetch) : ;;
  show)
    case "$2" in
      tagged-commit:scripts/intrabin-cli.sh)
        cat <<'SCRIPT'
set -eu
source "$(dirname "${BASH_SOURCE[0]}")/compose-serial.sh"
[[ "$(cat "$INTRABIN_DIR/.env")" == old-settings ]]
[[ "$*" == 'update --to v0.2.4 --prebuilt' ]]
echo tagged-updater > "$INTRABIN_DIR/result"
SCRIPT
        ;;
      tagged-commit:scripts/compose-serial.sh) echo ': # matching tagged helper' ;;
      *) exit 2 ;;
    esac ;;
  *) exit 3 ;;
esac
''')
            fake_git.chmod(0o755)
            # Root is required only by the entrypoint; run the same body without that guard.
            bootstrap = (ROOT / 'scripts/update.sh').read_text().replace('[[ $EUID -eq 0 ]] || die "run as root"', ':')
            result = subprocess.run(['bash', '-c', bootstrap, 'update.sh', 'v0.2.4', str(install)], env=dict(os.environ, PATH=tmp + os.pathsep + os.environ['PATH']), capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((install / 'result').read_text().strip(), 'tagged-updater')
            self.assertEqual((install / '.env').read_text(), 'old-settings')


if __name__ == '__main__':
    unittest.main()
