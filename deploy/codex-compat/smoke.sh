#!/bin/sh
set -eu
umask 077

key_file=${CLIPROXY_API_KEY_FILE:-/run/secrets/sidecar_api_key}
port=${CLIPROXY_PORT:-8317}
model=${1:-${CODEX_SMOKE_MODEL:-gpt-5-codex}}
case "$model" in
    ''|*[!a-zA-Z0-9._-]*) printf '%s\n' 'sidecar smoke: invalid model' >&2; exit 1 ;;
esac
# Canonical unpadded base64url encoding of a 32-byte synthetic caller scope.
# Production requests use a gateway-generated HMAC; this value only exercises
# the sidecar's strict header contract during the SSH-only smoke test.
affinity_scope=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
response_file=$(mktemp /run/cliproxy/smoke.XXXXXX)
trap 'rm -f "$response_file"' EXIT HUP INT TERM

key=$(tr -d '\r\n' < "$key_file")

# Startup remains independent of Gateway. Only generation requires the reverse
# allocation callback, so verify Gateway after both services are running.
{
    printf 'GET /readyz HTTP/1.1\r\nHost: gateway:8080\r\nConnection: close\r\n\r\n'
} | nc -w 3 gateway 8080 > "$response_file" || {
    printf '%s\n' 'sidecar smoke: Gateway is unreachable; start the matching Gateway before generation' >&2
    exit 1
}
case "$(sed -n '1{s/\r$//;p;}' "$response_file")" in
    'HTTP/1.1 200 '*|'HTTP/1.0 200 '*) ;;
    *) printf '%s\n' 'sidecar smoke: Gateway is not ready for account allocation' >&2; exit 1 ;;
esac

request() {
    method=$1
    path=$2
    body=$3
    length=${#body}

    {
        printf '%s %s HTTP/1.1\r\n' "$method" "$path"
        printf 'Host: 127.0.0.1:%s\r\n' "$port"
        printf 'Authorization: Bearer %s\r\n' "$key"
        printf 'X-Codex-Gateway-Affinity: %s\r\n' "$affinity_scope"
        printf 'Content-Type: application/json\r\n'
        printf 'Connection: close\r\n'
        printf 'Content-Length: %s\r\n\r\n' "$length"
        printf '%s' "$body"
    } | nc -w 180 127.0.0.1 "$port" > "$response_file"

    status=$(sed -n '1{s/\r$//;p;}' "$response_file")
    case "$status" in
        'HTTP/1.1 200 '*|'HTTP/1.0 200 '*) ;;
        *)
            printf '%s\n' "sidecar smoke test failed for ${path} (non-200 status)" >&2
            return 1
            ;;
    esac
}

request GET /internal/upstream-accounts ''
grep -q '"accounts"' "$response_file" || {
    printf '%s\n' 'sidecar smoke test did not observe normalized account metadata' >&2
    exit 1
}

request GET /v1/models ''
grep -Fq "\"$model\"" "$response_file" || {
    printf '%s\n' 'sidecar smoke test did not find the requested model' >&2
    exit 1
}
# Generation must traverse Gateway authentication, personal billing and group
# admission. Direct sidecar generation has no trusted user identity and fails
# closed; use a real Gateway API key for the separate JSON/SSE generation smoke.
request GET /internal/upstream-accounts/capabilities ''
grep -Fq '"upstream_account_access_v1"' "$response_file" || {
    printf '%s\n' 'sidecar smoke test did not observe account access enforcement capability' >&2
    exit 1
}
printf '%s\n' 'sidecar account-list, model-list and account access capability smoke tests passed'
printf '%s\n' 'Run JSON and streaming Responses through Gateway with a real user API key to verify generation and billing'
