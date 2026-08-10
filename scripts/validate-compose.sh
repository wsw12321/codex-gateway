#!/bin/sh
set -eu
umask 077

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
lock=$root/deploy/images.lock.env
env_file=$root/.env
compose=$root/scripts/compose.sh
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT HUP INT TERM

command -v jq >/dev/null 2>&1 || {
    printf '%s\n' 'validate-compose: jq is required' >&2
    exit 1
}
test -r "$lock" || {
    printf '%s\n' 'validate-compose: run scripts/lock-images.sh first' >&2
    exit 1
}
test -r "$env_file" || {
    printf '%s\n' 'validate-compose: copy deploy/env.example to .env and configure it first' >&2
    exit 1
}

"$compose" config --format json > "$tmp"

# Only Caddy may publish host ports, and it must publish exactly TCP 80 and
# TCP/UDP 443. All other service connectivity is Docker-internal.
jq -e '
  [.services | to_entries[] | select(.key != "caddy") | .value.ports // []] | add | length == 0
' "$tmp" >/dev/null
jq -e '
  [.services.caddy.ports[] | {
    target,
    published: (.published | tostring),
    protocol,
    mode,
    host_ip: (.host_ip // "")
  }] | sort_by(.target, .protocol) == [
    {target: 80, published: "80", protocol: "tcp", mode: "host", host_ip: ""},
    {target: 443, published: "443", protocol: "tcp", mode: "host", host_ip: ""},
    {target: 443, published: "443", protocol: "udp", mode: "host", host_ip: ""}
  ]
' "$tmp" >/dev/null

jq -e '
  .networks.edge_internal.internal == true and
  .networks.data_internal.internal == true and
  .networks.compat_internal.internal == true and
  .services.gateway.environment.TRUSTED_PROXY_CIDRS ==
    (.services.caddy.networks.edge_internal.ipv4_address + "/32")
' "$tmp" >/dev/null

lock_value() {
    sed -n "s/^$1=//p" "$lock"
}
caddy_image=$(lock_value CADDY_IMAGE)
postgres_image=$(lock_value POSTGRES_IMAGE)
golang_image=$(lock_value GOLANG_IMAGE)
cliproxy_golang_image=$(lock_value CLIPROXY_GOLANG_IMAGE)
runtime_image=$(lock_value RUNTIME_IMAGE)
squid_image=$(lock_value SQUID_IMAGE)

# Validate the rendered configuration, not only the lock file. This catches
# Compose precedence regressions or inherited shell variables that would
# otherwise replace a reviewed digest after the lock file itself was checked.
jq -e \
    --arg caddy "$caddy_image" \
    --arg postgres "$postgres_image" \
    --arg golang "$golang_image" \
    --arg cliproxy_golang "$cliproxy_golang_image" \
    --arg runtime "$runtime_image" \
    --arg squid "$squid_image" '
  .services.caddy.image == $caddy and
  .services.postgres.image == $postgres and
  .services["egress-allowlist"].image == $squid and
  .services.gateway.build.args.GOLANG_IMAGE == $golang and
  .services.gateway.build.args.RUNTIME_IMAGE == $runtime and
  .services["codex-compat"].build.args.GOLANG_IMAGE == $cliproxy_golang and
  .services["codex-compat"].build.args.RUNTIME_IMAGE == $runtime
' "$tmp" >/dev/null

jq -e '
  .services["codex-compat"].read_only == true and
  .services.gateway.read_only == true and
  (.services["codex-compat"].user | test("^10001:[1-9][0-9]*$")) and
  (.services.gateway.user | test("^10001:[1-9][0-9]*$")) and
  ((.services["codex-compat"].ports // []) | length == 0) and
  ((.services.postgres.ports // []) | length == 0) and
  ((.services.gateway.ports // []) | length == 0)
' "$tmp" >/dev/null

# The official PostgreSQL entrypoint connects over the local socket as the
# configured database user while creating POSTGRES_DB. Peer authentication
# compares that user with the container's OS user and blocks first startup, so
# both local initialization and later host connections must use SCRAM.
jq -e '
  .services.postgres.environment.POSTGRES_INITDB_ARGS ==
    "--auth-host=scram-sha-256 --auth-local=scram-sha-256"
' "$tmp" >/dev/null

if sed -n 's/^[A-Z0-9_]*=//p' "$lock" | grep -Ev '@sha256:[0-9a-f]{64}$' >/dev/null; then
    printf '%s\n' 'validate-compose: an image lock is not a sha256 digest' >&2
    exit 1
fi

printf '%s\n' 'Compose ports, isolation, PostgreSQL SCRAM auth, non-root sidecar, and rendered image locks validated'
