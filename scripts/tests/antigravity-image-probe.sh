#!/bin/sh
set -eu
umask 077

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
cat > "$probe_dir/markers" <<'MARKERS'
SYNTHETIC_ACCESS_PERSISTENCE_INITIAL
SYNTHETIC_REFRESH_PERSISTENCE_INITIAL
SYNTHETIC_ACCESS_PERSISTENCE_UPDATED
SYNTHETIC_REFRESH_PERSISTENCE_UPDATED
SYNTHETIC_ID_TOKEN_PERSISTENCE_
SYNTHETIC_STDERR_NOT_FOR_LOGS
MARKERS
for fixture in initial refreshed; do
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
if test "$mode" = refresh; then
    case "$1" in --version|models) expected=initial ;; esac
fi
cmp -s "$auth_file" "$probe_dir/$expected"
printf '%s\n' SYNTHETIC_STDERR_NOT_FOR_LOGS >&2
case "$1" in
    --version)
        test -z "$(cat)"
        printf '%s\n' 1.2.4
        ;;
    models)
        test -z "$(cat)"
        if test "$mode" = refresh; then
            cp "$probe_dir/refreshed" "$auth_file"
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
if test "$mode" = import; then
    /usr/local/bin/antigravity-entrypoint login < "$probe_dir/initial" > "$probe_dir/output" 2>&1 || status=$?
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
