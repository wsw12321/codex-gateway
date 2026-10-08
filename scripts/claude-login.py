#!/usr/bin/env python3
"""Private host-side helper for claude-login.sh; uses only the Python stdlib."""

import copy
import fcntl
import json
import os
from pathlib import Path
import re
import select
import selectors
import shutil
import signal
import subprocess
import sys
import termios
import time
from urllib.parse import parse_qsl, urlsplit


REQUEST_TIMEOUT = 10
CALLBACK_TIMEOUT = 300
STATUS_TIMEOUT = 120
POLL_INTERVAL = 2
MAX_RESPONSE = 1024 * 1024
MAX_HEADERS = 16384
MAX_CALLBACK = 16384
OPERATIONS = frozenset(("capabilities", "begin", "callback", "status", "accounts"))

# Only this fixed program crosses into the container. Neither keys nor callback
# data are interpolated into it. printf is a /bin/sh builtin in the CPA image;
# keys stay in unexported shell variables, and the HTTP body arrives on stdin.
TRANSPORT = r'''
set -eu
unset claude_key
case "$1" in
    capabilities)
        method=GET; route=/internal/anthropic-accounts/capabilities
        key_file=/run/secrets/sidecar_api_key ;;
    accounts)
        method=GET; route=/internal/anthropic-accounts
        key_file=/run/secrets/sidecar_api_key ;;
    begin)
        method=POST; route=/internal/gateway-management/anthropic/oauth
        key_file=/run/secrets/cpa_management_key ;;
    callback)
        method=POST; route=/internal/gateway-management/anthropic/oauth/callback
        key_file=/run/secrets/cpa_management_key ;;
    status)
        method=POST; route=/internal/gateway-management/anthropic/oauth/status
        key_file=/run/secrets/cpa_management_key ;;
    *) exit 1 ;;
esac
# Validate bytes before command substitution, which can silently drop NULs.
test "$(wc -c < "$key_file")" -le 257
test "$(LC_ALL=C tr -d 'A-Za-z0-9_\055\012' < "$key_file" | wc -c)" -eq 0
claude_key=$(cat "$key_file")
case "$claude_key" in ''|*[!A-Za-z0-9_-]*) exit 1 ;; esac
test "${#claude_key}" -ge 32
test "${#claude_key}" -le 256
{
    printf '%s %s HTTP/1.1\r\n' "$method" "$route"
    printf 'Authorization: Bearer %s\r\n' "$claude_key"
    cat
} | nc -N -w 9 127.0.0.1 8317
'''


class LoginError(Exception):
    """A fixed diagnostic, never an upstream response or exception string."""


class Interrupted(Exception):
    def __init__(self, signum):
        self.signum = signum


def run_process(args, data=b"", timeout=REQUEST_TIMEOUT):
    """Bound both runtime and output, including when a child never closes stdout."""
    deadline = time.monotonic() + timeout
    with subprocess.Popen(
        args, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL, start_new_session=True, close_fds=True,
    ) as process:
        try:
            result = bytearray()
            pending = memoryview(data)
            with selectors.DefaultSelector() as selector:
                os.set_blocking(process.stdout.fileno(), False)
                selector.register(process.stdout, selectors.EVENT_READ)
                if pending:
                    os.set_blocking(process.stdin.fileno(), False)
                    selector.register(process.stdin, selectors.EVENT_WRITE)
                else:
                    process.stdin.close()
                while selector.get_map():
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise LoginError("CPA request timed out")
                    for key, _ in selector.select(remaining):
                        if key.fileobj is process.stdin:
                            try:
                                written = os.write(process.stdin.fileno(), pending[:4096])
                            except BrokenPipeError:
                                raise LoginError("CPA transport failed") from None
                            pending = pending[written:]
                            if not pending:
                                selector.unregister(process.stdin)
                                process.stdin.close()
                        else:
                            chunk = os.read(process.stdout.fileno(), 65536)
                            if not chunk:
                                selector.unregister(process.stdout)
                            else:
                                result.extend(chunk)
                                if len(result) > MAX_RESPONSE:
                                    raise LoginError("CPA response exceeded the size limit")
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise LoginError("CPA request timed out")
            try:
                status = process.wait(timeout=remaining)
            except subprocess.TimeoutExpired:
                raise LoginError("CPA request timed out") from None
            if status != 0:
                raise LoginError("CPA transport failed; check deployment and container availability")
            return bytes(result)
        finally:
            # Compose's wrapper and Docker CLI must not outlive this request or
            # keep its pipes open. nc has its own timeout inside the container.
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()


