#!/usr/bin/env python3
"""Exercise deployment policy with real Compose rendering and a fake daemon."""

import copy
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]


@unittest.skipUnless(shutil.which("docker") and shutil.which("jq"),
                     "Docker Compose CLI and jq are required; no daemon is used")
class RelayValidationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.directory = tempfile.TemporaryDirectory(prefix="relay-policy-")
        cls.addClassCleanup(cls.directory.cleanup)
        cls.root = Path(cls.directory.name)
        shutil.copytree(ROOT / "deploy", cls.root / "deploy",
                        ignore=shutil.ignore_patterns("secrets"))
        (cls.root / "scripts").mkdir()
        shutil.copy2(ROOT / "scripts/validate-compose.sh", cls.root / "scripts")
        shutil.copy2(ROOT / "scripts/validate-cpa-panel.sh", cls.root / "scripts")
        shutil.copytree(ROOT / "internal/server/assets/cpa", cls.root / "internal/server/assets/cpa")
        shutil.copy2(ROOT / "docker-compose.yml", cls.root)
        config = (ROOT / "deploy/env.example").read_text()
        gid = os.getgid() or 1
        replacements = {
            "GATEWAY_DOMAIN": "relay-policy.ci.invalid",
            "GATEWAY_IMAGE_TAG": "a" * 40,
            "GATEWAY_VERSION": "a" * 40,
            "GATEWAY_REVISION": "a" * 40,
            "GATEWAY_SECRET_GID": str(gid),
        }
        config = "\n".join(
            f"{line.split('=', 1)[0]}={replacements[line.split('=', 1)[0]]}"
            if line.split("=", 1)[0] in replacements else line
            for line in config.splitlines()
        ) + "\n"
        (cls.root / ".env").write_text(config)
        cls.env = os.environ.copy()
        # Render only fixture settings even if the invoking shell has site vars.
        for source in (config, (cls.root / "deploy/images.lock.env").read_text()):
            for line in source.splitlines():
                if line and not line.startswith("#") and "=" in line:
                    cls.env.pop(line.split("=", 1)[0], None)

        def render(compose, overlay=False, shadowsocks=False):
            files = ["-f", str(compose)]
            if overlay:
                files += ["-f", str(cls.root / "deploy/oidc.override.yml")]
            if shadowsocks:
                files += ["-f", str(cls.root / "deploy/shadowsocks.override.yml")]
            result = subprocess.run(
                ["docker", "compose", "--profile", "legacy-bridge", "--env-file", str(cls.root / ".env"),
                 "--env-file", str(cls.root / "deploy/images.lock.env"),
                 *files, "config", "--format", "json"],
                env=cls.env, capture_output=True, text=True, timeout=30,
            )
            if result.returncode:
                raise RuntimeError(f"Compose fixture failed: {result.stderr}")
            return json.loads(result.stdout)

        cls.gateway_baseline = render(cls.root / "docker-compose.yml")
        cls.relay_baseline = render(cls.root / "deploy/relay/docker-compose.yml")
        cls.env.update({"OIDC_ENABLED": "true", "OIDC_AUTH_HOST": "staging.supabase.co",
                        "OIDC_ISSUER": "https://staging.supabase.co/auth/v1", "OIDC_CLIENT_ID": "staging-client"})
        cls.oidc_baseline = render(cls.root / "docker-compose.yml", overlay=True)
        for name in ("OIDC_ENABLED", "OIDC_AUTH_HOST", "OIDC_ISSUER", "OIDC_CLIENT_ID"):
            cls.env.pop(name, None)
        cls.env.update({"EGRESS_MODE": "shadowsocks", "SHADOWSOCKS_SERVER": "192.0.2.1"})
        cls.ss_baseline = render(cls.root / "docker-compose.yml", shadowsocks=True)
        cls.env.pop("EGRESS_MODE", None)
        cls.env.pop("SHADOWSOCKS_SERVER", None)
        secrets = cls.root / "deploy/secrets"
        secrets.mkdir()
        for name in cls.oidc_baseline["secrets"] | cls.ss_baseline["secrets"]:
            secret = secrets / name
            secret.write_text("A" * 43 if name == "gateway_api_key_encryption_key"
                              else f"non-production-fixture-{name}")
            secret.chmod(0o640)
            if gid != os.getgid():
                os.chown(secret, -1, gid)

        cls.gateway_json = cls.root / "gateway.json"
        cls.relay_json = cls.root / "relay.json"
        cls.run_log = cls.root / "docker-runs.jsonl"
        cls.egress_config = cls.root / "deploy/egress/squid.conf"
        cls.egress_baseline = cls.egress_config.read_text()
        cls.entrypoint = cls.root / "deploy/egress/entrypoint.sh"
        cls.entrypoint_baseline = cls.entrypoint.read_text()
        cls.b_config = cls.root / "deploy/relay/squid.conf"
        cls.b_baseline = cls.b_config.read_text()
        compose_stub = cls.root / "scripts/compose.sh"
        compose_stub.write_text(
            '#!/bin/sh\nset -eu\ncat "$RELAY_TEST_GATEWAY_JSON"\n'
        )
        compose_stub.chmod(0o755)
        stub_dir = cls.root / "bin"
        stub_dir.mkdir()
        docker_stub = stub_dir / "docker"
        docker_stub.write_text('''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys
args = sys.argv[1:]
if args and args[0] == "compose":
    sys.stdout.write(Path(os.environ["RELAY_TEST_B_JSON"]).read_text())
elif args and args[0] == "run":
    with open(os.environ["RELAY_TEST_RUN_LOG"], "a") as output:
        output.write(json.dumps(args) + "\\n")
    match = os.environ.get("RELAY_TEST_FAIL_PARSE", "")
    if match and match in " ".join(args):
        sys.exit(42)
else:
    sys.exit("unexpected Docker operation in policy test")
''')
        docker_stub.chmod(0o755)
        cls.env.update({
            "PATH": str(stub_dir) + os.pathsep + cls.env["PATH"],
            "RELAY_TEST_GATEWAY_JSON": str(cls.gateway_json),
            "RELAY_TEST_B_JSON": str(cls.relay_json),
            "RELAY_TEST_RUN_LOG": str(cls.run_log),
        })

    def setUp(self):
        self.gateway = copy.deepcopy(self.gateway_baseline)
        self.relay = copy.deepcopy(self.relay_baseline)
        self.egress_config.write_text(self.egress_baseline)
        self.entrypoint.write_text(self.entrypoint_baseline)
        self.b_config.write_text(self.b_baseline)
        self.run_log.write_text("")

    def validate(self, oidc=False, **env):
        self.gateway_json.write_text(json.dumps(self.gateway))
        self.relay_json.write_text(json.dumps(self.relay))
        return subprocess.run(
            ["sh", str(self.root / "scripts/validate-compose.sh"), *(["--oidc"] if oidc else [])],
            env={**self.env, **env}, capture_output=True, text=True, timeout=15,
        )

    def test_valid_fixture_parses_all_a_modes_and_b_without_site_networks(self):
        result = self.validate()
        self.assertEqual(result.returncode, 0, result.stderr)
        runs = [json.loads(line) for line in self.run_log.read_text().splitlines()]
        self.assertEqual(len(runs), 6)  # Legacy off/on, explicit direct/SS, B, Caddy.
        for args in runs:
            self.assertEqual(args[args.index("--network") + 1], "none")
            self.assertIn("--read-only", args)
        self.assertIn("CODEX_RELAY_IP=", runs[0])
        self.assertIn("CODEX_RELAY_IP=10.77.0.2", runs[1])
        self.assertIn("EGRESS_MODE=direct", runs[2])
        self.assertIn("EGRESS_MODE=shadowsocks", runs[3])
        self.assertIn("/usr/sbin/squid", runs[4])
        self.assertEqual(runs[4][runs[4].index("--ulimit") + 1], "nofile=4096:4096")

    def test_oidc_overlay_uses_only_its_file_secret_and_exact_internal_proxy(self):
        self.gateway = copy.deepcopy(self.oidc_baseline)
        result = self.validate(oidc=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        runs = [json.loads(line) for line in self.run_log.read_text().splitlines()]
        self.assertEqual(len(runs), 9)
        for index, mode in enumerate(("direct", "relay", "shadowsocks"), start=4):
            self.assertIn("OIDC_ENABLED=true", runs[index])
            self.assertIn("OIDC_AUTH_HOST=staging.supabase.co", runs[index])
            self.assertIn(f"EGRESS_MODE={mode}", runs[index])
            self.assertEqual(runs[index][runs[index].index("--network") + 1], "none")

    def test_shadowsocks_preserves_the_transport_boundary(self):
        self.gateway = copy.deepcopy(self.ss_baseline)
        result = self.validate()
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_shadowsocks_security_mutations_fail_before_running_images(self):
        mutations = [
            lambda c: c["networks"]["ss_internal"].update(internal=False),
            lambda c: c["services"]["gateway"]["networks"].update(ss_internal={}),
            lambda c: c["services"]["ss-egress"].update(read_only=False),
            lambda c: c["services"]["ss-egress"].update(user="0:1000"),
            lambda c: c["services"]["ss-egress"].update(cap_add=["NET_ADMIN"]),
            lambda c: c["services"]["ss-egress"].update(command=["-ext-ctl", "0.0.0.0:9090"]),
            lambda c: c["services"]["ss-egress"]["networks"].update(compat_internal={}),
            lambda c: c["services"]["ss-egress"]["networks"]["ss_internal"].update(ipv4_address="172.28.50.4"),
            lambda c: c["services"]["ss-egress"]["environment"].update(SHADOWSOCKS_PASSWORD="inline-secret"),
            lambda c: c["services"]["ss-egress"]["build"]["args"].update(MIHOMO_IMAGE="metacubex/mihomo:latest"),
            lambda c: c["services"]["ss-egress"]["healthcheck"].update(test=["CMD", "curl", "https://example.com"]),
            lambda c: c["services"]["egress-allowlist"]["depends_on"]["ss-egress"].update(condition="service_started"),
            lambda c: c["services"]["gateway"]["secrets"].append({"source": "shadowsocks_password"}),
            lambda c: c["services"]["ss-egress"].update(tmpfs=["/run/mihomo:mode=0777"]),
        ]
        for mutate in mutations:
            with self.subTest(mutation=mutate):
                self.gateway = copy.deepcopy(self.ss_baseline)
                mutate(self.gateway)
                self.run_log.write_text("")
                result = self.validate()
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.run_log.read_text(), "")

    def test_other_modes_cannot_load_shadowsocks_transport(self):
        for mode in ("direct", "relay"):
            self.gateway = copy.deepcopy(self.ss_baseline)
            self.gateway["services"]["egress-allowlist"]["environment"].update(
                EGRESS_MODE=mode, CODEX_RELAY_IP="10.77.0.2")
            result = self.validate()
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(self.run_log.read_text(), "")

    def test_oidc_rejects_host_mismatch_global_proxy_and_other_secret_readers(self):
        mutations = [
            lambda s: s["gateway"]["environment"].update(OIDC_PROXY_URL="http://egress-allowlist:3128"),
            lambda s: s["gateway"]["environment"].update(HTTPS_PROXY="http://172.28.30.4:3128"),
            lambda s: s["gateway"]["environment"].update(http_proxy="http://172.28.30.4:3128"),
            lambda s: s["gateway"]["environment"].update(OIDC_CLIENT_SECRET="inline-secret"),
            lambda s: s["gateway"]["environment"].update(OIDC_ISSUER="https://other.supabase.co/auth/v1"),
            lambda s: s["egress-allowlist"]["environment"].update(OIDC_AUTH_HOST="staging.supabase.co\n"),
            lambda s: s["egress-allowlist"]["environment"].update(OIDC_ENABLED="false"),
            lambda s: s["codex-compat"]["secrets"].append({"source": "oidc_client_secret"}),
        ]
        for mutate in mutations:
            with self.subTest(mutation=mutate):
                self.gateway = copy.deepcopy(self.oidc_baseline)
                mutate(self.gateway["services"])
                result = self.validate(oidc=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("OIDC requires its optional overlay", result.stderr)
                self.assertEqual(self.run_log.read_text(), "")

    def test_default_deployment_requires_no_oidc_secret(self):
        secret = self.root / "deploy/secrets/oidc_client_secret"
        content = secret.read_text()
        secret.unlink()
        try:
            result = self.validate()
            self.assertEqual(result.returncode, 0, result.stderr)
        finally:
            secret.write_text(content)
            secret.chmod(0o640)
            if (os.getgid() or 1) != os.getgid():
                os.chown(secret, -1, os.getgid() or 1)

    def test_caddy_runtime_error_logs_require_callback_query_redaction(self):
        caddyfile = self.root / "deploy/Caddyfile"
        original = caddyfile.read_text()
        try:
            caddyfile.write_text(original.replace('request>uri regexp "[?].*$" ""',
                                                  '# request URI redaction removed'))
            result = self.validate()
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Caddy runtime error logs must redact", result.stderr)
            self.assertEqual(self.run_log.read_text(), "")
        finally:
            caddyfile.write_text(original)

    def test_pricing_requires_all_reviewed_native_models_and_preserves_codex(self):
        environment = self.gateway["services"]["gateway"]["environment"]
        pricing = json.loads(environment["GATEWAY_USAGE_PRICING_JSON"])
        native = {
            "gemini-pro-agent", "gemini-3.1-pro-low", "gemini-3-flash",
            "gemini-3.6-flash-high", "gemini-3.7-flash-high", "gemini-3.8-flash-high",
            "gemini-3.1-flash-lite", "gemini-3.5-flash-lite", "gpt-6.1-sol",
        }
        codex = {
            "gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-5.6-luna",
            "gpt-5.6-sol", "gpt-5.6-terra", "gpt-6-astra", "gpt-6-luna", "gpt-6-sol",
        }
        self.assertEqual(set(pricing["models"]), native | codex | {"codex-auto-review"})
        for model in sorted(native):
            with self.subTest(missing=model):
                changed = copy.deepcopy(pricing)
                del changed["models"][model]
                environment["GATEWAY_USAGE_PRICING_JSON"] = json.dumps(changed)
                result = self.validate()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("strict reviewed schema v2 pricing catalog", result.stderr)
                self.assertEqual(self.run_log.read_text(), "")

    def test_pricing_rejects_retired_names_and_invalid_native_billing_shapes(self):
        environment = self.gateway["services"]["gateway"]["environment"]
        pricing = json.loads(environment["GATEWAY_USAGE_PRICING_JSON"])

        def add_retired(model):
            return lambda p: p["models"].update({model: p["models"]["gemini-pro-agent"]})

        mutations = [
            (model, add_retired(model)) for model in (
                "gemini-3.1-pro-high", "gemini-3.6-flash-medium",
                "gemini-3.7-flash-medium", "gemini-3.8-flash-medium",
                "gemini-3.1-pro-preview", "gemini-3.1-flash-lite-preview",
            )
        ] + [
            ("Gemini Flex", lambda p: p["models"]["gemini-3.1-pro-low"]["service_tiers"].update(
                flex=p["models"]["gemini-3.1-pro-low"]["service_tiers"]["standard"])),
            ("Gemini cache write", lambda p: p["models"]["gemini-3.1-flash-lite"]["service_tiers"]["standard"]["short"].update(
                cache_write_usd_per_million="0.25")),
            ("Pro threshold", lambda p: p["models"]["gemini-3.1-pro-low"].update(
                long_context_threshold_tokens=272000)),
            ("Sol threshold", lambda p: p["models"]["gpt-6.1-sol"].update(
                long_context_threshold_tokens=200000)),
            ("Sol cache write", lambda p: p["models"]["gpt-6.1-sol"]["service_tiers"]["standard"]["short"].pop(
                "cache_write_usd_per_million")),
        ]
        for name, mutate in mutations:
            with self.subTest(mutation=name):
                changed = copy.deepcopy(pricing)
                mutate(changed)
                environment["GATEWAY_USAGE_PRICING_JSON"] = json.dumps(changed)
                result = self.validate()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("strict reviewed schema v2 pricing catalog", result.stderr)
                self.assertEqual(self.run_log.read_text(), "")

    def test_b_descriptor_limit_cannot_be_removed_or_increased(self):
        for limit in (None, {}, 4096, {"soft": 4096}, {"hard": 4096},
                      {"soft": 1048576, "hard": 1048576},
                      {"soft": 4096, "hard": 1048576},
                      {"soft": -1, "hard": -1}):
            with self.subTest(limit=limit):
                self.relay = copy.deepcopy(self.relay_baseline)
                service = self.relay["services"]["relay"]
                if limit is None:
                    service.pop("ulimits", None)
                else:
                    service["ulimits"] = {"nofile": limit}
                result = self.validate()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("B relay nofile soft and hard limits", result.stderr)
                self.assertEqual(self.run_log.read_text(), "")

    def test_b_object_memory_cache_must_stay_disabled(self):
        for directive in ("", "cache_mem 256 MB", "cache_mem 0 MB\ncache_mem 256 MB"):
            with self.subTest(directive=directive):
                self.b_config.write_text(self.b_baseline.replace("cache_mem 0 MB", directive))
                result = self.validate()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("B CONNECT-only relay must disable", result.stderr)
                self.assertEqual(self.run_log.read_text(), "")

    def test_json_environment_injection_fails_before_any_proxy_is_run(self):
        for key, value in (
            ("CODEX_RELAY_IP", "10.77.0.2\n"),
            ("CODEX_RELAY_IP", "10.77.0.2\nnever_direct deny all"),
            ("CODEX_RELAY_PORT", "3128\n"),
            ("CODEX_RELAY_PORT", "3128\r"),
            ("CODEX_RELAY_PORT", "3128; true"),
        ):
            with self.subTest(key=key, value=value):
                self.gateway = copy.deepcopy(self.gateway_baseline)
                self.gateway["services"]["egress-allowlist"]["environment"][key] = value
                result = self.validate()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("CODEX_RELAY_IP must be empty", result.stderr)
                self.assertEqual(self.run_log.read_text(), "")

    def test_mount_or_entrypoint_override_is_rejected(self):
        for service, mutation in (
            ("a", lambda s: s["volumes"][0].update(read_only=False)),
            ("a", lambda s: s.update(entrypoint=["/usr/local/bin/entrypoint.sh"])),
            ("a", lambda s: s.update(command=[])),
            ("a", lambda s: s["volumes"].append({
                "type": "bind", "source": "/tmp/extra", "target": "/run"})),
            ("b", lambda s: s.update(network_mode="bridge")),
            ("b", lambda s: s["volumes"][0].update(read_only=False)),
        ):
            with self.subTest(service=service, mutation=mutation):
                self.gateway = copy.deepcopy(self.gateway_baseline)
                self.relay = copy.deepcopy(self.relay_baseline)
                selected = (self.gateway["services"]["egress-allowlist"]
                            if service == "a" else self.relay["services"]["relay"])
                mutation(selected)
                result = self.validate()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("A/B Squid entrypoint, mounts", result.stderr)
                self.assertEqual(self.run_log.read_text(), "")

    def test_static_config_cannot_bypass_parent(self):
        for directive in ("always_direct allow all", "never_direct deny antigravity_clients",
                          "cache_peer_access codex_relay deny antigravity_clients"):
            with self.subTest(directive=directive):
                self.egress_config.write_text(self.egress_baseline + f"\n{directive}\n")
                result = self.validate()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("generated relay routing fragment", result.stderr)
                self.assertEqual(self.run_log.read_text(), "")

    def test_both_client_groups_require_parent_access_and_no_direct_fallback(self):
        for client in ("codex_clients", "antigravity_clients"):
            for directive in (f"cache_peer_access codex_relay allow {client}",
                              f"never_direct allow {client}"):
                with self.subTest(directive=directive):
                    self.entrypoint.write_text(self.entrypoint_baseline.replace(
                        f"'{directive}'", f"'# removed {directive}'"))
                    result = self.validate()
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("unique mandatory parent without direct fallback", result.stderr)
                    self.assertEqual(self.run_log.read_text(), "")

    def test_a_keeps_provider_destination_rules_separate(self):
        for client, destinations in (("codex_clients", "codex_upstreams"),
                                     ("antigravity_clients", "antigravity_upstreams")):
            with self.subTest(client=client):
                self.egress_config.write_text(self.egress_baseline.replace(
                    f"http_access allow CONNECT {client} {destinations}",
                    f"http_access allow CONNECT {client}"))
                result = self.validate()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("separate Codex and Antigravity destination rules", result.stderr)
                self.assertEqual(self.run_log.read_text(), "")

    def test_a_must_not_authorize_literal_ips_via_reverse_dns(self):
        for destinations in ("codex_upstreams", "antigravity_upstreams"):
            with self.subTest(destinations=destinations):
                self.egress_config.write_text(self.egress_baseline.replace(
                    f"acl {destinations} dstdomain -n ",
                    f"acl {destinations} dstdomain "))
                result = self.validate()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("source and destination ACLs must equal the reviewed exact lists", result.stderr)
                self.assertEqual(self.run_log.read_text(), "")

    def test_b_must_match_both_exact_destination_lists_without_reverse_dns(self):
        for original, replacement in (
            ("auth.openai.com chatgpt.com", ".openai.com .chatgpt.com"),
            ("accounts.google.com", ".google.com"),
            ("cloudcode-pa.googleapis.com ", ".googleapis.com "),
            ("oauth2.googleapis.com ", ""),
            ("playwright-verizon.azureedge.net", "playwright-verizon.azureedge.net example.com"),
            ("antigravity_upstreams dstdomain -n", "antigravity_upstreams dstdomain"),
        ):
            with self.subTest(original=original, replacement=replacement):
                self.b_config.write_text(self.b_baseline.replace(original, replacement))
                result = self.validate()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("B must accept only A", result.stderr)
                self.assertEqual(self.run_log.read_text(), "")

    def test_both_client_logs_must_identify_the_forwarding_path(self):
        for name in ("codex_destinations", "bridge_destinations"):
            with self.subTest(name=name):
                self.egress_config.write_text(self.egress_baseline.replace(
                    f"logformat {name} %ts.%03tu %ru %>Hs %Sh/%<a",
                    f"logformat {name} %ts.%03tu %ru %>Hs"))
                result = self.validate()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("CONNECT logs must include destination, status", result.stderr)
                self.assertEqual(self.run_log.read_text(), "")

    def test_image_parse_failure_stops_validation(self):
        result = self.validate(RELAY_TEST_FAIL_PARSE="CODEX_RELAY_IP=10.77.0.2")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("validated", result.stdout)
        self.assertEqual(len(self.run_log.read_text().splitlines()), 2)


if __name__ == "__main__":
    unittest.main()
