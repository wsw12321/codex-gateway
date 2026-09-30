#!/bin/sh
set -eu
umask 077
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose=$root/scripts/compose.sh
lock_file=$root/.antigravity-login.lock
bridge_started=0
credentials_only=0
work_dir=
stage=setup
failure_reported=0
# Read the controlling terminal, since stdin may be redirected by the caller.
terminal_state=$({ stty -g < /dev/tty; } 2>/dev/null) || terminal_state=

compose_legacy() {
    "$compose" --profile legacy-bridge "$@"
}

diagnostic() {
    printf 'antigravity: stage=%s category=%s exit_code=%s\n' "$1" "$2" "$3" >&2
}
fail() {
    failure_reported=1
    diagnostic "$1" "$2" "$3"
    exit "$3"
}
restore_terminal() {
    if test -n "$terminal_state"; then
        { stty "$terminal_state" < /dev/tty; } 2>/dev/null || return 1
    fi
}
cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    if test "$status" -ne 0 && test "$failure_reported" -eq 0; then
        diagnostic "$stage" command_failed "$status"
    fi
    if test "$bridge_started" -eq 1; then
        if compose_legacy stop -t 10 antigravity-bridge </dev/null >/dev/null 2>&1; then :; else
            cleanup_status=$?
            diagnostic stop command_failed "$cleanup_status"
            test "$status" -ne 0 || status=$cleanup_status
        fi
    fi
    if restore_terminal; then :; else
        diagnostic terminal cleanup_failed 1
        test "$status" -ne 0 || status=1
    fi
    test -z "$work_dir" || rm -rf "$work_dir"
    exit "$status"
}
trap cleanup EXIT
trap 'fail "$stage" interrupted 129' HUP
trap 'fail "$stage" interrupted 130' INT
trap 'fail "$stage" interrupted 143' TERM

# Only our bounded diagnostic vocabulary may cross the container boundary.
# Compose/CLI stderr can contain OAuth URLs or credentials and is never echoed.
relay_diagnostics() {
    awk '/^antigravity: stage=(keyring|authorization|credential_restore|credential_save|models|usage|generation|http_models|http_json|http_sse) category=(command_failed|invalid_credentials|credential_missing|credential_invalid|credential_too_large|unsafe_path|invalid_permissions|keyring_failed|timeout|canceled|invalid_response|cleanup_failed|configuration|io_failed) exit_code=[0-9]+$/ { print > "/dev/stderr" }' "$work_dir/stderr"
}
run_quiet() {
    stage=$1
    shift
    if "$@" </dev/null >"$work_dir/stdout" 2>"$work_dir/stderr"; then return; else
        status=$?
        relay_diagnostics
        fail "$stage" command_failed "$status"
    fi
}

if test "${1:-}" = --credentials-only; then
    credentials_only=1
    shift
    test "$#" -eq 1 || fail setup configuration 1
fi
test "$#" -le 1 || fail setup configuration 1
if test "$#" -eq 1; then
    case "$1" in *[!a-z0-9_-]*|''|[_-]*) fail setup configuration 1 ;; esac
    test "${#1}" -le 32 || fail setup configuration 1
fi
stage=lock
command -v flock >/dev/null 2>&1 || fail lock configuration 1
if test -e "$lock_file" || test -L "$lock_file"; then
    test -f "$lock_file" && test ! -L "$lock_file" || fail lock unsafe_path 1
fi
{ exec 9>"$lock_file"; } 2>/dev/null
chmod 0600 "$lock_file" 2>/dev/null || fail lock invalid_permissions 1
flock -n 9 || fail lock busy 1
work_dir=$(mktemp -d /tmp/antigravity-login.XXXXXX) || fail setup io_failed 1

# A single Secret Service owns the volume. Failed login leaves the bridge
# stopped; Codex and Gateway continue serving their other models.
# Inspect the actual CPA process before any service or credential mutation.
# Emit only S (stopped), F (one literal disabled gate) or X (another value).
# Multiple matching variables yield multiple markers and fail closed. Never
# dump the container environment, which can contain credentials.
run_quiet service_state compose_legacy ps -q codex-compat
cpa_container_id=$(cat "$work_dir/stdout")
if test -n "$cpa_container_id"; then
    run_quiet service_state docker inspect -f '{{if .State.Running}}{{range .Config.Env}}{{if eq (index (split . "=") 0) "CPA_ANTIGRAVITY_ENABLED"}}{{if eq . "CPA_ANTIGRAVITY_ENABLED=false"}}F{{else}}X{{end}}{{end}}{{end}}{{else}}S{{end}}' "$cpa_container_id"
    cpa_google_state=$(cat "$work_dir/stdout")
    if test "$cpa_google_state" != S; then
        test "$credentials_only" -eq 0 && test "$cpa_google_state" = F || fail service_state still_running 1
    fi
fi
run_quiet stop compose_legacy stop -t 10 antigravity-bridge
run_quiet service_state compose_legacy ps -q antigravity-bridge
container_id=$(cat "$work_dir/stdout")
if test -n "$container_id"; then
    run_quiet service_state docker inspect -f '{{.State.Running}}' "$container_id"
    test "$(cat "$work_dir/stdout")" = false || fail service_state still_running 1
fi
run_quiet egress compose_legacy up -d egress-allowlist
printf '%s\n' 'Complete the official agy remote login, then use /exit. Independent credential, model, usage and text checks follow.'
stage=authorization
# Only official interactive authorization inherits stdin and terminal output.
authorization_command=login
test "$credentials_only" -eq 0 || authorization_command=reauthorize
# Keep this an external command: redirecting a shell function would also hide
# the parent shell's signal-trap diagnostics while interactive login runs.
if "$compose" --profile legacy-bridge run --rm --no-deps antigravity-bridge "$authorization_command" "$@" 2>"$work_dir/stderr"; then :; else
    status=$?
    relay_diagnostics
    fail authorization command_failed "$status"
fi
restore_terminal || fail terminal cleanup_failed 1

# A second container verifies encrypted credentials using a new D-Bus session
# and HOME. All checks are deliberately detached from the login terminal.
run_quiet verification compose_legacy run -T --rm --no-deps -e TERM=dumb antigravity-bridge verify-login "$@"
if test "$credentials_only" -eq 1; then
    printf '%s\n' 'Antigravity existing-slot reauthorization persisted; CLI verification passed; bridge remains stopped. Rerun the CPA credential migration and Gateway acceptance.'
    exit 0
fi
bridge_started=1
run_quiet start compose_legacy up -d --no-deps antigravity-bridge
stage=readiness
attempt=0
readiness_status=1
while test "$attempt" -lt 30; do
    if compose_legacy exec -T -e TERM=dumb antigravity-bridge curl -fsS --max-time 4 http://127.0.0.1:8318/readyz </dev/null >/dev/null 2>&1; then
        run_quiet http_acceptance compose_legacy exec -T -e TERM=dumb antigravity-bridge /usr/local/bin/antigravity-smoke "$@"
        restore_terminal || fail terminal cleanup_failed 1
        bridge_started=0
        printf '%s\n' 'Antigravity login persisted; readiness, JSON and SSE passed.'
        exit 0
    else
        readiness_status=$?
    fi
    attempt=$((attempt + 1))
    test "$attempt" -ge 30 || sleep 2
done
fail readiness command_failed "$readiness_status"
