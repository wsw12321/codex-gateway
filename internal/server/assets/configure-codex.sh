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

awk -v gateway_base_url="$gateway_base_url" '
BEGIN {
    in_root = 1
    found = 0
    replacement = "openai_base_url = \"" gateway_base_url "\""
}
{
    line = $0
    if (in_root && line ~ /^[[:space:]]*\[/) {
        in_root = 0
    }
    if (in_root && line ~ /^[[:space:]]*openai_base_url[[:space:]]*=/) {
        if (!found) {
            lines[NR] = replacement
            found = 1
        } else {
            skipped[NR] = 1
        }
        next
    }
    lines[NR] = line
}
END {
    if (!found) {
        print replacement
    }
    for (line_number = 1; line_number <= NR; line_number++) {
        if (!skipped[line_number]) {
            print lines[line_number]
        }
    }
}
' "$source_file" > "$temporary_file"

mv "$temporary_file" "$config_file"
trap - EXIT HUP INT TERM

printf '%s\n' "Codex configured: $config_file"
if [ -f "$config_file.bak" ]; then
    printf '%s\n' "Previous configuration backup: $config_file.bak"
fi
