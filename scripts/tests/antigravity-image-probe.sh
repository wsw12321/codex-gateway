#!/bin/sh
set -eu
umask 077

# Secret Service loads Expat through D-Bus; reject the vulnerable Bookworm build.
dpkg --compare-versions "$(dpkg-query -W -f='${Version}' libexpat1)" ge '2.5.0-1+deb12u4'
# DLA-4821-1 fixes the Perl package inherited from the locked Debian image.
dpkg --compare-versions "$(dpkg-query -W -f='${Version}' perl-base)" ge '5.36.0-7+deb12u4'

# Sent over Docker stdin by test-antigravity-image.sh. All fixtures and the fake
# executable live in private tmpfs; the only persistent mount is the keyring.
mode=$1
printf '%s' "$2" > /run/secrets/antigravity_keyring_password
probe_dir=/run/antigravity-probe
printf '%s\n' "$mode" > "$probe_dir/mode"
cat > "$probe_dir/template" <<'JSON'
{"token":{"access_token":"SYNTHETIC_ACCESS_PERSISTENCE_INITIAL","refresh_token":"SYNTHETIC_REFRESH_PERSISTENCE_INITIAL","token_type":"Bearer","expiry":"2099-01-01T00:00:00Z"},"auth_method":"oauth","project_id":"synthetic-project","region":"us-central1","user_tier":"synthetic-tier","tier_display_name":"Synthetic tier"}
JSON
# Exceed secret-tool's 8192-byte stdin limit to exercise production multipart
# persistence, including a refresh changing only the OAuth fields.
jq '.id_token = ("SYNTHETIC_ID_TOKEN_PERSISTENCE_" * 320)' \
    "$probe_dir/template" > "$probe_dir/initial"
jq '.token.access_token = "SYNTHETIC_ACCESS_PERSISTENCE_UPDATED" |
    .token.refresh_token = "SYNTHETIC_REFRESH_PERSISTENCE_UPDATED"' \
    "$probe_dir/initial" > "$probe_dir/refreshed"
for fixture in initial refreshed; do
    jq '.token.access_token = ("NAMED_" + .token.access_token) |
        .token.refresh_token = ("NAMED_" + .token.refresh_token)' \
        "$probe_dir/$fixture" > "$probe_dir/named-$fixture"
done
cat > "$probe_dir/markers" <<'MARKERS'
SYNTHETIC_ACCESS_PERSISTENCE_INITIAL
SYNTHETIC_REFRESH_PERSISTENCE_INITIAL
SYNTHETIC_ACCESS_PERSISTENCE_UPDATED
SYNTHETIC_REFRESH_PERSISTENCE_UPDATED
SYNTHETIC_ID_TOKEN_PERSISTENCE_
SYNTHETIC_STDERR_NOT_FOR_LOGS
MARKERS
for fixture in initial refreshed named-initial named-refreshed; do
    base64 -w0 < "$probe_dir/$fixture" >> "$probe_dir/markers"
    printf '\n' >> "$probe_dir/markers"
    split -b 6000 "$probe_dir/$fixture" "$probe_dir/$fixture-part-"
    for part in "$probe_dir/$fixture-part-"*; do
        base64 -w0 < "$part" >> "$probe_dir/markers"
        printf '\n' >> "$probe_dir/markers"
    done
done

if test "$mode" = ciphertext; then
    test -s /var/lib/antigravity/keyrings/login.keyring
    find /var/lib/antigravity/keyrings -type f > "$probe_dir/files"
    while IFS= read -r path; do
        if grep -aF -f "$probe_dir/markers" "$path" >/dev/null; then
            printf '%s\n' 'Persistent keyring contains an unencrypted synthetic credential' >&2
            exit 1
        else
            test "$?" -eq 1
        fi
    done < "$probe_dir/files"
    exit 0
fi

cat > "$probe_dir/agy" <<'CLI'
#!/bin/sh
set -eu
umask 077
probe_dir=/run/antigravity-probe
auth_dir=$HOME/.gemini/antigravity-cli
auth_file=$auth_dir/antigravity-oauth-token
mode=$(cat "$probe_dir/mode")
if test "$#" -eq 0; then
    # Emulate the official CLI's file write, taking credentials through stdin.
    cat > "$auth_file"
    printf '%s\n' 'unrelated login state' > "$auth_dir/must-not-persist"
    exit 0
fi
test ! -t 0
test ! -t 1
test "${TERM:-}" = dumb
test ! -e "$auth_dir/must-not-persist"
test "$(stat -c %a "$HOME")" = 700
test "$(stat -c %a "$HOME/.gemini")" = 700
test "$(stat -c %a "$auth_dir")" = 700
test "$(stat -c %a "$auth_file")" = 600
jq -e '.toolPermission == "strict"' "$auth_dir/settings.json" >/dev/null
expected=refreshed
prefix=
case "$mode" in named-*) prefix=named- ;; esac
if test "$mode" = serving && jq -e '.token.access_token | startswith("NAMED_")' "$auth_file" >/dev/null; then
    prefix=named-
fi
if test "$mode" = refresh || test "$mode" = named-refresh; then
    case "$1" in --version|models) expected=initial ;; esac
