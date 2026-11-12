#!/usr/bin/env python3
"""Static invariants of the plugin-mode CPA deployment.

The plugin image cannot be the default until it passes real-account
acceptance, so nothing else exercises these files on every change. The checks
here need no Docker: they keep the lock file, the residual patch, the
Dockerfile, the entrypoint and the Compose override telling the same story.
"""

import hashlib
import json
from pathlib import Path
import re
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[2]
COMPAT = ROOT / "deploy" / "codex-compat"
PLUGIN = COMPAT / "plugin"


def read(path):
    return Path(path).read_text(encoding="utf-8")


class CPAPluginDeploymentTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.lock = json.loads(read(COMPAT / "plugin.lock.json"))
        cls.source_lock = json.loads(read(COMPAT / "source.lock.json"))
        cls.dockerfile = read(COMPAT / "plugin.Dockerfile")
        cls.entrypoint = read(COMPAT / "plugin-entrypoint.sh")
        cls.healthcheck = read(COMPAT / "plugin-healthcheck.sh")
        cls.override = read(ROOT / "deploy" / "cpa-plugin.override.yml")
        cls.patch_path = COMPAT / cls.lock["residual_patch"]
        cls.patch = read(cls.patch_path)

    def test_lock_matches_residual_patch_and_pinned_source(self):
        digest = hashlib.sha256(self.patch_path.read_bytes()).hexdigest()
        self.assertEqual(self.lock["residual_patch_sha256"], digest)
        # Both images are built from the same reviewed CPA commit, so moving
        # between them never changes the upstream source underneath.
        for key in ("repository", "tag", "commit", "go_version"):
            self.assertEqual(self.lock[key], self.source_lock[key], key)
        self.assertEqual(self.lock["model_catalog_sha256"], self.source_lock["model_catalog"]["sha256"])
        self.assertIs(self.lock["automatic_updates"], False)
        expected_image = "codex-gateway-compat:{}-{}-{}-plugin".format(
            self.lock["tag"], self.lock["commit"][:8], digest[:16])
        self.assertEqual(self.lock["image"], expected_image)

    def test_residual_patch_is_small_and_additive(self):
        files = re.findall(r"^diff --git a/(\S+) b/\S+$", self.patch, flags=re.M)
        self.assertEqual(sorted(files), [
            "sdk/cliproxy/auth/gateway_plugin_only_selector.go",
            "sdk/cliproxy/auth/gateway_plugin_only_selector_test.go",
            "sdk/cliproxy/gateway_plugin_only_routing_test.go",
            "sdk/cliproxy/service_config.go",
        ])
        removed = [line for line in self.patch.splitlines()
                   if line.startswith("-") and not line.startswith("---")]
        # An additive patch cannot silently drop upstream behavior, and its
        # only context dependency is the routing strategy switch.
        self.assertEqual(removed, [])
        self.assertLess(len(self.patch.splitlines()), 200)
        self.assertIn('PluginOnlyStrategy = "plugin-only"', self.patch)
        self.assertIn("wrapRequestStopError", self.patch)

    def test_dockerfile_pins_and_gates(self):
        patched = read(COMPAT / "Dockerfile")
        for arg in ("GOLANG_IMAGE", "RUNTIME_IMAGE"):
            line = re.search(rf"^ARG {arg}=\S+$", patched, flags=re.M).group(0)
            self.assertIn(line + "\n", self.dockerfile, f"{arg} differs from the source-patched image")
        self.assertIn(self.lock["model_catalog_sha256"], self.dockerfile)
        for module, version in self.lock["security_modules"].items():
            self.assertIn(f"{module}@{version}", self.dockerfile)
        # The residual patch must apply exactly; fuzz would hide upstream drift.
        self.assertIn("git apply --check /tmp/cliproxy-residual.patch", self.dockerfile)
        self.assertNotIn("--ignore-space-change", self.dockerfile)
        self.assertIn('test "$(git rev-parse HEAD)" = "${CLIPROXY_COMMIT}"', self.dockerfile)
        self.assertIn("-replace github.com/router-for-me/CLIProxyAPI/v8=/src", self.dockerfile)
        self.assertIn("go test -race -count=1 ./...", self.dockerfile)
        self.assertIn("! grep -q -- '--- SKIP' /tmp/e2e.log", self.dockerfile)

        # Every end-to-end test must be asserted by name: a test that silently
        # stopped running would otherwise leave the build green.
        declared = []
        for source in sorted((PLUGIN / "e2e").glob("*_test.go")):
            declared += re.findall(r"^func (Test\w+)\(", read(source), flags=re.M)
        self.assertGreaterEqual(len(declared), 11)
        e2e_step = self.dockerfile[self.dockerfile.index("CPA_E2E_BINARY=/out/cli-proxy-api"):]
        asserted = re.findall(r"^\s+(Test\w+)(?: \\|; do \\)$", e2e_step, flags=re.M)
        self.assertEqual(sorted(asserted), sorted(declared))

        plugin_file = "codex-gateway-v{}.so".format(self.lock["plugin"]["version"])
        self.assertIn(
            f"COPY --from=build --chown=0:0 --chmod=0444 /out/plugins/{plugin_file} "
            f"/usr/local/lib/cliproxy/plugins/{plugin_file}", self.dockerfile)
        self.assertTrue(self.dockerfile.rstrip().endswith('ENTRYPOINT ["/usr/local/bin/sidecar-entrypoint"]'))
        self.assertIn("USER 10001:10001\nEXPOSE 8317", self.dockerfile)

    def test_plugin_version_is_consistent(self):
        version = self.lock["plugin"]["version"]
        source = read(PLUGIN / "internal" / "gatewayplugin" / "plugin.go")
        self.assertIn(f'PluginID      = "{self.lock["plugin"]["id"]}"', source)
        self.assertIn(f'PluginVersion = "{version}"', source)
        self.assertIn(f"plugin_file=${{plugin_dir}}/codex-gateway-v{version}.so", self.entrypoint)
        go_mod = read(PLUGIN / "go.mod")
        self.assertIn("github.com/router-for-me/CLIProxyAPI/v8 {}".format(self.lock["tag"]), go_mod)
        # The plugin must stay free of third-party dependencies: its only
        # requirement is the CPA module that defines the plugin interface.
        self.assertEqual(re.findall(r"^\s*(?:require\s+)?(\S+/\S+) v", go_mod, flags=re.M),
                         ["github.com/router-for-me/CLIProxyAPI/v8"])

    def test_entrypoint_writes_fail_closed_configuration(self):
        for script in (self.entrypoint, self.healthcheck):
            self.assertTrue(script.startswith("#!/bin/sh\n"))
            self.assertIn("\nset -eu\n", script)
        for path in ("plugin-entrypoint.sh", "plugin-healthcheck.sh"):
            subprocess.run(["sh", "-n", str(COMPAT / path)], check=True)
        config = self.entrypoint[self.entrypoint.index("<<CONFIG"):self.entrypoint.index("\nCONFIG\n") + 1]
        for block in (
            'routing:\n  # Account selection belongs to the Gateway plugin alone.',
            '  strategy: "plugin-only"\n  session-affinity: false\n',
            'plugins:\n  enabled: true\n  dir: "${plugin_dir}"\n  configs:\n    codex-gateway:\n      enabled: true\n',
            "discovery:\n  enabled: false\n",
            "request-retry: 0\nmax-retry-credentials: 4\n",
            "  bootstrap-retries: 0\n",
            "passthrough-headers: false\n",
            "request-log: false\n",
            'remote-management:\n  allow-remote: true\n  secret-key: "${management_key}"\n'
            "  disable-control-panel: true\n  disable-auto-update-panel: true\n",
            'api-keys:\n  - "${internal_key}"\n',
        ):
            self.assertIn(block, config)
        # Exactly one plugin is configured, and nothing can add a registry.
        self.assertEqual(config.count("      enabled: true\n"), 1)
        self.assertNotIn("store-sources", config)
        # The allocation callback origin is fixed, not taken from the caller's
        # environment, and the two keys must differ.
        self.assertIn("\nexport CPA_PLUGIN_GATEWAY_URL=http://gateway:8080\n", self.entrypoint)
        self.assertIn("\nexport CPA_PLUGIN_EGRESS_PROXY_HOST=egress-allowlist:3129\n", self.entrypoint)
        # CPA has no proxy of its own: no proxy-url and no proxy variables. The
        # plugin writes the global egress into credentials explicitly, so a
        # CPA fallback can only be a direct connection, which the network
        # refuses.
        self.assertIn('\nexport CPA_PLUGIN_GLOBAL_PROXY_URL="$proxy_url"\n', self.entrypoint)
        self.assertNotRegex(config, r"(?m)^proxy-url:")
        self.assertIn("\nunset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy\n", self.entrypoint)
        self.assertLess(self.entrypoint.index("\nunset HTTP_PROXY"), self.entrypoint.index('\nexec "$cpa_binary" "$@"'))
        self.assertIn("    http://*) ;;\n", self.entrypoint)
        gateway = read(ROOT / "internal" / "server" / "egress_runtime.go")
        self.assertIn('const accountEgressProxy = "http://egress-allowlist:3129"', gateway)
        self.assertIn('test "$internal_key" != "$management_key"', self.entrypoint)
        self.assertIn('"$oauth_entrypoint" verify-oauth', self.entrypoint)
        self.assertIn('flock -n 9 || fail "OAuth store is owned by another refresh process"', self.entrypoint)
        self.assertIn('chmod 0600 "$config_file"', self.entrypoint)
        # The healthcheck proves the plugin serves; a bare port probe would
        # report unmodified CPA without the plugin as healthy.
        self.assertIn("/v0/management/gateway/upstream-accounts/capabilities", self.healthcheck)
        self.assertIn("upstream_account_access_v1", self.healthcheck)

    def test_override_switches_both_services_and_default_is_unchanged(self):
        self.assertIn("  gateway:\n    environment:\n      CPA_INTEGRATION: plugin\n", self.override)
        self.assertIn("      dockerfile: deploy/codex-compat/plugin.Dockerfile\n", self.override)
        self.assertIn("    image: {}\n".format(self.lock["image"]), self.override)
        for key, value in (
            ("CLIPROXY_VERSION", self.lock["tag"]),
            ("CLIPROXY_COMMIT", self.lock["commit"]),
            ("CLIPROXY_RESIDUAL_PATCH_SHA256", self.lock["residual_patch_sha256"]),
        ):
            self.assertIn(f"        {key}: {value}\n", self.override)
        # CPA loads a plugin from a copy under its temporary directory, which
        # therefore must be executable; the shared /tmp stays noexec.
        self.assertIn("      TMPDIR: /run/cliproxy-plugin\n", self.override)
        for variable in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"):
            self.assertIn(f'      {variable}: ""\n', self.override)
        tmpfs = re.search(r"^\s+- (/run/cliproxy-plugin:\S+)$", self.override, flags=re.M).group(1)
        options = tmpfs.split(":", 1)[1].split(",")
        self.assertIn("exec", options)
        self.assertNotIn("noexec", options)
        for option in ("rw", "nosuid", "nodev", "mode=0700", "uid=10001", "gid=10001"):
            self.assertIn(option, options)

        compose = read(ROOT / "docker-compose.yml")
        self.assertNotIn("CPA_INTEGRATION", compose)
        self.assertNotIn("plugin.Dockerfile", compose)
        self.assertIn("/tmp:rw,noexec,nosuid,nodev", compose)
        self.assertIn(self.lock["commit"], compose)


if __name__ == "__main__":
    unittest.main()