def _headers(lines):
    headers = {}
    for line in lines:
        name, sep, value = line.partition(b":")
        if (not sep or not re.fullmatch(rb"[!#$%&'*+.^_`|~0-9A-Za-z-]+", name)
                or any(c < 32 or c > 126 for c in value)):
            raise LoginError("Invalid CPA HTTP headers")
        name = name.lower()
        if name in headers:
            raise LoginError("Duplicate CPA HTTP headers")
        headers[name] = value.strip()
    return headers


def _json_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise LoginError("Duplicate CPA JSON fields")
        result[key] = value
    return result


def _invalid_constant(_value):
    raise LoginError("Invalid CPA JSON response")


def parse_response(raw):
    if len(raw) > MAX_RESPONSE:
        raise LoginError("CPA response exceeded the size limit")
    head, sep, body = raw.partition(b"\r\n\r\n")
    if not sep or len(head) > MAX_HEADERS:
        raise LoginError("Invalid CPA HTTP response")
    status, *lines = head.split(b"\r\n")
    if not re.fullmatch(rb"HTTP/1\.[01] 200(?: [\x20-\x7e]*)?", status):
        # No redirects, error bodies or reason phrases are ever relayed.
        raise LoginError("CPA returned a non-success HTTP response")
    headers = _headers(lines)
    if headers.get(b"content-type", b"").split(b";", 1)[0].lower() != b"application/json":
        raise LoginError("CPA did not return JSON")
    if headers.get(b"content-encoding", b"identity").lower() != b"identity":
        raise LoginError("Unsupported CPA content encoding")
    length = headers.get(b"content-length")
    transfer = headers.get(b"transfer-encoding")
    if transfer is not None:
        if length is not None or transfer.lower() != b"chunked":
            raise LoginError("Invalid CPA HTTP framing")
        decoded = bytearray()
        offset = 0
        while True:
            end = body.find(b"\r\n", offset)
            size_line = body[offset:end] if end >= 0 else b""
            if (end < 0 or len(size_line) > 1024
                    or not re.fullmatch(rb"[0-9a-fA-F]{1,8}(?:;[\x20-\x7e]*)?", size_line)):
                raise LoginError("Invalid CPA chunk framing")
            offset = end + 2
            size = int(size_line.split(b";", 1)[0], 16)
            if size == 0:
                if body[offset:] != b"\r\n":
                    raise LoginError("Unexpected CPA trailers or trailing data")
                body = bytes(decoded)
                break
            if size > len(body) - offset - 2 or body[offset + size:offset + size + 2] != b"\r\n":
                raise LoginError("Truncated CPA chunk")
            decoded.extend(body[offset:offset + size])
            offset += size + 2
    elif length is not None:
        if not re.fullmatch(rb"[0-9]{1,10}", length) or int(length) != len(body):
            raise LoginError("Invalid CPA Content-Length")
    # Without either framing header, EOF delimits the body (Connection: close).
    try:
        result = json.loads(body.decode("utf-8"), object_pairs_hook=_json_object,
                            parse_constant=_invalid_constant)
    except (ValueError, UnicodeError, RecursionError):
        raise LoginError("Invalid CPA JSON response") from None
    if not isinstance(result, dict):
        raise LoginError("Invalid CPA response schema")
    return result


class Client:
    def __init__(self, root):
        self.compose = str(Path(root) / "scripts" / "compose.sh")

    def request(self, operation, payload=None, timeout=REQUEST_TIMEOUT):
        if operation not in OPERATIONS:
            raise LoginError("Invalid internal operation")
        body = b"" if payload is None else json.dumps(
            payload, ensure_ascii=True, separators=(",", ":"), allow_nan=False,
        ).encode("ascii")
        if len(body) > 16384:
            raise LoginError("OAuth request exceeded the size limit")
        data = (b"Host: 127.0.0.1:8317\r\nContent-Type: application/json\r\n"
                b"Connection: close\r\nContent-Length: " + str(len(body)).encode("ascii")
                + b"\r\n\r\n" + body)
        return parse_response(run_process(
            [self.compose, "exec", "-T", "codex-compat", "sh", "-c", TRANSPORT, "sh", operation],
            data, timeout=timeout,
        ))

    def verify_permissions(self):
        run_process([self.compose, "exec", "-T", "codex-compat",
                     "/usr/local/bin/sidecar-entrypoint", "verify-oauth"])


