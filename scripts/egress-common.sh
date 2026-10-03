#!/bin/sh
set -eu
# Shared, trusted helpers. Callers use set -eu before sourcing this file.

egress_fail() {
    printf '%s\n' "egress: $*" >&2
    exit 1
}

egress_prepare() {
    egress_auto_oidc=false
    egress_root=$1
    egress_env_file=$egress_root/.env
    egress_lock=$egress_root/deploy/images.lock.env
    test -r "$egress_env_file" || egress_fail 'copy deploy/env.example to .env and configure it first'
    test -r "$egress_lock" || egress_fail 'run scripts/lock-images.sh first'
    command -v jq >/dev/null 2>&1 || egress_fail 'jq is required'
    command -v python3 >/dev/null 2>&1 || egress_fail 'python3 is required'

    # Shell variables override Compose env files. Clear every declared name,
    # plus optional routing names which may be absent from an older .env.
    # Extract names only; values (including multiline quoted values) are left
    # to Compose. Nothing from either file is evaluated by a shell.
    for egress_name in $(sed -nE 's/^[[:space:]]*(export[[:space:]]+)?([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*=.*/\2/p' "$egress_env_file" "$egress_lock"); do
        unset "$egress_name"
    done
    unset EGRESS_MODE CODEX_RELAY_IP CODEX_RELAY_PORT \
        SHADOWSOCKS_SERVER SHADOWSOCKS_PORT SHADOWSOCKS_CIPHER \
        SHADOWSOCKS_PASSWORD SHADOWSOCKS_PASSWORD_FILE MIHOMO_IMAGE \
        GATEWAY_SECRET_GID OIDC_ENABLED OIDC_ISSUER OIDC_CLIENT_ID OIDC_AUTH_HOST \
        COMPOSE_FILE COMPOSE_PROJECT_NAME COMPOSE_PROFILES COMPOSE_ENV_FILES \
        COMPOSE_DISABLE_ENV_FILE COMPOSE_REMOVE_ORPHANS
    unset egress_name

    egress_raw_settings=$(docker compose \
        --project-name codex-gateway --project-directory "$egress_root" \
        --env-file "$egress_env_file" --env-file "$egress_lock" \
        -f "$egress_root/scripts/egress-settings.compose.yml" config --format json) || exit 1
    egress_settings=$(printf '%s' "$egress_raw_settings" |
        python3 "$egress_root/scripts/validate-egress.py" --root "$egress_root") || exit 1
    unset egress_raw_settings
    egress_mode=$(printf '%s' "$egress_settings" | jq -r '.mode')
    egress_oidc_enabled=$(printf '%s' "$egress_settings" | jq -r '.oidc_enabled')
}

egress_compose() {
    # The ordinary wrapper preserves explicit OIDC opt-in. apply-egress sets
    # this flag to retain the .env-selected authentication route on switches.
    if test "${egress_auto_oidc:-false}" = true && test "$egress_oidc_enabled" = true; then
        set -- -f "$egress_root/deploy/oidc.override.yml" "$@"
    fi
    if test "$egress_mode" = shadowsocks; then
        set -- -f "$egress_root/deploy/shadowsocks.override.yml" "$@"
    fi
    docker compose \
        --project-name codex-gateway --project-directory "$egress_root" \
        --env-file "$egress_env_file" --env-file "$egress_lock" \
        -f "$egress_root/docker-compose.yml" "$@"
}
