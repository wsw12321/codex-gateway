#!/bin/sh
set -eu
umask 077

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
secret_dir=$root/deploy/secrets

fail() {
    printf '%s\n' "bootstrap-secrets: $*" >&2
    exit 1
}

if test -e "$secret_dir" || test -L "$secret_dir"; then
    test -d "$secret_dir" && test ! -L "$secret_dir" || \
        fail "refusing non-directory or symlink path $secret_dir"
else
    mkdir -p "$secret_dir"
fi
test -d "$secret_dir" && test ! -L "$secret_dir" || \
    fail "refusing non-directory or symlink path $secret_dir"
chmod 0700 "$secret_dir"

require_command() {
    command -v "$1" >/dev/null 2>&1 || {
        printf '%s\n' "bootstrap-secrets: required command not found: $1" >&2
        exit 1
    }
}

create_random() {
    target=$1
    bytes=$2
    if test -e "$target" || test -L "$target"; then
        test -f "$target" && test ! -L "$target" || \
            fail "refusing non-regular or symlink path $target"
        chmod 0640 "$target"
        return
    fi

    target_name=${target##*/}
    tmp=$(mktemp "$secret_dir/.${target_name}.tmp.XXXXXX")
    trap 'test -z "${tmp:-}" || rm -f "$tmp"' EXIT HUP INT TERM
    openssl rand -hex "$bytes" > "$tmp"
    chmod 0640 "$tmp"
    mv -f "$tmp" "$target"
    tmp=
    trap - EXIT HUP INT TERM
}

require_command openssl
create_random "$secret_dir/postgres_password" 32
create_random "$secret_dir/gateway_api_key_pepper" 32
create_random "$secret_dir/gateway_session_secret" 32
create_random "$secret_dir/sidecar_api_key" 32

postgres_password=$(tr -d '\r\n' < "$secret_dir/postgres_password")
case "$postgres_password" in
    *[!A-Za-z0-9_-]*|'')
        printf '%s\n' 'bootstrap-secrets: postgres_password must be URL-safe' >&2
        exit 1
        ;;
esac

database_url=$secret_dir/database_url
if test -e "$database_url" || test -L "$database_url"; then
    test -f "$database_url" && test ! -L "$database_url" || \
        fail "refusing non-regular or symlink path $database_url"
fi

database_url_tmp=$(mktemp "$secret_dir/.database_url.tmp.XXXXXX")
trap 'rm -f "$database_url_tmp"' EXIT HUP INT TERM
printf 'postgres://gateway:%s@postgres:5432/gateway?sslmode=disable\n' \
    "$postgres_password" > "$database_url_tmp"
chmod 0640 "$database_url_tmp"
mv -f "$database_url_tmp" "$database_url"
database_url_tmp=
trap - EXIT HUP INT TERM
unset postgres_password

for secret_name in \
    postgres_password \
    gateway_api_key_pepper \
    gateway_session_secret \
    sidecar_api_key \
    database_url
do
    secret=$secret_dir/$secret_name
    test -f "$secret" && test ! -L "$secret" || \
        fail "refusing non-regular or symlink path $secret"
    chmod 0640 "$secret"
done

printf '%s\n' 'gateway secrets are present with mode 0640; values were not displayed'
printf '%s\n' "set GATEWAY_SECRET_GID=$(id -g) in .env before starting non-root services"
