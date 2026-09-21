#!/bin/sh
set -eu
umask 022

fail() {
    printf '%s\n' "egress startup refused: $*" >&2
    exit 1
}

relay_ip=${CODEX_RELAY_IP:-}
relay_port=${CODEX_RELAY_PORT:-3128}

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

render_config() {
    if test -z "$relay_ip"; then
        printf '%s\n' '# Codex relay disabled; preserve direct egress.'
        return
    fi
    printf 'cache_peer %s parent %s 0 no-query default name=codex_relay\n' "$relay_ip" "$relay_port"
    printf '%s\n' \
        'cache_peer_access codex_relay allow codex_clients' \
        'cache_peer_access codex_relay deny all' \
        'never_direct allow codex_clients' \
        'never_direct deny all'
}

# Used by deployment validation without writing /run or starting Squid.
if test "${1:-}" = --render-config; then
    test "$#" -eq 1 || fail '--render-config takes no arguments'
    render_config
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

exec /usr/local/bin/entrypoint.sh "$@"
