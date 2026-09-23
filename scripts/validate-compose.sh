#!/bin/sh
set -eu
umask 077

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
lock=$root/deploy/images.lock.env
env_file=$root/.env
compose=$root/scripts/compose.sh
caddyfile=$root/deploy/Caddyfile
secret_dir=$root/deploy/secrets
pricing_template=$root/deploy/pricing-v2.example.json
egress_config=$root/deploy/egress/squid.conf
egress_entrypoint=$root/deploy/egress/entrypoint.sh
relay_config=$root/deploy/relay/squid.conf
relay_compose=$root/deploy/relay/docker-compose.yml
relay_service=$root/deploy/relay/wg-codex
compat_dockerfile=$root/deploy/codex-compat/Dockerfile
compat_entrypoint=$root/deploy/codex-compat/entrypoint.sh
compat_patch=$root/deploy/codex-compat/cliproxy-v7.3.12-multi-account.patch
compat_patch_sha256=40cc02a0f66b5d28468db7ebc452a31950a2b222bbf40fb8e9ba04b986578134
bridge_dockerfile=$root/deploy/antigravity-bridge/Dockerfile
bridge_entrypoint=$root/deploy/antigravity-bridge/entrypoint.sh
agy_lock=$root/deploy/antigravity-bridge/agy.lock.json
compat_image=codex-gateway-compat:v7.3.12-2eb8dd11-40cc02a0f66b5d28-codex-only
tmp=$(mktemp)
relay_tmp=$(mktemp)
trap 'rm -f "$tmp" "$relay_tmp"' EXIT HUP INT TERM

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
test -s "$compat_patch" || fail 'reviewed CLIProxyAPI multi-account patch is missing'
test "$(sha256sum "$compat_patch" | awk '{print $1}')" = "$compat_patch_sha256" || \
    fail 'reviewed CLIProxyAPI multi-account patch checksum changed'
if grep -Eq 'gemini-cli|GEMINI_PLUGIN|geminicli-login|cliproxy-v7.2.150-gemini.patch' "$compat_dockerfile" "$compat_entrypoint"; then
    fail 'codex-compat must not build or load the retired Gemini plugin'
fi
grep -A1 '^plugins:' "$compat_entrypoint" | grep -Eq 'enabled:[[:space:]]*false' || \
    fail 'codex-compat must disable plugins and ignore legacy Gemini credentials'
grep -A1 '^discovery:' "$compat_entrypoint" | grep -Eq 'enabled:[[:space:]]*false' && \
    grep -A1 '^codex:' "$compat_entrypoint" | grep -Eq 'response-steering:[[:space:]]*false' || \
    fail 'codex-compat must disable LAN advertising and experimental websocket steering'
grep -Fq 'legacy-credentials.test.txt' "$compat_dockerfile" || \
    fail 'codex-compat must test preservation and rejection of legacy Gemini credentials'
jq -e '.version == "1.2.4" and .platform == "linux_amd64" and
    .url == "https://storage.googleapis.com/antigravity-public/antigravity-cli/1.2.4-6085322963025920/linux-x64/cli_linux_x64.tar.gz" and
    .sha512 == "5811d39ec1bf96a82ed06de6b8ee2bb7f5be8d74423b8c52b6b975e8f0e2c84c6cc2fa0baf902aad942c7566509c3ba6ddb5ef076260c6a635c4616e6ae17897"' \
    "$agy_lock" >/dev/null || fail 'agy version, official artifact and SHA512 must remain pinned'
grep -Fq 'sha512sum -c -' "$bridge_dockerfile" && \
    grep -Fq 'AGY_CLI_DISABLE_AUTO_UPDATE=true' "$bridge_dockerfile" && \
    grep -Fq 'dbus-run-session' "$bridge_entrypoint" && \
    grep -Fq 'gnome-keyring-daemon --unlock' "$bridge_entrypoint" || \
    fail 'Bridge requires checksum verification, disabled updates and a local Secret Service'
grep -Fxq 'http_access deny !CONNECT' "$egress_config" && \
    grep -Fxq 'http_access deny !TLS_port' "$egress_config" && \
    grep -Fxq 'http_access deny all' "$egress_config" && \
    grep -Fxq 'request_header_access Forwarded deny all' "$egress_config" || \
    fail 'egress must retain HTTPS CONNECT restrictions and exact reviewed provider domains'
test "$(awk '$1 == "acl" && ($2 == "codex_clients" || $2 == "antigravity_clients" || $2 == "codex_upstreams" || $2 == "antigravity_upstreams") { print }' "$egress_config")" = \
    "$(printf '%s\n' \
        'acl codex_clients src 172.28.30.3/32' \
        'acl antigravity_clients src 172.28.40.3/32' \
        'acl codex_upstreams dstdomain auth.openai.com chatgpt.com' \
        'acl antigravity_upstreams dstdomain accounts.google.com oauth2.googleapis.com www.googleapis.com cloudcode-pa.googleapis.com daily-cloudcode-pa.googleapis.com aicode.googleapis.com businessaicode.googleapis.com generativelanguage.googleapis.com lh3.googleusercontent.com antigravity-unleash.goog play.googleapis.com playwright.azureedge.net playwright-akamai.azureedge.net playwright-verizon.azureedge.net')" || \
    fail 'egress source and destination ACLs must equal the reviewed exact lists'
