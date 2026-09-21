#!/usr/bin/env python3
"""Validate the real relay configuration renderer without Docker or secrets."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
ENTRYPOINT = ROOT / "deploy/egress/entrypoint.sh"


class CodexRelayTests(unittest.TestCase):
    def render(self, ip=None, port=None, *args):
        env = os.environ.copy()
        env.pop("CODEX_RELAY_IP", None)
        env.pop("CODEX_RELAY_PORT", None)
        if ip is not None:
            env["CODEX_RELAY_IP"] = ip
        if port is not None:
            env["CODEX_RELAY_PORT"] = port
        return subprocess.run(
            ["sh", str(ENTRYPOINT), *(args or ("--render-config",))],
            env=env, capture_output=True, text=True, timeout=5,
        )

    def test_unconfigured_deployment_has_no_parent_or_direct_prohibition(self):
        for ip in (None, ""):
            with self.subTest(ip=ip):
                result = self.render(ip)
                self.assertEqual(result.returncode, 0, result.stderr)
                directives = [line for line in result.stdout.splitlines()
                              if line.strip() and not line.lstrip().startswith("#")]
                self.assertEqual(directives, [])

    def test_enabled_relay_is_the_only_parent_and_required_for_codex(self):
        for ip in ("10.77.0.2", "127.0.0.1", "255.255.255.255", "0.0.0.0"):
            for port, expected in ((None, "3128"), ("", "3128"), ("1", "1"),
                                   ("3128", "3128"), ("65535", "65535")):
                with self.subTest(ip=ip, port=port):
                    result = self.render(ip, port)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    directives = [line for line in result.stdout.splitlines()
                                  if line.strip() and not line.lstrip().startswith("#")]
                    self.assertEqual(directives, [
                        f"cache_peer {ip} parent {expected} 0 no-query default name=codex_relay",
                        "cache_peer_access codex_relay allow codex_clients",
                        "cache_peer_access codex_relay deny all",
                        "never_direct allow codex_clients",
                        "never_direct deny all",
                    ])

    def test_invalid_addresses_fail_before_proxy_start(self):
        invalid = ("localhost", "::1", "10.77.0", "10.77.0.2.1", "256.0.0.1",
                   "-1.0.0.1", "01.77.0.2", "10..0.2", "10.77.0.2/32",
                   " 10.77.0.2", "10.77.0.2 ", "10.77.0.2\n",
                   "10.77.0.2\nnever_direct deny all", "10.77.0.2\rnever_direct deny all",
                   "10.77.0.2\tparent", "10.77.0.2#", "1e1.77.0.2")
        for ip in invalid:
            for args in (("--render-config",), ("-f", "/nonexistent/squid.conf")):
                with self.subTest(ip=ip, args=args):
                    result = self.render(ip, None, *args)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("CODEX_RELAY_IP", result.stderr)
                    self.assertEqual(result.stdout, "")

    def test_invalid_ports_fail_even_when_relay_is_disabled(self):
        invalid = ("0", "65536", "999999999999999999999999", "-1", "+3128",
                   "03128", "3.128", "3128 ", " 3128", "3128\n", "https",
                   "3128\nnever_direct deny all", "3128\rnever_direct deny all",
                   "3128\t0", "3128; true")
        for port in invalid:
            for ip in ("", "10.77.0.2"):
                for args in (("--render-config",), ("-f", "/nonexistent/squid.conf")):
                    with self.subTest(ip=ip, port=port, args=args):
                        result = self.render(ip, port, *args)
                        self.assertNotEqual(result.returncode, 0)
                        self.assertIn("CODEX_RELAY_PORT", result.stderr)
                        self.assertEqual(result.stdout, "")

    def test_shell_injection_is_data_and_cannot_execute(self):
        with tempfile.TemporaryDirectory(prefix="codex-relay-injection-") as directory:
            marker = Path(directory) / "executed"
            for value in (f"$(touch {marker})", f"`touch {marker}`", f"1; touch {marker}"):
                for ip, port in ((value, "3128"), ("10.77.0.2", value)):
                    with self.subTest(ip=ip, port=port):
                        result = self.render(ip, port)
                        self.assertNotEqual(result.returncode, 0)
                        self.assertFalse(marker.exists())


if __name__ == "__main__":
    unittest.main()
