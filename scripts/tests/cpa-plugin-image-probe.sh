#!/bin/sh
# Runs inside the plugin-mode CPA image, as its service user, with no network.
# Mode "serving" expects a working plugin; mode "no-exec-tmp" expects the
# plugin to be unloadable and everything to stay closed.
set -eu
umask 077

mode=${1:-serving}
port=8317
internal=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
management=BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB
auth=/var/lib/cliproxy/oauth
run=/run/cliproxy
plugin=/usr/local/lib/cliproxy/plugins/codex-gateway-v1.0.0.so
# A synthetic, unsigned token for a ChatGPT account that does not exist. Its
# stable Gateway account index is 517a9dc5c8a7dec0.
token=eyJhbGciOiJub25lIn0.eyJlbWFpbCI6InN5bnRoZXRpY0BleGFtcGxlLmNvbSIsImV4cCI6NDA3MDkwODgwMCwiaHR0cHM6Ly9hcGkub3BlbmFpLmNvbS9hdXRoIjp7ImNoYXRncHRfYWNjb3VudF9pZCI6ImFjY3Qtc3ludGhldGljIiwiY2hhdGdwdF9wbGFuX3R5cGUiOiJwbHVzIiwiY2hhdGdwdF91c2VyX2lkIjoidXNlci1zeW50aGV0aWMifX0.synthetic
account=517a9dc5c8a7dec0
model=gpt-6-astra

fail() {
    printf '%s\n' "plugin image probe failed: $*" >&2
    exit 1
}

# request METHOD PATH KEY [BODY] writes the raw HTTP response to $run/response.
request() {
    if test -n "${4:-}"; then
        printf '%s %s HTTP/1.1\r\nHost: 127.0.0.1:%s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %s\r\nConnection: close\r\n\r\n%s' \
            "$1" "$2" "$port" "$3" "${#4}" "$4"
    else
        printf '%s %s HTTP/1.1\r\nHost: 127.0.0.1:%s\r\nAuthorization: Bearer %s\r\nConnection: close\r\n\r\n' \
            "$1" "$2" "$port" "$3"
    fi | nc -w 5 127.0.0.1 "$port" > "$run/response" || true
}

# generate sends one generation request with a complete trusted identity.
generate() {
    body="{\"model\":\"$model\",\"input\":[{\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"hi\"}]}]}"
    printf 'POST /v1/responses HTTP/1.1\r\nHost: 127.0.0.1:%s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nX-Codex-Gateway-User: 11111111-1111-4111-8111-111111111111\r\nX-Codex-Gateway-Affinity: AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE\r\nX-Codex-Gateway-Request: %s\r\nContent-Length: %s\r\nConnection: close\r\n\r\n%s' \
        "$port" "$1" "$internal" "${#body}" "$body" | nc -w 10 127.0.0.1 "$port" > "$run/response" || true
}

expect_status() {
    head -n 1 "$run/response" | grep -Fq " $1 " || fail "$2: expected HTTP $1, got $(head -n 1 "$run/response")"
}

if test "${CPA_PLUGIN_PROBE_SKIP_PACKAGES:-}" != 1; then
    # Reject the vulnerable perl-base and APT inherited from the pinned base.
    dpkg --compare-versions "$(dpkg-query -W -f='${Version}' perl-base)" ge '5.36.0-7+deb12u4' ||
        fail "perl-base lacks DLA-4821-1 security fixes"
    for package in apt libapt-pkg6.0; do
        dpkg --compare-versions "$(dpkg-query -W -f='${Version}' "$package")" ge '2.6.2' ||
            fail "$package lacks the CVE-2026-109518 security fix"
    done
fi

# The service user can read the plugin but can neither replace it nor add one.
test "$(id -u)" = 10001 || fail "the image does not run as the service user"
test -f "$plugin" && test -r "$plugin" || fail "plugin is missing or unreadable"
test "$(stat -c '%u %a' "$plugin")" = "0 444" || fail "plugin must be root-owned and mode 0444"
test ! -w "$plugin" && test ! -w "$(dirname "$plugin")" || fail "plugin is writable by the service user"

