#!/usr/bin/env python3
"""Exercise pinned Squid with synthetic loopback peers and no external network."""

from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import uuid


ROOT = Path(__file__).resolve().parents[2]
FIXTURE = "/tmp/relay_fixture.pl"


def command(*args, check=True, timeout=45):
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    if check and result.returncode:
        raise RuntimeError(f"{' '.join(args)}\n{result.stdout}{result.stderr}")
    return result


def eventually(predicate, description, seconds=12):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.15)
    raise AssertionError(f"timed out: {description}")


class Sandbox:
    def __init__(self, image, config, enabled=False, relay=False):
        self.image, self.config = image, config
        self.enabled, self.relay = enabled, relay
        self.name = "codex-relay-test-" + uuid.uuid4().hex[:12]

    def docker(self, *args, **kwargs):
        return command("docker", *args, **kwargs)

    def exec(self, *args, **kwargs):
        return self.docker("exec", self.name, *args, **kwargs)

    def fixture(self, *args, **kwargs):
        return self.exec("perl", FIXTURE, *args, **kwargs)

    def exists(self, path):
        return self.exec("test", "-e", path, check=False).returncode == 0

    def contents(self, path):
        return self.exec("cat", path, check=False).stdout

    def serve(self, service):
        self.docker("exec", "-d", self.name, "perl", FIXTURE, "serve", service)
        eventually(lambda: self.exists(f"/run/fixture-{service}.pid"), f"{service} listener")

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
                "--add-host", "auth.openai.com:127.0.0.6", "--add-host", "chatgpt.com:127.0.0.6",
                "--add-host", "accounts.google.com:127.0.0.6",
                "--entrypoint", "/bin/sh", self.image, "-c", "exec sleep 300",
            )
            self.serve("target")
            if self.enabled:
                self.serve("parent")
            entrypoint = "/usr/local/bin/entrypoint.sh" if self.relay else "/usr/local/bin/codex-egress-entrypoint.sh"
            self.docker("exec", "-d", "-e", "CODEX_RELAY_IP=" + ("127.0.0.5" if self.enabled else ""),
                        "-e", "CODEX_RELAY_PORT=3129", self.name, entrypoint,
                        "-f", "/etc/squid/squid.conf", "-NYC")
            eventually(lambda: self.exists("/run/squid.pid"), "Squid startup")
            eventually(lambda: self.probe(check=False).returncode == 0, "Squid accepting tunnels")
            return self
        except BaseException:
            self.__exit__(*sys.exc_info())
            raise

    def __exit__(self, kind, value, traceback):
        if kind:
            print(self.contents("/var/log/squid/cache.log"), file=sys.stderr)
        self.docker("rm", "-f", self.name, check=False)

    def probe(self, source=None, method="CONNECT", host="chatgpt.com", port="443",
              operation="echo", check=True):
        source = source or ("127.0.0.1" if self.relay else "127.0.0.2")
        return self.fixture("probe", "127.0.0.1", source, method, host, port, operation, check=check)

    def expect(self, status, **kwargs):
        output = self.probe(**kwargs).stdout
        assert output.splitlines()[0] == str(status), output
        if status == 200:
            assert "ECHO_OK" in output, output

    def no_active_targets(self):
        records = self.contents("/run/fixture-target.log").splitlines()
        return sum(line.startswith("open ") for line in records) == sum(
            line.startswith("close ") for line in records)

    def acl_checks(self):
        self.expect(403, source="127.0.0.4")
        self.expect(403, method="GET")
        self.expect(403, port="8443")
        for host in ("example.com", "api.openai.com", "sub.chatgpt.com", "127.0.0.6"):
            self.expect(403, host=host)
        if not self.relay:
            self.expect(403, host="accounts.google.com")
            self.expect(403, source="127.0.0.3", host="chatgpt.com")


