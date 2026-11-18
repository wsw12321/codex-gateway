#!/bin/sh
set -eu
# Pricing tools for a deployment. Prices are built into the Gateway image; the
# only local input is the optional deploy/pricing/override.json.
#
#   export-legacy           print the retired GATEWAY_USAGE_PRICING_JSON value
#   migrate-env < FILE      turn that value into a minimal override document
#   show [-builtin] [-model ID]
#                           print the effective (or built-in) catalog
#   check [NAME.json]       validate deploy/pricing/override.json (or NAME.json)
#
# Every command except export-legacy runs `gateway pricing` from the image
# selected by .env in a container without network, secrets or database.

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
# This is trusted repository code, never a shell interpretation of .env.
. "$root/scripts/egress-common.sh"

pricing_fail() {
    printf '%s\n' "pricing: $*" >&2
    exit 1
}

usage() {
    pricing_fail 'usage: scripts/pricing.sh export-legacy | migrate-env < FILE | show [-builtin] [-model ID] | check [NAME.json]'
}

test "$#" -gt 0 || usage
command=$1
shift
file=override.json

case "$command" in
export-legacy)
    test "$#" -eq 0 || usage
    # Compose reads .env exactly as a deployment does; no shell evaluates it.
    # This is the only command that works while the retired line is present.
    egress_clear_environment "$root"
    settings=$(docker compose \
        --project-name codex-gateway-pricing --project-directory "$root" \
        --env-file "$egress_env_file" \
        -f "$root/scripts/egress-settings.compose.yml" config --format json) || exit 1
    printf '%s' "$settings" | jq -er '
      .services["egress-settings"].environment.GATEWAY_USAGE_PRICING_JSON |
      select(type == "string" and length > 0)
    ' || pricing_fail 'GATEWAY_USAGE_PRICING_JSON is not set in .env; there is nothing to export'
    exit 0
    ;;
migrate-env)
    test "$#" -eq 0 || usage
    ;;
show)
    ;;
check)
    if test "$#" -gt 0; then
        test "$#" -eq 1 || usage
        file=$1
        shift
        printf '%s\n' "$file" | grep -Eq '^[A-Za-z0-9_-][A-Za-z0-9._-]*\.json$' || \
            pricing_fail 'check takes a file name inside deploy/pricing, such as override.candidate.json'
    fi
    ;;
*)
    usage
    ;;
esac

# The same .env validation as every Compose call: it refuses a leftover
# GATEWAY_USAGE_PRICING_JSON line, so migrate-env reads the exported file.
egress_prepare "$root"
exec docker compose \
    --project-name codex-gateway-pricing --project-directory "$root" \
    --env-file "$egress_env_file" --env-file "$egress_lock" \
    -f "$root/scripts/pricing-tool.compose.yml" \
    run --rm --no-deps -T \
    -e "GATEWAY_USAGE_PRICING_OVERRIDE_FILE=/etc/codex-gateway/pricing/$file" \
    pricing-tool "$command" "$@"
