#!/usr/bin/env python3
"""Security boundaries and deadlines for Claude SSH authorization."""

import contextlib
import importlib.util
import io
from pathlib import Path
import unittest
from unittest import mock
from urllib.parse import urlencode


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("claude_login_protocol", ROOT / "scripts/claude-login.py")
LOGIN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(LOGIN)
STATE = "0123456789abcdef" * 2
CODE = "synthetic-private-code"
URL = "https://claude.ai/oauth/authorize?" + urlencode({"state": STATE, "code": "true"})
CALLBACK = "http://localhost:54545/callback?" + urlencode({"state": STATE, "code": CODE})
ACCOUNT = {"id": "0123456789abcdef", "masked_email": "u***@example.com", "status": "available"}


class ClaudeURLTests(unittest.TestCase):
    def test_only_https_claude_authorization_with_matching_unique_state(self):
        self.assertEqual(LOGIN.validate_authorization({"url": URL, "state": STATE}), (URL, STATE))
        urls = [
            URL.replace("https:", "http:"), URL.replace("claude.ai", "claude.ai.evil.invalid"),
            URL.replace("claude.ai", "evil.invalid@claude.ai"),
            URL.replace("claude.ai", "claude.ai@evil.invalid"),
            URL.replace("claude.ai", "claude.ai:444"), URL.replace("claude.ai", "claude.ai."),
            URL.replace("claude.ai", "clаude.ai"), URL.replace("claude.ai", "claude.ai\\evil.invalid"),
            URL.replace(STATE, "f" * 32), URL + "&state=" + STATE,
            URL + "&st%61te=" + STATE, URL + "#", URL + "#fragment", URL + "\n",
            "\x1b[31m" + URL, " " + URL, URL + "&error=access_denied", URL + "&x=%GG",
            URL + "&x=" + "x" * LOGIN.MAX_CALLBACK,
        ]
        for url in urls:
            with self.subTest(url=url[:180]):
                with self.assertRaises(LOGIN.LoginError):
                    LOGIN.validate_authorization({"url": url, "state": STATE})

    def test_rejects_bad_state_and_schema_before_display(self):
        for state in (None, False, 5, [], "", "a", "a" * 129, "a.." * 10,
                      "../" + STATE, STATE + "\n", STATE + "\x1b", "é" * 32):
            with self.subTest(state=state):
                with self.assertRaises(LOGIN.LoginError):
                    LOGIN.validate_authorization({"url": URL, "state": state})
        for url in (None, [], 12, False):
            with self.assertRaises(LOGIN.LoginError):
                LOGIN.validate_authorization({"url": url, "state": STATE})

    def test_accepts_only_canonical_loopback_callbacks(self):
        for host in ("localhost", "127.0.0.1", "[::1]"):
            self.assertEqual(LOGIN.parse_callback(CALLBACK.replace("localhost", host), STATE), CODE)
        encoded = "http://localhost:54545/callback?state=" + STATE + "&code=code%2Bwith%2Fsymbols%3D"
        self.assertEqual(LOGIN.parse_callback(encoded, STATE), "code+with/symbols=")

    def test_rejects_duplicate_mismatched_or_error_callbacks(self):
        suffixes = ["&code=" + CODE, "&co%64e=another", "&state=" + STATE,
                    "&state=different", "&error=access_denied", "&error_description=private",
                    "&scope=extra", "#", "#" + STATE, "&code", "&broken"]
        values = [CALLBACK + suffix for suffix in suffixes]
        values += [CALLBACK.replace(STATE, "a" * 32), CALLBACK.replace("code=", "error="),
                   CALLBACK.replace(CODE, ""), CALLBACK.replace("state=" + STATE + "&", ""),
                   CALLBACK.replace(CODE, "x" * 8193), CALLBACK.replace(CODE, "abc%23" + STATE),
                   CALLBACK.replace(CODE, "abc%00"), CALLBACK.replace(CODE, "abc%0a"),
                   CALLBACK.replace(CODE, "%FF"), CALLBACK.replace(CODE, "%a"),
                   CALLBACK.replace(CODE, "ab+cd"), CALLBACK.replace(CODE, "abc%20"),
                   CALLBACK.replace(CODE, "%22" * LOGIN.MAX_CALLBACK)]
        for value in values:
            with self.subTest(value=value[:160]):
                with self.assertRaises(LOGIN.LoginError):
                    LOGIN.parse_callback(value, STATE)

    def test_rejects_callback_url_confusion_without_accessing_it(self):
        for host in ("evil.invalid", "localhost.evil.invalid", "127.0.0.1.evil.invalid",
                     "user@localhost", "user:password@localhost", "localhost.",
                     "2130706433", "127.1", "[::ffff:127.0.0.1]", "localhost%00.evil.invalid"):
            with self.subTest(host=host):
                with self.assertRaises(LOGIN.LoginError):
                    LOGIN.parse_callback(CALLBACK.replace("localhost", host), STATE)
        for value in (CALLBACK.replace("http:", "https:"), CALLBACK.replace("54545", "80"),
                      CALLBACK.replace(":54545", ""), CALLBACK.replace("54545", "054545"),
                      CALLBACK.replace("54545", "bad"), CALLBACK.replace("/callback", "/callback/"),
                      CALLBACK.replace("/callback", "/%63allback"), CALLBACK.replace("/callback", "/x/../callback"),
                      CALLBACK.replace("/callback", "//callback"), CALLBACK + "\r", "\t" + CALLBACK):
            with self.subTest(value=value):
                with self.assertRaises(LOGIN.LoginError):
                    LOGIN.parse_callback(value, STATE)


