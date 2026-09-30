#!/bin/sh
set -eu
umask 077

if test "${1:-}" != session; then
    exec dbus-run-session -- /usr/local/bin/antigravity-entrypoint session "$@"
fi
shift

fail() {
    printf 'antigravity: stage=keyring category=%s exit_code=1\n' "$1" >&2
    exit 1
}

test "$(id -u)" = 10002 || fail configuration
test "${AGY_CLI_DISABLE_AUTO_UPDATE:-}" = true || fail configuration
keyring_dir=/var/lib/antigravity/keyrings
key_file=/run/secrets/antigravity_keyring_password
test -d "$keyring_dir" && test ! -L "$keyring_dir" || fail unsafe_path
test "$(stat -c %u "$keyring_dir")" = 10002 || fail invalid_permissions
test "$(stat -c %a "$keyring_dir")" = 700 || fail invalid_permissions
test -z "$(find "$keyring_dir" ! -type d ! -type f -print -quit)" || fail unsafe_path
test -z "$(find "$keyring_dir" -type f ! -perm 0600 -print -quit)" || fail invalid_permissions
test -z "$(find "$keyring_dir" ! -user 10002 -print -quit)" || fail invalid_permissions
test -z "$(find "$keyring_dir" -type d ! -perm 0700 -print -quit)" || fail invalid_permissions
# One credential owner at a time: serving, login, and migration must never
# refresh the same token concurrently. The inherited descriptor holds the lock
# across exec; migration takes the same lock before opening Secret Service.
exec 9>"$keyring_dir/.gateway-refresh.lock"
chmod 0600 "$keyring_dir/.gateway-refresh.lock"
flock -n 9 || fail credential_owner_busy
test -r "$key_file" && test -s "$key_file" || fail configuration
export GNOME_KEYRING_CONTROL="$XDG_RUNTIME_DIR/keyring"
mkdir -p "$HOME/.gemini/antigravity-cli" "$GNOME_KEYRING_CONTROL" 2>/dev/null || fail io_failed
cp /etc/antigravity/settings.json "$HOME/.gemini/antigravity-cli/settings.json" 2>/dev/null || fail io_failed
# The password goes only through stdin. GNOME Secret Service stores its encrypted
# login keyring in the dedicated volume; all other CLI state uses private tmpfs.
tr -d '\r\n' < "$key_file" | gnome-keyring-daemon --unlock --components=secrets \
    --control-directory="$XDG_RUNTIME_DIR/keyring" >/dev/null 2>&1 || fail keyring_failed
gnome-keyring-daemon --start --components=secrets --control-directory="$GNOME_KEYRING_CONTROL" \
    </dev/null >/dev/null 2>&1 || fail keyring_failed
attempt=0
until gdbus call --session --dest org.freedesktop.secrets --object-path /org/freedesktop/secrets \
    --method org.freedesktop.DBus.Peer.Ping </dev/null >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    test "$attempt" -lt 20 || fail keyring_failed
    sleep 0.1
done

# A responding D-Bus name alone does not prove that encrypted credentials can
# be read. Never start/login against a locked or missing persistent collection.
locked=$(gdbus call --session --dest org.freedesktop.secrets \
    --object-path /org/freedesktop/secrets/collection/login \
    --method org.freedesktop.DBus.Properties.Get org.freedesktop.Secret.Collection Locked </dev/null 2>/dev/null) || \
    fail keyring_failed
test "$locked" = '(<false>,)' || fail keyring_failed

case "${1:-serve}" in
    serve) exec /usr/local/bin/antigravity-bridge ;;
    verify-keyring) printf '%s\n' 'Persistent keyring is unlocked' ;;
    login)
        # Force the documented manual remote OAuth flow; no host browser/socket
        # or callback port is exposed to this isolated container.
        export SSH_CONNECTION='127.0.0.1 1 127.0.0.1 22'
        shift
        exec /usr/local/bin/antigravity-bridge auth-login "$@"
        ;;
    reauthorize)
        # Migration retries must use an existing registry slot and retain its
        # Gateway ID and disabled state, never create a new account by typo.
        export SSH_CONNECTION='127.0.0.1 1 127.0.0.1 22'
        shift
        exec /usr/local/bin/antigravity-bridge auth-reauthorize "$@"
        ;;
    verify-login)
        export TERM=dumb
        shift
        exec /usr/local/bin/antigravity-bridge auth-verify "$@" </dev/null
        ;;
    *) fail configuration ;;
esac
