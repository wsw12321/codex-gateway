#!/usr/bin/env python3
"""Exercise real Squid -> Mihomo -> Shadowsocks AEAD without external traffic.

Both Mihomo processes share Squid's --network none namespace. A short-lived
setup container adds the production proxy IPs to that namespace's loopback;
only that setup container has NET_ADMIN. The runtime helper and its bind/source
restrictions are unchanged. The second pinned Mihomo is the synthetic B node,
using its native Shadowsocks inbound and a local echo target.
"""

import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import uuid

from relay_image import ALL_HOSTS, CLIENTS, FIXTURE, ROOT, Sandbox, command, eventually


PASSWORD = "synthetic-only: # $ ' \" \\ UTF-8-密码"
OIDC_HOST = "login.test.example"
CLIENT_IP = "172.28.50.3"
SQUID_IP = "172.28.50.2"
MIHOMO_FIXTURE = "/tmp/mihomo_fixture.pl"


class ShadowsocksSandbox(Sandbox):
    def __init__(self, squid_image, runtime_image, mihomo_image, directory):
        super().__init__(squid_image, directory / "squid.conf", enabled=True)
        self.name = "codex-ss-test-" + uuid.uuid4().hex[:12]
        self.client_name = self.name + "-client"
        self.server_name = self.name + "-server"
        self.runtime_image, self.mihomo_image = runtime_image, mihomo_image
        self.directory = directory
        self.password = directory / "password"

    def __enter__(self):
        try:
            self.docker(
                "run", "-d", "--name", self.name, "--network", "none", "--read-only",
                "--security-opt", "no-new-privileges:true",
                "--tmpfs", "/run:rw,noexec,nosuid,nodev,size=8m",
                "--tmpfs", "/var/log/squid:rw,noexec,nosuid,nodev,size=16m,mode=0750,uid=13,gid=13",
                "--tmpfs", "/var/spool/squid:rw,noexec,nosuid,nodev,size=64m,mode=0750,uid=13,gid=13",
                "--mount", f"type=bind,source={self.config},target=/etc/squid/squid.conf,readonly",
                "--mount", f"type=bind,source={ROOT / 'deploy/egress/entrypoint.sh'},target=/usr/local/bin/codex-egress-entrypoint.sh,readonly",
                "--mount", f"type=bind,source={ROOT / 'scripts/tests/relay_fixture.pl'},target={FIXTURE},readonly",
                "--mount", f"type=bind,source={self.directory / 'mihomo_fixture.pl'},target={MIHOMO_FIXTURE},readonly",
                *(argument for host in (*ALL_HOSTS, OIDC_HOST)
                  for argument in ("--add-host", f"{host}:127.0.0.6")),
                "--entrypoint", "/bin/sh", self.image, "-c", "exec sleep 600",
            )
            self.docker(
                "run", "--rm", "--network", "container:" + self.name,
                "--read-only", "--cap-drop", "ALL", "--cap-add", "NET_ADMIN",
                "--security-opt", "no-new-privileges:true", "--user", "0:0",
                "--entrypoint", "/bin/sh", self.runtime_image, "-ec",
                f"ip address add {SQUID_IP}/32 dev lo\nip address add {CLIENT_IP}/32 dev lo",
            )
            self.serve("target")
            self.start_server()
            self.start_client()
            self.docker(
                "exec", "-d", "-e", "EGRESS_MODE=shadowsocks",
                "-e", "CODEX_RELAY_IP=127.0.0.9", "-e", "CODEX_RELAY_PORT=3129",
                "-e", "OIDC_ENABLED=true", "-e", f"OIDC_AUTH_HOST={OIDC_HOST}",
                self.name, "/usr/local/bin/codex-egress-entrypoint.sh",
                "-f", "/etc/squid/squid.conf", "-NYC",
            )
            eventually(lambda: self.exists("/run/squid.pid"), "SS Squid startup")
            eventually(lambda: self.probe(check=False).returncode == 0, "SS tunnel readiness")
            return self
        except BaseException:
            self.__exit__(*sys.exc_info())
            raise

    def __exit__(self, kind, value, traceback):
        if kind:
            for container in (self.client_name, self.server_name):
                # Only synthetic credentials are used, but redact them even in
                # failure diagnostics so future regressions cannot teach leaks.
                logs = self.docker("logs", container, check=False)
                print((logs.stdout + logs.stderr).replace(PASSWORD, "<redacted>"), file=sys.stderr)
        for container in (self.client_name, self.server_name):
            self.docker("rm", "-f", container, check=False)
        super().__exit__(kind, value, traceback)

    def start_server(self):
        self.docker(
            "run", "-d", "--name", self.server_name,
            "--network", "container:" + self.name,
            "--read-only", "--cap-drop", "ALL", "--user", "10003:10003",
            "--security-opt", "no-new-privileges:true",
            "--tmpfs", "/run/mihomo:rw,noexec,nosuid,nodev,size=8m,mode=0700,uid=10003,gid=10003",
            "--mount", f"type=bind,source={self.directory / 'server.json'},target=/fixture.json,readonly",
            "--entrypoint", "/mihomo", self.mihomo_image,
            "-d", "/run/mihomo", "-f", "/fixture.json",
        )
        eventually(lambda: self.exec(
            "perl", "-MIO::Socket::INET", "-e",
            "exit(IO::Socket::INET->new(PeerAddr=>'127.0.0.5',PeerPort=>24019,Timeout=>1)?0:1)",
            check=False,
        ).returncode == 0, "synthetic B Shadowsocks listener")

    def start_client(self):
        self.docker(
            "run", "-d", "--name", self.client_name,
            "--network", "container:" + self.name,
            "--read-only", "--cap-drop", "ALL", "--user", f"10003:{os.getgid()}",
            "--security-opt", "no-new-privileges:true",
            "--tmpfs", f"/run/mihomo:rw,noexec,nosuid,nodev,size=8m,mode=0700,uid=10003,gid={os.getgid()}",
            "--mount", f"type=bind,source={self.password},target=/run/secrets/shadowsocks_password,readonly",
            "-e", "SHADOWSOCKS_SERVER=127.0.0.5", "-e", "SHADOWSOCKS_PORT=24019",
            "-e", "SHADOWSOCKS_CIPHER=aes-128-gcm", self.runtime_image,
        )
        eventually(self.healthy, "local Mihomo health independent of B")

    def healthy(self):
        return self.docker("exec", self.client_name,
                           "/usr/local/bin/ss-egress-helper", "healthcheck", check=False).returncode == 0

    def check_secrecy(self):
        assert self.docker("exec", self.client_name, "stat", "-c", "%a",
                           "/run/mihomo/config.json").stdout.strip() == "600"
        for args in (("inspect", self.client_name), ("logs", self.client_name),
                     ("exec", self.client_name, "cat", "/proc/1/cmdline")):
            output = self.docker(*args)
            assert "synthetic-only" not in output.stdout + output.stderr, "password leaked in container metadata/logs/argv"
        runtime = json.loads(self.docker("inspect", self.client_name).stdout)[0]
        assert runtime["Config"]["User"] == f"10003:{os.getgid()}"
        assert runtime["HostConfig"]["ReadonlyRootfs"]
        assert not runtime["HostConfig"]["PortBindings"]

    def oidc_checks(self):
        self.expect(200, source="127.0.0.8", host=OIDC_HOST)
        for host in ALL_HOSTS:
            self.expect(403, source="127.0.0.8", host=host)
        for _, source, _ in CLIENTS:
            self.expect(403, source=source, host=OIDC_HOST)
        eventually(lambda: re.search(rf"{OIDC_HOST}:443 200 HIER_DIRECT/127\.0\.0\.6",
                                     self.contents("/var/log/squid/access.log")), "OIDC direct route")

    def source_checks(self):
        allowed = self.exec("perl", MIHOMO_FIXTURE, "probe", CLIENT_IP, SQUID_IP,
                            "CONNECT", "chatgpt.com", "443")
        assert "ECHO_OK" in allowed.stdout
        for source in ("127.0.0.2", "127.0.0.3", "127.0.0.8", "127.0.0.1"):
            result = self.exec("perl", MIHOMO_FIXTURE, "probe", CLIENT_IP, source,
                               "CONNECT", "chatgpt.com", "443", check=False)
            assert "ECHO_OK" not in result.stdout, "business source bypassed Squid via Mihomo"
        # The listener must not also bind wildcard/loopback, which could let a
        # future second network make the proxy accessible to business clients.
        result = self.exec("perl", MIHOMO_FIXTURE, "probe", "127.0.0.1", SQUID_IP,
                           "CONNECT", "chatgpt.com", "443", check=False)
        assert result.returncode != 0 and "ECHO_OK" not in result.stdout

    def fail_closed(self, reason):
        eventually(self.no_active_targets, "all completed SS tunnels close")
        before = self.contents("/run/fixture-target.log").count("open ")
        for label, source, host in CLIENTS:
            output = self.probe(source=source, host=host, check=False)
            assert "ECHO_OK" not in output.stdout, reason + ": " + label
        assert self.contents("/run/fixture-target.log").count("open ") == before, reason + ": direct fallback"
        assert self.healthy(), "local health must not depend on B availability/password"
        for _, source, _ in CLIENTS:
            assert "ECHO_OK" in self.fixture("probe", "127.0.0.6", source, "DIRECT", "", "443").stdout
        self.oidc_checks()

    def outage(self):
        streams = []
        try:
            for label, source, host in CLIENTS:
                stream = subprocess.Popen(
                    ["docker", "exec", self.name, "perl", FIXTURE, "probe", "127.0.0.1", source,
                     "CONNECT", host, "443", "disconnect"],
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                )
                streams.append((label, stream))
            eventually(lambda: all(self.exists(f"/run/fixture-stream-ready-{source}-{host}")
                                   for _, source, host in CLIENTS), "active SS streams before B outage")
            self.docker("rm", "-f", self.server_name)
            for label, stream in streams:
                stdout, stderr = stream.communicate(timeout=25)
                assert stream.returncode == 0 and "DISCONNECTED" in stdout, label + stdout + stderr
        finally:
            for _, stream in streams:
                if stream.poll() is None:
                    stream.kill()
                    stream.wait()
        self.fail_closed("B outage")