class Clock:
    def __init__(self):
        self.now = 0
        self.sleeps = []

    def monotonic(self):
        return self.now

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.now += seconds


class Client:
    def __init__(self, statuses, clock):
        self.calls = []
        self.statuses = iter(statuses)
        self.clock = clock
        self.callback_failure = None
        self.status_duration = 0

    def request(self, operation, payload=None, timeout=10):
        self.calls.append((operation, payload, timeout))
        if operation == "capabilities":
            return {"protocol": "upstream_account_access_v1", "anthropic_messages": "anthropic_messages_v1"}
        if operation == "begin":
            return {"url": URL, "state": STATE}
        if operation == "callback":
            if self.callback_failure:
                raise self.callback_failure
            return {"status": "ok"}
        if operation == "status":
            self.clock.now += min(timeout, self.status_duration)
            result = next(self.statuses, {"status": "wait"})
            if isinstance(result, Exception):
                raise result
            return result
        if operation == "accounts":
            return {"accounts": [ACCOUNT]}
        raise AssertionError(operation)

    def verify_permissions(self):
        self.calls.append(("permissions", None, 10))


class ClaudeLifecycleTests(unittest.TestCase):
    def setUp(self):
        self.clock = Clock()
        self.addCleanup(mock.patch.stopall)
        mock.patch.object(LOGIN.time, "monotonic", self.clock.monotonic).start()
        mock.patch.object(LOGIN.time, "sleep", self.clock.sleep).start()
        self.terminal = mock.Mock()
        self.terminal.callback.return_value = CODE
        self.output = io.StringIO()
        self.output_context = contextlib.redirect_stdout(self.output)
        self.output_context.__enter__()
        self.addCleanup(self.output_context.__exit__, None, None, None)

    def test_wait_then_success_and_a_lost_callback_response_never_resubmits(self):
        client = Client([LOGIN.LoginError("timeout"), {"status": "wait"}, {"status": "ok"}], self.clock)
        client.callback_failure = LOGIN.LoginError("timeout")
        login = LOGIN.Login(client)
        login.run(self.terminal)
        self.assertTrue(login.saved)
        self.assertEqual([call[0] for call in client.calls],
                         ["capabilities", "begin", "callback", "status", "status", "status", "permissions", "accounts"])
        self.assertEqual(self.clock.sleeps, [2, 2])
        self.terminal.callback.assert_called_once_with(STATE, 300)
        self.assertNotIn(CODE, self.output.getvalue())
        self.assertIn("authorization completed", self.output.getvalue())

    def test_polling_has_absolute_two_minute_deadline_including_slow_requests(self):
        client = Client([], self.clock)
        client.status_duration = 10
        login = LOGIN.Login(client)
        with self.assertRaisesRegex(LOGIN.LoginError, "Timed out"):
            login.run(self.terminal)
        self.assertTrue(login.submitted)
        self.assertFalse(login.saved)
        self.assertEqual(self.clock.now, 120)
        operations = [call[0] for call in client.calls]
        self.assertEqual(operations.count("callback"), 1)
        self.assertNotIn("permissions", operations)
        self.assertNotIn("accounts", operations)

    def test_last_poll_request_uses_only_remaining_time(self):
        client = Client([], self.clock)
        client.status_duration = 9
        with self.assertRaises(LOGIN.LoginError):
            LOGIN.Login(client).run(self.terminal)
        self.assertEqual(client.calls[-1][0], "status")
        self.assertEqual(client.calls[-1][2], 10)
        self.assertEqual(self.clock.now, 120)
        # A ten-second request + two-second polling interval leaves only one
        # second for the final request when the total budget is 121 seconds.
        self.clock.now = 0
        client = Client([], self.clock)
        client.status_duration = 10
        with mock.patch.object(LOGIN, "STATUS_TIMEOUT", 121):
            with self.assertRaises(LOGIN.LoginError):
                LOGIN.Login(client).run(self.terminal)
        self.assertEqual(client.calls[-1][2], 1)
        self.assertEqual(self.clock.now, 121)

    def test_error_and_unknown_status_cannot_claim_saved_credentials(self):
        for result in ({"status": "error"}, {"status": "unexpected"}, {}, {"status": 1}):
            with self.subTest(result=result):
                client = Client([result], self.clock)
                login = LOGIN.Login(client)
                with self.assertRaises(LOGIN.LoginError):
                    login.run(self.terminal)
                self.assertFalse(login.saved)
                self.assertNotIn("permissions", [call[0] for call in client.calls])

    def test_capability_failure_never_begins_oauth(self):
        for capability in ({}, {"protocol": "upstream_account_access_v1"},
                           {"anthropic_messages": "anthropic_messages_v1"}):
            client = mock.Mock()
            client.request.return_value = capability
            with self.assertRaises(LOGIN.LoginError):
                LOGIN.Login(client).run(self.terminal)
            client.request.assert_called_once_with("capabilities")


