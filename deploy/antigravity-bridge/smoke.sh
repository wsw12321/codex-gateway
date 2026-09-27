#!/bin/sh
set -eu
umask 077
export TERM=dumb

# Compatibility for manual CLI checks; the binary owns credential restoration,
# per-command refresh persistence and safe diagnostics.
if test "${1:-}" = cli; then
    shift
    exec /usr/local/bin/antigravity-bridge auth-verify "$@" </dev/null
fi

account=${1:-default}
case "$account" in *[!a-z0-9_-]*|'') exit 1 ;; esac
test "${#account}" -le 32 || exit 1

stage=http_models
fail() {
    printf 'antigravity: stage=%s category=%s exit_code=%s\n' "$stage" "$1" "$2" >&2
    exit "$2"
}
work_dir=$(mktemp -d /tmp/antigravity-smoke.XXXXXX) || fail io_failed 1
cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    rm -rf "$work_dir"
    exit "$status"
}
trap cleanup EXIT
trap 'fail canceled 129' HUP
trap 'fail canceled 130' INT
trap 'fail canceled 143' TERM
cd "$work_dir" || fail io_failed 1

# Curl reads the Bearer secret from a private file, never process arguments.
key=$({ tr -d '\r\n' < /run/secrets/antigravity_bridge_api_key; } 2>/dev/null) || fail configuration 1
case "$key" in *[!A-Za-z0-9_-]*|'') fail configuration 1 ;; esac
printf 'header = "Authorization: Bearer %s"\n' "$key" > "$work_dir/curl.conf"
unset key
url=http://127.0.0.1:8318
if curl -fsS --max-time 30 --config "$work_dir/curl.conf" "$url/v1/models" \
    </dev/null > "$work_dir/models" 2>/dev/null; then :; else
    fail command_failed "$?"
fi
jq -e 'any(.data[]; .id == "gemini-3.1-pro-preview")' "$work_dir/models" \
    </dev/null >/dev/null 2>&1 || fail invalid_response 1
for stream in false true; do
    if test "$stream" = true; then stage=http_sse; else stage=http_json; fi
    printf '{"model":"gemini-3.1-pro-preview","input":"Reply with exactly OK.","store":false,"stream":%s}\n' "$stream" > "$work_dir/request"
    if curl -fsS --max-time 310 --config "$work_dir/curl.conf" -H 'Content-Type: application/json' \
        --data-binary @"$work_dir/request" "$url/internal/smoke/responses/$account" \
        </dev/null > "$work_dir/response" 2>/dev/null; then :; else
        fail command_failed "$?"
    fi
    if test "$stream" = true; then
        grep -Fxq 'data: [DONE]' "$work_dir/response" || fail invalid_response 1
        # Keep the protocol data pipe; only commands needing no input use null.
        if awk '/^data: / && $0 != "data: [DONE]" { sub(/^data: /, ""); print }' "$work_dir/response" |
            jq -se '([.[] | select(.type == "response.completed")] | length) == 1 and
                all(.[]; .type != "error" and .type != "response.failed") and
                any(.[]; .type == "response.completed" and .response.status == "completed" and
                    .response.usage.input_tokens >= 0 and .response.usage.output_tokens >= 0 and
                    any(.response.output[]?; .type == "message" and
                        any(.content[]?; .type == "output_text" and (.text | length > 0))))' \
                >/dev/null 2>&1; then :; else
            fail invalid_response 1
        fi
    else
        jq -e '.status == "completed" and .usage.input_tokens >= 0 and .usage.output_tokens >= 0 and
            any(.output[]?; .type == "message" and
                any(.content[]?; .type == "output_text" and (.text | length > 0)))' \
            "$work_dir/response" </dev/null >/dev/null 2>&1 || fail invalid_response 1
    fi
done
