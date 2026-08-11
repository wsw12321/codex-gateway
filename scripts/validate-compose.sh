#!/bin/sh
set -eu
umask 077

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
lock=$root/deploy/images.lock.env
env_file=$root/.env
compose=$root/scripts/compose.sh
caddyfile=$root/deploy/Caddyfile
secret_dir=$root/deploy/secrets
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT HUP INT TERM

fail() {
    printf '%s\n' "validate-compose: $*" >&2
    exit 1
}

command -v jq >/dev/null 2>&1 || {
    fail 'jq is required'
}
test -r "$lock" || {
    fail 'run scripts/lock-images.sh first'
}
test -r "$env_file" || {
    fail 'copy deploy/env.example to .env and configure it first'
}
test -d "$secret_dir" && test ! -L "$secret_dir" || {
    fail "secret directory must be a real directory: $secret_dir"
}

"$compose" config --format json > "$tmp"

gateway_domain=$(jq -r '.services.caddy.environment.GATEWAY_DOMAIN // ""' "$tmp")
case "$gateway_domain" in
    ''|codex.example.com|replace-*)
        fail 'GATEWAY_DOMAIN must be the real Cloudflare-managed hostname'
        ;;
esac

pricing_json=$(jq -er '
  .services.gateway.environment.GATEWAY_USAGE_PRICING_JSON |
  select(type == "string" and length > 0)
' "$tmp") || fail 'GATEWAY_USAGE_PRICING_JSON must be set in .env'
printf '%s\n' "$pricing_json" | jq -e '
  def decimal:
    type == "string" and
    length > 0 and length <= 40 and
    test("^(0|[1-9][0-9]*)(\\.[0-9]+)?$");
  def bounded_decimal:
    decimal and
    test("^(?:[0-9]{1,9}(?:\\.[0-9]+)?|1000000000(?:\\.0+)?)$");
  def snapshot_date:
    type == "string" and
    test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$") and
    ((try strptime("%Y-%m-%d") catch null) != null);
  (keys | sort) == ["catalog_as_of", "fx_as_of", "models", "usd_cny_rate"] and
  (.catalog_as_of | snapshot_date) and
  (.fx_as_of | snapshot_date) and
  (.usd_cny_rate | bounded_decimal and (test("^0(?:\\.0+)?$") | not)) and
  (.models | type == "object" and length > 0 and length <= 1000) and
  all(.models | to_entries[];
    (.key |
      length >= 1 and length <= 128 and
      . != "exact-recorded-model" and
      (startswith("replace-") | not) and
      test("^[A-Za-z0-9._:-]+$")) and
    (.value | type == "object") and
    (.value | keys | sort) == [
      "cached_input_usd_per_million",
      "input_usd_per_million",
      "output_usd_per_million"
    ] and
    (.value.input_usd_per_million | bounded_decimal) and
    (.value.cached_input_usd_per_million | bounded_decimal) and
    (.value.output_usd_per_million | bounded_decimal)
  )
' >/dev/null 2>&1 || \
    fail 'GATEWAY_USAGE_PRICING_JSON must contain reviewed dates, FX rate, exact models, and decimal prices'
unset pricing_json

# Cloudflare Tunnel is the only public ingress, so no service may publish a
# host port. Only cloudflared and the Squid allowlist proxy may reach an
# external Docker network, each through its dedicated egress network.
jq -e '
  all(.services[]; ((.ports // []) | length) == 0)
' "$tmp" >/dev/null

jq -e '
  .networks.edge_internal.internal == true and
  .networks.data_internal.internal == true and
  .networks.compat_internal.internal == true and
  ((.networks.tunnel_external.internal // false) == false) and
  ((.networks.egress_external.internal // false) == false) and
  ([.networks | to_entries[] | select((.value.internal // false) == false) | .key] | sort) ==
    ["egress_external", "tunnel_external"] and
  (.services.cloudflared.networks | keys | sort) == ["edge_internal", "tunnel_external"] and
  (.services.caddy.networks | keys) == ["edge_internal"] and
  ([.services | to_entries[] | select(.value.networks.tunnel_external != null) | .key]) ==
    ["cloudflared"] and
  ([.services | to_entries[] | select(.value.networks.egress_external != null) | .key]) ==
    ["egress-allowlist"] and
  .services.cloudflared.networks.edge_internal.ipv4_address == "172.28.10.4" and
  .services.caddy.networks.edge_internal.ipv4_address == "172.28.10.2" and
  .services.gateway.environment.TRUSTED_PROXY_CIDRS ==
    (.services.caddy.networks.edge_internal.ipv4_address + "/32")
' "$tmp" >/dev/null

jq -e '
  .services.cloudflared.read_only == true and
  (.services.cloudflared.user | test("^65532:[1-9][0-9]*$")) and
  (.services.cloudflared.cap_drop | sort) == ["ALL"] and
  .services.cloudflared.command == [
    "tunnel",
    "--metrics",
    "127.0.0.1:2000",
    "run",
    "--token-file",
    "/run/secrets/cloudflared_tunnel_token"
  ] and
  ([.services.cloudflared.secrets[] | .source]) == ["cloudflared_tunnel_token"] and
  ([.services | to_entries[] |
    select(any(.value.secrets[]?; .source == "cloudflared_tunnel_token")) |
    .key]) == ["cloudflared"]
' "$tmp" >/dev/null

lock_value() {
    sed -n "s/^$1=//p" "$lock"
}
caddy_image=$(lock_value CADDY_IMAGE)
cloudflared_image=$(lock_value CLOUDFLARED_IMAGE)
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
    --arg cloudflared "$cloudflared_image" \
    --arg postgres "$postgres_image" \
    --arg golang "$golang_image" \
    --arg cliproxy_golang "$cliproxy_golang_image" \
    --arg runtime "$runtime_image" \
    --arg squid "$squid_image" '
  .services.caddy.image == $caddy and
  .services.cloudflared.image == $cloudflared and
  .services.postgres.image == $postgres and
  .services["egress-allowlist"].image == $squid and
  .services.gateway.build.args.GOLANG_IMAGE == $golang and
  .services.gateway.build.args.RUNTIME_IMAGE == $runtime and
  .services["codex-compat"].build.args.GOLANG_IMAGE == $cliproxy_golang and
  .services["codex-compat"].build.args.RUNTIME_IMAGE == $runtime
' "$tmp" >/dev/null

gateway_image=$(jq -r '.services.gateway.image' "$tmp")
gateway_version=$(jq -r '.services.gateway.build.args.VERSION' "$tmp")
gateway_revision=$(jq -r '.services.gateway.build.args.REVISION' "$tmp")
gateway_tag=${gateway_image#codex-gateway-gateway:}
test "$gateway_image" != "$gateway_tag" || \
    fail 'gateway image must use the codex-gateway-gateway:<tag> name'
case "$gateway_tag" in
    ''|local|dev|latest|unknown|replace-*)
        fail 'GATEWAY_IMAGE_TAG must be an immutable release version or Git revision'
        ;;
esac
case "$gateway_version" in
    ''|local|dev|latest|unknown|replace-*)
        fail 'GATEWAY_VERSION must identify the reviewed release'
        ;;
esac
printf '%s\n' "$gateway_revision" | grep -Eq '^[0-9a-f]{40}$' || \
    fail 'GATEWAY_REVISION must be a full lowercase 40-character Git revision'
if test "$gateway_tag" != "$gateway_version" && \
    test "$gateway_tag" != "$gateway_revision"; then
    fail 'GATEWAY_IMAGE_TAG must equal GATEWAY_VERSION or GATEWAY_REVISION'
fi

jq -e '
  .services["codex-compat"].read_only == true and
  .services.gateway.read_only == true and
  (.services["codex-compat"].user | test("^10001:[1-9][0-9]*$")) and
  (.services.gateway.user | test("^10001:[1-9][0-9]*$")) and
  ((.services["codex-compat"].ports // []) | length == 0) and
  ((.services.postgres.ports // []) | length == 0) and
  ((.services.gateway.ports // []) | length == 0)
' "$tmp" >/dev/null

secret_gid=$(jq -r '.services.gateway.user | split(":")[1]' "$tmp")
test "$(jq -r '.services.cloudflared.user | split(":")[1]' "$tmp")" = "$secret_gid" || \
    fail 'cloudflared and gateway must use the same configured secret GID'
for secret_name in \
    cloudflared_tunnel_token \
    postgres_password \
    gateway_api_key_pepper \
    gateway_session_secret \
    sidecar_api_key \
    database_url
do
    secret=$secret_dir/$secret_name
    test -f "$secret" && test ! -L "$secret" || \
        fail "secret must be a regular non-symlink file: $secret"
    test -s "$secret" || fail "secret must not be empty: $secret"
    test "$(stat -c '%a' "$secret")" = 640 || \
        fail "secret must have mode 0640: $secret"
    test "$(stat -c '%g' "$secret")" = "$secret_gid" || \
        fail "secret group must match GATEWAY_SECRET_GID=$secret_gid: $secret"
done
unset gateway_domain secret secret_gid secret_name

# The official PostgreSQL entrypoint connects over the local socket as the
# configured database user while creating POSTGRES_DB. Peer authentication
# compares that user with the container's OS user and blocks first startup, so
# both local initialization and later host connections must use SCRAM.
jq -e '
  .services.postgres.environment.POSTGRES_INITDB_ARGS ==
    "--auth-host=scram-sha-256 --auth-local=scram-sha-256"
' "$tmp" >/dev/null

if sed -n 's/^[A-Z0-9_]*=//p' "$lock" | grep -Ev '@sha256:[0-9a-f]{64}$' >/dev/null; then
    fail 'an image lock is not a sha256 digest'
fi

grep -Fq 'http://{$GATEWAY_DOMAIN}' "$caddyfile" || \
    fail 'Caddy must expose only the internal HTTP origin'
grep -Fq 'trusted_proxies static 172.28.10.4/32' "$caddyfile" || \
    fail 'Caddy must trust only the fixed cloudflared address'
grep -Fq 'client_ip_headers CF-Connecting-IP' "$caddyfile" || \
    fail 'Caddy must derive client IP only from CF-Connecting-IP'
grep -Fq 'header_up -Forwarded' "$caddyfile" || \
    fail 'Caddy must remove Forwarded before proxying'
grep -Fq 'header_up -X-Forwarded-For' "$caddyfile" || \
    fail 'Caddy must remove the incoming X-Forwarded-For value'
grep -Fq 'header_up X-Forwarded-For {http.request.client_ip}' "$caddyfile" || \
    fail 'Caddy must send only its validated client IP to Gateway'
if grep -Fq 'preload' "$caddyfile"; then
    fail 'Caddy must not opt the deployment domain into HSTS preload'
fi

# Adapt the Caddyfile with the exact digest-locked binary that will run in
# production; this catches invalid directives in addition to textual policy.
"$compose" run --rm --no-deps caddy caddy validate \
    --config /etc/caddy/Caddyfile --adapter caddyfile

printf '%s\n' 'Compose ingress, network isolation, secrets, immutable revisions, usage pricing, Caddy policy, PostgreSQL SCRAM auth, and rendered image locks validated'
