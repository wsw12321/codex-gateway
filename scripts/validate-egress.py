#!/usr/bin/env python3
"""Validate Compose-parsed routing settings without exposing file credentials."""

import argparse
import ipaddress
import json
import os
from pathlib import Path
import re
import stat
import sys
from urllib.parse import urlsplit


class ValidationError(ValueError):
    pass


def fail(message):
    raise ValidationError(message)


def port(value, name):
    if not re.fullmatch(r"[1-9][0-9]{0,4}", value) or int(value) > 65535:
        fail(f"{name} must be a decimal port from 1 to 65535 without leading zeros")


def ipv4(value, name):
    try:
        if str(ipaddress.IPv4Address(value)) != value:
            raise ValueError()
    except ValueError:
        fail(f"{name} must be a canonical IPv4 address")


def hostname(value):
    if (len(value) > 253 or "." not in value or
            not re.fullmatch(r"[a-z]+", value.rsplit(".", 1)[-1])):
        return False
    return all(re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", label)
               for label in value.split("."))


def password_file(path, gid):
    try:
        # O_NOFOLLOW prevents the final component from changing to a symlink
        # between inspection and opening. The contents never leave this process.
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd, "rb") as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode):
                fail("SHADOWSOCKS_PASSWORD_FILE must be a regular non-symlink file")
            if stat.S_IMODE(info.st_mode) != 0o640:
                fail("SHADOWSOCKS_PASSWORD_FILE must have mode 0640")
            if info.st_gid != gid:
                fail("SHADOWSOCKS_PASSWORD_FILE group must match GATEWAY_SECRET_GID")
            if info.st_size > 4096:
                fail("SHADOWSOCKS_PASSWORD_FILE must contain at most 4096 bytes")
            value = stream.read(4097)
        if len(value) > 4096:
            fail("SHADOWSOCKS_PASSWORD_FILE must contain at most 4096 bytes")
        if value.endswith(b"\n"):
            value = value[:-1]
            if value.endswith(b"\r"):
                value = value[:-1]
        if not value or any(character in value for character in (b"\0", b"\r", b"\n")):
            fail("SHADOWSOCKS_PASSWORD_FILE must contain one nonempty password line")
        try:
            value.decode("utf-8")
        except UnicodeDecodeError:
            fail("SHADOWSOCKS_PASSWORD_FILE must contain valid UTF-8")
    except OSError:
        fail("SHADOWSOCKS_PASSWORD_FILE must be a readable regular non-symlink file")