export CLIPROXY_API_KEY_FILE="$run/synthetic-key"
export CPA_MANAGEMENT_KEY_FILE="$run/synthetic-management-key"
printf '%s\n' "$internal" > "$CLIPROXY_API_KEY_FILE"
printf '%s\n' "$management" > "$CPA_MANAGEMENT_KEY_FILE"
printf '{"type":"codex","email":"synthetic@example.com","account_id":"acct-synthetic","id_token":"%s","access_token":"%s","refresh_token":"synthetic-refresh","expired":"2099-01-01T00:00:00Z","last_refresh":"2026-01-01T00:00:00Z"}\n' \
    "$token" "$token" > "$auth/codex-synthetic.json"

# The two keys must differ, and a second process must not share the store.
if CPA_MANAGEMENT_KEY_FILE="$CLIPROXY_API_KEY_FILE" /usr/local/bin/sidecar-entrypoint --version > "$run/same-key.log" 2>&1; then
    fail "started with one key for both planes"
fi
grep -Fq "management key must differ from the internal API key" "$run/same-key.log" || fail "wrong refusal for a shared key"

/usr/local/bin/sidecar-entrypoint > "$run/startup.log" 2>&1 &
sidecar_pid=$!
trap 'kill "$sidecar_pid" 2>/dev/null || true' EXIT
attempt=0
until nc -z -w 1 127.0.0.1 "$port" 2>/dev/null; do
    kill -0 "$sidecar_pid" 2>/dev/null || { cat "$run/startup.log" >&2; fail "CPA exited during startup"; }
    attempt=$((attempt + 1))
    test "$attempt" -lt 30 || fail "CPA did not open its port"
    sleep 1
done
# Wait until the synthetic credential's models are published.
attempt=0
until request GET /v1/models "$internal" && grep -Fq "\"$model\"" "$run/response"; do
    attempt=$((attempt + 1))
    # Without the plugin the native key check still lists models; with it the
    # same key is checked by the plugin. Either way the list must appear.
    test "$attempt" -lt 30 || fail "the synthetic credential never registered its models"
    sleep 1
done

grep -Fq 'strategy: "plugin-only"' "$run/config.yaml" || fail "routing strategy is not plugin-only"
grep -A1 '^discovery:' "$run/config.yaml" | grep -Eq 'enabled:[[:space:]]*false' || fail "LAN discovery is enabled"
test "$(stat -c '%a' "$run/config.yaml")" = 600 || fail "configuration file is not private"
if /usr/local/bin/sidecar-entrypoint --version > "$run/second-process.log" 2>&1; then
    fail "a second OAuth refresh process acquired the credential store"
fi
grep -Fq "OAuth store is owned by another refresh process" "$run/second-process.log" || fail "wrong refusal for a second process"

if test "$mode" = no-exec-tmp; then
    # The plugin cannot be mapped from a noexec directory. That must look like
    # an outage, never like a CPA that routes on its own.
    sleep 3
    if /usr/local/bin/sidecar-healthcheck; then
        fail "healthy without a loaded plugin"
    fi
    request GET /v0/management/gateway/upstream-accounts/capabilities "$management"
    expect_status 404 "capabilities without the plugin"
    generate "$internal"
    expect_status 503 "generation without the plugin"
    grep -Fq gateway_allocation_unavailable "$run/response" || fail "generation without the plugin was not refused by the selector"
    printf '%s\n' "Without an executable temporary directory the plugin does not load, the container is unhealthy, the capability probe fails and generation is refused"
    exit 0
fi

attempt=0
until /usr/local/bin/sidecar-healthcheck; do
    kill -0 "$sidecar_pid" 2>/dev/null || { cat "$run/startup.log" >&2; fail "CPA exited before becoming healthy"; }
    attempt=$((attempt + 1))
    test "$attempt" -lt 30 || { cat "$run/startup.log" >&2; fail "the plugin never became healthy"; }
    sleep 1
done
grep -Fq "Version: ${CPA_PLUGIN_PROBE_VERSION:-v8.0.59}" "$run/startup.log" || fail "unexpected CPA version"
grep -Fq "Commit: ${CPA_PLUGIN_PROBE_COMMIT:-c5251b57b3fa5dcbc689f514f4aa166daacb4481}" "$run/startup.log" || fail "unexpected CPA commit"

