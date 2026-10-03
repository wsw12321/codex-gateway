#!/usr/bin/env python3
"""Exercise OIDC egress source/destination isolation without site credentials."""

import os
from pathlib import Path
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[2]
ENTRYPOINT = ROOT / "deploy/egress/entrypoint.sh"


class OIDCEgressTests(unittest.TestCase):
    def render(self, enabled="false", host="", relay="", operation="--render-oidc-config", mode=""):
        return subprocess.run(
            ["sh", str(ENTRYPOINT), operation],
            env={**os.environ, "OIDC_ENABLED": enabled, "OIDC_AUTH_HOST": host,
                 "CODEX_RELAY_IP": relay, "CODEX_RELAY_PORT": "3128", "EGRESS_MODE": mode},
            capture_output=True, text=True, timeout=5,
        )

    def test_disabled_does_not_grant_egress(self):
        for host in ("", "project.supabase.co"):
            result = self.render(host=host)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertFalse(any(line and not line.startswith("#")
                                 for line in result.stdout.splitlines()))

    def test_gateway_has_one_exact_host_and_never_uses_model_relay(self):
        for host in ("project.supabase.co", "auth.water555.com"):
            for relay in ("", "10.77.0.2"):
                with self.subTest(host=host, relay=relay):
                    result = self.render("true", host, relay)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout.splitlines(), [
                        f"acl gateway_oidc_upstream dstdomain -n {host}",
                        "http_access allow CONNECT TLS_port gateway_oidc_clients gateway_oidc_upstream",
                        "always_direct allow gateway_oidc_clients",
                    ])
                    provider_rules = self.render("true", host, relay, "--render-config")
                    self.assertEqual(provider_rules.returncode, 0, provider_rules.stderr)
                    self.assertNotIn("gateway_oidc", provider_rules.stdout)

    def test_invalid_host_and_config_injection_fail_before_squid(self):
        invalid = ("", ".supabase.co", "*.supabase.co", "PROJECT.supabase.co",
                   "project.supabase.co.", "project..supabase.co", "localhost",
                   "127.0.0.1", "::1", "project.supabase.co:443", "https://project.supabase.co",
                   "-project.supabase.co", "project-.supabase.co", "a" * 64 + ".com",
                   ".".join(["a" * 63] * 4), "project.supabase.co another.example",
                   "project.supabase.co\nhttp_access allow all", "project.supabase.co\n",
                   "project.supabase.co\r", "project.supabase.co\t", "$(id).supabase.co")
        for host in invalid:
            with self.subTest(host=host):
                result = self.render("true", host)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("OIDC_AUTH_HOST", result.stderr)
                self.assertEqual(result.stdout, "")

    def test_invalid_enable_flag_fails_closed(self):
        for enabled in ("1", "yes", "TRUE", "true\n", "false\nhttp_access allow all"):
            result = self.render(enabled, "project.supabase.co")
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("OIDC_ENABLED", result.stderr)

    def test_explicit_modes_preserve_oidc_direct_route(self):
        for mode in ("direct", "relay", "shadowsocks"):
            with self.subTest(mode=mode):
                result = self.render("true", "auth.example.com", "10.77.0.2", mode=mode)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.splitlines(), [
                    "acl gateway_oidc_upstream dstdomain -n auth.example.com",
                    "http_access allow CONNECT TLS_port gateway_oidc_clients gateway_oidc_upstream",
                    "always_direct allow gateway_oidc_clients",
                ])
                providers = self.render("true", "auth.example.com", "10.77.0.2", "--render-config", mode)
                self.assertEqual(providers.returncode, 0, providers.stderr)
                self.assertNotIn("gateway_oidc", providers.stdout)


if __name__ == "__main__":
    unittest.main()
