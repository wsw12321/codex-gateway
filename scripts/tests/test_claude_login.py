#!/usr/bin/env python3
"""Run the SSH entrypoints with a real controlling terminal and synthetic CPA."""

import fcntl
import json
import os
from pathlib import Path
import pty
import select
import shutil
import signal
import subprocess
import sys
import tempfile
import termios
import time
import unittest


ROOT = Path(__file__).resolve().parents[2]
STATE = "0123456789abcdef0123456789abcdef"
CODE = "synthetic-private-authorization-code"
PRIVATE = "SYNTHETIC_SECRET_CONTAINER_ERROR_NEVER_DISPLAY"
CALLBACK = f"http://localhost:54545/callback?code={CODE}&state={STATE}"
PROMPT = "Paste the full callback URL"
SUCCESS = "Claude authorization completed"
ACCOUNT = {"id": "0123456789abcdef", "masked_email": "a***@example.com", "status": "available"}

# This driver substitutes only the Compose/container boundary. Actual shell
# entrypoints, Python subprocess handling, HTTP parsing and terminal I/O run.
# Only synthetic inputs are inspected; callback data is never written to logs.
DRIVER = r'''
import json
import os
from pathlib import Path
import sys
import time

root = Path(__file__).resolve().parents[1]
scenario = json.loads((root / "scenario.json").read_text())
args = sys.argv[1:]
expected_prefix = ["exec", "-T", "codex-compat"]
assert args[:3] == expected_prefix, "unexpected service operation"
verify = args[3:] == ["/usr/local/bin/sidecar-entrypoint", "verify-oauth"]
if verify:
    operation = "verify"
else:
    assert len(args) == 8 and args[3:5] == ["sh", "-c"] and args[6] == "sh"
    operation = args[-1]
data = sys.stdin.buffer.read()
payload_ok = True
if not verify:
    head, sep, body = data.partition(b"\r\n\r\n")
    expected_head = (b"Host: 127.0.0.1:8317\r\nContent-Type: application/json\r\n"
        b"Connection: close\r\nContent-Length: " + str(len(body)).encode())
    payload_ok = sep == b"\r\n\r\n" and head == expected_head
    expected = {
        "begin": {}, "callback": {"state": "0123456789abcdef0123456789abcdef",
            "code": "synthetic-private-authorization-code"},
        "status": {"state": "0123456789abcdef0123456789abcdef"},
    }
    payload_ok = payload_ok and (json.loads(body) == expected[operation]
        if operation in expected else not body)
else:
    payload_ok = not data
record = {"args": args, "operation": operation, "payload_ok": payload_ok,
    "stdin_tty": os.isatty(0), "stdout_tty": os.isatty(1),
    "secret_in_env": any("synthetic-private-authorization-code" in value or
        "SYNTHETIC_SECRET_CONTAINER_ERROR_NEVER_DISPLAY" in value
        for value in os.environ.values())}
with (root / "commands.jsonl").open("a") as log:
    log.write(json.dumps(record) + "\n")
(root / ("reached-" + operation)).touch()
print("SYNTHETIC_SECRET_CONTAINER_ERROR_NEVER_DISPLAY", file=sys.stderr)
assert payload_ok, "invalid HTTP request or OAuth payload"
if scenario.get("stall") == operation:
    time.sleep(20)
if scenario.get("failure") == operation:
    print("SYNTHETIC_SECRET_CONTAINER_ERROR_NEVER_DISPLAY")
    sys.exit(7)
if verify:
    print("SYNTHETIC_SECRET_CONTAINER_ERROR_NEVER_DISPLAY")
    sys.exit(0)
if operation == "capabilities":
    response = {"protocol": "upstream_account_access_v1",
        "anthropic_messages": "unsupported" if scenario.get("bad_capability")
            else "anthropic_messages_v1"}
elif operation == "begin":
    response = {"state": "0123456789abcdef0123456789abcdef", "url":
        scenario.get("begin_url", "https://claude.ai/oauth/authorize?state=0123456789abcdef0123456789abcdef")}
elif operation == "callback":
    response = {"status": scenario.get("callback_status", "ok")}
elif operation == "status":
    count = sum(json.loads(line)["operation"] == "status"
        for line in (root / "commands.jsonl").read_text().splitlines())
    statuses = scenario.get("statuses", ["ok"])
    response = {"status": statuses[min(count - 1, len(statuses) - 1)]}
elif operation == "accounts":
    response = {"accounts": scenario.get("accounts", [{"id": "0123456789abcdef",
        "masked_email": "a***@example.com", "status": "available"}])}
else:
    raise AssertionError("unexpected operation")
body = json.dumps(response).encode()
status = b"200 OK"
if scenario.get("http_error") == operation:
    status = b"500 SYNTHETIC_SECRET_CONTAINER_ERROR_NEVER_DISPLAY"
    body = b"SYNTHETIC_SECRET_CONTAINER_ERROR_NEVER_DISPLAY"
if scenario.get("invalid_json") == operation:
    body = b"SYNTHETIC_SECRET_CONTAINER_ERROR_NEVER_DISPLAY"
sys.stdout.buffer.write(b"HTTP/1.1 " + status + b"\r\nContent-Type: application/json\r\n"
    b"Content-Length: " + str(len(body)).encode() + b"\r\n\r\n" + body)
'''


class ClaudeLoginTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="claude-login-test-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.scripts = self.root / "scripts"
        self.scripts.mkdir()
        self.bin = self.root / "bin"
        self.bin.mkdir()
        for name in ("claude-login.sh", "claude-login.py", "oauth-login.sh"):
            target = self.scripts / name
            shutil.copyfile(ROOT / "scripts" / name, target)
            target.chmod(0o700)
        self.script = self.scripts / "claude-login.sh"
        helper = self.scripts / "claude-login.py"
        source = helper.read_text()
        # Speed up integration deadlines only in this disposable copy. The
        # production entrypoint must never accept environment test overrides.
        for before, after in (("REQUEST_TIMEOUT = 10", "REQUEST_TIMEOUT = 1"),
                              ("CALLBACK_TIMEOUT = 300", "CALLBACK_TIMEOUT = 2"),
                              ("STATUS_TIMEOUT = 120", "STATUS_TIMEOUT = 1"),
                              ("POLL_INTERVAL = 2", "POLL_INTERVAL = 0.01")):
            self.assertEqual(source.count(before), 1)
            source = source.replace(before, after)
        helper.write_text(source)
        compose = self.scripts / "compose.sh"
        compose.write_text(f"#!{sys.executable}\n" + DRIVER)
        compose.chmod(0o700)
        (self.root / "deploy").mkdir()
        (self.root / ".env").write_text("# synthetic deployment\n")
        (self.root / "deploy/images.lock.env").write_text("# synthetic image lock\n")
        for name in ("dirname", "chmod", "flock"):
            command = shutil.which(name)
            self.assertIsNotNone(command, f"test requires {name}")
            (self.bin / name).symlink_to(command)
        (self.bin / "python3").symlink_to(sys.executable)
        for name in ("docker", "jq"):
            command = self.bin / name
            command.write_text("#!/bin/sh\nset -eu\nexit 0\n")
            command.chmod(0o700)
        self.env = dict(os.environ, PATH=str(self.bin))
        self.scenario()

    def scenario(self, **values):
        (self.root / "scenario.json").write_text(json.dumps(values))
        (self.root / "commands.jsonl").write_text("")
        for marker in self.root.glob("reached-*"):
            marker.unlink()

    def records(self):
        return [json.loads(line) for line in (self.root / "commands.jsonl").read_text().splitlines()]

    def operations(self):
        return [record["operation"] for record in self.records()]

    def assert_private(self, output):
        self.assertNotIn(CODE, output)
        self.assertNotIn(PRIVATE, output)
        self.assertNotIn(CALLBACK, output)
        for record in self.records():
            self.assertEqual(record["args"][:3], ["exec", "-T", "codex-compat"])
            self.assertNotIn(CODE, json.dumps(record["args"]))
            self.assertNotIn(PRIVATE, json.dumps(record["args"]))
            self.assertFalse(record["secret_in_env"])
            self.assertFalse(record["stdin_tty"])
            self.assertFalse(record["stdout_tty"])
            self.assertTrue(record["payload_ok"])

    def assert_unlocked(self):
        with (self.root / ".device-login.lock").open("a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)

    def run_without_terminal(self, *args, input_text=""):
        return subprocess.run([str(self.script), *args], env=self.env, input=input_text,
                              capture_output=True, text=True, timeout=8, start_new_session=True)

    def run_terminal(self, inputs=(CALLBACK,), entrypoint=None, args=(),
                     interrupted=None, interrupt_at="prompt", stdin_data=None):
        master, slave = pty.openpty()
        original = termios.tcgetattr(slave)

        def controlling_terminal():
            os.setsid()
            fcntl.ioctl(1, termios.TIOCSCTTY, 0)

        process = subprocess.Popen([str(entrypoint or self.script), *args], env=self.env,
                                   stdin=slave if stdin_data is None else subprocess.PIPE,
                                   stdout=slave, stderr=slave, preexec_fn=controlling_terminal)
        output = bytearray()
        sent = 0
        signaled = False
        try:
            if stdin_data is not None:
                process.stdin.write(stdin_data.encode())
                process.stdin.close()
            os.set_blocking(master, False)
            deadline = time.monotonic() + 8
            while True:
                if select.select([master], [], [], 0.01)[0]:
                    output.extend(os.read(master, 65536))
                decoded = output.decode(errors="replace")
                prompts = decoded.count(PROMPT)
                if sent < len(inputs) and prompts > sent:
                    # The prompt is emitted only after echo has been disabled.
                    self.assertFalse(termios.tcgetattr(slave)[3] & termios.ECHO)
                    os.write(master, inputs[sent].encode() + b"\n")
                    sent += 1
                if interrupted and not signaled:
                    ready = (prompts > 0 if interrupt_at == "prompt" else
                             (self.root / f"reached-{interrupt_at}").exists())
                    if ready:
                        os.killpg(process.pid, interrupted)
                        signaled = True
                if process.poll() is not None:
                    while select.select([master], [], [], 0)[0]:
                        output.extend(os.read(master, 65536))
                    break
                self.assertLess(time.monotonic(), deadline, "SSH login did not terminate")
            if interrupted:
                self.assertTrue(signaled, "login did not reach the requested interrupt phase")
            self.assertEqual(termios.tcgetattr(slave), original, "terminal settings were not restored")
            result = (process.returncode, output.decode(errors="replace"))
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
            os.close(master)
            os.close(slave)
        self.assert_unlocked()
        self.assert_private(result[1])
        return result

    def test_direct_login_uses_running_cpa_and_reports_masked_accounts(self):
        self.scenario(statuses=["wait", "ok"])
        status, output = self.run_terminal()
        self.assertEqual(status, 0, output)
        self.assertIn(SUCCESS, output)
        self.assertIn("a***@example.com", output)
        self.assertIn("No paid generation request was sent", output)
        self.assertEqual(self.operations(), ["capabilities", "begin", "callback", "status", "status", "verify", "accounts"])
        self.assertEqual((self.root / ".device-login.lock").stat().st_mode & 0o777, 0o600)

    def test_oauth_dispatch_and_reauthorization_need_no_account_inventory_change(self):
        for _attempt in range(2):
            with self.subTest(attempt=_attempt):
                self.scenario(accounts=[ACCOUNT])
                status, output = self.run_terminal(entrypoint=self.scripts / "oauth-login.sh", args=("claude",))
                self.assertEqual(status, 0, output)
                self.assertIn("does not identify the account just authorized", output)
                self.assertEqual(self.operations(), ["capabilities", "begin", "callback", "status", "verify", "accounts"])

    def test_unsupported_capability_and_bad_authorization_url_stop_before_callback(self):
        for scenario, operations in (({"bad_capability": True}, ["capabilities"]),
                                     ({"begin_url": "https://evil.example/authorize?state=" + STATE}, ["capabilities", "begin"]),
                                     ({"begin_url": "https://claude.ai/oauth/authorize?state=wrong"}, ["capabilities", "begin"]),
                                     ({"invalid_json": "begin"}, ["capabilities", "begin"])):
            with self.subTest(scenario=scenario):
                self.scenario(**scenario)
                status, output = self.run_terminal(inputs=())
                self.assertEqual(status, 1, output)
                self.assertIn("No callback was submitted", output)
                self.assertNotIn("evil.example", output)
                self.assertNotIn(PROMPT, output)
                self.assertEqual(self.operations(), operations)

    def test_invalid_callbacks_retry_without_submitting_until_valid(self):
        invalid = [CALLBACK + "&code=duplicate", CALLBACK.replace(STATE, "different"),
                   CALLBACK.replace("localhost", "evil.example"), CALLBACK + "#fragment"]
        status, output = self.run_terminal(inputs=(*invalid, CALLBACK))
        self.assertEqual(status, 0, output)
        self.assertEqual(output.count("Invalid callback;"), len(invalid))
        self.assertEqual(self.operations().count("callback"), 1)

    def test_uncertain_callback_result_is_never_resubmitted(self):
        for scenario in ({"failure": "callback"}, {"http_error": "callback"},
                         {"callback_status": "unexpected"}, {"stall": "callback"}):
            with self.subTest(scenario=scenario):
                self.scenario(**scenario)
                status, output = self.run_terminal()
                self.assertEqual(status, 0, output)
                self.assertIn("checking status without submitting again", output)
                self.assertIn(SUCCESS, output)
                self.assertEqual(self.operations().count("callback"), 1)
                self.assertEqual(self.operations().count("status"), 1)

    def test_saved_credentials_report_post_login_failures_separately(self):
        for scenario in ({"failure": "verify"}, {"failure": "accounts"}, {"accounts": []},
                         {"accounts": [{**ACCOUNT, "masked_email": PRIVATE}]}):
            with self.subTest(scenario=scenario):
                self.scenario(**scenario)
                status, output = self.run_terminal()
                self.assertEqual(status, 1, output)
                self.assertIn("凭据已保存，检查未通过", output)
                self.assertNotIn(SUCCESS, output)
                self.assertEqual(self.operations().count("callback"), 1)
                self.assertNotIn("Authorization may still complete", output)

    def test_input_timeout_restores_terminal_without_submitting(self):
        status, output = self.run_terminal(inputs=())
        self.assertEqual(status, 1, output)
        self.assertIn("Timed out waiting for the callback URL", output)
        self.assertEqual(self.operations(), ["capabilities", "begin"])

    def test_waiting_failed_or_malformed_status_never_reports_saved_credentials(self):
        for scenario in ({"statuses": ["wait"]}, {"failure": "status"},
                         {"statuses": ["error"]}, {"statuses": ["unexpected"]}):
            with self.subTest(scenario=scenario):
                self.scenario(**scenario)
                status, output = self.run_terminal()
                self.assertEqual(status, 1, output)
                self.assertIn("Authorization may still complete", output)
                self.assertNotIn(SUCCESS, output)
                self.assertNotIn("verify", self.operations())
                self.assertEqual(self.operations().count("callback"), 1)

    def test_controlling_terminal_is_required_and_stdin_is_not_a_fallback(self):
        result = self.run_without_terminal(input_text=CALLBACK + "\n")
        self.assertEqual(result.returncode, 1)
        self.assertIn("/dev/tty", result.stderr)
        self.assertEqual(self.operations(), [])
        self.assert_private(result.stdout + result.stderr)
        status, output = self.run_terminal(stdin_data="invalid-redirected-input\n")
        self.assertEqual(status, 0, output)
        self.assertNotIn("Invalid callback;", output)
        self.assertNotIn("invalid-redirected-input", output)

    def test_missing_dependencies_and_deployment_files_fail_before_container_access(self):
        for name in ("python3", "flock", "docker", "jq"):
            with self.subTest(dependency=name):
                target = self.bin / name
                moved = self.bin / (name + ".disabled")
                target.rename(moved)
                try:
                    result = self.run_without_terminal()
                finally:
                    moved.rename(target)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn(f"{name} is required", result.stderr)
                self.assertEqual(self.operations(), [])
        for relative in (".env", "deploy/images.lock.env", "scripts/compose.sh"):
            with self.subTest(configuration=relative):
                target = self.root / relative
                moved = target.with_name(target.name + ".disabled")
                target.rename(moved)
                try:
                    result = self.run_without_terminal()
                finally:
                    moved.rename(target)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn("Deployment files are missing", result.stderr)
                self.assertEqual(self.operations(), [])

    def test_busy_lock_and_symlink_are_rejected_without_deployment_actions(self):
        lock_path = self.root / ".device-login.lock"
        with lock_path.open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = self.run_without_terminal()
        self.assertEqual(result.returncode, 1)
        self.assertIn("another login or upgrade operation", result.stderr)
        self.assertEqual(self.operations(), [])
        lock_path.unlink()
        target = self.root / "unrelated-file"
        target.write_text("unchanged")
        lock_path.symlink_to(target)
        result = self.run_without_terminal()
        self.assertEqual(result.returncode, 1)
        self.assertIn("lock path must be a regular file", result.stderr)
        self.assertEqual(target.read_text(), "unchanged")
        self.assertEqual(self.operations(), [])

    def test_entrypoints_reject_extra_arguments_before_deployment_actions(self):
        result = self.run_without_terminal("unexpected")
        self.assertEqual(result.returncode, 1)
        result = subprocess.run([str(self.scripts / "oauth-login.sh"), "claude", "unexpected"],
                                env=self.env, capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(self.operations(), [])

    def test_signals_before_submission_restore_terminal_and_release_lock(self):
        for signum in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
            with self.subTest(signal=signum):
                self.scenario()
                status, output = self.run_terminal(inputs=(), interrupted=signum)
                self.assertEqual(status, 128 + signum, output)
                self.assertIn("No callback was submitted", output)
                self.assertNotIn(SUCCESS, output)
                self.assertEqual(self.operations(), ["capabilities", "begin"])

    def test_signals_after_submission_warn_authorization_can_still_complete(self):
        for signum in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
            with self.subTest(signal=signum):
                self.scenario(stall="callback")
                status, output = self.run_terminal(interrupted=signum, interrupt_at="callback")
                self.assertEqual(status, 128 + signum, output)
                self.assertIn("Authorization may still complete", output)
                self.assertIn("has not been canceled", output)
                self.assertNotIn(SUCCESS, output)
                self.assertEqual(self.operations().count("callback"), 1)
                self.assertNotIn("verify", self.operations())


if __name__ == "__main__":
    unittest.main()
