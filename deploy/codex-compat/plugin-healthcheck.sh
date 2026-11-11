#!/bin/sh
set -eu

# Healthy means the Gateway plugin is loaded and serving, not merely that the
# port is open: unmodified CPA without the plugin answers this route with 404.
# The key is passed through the shell's printf builtin and a pipe, so it never
# appears in a process argument list.
port=${CLIPROXY_PORT:-8317}
key_file=${CPA_MANAGEMENT_KEY_FILE:-/run/secrets/cpa_management_key}
key=$(tr -d '\r\n' < "$key_file")
response=$(
    printf 'GET /v0/management/gateway/upstream-accounts/capabilities HTTP/1.1\r\nHost: 127.0.0.1:%s\r\nAuthorization: Bearer %s\r\nConnection: close\r\n\r\n' "$port" "$key" |
        nc -w 3 127.0.0.1 "$port"
) || exit 1
unset key
case "$response" in
    "HTTP/1.1 200 "*upstream_account_access_v1*) exit 0 ;;
esac
exit 1
