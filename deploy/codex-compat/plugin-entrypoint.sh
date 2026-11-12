#!/bin/sh
# Entrypoint of the plugin-mode CPA image: unmodified CPA, the residual
# plugin-only selector and the Gateway plugin. deploy/codex-compat/entrypoint.sh
# remains the entrypoint of the source-patched image and is reused here for the
# credential-store checks, so both images accept exactly the same volume.
set -eu
umask 077

auth_dir=${CLIPROXY_AUTH_DIR:-/var/lib/cliproxy/oauth}
run_dir=/run/cliproxy
config_file=${run_dir}/config.yaml
key_file=${CLIPROXY_API_KEY_FILE:-/run/secrets/sidecar_api_key}
management_key_file=${CPA_MANAGEMENT_KEY_FILE:-/run/secrets/cpa_management_key}
listen_addr=${CLIPROXY_LISTEN_ADDR:-0.0.0.0}
listen_port=${CLIPROXY_PORT:-8317}
proxy_url=${CLIPROXY_PROXY_URL:-http://egress-allowlist:3128}
oauth_entrypoint=/usr/local/bin/sidecar-oauth-entrypoint
plugin_dir=/usr/local/lib/cliproxy/plugins
plugin_file=${plugin_dir}/codex-gateway-v1.0.0.so
cpa_binary=/usr/local/bin/cli-proxy-api

fail() {
    printf '%s\n' "sidecar startup refused: $*" >&2
    exit 1
}

read_key() {
    # Prints the key in $1 after checking its shape; $2 names it in errors.
    test -r "$1" || fail "$2 is unreadable"
    key_value=$(tr -d '\r\n' < "$1")
    case "$key_value" in
        *[!A-Za-z0-9_-]*|'') fail "$2 has an invalid format" ;;
    esac
    test "${#key_value}" -ge 43 || fail "$2 is too short"
    printf '%s' "$key_value"
    unset key_value
}

case "${1:-}" in
    verify-oauth|oauth-inventory)
        exec "$oauth_entrypoint" "$@"
        ;;
esac
"$oauth_entrypoint" verify-oauth

case "$listen_port" in
    *[!0-9]*|'') fail "listen port is invalid" ;;
esac
case "$listen_addr" in
    *[!0-9A-Fa-f.:]*|'') fail "listen address is invalid" ;;
esac
case "$proxy_url" in
    *[!A-Za-z0-9._~:/@%+-]*) fail "proxy URL has an invalid format" ;;
esac

internal_key=$(read_key "$key_file" "internal API key")
management_key=$(read_key "$management_key_file" "management key")
# The management key opens CPA's whole management API. It must never be the
# key that travels with every generation request.
test "$internal_key" != "$management_key" || fail "management key must differ from the internal API key"

# The plugin decides which upstream account serves which user. Only the image
# may provide it: a regular, root-owned file the service user cannot replace.
test -f "$plugin_file" && test ! -L "$plugin_file" || fail "Gateway plugin is missing"
test ! -w "$plugin_file" && test ! -w "$plugin_dir" || fail "Gateway plugin must not be writable by the service"
unexpected_plugin=$(find "$plugin_dir" -mindepth 1 ! -path "$plugin_file" -print -quit)
test -z "$unexpected_plugin" || fail "plugin directory contains an unexpected entry"

# CPA maps a plugin from a private copy under its temporary directory. A
# noexec or too-small directory makes the load fail, which is closed but not
# useful, so refuse early with a reason instead.
plugin_tmp=${TMPDIR:-/tmp}
test -d "$plugin_tmp" && test -w "$plugin_tmp" || fail "temporary directory is not writable"

# Keep a stable lock inode across startup, login and server-side migration.
# FD 9 remains inherited by CPA, so no second process can refresh this store.
exec 9>"$auth_dir/.gateway-refresh.lock"
flock -n 9 || fail "OAuth store is owned by another refresh process"

mkdir -p "$run_dir"
cat > "$config_file" <<CONFIG
host: "${listen_addr}"
port: ${listen_port}
auth-dir: "${auth_dir}"
proxy-url: "${proxy_url}"
# The Gateway is the only client. Keep its internal SSE uncompressed so usage
# and first-token timing are parsed from the stream as it is produced.
response-compression:
  enabled: false
# Exactly one plugin, shipped in the image. The plugin store is unreachable:
# the directory is read-only and the egress allowlist has no registry.
plugins:
  enabled: true
  dir: "${plugin_dir}"
  configs:
    codex-gateway:
      enabled: true
      priority: 100
discovery:
  enabled: false
codex:
  response-steering: false
claude:
  tool-search: false
tls-handshake-timeout-seconds: 0
debug: false
commercial-mode: true
logging-to-file: false
request-log: false
error-logs-max-files: 0
usage-statistics-enabled: false
request-retry: 0
max-retry-credentials: 4
max-retry-interval: 0
passthrough-headers: false
ws-auth: true
streaming:
  send-keepalive-before-ft: false
  # One request may reach at most max-retry-credentials accounts. A
  # handler-layer retry would start a fresh budget and exceed that cap.
  bootstrap-retries: 0
routing:
  # Account selection belongs to the Gateway plugin alone. This strategy comes
  # from the residual patch: with no plugin decision the request is rejected,
  # never handed to a built-in strategy. Session affinity stays off because the
  # plugin keeps the bindings and rechecks authorization on every reuse.
  strategy: "plugin-only"
  session-affinity: false
pprof:
  enable: false
api-keys:
  - "${internal_key}"
# The plugin's account routes are served below /v0/management, so the
# management API is on. It is reachable only on the internal network and only
# with its own key; the control panel and its updater stay off.
remote-management:
  allow-remote: true
  secret-key: "${management_key}"
  disable-control-panel: true
  disable-auto-update-panel: true
CONFIG
unset internal_key management_key
chmod 0600 "$config_file"

# Plugin wiring. The Gateway origin and the per-account egress listener are
# fixed, as they were in the source patch: neither the allocation callback nor
# an account's route may be redirected through the environment.
export CPA_PLUGIN_GATEWAY_URL=http://gateway:8080
export CPA_PLUGIN_EGRESS_PROXY_HOST=egress-allowlist:3129
export CPA_PLUGIN_LOOPBACK_URL="http://127.0.0.1:${listen_port}"
export CPA_PLUGIN_AUTH_DIR="$auth_dir"
export CLIPROXY_API_KEY_FILE="$key_file"
export CPA_MANAGEMENT_KEY_FILE="$management_key_file"

if test "$#" -eq 0; then
    set -- -config "$config_file" -local-model
else
    set -- -config "$config_file" -local-model "$@"
fi

exec "$cpa_binary" "$@"
