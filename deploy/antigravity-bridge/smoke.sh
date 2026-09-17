#!/bin/sh
set -eu
umask 077
work_dir=$(mktemp -d /tmp/antigravity-smoke.XXXXXX)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM
cd "$work_dir"

if test "${1:-}" = cli; then
    agy models > "$work_dir/models" 2> "$work_dir/stderr"
    awk '$1 == "gemini-3.1-pro-high" { found=1 } END { exit !found }' "$work_dir/models"
    agy --print /usage --print-timeout 30s > "$work_dir/usage" 2> "$work_dir/stderr"
    test -s "$work_dir/usage"
    printf '%s\n' '{"event":"user","message":{"content":"Reply with exactly OK."}}' |
        agy --input-format stream-json --output-format stream-json --model gemini-3.1-pro-high \
            --disable-slash-commands --log-file "$work_dir/agy.log" --print-timeout 5m \
            > "$work_dir/response" 2> "$work_dir/stderr"
    jq -se '([.[] | select(.event == "result")] | length) == 1 and
        all(.[]; .event != "step_update" or .step_update.step_type != "tool") and
        any(.[]; .event == "result" and .result.status == "SUCCESS" and (.result.response | length > 0))' \
        "$work_dir/response" >/dev/null
else
    # Curl reads the Bearer secret from a private file, never process arguments.
    key=$(tr -d '\r\n' < /run/secrets/antigravity_bridge_api_key)
    case "$key" in *[!A-Za-z0-9_-]*|'') exit 1 ;; esac
    printf 'header = "Authorization: Bearer %s"\n' "$key" > "$work_dir/curl.conf"
    unset key
    url=http://127.0.0.1:8318
    curl -fsS --config "$work_dir/curl.conf" "$url/v1/models" > "$work_dir/models"
    jq -e 'any(.data[]; .id == "gemini-3.1-pro-preview")' "$work_dir/models" >/dev/null
    for stream in false true; do
        printf '{"model":"gemini-3.1-pro-preview","input":"Reply with exactly OK.","store":false,"stream":%s}\n' "$stream" > "$work_dir/request"
        curl -fsS --max-time 310 --config "$work_dir/curl.conf" -H 'Content-Type: application/json' \
            --data-binary @"$work_dir/request" "$url/v1/responses" > "$work_dir/response"
        if test "$stream" = true; then
            grep -Fq 'response.completed' "$work_dir/response"
            grep -Fxq 'data: [DONE]' "$work_dir/response"
        else
            jq -e '.status == "completed" and .usage.input_tokens >= 0 and .usage.output_tokens >= 0' "$work_dir/response" >/dev/null
        fi
    done
fi
printf '%s\n' 'Antigravity model, usage and text smoke checks passed; response and credential contents were not displayed'
