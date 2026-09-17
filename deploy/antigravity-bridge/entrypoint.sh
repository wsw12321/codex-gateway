#!/bin/sh
set -eu
umask 077

if test "${1:-}" != session; then
    exec dbus-run-session -- /usr/local/bin/antigravity-entrypoint session "$@"
fi
shift

fail() {
    printf '%s\n' "antigravity startup refused: $*" >&2
    exit 1
}

test "$(id -u)" = 10002 || fail 'dedicated uid 10002 is required'
test "${AGY_CLI_DISABLE_AUTO_UPDATE:-}" = true || fail 'automatic updates must remain disabled'
keyring_dir=/var/lib/antigravity/keyrings
key_file=/run/secrets/antigravity_keyring_password
test -d "$keyring_dir" && test ! -L "$keyring_dir" || fail 'keyring directory missing or unsafe'
test "$(stat -c %u "$keyring_dir")" = 10002 || fail 'keyring owner is invalid'
test "$(stat -c %a "$keyring_dir")" = 700 || fail 'keyring mode must be 0700'
test -z "$(find "$keyring_dir" ! -type d ! -type f -print -quit)" || fail 'unsafe keyring entry'
test -z "$(find "$keyring_dir" -type f ! -perm 0600 -print -quit)" || fail 'keyring files must use 0600'
test -z "$(find "$keyring_dir" ! -user 10002 -print -quit)" || fail 'keyring entries must belong to uid 10002'
test -z "$(find "$keyring_dir" -type d ! -perm 0700 -print -quit)" || fail 'keyring directories must use 0700'
test -r "$key_file" && test -s "$key_file" || fail 'keyring password is unavailable'
export GNOME_KEYRING_CONTROL="$XDG_RUNTIME_DIR/keyring"
mkdir -p "$HOME/.gemini/antigravity-cli" "$GNOME_KEYRING_CONTROL"
cp /etc/antigravity/settings.json "$HOME/.gemini/antigravity-cli/settings.json"
# The password goes only through stdin. GNOME Secret Service stores its encrypted
# login keyring in the dedicated volume; all other CLI state uses private tmpfs.
tr -d '\r\n' < "$key_file" | gnome-keyring-daemon --unlock --components=secrets \
    --control-directory="$XDG_RUNTIME_DIR/keyring" >/dev/null 2>&1 || fail 'keyring unlock failed'
gnome-keyring-daemon --start --components=secrets --control-directory="$GNOME_KEYRING_CONTROL" \
    >/dev/null 2>&1 || fail 'Secret Service initialization failed'
attempt=0
until gdbus call --session --dest org.freedesktop.secrets --object-path /org/freedesktop/secrets \
    --method org.freedesktop.DBus.Peer.Ping >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    test "$attempt" -lt 20 || fail 'Secret Service did not start'
    sleep 0.1
done

# A responding D-Bus name alone does not prove that encrypted credentials can
# be read. Never start/login against a locked or missing persistent collection.
locked=$(gdbus call --session --dest org.freedesktop.secrets \
    --object-path /org/freedesktop/secrets/collection/login \
    --method org.freedesktop.DBus.Properties.Get org.freedesktop.Secret.Collection Locked 2>/dev/null) || \
    fail 'persistent login collection is unavailable'
test "$locked" = '(<false>,)' || fail 'persistent login collection is locked'

case "${1:-serve}" in
    serve) exec /usr/local/bin/antigravity-bridge ;;
    verify-keyring) printf '%s\n' 'Persistent keyring is unlocked' ;;
    login)
        # Force the documented manual remote OAuth flow; no host browser/socket
        # or callback port is exposed to this isolated container.
        export SSH_CONNECTION='127.0.0.1 1 127.0.0.1 22'
        cd /tmp
        agy
        exec /usr/local/bin/antigravity-smoke cli
        ;;
    verify-login) exec /usr/local/bin/antigravity-smoke cli ;;
    *) fail 'expected serve, login, verify-login, or verify-keyring' ;;
esac
