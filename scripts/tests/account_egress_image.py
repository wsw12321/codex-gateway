#!/usr/bin/env python3
"""Real Squid PASSTHRU -> Go router -> Mihomo -> two independent native SS exits.

All sockets live in one Docker --network none namespace. Only the temporary IP
setup container receives NET_ADMIN; no production volume or network is used.
"""
import base64
import hashlib
import hmac
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import uuid

from relay_image import ROOT, ALL_HOSTS, command, eventually

KEY = "synthetic-account-control-key-not-production-0123456789"
PASSWORD = "synthetic SS: # $ ' \" \\ 中文"
FIXTURE = "/fixture/account_egress_fixture.pl"


def route(identifier, node, revision):
    digest = hmac.new(KEY.encode(), b"codex-gateway/egress-route/v1\x00" + identifier.encode(), hashlib.sha256).digest()
    return {"id": identifier, "token": base64.urlsafe_b64encode(digest).decode().rstrip("="),
            "node_id": node, "node_revision": revision}


def snapshot(version, password=PASSWORD):
    return {"version": version, "nodes": [
        {"id": "node-a", "revision": version, "server": "127.0.0.5", "port": 24019,
         "cipher": "aes-128-gcm", "password": password, "udp": True},
        {"id": "node-b", "revision": 1, "server": "127.0.0.5", "port": 24020,
         "cipher": "chacha20-ietf-poly1305", "password": PASSWORD, "udp": False},
    ], "routes": [route(f"account-a-revision-{version}", "node-a", version),
                  route("account-b-revision-1", "node-b", 1),
                  route(f"shared-account-revision-{version}", "node-a", version)]}


