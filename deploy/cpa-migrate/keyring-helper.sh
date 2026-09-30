#!/bin/sh
set -eu
umask 077

fail() {
    printf '%s\n' 'cpa-credentials-migrate: keyring_helper_failed' >&2
    exit 1
}

test "$(id -u)" = 10002 || fail
test "${AGY_CLI_DISABLE_AUTO_UPDATE:-}" = true || fail
if test "${1:-}" != session; then
    export XDG_RUNTIME_DIR=/run/cpa-migrate/keyring
    exec dbus-run-session -- /usr/local/bin/cpa-keyring-helper session "$@"
fi
shift
test "$#" = 4 || fail
test "$1" = /usr/local/bin/cpa-credentials-migrate || fail
case "$2" in restore|save|verify) ;; *) fail ;; esac

# Parent Go process owns both persistent refresh locks before this helper can
# start. Only a short-lived Secret Service session runs as the legacy identity.
migration_runtime=$(mktemp -d /run/cpa-migrate/keyring/session.XXXXXXXX) || fail
trap 'rm -rf "$migration_runtime"' EXIT HUP INT TERM
export XDG_RUNTIME_DIR="$migration_runtime"
export XDG_DATA_HOME=/var/lib/antigravity
export GNOME_KEYRING_CONTROL="$migration_runtime/keyring"
mkdir -m 0700 "$GNOME_KEYRING_CONTROL" || fail
test -r /run/secrets/antigravity_keyring_password && test -s /run/secrets/antigravity_keyring_password || fail
password_bytes=$(tr -d '\r\n' < /run/secrets/antigravity_keyring_password | wc -c)
test "$password_bytes" -gt 0 || fail
tr -d '\r\n' < /run/secrets/antigravity_keyring_password | \
    gnome-keyring-daemon --unlock --components=secrets --control-directory="$GNOME_KEYRING_CONTROL" >/dev/null 2>&1 || fail
gnome-keyring-daemon --start --components=secrets --control-directory="$GNOME_KEYRING_CONTROL" </dev/null >/dev/null 2>&1 || fail
"$1" -keyring-operation "$2" -account "$3" -staging-home "$4"