test "$(awk '$1 == "http_access" { print }' "$egress_config")" = \
    "$(printf '%s\n' 'http_access deny !CONNECT' 'http_access deny !TLS_port' \
        'http_access allow CONNECT codex_clients codex_upstreams' \
        'http_access allow CONNECT antigravity_clients antigravity_upstreams' 'http_access deny all')" || \
    fail 'egress must separate Codex and Antigravity destination rules'
# The generated fragment is the only place permitted to select an upstream or
# change direct routing. Check its output as well as parsing it with Squid below.
sh -n "$egress_entrypoint" && sh -n "$relay_service" || fail 'relay shell syntax is invalid'
test "$(awk '$1 == "include" || $1 ~ /^(cache_peer|cache_peer_access|cache_peer_domain|always_direct|never_direct|ssl_bump|https_port)$/ { print }' "$egress_config")" = \
    'include /run/codex-relay.conf' || fail 'egress must use only its generated relay routing fragment'
relay_rules=$(CODEX_RELAY_IP= CODEX_RELAY_PORT=3128 sh "$egress_entrypoint" --render-config) || \
    fail 'disabled relay fragment generation failed'
test -z "$(printf '%s\n' "$relay_rules" | awk 'NF && $1 !~ /^#/ { print }')" || \
    fail 'disabled relay must retain direct egress'
relay_rules=$(CODEX_RELAY_IP=10.77.0.2 CODEX_RELAY_PORT=3128 sh "$egress_entrypoint" --render-config) || \
    fail 'relay fragment generation failed'
test "$relay_rules" = "$(printf '%s\n' \
    'cache_peer 10.77.0.2 parent 3128 0 no-query default name=codex_relay' \
    'cache_peer_access codex_relay allow codex_clients' \
    'cache_peer_access codex_relay deny all' \
    'never_direct allow codex_clients' \
    'never_direct deny all')" || fail 'Codex must use a unique mandatory parent while Antigravity stays direct'
unset relay_rules
grep -Fxq 'logformat codex_destinations %ts.%03tu %ru %>Hs %Sh/%<a' "$egress_config" && \
    grep -Fxq 'access_log stdio:/var/log/squid/access.log codex_destinations CONNECT codex_clients' "$egress_config" || \
    fail 'Codex CONNECT logs must include destination, status and the selected forwarding path'
test "$(awk '$1 ~ /^(acl|http_port|https_port|http_access|include|cache_peer|cache_peer_access|always_direct|never_direct|ssl_bump)$/ { print }' "$relay_config")" = \
    "$(printf '%s\n' \
        'http_port 10.77.0.2:3128' \
        'acl CONNECT method CONNECT' \
        'acl TLS_port port 443' \
        'acl relay_clients src 10.77.0.1/32' \
        'acl codex_upstreams dstdomain -n auth.openai.com chatgpt.com' \
        'http_access deny !CONNECT' \
        'http_access deny !TLS_port' \
        'http_access allow CONNECT relay_clients codex_upstreams' \
        'http_access deny all')" || fail 'B must accept only A over WireGuard for the exact Codex HTTPS destinations'
test "$(awk '$1 == "cache_mem" { print }' "$relay_config")" = 'cache_mem 0 MB' || \
    fail 'B CONNECT-only relay must disable the object memory cache'
grep -Fxq '    need net' "$relay_service" && \
    grep -Fxq '    before docker' "$relay_service" && \
    grep -Fq '/usr/bin/wg-quick up wg-codex' "$relay_service" && \
    grep -Fq '/usr/bin/wg-quick down wg-codex' "$relay_service" || \
    fail 'B requires a WireGuard OpenRC service ordered before Docker'
grep -Fq 'git apply --check --ignore-space-change /tmp/cliproxy-multi-account.patch' "$compat_dockerfile" && \
    grep -Fq 'git apply --ignore-space-change /tmp/cliproxy-multi-account.patch' "$compat_dockerfile" || \
    fail 'codex-compat image must fail closed when the reviewed patch no longer applies'
grep -Fq 'go test -count=1' "$compat_dockerfile" || \
    fail 'codex-compat image must run the patch regression tests'
grep -Fq 'go test -list' "$compat_dockerfile" && grep -Fq 'grep -Fxq "$test_name"' "$compat_dockerfile" || \
    fail 'codex-compat image must fail closed when a named runtime regression test is missing'
grep -Fq 'TestCodexAstraUpgradeHTTPTransports' "$compat_dockerfile" && \
    grep -Fq 'TestAstraUpgradeModels' "$compat_patch" && \
    grep -Fq 'TestAstraUpgradeResponses' "$compat_patch" || \
    fail 'codex-compat must verify Astra models, Responses, SSE, and compact'
