#!/bin/sh
set -eu
umask 077

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
test "$#" -eq 0 || { printf '%s\n' 'usage: claude-login.sh' >&2; exit 1; }
for dependency in python3 flock; do
    command -v "$dependency" >/dev/null 2>&1 || {
        printf 'claude-login: %s is required\n' "$dependency" >&2
        exit 1
    }
done

lock_file=$root/.device-login.lock
if test -e "$lock_file" || test -L "$lock_file"; then
    test -f "$lock_file" && test ! -L "$lock_file" || {
        printf '%s\n' 'claude-login: lock path must be a regular file' >&2
        exit 1
    }
fi
exec 9>"$lock_file"
chmod 0600 "$lock_file"
flock -n 9 || {
    printf '%s\n' 'claude-login: another login or upgrade operation holds the lock' >&2
    exit 1
}
# The helper inherits the lock; its container subprocesses do not. Process exit
# releases it even on interruption, without removing the shared lock inode.
exec python3 "$root/scripts/claude-login.py"
