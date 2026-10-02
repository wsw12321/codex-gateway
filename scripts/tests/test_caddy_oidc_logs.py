#!/usr/bin/env python3
"""Opt-in regression using the pinned Caddy binary and an isolated fake upstream.

Run: RUN_DOCKER_INTEGRATION=1 python3 -m unittest scripts/tests/test_caddy_oidc_logs.py
Only the digest-locked, already-cached image is used. No project network or secret
is mounted; containers cannot reach the network and are removed after the test.
"""

import json
import os
from pathlib import Path
import shutil
import subprocess
import unittest
import uuid

ROOT = Path(__file__).resolve().parents[2]


@unittest.skipUnless(os.environ.get("RUN_DOCKER_INTEGRATION") == "1" and shutil.which("docker"),
                     "set RUN_DOCKER_INTEGRATION=1 with Docker and the cached locked Caddy image")
class CaddyOIDCLogTests(unittest.TestCase):
    def test_upstream_error_logs_keep_path_and_remove_callback_queries(self):
        lock = dict(line.split("=", 1) for line in (ROOT / "deploy/images.lock.env").read_text().splitlines()
                    if line and not line.startswith("#"))
        name = "cg-oidc-log-test-" + uuid.uuid4().hex
        marker = "synthetic-oidc-sensitive-marker"
        command = r'''
set -eu
caddy run --config /etc/caddy/Caddyfile --adapter caddyfile >/tmp/caddy.log 2>&1 &
pid=$!
trap 'kill "$pid" 2>/dev/null || true' EXIT
for n in 1 2 3 4 5; do
    if wget -q -O /dev/null http://127.0.0.1:2019/config/; then break; fi
    sleep 1
done
wget -q -O /dev/null --header 'Host: oidc-error.ci.invalid' \
    --header 'Referer: https://oidc-error.ci.invalid/auth/oidc/callback?code=synthetic-oidc-sensitive-marker-referrer' \
    --header 'Authorization: Bearer synthetic-oidc-sensitive-marker-authorization' \
    --header 'Cookie: __Host-cg_oidc=synthetic-oidc-sensitive-marker-cookie' \
    'http://127.0.0.1/auth/oidc/callback?code=synthetic-oidc-sensitive-marker-code&state=synthetic-oidc-sensitive-marker-state&error_description=synthetic-oidc-sensitive-marker-error' 2>/dev/null || true
kill "$pid"
wait "$pid" || true
cat /tmp/caddy.log
'''
        args = ["docker", "run", "--rm", "--pull", "never", "--name", name,
                "--network", "none", "--read-only", "--cap-drop", "ALL", "--cap-add", "NET_BIND_SERVICE",
                "--security-opt", "no-new-privileges:true", "--add-host", "gateway:127.0.0.2",
                "--tmpfs", "/config:rw,noexec,nosuid,nodev,size=1m",
                "--tmpfs", "/data:rw,noexec,nosuid,nodev,size=1m",
                "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=8m",
                "-e", "GATEWAY_DOMAIN=oidc-error.ci.invalid",
                "-v", str(ROOT / "deploy/Caddyfile") + ":/etc/caddy/Caddyfile:ro",
                "--entrypoint", "sh", lock["CADDY_IMAGE"], "-c", command]
        try:
            result = subprocess.run(args, capture_output=True, text=True, timeout=30)
        finally:
            subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL, timeout=10, check=False)
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        self.assertNotIn(marker, result.stdout + result.stderr, "runtime error logs leaked synthetic credentials")
        logs = [json.loads(line) for line in result.stdout.splitlines() if line.startswith("{")]
        errors = [entry for entry in logs if entry.get("logger") == "http.log.error"]
        self.assertEqual(len(errors), 1, result.stdout)
        self.assertEqual(errors[0]["status"], 502)
        self.assertEqual(errors[0]["request"]["uri"], "/auth/oidc/callback")
        self.assertEqual(errors[0]["request"]["headers"]["Referer"],
                         ["https://oidc-error.ci.invalid/auth/oidc/callback"])


if __name__ == "__main__":
    unittest.main()