def run(image):
    # Replace only topology addresses for loopback isolation. Domain, method,
    # port and parent rules are the real deployment configuration.
    with tempfile.TemporaryDirectory(prefix="codex-relay-image-") as directory:
        paths = {}
        for label, source in (("a", "deploy/egress/squid.conf"), ("b", "deploy/relay/squid.conf")):
            config = (ROOT / source).read_text()
            config = config.replace("172.28.30.3/32", "127.0.0.2/32")
            config = config.replace("172.28.40.3/32", "127.0.0.3/32")
            config = config.replace("10.77.0.1/32", "127.0.0.1/32")
            config = config.replace("http_port 10.77.0.2:3128", "http_port 127.0.0.1:3128")
            config = config.replace("http_port 3128", "http_port 127.0.0.1:3128")
            config += "\nconnect_timeout 2 seconds\nforward_timeout 5 seconds\npeer_connect_timeout 1 seconds\ndead_peer_timeout 1 seconds\n"
            paths[label] = Path(directory) / f"{label}.conf"
            paths[label].write_text(config)
            paths[label].chmod(0o644)

        with Sandbox(image, paths["a"]) as sandbox:
            sandbox.expect(200, host="auth.openai.com")
            sandbox.expect(200, source="127.0.0.3", host="accounts.google.com")
            sandbox.acl_checks()
            print("A without relay: direct tunnels and access restrictions passed", flush=True)

        with Sandbox(image, paths["a"], enabled=True) as sandbox:
            sandbox.expect(200, host="auth.openai.com")
            sandbox.expect(200, operation="stream")
            sandbox.acl_checks()
            # A successful tunnel can only have used the mock parent if it
            # recorded the CONNECT; prove the target itself is reachable too.
            assert "CONNECT auth.openai.com:443" in sandbox.contents("/run/fixture-parent.log")
            assert "ECHO_OK" in sandbox.fixture("probe", "127.0.0.6", "127.0.0.2", "DIRECT", "", "443").stdout
            before = sandbox.contents("/run/fixture-parent.log").count("CONNECT ")
            sandbox.expect(200, source="127.0.0.3", host="accounts.google.com")
            assert sandbox.contents("/run/fixture-parent.log").count("CONNECT ") == before
            eventually(sandbox.no_active_targets, "client cancellation closes upstream tunnels")

            stream = subprocess.Popen(
                ["docker", "exec", sandbox.name, "perl", FIXTURE, "probe", "127.0.0.1", "127.0.0.2",
                 "CONNECT", "chatgpt.com", "443", "disconnect"],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            )
            try:
                eventually(lambda: sandbox.exists("/run/fixture-stream-ready"), "active tunnel before outage")
                sandbox.fixture("stop", "parent")
                stdout, stderr = stream.communicate(timeout=20)
                assert stream.returncode == 0 and "DISCONNECTED" in stdout, stdout + stderr
            finally:
                if stream.poll() is None:
                    stream.kill()
                    stream.wait()
            output = sandbox.probe().stdout
            assert output.splitlines()[0] in ("502", "503", "504"), output
            assert "ECHO_OK" not in output, output
            assert "ECHO_OK" in sandbox.fixture("probe", "127.0.0.6", "127.0.0.2", "DIRECT", "", "443").stdout
            sandbox.expect(200, source="127.0.0.3", host="accounts.google.com")
            sandbox.serve("parent")
            time.sleep(1.1)
            sandbox.expect(200)
            eventually(lambda: re.search(r"chatgpt\.com:443 200 \S*PARENT/127\.0\.0\.5",
                                         sandbox.contents("/var/log/squid/access.log")),
                       "Codex log identifies the actual parent")
            print("A with relay: required parent, fail-closed outage, Antigravity direct, streaming, cancellation and recovery passed", flush=True)

        with Sandbox(image, paths["b"], relay=True) as sandbox:
            sandbox.expect(200, host="auth.openai.com")
            sandbox.expect(200, operation="stream")
            sandbox.acl_checks()
            sandbox.expect(403, host="accounts.google.com")
            eventually(sandbox.no_active_targets, "B releases cancelled tunnels")
            print("B: allowed tunnels, streaming, cancellation and all access restrictions passed", flush=True)


if __name__ == "__main__":
    if len(sys.argv) > 2:
        raise SystemExit("usage: test-relay-image.sh [pinned-squid-image]")
    locked = dict(line.split("=", 1) for line in (ROOT / "deploy/images.lock.env").read_text().splitlines()
                  if line and not line.startswith("#"))
    image = sys.argv[1] if len(sys.argv) == 2 else locked["SQUID_IMAGE"]
    if not re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", image):
        raise SystemExit("relay tests require a digest-pinned Squid image")
    run(image)