grep -Fq './internal/watcher' "$compat_dockerfile" && \
    grep -Fq './internal/auth/codex' "$compat_dockerfile" && \
    grep -Fq './sdk/auth' "$compat_dockerfile" || \
    fail 'codex-compat image must test OAuth redirects, stable-index, and duplicate-file reconciliation'
grep -Eq '^max-retry-credentials:[[:space:]]*2[[:space:]]*$' "$compat_entrypoint" || \
    fail 'codex-compat must cap each request at two credentials'
grep -Eq '^request-retry:[[:space:]]*0[[:space:]]*$' "$compat_entrypoint" && \
    grep -Eq '^max-retry-interval:[[:space:]]*0[[:space:]]*$' "$compat_entrypoint" || \
    fail 'codex-compat must not allocate handler-layer request retry attempts'
grep -Eq '^[[:space:]]+strategy:[[:space:]]*"gateway-allocation"[[:space:]]*$' "$compat_entrypoint" && \
    grep -Eq '^[[:space:]]+session-affinity:[[:space:]]*true[[:space:]]*$' "$compat_entrypoint" || \
    fail 'codex-compat must use Gateway account allocation with session affinity'
grep -Fq 'http://gateway:8080/internal/upstream-accounts/select' "$compat_patch" && \
    grep -Fq 'TestGatewayAllocationConcurrentFirstBinding' "$compat_patch" && \
    grep -Fq 'TestGatewayAllocationFailoverFinalAttribution' "$compat_patch" && \
    grep -Fq 'TestGatewayAllocationRejectsAlternateSchedulers' "$compat_patch" && \
    grep -Fq 'TestGatewayAllocationRechecksControlWithoutHoldingManagerLock' "$compat_patch" || \
    fail 'codex-compat must carry bounded Gateway allocation and concurrency regressions'
grep -Fq 'http://gateway:8080/internal/upstream-accounts/eligible' "$compat_patch" && \
    grep -Fq 'upstream_account_access_v1' "$compat_patch" && \
    grep -Fq 'TestGatewayAllocationAccessRechecksBindingsAndPreservesOtherSessions' "$compat_patch" && \
    grep -Fq 'TestGatewayUserIdentityConsumedAndValidated' "$compat_patch" || \
    fail 'codex-compat must enforce authenticated per-user account eligibility before binding reuse'
grep -Eq '^[[:space:]]+session-affinity-ttl:[[:space:]]*"1h"[[:space:]]*$' "$compat_entrypoint" || \
    fail 'codex-compat must retain session affinity for one hour'
grep -Eq '^[[:space:]]+bootstrap-retries:[[:space:]]*0[[:space:]]*$' "$compat_entrypoint" || \
    fail 'codex-compat must disable handler-layer stream bootstrap retries'
grep -Eq '^[[:space:]]+allow-remote:[[:space:]]*false[[:space:]]*$' "$compat_entrypoint" && \
    grep -Eq '^[[:space:]]+secret-key:[[:space:]]*""[[:space:]]*$' "$compat_entrypoint" && \
    grep -Eq '^[[:space:]]+disable-control-panel:[[:space:]]*true[[:space:]]*$' "$compat_entrypoint" && \
    grep -Eq '^[[:space:]]+disable-auto-update-panel:[[:space:]]*true[[:space:]]*$' "$compat_entrypoint" || \
    fail 'codex-compat full management API and control panels must remain disabled'
grep -Fq 'X-Codex-Gateway-Affinity' "$compat_patch" || \
    fail 'CLIProxyAPI patch must carry the reviewed caller-scope header contract'
grep -Fq 'X-Codex-Upstream-Account' "$compat_patch" || \
    fail 'CLIProxyAPI patch must carry the reviewed account attribution contract'
grep -Fq '/internal/upstream-accounts' "$compat_patch" || \
    fail 'CLIProxyAPI patch must carry the narrow internal account API'
grep -Fq 'internalAccounts.GET("/concurrency", s.getUpstreamConcurrency)' "$compat_patch" && \
    grep -Fq 'TestGatewayConcurrencyRepeatedReleaseAndRaces' "$compat_patch" && \
    grep -Fq 'TestGatewayConcurrencyStreamWaitOutputCloseCancelAndFailure' "$compat_patch" && \
    grep -Fq 'TestInternalUpstreamConcurrencyRequiresBearerAndReturnsOnlyCounters' "$compat_patch" || \
    fail 'CLIProxyAPI must expose authenticated execution concurrency with lifecycle regressions'
grep -Fq 'internalAccounts.PUT("/:id/status", s.setUpstreamAccountStatus)' "$compat_patch" && \
    grep -Fq '.gateway-account-state' "$compat_patch" && \
    grep -Fq 'TestGatewayAccountConcurrentControlsRemainDurable' "$compat_patch" && \
    grep -Fq 'TestGatewayAccountStateLoadRejectsInvalidExistingState' "$compat_patch" && \
    grep -Fq 'TestInternalUpstreamAccountStatusPersistenceFailure' "$compat_patch" && \
    grep -Fq 'TestCodexGatewayQuotaHTTPTransports' "$compat_dockerfile" && \
    grep -Fq './sdk/cliproxy' "$compat_dockerfile" || \
    fail 'CLIProxyAPI must test persistent account controls, fail-closed state, and quota signals'
