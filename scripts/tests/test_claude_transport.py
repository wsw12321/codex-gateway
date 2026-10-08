#!/usr/bin/env python3
"""Exercise Claude login's HTTP parser, bounded subprocesses and real shell transport."""

import contextlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("claude_login_transport", ROOT / "scripts/claude-login.py")
LOGIN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(LOGIN)

SIDE_KEY = "A" * 43
MANAGEMENT_KEY = "B" * 43
AUTH_CODE = "synthetic-private-authorization-code"
PRIVATE = "synthetic-private-container-diagnostic"
JSON_BODY = b'{"status":"ok"}'


def response(body=JSON_BODY, headers=None, status="200 OK"):
    if headers is None:
        headers = ["Content-Type: application/json", f"Content-Length: {len(body)}"]
    elif not any(header.lower().startswith("content-type:") for header in headers):
        headers = ["Content-Type: application/json", *headers]
    return ("HTTP/1.1 " + status + "\r\n" + "\r\n".join(headers) + "\r\n\r\n").encode() + body


def request_input(payload=None):
    body = b"" if payload is None else json.dumps(payload, separators=(",", ":")).encode()
    return (b"Host: 127.0.0.1:8317\r\nContent-Type: application/json\r\nContent-Length: "
            + str(len(body)).encode() + b"\r\nConnection: close\r\n\r\n" + body)


class ClaudeHTTPTests(unittest.TestCase):
    def assert_rejected(self, raw):
        with self.assertRaises(LOGIN.LoginError) as error:
            LOGIN.parse_response(raw)
        self.assertNotIn(PRIVATE, str(error.exception))

    def test_content_length_chunked_and_connection_close(self):
        chunked = b"5\r\n" + JSON_BODY[:5] + b"\r\n" + format(len(JSON_BODY[5:]), "x").encode()
        chunked += b"\r\n" + JSON_BODY[5:] + b"\r\n0\r\n\r\n"
        samples = [response(), response(headers=["Content-Type: application/json"]),
                   response(chunked, ["Content-Type: application/json", "Transfer-Encoding: chunked"]),
                   response(headers=["content-type: application/json; charset=utf-8",
                                     f"content-length: {len(JSON_BODY)}"]),
                   response().replace(b"HTTP/1.1", b"HTTP/1.0"),
                   response(chunked.replace(b"5\r\n", b"5;extension=value\r\n", 1),
                            ["Transfer-Encoding: chunked"])]
        for raw in samples:
            with self.subTest(raw=raw):
                self.assertEqual(LOGIN.parse_response(raw), {"status": "ok"})

    def test_rejects_ambiguous_or_incomplete_message_framing(self):
        invalid_headers = [
            ["Content-Length: 15", "Content-Length: 15"],
            ["Content-Length: 15", "content-length: 14"],
            ["Content-Length: 15", "Transfer-Encoding: chunked"],
            ["Content-Length: -1"], ["Content-Length: +15"],
            ["Content-Length: 15, 15"], ["Content-Length: 14"],
            ["Content-Length: 16"], ["Transfer-Encoding: gzip"],
            ["Transfer-Encoding: gzip, chunked"],
            ["Transfer-Encoding: chunked", "Transfer-Encoding: chunked"],
            ["Content-Length : 15"], ["Content-Length: 15", " folded: value"],
            ["Content-Length: 15", "bad-header"],
        ]
        for headers in invalid_headers:
            with self.subTest(headers=headers):
                self.assert_rejected(response(headers=headers))
        for raw in (response() + b"trailing", response().replace(b"\r\n", b"\n"),
                    b"HTTP/1.1 200 OK\r\n", b"not-http\r\n\r\n" + JSON_BODY,
                    b"HTTP/1.1 200 OK\r\nX-Test: bad\x00value\r\n\r\n" + JSON_BODY):
            with self.subTest(raw=raw):
                self.assert_rejected(raw)

    def test_rejects_invalid_chunk_sizes_terminators_and_trailing_bytes(self):
        cases = [b"g\r\n{}\r\n0\r\n\r\n", b"-1\r\n{}\r\n0\r\n\r\n",
                 b"+2\r\n{}\r\n0\r\n\r\n", b"2\r\n{}X\n0\r\n\r\n",
                 b"2\r\n{}\r\n", b"2\r\n{}\r\n0\r\n", b"2\r\n{}\r\n0\r\n\r\nx",
                 b"2\r\n{}\r\n0\r\nX-Trailer: value\r\n\r\n",
                 b"10000000000000000000000000000000\r\n{}\r\n0\r\n\r\n"]
        for body in cases:
            with self.subTest(body=body):
                self.assert_rejected(response(body, ["Transfer-Encoding: chunked"]))

    def test_redirects_errors_and_malformed_json_are_private(self):
        for status in ("301 Moved Permanently", "302 Found", "307 Temporary Redirect", "401 Unauthorized",
                       "403 Forbidden", "500 Internal Server Error"):
            with self.subTest(status=status):
                self.assert_rejected(response(PRIVATE.encode(), ["Location: https://example.com/"], status))
        for body in (b"[]", b"null", b"false", b"42", b'"text"', PRIVATE.encode(), b"{", b"\xff",
                     b'{"status":"ok","status":"error"}', b'{"value":NaN}'):
            with self.subTest(body=body):
                self.assert_rejected(response(body))

    def test_rejects_oversized_http_response(self):
        self.assert_rejected(response(b'{"value":"' + b"x" * (2 * 1024 * 1024) + b'"}'))

    def test_rejects_oversized_headers_wrong_content_type_and_compression(self):
        self.assert_rejected(response(headers=["X-Large: " + "x" * LOGIN.MAX_HEADERS]))
        self.assert_rejected(response().replace(b"Content-Type: application/json\r\n", b""))
        self.assert_rejected(response(headers=["Content-Type: text/plain"]))
        self.assert_rejected(response(headers=["Content-Encoding: gzip"]))
        self.assert_rejected(response(headers=["Content-Type: application/json",
                                               "content-type: application/json"]))


