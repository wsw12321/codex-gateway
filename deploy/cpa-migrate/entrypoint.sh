#!/bin/sh
set -eu
umask 077

fail() {
    printf '%s\n' 'cpa-credentials-migrate: maintenance_environment_invalid' >&2
    exit 1
}

test "$(id -u)" = 0 || fail
test "${AGY_CLI_DISABLE_AUTO_UPDATE:-}" = true || fail
test -d /run/cpa-migrate && test ! -L /run/cpa-migrate || fail
test "$(stat -f -c %T /run/cpa-migrate)" = tmpfs || fail
test "$(stat -c %u /run/cpa-migrate)" = 0 || fail
test "$(stat -c %a /run/cpa-migrate)" = 700 || fail
install -d -o 10002 -g 10002 -m 0700 /run/cpa-migrate/staging /run/cpa-migrate/keyring
# The parent contains only these two directory names. Their private 0700 modes
# protect all contents while allowing the helper's no-symlink directory walk.
chmod 0755 /run/cpa-migrate
exec /usr/local/bin/cpa-credentials-migrate "$@"