fi
cmp -s "$auth_file" "$probe_dir/$prefix$expected"
printf '%s\n' SYNTHETIC_STDERR_NOT_FOR_LOGS >&2
case "$1" in
    --version)
        test -z "$(cat)"
        printf '%s\n' 1.2.4
        ;;
    models)
        test -z "$(cat)"
        if test "$mode" = refresh || test "$mode" = named-refresh; then
            cp "$probe_dir/${prefix}refreshed" "$auth_file"
        fi
        printf '%s\n' gemini-3.1-pro-high
        ;;
    --print)
        test "$2" = /usage
        test -z "$(cat)"
        printf '%s\n' 'Synthetic usage available'
        ;;
    --input-format)
        jq -e '.event == "user" and (.message.content | length > 0)' >/dev/null
        printf '%s\n' \
            '{"event":"init","init":{"model":"gemini-3.1-pro-high","permission_mode":"strict"}}' \
            '{"event":"result","result":{"status":"SUCCESS","response":"SYNTHETIC_MODEL_REPLY_NOT_FOR_LOGS","num_turns":1,"usage":{"input_tokens":1,"output_tokens":1,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":2}}}'
        ;;
    *) exit 1 ;;
esac
CLI
chmod 0700 "$probe_dir/agy"
printf '%s\n' SYNTHETIC_MODEL_REPLY_NOT_FOR_LOGS >> "$probe_dir/markers"
export AGY_BINARY="$probe_dir/agy"
status=0
if test "$mode" = serving; then
    printf '%s' 'synthetic-api-key-at-least-32-bytes-long' > /run/secrets/antigravity_bridge_api_key
    export ANTIGRAVITY_BRIDGE_API_KEY_FILE=/run/secrets/antigravity_bridge_api_key
    /usr/local/bin/antigravity-entrypoint serve > "$probe_dir/output" 2>&1 &
    bridge_pid=$!
    trap 'kill "$bridge_pid" 2>/dev/null || :; wait "$bridge_pid" 2>/dev/null || :' EXIT
    attempt=0
    until curl -fsS --max-time 2 http://127.0.0.1:8318/readyz >/dev/null 2>&1; do
        attempt=$((attempt + 1))
        test "$attempt" -lt 30 || exit 1
        kill -0 "$bridge_pid"
        sleep 1
    done
    printf '%s\n' 'header = "Authorization: Bearer synthetic-api-key-at-least-32-bytes-long"' > "$probe_dir/curl.conf"
    curl -fsS --config "$probe_dir/curl.conf" http://127.0.0.1:8318/internal/upstream-accounts/capabilities > "$probe_dir/capabilities"
    jq -e '.protocol == "upstream_account_access_v1"' "$probe_dir/capabilities" >/dev/null
    attempt=0
    while :; do
        curl -fsS --config "$probe_dir/curl.conf" http://127.0.0.1:8318/internal/upstream-accounts > "$probe_dir/accounts"
        if jq -e '(.accounts | length) == 2 and all(.accounts[]; .status == "available")' "$probe_dir/accounts" >/dev/null; then break; fi
        attempt=$((attempt + 1))
        test "$attempt" -lt 30 || exit 1
        sleep 1
    done
    jq -e '(.accounts | length) == 2 and all(.accounts[]; .status == "available") and
        ([.accounts[].display_name] | sort) == ["default", "work"] and
        all(.accounts[]; .masked_email == "" and .plan == "unknown")' "$probe_dir/accounts" >/dev/null
    for account in default work; do
        /usr/local/bin/antigravity-smoke "$account" >> "$probe_dir/output" 2>&1
    done
    # Normal model requests never inherit the local operator smoke exemption.
    code=$(curl -sS --config "$probe_dir/curl.conf" -H 'Content-Type: application/json' \
        --data '{"model":"gemini-3.1-pro-high","input":"hello"}' -o "$probe_dir/rejected" \
        -w '%{http_code}' http://127.0.0.1:8318/v1/responses)
    test "$code" = 503
    jq -e '.error.code == "upstream_allocation_unavailable"' "$probe_dir/rejected" >/dev/null
    kill "$bridge_pid"
    wait "$bridge_pid" || :
    trap - EXIT
elif test "$mode" = import; then
    /usr/local/bin/antigravity-entrypoint login < "$probe_dir/initial" > "$probe_dir/output" 2>&1 || status=$?
elif test "$mode" = named-import; then
    /usr/local/bin/antigravity-entrypoint login work < "$probe_dir/named-initial" > "$probe_dir/output" 2>&1 || status=$?
elif test "$mode" = named-refresh || test "$mode" = named-restored; then
    /usr/local/bin/antigravity-entrypoint verify-login work < /dev/null > "$probe_dir/output" 2>&1 || status=$?
else
    /usr/local/bin/antigravity-entrypoint verify-login < /dev/null > "$probe_dir/output" 2>&1 || status=$?
fi
if grep -aF -f "$probe_dir/markers" "$probe_dir/output" >/dev/null; then
    printf '%s\n' 'Bridge exposed a credential, model reply or CLI stderr' >&2
    exit 1
else
    test "$?" -eq 1
fi
if test "$mode" = wrong-password; then
    test "$status" -ne 0
    grep -Fx 'antigravity: stage=keyring category=keyring_failed exit_code=1' "$probe_dir/output" >/dev/null
    exit 0
fi
cat "$probe_dir/output"
exit "$status"