for namespace in upstream-accounts antigravity-accounts anthropic-accounts; do
    request GET "/v0/management/gateway/$namespace/capabilities" "$management"
    expect_status 200 "$namespace capabilities"
    grep -Fq '"protocol":"upstream_account_access_v1"' "$run/response" || fail "$namespace lacks the access protocol"
    grep -Fq '"group_upstream_account_scope":"group_upstream_account_scope_v1"' "$run/response" || fail "$namespace lacks the group scope protocol"
    grep -Fq '"account_egress":"account_egress_v1"' "$run/response" || fail "$namespace does not enforce account egress"
    request GET "/v0/management/gateway/$namespace/funding-layers" "$management"
    grep -Fq '"protocol":"group_upstream_account_layers_v1"' "$run/response" || fail "$namespace lacks the funding layer protocol"
    # The data-plane key and a missing key open nothing on the management side.
    request GET "/v0/management/gateway/$namespace/capabilities" "$internal"
    expect_status 401 "$namespace capabilities with the data-plane key"
done
request GET /v0/management/gateway/anthropic-accounts/capabilities "$management"
grep -Fq '"anthropic_messages":"anthropic_messages_v1"' "$run/response" || fail "anthropic namespace lacks the Messages protocol"
request GET /v0/management/auth-files "$internal"
expect_status 401 "native management API with the data-plane key"
request GET /v1/models "$management"
expect_status 401 "data plane with the management key"

request GET /v0/management/gateway/upstream-accounts "$management"
expect_status 200 "account list"
grep -Fq "\"id\":\"$account\"" "$run/response" || fail "the synthetic account is not listed under its stable index"
grep -Fq '"masked_email":"s***@example.com"' "$run/response" || fail "the account e-mail is not masked"
grep -Fq '"plan":"plus"' "$run/response" || fail "the account plan is not reported"
if grep -Eq 'synthetic@example.com|acct-synthetic|synthetic-refresh|eyJhbGci' "$run/response"; then
    fail "the account list leaks credential content"
fi
# Without a Gateway the account's route cannot be confirmed, so it is not
# changed either.
request PUT "/v0/management/gateway/upstream-accounts/egress?id=$account" "$management" '{}'
expect_status 503 "egress sync without a Gateway"
request PUT "/v0/management/gateway/upstream-accounts/egress?id=$account" "$internal" '{}'
expect_status 401 "egress sync with the data-plane key"
request GET /v0/management/gateway/upstream-accounts/concurrency "$management"
expect_status 200 "concurrency snapshot"
grep -Fq "\"id\":\"$account\"" "$run/response" || fail "the concurrency snapshot omits the account"

request PUT "/v0/management/gateway/upstream-accounts/status?id=$account" "$management" '{"enabled":false}'
expect_status 200 "disable account"
grep -Fq '"gateway_manual_status":"manual_disabled"' "$run/response" || fail "the account was not disabled"
test "$(stat -c '%a' "$auth/.gateway-account-state")" = 600 || fail "the control document is not private"
grep -Fq "\"$account\"" "$auth/.gateway-account-state" || fail "the disable was not persisted"
request PUT "/v0/management/gateway/upstream-accounts/status?id=$account" "$management" '{"enabled":true}'
expect_status 200 "enable account"
grep -Fq '"status":"available"' "$run/response" || fail "the account was not re-enabled"
request PUT "/v0/management/gateway/upstream-accounts/status?id=0123456789abcdef" "$management" '{"enabled":true}'
expect_status 404 "status of an unknown account"

# No Gateway exists on this network, so allocation cannot be decided. The
# request must be refused before anything is sent upstream.
generate "$internal"
expect_status 503 "generation without a Gateway"
generate "$management"
expect_status 401 "generation with the management key"

kill -0 "$sidecar_pid" 2>/dev/null || fail "CPA exited during the probe"
printf '%s\n' "Plugin integrity, key separation, exclusive refresh locking, account routes for all three providers, durable account control and fail-closed allocation checks passed"