grep -Fq 'codexQuotaRPCMethod          = "account/rateLimits/read"' "$compat_patch" && \
    grep -Fq 'codexQuotaRequestMaxBodySize = 256' "$compat_patch" && \
    grep -Fq 'internalAccounts.POST("/:id/quota", s.getUpstreamAccountQuota)' "$compat_patch" || \
    fail 'CLIProxyAPI patch must carry the strict POST quota RPC contract'
grep -Fq 'TestInternalUpstreamAccountQuotaRequiresExactRPCRequest' "$compat_patch" && \
    grep -Fq 'TestInternalUpstreamAccountQuotaIgnoresPlanMetadata' "$compat_patch" && \
    grep -Fq 'TestNormalizeUpstreamUsageSupportsNullableWindowsAndCeilsDuration' "$compat_patch" && \
    grep -Fq 'TestQuotaSchemaErrorCodeRejectsUnrecognizedErrors' "$compat_patch" && \
    grep -Fq 'TestInternalUpstreamAccountQuotaRejectsOversizedSensitiveResponse' "$compat_patch" || \
    fail 'CLIProxyAPI patch must carry the quota RPC security regressions'
test -d "$secret_dir" && test ! -L "$secret_dir" || {
    fail "secret directory must be a real directory: $secret_dir"
}

"$compose" config --format json > "$tmp"

# Validate JSON strings before extracting them: command substitution strips
# trailing newlines, which must never turn an injected value into a valid one.
jq -e '
  def ipv4:
    type == "string" and
    (test("[^0-9.]") | not) and
    test("^(0|[1-9][0-9]{0,2})(\\.(0|[1-9][0-9]{0,2})){3}$") and
    (split(".") | all(.[]; tonumber <= 255));
  def port:
    type == "string" and
    (test("[^0-9]") | not) and test("^[1-9][0-9]{0,4}$") and
    (tonumber <= 65535);
  .services["egress-allowlist"].environment |
  (keys | sort) == ["CODEX_RELAY_IP", "CODEX_RELAY_PORT"] and
  (.CODEX_RELAY_IP | . == "" or ipv4) and (.CODEX_RELAY_PORT | port)