def run(squid_image, mihomo_image, runtime_image):
    with tempfile.TemporaryDirectory(prefix="codex-ss-image-") as temporary:
        directory = Path(temporary)
        config = (ROOT / "deploy/egress/squid.conf").read_text()
        config = config.replace("172.28.30.3/32", "127.0.0.2/32")
        config = config.replace("172.28.40.3/32", "127.0.0.3/32")
        config = config.replace("172.28.30.2/32", "127.0.0.8/32")
        config = config.replace("http_port 3128", "http_port 127.0.0.1:3128")
        config += (f"\ntcp_outgoing_address {SQUID_IP} codex_clients\n"
                   f"tcp_outgoing_address {SQUID_IP} antigravity_clients\n"
                   "connect_timeout 2 seconds\nforward_timeout 5 seconds\n"
                   "peer_connect_timeout 1 seconds\ndead_peer_timeout 1 seconds\n")
        (directory / "squid.conf").write_text(config)
        (directory / "mihomo_fixture.pl").write_text(
            (ROOT / "scripts/tests/relay_fixture.pl").read_text().replace(": 3128,", ": 17890,")
        )
        # Native SS inbound syntax is pinned by Mihomo v1.19.32's
        # listener/inbound/{base,shadowsocks}.go. No custom crypto simulation.
        server_config = {
            "mode": "rule", "log-level": "warning", "ipv6": False,
            "listeners": [{"name": "synthetic-ss", "type": "shadowsocks",
                           "listen": "127.0.0.5", "port": 24019,
                           "cipher": "aes-128-gcm", "password": PASSWORD,
                           "udp": False}],
            "hosts": {host: "127.0.0.6" for host in ALL_HOSTS},
            "rules": ["MATCH,DIRECT"],
        }
        (directory / "server.json").write_text(json.dumps(server_config))
        for filename in ("squid.conf", "mihomo_fixture.pl", "server.json"):
            (directory / filename).chmod(0o644)
        password = directory / "password"
        password.write_text(PASSWORD + "\n")
        password.chmod(0o640)
        with ShadowsocksSandbox(squid_image, runtime_image, mihomo_image, directory) as sandbox:
            sandbox.allowed_destinations()
            sandbox.acl_checks()
            sandbox.source_checks()
            sandbox.oidc_checks()
            sandbox.check_secrecy()
            for _, source, host in CLIENTS:
                sandbox.expect(200, source=source, host=host, operation="stream")
            eventually(sandbox.no_active_targets, "SS streaming cancellation closes upstreams")
            for _, _, host in CLIENTS:
                eventually(lambda host=host: re.search(
                    rf"{re.escape(host)}:443 200 \S*PARENT/172\.28\.50\.3",
                    sandbox.contents("/var/log/squid/access.log")), "Squid selected only Mihomo parent")
            print("SS: real AES-128-GCM forwarding, special-character password, streaming, whitelist, source restrictions and OIDC direct routing passed", flush=True)

            sandbox.outage()
            sandbox.start_server()
            time.sleep(1.1)
            sandbox.expect(200)
            print("SS: B outage closes active streams, forbids direct fallback, preserves OIDC and recovers", flush=True)

            sandbox.docker("rm", "-f", sandbox.client_name)
            password.write_text("synthetic-wrong-password\n")
            sandbox.start_client()
            sandbox.fail_closed("wrong password")
            sandbox.docker("rm", "-f", sandbox.client_name)
            password.write_text(PASSWORD + "\n")
            sandbox.start_client()
            time.sleep(1.1)
            sandbox.expect(200)
            sandbox.check_secrecy()
            print("SS: wrong password fails closed; recreated client reads rotated password and recovers", flush=True)


