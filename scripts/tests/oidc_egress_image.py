#!/usr/bin/env python3
"""Verify real Squid OIDC ACLs without site credentials or external traffic."""

import argparse
from pathlib import Path
import re
import sys
import tempfile

from relay_image import ALL_HOSTS, FIXTURE, ROOT, Sandbox, eventually


GATEWAY = "127.0.0.4"
MODEL_SOURCES = ("127.0.0.2", "127.0.0.3")
OIDC_HOSTS = ("auth.example.test", "project.supabase.co")


class OIDCSandbox(Sandbox):
    def __init__(self, image, config, mode, host, enabled, log_dir):
        super().__init__(image, config, enabled=mode == "relay")
        self.name = self.name.replace("codex-relay-test-", "codex-oidc-egress-test-")
        self.mode, self.host, self.oidc_enabled = mode, host, enabled
        self.log_dir = log_dir

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
                *(argument for host in (*ALL_HOSTS, self.host)
                  for argument in ("--add-host", f"{host}:127.0.0.6")),
                "--entrypoint", "/bin/sh", self.image, "-c", "exec sleep 300",
            )
            self.serve("target")
            if self.mode == "relay":
                self.serve("parent")
            self.docker(
                "exec", "-d", "-e", f"EGRESS_MODE={self.mode}",
                "-e", "CODEX_RELAY_IP=127.0.0.5", "-e", "CODEX_RELAY_PORT=3129",
                "-e", f"OIDC_ENABLED={str(self.oidc_enabled).lower()}",
                "-e", f"OIDC_AUTH_HOST={self.host}", self.name,
                "/usr/local/bin/codex-egress-entrypoint.sh", "-f", "/etc/squid/squid.conf", "-NYC",
            )
            eventually(lambda: self.exists("/run/squid.pid"), "Squid startup")
            eventually(lambda: self.probe(source="127.0.0.7", host=self.host,
                                          check=False).stdout.startswith("403\n"),
                       "Squid accepting and denying unauthorized clients")
            return self
        except BaseException:
            self.__exit__(*sys.exc_info())
            raise

    def __exit__(self, kind, value, traceback):
        try:
            if self.log_dir:
                label = f"{self.mode}-{self.host}-{'enabled' if self.oidc_enabled else 'disabled'}"
                for name, source in (("access", "/var/log/squid/access.log"),
                                     ("cache", "/var/log/squid/cache.log"),
                                     ("parent", "/run/fixture-parent.log"),
                                     ("target", "/run/fixture-target.log")):
                    (self.log_dir / f"{label}-{name}.log").write_text(self.contents(source))
        finally:
            super().__exit__(kind, value, traceback)

    def oidc_checks(self):
        self.expect(200 if self.oidc_enabled else 403, source=GATEWAY, host=self.host)
        # An exact issuer/project hostname never grants its parent, sibling,
        # subdomain, model endpoints, raw IPs, plain HTTP, or alternate ports.
        for host in ("other.supabase.co", "supabase.co", f"sub.{self.host}",
                     "example.com", "127.0.0.6", "chatgpt.com", "accounts.google.com"):
            self.expect(403, source=GATEWAY, host=host)
        for port in ("80", "8443"):
            self.expect(403, source=GATEWAY, host=self.host, port=port)
        self.expect(403, source=GATEWAY, host=self.host, method="GET")
        for source in (*MODEL_SOURCES, "127.0.0.7"):
            self.expect(403, source=source, host=self.host)
        if self.oidc_enabled:
            eventually(lambda: re.search(
                rf"{re.escape(self.host)}:443 200 HIER_DIRECT/127\.0\.0\.6",
                self.contents("/var/log/squid/access.log")), "OIDC direct route in access log")
        assert f"CONNECT {self.host}:443" not in self.contents("/run/fixture-parent.log")

    def provider_checks(self):
        for source, host in ((MODEL_SOURCES[0], "chatgpt.com"),
                             (MODEL_SOURCES[1], "cloudcode-pa.googleapis.com")):
            if self.mode == "shadowsocks":
                # The real configured SS peer is intentionally unreachable in
                # --network none. Model traffic must not fall back to direct.
                eventually(self.no_active_targets, "prior target connections close")
                connections = self.contents("/run/fixture-target.log").count("open ")
                result = self.probe(source=source, host=host).stdout
                # Squid can report ERR_CANNOT_FORWARD (500) when the isolated
                # namespace has no route to the configured parent address.
                assert result.splitlines()[0] in ("500", "502", "503", "504"), result
                assert "ECHO_OK" not in result, result
                assert self.contents("/run/fixture-target.log").count("open ") == connections
                assert "ECHO_OK" in self.fixture(
                    "probe", "127.0.0.6", source, "DIRECT", "", "443").stdout
            else:
                self.expect(200, source=source, host=host)
                route = (r"\S*PARENT/127\.0\.0\.5" if self.mode == "relay"
                         else r"HIER_DIRECT/127\.0\.0\.6")
                eventually(lambda: re.search(
                    rf"{re.escape(host)}:443 200 {route}",
                    self.contents("/var/log/squid/access.log")), "provider route in access log")
        if self.mode == "relay":
            self.fixture("stop", "parent")
            self.expect(200 if self.oidc_enabled else 403, source=GATEWAY, host=self.host)
            failed = self.probe(source=MODEL_SOURCES[0], host="chatgpt.com").stdout
            assert failed.splitlines()[0] in ("502", "503", "504"), failed
            assert "ECHO_OK" not in failed, failed


def run(image, log_dir):
    with tempfile.TemporaryDirectory(prefix="codex-oidc-egress-image-") as directory:
        # Only source topology and listener addresses change. Domain/port/
        # method ACLs and the deployed rendering entrypoint remain unchanged.
        config = (ROOT / "deploy/egress/squid.conf").read_text()
        for original, replacement in (("172.28.30.3/32", f"{MODEL_SOURCES[0]}/32"),
                                      ("172.28.40.3/32", f"{MODEL_SOURCES[1]}/32"),
                                      ("172.28.30.2/32", f"{GATEWAY}/32"),
                                      ("http_port 3128", "http_port 127.0.0.1:3128")):
            assert config.count(original) == 1, original
            config = config.replace(original, replacement)
        config += ("\nconnect_timeout 2 seconds\nforward_timeout 5 seconds\n"
                   "peer_connect_timeout 1 seconds\ndead_peer_timeout 1 seconds\n")
        path = Path(directory) / "squid.conf"
        path.write_text(config)
        path.chmod(0o644)
        for mode in ("direct", "relay", "shadowsocks"):
            for enabled, host in ((False, OIDC_HOSTS[0]),
                                  *((True, host) for host in OIDC_HOSTS)):
                with OIDCSandbox(image, path, mode, host, enabled, log_dir) as sandbox:
                    sandbox.oidc_checks()
                    sandbox.provider_checks()
                    print(f"{mode}: OIDC {'enabled' if enabled else 'disabled'} ({host}); "
                          "source/domain/port/method isolation and provider routing passed", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("image", nargs="?", help="digest-pinned Squid image; defaults to image lock")
    parser.add_argument("--log-dir", type=Path, help="optional directory for container access/cache logs")
    args = parser.parse_args()
    locked = dict(line.split("=", 1) for line in (ROOT / "deploy/images.lock.env").read_text().splitlines()
                  if line and not line.startswith("#"))
    image = args.image or locked["SQUID_IMAGE"]
    if not re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", image):
        parser.error("OIDC egress tests require a digest-pinned Squid image")
    if args.log_dir:
        args.log_dir.mkdir(parents=True, exist_ok=True)
    run(image, args.log_dir)