' "$tmp" >/dev/null || fail 'CODEX_RELAY_IP must be empty or canonical IPv4; CODEX_RELAY_PORT must be 1..65535'
relay_ip=$(jq -r '.services["egress-allowlist"].environment.CODEX_RELAY_IP' "$tmp")
relay_port=$(jq -r '.services["egress-allowlist"].environment.CODEX_RELAY_PORT' "$tmp")
CODEX_RELAY_IP=$relay_ip CODEX_RELAY_PORT=$relay_port sh "$egress_entrypoint" --render-config >/dev/null || \
    fail 'configured relay values were rejected by the startup wrapper'

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
pricing_validator='
  def exact_keys($expected):
    type == "object" and ((keys | sort) == ($expected | sort));
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
  def positive_price:
    bounded_decimal and (tonumber > 0);
  def separate_price:
    exact_keys([
      "cache_write_usd_per_million",
      "cached_input_usd_per_million",
      "input_usd_per_million",
      "output_usd_per_million"
    ]) and
    (.input_usd_per_million | positive_price) and
    (.cached_input_usd_per_million | positive_price) and
    (.cache_write_usd_per_million | positive_price) and
    (.output_usd_per_million | positive_price);
  def included_price:
    exact_keys([
      "cached_input_usd_per_million",
      "input_usd_per_million",
      "output_usd_per_million"
    ]) and
    (.input_usd_per_million | positive_price) and
    (.cached_input_usd_per_million | positive_price) and
    (.output_usd_per_million | positive_price);
  def zero_included_price:
    exact_keys([
      "cached_input_usd_per_million",
      "input_usd_per_million",
      "output_usd_per_million"
    ]) and
    all(.[]; bounded_decimal and (tonumber == 0));
  def separate_long_tier:
    exact_keys(["long", "short"]) and
    (.short | separate_price) and
    (.long | separate_price);
  def included_long_tier:
    exact_keys(["long", "short"]) and
    (.short | included_price) and
    (.long | included_price);
  def included_short_tier:
    exact_keys(["short"]) and (.short | included_price);
  def separate_long_model:
    exact_keys([
      "cache_write_mode",
      "long_context_threshold_tokens",
      "max_input_tokens",
      "service_tiers"
    ]) and
    .cache_write_mode == "separate" and
    .max_input_tokens == 1050000 and
    .long_context_threshold_tokens == 272000 and
    .long_context_threshold_tokens < .max_input_tokens and
    (.service_tiers |
      exact_keys(["fast", "flex", "standard"]) and
      all(.[]; separate_long_tier));
  def long_included_model:
    exact_keys([
      "cache_write_mode",
      "long_context_threshold_tokens",
      "max_input_tokens",
      "service_tiers"
    ]) and
    .cache_write_mode == "included_in_input" and
    .max_input_tokens == 1050000 and
    .long_context_threshold_tokens == 272000 and
    .long_context_threshold_tokens < .max_input_tokens and
    (.service_tiers |
      exact_keys(["fast", "flex", "standard"]) and
      (.standard | included_long_tier) and
      (.flex | included_long_tier) and
      (.fast | included_short_tier));
  def mini_model:
    exact_keys([
      "cache_write_mode",
      "long_context_threshold_tokens",
      "max_input_tokens",
      "service_tiers"
    ]) and
    .cache_write_mode == "included_in_input" and
    .max_input_tokens == 272000 and
    .long_context_threshold_tokens == 272000 and
    (.service_tiers |
      exact_keys(["fast", "flex", "standard"]) and
      all(.[]; included_short_tier));
  def gemini_pro_model:
    exact_keys([
      "cache_write_mode",
      "long_context_threshold_tokens",
      "max_input_tokens",
      "service_tiers"
    ]) and
    .cache_write_mode == "included_in_input" and
    .max_input_tokens == 1048576 and
    .long_context_threshold_tokens == 200000 and
    (.service_tiers |
      exact_keys(["standard"]) and
      (.standard | included_long_tier));
  def internal_zero_model:
    exact_keys([
      "cache_write_mode",
      "long_context_threshold_tokens",
      "max_input_tokens",
      "service_tiers"
    ]) and
    .cache_write_mode == "included_in_input" and
    .max_input_tokens == 272000 and
    .long_context_threshold_tokens == 272000 and
    (.service_tiers |
      exact_keys(["standard"]) and
      (.standard | exact_keys(["short"]) and (.short | zero_included_price)));
  exact_keys([
    "catalog_as_of",
    "fallback_policy",
    "fx_as_of",
    "models",
    "schema_version",
    "usd_cny_rate"
  ]) and
  .schema_version == 2 and
  (.catalog_as_of | snapshot_date) and
  (.fx_as_of | snapshot_date) and
  (.usd_cny_rate | bounded_decimal and (test("^0(?:\\.0+)?$") | not)) and
  (.fallback_policy |
    exact_keys([
      "missing_cache_write_tokens",
      "missing_price_combination",
      "unknown_service_tier"
    ]) and
    .unknown_service_tier == "max_published" and
    .missing_price_combination == "max_published" and
    .missing_cache_write_tokens == "all_uncached_as_write") and
  (.models |
    exact_keys([
      "codex-auto-review",
      "gemini-3.1-pro-preview",
      "gpt-5.4",
      "gpt-5.4-mini",
      "gpt-5.5",
      "gpt-5.6-luna",
      "gpt-5.6-sol",
      "gpt-5.6-terra",
      "gpt-6-astra"
    ]) and
    (.["gpt-6-astra"] | separate_long_model) and
    (.["gpt-5.6-sol"] | separate_long_model) and
    (.["gpt-5.6-terra"] | separate_long_model) and
    (.["gpt-5.6-luna"] | separate_long_model) and
    (.["gpt-5.5"] | long_included_model) and
    (.["gpt-5.4"] | long_included_model) and
    (.["gpt-5.4-mini"] | mini_model) and
    (.["gemini-3.1-pro-preview"] | gemini_pro_model) and
    (.["codex-auto-review"] | internal_zero_model))
'
printf '%s\n' "$pricing_json" | jq -e "$pricing_validator" >/dev/null 2>&1 || \
    fail 'GATEWAY_USAGE_PRICING_JSON must be the strict reviewed schema v2 pricing catalog'

# Keep the readable canonical template and the validator in lockstep. These
# negative checks guard the v2 tagged union against accidentally accepting a
# v1 price field, an incomplete fallback policy, a mismatched cache-write mode,
# or an unpublished service tier.
test -r "$pricing_template" || fail "pricing template is missing: $pricing_template"
jq -e "$pricing_validator" "$pricing_template" >/dev/null 2>&1 || \
    fail 'deploy/pricing-v2.example.json does not pass the production pricing validator'
reject_pricing_mutation() {
    pricing_mutation=$1
    pricing_rejection=$2
    if jq "$pricing_mutation" "$pricing_template" |
        jq -e "$pricing_validator" >/dev/null 2>&1
    then
        fail "pricing validator accepted $pricing_rejection"
    fi
}
reject_pricing_mutation \
    '.models["gpt-5.6-sol"].input_usd_per_million = "5"' \
    'a mixed-in v1 price field'
reject_pricing_mutation \
    'del(.fallback_policy)' \
    'a missing fallback policy'
reject_pricing_mutation \
    '.models["gpt-5.6-sol"].cache_write_mode = "included_in_input"' \
    'an invalid cache-write mode'
reject_pricing_mutation \
    '.models["gpt-5.4-mini"].service_tiers.ultrafast = .models["gpt-5.4-mini"].service_tiers.fast' \
    'an unpublished service tier'
reject_pricing_mutation \
    'del(.models["gemini-3.1-pro-preview"])' \
    'a missing Gemini model'
reject_pricing_mutation \
    '.models["gemini-3.1-pro-preview"].service_tiers.flex = .models["gemini-3.1-pro-preview"].service_tiers.standard' \
    'an unconfigured Gemini service tier'
