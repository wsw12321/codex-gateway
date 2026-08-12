#!/bin/sh
set -eu

gateway_base_url=${1:-'__CODEX_GATEWAY_BASE_URL__'}
gateway_base_url=${gateway_base_url%/}

case "$gateway_base_url" in
    http://*|https://*) ;;
    *)
        printf '%s\n' 'configure-codex: Gateway URL must start with http:// or https://' >&2
        exit 1
        ;;
esac

config_dir=${CODEX_HOME:-"${HOME:?HOME is not set}/.codex"}
config_file=$config_dir/config.toml
umask 077
mkdir -p "$config_dir"

if [ -f "$config_file" ]; then
    cp "$config_file" "$config_file.bak"
elif [ -e "$config_file" ]; then
    printf '%s\n' "configure-codex: $config_file is not a regular file" >&2
    exit 1
fi

temporary_file=$(mktemp "$config_dir/config.toml.tmp.XXXXXX")
cleanup() {
    rm -f "$temporary_file"
}
trap cleanup EXIT HUP INT TERM

source_file=$config_file
if [ ! -f "$source_file" ]; then
    source_file=/dev/null
fi

awk '
BEGIN {
    in_gateway = 0
    seen_section = 0
    wrote_provider = 0
}
{
    line = $0
    if (in_gateway) {
        if (line ~ /^[[:space:]]*\[/) {
            in_gateway = 0
        } else {
            next
        }
    }
    if (line ~ /^[[:space:]]*\[model_providers\.gateway\][[:space:]]*(#.*)?$/) {
        in_gateway = 1
        next
    }
    if (!seen_section && line ~ /^[[:space:]]*\[/) {
        if (!wrote_provider) {
            print "model_provider = \"gateway\""
            print ""
            wrote_provider = 1
        }
        seen_section = 1
    }
    if (!seen_section && line ~ /^[[:space:]]*model_provider[[:space:]]*=/) {
        if (!wrote_provider) {
            print "model_provider = \"gateway\""
            wrote_provider = 1
        }
        next
    }
    print line
}
END {
    if (!wrote_provider) {
        print ""
        print "model_provider = \"gateway\""
    }
}
' "$source_file" > "$temporary_file"

printf '%s\n' \
    '' \
    '[model_providers.gateway]' \
    'name = "Personal Codex Gateway"' \
    "base_url = \"$gateway_base_url\"" \
    'env_key = "CODEX_GATEWAY_API_KEY"' \
    'wire_api = "responses"' \
    'env_http_headers = { "X-Codex-Project" = "CODEX_GATEWAY_PROJECT" }' \
    'request_max_retries = 2' \
    'stream_max_retries = 2' >> "$temporary_file"

mv "$temporary_file" "$config_file"
trap - EXIT HUP INT TERM

printf '%s\n' "Codex configured: $config_file"
if [ -f "$config_file.bak" ]; then
    printf '%s\n' "Previous configuration backup: $config_file.bak"
fi