def _url(value):
    if (not isinstance(value, str) or not value or len(value) > MAX_CALLBACK
            or any(ord(c) < 33 or ord(c) > 126 for c in value)
            or "\\" in value or "#" in value
            or re.search(r"%(?![0-9A-Fa-f]{2})", value)):
        raise LoginError("Invalid OAuth URL")
    try:
        parsed = urlsplit(value)
        if parsed.username is not None or parsed.password is not None:
            raise LoginError("OAuth URL must not contain user information")
        query = {}
        for key, item in parse_qsl(parsed.query, keep_blank_values=True,
                                   strict_parsing=True, encoding="utf-8",
                                   errors="strict", max_num_fields=32):
            if key in query:
                raise LoginError("Duplicate OAuth URL parameters")
            query[key] = item
        return parsed, query
    except (ValueError, UnicodeError):
        raise LoginError("Invalid OAuth URL") from None


def validate_authorization(result):
    state = result.get("state")
    if (not isinstance(state, str) or not re.fullmatch(r"[A-Za-z0-9_.-]{16,128}", state)
            or ".." in state):
        raise LoginError("Invalid CPA OAuth state")
    url = result.get("url")
    parsed, query = _url(url)
    if (parsed.scheme != "https" or parsed.netloc != "claude.ai"
            or query.get("state") != state or any(key.startswith("error") for key in query)):
        raise LoginError("Invalid CPA Claude authorization URL or state")
    return url, state


def parse_callback(value, state):
    parsed, query = _url(value)
    if (parsed.scheme != "http" or parsed.netloc not in
            ("localhost:54545", "127.0.0.1:54545", "[::1]:54545")
            or parsed.path != "/callback" or set(query) != {"state", "code"}
            or query["state"] != state):
        raise LoginError("Invalid callback URL or state")
    code = query["code"]
    if (not code or len(code) > 8192
            or any(ord(c) < 33 or ord(c) > 126 for c in code) or "#" in code):
        raise LoginError("Invalid callback authorization code")
    return code


class Terminal:
    def __enter__(self):
        try:
            self.fd = os.open("/dev/tty", os.O_RDWR | os.O_NOCTTY)
        except OSError:
            raise LoginError("An interactive SSH terminal (/dev/tty) is required") from None
        return self

    def __exit__(self, *_args):
        os.close(self.fd)

    def callback(self, state, deadline):
        original = termios.tcgetattr(self.fd)
        hidden = copy.deepcopy(original)
        hidden[3] &= ~(termios.ECHO | termios.ECHONL | termios.ICANON)
        hidden[6][termios.VMIN] = 1
        hidden[6][termios.VTIME] = 0
        try:
            termios.tcsetattr(self.fd, termios.TCSAFLUSH, hidden)
            while True:
                print("Paste the full callback URL (hidden; five-minute limit): ", end="", flush=True)
                value = bytearray()
                overflow = False
                while True:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0 or not select.select([self.fd], [], [], remaining)[0]:
                        raise LoginError("Timed out waiting for the callback URL")
                    part = os.read(self.fd, 1)
                    if not part or part == b"\x04":
                        raise LoginError("Callback input canceled")
                    if part in (b"\n", b"\r"):
                        break
                    if part in (b"\x7f", b"\b"):
                        del value[-1:]
                    elif part == b"\x15":
                        value.clear()
                    elif not overflow:
                        value.extend(part)
                        overflow = len(value) > MAX_CALLBACK
                print(flush=True)
                try:
                    if overflow:
                        raise LoginError("Callback input exceeded the size limit")
                    return parse_callback(value.decode("ascii"), state)
                except (LoginError, UnicodeError):
                    print("Invalid callback; paste the complete loopback URL with matching state again.", flush=True)
        finally:
            # Discard any remaining pasted credentials before restoring echo.
            termios.tcsetattr(self.fd, termios.TCSAFLUSH, original)


def account_lines(result):
    accounts = result.get("accounts")
    if not isinstance(accounts, list) or not accounts:
        raise LoginError("Claude account list is missing or empty")
    lines = []
    seen = set()
    for account in accounts:
        if not isinstance(account, dict):
            raise LoginError("Invalid Claude account list")
        identity = account.get("id")
        email = account.get("masked_email")
        status = account.get("status")
        if (not isinstance(identity, str) or not re.fullmatch(r"[0-9a-f]{16}", identity)
                or identity in seen or not isinstance(email, str) or len(email) > 254
                or not re.fullmatch(r"[A-Za-z0-9]\*\*\*@[A-Za-z0-9.-]+", email)
                or status not in ("available", "unavailable")):
            raise LoginError("Invalid Claude account metadata")
        seen.add(identity)
        lines.append(f"  {identity}  {email}  {status}")
    return lines


