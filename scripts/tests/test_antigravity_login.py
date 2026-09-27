#!/usr/bin/env python3
"""Exercise login phases, safe diagnostics and the actual controlling terminal."""

import fcntl
import json
import os
from pathlib import Path
import pty
import shutil
import signal
import subprocess
import sys
import tempfile
import termios
import time
import unittest


ROOT = Path(__file__).resolve().parents[2]
STOP = ["stop", "-t", "10", "antigravity-bridge"]
LOGIN = ["run", "--rm", "--no-deps", "antigravity-bridge", "login"]
VERIFY = ["run", "-T", "--rm", "--no-deps", "-e", "TERM=dumb", "antigravity-bridge", "verify-login"]
START = ["up", "-d", "--no-deps", "antigravity-bridge"]
SMOKE = ["exec", "-T", "-e", "TERM=dumb", "antigravity-bridge", "/usr/local/bin/antigravity-smoke"]
SUCCESS = "Antigravity login persisted; readiness, JSON and SSE passed."
PRIVATE = "SENSITIVE_TOKEN_OAUTH_URL_CLI_STDERR_MODEL_REPLY"
DRIVER = r'''
import json
import os
from pathlib import Path
import sys
import termios
import time
root = Path(os.environ["BRIDGE_TEST_ROOT"])
scenario = json.loads((root / "scenario.json").read_text())
args = sys.argv[1:]
with (root / "commands.jsonl").open("a") as output:
    output.write(json.dumps({"args": args, "stdin_tty": os.isatty(0),
        "stdout_tty": os.isatty(1),
        "stdin_null": os.fstat(0).st_rdev == os.stat("/dev/null").st_rdev}) + "\n")
if args == ["ps", "-q", "antigravity-bridge"]:
    step = "ps"
    print("synthetic-bridge")
elif args == ["inspect", "-f", "{{.State.Running}}", "synthetic-bridge"]:
    step = "inspect"
    print("true" if scenario.get("still_running") else "false")
elif "login" in args:
    step = "login"
    if scenario.get("change_terminal"):
        attrs = termios.tcgetattr(0)
        attrs[3] &= ~(termios.ECHO | termios.ICANON)
        termios.tcsetattr(0, termios.TCSANOW, attrs)
        (root / "terminal_changed").touch()
    if scenario.get("wait_signal"):
        time.sleep(10)
elif "verify-login" in args:
    step = "verify"
    print("SENSITIVE_TOKEN_OAUTH_URL_CLI_STDERR_MODEL_REPLY")
elif "/usr/local/bin/antigravity-smoke" in args:
    step = "smoke"
    print("SENSITIVE_TOKEN_OAUTH_URL_CLI_STDERR_MODEL_REPLY")
elif args[0:1] == ["exec"]:
    step = "readiness"
elif args[0:1] == ["stop"]:
    step = "stop"
elif args[-1:] == ["egress-allowlist"]:
    step = "egress"
else:
    step = "start"
print("SENSITIVE_TOKEN_OAUTH_URL_CLI_STDERR_MODEL_REPLY", file=sys.stderr)
if scenario.get("failure") == step:
    if scenario.get("diagnostic"):
        print(scenario["diagnostic"], file=sys.stderr)
    sys.exit(scenario.get("exit_code", 7))
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
        (scripts / "sleep").write_text("#!/bin/sh\nset -eu\nexit 0\n")
        (scripts / "sleep").chmod(0o700)
        self.env = dict(os.environ, BRIDGE_TEST_ROOT=str(self.root))
        self.env["PATH"] = str(scripts) + os.pathsep + os.environ.get("PATH", "")
        self.scenario()

    def scenario(self, **values):
        (self.root / "scenario.json").write_text(json.dumps(values))
        (self.root / "commands.jsonl").write_text("")
        (self.root / "terminal_changed").unlink(missing_ok=True)

    def run_login(self, *args):
        return subprocess.run([str(self.script), *args], env=self.env, text=True,
                              capture_output=True, timeout=10)

    def records(self):
        return [json.loads(line) for line in (self.root / "commands.jsonl").read_text().splitlines()]

    def commands(self):
        return [record["args"] for record in self.records()]

    def test_login_and_new_session_verification_precede_start(self):
        result = self.run_login()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(SUCCESS, result.stdout)
        self.assertNotIn(PRIVATE, result.stdout + result.stderr)
        commands = self.commands()
        for before, after in zip([STOP, LOGIN, VERIFY, START], [LOGIN, VERIFY, START, SMOKE]):
            self.assertLess(commands.index(before), commands.index(after))
        self.assertEqual(commands.count(STOP), 1)
        self.assertNotIn("codex-compat", json.dumps(commands))
        self.assertEqual((self.root / ".antigravity-login.lock").stat().st_mode & 0o777, 0o600)

    def test_named_account_reaches_login_verification_and_http_smoke(self):
        result = self.run_login("work-account_2")
        self.assertEqual(result.returncode, 0, result.stderr)
        commands = self.commands()
        for base in (LOGIN, VERIFY, SMOKE):
            self.assertIn(base + ["work-account_2"], commands)
        self.assertNotIn(PRIVATE, result.stdout + result.stderr)

    def test_invalid_slots_are_rejected_before_stopping_service(self):
        for slot in ("../secret", "two words", "UpperCase", "-option", "_slot", "", "x" * 33):
            with self.subTest(slot=slot):
                self.scenario()
                result = self.run_login(slot)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("category=configuration", result.stderr)
                self.assertEqual(self.commands(), [])

    def test_every_host_phase_reports_failure_and_never_success(self):
        for failure, stage in [("stop", "stop"), ("ps", "service_state"),
                               ("inspect", "service_state"), ("egress", "egress"),
                               ("login", "authorization"), ("verify", "verification"),
                               ("start", "start"), ("readiness", "readiness"),
                               ("smoke", "http_acceptance")]:
            with self.subTest(failure=failure):
                self.scenario(failure=failure)
                result = self.run_login()
                self.assertEqual(result.returncode, 7, result.stderr)
                self.assertIn(f"stage={stage} category=command_failed exit_code=7", result.stderr)
                self.assertNotIn(SUCCESS, result.stdout)
                self.assertNotIn(PRIVATE, result.stdout + result.stderr)
                if failure in ("login", "verify"):
                    self.assertNotIn(START, self.commands())
                if failure in ("start", "readiness", "smoke"):
                    self.assertEqual(self.commands()[-1], STOP)

    def test_credential_cli_and_http_failures_keep_their_safe_stage(self):
        for failure, stages in [("login", ["credential_save"]),
                                ("verify", ["credential_restore", "credential_save", "models", "usage", "generation"]),
                                ("smoke", ["http_models", "http_json", "http_sse"])]:
            for stage in stages:
                with self.subTest(stage=stage):
                    diagnostic = f"antigravity: stage={stage} category=invalid_response exit_code=7"
                    self.scenario(failure=failure, diagnostic=diagnostic)
                    result = self.run_login()
                    self.assertEqual(result.returncode, 7)
                    self.assertIn(diagnostic, result.stderr)
                    self.assertNotIn(SUCCESS, result.stdout)
                    self.assertNotIn(PRIVATE, result.stdout + result.stderr)

    def test_unrecognized_diagnostics_are_not_relayed(self):
        self.scenario(failure="verify", diagnostic=f"antigravity: stage=usage category={PRIVATE} exit_code=7")
        result = self.run_login()
        self.assertEqual(result.returncode, 7)
        self.assertNotIn(PRIVATE, result.stdout + result.stderr)

    def test_running_service_prevents_shared_keyring(self):
        self.scenario(still_running=True)
        result = self.run_login()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("stage=service_state category=still_running", result.stderr)
        self.assertNotIn(LOGIN, self.commands())

    def test_lock_prevents_concurrent_login(self):
        with (self.root / ".antigravity-login.lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = self.run_login()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("stage=lock category=busy", result.stderr)
        self.assertEqual(self.commands(), [])

    def test_symlink_lock_is_rejected(self):
        target = self.root / "target"
        target.write_text("unchanged")
        (self.root / ".antigravity-login.lock").symlink_to(target)
        result = self.run_login()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("stage=lock category=unsafe_path", result.stderr)
        self.assertEqual(target.read_text(), "unchanged")

    def run_with_terminal(self, interrupted=None):
        master, slave = pty.openpty()
        self.addCleanup(os.close, master)
        self.addCleanup(os.close, slave)
        original = termios.tcgetattr(slave)

        def control_terminal():
            os.setsid()
            fcntl.ioctl(0, termios.TIOCSCTTY, 0)

        process = subprocess.Popen([str(self.script)], env=self.env, stdin=slave,
                                   stdout=slave, stderr=slave, preexec_fn=control_terminal)
        try:
            if interrupted:
                deadline = time.monotonic() + 5
                while not (self.root / "terminal_changed").exists():
                    self.assertIsNone(process.poll(), "login exited before changing its terminal")
                    self.assertLess(time.monotonic(), deadline, "login did not reach interactive phase")
                    time.sleep(0.01)
                os.killpg(process.pid, interrupted)
            status = process.wait(timeout=10)
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
        self.assertEqual(termios.tcgetattr(slave), original, "terminal settings were not restored")
        os.set_blocking(master, False)
        output = bytearray()
        while True:
            try:
                part = os.read(master, 65536)
            except BlockingIOError:
                break
            if not part:
                break
            output.extend(part)
        return status, output.decode()

    def test_only_interactive_authorization_inherits_terminal(self):
        self.scenario(change_terminal=True)
        status, output = self.run_with_terminal()
        self.assertEqual(status, 0, output)
        self.assertIn(SUCCESS, output)
        for record in self.records():
            with self.subTest(command=record["args"]):
                if record["args"] == LOGIN:
                    self.assertTrue(record["stdin_tty"])
                    self.assertTrue(record["stdout_tty"])
                    self.assertNotIn("-T", record["args"])
                else:
                    self.assertFalse(record["stdin_tty"])
                    self.assertFalse(record["stdout_tty"])
                    self.assertTrue(record["stdin_null"])
        self.assertIn(VERIFY, self.commands())
        self.assertIn(SMOKE, self.commands())
        for command in self.commands():
            if command[0] == "exec":
                self.assertIn("-T", command)
                self.assertIn("TERM=dumb", command)

    def test_terminal_restored_when_login_fails(self):
        self.scenario(change_terminal=True, failure="login")
        status, output = self.run_with_terminal()
        self.assertEqual(status, 7, output)
        self.assertIn("stage=authorization category=command_failed exit_code=7", output)
        self.assertNotIn(SUCCESS, output)

    def test_terminal_restored_when_signal_interrupts_login(self):
        for signum in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
            with self.subTest(signal=signum):
                self.scenario(change_terminal=True, wait_signal=True)
                status, output = self.run_with_terminal(interrupted=signum)
                expected = 128 + signum
                self.assertEqual(status, expected, output)
                self.assertIn(f"stage=authorization category=interrupted exit_code={expected}", output)
                self.assertNotIn(SUCCESS, output)
                self.assertNotIn(START, self.commands())


HTTP_DRIVER = r'''
import json
import os
from pathlib import Path
import sys
root = Path(os.environ["BRIDGE_TEST_ROOT"])
scenario = json.loads((root / "scenario.json").read_text())
args = sys.argv[1:]
if args[-1].endswith("/v1/models"):
    stage = "http_models"
