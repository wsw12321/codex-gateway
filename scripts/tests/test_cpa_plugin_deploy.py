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
        cls.compose = read(ROOT / "docker-compose.yml")
        cls.rollback = read(ROOT / "deploy" / "patch-cpa.override.yml")
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
        self.assertLess(self.entrypoint.index("\nunset HTTP_PROXY"), self.entrypoint.index('\nexec "$cpa_binary" -config'))
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

    def test_antigravity_serves_only_the_reviewed_models(self):
        config = self.entrypoint[self.entrypoint.index("<<CONFIG"):self.entrypoint.index("\nCONFIG\n") + 1]
        block = re.search(r'^oauth-excluded-models:\n  antigravity:\n((?:    - "[^"\n]+"\n)+)(?! )', config, flags=re.M)
        self.assertIsNotNone(block, "the generated configuration does not restrict Antigravity models")
        # One rule set, for Antigravity only: Codex and Claude register CPA's
        # catalog unchanged, as they did in the source-patched image.
        self.assertEqual(config.count("oauth-excluded-models:"), 1)
        rules = re.findall(r'^    - "([^"]+)"$', block.group(1), flags=re.M)
        self.assertEqual(rules, ["claude-*", "gpt-*", "gemini-3.1-flash-image"])

        def excluded(model):
            # CPA matches case-insensitively and knows no wildcard but "*".
            return any(re.fullmatch(".*".join(map(re.escape, rule.lower().split("*"))), model.lower())
                       for rule in rules)

        # The reviewed models are the Gateway's own allowlist; no rule may hide
        # one, and every other model the pinned catalog names for the provider
        # must be covered.
        gateway = read(ROOT / "internal" / "config" / "antigravity.go")
        body = gateway[gateway.index("func AntigravityModels() []string {"):gateway.index("// LegacyAntigravityModels")]
        reviewed = re.findall(r'"([a-z0-9.-]+)"', body)
        self.assertEqual(len(reviewed), 5)
        for model in reviewed:
            self.assertFalse(excluded(model), model)
        for model in ("claude-opus-4-6-thinking", "claude-sonnet-4-6", "claude-opus-5-5-high",
                      "claude-sonnet-5-5-high", "gemini-3.1-flash-image", "gpt-oss-120b-medium"):
            self.assertTrue(excluded(model), model)

        # The catalog itself lives in the CPA binary, so the proof is the
        # end-to-end test: it starts CPA with exactly this block, lets the
        # provider report the binary's whole catalog, and requires the same
        # five models. The Dockerfile asserts that test by name (see above).
        harness = read(PLUGIN / "e2e" / "harness_test.go")
        self.assertIn(block.group(0), harness)
        catalog_test = read(PLUGIN / "e2e" / "catalog_e2e_test.go")
        declared = catalog_test[catalog_test.index("var reviewedAntigravityModels = []string{"):]
        declared = declared[:declared.index("}\n")]
        self.assertEqual(sorted(re.findall(r'"([a-z0-9.-]+)"', declared)), sorted(reviewed))
        self.assertIn("/v0/management/model-definitions/antigravity", catalog_test)
        self.assertIn("TestAntigravityModelCatalog", self.dockerfile)
        # The image probe confirms the shipped binary parsed the rule.
        probe = read(ROOT / "scripts" / "tests" / "cpa-plugin-image-probe.sh")
        self.assertIn("/v0/management/oauth-excluded-models", probe)
        self.assertIn('"antigravity":[{}]'.format(",".join(f'"{rule}"' for rule in rules)), probe)

    def compat_service(self):
        start = self.compose.index("\n  codex-compat:\n")
        end = self.compose.index("\n  antigravity-bridge:\n")
        return self.compose[start:end]

    def test_default_compose_runs_plugin_mode(self):
        self.assertIn("      CPA_INTEGRATION: plugin\n", self.compose)
        service = self.compat_service()
        self.assertIn("      dockerfile: deploy/codex-compat/plugin.Dockerfile\n", service)
        self.assertIn("    image: {}\n".format(self.lock["image"]), service)
        for key, value in (
            ("CLIPROXY_VERSION", self.lock["tag"]),
            ("CLIPROXY_COMMIT", self.lock["commit"]),
            ("CLIPROXY_RESIDUAL_PATCH_SHA256", self.lock["residual_patch_sha256"]),
        ):
            self.assertIn(f"        {key}: {value}\n", service)
        self.assertNotIn("CLIPROXY_PATCH_SHA256", service)
        # CPA has no proxy of its own; the global egress is only handed to the
        # plugin, which writes it into credentials.
        self.assertIn("      CLIPROXY_PROXY_URL: http://egress-allowlist:3128\n", service)
        for variable in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"):
            self.assertNotIn(f"      {variable}:", service)
        # CPA loads a plugin from a copy under its temporary directory, which
        # therefore must be executable; the shared /tmp stays noexec.
        self.assertIn("      TMPDIR: /run/cliproxy-plugin\n", service)
        tmpfs = re.search(r"^\s+- (/run/cliproxy-plugin:\S+)$", service, flags=re.M).group(1)
        options = tmpfs.split(":", 1)[1].split(",")
        self.assertIn("exec", options)
        self.assertNotIn("noexec", options)
        for option in ("rw", "nosuid", "nodev", "mode=0700", "uid=10001", "gid=10001"):
            self.assertIn(option, options)
        self.assertIn("/tmp:rw,noexec,nosuid,nodev", service)
        self.assertIn(self.lock["commit"], self.compose)
        self.assertFalse((ROOT / "deploy" / "cpa-plugin.override.yml").exists())

    def test_rollback_override_restores_the_source_patched_image(self):
        patch_sha = self.source_lock["patch_sha256"]
        image = "codex-gateway-compat:{}-{}-{}-cpa".format(
            self.source_lock["tag"], self.source_lock["commit"][:8], patch_sha[:16])
        self.assertIn("  gateway:\n    environment:\n      CPA_INTEGRATION: patch\n", self.rollback)
        self.assertIn("    image: {}\n".format(image), self.rollback)
        self.assertIn("      dockerfile: deploy/codex-compat/Dockerfile\n", self.rollback)
        self.assertIn(f"        CLIPROXY_PATCH_SHA256: {patch_sha}\n", self.rollback)
        self.assertIn("      HTTPS_PROXY: http://egress-allowlist:3128\n", self.rollback)
        self.assertIn("      TMPDIR: /tmp\n", self.rollback)
        validate = read(ROOT / "scripts" / "validate-compose.sh")
        self.assertIn(f"compat_image={image}\n", validate)
        self.assertIn(f"compat_patch_sha256={patch_sha}\n", validate)

    def test_env_selects_the_rollback_for_every_compose_call(self):
        settings = read(ROOT / "scripts" / "egress-settings.compose.yml")
        self.assertIn("      CPA_INTEGRATION: ${CPA_INTEGRATION:-plugin}\n", settings)
        common = read(ROOT / "scripts" / "egress-common.sh")
        self.assertIn('if test "$egress_cpa_integration" = patch; then\n'
                      '        set -- -f "$egress_root/deploy/patch-cpa.override.yml" "$@"', common)
        self.assertIn(" CPA_INTEGRATION ", common)
        validator = read(ROOT / "scripts" / "validate-egress.py")
        self.assertIn('fail("CPA_INTEGRATION must be plugin or patch")', validator)
        self.assertIn('fail("CPA_INTEGRATION and the rendered CPA image disagree")', validator)
        for example in ("env.example", "env.gpt-5.6.example"):
            self.assertIn("\nCPA_INTEGRATION=plugin\n", read(ROOT / "deploy" / example))
        # A command-line login cannot run in plugin mode: CPA would have no
        # egress for it and the login would bypass the egress registration.
        self.assertIn('test "$#" -eq 0 || fail "command-line logins are not available in plugin mode', self.entrypoint)
        self.assertIn('exec "$cpa_binary" -config "$config_file" -local-model\n', self.entrypoint)

if __name__ == "__main__":
    unittest.main()