reject_pricing_mutation \
    '.models["gemini-3.1-pro-preview"].long_context_threshold_tokens = 272000' \
    'an incorrect Gemini long-context boundary'
reject_pricing_mutation \
    '.models["gemini-3.1-pro-preview"].max_input_tokens = 1050000' \
    'an incorrect Gemini input limit'
reject_pricing_mutation \
    '.models["gemini-3.1-pro-preview"].service_tiers.standard.short.cache_write_usd_per_million = "2"' \
    'a separate Gemini cache-write price'
unset pricing_json pricing_mutation pricing_rejection

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
  .networks.antigravity_internal.internal == true and
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
  .services.gateway.networks.compat_internal.ipv4_address == "172.28.30.2" and
  .services["codex-compat"].networks.compat_internal.ipv4_address == "172.28.30.3" and
  .services["egress-allowlist"].networks.compat_internal.ipv4_address == "172.28.30.4" and
  .services.gateway.networks.antigravity_internal.ipv4_address == "172.28.40.2" and
  .services["antigravity-bridge"].networks.antigravity_internal.ipv4_address == "172.28.40.3" and
  .services["egress-allowlist"].networks.antigravity_internal.ipv4_address == "172.28.40.4" and
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
cliproxy_runtime_image=$(lock_value CLIPROXY_RUNTIME_IMAGE)
squid_image=$(lock_value SQUID_IMAGE)

# Render B independently of site settings, using the same reviewed digest.
(
    unset SQUID_IMAGE
    docker compose --project-name codex-relay --env-file "$lock" \
        -f "$relay_compose" config --format json > "$relay_tmp"
)
jq -e '.services.relay.ulimits.nofile == {"soft":4096, "hard":4096}' \
    "$relay_tmp" >/dev/null || fail 'B relay nofile soft and hard limits must both be 4096'