else:
    request = Path(args[args.index("--data-binary") + 1].removeprefix("@"))
    stage = "http_sse" if json.loads(request.read_text())["stream"] else "http_json"
with (root / "commands.jsonl").open("a") as output:
    output.write(json.dumps({"args": args, "stage": stage,
        "stdin_null": os.fstat(0).st_rdev == os.stat("/dev/null").st_rdev,
        "term": os.environ.get("TERM")}) + "\n")
print("SENSITIVE_TOKEN_OAUTH_URL_CLI_STDERR_MODEL_REPLY", file=sys.stderr)
if scenario.get("failure") == stage:
    sys.exit(28)
invalid = scenario.get("invalid") == stage
response = {"status": "failed" if invalid else "completed",
    "usage": {"input_tokens": 1, "output_tokens": 1},
    "output": [{"type": "message", "content": [{"type": "output_text",
        "text": "SENSITIVE_TOKEN_OAUTH_URL_CLI_STDERR_MODEL_REPLY"}]}]}
if stage == "http_models":
    print(json.dumps({"data": [] if invalid else [{"id": "gemini-3.1-pro-preview"}]}))
elif stage == "http_json":
    print(json.dumps(response))
else:
    if scenario.get("sse_error"):
        print('event: error\ndata: {"type":"error"}\n')
    print("event: response.completed\ndata: " + json.dumps({"type": "response.completed", "response": response}) + "\n")
    if not scenario.get("no_done"):
        print("data: [DONE]\n")
