#!/usr/bin/env python3
"""Verify isolated login lifecycle without a Docker daemon or real secrets."""

import fcntl
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
STOP = ["stop", "-t", "10", "antigravity-bridge"]
LOGIN = ["run", "--rm", "--no-deps", "antigravity-bridge", "login"]
VERIFY = ["run", "--rm", "--no-deps", "antigravity-bridge", "verify-login"]
START = ["up", "-d", "--no-deps", "antigravity-bridge"]
SMOKE = ["exec", "-T", "antigravity-bridge", "/usr/local/bin/antigravity-smoke"]
DRIVER = r'''
import json
import os
from pathlib import Path
import sys
root = Path(os.environ["BRIDGE_TEST_ROOT"])
scenario = json.loads((root / "scenario.json").read_text())
args = sys.argv[1:]
with (root / "commands.jsonl").open("a") as output:
    output.write(json.dumps(args) + "\n")
if args == ["ps", "-q", "antigravity-bridge"]:
    print("synthetic-bridge")
elif args == ["inspect", "-f", "{{.State.Running}}", "synthetic-bridge"]:
    print("true" if scenario.get("still_running") else "false")
elif args[-1:] == ["login"]:
    sys.exit(scenario.get("login_exit", 0))
elif args[-1:] == ["verify-login"]:
    sys.exit(scenario.get("verify_exit", 0))
elif args[-1:] == ["/usr/local/bin/antigravity-smoke"]:
    sys.exit(scenario.get("smoke_exit", 0))
'''


class AntigravityLoginTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="antigravity-login-test-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        scripts = self.root / "scripts"
        scripts.mkdir()
        self.script = scripts / "antigravity-login.sh"
        shutil.copyfile(ROOT / "scripts/antigravity-login.sh", self.script)
        self.script.chmod(0o700)
        for path in (scripts / "compose.sh", scripts / "docker"):
            path.write_text(f"#!{sys.executable}\n" + DRIVER)
            path.chmod(0o700)
        self.env = dict(os.environ, BRIDGE_TEST_ROOT=str(self.root))
        self.env["PATH"] = str(scripts) + os.pathsep + os.environ.get("PATH", "")
        self.scenario()

    def scenario(self, **values):
        (self.root / "scenario.json").write_text(json.dumps(values))
        (self.root / "commands.jsonl").write_text("")

    def run_login(self):
        return subprocess.run([str(self.script)], env=self.env, text=True,
                              capture_output=True, timeout=10)

    def commands(self):
        return [json.loads(line) for line in (self.root / "commands.jsonl").read_text().splitlines()]

    def test_login_and_new_session_verification_precede_start(self):
        result = self.run_login()
        self.assertEqual(result.returncode, 0, result.stderr)
        commands = self.commands()
        self.assertLess(commands.index(STOP), commands.index(LOGIN))
        self.assertLess(commands.index(LOGIN), commands.index(VERIFY))
        self.assertLess(commands.index(VERIFY), commands.index(START))
        self.assertLess(commands.index(START), commands.index(SMOKE))
        self.assertEqual(commands.count(STOP), 1)
        self.assertNotIn("codex-compat", json.dumps(commands))
        self.assertEqual((self.root / ".antigravity-login.lock").stat().st_mode & 0o777, 0o600)

    def test_login_failure_does_not_start_service(self):
        self.scenario(login_exit=1)
        self.assertNotEqual(self.run_login().returncode, 0)
        self.assertNotIn(VERIFY, self.commands())
        self.assertNotIn(START, self.commands())

    def test_keyring_persistence_failure_does_not_start_service(self):
        self.scenario(verify_exit=1)
        self.assertNotEqual(self.run_login().returncode, 0)
        self.assertIn(VERIFY, self.commands())
        self.assertNotIn(START, self.commands())

    def test_smoke_failure_stops_restarted_service(self):
        self.scenario(smoke_exit=1)
        self.assertNotEqual(self.run_login().returncode, 0)
        self.assertIn(START, self.commands())
        self.assertEqual(self.commands()[-1], STOP)

    def test_running_service_prevents_shared_keyring(self):
        self.scenario(still_running=True)
        result = self.run_login()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("refusing shared keyring access", result.stderr)
        self.assertNotIn(LOGIN, self.commands())

    def test_lock_prevents_concurrent_login(self):
        with (self.root / ".antigravity-login.lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = self.run_login()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("another login holds the lock", result.stderr)
        self.assertEqual(self.commands(), [])


if __name__ == "__main__":
    unittest.main()