jq -e --slurpfile relay "$relay_tmp" --arg squid "$squid_image" \
    --arg egress_config "$egress_config" --arg egress_entrypoint "$egress_entrypoint" \
    --arg relay_config "$relay_config" '
  def squid_security:
    .image == $squid and .read_only == true and .restart == "unless-stopped" and
    ((.privileged // false) == false) and .build == null and
    ((.ports // []) | length == 0) and ((.secrets // []) | length == 0) and
    (.security_opt | index("no-new-privileges:true")) != null and
    .logging.driver == "json-file" and
    .logging.options == {"max-size":"10m", "max-file":"5"} and
    (.tmpfs | sort) == [
      "/run:rw,noexec,nosuid,nodev,size=8m",
      "/var/log/squid:rw,noexec,nosuid,nodev,size=16m,mode=0750,uid=13,gid=13",
      "/var/spool/squid:rw,noexec,nosuid,nodev,size=64m,mode=0750,uid=13,gid=13"
    ];
  def mount($source; $target):
    .type == "bind" and .source == $source and .target == $target and .read_only == true;
  (.services["egress-allowlist"] |
    squid_security and .network_mode == null and
    (.networks | keys | sort) == ["antigravity_internal", "compat_internal", "egress_external"] and
    .entrypoint == ["/usr/local/bin/codex-egress-entrypoint.sh"] and
    .command == ["-f", "/etc/squid/squid.conf", "-NYC"] and
    (.volumes | length == 2) and
    any(.volumes[]; mount($egress_config; "/etc/squid/squid.conf")) and
    any(.volumes[]; mount($egress_entrypoint; "/usr/local/bin/codex-egress-entrypoint.sh"))) and
  ($relay[0] |
    (.services | keys) == ["relay"] and
    (.services.relay |
      squid_security and .network_mode == "host" and
      .entrypoint == null and .command == null and
      ((.networks // {}) | length == 0) and
      (.volumes | length == 1) and
      (.volumes[0] | mount($relay_config; "/etc/squid/squid.conf"))))
' "$tmp" >/dev/null || fail 'A/B Squid entrypoint, mounts, image lock, isolation or log limits changed'

case "$cliproxy_runtime_image" in
    docker.io/library/debian:bookworm-20260824-slim@sha256:*) ;;
    *) fail 'codex-compat requires its own reviewed Debian glibc runtime lock' ;;
esac

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
    --arg cliproxy_runtime "$cliproxy_runtime_image" \
    --arg squid "$squid_image" '
  .services.caddy.image == $caddy and
  .services.cloudflared.image == $cloudflared and
  .services.postgres.image == $postgres and
  .services["egress-allowlist"].image == $squid and
  .services.gateway.build.args.GOLANG_IMAGE == $golang and
  .services.gateway.build.args.RUNTIME_IMAGE == $runtime and
  .services["codex-compat"].build.args.GOLANG_IMAGE == $cliproxy_golang and
  .services["codex-compat"].build.args.RUNTIME_IMAGE == $cliproxy_runtime and
  .services["antigravity-bridge"].build.args.GOLANG_IMAGE == $golang and
  .services["antigravity-bridge"].build.args.RUNTIME_IMAGE == $cliproxy_runtime
' "$tmp" >/dev/null

jq -e '
  .services["codex-compat"].build.args.CLIPROXY_VERSION == "v7.3.12" and
  .services["codex-compat"].build.args.CLIPROXY_COMMIT ==
    "2eb8dd11d2480c5fd8bc8f2796cec6af534bc3b6" and
  .services["codex-compat"].build.args.GEMINI_PLUGIN_COMMIT == null
' "$tmp" >/dev/null || fail 'codex-compat must remain pinned to the reviewed host without the Gemini plugin'
test "$(jq -r '.services["codex-compat"].image' "$tmp")" = "$compat_image" || \
    fail 'codex-compat image tag must identify the reviewed Codex-only build'
jq -e '.services["codex-compat"].depends_on.gateway == null and
    .services.gateway.depends_on["codex-compat"].condition == "service_healthy" and
    .services.gateway.environment.GATEWAY_LISTEN == ":8080"' "$tmp" >/dev/null || \
    fail 'sidecar startup must not wait for its Gateway allocation callback'

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

jq -e --arg encryption_key_file "$secret_dir/gateway_api_key_encryption_key" '
  .services.gateway.environment.API_KEY_ENCRYPTION_KEY_FILE ==
    "/run/secrets/gateway_api_key_encryption_key" and
  (.services.gateway.environment.API_KEY_ENCRYPTION_KEY == null) and
  .secrets.gateway_api_key_encryption_key.file == $encryption_key_file and
  ([.services.gateway.secrets[] | .source] | sort) == [
    "antigravity_bridge_api_key",
    "database_url",
    "gateway_api_key_encryption_key",
    "gateway_api_key_pepper",
    "gateway_session_secret",
    "sidecar_api_key"
  ] and
  ([.services | to_entries[] |
    select(any(.value.secrets[]?; .source == "gateway_api_key_encryption_key")) |
    .key]) == ["gateway"]
' "$tmp" >/dev/null || \
    fail 'API key encryption key must be mounted only into Gateway through its required file setting'

# Bridge receives only its dedicated secrets/keyring and an internal network.
# Neither the Docker socket, host filesystem nor the Codex OAuth volume exists
# in this container. Gateway availability does not depend on Bridge readiness.
jq -e '
  .services["antigravity-bridge"] as $bridge |
  $bridge.read_only == true and
  ($bridge.user | test("^10002:[1-9][0-9]*$")) and
  ($bridge.cap_drop | sort) == ["ALL"] and
  ($bridge.security_opt | index("no-new-privileges:true")) != null and
  $bridge.init == true and $bridge.pids_limit == 128 and
  $bridge.platform == "linux/amd64" and
  ($bridge.networks | keys) == ["antigravity_internal"] and
  ($bridge.volumes | length) == 1 and
  $bridge.volumes[0].type == "volume" and
  $bridge.volumes[0].source == "antigravity_keyring" and
  $bridge.volumes[0].target == "/var/lib/antigravity/keyrings" and
  ([.services | to_entries[] | select(any(.value.volumes[]?; .source == "antigravity_keyring")) | .key]) == ["antigravity-bridge"] and
  ([.services | to_entries[] | select(any(.value.volumes[]?; .source == "codex_oauth")) | .key]) == ["codex-compat"] and
  ($bridge.secrets | map(.source) | sort) == ["antigravity_bridge_api_key", "antigravity_keyring_password"] and
  ([.services | to_entries[] | select(any(.value.secrets[]?; .source == "antigravity_keyring_password")) | .key]) == ["antigravity-bridge"] and
  ([.services | to_entries[] | select(any(.value.secrets[]?; .source == "antigravity_bridge_api_key")) | .key] | sort) == ["antigravity-bridge", "gateway"] and
  $bridge.environment.AGY_CLI_DISABLE_AUTO_UPDATE == "true" and
  $bridge.environment.ANTIGRAVITY_BRIDGE_API_KEY_FILE == "/run/secrets/antigravity_bridge_api_key" and
  .services.gateway.environment.ANTIGRAVITY_BRIDGE_URL == "http://antigravity-bridge:8318" and
  .services.gateway.environment.ANTIGRAVITY_BRIDGE_API_KEY_FILE == "/run/secrets/antigravity_bridge_api_key" and
  .services.gateway.depends_on["antigravity-bridge"] == null and
  ($bridge.tmpfs | sort) == [
    "/run/antigravity:rw,noexec,nosuid,nodev,size=16m,mode=0700,uid=10002,gid=10002",
    "/tmp:rw,noexec,nosuid,nodev,size=256m,mode=0700,uid=10002,gid=10002"
  ] and
  (.services.gateway.environment.ANTIGRAVITY_MODEL_ROUTES_JSON | if . == "" then {} else fromjson end |
    . == {} or . == {"gemini-3.1-pro-preview":"gemini-3.1-pro-high"})
' "$tmp" >/dev/null || fail 'Antigravity process, network, route, keyring or secret isolation failed'
if cmp -s "$secret_dir/sidecar_api_key" "$secret_dir/antigravity_bridge_api_key"; then
    fail 'Codex and Antigravity must use distinct internal Bearer secrets'
fi

# prepareModelBody admits at most four simultaneous 64 MiB request files.
# Require a private, non-executable 320 MiB tmpfs: four full spools plus one
# body-sized margin for filesystem accounting and cleanup overlap.
jq -e '
  .services.gateway.environment.GATEWAY_BODY_LIMIT_BYTES == "67108864" and
  .services.gateway.tmpfs == [
    "/tmp:rw,noexec,nosuid,nodev,size=320m,mode=0700,uid=10001,gid=10001"
  ]
' "$tmp" >/dev/null || \
    fail 'Gateway must use the reviewed 64 MiB body limit and secure 320 MiB /tmp tmpfs'

secret_gid=$(jq -r '.services.gateway.user | split(":")[1]' "$tmp")
test "$(jq -r '.services.cloudflared.user | split(":")[1]' "$tmp")" = "$secret_gid" || \
    fail 'cloudflared and gateway must use the same configured secret GID'
for secret_name in \
    cloudflared_tunnel_token \
    postgres_password \
    gateway_api_key_encryption_key \
    gateway_api_key_pepper \
    gateway_session_secret \
    sidecar_api_key \
    antigravity_bridge_api_key \
    antigravity_keyring_password \
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

api_key_encryption_key=$(cat "$secret_dir/gateway_api_key_encryption_key")
case "$api_key_encryption_key" in
    *[!A-Za-z0-9_-]*|'')
        fail 'gateway_api_key_encryption_key must be unpadded URL-safe Base64'
        ;;
esac
test "${#api_key_encryption_key}" -eq 43 || \
    fail 'gateway_api_key_encryption_key must decode to exactly 32 bytes'
unset api_key_encryption_key gateway_domain secret secret_gid secret_name

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
grep -Fq '@internal path /internal /internal/*' "$caddyfile" && \
    grep -Fq 'respond @internal 404' "$caddyfile" || \
    fail 'Caddy must block external access to every internal Gateway endpoint'
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

# Validate with the exact digest-locked binary, without project networks, service
# dependencies, volumes, or secrets. A validation run must not allocate static IPs.
validate_egress_squid() {
    docker run --rm --network none --read-only --security-opt no-new-privileges:true \
        --tmpfs /run:rw,noexec,nosuid,nodev,size=8m \
        --tmpfs /var/log/squid:rw,noexec,nosuid,nodev,size=16m,mode=0750,uid=13,gid=13 \
        --tmpfs /var/spool/squid:rw,noexec,nosuid,nodev,size=64m,mode=0750,uid=13,gid=13 \
        -e "CODEX_RELAY_IP=$1" -e "CODEX_RELAY_PORT=$2" \
        -v "$egress_config:/etc/squid/squid.conf:ro" \
        -v "$egress_entrypoint:/usr/local/bin/codex-egress-entrypoint.sh:ro" \
        --entrypoint /usr/local/bin/codex-egress-entrypoint.sh \
        "$squid_image" -k parse -f /etc/squid/squid.conf
}
validate_egress_squid '' 3128
validate_egress_squid 10.77.0.2 3128
if test -n "$relay_ip" && { test "$relay_ip" != 10.77.0.2 || test "$relay_port" != 3128; }; then
    validate_egress_squid "$relay_ip" "$relay_port"
fi
docker run --rm --network none --read-only --security-opt no-new-privileges:true \
    --ulimit nofile=4096:4096 \
    --tmpfs /run:rw,noexec,nosuid,nodev,size=8m \
    --tmpfs /var/log/squid:rw,noexec,nosuid,nodev,size=16m,mode=0750,uid=13,gid=13 \
    --tmpfs /var/spool/squid:rw,noexec,nosuid,nodev,size=64m,mode=0750,uid=13,gid=13 \
    -v "$relay_config:/etc/squid/squid.conf:ro" \
    --entrypoint /usr/sbin/squid "$squid_image" -k parse -f /etc/squid/squid.conf

caddy_validation_image=$(jq -r '.services.caddy.image' "$tmp")
caddy_validation_domain=$(jq -r '.services.caddy.environment.GATEWAY_DOMAIN' "$tmp")
docker run --rm --network none --read-only --cap-drop ALL --cap-add NET_BIND_SERVICE \
    --security-opt no-new-privileges:true \
    --tmpfs /config:rw,noexec,nosuid,nodev,size=1m \
    --tmpfs /data:rw,noexec,nosuid,nodev,size=1m \
    -e "GATEWAY_DOMAIN=$caddy_validation_domain" \
    -v "$caddyfile:/etc/caddy/Caddyfile:ro" \
    "$caddy_validation_image" caddy validate \
    --config /etc/caddy/Caddyfile --adapter caddyfile

printf '%s\n' \
    'Compose ingress, network isolation, secrets, and immutable revisions validated' \
    'Pricing v2, request tmpfs, Caddy policy, PostgreSQL SCRAM auth, and image locks validated' \
    'A/B Squid image parsing, Codex mandatory relay routing and direct Antigravity egress validated'
