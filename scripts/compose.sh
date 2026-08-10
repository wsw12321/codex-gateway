#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
env_file=$root/.env
lock=$root/deploy/images.lock.env

test -r "$env_file" || {
    printf '%s\n' 'compose: copy deploy/env.example to .env and configure it first' >&2
    exit 1
}
test -r "$lock" || {
    printf '%s\n' 'compose: run scripts/lock-images.sh first' >&2
    exit 1
}

# Shell variables take precedence over every Compose --env-file. Remove all
# variables owned by either project env file so an inherited environment
# cannot silently replace site settings, a reviewed digest, or a builder
# image. Compose then reloads the files below in the documented order.
unset_file_variables() {
    source_file=$1
    while IFS='=' read -r name _value; do
        case "$name" in
            ''|'#'*) continue ;;
            [A-Z_]*)
                case "$name" in
                    *[!A-Z0-9_]*)
                        printf '%s\n' "compose: invalid variable name in $source_file" >&2
                        exit 1
                        ;;
                esac
                ;;
            *)
                printf '%s\n' "compose: invalid variable name in $source_file" >&2
                exit 1
                ;;
        esac
        unset "$name"
    done < "$source_file"
}

unset_file_variables "$env_file"
unset_file_variables "$lock"
unset name _value source_file

exec docker compose \
    --project-name codex-gateway \
    --project-directory "$root" \
    --env-file "$env_file" \
    --env-file "$lock" \
    -f "$root/docker-compose.yml" \
    "$@"
