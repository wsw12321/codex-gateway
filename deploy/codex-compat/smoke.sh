#!/bin/sh
set -eu
umask 077

key_file=${CLIPROXY_API_KEY_FILE:-/run/secrets/sidecar_api_key}
port=${CLIPROXY_PORT:-8317}
model=${CODEX_SMOKE_MODEL:-gpt-5-codex}
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

request GET /v1/models ''
body=$(printf '{"model":"%s","input":"Reply with OK.","stream":true,"store":false}' "$model")
request POST /v1/responses "$body"
grep -q 'response.completed' "$response_file" || {
    printf '%s\n' 'sidecar smoke test did not observe response.completed' >&2
    exit 1
}

# Never print the upstream response body.
printf '%s\n' 'sidecar model-list and minimal streaming Responses smoke tests passed'

