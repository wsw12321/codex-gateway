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
        # Reject the vulnerable perl-base inherited from the pinned base image.
        perl_base_version=$(dpkg-query -W -f="\${Version}" perl-base)
        if ! dpkg --compare-versions "$perl_base_version" ge "5.36.0-7+deb12u4"; then
            printf "%s\n" "perl-base lacks DLA-4821-1 security fixes" >&2
            exit 1
        fi
        umask 077
        export CLIPROXY_API_KEY_FILE=/run/cliproxy/synthetic-key
        printf "%s\n" AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA > "$CLIPROXY_API_KEY_FILE"

        export CPA_MANAGEMENT_KEY_FILE=/run/cliproxy/synthetic-management-key
        printf "%s\n" BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB > "$CPA_MANAGEMENT_KEY_FILE"

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
        grep -Fq "Version: v8.0.4" /run/cliproxy/startup.log
        grep -Fq "Commit: d33f63f8e3d98428440ebca5a5b6a981a61ff71e" /run/cliproxy/startup.log
        # No Gateway exists on this network: health must not wait for allocation.
        grep -Fq "strategy: \"gateway-allocation\"" /run/cliproxy/config.yaml
        grep -A1 "^discovery:" /run/cliproxy/config.yaml | grep -Eq "enabled:[[:space:]]*false"
        grep -A1 "^codex:" /run/cliproxy/config.yaml | grep -Eq "response-steering:[[:space:]]*false"
        {
            printf "GET /internal/upstream-accounts/capabilities HTTP/1.1\r\nHost: 127.0.0.1:8317\r\nAuthorization: Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\r\nConnection: close\r\n\r\n"
        } | nc -w 3 127.0.0.1 8317 > /run/cliproxy/capabilities
        grep -Fq "upstream_account_access_v1" /run/cliproxy/capabilities
        {
            printf "GET /internal/model-identification/capabilities HTTP/1.1\r\nHost: 127.0.0.1:8317\r\nAuthorization: Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\r\nConnection: close\r\n\r\n"
        } | nc -w 3 127.0.0.1 8317 > /run/cliproxy/diagnostic-capabilities
        grep -Fq "200 OK" /run/cliproxy/diagnostic-capabilities
        grep -Fq "model_identification_direct_v1" /run/cliproxy/diagnostic-capabilities
        {
            printf "POST /internal/model-identification/accounts/0123456789abcdef/probe HTTP/1.1\r\nHost: 127.0.0.1:8317\r\nAuthorization: Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
        } | nc -w 3 127.0.0.1 8317 > /run/cliproxy/diagnostic-actor-required
        grep -Fq "400 Bad Request" /run/cliproxy/diagnostic-actor-required
        grep -Fq "probe_actor_invalid" /run/cliproxy/diagnostic-actor-required
        {
            printf "GET /internal/model-identification/capabilities HTTP/1.1\r\nHost: 127.0.0.1:8317\r\nConnection: close\r\n\r\n"
        } | nc -w 3 127.0.0.1 8317 > /run/cliproxy/diagnostic-unauthorized
        grep -Fq "401 Unauthorized" /run/cliproxy/diagnostic-unauthorized
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
        {
            printf "GET /internal/antigravity-accounts/capabilities HTTP/1.1\r\nHost: 127.0.0.1:8317\r\nAuthorization: Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\r\nConnection: close\r\n\r\n"
        } | nc -w 3 127.0.0.1 8317 > /run/cliproxy/antigravity-capabilities
        grep -Fq "200 OK" /run/cliproxy/antigravity-capabilities
        grep -Fq "upstream_account_access_v1" /run/cliproxy/antigravity-capabilities
        {
            printf "POST /internal/gateway-management/antigravity/credentials HTTP/1.1\r\nHost: 127.0.0.1:8317\r\nAuthorization: Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}"
        } | nc -w 3 127.0.0.1 8317 > /run/cliproxy/management-wrong-key
        grep -Fq "401 Unauthorized" /run/cliproxy/management-wrong-key
        {
            printf "POST /internal/gateway-management/antigravity/credentials HTTP/1.1\r\nHost: 127.0.0.1:8317\r\nAuthorization: Bearer BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}"
        } | nc -w 3 127.0.0.1 8317 > /run/cliproxy/management-invalid-import
        grep -Fq "400 Bad Request" /run/cliproxy/management-invalid-import
        if /usr/local/bin/sidecar-entrypoint --version > /run/cliproxy/second-process.log 2>&1; then
            printf "%s\n" "A second OAuth refresh process acquired the credential store" >&2
            exit 1
        fi
        grep -Fq "OAuth store is owned by another refresh process" /run/cliproxy/second-process.log
        printf "%s\n" "CPA OAuth permissions, both provider capabilities, diagnostics, concurrency, separate management authentication, exclusive refresh locking and Gateway-independent startup checks passed"
    '
