#!/usr/bin/env bash
set -eu
set -o pipefail
umask 077

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose=$root/scripts/compose.sh
recipient_file=$root/deploy/secrets/backup_age_recipient
backup_dir=${BACKUP_DIR:-$root/backups}

command -v age >/dev/null 2>&1 || {
    printf '%s\n' 'backup-postgres: age is required' >&2
    exit 1
}
command -v flock >/dev/null 2>&1 || {
    printf '%s\n' 'backup-postgres: flock from util-linux is required' >&2
    exit 1
}
test -f "$recipient_file" && test ! -L "$recipient_file" && test -r "$recipient_file" || {
    printf '%s\n' 'backup-postgres: run scripts/init-backup-key.sh first' >&2
    exit 1
}

recipient=$(tr -d '\r\n' < "$recipient_file")
case "$recipient" in
    age1*) ;;
    *)
        printf '%s\n' 'backup-postgres: invalid age recipient file' >&2
        exit 1
        ;;
esac

if test -e "$backup_dir" || test -L "$backup_dir"; then
    test -d "$backup_dir" && test ! -L "$backup_dir" || {
        printf '%s\n' 'backup-postgres: backup directory must not be a symlink' >&2
        exit 1
    }
else
    mkdir -p "$backup_dir"
fi
chmod 0700 "$backup_dir"
backup_dir=$(CDPATH= cd -- "$backup_dir" && pwd)

lock_file=$backup_dir/.backup.lock
if test -e "$lock_file" || test -L "$lock_file"; then
    test -f "$lock_file" && test ! -L "$lock_file" || {
        printf '%s\n' 'backup-postgres: lock path must be a regular file' >&2
        exit 1
    }
fi
exec 9>"$lock_file"
chmod 0600 "$lock_file"
flock -n 9 || {
    printf '%s\n' 'backup-postgres: another backup is already running' >&2
    exit 1
}

timestamp=$(date -u +%Y%m%dT%H%M%SZ)
output_name=gateway-${timestamp}.dump.age
output=$backup_dir/$output_name
checksum=$output.sha256
if test -e "$output" || test -L "$output" || \
   test -e "$checksum" || test -L "$checksum"; then
    printf '%s\n' "backup-postgres: refusing to overwrite backup timestamp $timestamp" >&2
    exit 1
fi
tmp=$(mktemp "$backup_dir/.${output_name}.tmp.XXXXXX")
checksum_tmp=$(mktemp "$backup_dir/.${output_name}.sha256.tmp.XXXXXX")
cleanup() {
    test -z "${tmp:-}" || rm -f -- "$tmp"
    test -z "${checksum_tmp:-}" || rm -f -- "$checksum_tmp"
}
trap cleanup EXIT HUP INT TERM

"$compose" \
    exec -T postgres sh -eu -c '
        PGPASSWORD=$(cat /run/secrets/postgres_password)
        export PGPASSWORD
        exec pg_dump --host=127.0.0.1 --username=gateway --dbname=gateway \
            --format=custom --compress=9 --no-owner --no-privileges
    ' | age --encrypt --recipient "$recipient" --output "$tmp"

chmod 0600 "$tmp"
digest=$(sha256sum "$tmp")
digest=${digest%% *}
case "$digest" in
    ''|*[!0-9a-f]*)
        printf '%s\n' 'backup-postgres: could not calculate SHA-256 digest' >&2
        exit 1
        ;;
esac
test "${#digest}" -eq 64 || {
    printf '%s\n' 'backup-postgres: could not calculate SHA-256 digest' >&2
    exit 1
}
printf '%s  %s\n' "$digest" "$output_name" > "$checksum_tmp"
chmod 0600 "$checksum_tmp"
mv "$tmp" "$output"
tmp=
mv "$checksum_tmp" "$checksum"
checksum_tmp=
trap - EXIT HUP INT TERM

printf '%s\n' "encrypted PostgreSQL backup written to $output"
printf '%s\n' 'OAuth data was not included'
