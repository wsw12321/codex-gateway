#!/usr/bin/env python3
"""Exercise the sourced OpenRC callbacks without root or a real tunnel."""

import os
from pathlib import Path
import shlex
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
SERVICE = ROOT / "deploy/relay/wg-codex"


class RelayOpenRCTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="relay-openrc-test-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.calls = self.root / "calls"
        mock = self.root / "wg-quick"
        mock.write_text(
            '#!/bin/sh\nset -eu\n'
            'printf "%s\\n" "$@" > "$RELAY_CALLS"\n'
            'exit "${RELAY_EXIT:-0}"\n'
        )
        mock.chmod(0o700)
        source = SERVICE.read_text()
        self.assertEqual(source.count("/usr/bin/wg-quick"), 2)
        self.service = self.root / "wg-codex"
        # Patch only this temporary copy; the fixture can never alter interfaces.
        self.service.write_text(source.replace("/usr/bin/wg-quick", shlex.quote(str(mock))))
        self.env = dict(os.environ, RELAY_SERVICE=str(self.service),
                        RELAY_CALLS=str(self.calls), RELAY_EXIT="0")

    def shell(self, body, options="", **env):
        args = ["sh"] + ([options] if options else [])
        return subprocess.run(args + ["-c", body], env=dict(self.env, **env),
                              text=True, capture_output=True, timeout=10)

    def test_sourcing_preserves_openrc_shell_options(self):
        for options in ("", "-e", "-u", "-eu"):
            with self.subTest(options=options):
                result = self.shell('flags=$-; . "$RELAY_SERVICE"; test "$-" = "$flags"', options)
                self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.calls.exists())

    def test_dependencies_require_network_and_precede_docker(self):
        result = self.shell('''
            need() { printf 'need %s\n' "$*"; }
            before() { printf 'before %s\n' "$*"; }
            . "$RELAY_SERVICE"
            depend
        ''')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.splitlines(), ["need net", "before docker"])
        self.assertFalse(self.calls.exists())

    def run_callback(self, action, exit_code):
        return self.shell('''
            flags=$-
            ebegin() { :; }
            eend() { return "$1"; }
            . "$RELAY_SERVICE"
            "$RELAY_ACTION"
            result=$?
            test "$-" = "$flags" || exit 125
            exit "$result"
        ''', RELAY_ACTION=action, RELAY_EXIT=str(exit_code))

    def test_start_and_stop_use_the_exact_interface(self):
        for action, operation in (("start", "up"), ("stop", "down")):
            with self.subTest(action=action):
                result = self.run_callback(action, 0)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.calls.read_text().splitlines(), [operation, "wg-codex"])

    def test_start_and_stop_propagate_wireguard_failure(self):
        for action, operation in (("start", "up"), ("stop", "down")):
            with self.subTest(action=action):
                result = self.run_callback(action, 7)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertEqual(self.calls.read_text().splitlines(), [operation, "wg-codex"])


if __name__ == "__main__":
    unittest.main()