class ClaudeClientTests(unittest.TestCase):
    def test_callback_code_and_state_only_cross_stdin(self):
        client = LOGIN.Client(ROOT)
        payload = {"state": "synthetic-private-state", "code": AUTH_CODE}
        environment = dict(os.environ)
        with mock.patch.object(LOGIN, "run_process", return_value=response()) as process:
            self.assertEqual(client.request("callback", payload, timeout=3), {"status": "ok"})
        process.assert_called_once()
        args, data = process.call_args.args
        self.assertEqual(args, [str(ROOT / "scripts/compose.sh"), "exec", "-T", "codex-compat",
                                "sh", "-c", LOGIN.TRANSPORT, "sh", "callback"])
        self.assertEqual(process.call_args.kwargs, {"timeout": 3})
        headers, body = data.split(b"\r\n\r\n", 1)
        self.assertEqual(json.loads(body), payload)
        self.assertIn(b"Host: 127.0.0.1:8317", headers)
        self.assertIn(b"Content-Type: application/json", headers)
        self.assertIn(b"Connection: close", headers)
        self.assertIn(b"Content-Length: " + str(len(body)).encode(), headers)
        for value in (SIDE_KEY, MANAGEMENT_KEY, *payload.values()):
            self.assertNotIn(value, json.dumps(args))
            self.assertNotIn(value.encode(), headers)
        self.assertEqual(dict(os.environ), environment)

    def test_unknown_operations_and_oversized_payload_never_start_transport(self):
        client = LOGIN.Client(ROOT)
        with mock.patch.object(LOGIN, "run_process") as process:
            for operation in ("refresh", "delete", "https://example.com", "callback; false", ""):
                with self.subTest(operation=operation), self.assertRaises(LOGIN.LoginError):
                    client.request(operation)
            with self.assertRaises(LOGIN.LoginError):
                client.request("callback", {"code": "x" * 20000})
        process.assert_not_called()


