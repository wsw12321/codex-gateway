#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
image=${1:-}
if test -z "$image"; then
    image=$("$root/scripts/compose.sh" config --format json | jq -er '.services["codex-compat"].image')
fi

# Synthetic credentials and private tmpfs only: no site secrets, OAuth volume,
# external network or published ports are used by these runtime regressions.
docker run --rm --network none --read-only --cap-drop ALL \
    --security-opt no-new-privileges:true \
    --tmpfs /var/lib/cliproxy/oauth:rw,noexec,nosuid,nodev,mode=0700,uid=10001,gid=10001 \
    --tmpfs /run/cliproxy:rw,noexec,nosuid,nodev,mode=0700,uid=10001,gid=10001 \
    --tmpfs /tmp:rw,noexec,nosuid,nodev,mode=0700,uid=10001,gid=10001 \
    --entrypoint /bin/sh "$image" -eu -c '
        umask 077
        export CLIPROXY_API_KEY_FILE=/run/cliproxy/synthetic-key
        printf "%s\n" AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA > "$CLIPROXY_API_KEY_FILE"

        auth=/var/lib/cliproxy/oauth
        printf "%s\n" "{}" > "$auth/imported-codex.json"
        printf "%s\n" "{}" > "$auth/imported-gemini.JSON"
        /usr/local/bin/sidecar-entrypoint oauth-inventory > /run/cliproxy/inventory
        awk "NF != 2 || length(\$1) != 64 || length(\$2) != 64 { bad = 1 } END { exit (bad || NR != 2) }" /run/cliproxy/inventory

        chmod 0644 "$auth/imported-gemini.JSON"
        if /usr/local/bin/sidecar-entrypoint verify-oauth >/dev/null 2>&1; then
            printf "%s\n" "OAuth verification accepted an unsafe file mode" >&2
            exit 1
        fi
        chmod 0600 "$auth/imported-gemini.JSON"
        ln -s /tmp "$auth/unsafe-link"
        if /usr/local/bin/sidecar-entrypoint verify-oauth >/dev/null 2>&1; then
            printf "%s\n" "OAuth verification accepted a symlink" >&2
            exit 1
        fi
        rm "$auth/unsafe-link" "$auth/imported-codex.json" "$auth/imported-gemini.JSON"
        /usr/local/bin/sidecar-entrypoint verify-oauth

        /usr/local/bin/sidecar-entrypoint > /run/cliproxy/startup.log 2>&1 &
        sidecar_pid=$!
        trap "kill $sidecar_pid 2>/dev/null || true; wait $sidecar_pid 2>/dev/null || true" EXIT HUP INT TERM
        attempt=0
        until /usr/local/bin/sidecar-healthcheck >/dev/null 2>&1; do
            kill -0 "$sidecar_pid"
            attempt=$((attempt + 1))
            test "$attempt" -lt 30
            sleep 1
        done
        # No Gateway exists on this network: health must not wait for allocation.
        grep -Fq "strategy: \"gateway-allocation\"" /run/cliproxy/config.yaml
        {
            printf "GET /internal/upstream-accounts/capabilities HTTP/1.1\r\nHost: 127.0.0.1:8317\r\nAuthorization: Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\r\nConnection: close\r\n\r\n"
        } | nc -w 3 127.0.0.1 8317 > /run/cliproxy/capabilities
        grep -Fq "upstream_account_access_v1" /run/cliproxy/capabilities
        {
            printf "GET /internal/upstream-accounts/concurrency HTTP/1.1\r\nHost: 127.0.0.1:8317\r\nAuthorization: Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\r\nConnection: close\r\n\r\n"
        } | nc -w 3 127.0.0.1 8317 > /run/cliproxy/concurrency
        grep -Fq "200 OK" /run/cliproxy/concurrency
        grep -Fq "\"sampled_at\"" /run/cliproxy/concurrency
        grep -Fq "\"accounts\":[]" /run/cliproxy/concurrency
        {
            printf "GET /internal/upstream-accounts/concurrency HTTP/1.1\r\nHost: 127.0.0.1:8317\r\nConnection: close\r\n\r\n"
        } | nc -w 3 127.0.0.1 8317 > /run/cliproxy/concurrency-unauthorized
        grep -Fq "401 Unauthorized" /run/cliproxy/concurrency-unauthorized
        printf "%s\n" "Sidecar OAuth inventory, permission rejection, account access capability, authenticated concurrency and Gateway-independent startup checks passed"
    '