if __name__ == "__main__":
    if len(sys.argv) > 2:
        raise SystemExit("usage: test-shadowsocks-image.sh [built-ss-egress-image]")
    locked = dict(line.split("=", 1) for line in (ROOT / "deploy/images.lock.env").read_text().splitlines()
                  if line and not line.startswith("#"))
    for key in ("SQUID_IMAGE", "MIHOMO_IMAGE", "GOLANG_IMAGE", "RUNTIME_IMAGE"):
        if not re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", locked.get(key, "")):
            raise SystemExit(f"SS tests require digest-pinned {key}")
    built = len(sys.argv) == 1
    image = sys.argv[1] if not built else "codex-ss-test-build:" + uuid.uuid4().hex[:12]
    try:
        if built:
            command("docker", "build", "--file", str(ROOT / "deploy/shadowsocks/Dockerfile"),
                    "--tag", image,
                    *(argument for key in ("MIHOMO_IMAGE", "GOLANG_IMAGE", "RUNTIME_IMAGE")
                      for argument in ("--build-arg", key + "=" + locked[key])),
                    str(ROOT), timeout=600)
        run(locked["SQUID_IMAGE"], locked["MIHOMO_IMAGE"], image)
    finally:
        if built:
            command("docker", "image", "rm", image, check=False)
