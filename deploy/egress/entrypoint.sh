#!/bin/sh
set -eu
umask 022

fail() {
    printf '%s\n' "egress startup refused: $*" >&2
    exit 1
}

relay_ip=${CODEX_RELAY_IP:-}
relay_port=${CODEX_RELAY_PORT:-3128}
egress_mode=${EGRESS_MODE:-}
if test -z "$egress_mode"; then
    if test -n "$relay_ip"; then egress_mode=relay; else egress_mode=direct; fi
fi
case "$egress_mode" in
    direct|relay|shadowsocks) ;;
    *) fail 'EGRESS_MODE must be direct, relay or shadowsocks' ;;
esac
oidc_enabled=${OIDC_ENABLED:-false}
oidc_host=${OIDC_AUTH_HOST:-}

case "$oidc_enabled" in
    true|false) ;;
    *) fail 'OIDC_ENABLED must be true or false' ;;
esac
validate_hostname() {
    case "$1" in
        ''|*[!a-z0-9.-]*|.*|*.|*..*) return 1 ;;
    esac
    test "${#1}" -le 253 || return 1
    saved_ifs=$IFS
    IFS=.
    set -- $1
    IFS=$saved_ifs
    test "$#" -ge 2 || return 1
    for label do
        case "$label" in ''|-*|*-) return 1 ;; esac
        test "${#label}" -le 63 || return 1
    done
    # Reject literal IP addresses and numeric final labels.
    case "$label" in *[!a-z]*) return 1 ;; esac
}
if test -n "$oidc_host"; then
    validate_hostname "$oidc_host" || fail 'OIDC_AUTH_HOST must be one exact lowercase DNS hostname'
fi
if test "$oidc_enabled" = true && test -z "$oidc_host"; then
    fail 'OIDC_AUTH_HOST is required when OIDC is enabled'
fi

# Accept canonical decimal values only. Validate even when the relay is off so
# a bad dormant port cannot become active during a later configuration change.
case "$relay_port" in
    ''|*[!0-9]*|0*) fail 'CODEX_RELAY_PORT must be a decimal port from 1 to 65535' ;;
esac
test "${#relay_port}" -le 5 && test "$relay_port" -le 65535 || \
    fail 'CODEX_RELAY_PORT must be a decimal port from 1 to 65535'

validate_ipv4() {
    case "$1" in
        *[!0-9.]*|.*|*.|*..*) return 1 ;;
    esac
    test "${#1}" -le 15 || return 1
    saved_ifs=$IFS
    IFS=.
    set -- $1
    IFS=$saved_ifs
    test "$#" -eq 4 || return 1
    for octet do
        case "$octet" in
            ''|0[0-9]*) return 1 ;;
        esac
        test "${#octet}" -le 3 && test "$octet" -le 255 || return 1
    done
}

if test -n "$relay_ip"; then
    validate_ipv4 "$relay_ip" || fail 'CODEX_RELAY_IP must be a canonical IPv4 address'
fi
if test "$egress_mode" = relay && test -z "$relay_ip"; then
    fail 'CODEX_RELAY_IP is required in relay mode'
fi

render_config() {
    if test "$egress_mode" = direct; then
        printf '%s\n' '# Shared Codex/Antigravity relay disabled; preserve direct egress.'
        return
    fi
    if test "$egress_mode" = shadowsocks; then
        # The optional SS overlay is the only attachment to this isolated LAN.
        # Keep one parent and forbid model-provider traffic from falling back.
        printf '%s\n' 'cache_peer 172.28.50.3 parent 17890 0 no-query default name=codex_relay'
    else
        printf 'cache_peer %s parent %s 0 no-query default name=codex_relay\n' "$relay_ip" "$relay_port"
    fi
    printf '%s\n' \
        'cache_peer_access codex_relay allow codex_clients' \
        'cache_peer_access codex_relay allow antigravity_clients' \
        'cache_peer_access codex_relay deny all' \
        'never_direct allow codex_clients' \
        'never_direct allow antigravity_clients' \
        'never_direct deny all'
}

render_oidc_config() {
    if test "$oidc_enabled" = false; then
        printf '%s\n' '# Gateway OIDC egress disabled.'
        return
    fi
    printf 'acl gateway_oidc_upstream dstdomain -n %s\n' "$oidc_host"
    printf '%s\n' \
        'http_access allow CONNECT TLS_port gateway_oidc_clients gateway_oidc_upstream' \
        'always_direct allow gateway_oidc_clients'
}

# Used by deployment validation without writing /run or starting Squid.
if test "${1:-}" = --render-config; then
    test "$#" -eq 1 || fail '--render-config takes no arguments'
    render_config
    exit 0
fi
if test "${1:-}" = --render-oidc-config; then
    test "$#" -eq 1 || fail '--render-oidc-config takes no arguments'
    render_oidc_config
    exit 0
fi

# Squid rereads this include after dropping privileges. It contains no secrets
# and must remain readable by its proxy uid on the otherwise read-only image.
relay_config=/run/codex-relay.conf
relay_tmp=$(mktemp "${relay_config}.XXXXXX")
trap 'rm -f "$relay_tmp"' EXIT HUP INT TERM
render_config > "$relay_tmp"
chmod 0644 "$relay_tmp"
mv -f "$relay_tmp" "$relay_config"
trap - EXIT HUP INT TERM

oidc_config=/run/gateway-oidc.conf
oidc_tmp=$(mktemp "${oidc_config}.XXXXXX")
trap 'rm -f "$oidc_tmp"' EXIT HUP INT TERM
render_oidc_config > "$oidc_tmp"
chmod 0644 "$oidc_tmp"
mv -f "$oidc_tmp" "$oidc_config"
trap - EXIT HUP INT TERM

exec /usr/local/bin/entrypoint.sh "$@"