class Login:
    def __init__(self, client):
        self.client = client
        self.submitted = False
        self.saved = False

    def run(self, terminal):
        capabilities = self.client.request("capabilities")
        if (capabilities.get("protocol") != "upstream_account_access_v1"
                or capabilities.get("anthropic_messages") != "anthropic_messages_v1"):
            raise LoginError("CPA anthropic_messages_v1 capability is required; deploy the matching CPA")
        input_deadline = time.monotonic() + CALLBACK_TIMEOUT
        url, state = validate_authorization(self.client.request("begin", {}))
        print("Open this authorization link in your local browser:", flush=True)
        print(url, flush=True)
        print("After login, copy the full localhost callback URL from the address bar, even if the page cannot connect.", flush=True)
        code = terminal.callback(state, input_deadline)
        # Mark before I/O: interruption or timeout may follow server receipt.
        self.submitted = True
        try:
            result = self.client.request("callback", {"state": state, "code": code})
            if result != {"status": "ok"}:
                raise LoginError("Invalid callback submission response")
        except LoginError:
            print("Callback submission was not confirmed; checking status without submitting again.", flush=True)
        finally:
            code = None
        deadline = time.monotonic() + STATUS_TIMEOUT
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise LoginError("Timed out waiting for CPA authorization status")
            try:
                result = self.client.request("status", {"state": state}, timeout=min(REQUEST_TIMEOUT, remaining))
            except LoginError:
                # A transient status failure must not cause a second callback.
                result = {"status": "wait"}
            status = result.get("status")
            if status == "ok":
                self.saved = True
                break
            if status == "error":
                raise LoginError("CPA reported an authorization error; inspect account status in the console")
            if status != "wait":
                raise LoginError("Invalid CPA OAuth status response")
            time.sleep(min(POLL_INTERVAL, max(0, deadline - time.monotonic())))
        self.client.verify_permissions()
        lines = account_lines(self.client.request("accounts"))
        print("Claude authorization completed; CPA verified the identity and saved credentials.")
        print("Current Claude accounts (masked; this list does not identify the account just authorized):")
        for line in lines:
            print(line)
        print("Check account status, model prices and grants in the console. No paid generation request was sent.", flush=True)


def check_dependencies(root):
    for command in ("docker", "jq"):
        if shutil.which(command) is None:
            raise LoginError(f"{command} is required")
    for path in (root / ".env", root / "deploy/images.lock.env", root / "scripts/compose.sh"):
        if not path.is_file() or not os.access(path, os.R_OK):
            raise LoginError("Deployment files are missing; configure .env and image locks first")


def main():
    root = Path(__file__).resolve().parents[1]
    login = Login(Client(root))

    def interrupt(signum, _frame):
        raise Interrupted(signum)

    for signum in (signal.SIGINT, signal.SIGHUP, signal.SIGTERM, signal.SIGQUIT, signal.SIGTSTP):
        signal.signal(signum, interrupt)
    try:
        if len(sys.argv) != 1:
            raise LoginError("Use scripts/claude-login.sh without arguments")
        # Fail closed when someone invokes this private helper without its lock.
        try:
            lock_stat = os.fstat(9)
        except OSError:
            raise LoginError("Use scripts/claude-login.sh to acquire the operation lock") from None
        if not os.path.samestat(lock_stat, (root / ".device-login.lock").stat()):
            raise LoginError("Invalid operation lock")
        fcntl.flock(9, fcntl.LOCK_EX | fcntl.LOCK_NB)
        check_dependencies(root)
        with Terminal() as terminal:
            login.run(terminal)
        return 0
    except Interrupted as exc:
        message = "Interrupted"
        exit_code = 128 + exc.signum
    except LoginError as exc:
        message = str(exc)
        exit_code = 1
    except (OSError, ValueError, termios.error):
        message = "Local terminal or deployment operation failed"
        exit_code = 1
    # Do not print exception details from subprocess, JSON, termios or Docker.
    if login.saved:
        print("claude-login: 凭据已保存，检查未通过 (credentials saved; post-login checks did not complete). "
              "Inspect the console; do not repeat login automatically.", file=sys.stderr)
    elif login.submitted:
        print("claude-login: Authorization may still complete in CPA; it has not been canceled. "
              "Check the console before starting another login.", file=sys.stderr)
    else:
        print("claude-login: No callback was submitted; any pending CPA authorization will time out.", file=sys.stderr)
    print(f"claude-login: {message}", file=sys.stderr)
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