class ClaudeAccountOutputTests(unittest.TestCase):
    def test_only_masked_fields_and_actual_cpa_statuses_are_displayed(self):
        for status in ("available", "unavailable"):
            account = dict(ACCOUNT, status=status, email="private@example.com", access_token=CODE)
            lines = LOGIN.account_lines({"accounts": [account]})
            self.assertEqual(lines, [f"  0123456789abcdef  u***@example.com  {status}"])
            self.assertNotIn(CODE, str(lines))
            self.assertNotIn("private@example.com", str(lines))

    def test_rejects_unmasked_or_terminal_control_data_and_duplicate_ids(self):
        accounts = [dict(ACCOUNT, masked_email="private@example.com"),
                    dict(ACCOUNT, masked_email="u***@example.com\x1b[31m"),
                    dict(ACCOUNT, masked_email="u***@example.com\nprivate"),
                    dict(ACCOUNT, id=CODE), dict(ACCOUNT, status=CODE),
                    dict(ACCOUNT, status={"error": CODE}), None, "private"]
        for account in accounts:
            with self.subTest(account=account):
                with self.assertRaises(LOGIN.LoginError):
                    LOGIN.account_lines({"accounts": [account]})
        for result in ({}, {"accounts": []}, {"accounts": None}, {"accounts": [ACCOUNT, ACCOUNT]}):
            with self.assertRaises(LOGIN.LoginError):
                LOGIN.account_lines(result)


if __name__ == "__main__":
    unittest.main()
