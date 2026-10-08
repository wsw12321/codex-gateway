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
CODEX_HOSTS = ("auth.openai.com", "chatgpt.com")


# The static policy tests pin A's reviewed list. Exercise every one of those
# destinations against both A and B, including OAuth and model endpoints.
def upstream_hosts(acl):
    return tuple(host for host in next(
        line for line in (ROOT / "deploy/egress/squid.conf").read_text().splitlines()
        if line.startswith(f"acl {acl} dstdomain ")
    ).split()[3:] if host != "-n")


CPA_GOOGLE_HOSTS = upstream_hosts("cpa_google_upstreams")
ANTIGRAVITY_HOSTS = upstream_hosts("antigravity_upstreams")
CPA_ANTHROPIC_HOSTS = upstream_hosts("cpa_anthropic_upstreams")
CPA_HOSTS = (*CODEX_HOSTS, *CPA_GOOGLE_HOSTS, *CPA_ANTHROPIC_HOSTS)
ALL_HOSTS = tuple(dict.fromkeys((*CPA_HOSTS, *ANTIGRAVITY_HOSTS)))
CLIENTS = (
    ("Codex", "127.0.0.2", "chatgpt.com"),
    ("CPA Google", "127.0.0.2", "daily-cloudcode-pa.sandbox.googleapis.com"),
    ("CPA Anthropic", "127.0.0.2", "api.anthropic.com"),
    ("Antigravity", "127.0.0.3", "cloudcode-pa.googleapis.com"),
)


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
                *(("--ulimit", "nofile=4096:4096") if self.relay else ()),
                "--security-opt", "no-new-privileges:true",
                "--tmpfs", "/run:rw,noexec,nosuid,nodev,size=8m",
                "--tmpfs", "/var/log/squid:rw,noexec,nosuid,nodev,size=16m,mode=0750,uid=13,gid=13",
                "--tmpfs", "/var/spool/squid:rw,noexec,nosuid,nodev,size=64m,mode=0750,uid=13,gid=13",
                "--mount", f"type=bind,source={self.config},target=/etc/squid/squid.conf,readonly",
                "--mount", f"type=bind,source={ROOT / 'deploy/egress/entrypoint.sh'},target=/usr/local/bin/codex-egress-entrypoint.sh,readonly",
                "--mount", f"type=bind,source={ROOT / 'scripts/tests/relay_fixture.pl'},target={FIXTURE},readonly",
                *(argument for host in ALL_HOSTS
                  for argument in ("--add-host", f"{host}:127.0.0.6")),
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
        context = f"relay={self.relay} enabled={self.enabled} expected={status} request={kwargs}: {output}"
        assert output.splitlines()[0] == str(status), context
        if status == 200:
            assert "ECHO_OK" in output, context

    def no_active_targets(self):
        records = self.contents("/run/fixture-target.log").splitlines()
        return sum(line.startswith("open ") for line in records) == sum(
            line.startswith("close ") for line in records)

    def acl_checks(self):
        for _, source, allowed_host in CLIENTS:
            source = "127.0.0.1" if self.relay else source
            self.expect(403, source="127.0.0.4", host=allowed_host)
            self.expect(403, source=source, host=allowed_host, method="GET")
            self.expect(403, source=source, host=allowed_host, port="8443")
            for host in ("example.com", "api.openai.com", "sub.chatgpt.com", "127.0.0.6",
                         "google.com", "googleapis.com", "sub.accounts.google.com",
                         "sub.cloudcode-pa.googleapis.com", "storage.googleapis.com",
                         "sub.daily-cloudcode-pa.sandbox.googleapis.com"):
                self.expect(403, source=source, host=host)
        if not self.relay:
            for host in ANTIGRAVITY_HOSTS:
                if host not in CPA_HOSTS:
                    self.expect(403, source="127.0.0.2", host=host)
            for host in CPA_HOSTS:
                if host not in ANTIGRAVITY_HOSTS:
                    self.expect(403, source="127.0.0.3", host=host)

    def allowed_destinations(self):
        for hosts, source in ((CPA_HOSTS, "127.0.0.2"), (ANTIGRAVITY_HOSTS, "127.0.0.3")):
            for host in hosts:
                self.expect(200, source="127.0.0.1" if self.relay else source, host=host)


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
            sandbox.allowed_destinations()
            sandbox.acl_checks()
            print("A without relay: Codex, CPA Google and legacy Antigravity direct tunnels and access restrictions passed", flush=True)

        with Sandbox(image, paths["a"], enabled=True) as sandbox:
            sandbox.allowed_destinations()
            for _, source, host in CLIENTS:
                sandbox.expect(200, source=source, host=host, operation="stream")
            sandbox.acl_checks()
            parent_records = sandbox.contents("/run/fixture-parent.log").splitlines()
            for host in ALL_HOSTS:
                assert f"CONNECT {host}:443" in parent_records, host
            eventually(sandbox.no_active_targets, "all clients' cancellation closes upstream tunnels")

            streams = []
            try:
                for label, source, host in CLIENTS:
                    stream = subprocess.Popen(
                        ["docker", "exec", sandbox.name, "perl", FIXTURE, "probe", "127.0.0.1", source,
                         "CONNECT", host, "443", "disconnect"],
                        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                    )
                    streams.append((label, stream))
                eventually(lambda: all(sandbox.exists(f"/run/fixture-stream-ready-{source}-{host}")
                                       for _, source, host in CLIENTS), "all active tunnels before outage")
                sandbox.fixture("stop", "parent")
                for label, stream in streams:
                    stdout, stderr = stream.communicate(timeout=20)
                    assert stream.returncode == 0 and "DISCONNECTED" in stdout, label + stdout + stderr
            finally:
                for _, stream in streams:
                    if stream.poll() is None:
                        stream.kill()
                        stream.wait()
            eventually(sandbox.no_active_targets, "relay outage closes all upstream tunnels")
            target_connections = sandbox.contents("/run/fixture-target.log").count("open ")
            for label, source, host in CLIENTS:
                output = sandbox.probe(source=source, host=host).stdout
                assert output.splitlines()[0] in ("502", "503", "504"), label + output
                assert "ECHO_OK" not in output, label + output
            assert sandbox.contents("/run/fixture-target.log").count("open ") == target_connections
            # Direct reachability from both sources survives the outage. This
            # proves the failures above are policy, not network isolation.
            for _, source, _ in CLIENTS:
                assert "ECHO_OK" in sandbox.fixture("probe", "127.0.0.6", source, "DIRECT", "", "443").stdout
            sandbox.serve("parent")
            time.sleep(1.1)
            for label, source, host in CLIENTS:
                sandbox.expect(200, source=source, host=host)
                eventually(lambda: re.search(rf"{re.escape(host)}:443 200 \S*PARENT/127\.0\.0\.5",
                                             sandbox.contents("/var/log/squid/access.log")),
                           f"{label} log identifies the actual parent")
            eventually(sandbox.no_active_targets, "recovered tunnels close after cancellation")
            print("A with relay: Codex, CPA Google and legacy Antigravity require the parent; fail-closed outage, streaming, cancellation and recovery passed", flush=True)

        with Sandbox(image, paths["b"], relay=True) as sandbox:
            sandbox.allowed_destinations()
            for _, _, host in CLIENTS:
                sandbox.expect(200, host=host, operation="stream")
            sandbox.acl_checks()
            eventually(sandbox.no_active_targets, "B releases cancelled tunnels")
            print("B: all exact domain lists, streaming, cancellation and access restrictions passed", flush=True)


if __name__ == "__main__":
    if len(sys.argv) > 2:
        raise SystemExit("usage: test-relay-image.sh [pinned-squid-image]")
    locked = dict(line.split("=", 1) for line in (ROOT / "deploy/images.lock.env").read_text().splitlines()
                  if line and not line.startswith("#"))
    image = sys.argv[1] if len(sys.argv) == 2 else locked["SQUID_IMAGE"]
    if not re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", image):
        raise SystemExit("relay tests require a digest-pinned Squid image")
    run(image)
