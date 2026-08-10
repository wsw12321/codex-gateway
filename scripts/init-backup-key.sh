#!/bin/sh
set -eu
umask 077

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
secret_dir=$root/deploy/secrets
identity=$secret_dir/backup_age_identity
recipient=$secret_dir/backup_age_recipient

command -v age-keygen >/dev/null 2>&1 || {
    printf '%s\n' 'init-backup-key: age-keygen is required' >&2
    exit 1
}
mkdir -p "$secret_dir"
chmod 0700 "$secret_dir"

if test -e "$identity" || test -e "$recipient"; then
    printf '%s\n' 'init-backup-key: backup key files already exist; refusing to overwrite' >&2
    exit 1
fi

tmp_identity=${identity}.tmp.$$
tmp_recipient=${recipient}.tmp.$$
trap 'rm -f "$tmp_identity" "$tmp_recipient"' EXIT HUP INT TERM
age-keygen -o "$tmp_identity" 2>/dev/null
age-keygen -y "$tmp_identity" > "$tmp_recipient"
chmod 0600 "$tmp_identity" "$tmp_recipient"
mv -f "$tmp_identity" "$identity"
mv -f "$tmp_recipient" "$recipient"
trap - EXIT HUP INT TERM

printf '%s\n' 'backup age identity and recipient created; values were not displayed'