'''


@unittest.skipUnless(shutil.which("jq"), "HTTP smoke regression requires jq")
class AntigravitySmokeTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="antigravity-smoke-test-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.key_file = self.root / "api_key"
        self.key_file.write_text("synthetic_private_api_key")
        source = (ROOT / "deploy/antigravity-bridge/smoke.sh").read_text()
        # Only relocate the fixed secret mount into the disposable test fixture.
        self.assertEqual(source.count("/run/secrets/antigravity_bridge_api_key"), 1)
        source = source.replace("/run/secrets/antigravity_bridge_api_key", str(self.key_file))
        self.script = self.root / "smoke.sh"
        self.script.write_text(source)
        self.script.chmod(0o700)
        curl = self.root / "curl"
        curl.write_text(f"#!{sys.executable}\n" + HTTP_DRIVER)
        curl.chmod(0o700)
        self.env = dict(os.environ, BRIDGE_TEST_ROOT=str(self.root))
        self.env["PATH"] = str(self.root) + os.pathsep + os.environ.get("PATH", "")
        self.scenario()

    def scenario(self, **values):
        (self.root / "scenario.json").write_text(json.dumps(values))
        (self.root / "commands.jsonl").write_text("")

    def run_smoke(self):
        return subprocess.run([str(self.script)], env=self.env, capture_output=True,
                              text=True, timeout=10)

    def test_success_validates_real_json_and_sse_without_disclosing_contents(self):
        result = self.run_smoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout + result.stderr, "")
        records = [json.loads(line) for line in (self.root / "commands.jsonl").read_text().splitlines()]
        self.assertEqual([record["stage"] for record in records], ["http_models", "http_json", "http_sse"])
        for record in records:
            self.assertTrue(record["stdin_null"])
            self.assertEqual(record["term"], "dumb")
            self.assertNotIn("synthetic_private_api_key", json.dumps(record["args"]))

    def test_each_http_stage_reports_transport_and_response_failures(self):
        for stage in ("http_models", "http_json", "http_sse"):
            for scenario, category, code in [("failure", "command_failed", 28),
                                               ("invalid", "invalid_response", 1)]:
                with self.subTest(stage=stage, failure=scenario):
                    self.scenario(**{scenario: stage})
                    result = self.run_smoke()
                    self.assertEqual(result.returncode, code)
                    self.assertEqual(result.stderr,
                                     f"antigravity: stage={stage} category={category} exit_code={code}\n")
                    self.assertEqual(result.stdout, "")
                    self.assertNotIn(PRIVATE, result.stdout + result.stderr)

    def test_sse_error_or_missing_terminator_cannot_report_success(self):
        for failure in ("sse_error", "no_done"):
            with self.subTest(failure=failure):
                self.scenario(**{failure: True})
                result = self.run_smoke()
                self.assertEqual(result.returncode, 1)
                self.assertEqual(result.stderr,
                                 "antigravity: stage=http_sse category=invalid_response exit_code=1\n")

    def test_missing_or_invalid_key_has_safe_configuration_failure(self):
        for missing in (False, True):
            with self.subTest(missing=missing):
                if missing:
                    self.key_file.unlink()
                else:
                    self.key_file.write_text("not a valid key")
                result = self.run_smoke()
                self.assertEqual(result.returncode, 1)
                self.assertEqual(result.stderr,
                                 "antigravity: stage=http_models category=configuration exit_code=1\n")
                self.assertEqual(result.stdout, "")


if __name__ == "__main__":
    unittest.main()
