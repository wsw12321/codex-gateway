#!/bin/sh
set -eu
umask 077

key_file=${CLIPROXY_API_KEY_FILE:-/run/secrets/sidecar_api_key}
port=${CLIPROXY_PORT:-8317}
model=${CODEX_SMOKE_MODEL:-gpt-5-codex}
# Canonical unpadded base64url encoding of a 32-byte synthetic caller scope.
# Production requests use a gateway-generated HMAC; this value only exercises
# the sidecar's strict header contract during the SSH-only smoke test.
affinity_scope=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
response_file=$(mktemp /run/cliproxy/smoke.XXXXXX)
trap 'rm -f "$response_file"' EXIT HUP INT TERM

key=$(tr -d '\r\n' < "$key_file")

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
body=$(printf '{"model":"%s","input":"Reply with OK.","stream":true,"store":false}' "$model")
request POST /v1/responses "$body"
grep -q 'response.completed' "$response_file" || {
    printf '%s\n' 'sidecar smoke test did not observe response.completed' >&2
    exit 1
}
awk '
    !headers_done && $0 ~ /^\r?$/ { headers_done = 1; next }
    !headers_done && tolower($0) ~ /^x-codex-upstream-account:/ {
        count++
        value = $0
        sub(/\r$/, "", value)
        sub(/^[^:]*:[[:space:]]*/, "", value)
        sub(/[[:space:]]*$/, "", value)
        if (length(value) != 16 || value !~ /^[0-9a-f]+$/) invalid = 1
    }
    END { exit !(count == 1 && invalid == 0) }
' "$response_file" || {
    printf '%s\n' 'sidecar smoke test did not observe upstream account attribution' >&2
    exit 1
}

# Never print the upstream response body.
printf '%s\n' 'sidecar account-list, model-list, attribution, and minimal streaming Responses smoke tests passed'
