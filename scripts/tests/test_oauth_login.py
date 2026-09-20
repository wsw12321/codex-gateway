#!/usr/bin/env python3
"""Exercise the real login wrappers without Docker, OAuth accounts, or secrets."""

import fcntl
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


REPO_ROOT = Path(__file__).resolve().parents[2]
RESTART = ["up", "-d", "--no-deps", "codex-compat"]
LOGIN_PREFIX = ["run", "--rm", "--no-deps", "codex-compat"]
INITIAL_STOP = ["stop", "-t", "30", "codex-compat"]
CLEANUP_STOP = ["stop", "-t", "10", "codex-compat"]

FAKE_DRIVER = r'''
import json
import os
from pathlib import Path
import sys

root = Path(os.environ["OAUTH_TEST_ROOT"])
config = json.loads((root / "scenario.json").read_text())
state_path = root / "state.json"
state = json.loads(state_path.read_text())
tool = "docker" if Path(sys.argv[0]).name == "docker" else "compose"
args = sys.argv[1:]
with (root / "commands.jsonl").open("a") as log:
    log.write(json.dumps({"tool": tool, "args": args}) + "\n")

def finish(status=0, output=""):
    state_path.write_text(json.dumps(state))
    sys.stdout.write(output)
    sys.exit(status)

if tool == "docker":
    if args == ["inspect", "-f", "{{.State.Running}}", "fixture-sidecar"]:
        finish(output="true\n" if state["running"] else "false\n")
    if args == ["inspect", "-f", "{{if .State.Health}}{{.State.Health.Status}}{{end}}", "fixture-sidecar"]:
        finish(output=config.get("health", "healthy") + "\n")
else:
    if args in (["stop", "-t", "30", "codex-compat"], ["stop", "-t", "10", "codex-compat"]):
        state["running"] = config.get("still_running", False)
        finish()
    if args == ["ps", "-q", "codex-compat"]:
        finish(output="fixture-sidecar\n")
    if args == ["rm", "-f", "codex-compat"]:
        finish()
    if args == ["up", "-d", "egress-allowlist"]:
        finish()
    if args == ["up", "-d", "--no-deps", "codex-compat"]:
        state["running"] = True
        finish()
    if args == ["up", "-d", "--no-deps", "gateway"]:
        finish(config.get("gateway_start_status", 0))
    if args == ["exec", "-T", "gateway", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/readyz"]:
        finish()
    if args[:4] == ["run", "--rm", "--no-deps", "codex-compat"]:
        operation = args[4:]
        if operation == ["oauth-inventory"]:
            count = state["inventory_calls"]
            state["inventory_calls"] += 1
            if count >= len(config["inventories"]):
                finish(98, "unexpected extra inventory call\n")
            finish(output=config["inventories"][count])
        if operation == ["verify-oauth"]:
            finish(config.get("permission_status", 0))
        if operation and operation[0] in ("--codex-device-login",):
            finish(config.get("login_status", 0))
    if args[:4] == ["exec", "-T", "codex-compat", "/usr/local/bin/sidecar-smoke"]:
        finish(config.get("smoke_status", 0))
finish(99, "unexpected fake command: " + tool + " " + repr(args) + "\n")
'''


def inventory(*accounts):
    return "".join(f"{identity * 64} {revision * 64}\n" for identity, revision in accounts)


class OAuthLoginTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="oauth-login-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        scripts = self.root / "scripts"
        scripts.mkdir()
        for name in ("oauth-login.sh", "codex-device-login.sh"):
            shutil.copyfile(REPO_ROOT / "scripts" / name, scripts / name)
            (scripts / name).chmod(0o700)
        binaries = self.root / "bin"
        binaries.mkdir()
        for path in (scripts / "compose.sh", binaries / "docker"):
            path.write_text(f"#!{sys.executable}\n" + FAKE_DRIVER)
            path.chmod(0o700)
        self.env = os.environ.copy()
        self.env["PATH"] = str(binaries) + os.pathsep + self.env.get("PATH", "")
        self.env["OAUTH_TEST_ROOT"] = str(self.root)
        self.scenario()

    def scenario(self, before="", after=None, **options):
        if after is None:
            after = inventory(("a", "1"))
        (self.root / "scenario.json").write_text(
            json.dumps({"inventories": [before, after], **options})
        )
        (self.root / "state.json").write_text(
            json.dumps({"inventory_calls": 0, "running": True})
        )
        (self.root / "commands.jsonl").write_text("")

    def run_login(self, wrapper="codex-device-login.sh", *args):
        result = subprocess.run(
            [str(self.root / "scripts" / wrapper), *args],
            cwd=self.root,
            env=self.env,
            capture_output=True,
            text=True,
            timeout=10,
        )
        self.assertNotIn("unexpected fake command", result.stdout + result.stderr)
        return result

    def commands(self, tool="compose"):
        events = [
            json.loads(line)
            for line in (self.root / "commands.jsonl").read_text().splitlines()
        ]
        return [event["args"] for event in events if event["tool"] == tool]

    def assert_stopped_before_restart(self, result, message):
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(message, result.stderr)
        commands = self.commands()
        self.assertIn(INITIAL_STOP, commands)
        self.assertNotIn(RESTART, commands)
        self.assertNotIn(CLEANUP_STOP, commands)
        self.assertFalse(json.loads((self.root / "state.json").read_text())["running"])

    def test_codex_preserves_device_login_flags_and_default_smoke(self):
        result = self.run_login("codex-device-login.sh")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        commands = self.commands()
        self.assertIn(LOGIN_PREFIX + ["--codex-device-login"], commands)
        self.assertIn(["exec", "-T", "codex-compat", "/usr/local/bin/sidecar-smoke"], commands)

    def test_refreshing_one_account_preserves_other_accounts(self):
        self.scenario(
            before=inventory(("a", "1"), ("b", "2")),
            after=inventory(("b", "2"), ("a", "3")),
        )
        result = self.run_login()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(RESTART, self.commands())

    def test_generation_smoke_waits_for_gateway_after_sidecar_health(self):
        result = self.run_login()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        commands = self.commands()
        gateway_start = ["up", "-d", "--no-deps", "gateway"]
        gateway_ready = ["exec", "-T", "gateway", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/readyz"]
        smoke = ["exec", "-T", "codex-compat", "/usr/local/bin/sidecar-smoke"]
        self.assertLess(commands.index(RESTART), commands.index(gateway_start))
        self.assertLess(commands.index(gateway_start), commands.index(gateway_ready))
        self.assertLess(commands.index(gateway_ready), commands.index(smoke))

    def test_gateway_start_failure_stops_unverified_sidecar(self):
        self.scenario(gateway_start_status=1)
        result = self.run_login()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(CLEANUP_STOP, self.commands())
        self.assertNotIn(["exec", "-T", "codex-compat", "/usr/local/bin/sidecar-smoke"], self.commands())

    def test_codex_obeys_operation_lock(self):
        with (self.root / ".device-login.lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            for wrapper in ("codex-device-login.sh",):
                with self.subTest(wrapper=wrapper):
                    result = self.run_login(wrapper)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("another login or upgrade operation holds the lock", result.stderr)
                    self.assertEqual(self.commands(), [])
                    self.assertEqual(self.commands("docker"), [])

    def test_zero_multiple_or_deleted_accounts_are_rejected_before_restart(self):
        cases = {
            "no accounts": ("", ""),
            "unchanged account": (inventory(("a", "1")), inventory(("a", "1"))),
            "two additions": ("", inventory(("a", "1"), ("b", "2"))),
            "two refreshes": (
                inventory(("a", "1"), ("b", "2")),
                inventory(("a", "3"), ("b", "4")),
            ),
            "refresh and addition": (inventory(("a", "1")), inventory(("a", "2"), ("b", "3"))),
            "deletion": (inventory(("a", "1"), ("b", "2")), inventory(("a", "3"))),
            "replacement": (inventory(("a", "1")), inventory(("b", "2"))),
        }
        for name, (before, after) in cases.items():
            with self.subTest(mutation=name):
                self.scenario(before=before, after=after)
                self.assert_stopped_before_restart(
                    self.run_login(), "expected exactly one added or refreshed account and no removed accounts"
                )

    def test_invalid_inventory_is_rejected_before_restart(self):
        malformed = {
            "short digest": "abc def\n",
            "uppercase digest": inventory(("A", "1")),
            "duplicate account": inventory(("a", "1"), ("a", "2")),
            "missing revision": "a" * 64 + "\n",
            "extra field": inventory(("a", "1")).rstrip("\n") + " unexpected\n",
        }
        for name, value in malformed.items():
            for phase in ("before", "after"):
                with self.subTest(inventory=name, phase=phase):
                    self.scenario(**{phase: value})
                    self.assert_stopped_before_restart(self.run_login(), "invalid OAuth inventory response")
                    if phase == "before":
                        self.assertNotIn(
                            LOGIN_PREFIX + ["--codex-device-login"], self.commands()
                        )

    def test_permission_failure_keeps_sidecar_stopped(self):
        self.scenario(permission_status=1)
        self.assert_stopped_before_restart(self.run_login(), "OAuth permission validation failed")
        self.assertEqual(self.commands().count(LOGIN_PREFIX + ["oauth-inventory"]), 1)

    def test_login_failure_keeps_sidecar_stopped(self):
        self.scenario(login_status=1)
        self.assert_stopped_before_restart(self.run_login(), "login failed; sidecar remains stopped")
        self.assertNotIn(LOGIN_PREFIX + ["verify-oauth"], self.commands())

    def test_smoke_failure_stops_the_restarted_sidecar(self):
        self.scenario(smoke_status=1)
        result = self.run_login()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("upstream smoke test failed", result.stderr)
        commands = self.commands()
        self.assertIn(RESTART, commands)
        self.assertEqual(commands[-1], CLEANUP_STOP)
        self.assertFalse(json.loads((self.root / "state.json").read_text())["running"])

    def test_removed_gemini_login_rejected_before_stopping(self):
        result = self.run_login("oauth-login.sh", "gemini")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("antigravity-login.sh", result.stderr)
        self.assertEqual(self.commands(), [])


if __name__ == "__main__":
    unittest.main()
