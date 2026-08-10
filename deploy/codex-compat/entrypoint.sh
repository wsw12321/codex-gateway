#!/bin/sh
set -eu
umask 077

auth_dir=${CLIPROXY_AUTH_DIR:-/var/lib/cliproxy/oauth}
run_dir=/run/cliproxy
config_file=${run_dir}/config.yaml
key_file=${CLIPROXY_API_KEY_FILE:-/run/secrets/sidecar_api_key}
listen_addr=${CLIPROXY_LISTEN_ADDR:-0.0.0.0}
listen_port=${CLIPROXY_PORT:-8317}
proxy_url=${CLIPROXY_PROXY_URL:-http://egress-allowlist:3128}

fail() {
    printf '%s\n' "sidecar startup refused: $*" >&2
    exit 1
}

verify_oauth() {
    test -d "$auth_dir" || fail "OAuth directory is missing"
    test ! -L "$auth_dir" || fail "OAuth directory must not be a symlink"

    bad_link=$(find "$auth_dir" -type l -print -quit)
    test -z "$bad_link" || fail "OAuth directory contains a symlink"

    bad_type=$(find "$auth_dir" ! -type d ! -type f -print -quit)
    test -z "$bad_type" || fail "OAuth directory contains a non-regular entry"

    bad_dir_mode=$(find "$auth_dir" -type d ! -perm 0700 -print -quit)
    test -z "$bad_dir_mode" || fail "every OAuth directory must have mode 0700"

    bad_dir_owner=$(find "$auth_dir" -type d ! -user 10001 -print -quit)
    test -z "$bad_dir_owner" || fail "every OAuth directory must be owned by uid 10001"

    bad_mode=$(find "$auth_dir" -type f ! -perm 0600 -print -quit)
    test -z "$bad_mode" || fail "every OAuth file must have mode 0600"

    bad_owner=$(find "$auth_dir" -type f ! -user 10001 -print -quit)
    test -z "$bad_owner" || fail "every OAuth file must be owned by uid 10001"
}

verify_oauth

if test "${1:-}" = "verify-oauth"; then
    exit 0
fi

test -r "$key_file" || fail "internal API key is unreadable"
internal_key=$(tr -d '\r\n' < "$key_file")
case "$internal_key" in
    *[!A-Za-z0-9_-]*|'') fail "internal API key has an invalid format" ;;
esac
test "${#internal_key}" -ge 43 || fail "internal API key is too short"

mkdir -p "$run_dir"
cat > "$config_file" <<EOF
host: "${listen_addr}"
port: ${listen_port}
auth-dir: "${auth_dir}"
proxy-url: "${proxy_url}"
debug: false
commercial-mode: true
logging-to-file: false
request-log: false
error-logs-max-files: 0
usage-statistics-enabled: false
request-retry: 0
passthrough-headers: false
ws-auth: true
pprof:
  enable: false
api-keys:
  - "${internal_key}"
remote-management:
  allow-remote: false
  secret-key: ""
  disable-control-panel: true
  disable-auto-update-panel: true
EOF
unset internal_key
chmod 0600 "$config_file"

if test "$#" -eq 0; then
    set -- -config "$config_file" -local-model
else
    set -- -config "$config_file" -local-model "$@"
fi

exec /usr/local/bin/cli-proxy-api "$@"