class AccountSandbox:
    def __init__(self, images, directory, helper):
        self.images, self.directory, self.helper = images, directory, helper
        self.name = "account-egress-test-" + uuid.uuid4().hex[:10]
        self.router, self.server_a, self.server_b = (self.name + suffix for suffix in ("-router", "-a", "-b"))
        self.containers = []

    def docker(self, *args, **kwargs):
        return command("docker", *args, **kwargs)

    def fixture(self, *args, **kwargs):
        return self.docker("exec", self.name, "perl", FIXTURE, *args, **kwargs)

    def contents(self, path):
        return self.docker("exec", self.name, "cat", path, check=False).stdout

    def __enter__(self):
        try:
            self.docker("run", "-d", "--name", self.name, "--network", "none", "--read-only",
                        "--security-opt", "no-new-privileges:true", "--tmpfs", "/run:rw,noexec,nosuid,nodev,size=8m",
                        "--tmpfs", "/var/log/squid:rw,noexec,nosuid,nodev,size=16m,mode=0750,uid=13,gid=13",
                        "--tmpfs", "/var/spool/squid:rw,noexec,nosuid,nodev,size=64m,mode=0750,uid=13,gid=13",
                        "--mount", f"type=bind,source={self.directory},target=/fixture,readonly",
                        "--mount", f"type=bind,source={self.directory / 'squid.conf'},target=/etc/squid/squid.conf,readonly",
                        "--mount", f"type=bind,source={ROOT / 'deploy/egress/entrypoint.sh'},target=/usr/local/bin/codex-egress-entrypoint.sh,readonly",
                        *(arg for host in ALL_HOSTS for arg in ("--add-host", f"{host}:127.0.0.6")),
                        "--entrypoint", "/bin/sh", self.images["SQUID_IMAGE"], "-c", "exec sleep 600")
            self.containers.append(self.name)
            addresses = ("172.28.30.4", "172.28.60.2", "172.28.60.3", "172.28.70.2", "172.28.70.3")
            setup = "\n".join(f"ip address add {address}/32 dev lo" for address in addresses)
            self.docker("run", "--rm", "--network", "container:" + self.name, "--read-only", "--cap-drop", "ALL",
                        "--cap-add", "NET_ADMIN", "--security-opt", "no-new-privileges:true", "--user", "0:0",
                        "--entrypoint", "/bin/sh", self.images["RUNTIME_IMAGE"], "-ec", setup)
            for label, address in (("A", "127.0.0.6"), ("B", "127.0.0.7")):
                self.docker("exec", "-d", self.name, "perl", FIXTURE, "serve", label, address)
            self.start_server(self.server_a, "server-a.json")
            self.start_server(self.server_b, "server-b.json")
            self.docker("run", "-d", "--name", self.router, "--network", "container:" + self.name,
                        "--read-only", "--cap-drop", "ALL", "--user", f"10004:{os.getgid()}",
                        "--security-opt", "no-new-privileges:true",
                        "--tmpfs", "/run/account-egress:rw,noexec,nosuid,nodev,size=32m,mode=0700,uid=10004,gid=10004",
                        "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=8m,mode=0700,uid=10004,gid=10004",
                        "--mount", f"type=bind,source={self.directory / 'key'},target=/run/secrets/egress_control_key,readonly",
                        self.helper)
            self.containers.append(self.router)
            eventually(lambda: self.fixture("control", "/status", "GET", check=False).stdout.startswith("200\n"), "runtime control readiness")
            self.apply(1)
            self.docker("exec", "-d", "-e", "EGRESS_MODE=direct", self.name,
                        "/usr/local/bin/codex-egress-entrypoint.sh", "-f", "/etc/squid/squid.conf", "-NYC")
            eventually(lambda: self.probe(1, 0, "A", check=False).returncode == 0, "real account route readiness")
            return self
        except BaseException:
            self.__exit__(*sys.exc_info())
            raise

    def start_server(self, name, filename):
        self.docker("run", "-d", "--name", name, "--network", "container:" + self.name,
                    "--read-only", "--cap-drop", "ALL", "--user", "10003:10003",
                    "--security-opt", "no-new-privileges:true",
                    "--tmpfs", "/run/mihomo:rw,noexec,nosuid,nodev,size=8m,mode=0700,uid=10003,gid=10003",
                    "--mount", f"type=bind,source={self.directory / filename},target=/fixture.json,readonly",
                    "--entrypoint", "/mihomo", self.images["MIHOMO_IMAGE"], "-d", "/run/mihomo", "-f", "/fixture.json")
        self.containers.append(name)

    def apply(self, version):
        result = self.fixture("control", "/config", "PUT", f"/fixture/snapshot-{version}.json")
        assert result.stdout.startswith("200\n"), result.stdout
        status = json.loads(result.stdout.split("\n", 1)[1])
        assert status["ready"] and status["applied_version"] == version
        assert "password" not in result.stdout and KEY not in result.stdout

    def probe(self, version, index, label, mode="once", **kwargs):
        return self.fixture("probe", f"/fixture/snapshot-{version}.json", str(index), label, mode, **kwargs)

    def mode(self, mode):
        self.docker("exec", "-e", f"EGRESS_MODE={mode}", "-e", "CODEX_RELAY_IP=127.0.0.9", self.name,
                    "/bin/sh", "-ec", "/usr/local/bin/codex-egress-entrypoint.sh --render-config > /run/codex-relay.conf\n/usr/sbin/squid -k reconfigure -f /etc/squid/squid.conf")

    def checks(self):
        for mode in ("direct", "relay", "shadowsocks"):
            self.mode(mode)
            eventually(lambda: "EXIT_A" in self.probe(1, 0, "A", check=False).stdout, f"A route with global {mode}")
            assert "EXIT_B" in self.probe(1, 1, "B").stdout
            assert "EXIT_A" in self.probe(1, 2, "A").stdout
        for mode in ("bad-token", "missing-token", "bad-host"):
            result = self.probe(1, 0, "A", mode)
            assert not result.stdout.startswith("200\n"), result.stdout
        # Keep a real encrypted SS tunnel active while the node's revision is
        # replaced. Every echo is checked, so a reset or wrong exit fails.
        stream = subprocess.Popen(["docker", "exec", self.name, "perl", FIXTURE, "probe", "/fixture/snapshot-1.json", "0", "A", "stream"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        eventually(lambda: self.docker("exec", self.name, "test", "-e", "/run/account-stream-ready", check=False).returncode == 0, "active encrypted stream")
        self.apply(2)
        stdout, stderr = stream.communicate(timeout=15)
        assert stream.returncode == 0 and "EXIT_A" in stdout, stderr
        assert "EXIT_A" in self.probe(2, 0, "A").stdout
        assert not self.probe(1, 0, "A").stdout.startswith("200\n"), "old route remained usable"
        # A wrong SS password can be detected only after CONNECT when encrypted
        # traffic begins. It must close, without reaching either direct origin.
        self.apply(3)
        assert "BLOCKED" in self.probe(3, 0, "A", "reject-tunnel").stdout
        assert "EXIT_B" in self.probe(3, 1, "B").stdout
        before = json.loads(self.fixture("control", "/status", "GET").stdout.split("\n", 1)[1])
        self.docker("restart", "--time", "1", self.router)
        eventually(lambda: self.fixture("control", "/status", "GET", check=False).stdout.startswith("200\n"), "runtime restarted")
        restarted = json.loads(self.fixture("control", "/status", "GET").stdout.split("\n", 1)[1])
        assert restarted["instance_id"] != before["instance_id"] and not restarted["ready"]
        assert not self.probe(3, 1, "B").stdout.startswith("200\n"), "restart accepted stale route state"
        self.apply(3)
        assert "EXIT_B" in self.probe(3, 1, "B").stdout
        self.docker("stop", "--time", "1", self.server_b)
        assert "BLOCKED" in self.probe(3, 1, "B", "reject-tunnel").stdout
        logs = self.docker("logs", self.router).stdout + self.docker("logs", self.router).stderr
        assert KEY not in logs and PASSWORD not in logs

    def __exit__(self, kind, value, traceback):
        if kind and self.name in self.containers:
            print(self.contents("/var/log/squid/cache.log"), file=sys.stderr)
            if self.router in self.containers:
                logs = self.docker("logs", self.router, check=False)
                print((logs.stdout + logs.stderr).replace(KEY, "<redacted>").replace(PASSWORD, "<redacted>"), file=sys.stderr)
        for name in reversed(self.containers):
            self.docker("rm", "-f", name, check=False)


def run(helper=None):
    images = dict(line.split("=", 1) for line in (ROOT / "deploy/images.lock.env").read_text().splitlines() if line and not line.startswith("#"))
    if not helper:
        helper = "codex-gateway-account-egress:1.19.32-v1"
        args = ["docker", "build", "-f", str(ROOT / "deploy/account-egress/Dockerfile"), "-t", helper]
        for key in ("MIHOMO_IMAGE", "GOLANG_IMAGE", "RUNTIME_IMAGE"):
            args += ["--build-arg", f"{key}={images[key]}"]
        result = subprocess.run(args + [str(ROOT)], check=False)
        if result.returncode:
            raise SystemExit(result.returncode)
    with tempfile.TemporaryDirectory(prefix="account-egress-image-") as directory:
        directory = Path(directory)
        directory.chmod(0o755)
        (directory / "account_egress_fixture.pl").write_text((ROOT / "scripts/tests/account_egress_fixture.pl").read_text())
        config = (ROOT / "deploy/egress/squid.conf").read_text()
        config = config.replace("172.28.30.3/32", "127.0.0.2/32")
        config = config.replace("http_port 3128", "http_port 127.0.0.1:3128")
        config += "\ntcp_outgoing_address 172.28.60.2 account_route\nconnect_timeout 2 seconds\nforward_timeout 5 seconds\npeer_connect_timeout 1 seconds\ndead_peer_timeout 1 seconds\n"
        (directory / "squid.conf").write_text(config)
        for label, target, port, cipher in (("a", "127.0.0.6", 24019, "aes-128-gcm"), ("b", "127.0.0.7", 24020, "chacha20-ietf-poly1305")):
            native = {"mode": "rule", "log-level": "silent", "ipv6": False,
                      "listeners": [{"name": f"server-{label}", "type": "shadowsocks", "listen": "127.0.0.5", "port": port, "cipher": cipher, "password": PASSWORD, "udp": True}],
                      "hosts": {host: target for host in ALL_HOSTS}, "rules": ["MATCH,DIRECT"]}
            (directory / f"server-{label}.json").write_text(json.dumps(native))
        for version in (1, 2, 3):
            (directory / f"snapshot-{version}.json").write_text(json.dumps(snapshot(version, "wrong-password" if version == 3 else PASSWORD)))
        for path in directory.iterdir():
            path.chmod(0o644)
        (directory / "key").write_text(KEY + "\n")
        (directory / "key").chmod(0o640)
        with AccountSandbox(images, directory, helper) as sandbox:
            sandbox.checks()
    print("Two real SS exits, shared nodes, global mode independence, authenticated routing, hot stream updates, restart recovery and failure without fallback passed.", flush=True)


if __name__ == "__main__":
    if len(sys.argv) > 2:
        raise SystemExit("usage: test-account-egress-image.sh [built-image]")
    run(sys.argv[1] if len(sys.argv) == 2 else None)
