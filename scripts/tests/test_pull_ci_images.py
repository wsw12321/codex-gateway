import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
REVISION = "a" * 40
SERVICES = ("gateway", "codex-compat", "antigravity-bridge")


@unittest.skipUnless(shutil.which("jq"), "jq is required")
class PullCIImagesTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "scripts").mkdir()
        (self.root / "bin").mkdir()
        self.script = self.root / "scripts/pull-ci-images.sh"
        shutil.copy2(ROOT / "scripts/pull-ci-images.sh", self.script)
        self.log = self.root / "docker.jsonl"
        self.manifest = {
            "schema_version": 1,
            "revision": REVISION,
            "repository": "example/gateway",
            "platform": "linux/amd64",
            "images": {
                service: f"ghcr.io/example/gateway/{service}@sha256:{str(i) * 64}"
                for i, service in enumerate(SERVICES, 1)
            },
        }
        self.config = {
            "services": {
                "gateway": {
                    "image": f"codex-gateway-gateway:{REVISION}",
                    "build": {"args": {"REVISION": REVISION, "VERSION": REVISION}},
                },
                "codex-compat": {"image": "codex-gateway-compat:reviewed"},
                "antigravity-bridge": {
                    "image": f"codex-gateway-antigravity:agy1.2.4-{REVISION}"
                },
            }
        }
        self.env = dict(
            os.environ,
            PATH=f"{self.root / 'bin'}:{os.environ['PATH']}",
            CI_TEST_HEAD=REVISION,
            CI_TEST_CONFIG=str(self.root / "config.json"),
            CI_TEST_DOCKER_LOG=str(self.log),
        )
        self.write_executable(
            "scripts/compose.sh",
            '#!/bin/sh\nset -eu\ncat "$CI_TEST_CONFIG"\n',
        )
        self.write_executable(
            "bin/git",
            '#!/bin/sh\nset -eu\nprintf "%s\\n" "$CI_TEST_HEAD"\n',
        )
        self.write_executable(
            "bin/docker",
            """#!/usr/bin/env python3
import json
import os
import sys
args = sys.argv[1:]
with open(os.environ['CI_TEST_DOCKER_LOG'], 'a') as stream:
    stream.write(json.dumps(args) + '\\n')
if args[0] == 'pull':
    failing = os.environ.get('CI_TEST_FAIL_PULL')
    if failing and failing in args[-1]:
        sys.exit(1)
elif args[:2] == ['image', 'inspect']:
    print(os.environ.get('CI_TEST_PLATFORM', 'linux/amd64'))
elif args[0] != 'tag':
    sys.exit('Unexpected Docker operation: ' + repr(args))
""",
        )

    def write_executable(self, name, contents):
        path = self.root / name
        path.write_text(contents)
        path.chmod(0o755)

    def run_pull(self):
        manifest_file = self.root / "deployment-images.json"
        manifest_file.write_text(json.dumps(self.manifest))
        (self.root / "config.json").write_text(json.dumps(self.config))
        result = subprocess.run(
            ["sh", str(self.script), str(manifest_file)],
            env=self.env,
            text=True,
            capture_output=True,
            timeout=10,
        )
        calls = (
            [json.loads(line) for line in self.log.read_text().splitlines()]
            if self.log.exists()
            else []
        )
        return result, calls

    def test_pulls_all_digests_before_retagging_without_starting_services(self):
        result, calls = self.run_pull()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            [call[0] for call in calls],
            ["pull", "image", "pull", "image", "pull", "image", "tag", "tag", "tag"],
        )
        for index, service in enumerate(SERVICES):
            self.assertEqual(
                calls[index * 2],
                ["pull", "--platform", "linux/amd64", self.manifest["images"][service]],
            )
            self.assertEqual(
                calls[6 + index],
                ["tag", self.manifest["images"][service], self.config["services"][service]["image"]],
            )

    def test_rejects_mutable_tags_before_docker(self):
        self.manifest["images"]["gateway"] = "ghcr.io/example/gateway/gateway:latest"
        result, calls = self.run_pull()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(calls, [])

    def test_rejects_images_from_another_repository(self):
        self.manifest["images"]["gateway"] = f"ghcr.io/other/gateway/gateway@sha256:{'1' * 64}"
        result, calls = self.run_pull()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(calls, [])

    def test_rejects_wrong_checkout(self):
        self.env["CI_TEST_HEAD"] = "b" * 40
        result, calls = self.run_pull()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("check out commit", result.stderr)
        self.assertEqual(calls, [])

    def test_rejects_wrong_version_configuration(self):
        self.config["services"]["gateway"]["build"]["args"]["VERSION"] = "old"
        result, calls = self.run_pull()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("GATEWAY_IMAGE_TAG", result.stderr)
        self.assertEqual(calls, [])

    def test_rejects_incomplete_manifest(self):
        del self.manifest["images"]["codex-compat"]
        result, calls = self.run_pull()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(calls, [])

    def test_failed_pull_preserves_all_local_tags(self):
        self.env["CI_TEST_FAIL_PULL"] = "/codex-compat@"
        result, calls = self.run_pull()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual([call[0] for call in calls], ["pull", "image", "pull"])

    def test_wrong_image_platform_preserves_all_local_tags(self):
        self.env["CI_TEST_PLATFORM"] = "linux/arm64"
        result, calls = self.run_pull()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unexpected platform", result.stderr)
        self.assertFalse(any(call[0] == "tag" for call in calls))


if __name__ == "__main__":
    unittest.main()
