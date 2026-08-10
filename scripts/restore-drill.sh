#!/usr/bin/env bash
set -eu
set -o pipefail
umask 077

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
lock=$root/deploy/images.lock.env
identity=$root/deploy/secrets/backup_age_identity
backup=${1:-}

test -n "$backup" || {
    printf '%s\n' 'usage: scripts/restore-drill.sh BACKUP.dump.age' >&2
    exit 2
}
test -f "$backup" && test ! -L "$backup" || {
    printf '%s\n' 'restore-drill: backup must be a regular file' >&2
    exit 1
}
test -f "$identity" && test ! -L "$identity" && test -r "$identity" || {
    printf '%s\n' 'restore-drill: backup age identity is unavailable' >&2
    exit 1
}
command -v age >/dev/null 2>&1 || {
    printf '%s\n' 'restore-drill: age is required' >&2
    exit 1
}

checksum=$backup.sha256
if test -e "$checksum" || test -L "$checksum"; then
    test -f "$checksum" && test ! -L "$checksum" || {
        printf '%s\n' 'restore-drill: checksum must be a regular file' >&2
        exit 1
    }
    (cd "$(dirname -- "$backup")" && sha256sum -c "$(basename -- "$checksum")")
fi

test -f "$lock" && test ! -L "$lock" && test -r "$lock" || {
    printf '%s\n' 'restore-drill: image lock file is unavailable' >&2
    exit 1
}
postgres_image=$(sed -n 's/^POSTGRES_IMAGE=//p' "$lock")
case "$postgres_image" in
    *@sha256:????????????????????????????????????????????????????????????????) ;;
    *)
        printf '%s\n' 'restore-drill: POSTGRES_IMAGE is not digest locked' >&2
        exit 1
        ;;
esac

work=$(mktemp -d /tmp/codex-gateway-restore.XXXXXX)
container_id=
data_volume=

cleanup() {
    status=$?
    cleanup_failed=0
    trap - EXIT HUP INT TERM

    if test -n "$container_id" && docker inspect "$container_id" >/dev/null 2>&1; then
        docker stop -t 10 "$container_id" >/dev/null 2>&1 || true
        if ! docker rm -f "$container_id" >/dev/null 2>&1; then
            printf '%s\n' "restore-drill: failed to remove temporary container $container_id" >&2
            cleanup_failed=1
        fi
    fi
    if test -n "$data_volume"; then
        if ! docker volume rm "$data_volume" >/dev/null 2>&1; then
            printf '%s\n' "restore-drill: failed to remove decrypted data volume $data_volume" >&2
            cleanup_failed=1
        fi
    fi
    case "$work" in
        /tmp/codex-gateway-restore.*)
            if ! rm -rf -- "$work"; then
                printf '%s\n' "restore-drill: failed to remove temporary secret directory $work" >&2
                cleanup_failed=1
            fi
            ;;
    esac
    if test "$cleanup_failed" -ne 0 && test "$status" -eq 0; then
        status=1
    fi
    exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "$work/secrets"
chmod 0700 "$work/secrets"
openssl rand -hex 32 > "$work/secrets/postgres_password"
chmod 0600 "$work/secrets/postgres_password"

data_volume=$(docker volume create \
    --label com.codex-gateway.purpose=restore-drill)

container_id=$(docker run --detach \
    --network none \
    --security-opt no-new-privileges:true \
    --label com.codex-gateway.purpose=restore-drill \
    --env POSTGRES_DB=restore_drill \
    --env POSTGRES_USER=drill \
    --env POSTGRES_PASSWORD_FILE=/run/secrets/postgres_password \
    --mount "type=volume,src=$data_volume,dst=/var/lib/postgresql/data" \
    --mount "type=bind,src=$work/secrets/postgres_password,dst=/run/secrets/postgres_password,readonly" \
    "$postgres_image")

attempt=0
until docker exec "$container_id" sh -eu -c '
    PGPASSWORD=$(cat /run/secrets/postgres_password)
    export PGPASSWORD
    exec psql --host=127.0.0.1 --username=drill --dbname=restore_drill \
        --no-psqlrc --tuples-only --command="select 1" >/dev/null
'; do
    attempt=$((attempt + 1))
    test "$attempt" -lt 60 || {
        printf '%s\n' 'restore-drill: temporary PostgreSQL did not become ready' >&2
        exit 1
    }
    sleep 1
done

age --decrypt --identity "$identity" "$backup" | \
    docker exec -i "$container_id" sh -eu -c '
        PGPASSWORD=$(cat /run/secrets/postgres_password)
        export PGPASSWORD
        exec pg_restore --host=127.0.0.1 --username=drill \
            --dbname=restore_drill --exit-on-error --no-owner --no-privileges
    '

table_count=$(docker exec "$container_id" sh -eu -c '
    PGPASSWORD=$(cat /run/secrets/postgres_password)
    export PGPASSWORD
    exec psql --host=127.0.0.1 --username=drill --dbname=restore_drill \
        --tuples-only --no-align \
        --command="select count(*) from pg_catalog.pg_tables where schemaname not in ('\''pg_catalog'\'', '\''information_schema'\'');"
')
case "$table_count" in
    ''|*[!0-9]*)
        printf '%s\n' 'restore-drill: could not validate restored table count' >&2
        exit 1
        ;;
esac
test "$table_count" -gt 0 || {
    printf '%s\n' 'restore-drill: restored database contains no application tables' >&2
    exit 1
}

printf '%s\n' "restore drill passed ($table_count application tables)"
printf '%s\n' 'temporary database and decrypted data will now be removed'
