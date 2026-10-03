#!/usr/bin/env python3
"""Exercise real Compose parsing and egress switches with a simulated daemon."""

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
class EgressModeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.directory = tempfile.TemporaryDirectory(prefix="egress-modes-")
        cls.addClassCleanup(cls.directory.cleanup)
        cls.root = Path(cls.directory.name)
        shutil.copytree(ROOT / "deploy", cls.root / "deploy",
                        ignore=shutil.ignore_patterns("secrets"))
        shutil.copy2(ROOT / "docker-compose.yml", cls.root)
        scripts = cls.root / "scripts"
        scripts.mkdir()
        for name in ("compose.sh", "apply-egress.sh", "egress-common.sh",
                     "egress-settings.compose.yml", "validate-egress.py"):
            shutil.copy2(ROOT / "scripts" / name, scripts / name)
        cls.secret = cls.root / "deploy/secrets/shadowsocks_password"
        cls.secret.parent.mkdir()
        cls.log = cls.root / "docker.jsonl"
        cls.gid = os.getgid() or 1
        cls.baseline = {
            "GATEWAY_DOMAIN": "egress.ci.invalid", "GATEWAY_IMAGE_TAG": "a" * 40,
            "GATEWAY_VERSION": "a" * 40, "GATEWAY_REVISION": "a" * 40,
            "GATEWAY_USAGE_PRICING_JSON": "{}", "GATEWAY_SECRET_GID": str(cls.gid),
            "CODEX_RELAY_PORT": "3128",
        }
        cls.ss_settings = {
            "EGRESS_MODE": "shadowsocks", "SHADOWSOCKS_SERVER": "203.0.113.25",
            "SHADOWSOCKS_PORT": "24019", "SHADOWSOCKS_CIPHER": "aes-128-gcm",
            "SHADOWSOCKS_PASSWORD_FILE": "./deploy/secrets/shadowsocks_password",
        }
        cls.oidc_settings = {
            "OIDC_ENABLED": "true", "OIDC_AUTH_HOST": "auth.ci.invalid",
            "OIDC_ISSUER": "https://auth.ci.invalid/auth/v1",
            "OIDC_CLIENT_ID": "synthetic-client",
        }
        bins = cls.root / "bin"
        bins.mkdir()
        docker = bins / "docker"
        docker.write_text('''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import subprocess
import sys

args = sys.argv[1:]
record = {"args": args, "remove_orphans": os.environ.get("COMPOSE_REMOVE_ORPHANS")}
if args[0] == "compose" and "-f" in args:
    files = [args[index + 1] for index, value in enumerate(args) if value == "-f"]
    record["files"] = files
    if files[-1].endswith("compose.json"):
        record["snapshot"] = json.loads(Path(files[-1]).read_text())
with open(os.environ["EGRESS_TEST_LOG"], "a") as output:
    output.write(json.dumps(record) + "\\n")
if args[0] == "compose" and "config" in args:
    if os.environ.get("EGRESS_TEST_FAIL_CONFIG") and files[-1].endswith("compose.json"):
        sys.exit(43)
    if os.environ.get("EGRESS_TEST_FAIL_SELECTOR") and files[-1].endswith("egress-settings.compose.yml"):
        subprocess.run([os.environ["EGRESS_TEST_REAL_DOCKER"], *args], check=True)
        sys.exit(43)
    if os.environ.get("EGRESS_TEST_CHANGE_MODE") and files[-1].endswith("docker-compose.yml"):
        output = subprocess.check_output([os.environ["EGRESS_TEST_REAL_DOCKER"], *args], text=True)
        model = json.loads(output)
        model["services"]["egress-allowlist"]["environment"].update(
            EGRESS_MODE=os.environ["EGRESS_TEST_CHANGE_MODE"], CODEX_RELAY_IP="10.77.0.2")
        print(json.dumps(model))
        sys.exit(0)
    sys.exit(subprocess.run([os.environ["EGRESS_TEST_REAL_DOCKER"], *args]).returncode)
if args[0] == "compose" and "build" in args:
    sys.exit(42 if os.environ.get("EGRESS_TEST_FAIL_BUILD") else 0)
if args[0] == "compose" and "up" in args:
    if args[-1] == os.environ.get("EGRESS_TEST_FAIL_SERVICE"):
        sys.exit(42)
    sys.exit(0)
if args[:3] == ["container", "ls", "-aq"]:
    expected = {"label=com.docker.compose.project=codex-gateway",
                "label=com.docker.compose.service=ss-egress"}
    if set(args[4::2]) != expected:
        sys.exit("incorrect cleanup filters")
    print("c0de1234\\nc0de5678")
    sys.exit(0)
if args[:3] == ["container", "rm", "-f"] and args[3:] in (["c0de1234"], ["c0de5678"]):
    sys.exit(0)
sys.exit("unexpected daemon operation in egress test")
''')
        docker.chmod(0o755)
        cls.env = {
            **os.environ, "PATH": str(bins) + os.pathsep + os.environ["PATH"],
            "EGRESS_TEST_REAL_DOCKER": shutil.which("docker"),
            "EGRESS_TEST_LOG": str(cls.log),
        }

    def setUp(self):
        self.log.write_text("")
        if self.secret.exists() or self.secret.is_symlink():
            self.secret.unlink()
        self.secret.write_bytes(b"synthetic-egress-password\n")
        self.secret.chmod(0o640)
        if self.gid != os.getgid():
            os.chown(self.secret, -1, self.gid)
        self.settings = dict(self.baseline)

    def run_script(self, settings=None, apply=False, **env):
        if settings:
            self.settings.update(settings)
        (self.root / ".env").write_text("\n".join(
            f"{name}={value}" for name, value in self.settings.items()) + "\n")
        script = "apply-egress.sh" if apply else "compose.sh"
        args = [] if apply else ["config", "--format", "json"]
        return subprocess.run(
            ["sh", str(self.root / "scripts" / script), *args],
            env={**self.env, **env}, capture_output=True, text=True, timeout=30,
        )

    def records(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()]

    def mutations(self):
        return [entry for entry in self.records()
                if "up" in entry["args"] or entry["args"][:2] == ["container", "rm"]]

    def assert_valid(self, result):
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def test_three_modes_and_legacy_env_select_only_optional_ss_overlay(self):
        for mode, relay in ((None, ""), ("", "10.77.0.2"), ("direct", "10.77.0.2"),
                            ("relay", "10.77.0.2"), ("shadowsocks", "10.77.0.2")):
            with self.subTest(mode=mode):
                self.settings = {**self.baseline, "CODEX_RELAY_IP": relay}
                if mode is not None:
                    self.settings["EGRESS_MODE"] = mode
                if mode == "shadowsocks":
                    self.settings.update(self.ss_settings)
                config = self.assert_valid(self.run_script())
                self.assertEqual("ss-egress" in config["services"], mode == "shadowsocks")
                self.assertEqual(config["services"]["egress-allowlist"]["environment"]["EGRESS_MODE"],
                                 mode or "")

    def test_dotenv_precedence_clears_inherited_optional_routing_and_images(self):
        config = self.assert_valid(self.run_script(
            EGRESS_MODE="shadowsocks", CODEX_RELAY_IP="10.77.0.2",
            CODEX_RELAY_PORT="bad", SHADOWSOCKS_SERVER="bad\ninjection",
            OIDC_ENABLED="true", GATEWAY_SECRET_GID="9999", SQUID_IMAGE="bad:image",
            COMPOSE_PROFILES="legacy-bridge", COMPOSE_REMOVE_ORPHANS="1"))
        self.assertNotIn("ss-egress", config["services"])
        self.assertNotIn("antigravity-bridge", config["services"])
        self.assertEqual(config["services"]["egress-allowlist"]["environment"]["CODEX_RELAY_IP"], "")
        self.assertNotEqual(config["services"]["egress-allowlist"]["image"], "bad:image")

    def test_compose_owns_dotenv_quotes_and_export_syntax(self):
        config = self.assert_valid(self.run_script({
            "export EGRESS_MODE": "'direct' # selected explicitly",
            "CODEX_RELAY_IP": '"10.77.0.2"',
        }, EGRESS_MODE="relay"))
        self.assertEqual(config["services"]["egress-allowlist"]["environment"]["EGRESS_MODE"], "direct")

    def test_direct_and_relay_do_not_need_ss_secret_or_valid_ss_settings(self):
        self.secret.unlink()
        for mode in ("direct", "relay"):
            self.assert_valid(self.run_script({
                "EGRESS_MODE": mode, "CODEX_RELAY_IP": "10.77.0.2",
                "SHADOWSOCKS_PORT": "not-a-port", "SHADOWSOCKS_SERVER": "bad host",
                "SHADOWSOCKS_PASSWORD_FILE": "/missing/secret",
            }))

    def test_unknown_mode_empty_relay_and_injection_fail_before_mutation(self):
        marker = self.root / "executed"
        for value in ("invalid", "DIRECT", "'direct\nrelay'", f"'$(touch {marker})'",
                      f"'`touch {marker}`'", f"'direct; touch {marker}'", "relay"):
            with self.subTest(value=value):
                self.log.write_text("")
                result = self.run_script({"EGRESS_MODE": value}, apply=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("EGRESS_MODE", result.stderr)
                self.assertEqual(self.mutations(), [])
                self.assertFalse(marker.exists())

    def test_ss_missing_bad_permissions_gid_symlink_and_invalid_passwords_fail_early(self):
        self.settings.update(self.ss_settings)
        cases = (b"", b"two\nlines", b"nul\0byte", b"bad\xffutf8", b"x" * 4097)
        for value in cases:
            with self.subTest(password=value[:8]):
                self.secret.write_bytes(value)
                result = self.run_script(apply=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("SHADOWSOCKS_PASSWORD_FILE", result.stderr)
                self.assertEqual(self.mutations(), [])
        self.secret.write_text("synthetic-password")
        self.secret.chmod(0o644)
        self.assertIn("0640", self.run_script(apply=True).stderr)
        self.secret.chmod(0o640)
        self.assertIn("group", self.run_script({"GATEWAY_SECRET_GID": str(self.gid + 1)}, apply=True).stderr)
        self.settings["GATEWAY_SECRET_GID"] = str(self.gid)
        other = self.secret.with_name("other")
        self.secret.rename(other)
        self.secret.symlink_to(other)
        self.assertIn("non-symlink", self.run_script(apply=True).stderr)
        self.secret.unlink()
        self.assertIn("readable", self.run_script(apply=True).stderr)
        self.assertEqual(self.mutations(), [])

    def test_ss_server_port_cipher_validation(self):
        invalid = {
            "SHADOWSOCKS_SERVER": ("", "localhost", "203.0.113.025", "UPPER.example", "a.123",
                                   "a.tld-2", "'server.example\nproxies:'"),
            "SHADOWSOCKS_PORT": ("0", "024019", "65536", "'24019; true'"),
            "SHADOWSOCKS_CIPHER": ("none", "aes-128-cfb", "'aes-128-gcm\nrules:'"),
        }
        for name, values in invalid.items():
            for value in values:
                self.settings = {**self.baseline, **self.ss_settings, name: value}
                result = self.run_script(apply=True)
                self.assertNotEqual(result.returncode, 0, (name, value))
                self.assertIn(name, result.stderr)
                self.assertEqual(self.mutations(), [])

    def test_special_password_bytes_are_accepted_and_never_disclosed(self):
        password = "  SYNTHETIC_PASSWORD_NEVER_LOG_$'\"\\雪  "
        for ending in ("", "\n", "\r\n"):
            self.secret.write_text(password + ending)
            result = self.run_script(self.ss_settings, apply=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertNotIn(password, result.stdout + result.stderr + self.log.read_text())

    def test_apply_order_all_transitions_rotation_and_exact_cleanup(self):
        for mode in ("direct", "shadowsocks", "relay", "direct", "shadowsocks", "shadowsocks"):
            with self.subTest(mode=mode):
                self.log.write_text("")
                self.secret.write_text(f"synthetic-rotated-{mode}")
                result = self.run_script({**self.ss_settings, "EGRESS_MODE": mode,
                                          "CODEX_RELAY_IP": "10.77.0.2"}, apply=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                operations = [entry["args"] for entry in self.mutations()]
                targets = [args[-1] for args in operations]
                self.assertEqual(targets, ["ss-egress", "egress-allowlist"] if mode == "shadowsocks"
                                 else ["egress-allowlist", "c0de1234", "c0de5678"])
                for args in operations:
                    if "up" in args:
                        for flag in ("--force-recreate", "--no-deps", "--wait", "--wait-timeout"):
                            self.assertIn(flag, args)
                    self.assertNotIn("down", args)
                    self.assertNotIn("--remove-orphans", args)

    def test_failed_ss_start_keeps_squid_and_failed_squid_keeps_old_ss(self):
        for mode, failed, expected in (("shadowsocks", "ss-egress", ["ss-egress"]),
                                       ("direct", "egress-allowlist", ["egress-allowlist"])):
            self.log.write_text("")
            result = self.run_script({**self.ss_settings, "EGRESS_MODE": mode}, apply=True,
                                     EGRESS_TEST_FAIL_SERVICE=failed)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual([entry["args"][-1] for entry in self.mutations()], expected)

    def test_invalid_final_compose_model_does_not_change_containers(self):
        result = self.run_script({"EGRESS_MODE": "direct"}, apply=True, EGRESS_TEST_FAIL_CONFIG="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.mutations(), [])

    def test_selector_failure_with_valid_json_still_aborts(self):
        result = self.run_script(apply=True, EGRESS_TEST_FAIL_SELECTOR="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.mutations(), [])

    def test_inherited_internal_flag_cannot_select_oidc_overlay(self):
        config = self.assert_valid(self.run_script(self.oidc_settings, egress_auto_oidc="true"))
        self.assertNotIn("oidc_client_secret", config["secrets"])

    def test_image_build_failure_does_not_change_containers(self):
        result = self.run_script(self.ss_settings, apply=True, EGRESS_TEST_FAIL_BUILD="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.mutations(), [])

    def test_apply_disables_inherited_and_site_automatic_orphan_cleanup(self):
        result = self.run_script({"COMPOSE_REMOVE_ORPHANS": "true"}, apply=True,
                                 COMPOSE_REMOVE_ORPHANS="true")
        self.assertEqual(result.returncode, 0, result.stderr)
        for record in self.records():
            if "snapshot" in record:
                self.assertIn("/dev/null", record["args"])
                self.assertEqual(record["remove_orphans"], "false")

    def test_frozen_model_roundtrip_preserves_healthcheck_shell_and_literal_dollars(self):
        result = self.run_script({"CODEX_SMOKE_MODEL": "'synthetic-$VALUE-${literal}'"}, apply=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        model = self.mutations()[0]["snapshot"]
        path = self.root / "roundtrip.json"
        path.write_text(json.dumps(model))
        rendered = subprocess.run(
            [self.env["EGRESS_TEST_REAL_DOCKER"], "compose", "--project-name", "codex-gateway",
             "--env-file", "/dev/null", "-f", str(path), "config", "--format", "json"],
            capture_output=True, text=True, timeout=30,
        )
        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        roundtrip = json.loads(rendered.stdout)
        for name, service in model["services"].items():
            self.assertEqual(roundtrip["services"][name].get("healthcheck"), service.get("healthcheck"))
            self.assertEqual(roundtrip["services"][name].get("environment"), service.get("environment"))

    def test_changed_mode_between_selector_and_model_fails_preflight(self):
        result = self.run_script({"EGRESS_MODE": "direct"}, apply=True,
                                 EGRESS_TEST_CHANGE_MODE="relay")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("changed during preflight", result.stderr)
        self.assertEqual(self.mutations(), [])

    def test_apply_preserves_oidc_overlay_in_all_modes(self):
        for mode in ("direct", "relay", "shadowsocks"):
            self.log.write_text("")
            result = self.run_script({**self.ss_settings, **self.oidc_settings,
                                      "EGRESS_MODE": mode, "CODEX_RELAY_IP": "10.77.0.2"}, apply=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            snapshot = self.mutations()[0]["snapshot"]
            self.assertEqual(snapshot["services"]["egress-allowlist"]["environment"]["OIDC_ENABLED"], "true")
            self.assertEqual(snapshot["services"]["gateway"]["environment"]["OIDC_PROXY_URL"],
                             "http://172.28.30.4:3128")

    def test_invalid_oidc_settings_fail_before_changes(self):
        for changes in ({"OIDC_ENABLED": "yes"}, {"OIDC_AUTH_HOST": "'auth.ci.invalid\nallow all'"},
                        {"OIDC_ISSUER": "https://other.ci.invalid"}, {"OIDC_CLIENT_ID": ""},
                        {"OIDC_ISSUER": "'\x01https://auth.ci.invalid'"},
                        {"OIDC_ISSUER": "https://auth.ci.invalid/path%20space"},
                        {"OIDC_CLIENT_ID": "'client with whitespace'"},
                        {"OIDC_CLIENT_ID": "x" * 513}, {"OIDC_CLIENT_ID": "'client\x01id'"}):
            self.settings = {**self.baseline, **self.oidc_settings, **changes}
            result = self.run_script(apply=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("OIDC", result.stderr)
            self.assertEqual(self.mutations(), [])


if __name__ == "__main__":
    unittest.main()