def validate(settings, root):
    env = settings["services"]["egress-settings"]["environment"]
    if not all(isinstance(value, str) for value in env.values()):
        fail("invalid Compose routing settings")
    raw_mode = env["EGRESS_MODE"]
    if raw_mode not in ("", "direct", "relay", "shadowsocks"):
        fail("EGRESS_MODE must be direct, relay or shadowsocks (or empty for legacy selection)")
    mode = raw_mode or ("relay" if env["CODEX_RELAY_IP"] else "direct")
    # Existing deployments validate a provided relay address/port even when
    # inactive. Explicit direct still permits retaining valid old settings.
    port(env["CODEX_RELAY_PORT"], "CODEX_RELAY_PORT")
    if env["CODEX_RELAY_IP"]:
        ipv4(env["CODEX_RELAY_IP"], "CODEX_RELAY_IP")
    if mode == "relay" and not env["CODEX_RELAY_IP"]:
        fail("EGRESS_MODE=relay requires CODEX_RELAY_IP")
    if env["OIDC_ENABLED"] not in ("true", "false"):
        fail("OIDC_ENABLED must be true or false")
    oidc_enabled = env["OIDC_ENABLED"] == "true"
    host = env["OIDC_AUTH_HOST"]
    if (host or oidc_enabled) and not hostname(host):
        fail("OIDC_AUTH_HOST must be an exact lowercase DNS hostname")
    if oidc_enabled:
        issuer = env["OIDC_ISSUER"]
        if not re.fullmatch(r"https://[a-z0-9.-]+(?:/[A-Za-z0-9._~/-]+)?", issuer):
            fail("OIDC_ISSUER must be an HTTPS issuer on exactly OIDC_AUTH_HOST")
        parsed = urlsplit(issuer)
        if (parsed.scheme != "https" or parsed.netloc != host or parsed.query or
                parsed.fragment or any(character.isspace() for character in issuer)):
            fail("OIDC_ISSUER must be an HTTPS issuer on exactly OIDC_AUTH_HOST")
        client_id = env["OIDC_CLIENT_ID"]
        if (not client_id or len(client_id) > 512 or
                re.search(r"[\s\x00-\x1f\x7f-\x9f]", client_id)):
            fail("OIDC_CLIENT_ID must contain 1 to 512 characters without whitespace or controls")
    if mode == "shadowsocks":
        server = env["SHADOWSOCKS_SERVER"]
        if not hostname(server):
            ipv4(server, "SHADOWSOCKS_SERVER")
        port(env["SHADOWSOCKS_PORT"], "SHADOWSOCKS_PORT")
        if env["SHADOWSOCKS_CIPHER"] not in (
                "aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305"):
            fail("SHADOWSOCKS_CIPHER must be a supported AEAD cipher")
        gid = env["GATEWAY_SECRET_GID"]
        if not re.fullmatch(r"[1-9][0-9]{0,9}", gid) or int(gid) > 4294967294:
            fail("GATEWAY_SECRET_GID must be a nonzero numeric group ID")
        file_name = env["SHADOWSOCKS_PASSWORD_FILE"]
        if not file_name or "\0" in file_name or "\n" in file_name or "\r" in file_name:
            fail("SHADOWSOCKS_PASSWORD_FILE must name a password file")
        path = Path(file_name)
        if not path.is_absolute():
            path = root / path
        password_file(path, int(gid))
    return {"mode": mode, "oidc_enabled": oidc_enabled}


def deployment_settings(model):
    """Revalidate the frozen model actually used by apply, after overlay merge."""
    services = model["services"]
    squid = services["egress-allowlist"]["environment"]
    gateway = services["gateway"]["environment"]
    mode = squid.get("EGRESS_MODE", "") or ("relay" if squid.get("CODEX_RELAY_IP") else "direct")
    if ("ss-egress" in services) != (mode == "shadowsocks"):
        fail("EGRESS_MODE and the rendered Shadowsocks overlay disagree")
    oidc = squid.get("OIDC_ENABLED", "false")
    if gateway.get("OIDC_ENABLED", "false") != oidc:
        fail("rendered Gateway and Squid OIDC settings disagree")
    env = {
        "EGRESS_MODE": squid.get("EGRESS_MODE", ""),
        "CODEX_RELAY_IP": squid.get("CODEX_RELAY_IP", ""),
        "CODEX_RELAY_PORT": squid.get("CODEX_RELAY_PORT", "3128"),
        "OIDC_ENABLED": oidc,
        "OIDC_AUTH_HOST": squid.get("OIDC_AUTH_HOST", ""),
        "OIDC_ISSUER": gateway.get("OIDC_ISSUER", ""),
        "OIDC_CLIENT_ID": gateway.get("OIDC_CLIENT_ID", ""),
    }
    if mode == "shadowsocks":
        service = services["ss-egress"]
        env.update(service["environment"])
        env["GATEWAY_SECRET_GID"] = service["user"].split(":")[1]
        env["SHADOWSOCKS_PASSWORD_FILE"] = model["secrets"]["shadowsocks_password"]["file"]
    return {"services": {"egress-settings": {"environment": env}}}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--deployment", action="store_true")
    args = parser.parse_args()
    try:
        settings = json.load(sys.stdin)
        if args.deployment:
            settings = deployment_settings(settings)
        result = validate(settings, args.root)
    except (ValueError, KeyError, TypeError, IndexError):
        error = sys.exc_info()[1]
        # JSON decoder and key errors can include arbitrary input. Only our
        # deliberate validation failures are suitable for operator logs.
        message = str(error) if isinstance(error, ValidationError) else "invalid Compose routing settings"
        print(f"egress: {message}", file=sys.stderr)
        return 1
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    sys.exit(main())
