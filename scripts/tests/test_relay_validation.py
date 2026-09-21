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

        def render(compose):
            result = subprocess.run(
                ["docker", "compose", "--env-file", str(cls.root / ".env"),
                 "--env-file", str(cls.root / "deploy/images.lock.env"),
                 "-f", str(compose), "config", "--format", "json"],
                env=cls.env, capture_output=True, text=True, timeout=30,
            )
            if result.returncode:
                raise RuntimeError(f"Compose fixture failed: {result.stderr}")
            return json.loads(result.stdout)

        cls.gateway_baseline = render(cls.root / "docker-compose.yml")
        cls.relay_baseline = render(cls.root / "deploy/relay/docker-compose.yml")
        secrets = cls.root / "deploy/secrets"
        secrets.mkdir()
        for name in cls.gateway_baseline["secrets"]:
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
        self.b_config.write_text(self.b_baseline)
        self.run_log.write_text("")

    def validate(self, **env):
        self.gateway_json.write_text(json.dumps(self.gateway))
        self.relay_json.write_text(json.dumps(self.relay))
        return subprocess.run(
            ["sh", str(self.root / "scripts/validate-compose.sh")],
            env={**self.env, **env}, capture_output=True, text=True, timeout=15,
        )

    def test_valid_fixture_parses_both_a_modes_and_b_without_site_networks(self):
        result = self.validate()
        self.assertEqual(result.returncode, 0, result.stderr)
        runs = [json.loads(line) for line in self.run_log.read_text().splitlines()]
        self.assertEqual(len(runs), 4)  # A disabled, A enabled, B, then Caddy.
        for args in runs:
            self.assertEqual(args[args.index("--network") + 1], "none")
            self.assertIn("--read-only", args)
        self.assertIn("CODEX_RELAY_IP=", runs[0])
        self.assertIn("CODEX_RELAY_IP=10.77.0.2", runs[1])
        self.assertIn("/usr/sbin/squid", runs[2])
        self.assertEqual(runs[2][runs[2].index("--ulimit") + 1], "nofile=4096:4096")

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

    def test_static_config_cannot_bypass_parent_or_widen_b_destinations(self):
        self.egress_config.write_text(self.egress_baseline + "\nalways_direct allow all\n")
        result = self.validate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("generated relay routing fragment", result.stderr)
        self.assertEqual(self.run_log.read_text(), "")
        self.egress_config.write_text(self.egress_baseline)
        self.b_config.write_text(self.b_baseline.replace(
            "auth.openai.com chatgpt.com", ".openai.com .chatgpt.com"))
        result = self.validate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("B must accept only A", result.stderr)
        self.assertEqual(self.run_log.read_text(), "")

    def test_image_parse_failure_stops_validation(self):
        result = self.validate(RELAY_TEST_FAIL_PARSE="CODEX_RELAY_IP=10.77.0.2")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("validated", result.stdout)
        self.assertEqual(len(self.run_log.read_text().splitlines()), 2)


if __name__ == "__main__":
    unittest.main()