class ClaudeProcessTests(unittest.TestCase):
    def test_stdout_returns_but_container_stderr_is_suppressed(self):
        with tempfile.TemporaryFile() as captured:
            saved_stderr = os.dup(2)
            try:
                os.dup2(captured.fileno(), 2)
                output = LOGIN.run_process([sys.executable, "-c",
                    "import sys; sys.stderr.write(" + repr(PRIVATE) + "); sys.stdout.buffer.write(b'valid')"])
            finally:
                os.dup2(saved_stderr, 2)
                os.close(saved_stderr)
            captured.seek(0)
            self.assertEqual(captured.read(), b"")
        self.assertEqual(output, b"valid")

    def test_nonzero_exit_does_not_include_child_output(self):
        with self.assertRaises(LOGIN.LoginError) as error:
            LOGIN.run_process([sys.executable, "-c", "import sys; print(" + repr(PRIVATE)
                               + "); print(" + repr(PRIVATE) + ", file=sys.stderr); sys.exit(9)"])
        self.assertNotIn(PRIVATE, str(error.exception))

    def test_output_limit_stops_a_writer(self):
        started = time.monotonic()
        with self.assertRaises(LOGIN.LoginError):
            LOGIN.run_process([sys.executable, "-c",
                "import os; data=b'x'*65536\nwhile True: os.write(1,data)"], timeout=3)
        self.assertLess(time.monotonic() - started, 2)

    def test_timeout_kills_descendants_and_does_not_echo_input(self):
        with tempfile.TemporaryDirectory(prefix="claude-process-test-") as directory:
            marker = Path(directory) / "child-survived"
            child = "import time; from pathlib import Path; time.sleep(0.7); Path(" + repr(str(marker)) + ").touch()"
            parent = "import subprocess,sys,time; subprocess.Popen([sys.executable,'-c'," + repr(child) + "]); time.sleep(10)"
            started = time.monotonic()
            with self.assertRaises(LOGIN.LoginError) as error:
                LOGIN.run_process([sys.executable, "-c", parent], AUTH_CODE.encode(), timeout=0.3)
            self.assertLess(time.monotonic() - started, 2)
            self.assertNotIn(AUTH_CODE, str(error.exception))
            time.sleep(0.8)
            self.assertFalse(marker.exists(), "request timeout left a descendant running")


class ClaudeShellTransportTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="claude-transport-test-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.side_secret = self.root / "sidecar_api_key"
        self.management_secret = self.root / "cpa_management_key"
        self.side_secret.write_text(SIDE_KEY + "\n")
        self.management_secret.write_text(MANAGEMENT_KEY + "\n")
        self.transport = LOGIN.TRANSPORT.replace("/run/secrets/sidecar_api_key", str(self.side_secret))
        self.transport = self.transport.replace("/run/secrets/cpa_management_key", str(self.management_secret))
        self.bin = self.root / "bin"
        self.bin.mkdir()
        executable = self.bin / "nc"
        executable.write_text(f"#!{sys.executable}\n" + """
import json, os, sys
from pathlib import Path
root = Path(os.environ['CLAUDE_TRANSPORT_TEST_ROOT'])
(root / 'request').write_bytes(sys.stdin.buffer.read())
(root / 'nc.json').write_text(json.dumps({'args': sys.argv[1:], 'env': dict(os.environ)}))
sys.stdout.buffer.write(b'HTTP/1.1 200 OK\\r\\nContent-Type: application/json\\r\\nContent-Length: 15\\r\\n\\r\\n{"status":"ok"}')
""")
        executable.chmod(0o700)
        self.env = dict(os.environ, CLAUDE_TRANSPORT_TEST_ROOT=str(self.root),
                        PATH=str(self.bin) + os.pathsep + os.environ.get("PATH", ""))

    def shell(self, operation, payload=None, transport=None, env=None):
        return subprocess.run(["/bin/sh", "-c", transport or self.transport, "sh", operation],
                              input=request_input(payload), capture_output=True,
                              env=env or self.env, timeout=5)

    def test_all_routes_use_fixed_method_and_separate_authentication(self):
        operations = {
            "capabilities": ("GET", "/internal/anthropic-accounts/capabilities", SIDE_KEY, None),
            "accounts": ("GET", "/internal/anthropic-accounts", SIDE_KEY, None),
            "begin": ("POST", "/internal/gateway-management/anthropic/oauth", MANAGEMENT_KEY, {}),
            "callback": ("POST", "/internal/gateway-management/anthropic/oauth/callback", MANAGEMENT_KEY,
                         {"state": "synthetic-state", "code": AUTH_CODE}),
            "status": ("POST", "/internal/gateway-management/anthropic/oauth/status", MANAGEMENT_KEY,
                       {"state": "synthetic-state"}),
        }
        for operation, (method, path, key, payload) in operations.items():
            with self.subTest(operation=operation):
                result = self.shell(operation, payload)
                self.assertEqual(result.returncode, 0, result.stderr)
                wire = (self.root / "request").read_bytes()
                self.assertEqual(wire, (f"{method} {path} HTTP/1.1\r\nAuthorization: Bearer {key}\r\n").encode()
                                 + request_input(payload))
                self.assertEqual(LOGIN.parse_response(result.stdout), {"status": "ok"})
                record = json.loads((self.root / "nc.json").read_text())
                self.assertEqual(record["args"], ["-N", "-w", "9", "127.0.0.1", "8317"])
                for secret in (SIDE_KEY, MANAGEMENT_KEY, AUTH_CODE):
                    self.assertNotIn(secret, json.dumps(record))
                    self.assertNotIn(secret.encode(), result.stdout + result.stderr)

    def test_unknown_operations_cannot_change_endpoint_or_run_shell(self):
        for operation in ("", "credentials", "https://example.com/", "../oauth", "begin; touch forbidden",
                          "$(touch forbidden)", "callback\r\nX-Injected: true"):
            with self.subTest(operation=operation):
                result = self.shell(operation)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.root / "request").exists())

    def test_malformed_secrets_cannot_inject_headers(self):
        for value in (b"", b"A" * 20 + b"\r\nX-Injected: true", b"A" * 20 + b"\nX-Injected: true",
                      b"A" * 32 + b" space", b"A" * 32 + b"\tvalue", b"A" * 32 + b"\x00value",
                      b"A" * 32 + b"\x01value", b"A" * 32 + b"\xffvalue"):
            with self.subTest(value=value):
                self.management_secret.write_bytes(value)
                result = self.shell("callback", {"state": "synthetic-state", "code": AUTH_CODE})
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.root / "request").exists())
                self.assertNotIn(AUTH_CODE.encode(), result.stdout + result.stderr)
                self.assertNotIn(b"X-Injected", result.stdout + result.stderr)

    def test_missing_secret_fails_without_sending_request(self):
        self.management_secret.unlink()
        result = self.shell("begin", {})
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.root / "request").exists())

    @unittest.skipUnless(shutil.which("nc"), "real nc is not installed")
    def test_real_nc_delivers_framed_callback_and_reads_chunked_response(self):
        chunked = b"5\r\n" + JSON_BODY[:5] + b"\r\n" + format(len(JSON_BODY[5:]), "x").encode()
        chunked += b"\r\n" + JSON_BODY[5:] + b"\r\n0\r\n\r\n"
        responses = [response(), response(chunked, ["Transfer-Encoding: chunked"])]
        for raw in responses:
            with self.subTest(chunked=b"Transfer-Encoding" in raw):
                errors, requests = [], []
                with contextlib.closing(socket.socket()) as listener:
                    listener.bind(("127.0.0.1", 0))
                    listener.listen(1)
                    listener.settimeout(4)
                    port = listener.getsockname()[1]

                    def serve():
                        try:
                            connection, _ = listener.accept()
                            with connection:
                                connection.settimeout(4)
                                wire = bytearray()
                                while True:
                                    part = connection.recv(4096)
                                    if not part:
                                        break
                                    wire.extend(part)
                                requests.append(bytes(wire))
                                # Split the HTTP header boundary across separate writes.
                                boundary = raw.index(b"\r\n\r\n") + 3
                                connection.sendall(raw[:boundary])
                                connection.sendall(raw[boundary:])
                        except Exception as error:
                            errors.append(error)

                    server = threading.Thread(target=serve, daemon=True)
                    server.start()
                    payload = {"state": "synthetic-state", "code": AUTH_CODE}
                    result = self.shell("callback", payload,
                                        transport=self.transport.replace("8317", str(port)), env=dict(os.environ))
                    server.join(5)
                    self.assertFalse(server.is_alive())
                    self.assertEqual(errors, [])
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(LOGIN.parse_response(result.stdout), {"status": "ok"})
                self.assertEqual(len(requests), 1)
                headers, body = requests[0].split(b"\r\n\r\n", 1)
                self.assertIn(b"Authorization: Bearer " + MANAGEMENT_KEY.encode(), headers)
                self.assertIn(b"Content-Length: " + str(len(body)).encode(), headers)
                self.assertEqual(json.loads(body), payload)


if __name__ == "__main__":
    unittest.main()
