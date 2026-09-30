#!/bin/sh
# Run only in the disposable maintenance-image test container, with --network
# none and tmpfs mounts. Every password and token here is a synthetic fixture.
set -eu
umask 077

install -d -o 10002 -g 10002 -m 0700 /run/cpa-migrate/keyring /run/cpa-migrate/staging \
    /run/cpa-migrate/staging/source /run/cpa-migrate/staging/source/.gemini \
    /run/cpa-migrate/staging/source/.gemini/antigravity-cli /run/cpa-migrate/staging/restored
chmod 0755 /run/cpa-migrate
install -d -m 0755 /run/secrets
printf '%s' 'synthetic-test-password' > /run/secrets/antigravity_keyring_password
chown 10002:10002 /run/secrets/antigravity_keyring_password
printf '%s' '{"token":{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","token_type":"Bearer","expiry":"2099-01-01T00:00:00Z"},"project_id":"synthetic-project"}' \
    > /run/cpa-migrate/staging/source/.gemini/antigravity-cli/antigravity-oauth-token
chown 10002:10002 /run/cpa-migrate/staging/source/.gemini/antigravity-cli/antigravity-oauth-token

setpriv --reuid 10002 --regid 10002 --clear-groups /usr/local/bin/cpa-keyring-helper \
    /usr/local/bin/cpa-credentials-migrate save default /run/cpa-migrate/staging/source
setpriv --reuid 10002 --regid 10002 --clear-groups /usr/local/bin/cpa-keyring-helper \
    /usr/local/bin/cpa-credentials-migrate restore default /run/cpa-migrate/staging/restored
cmp /run/cpa-migrate/staging/source/.gemini/antigravity-cli/antigravity-oauth-token \
    /run/cpa-migrate/staging/restored/.gemini/antigravity-cli/antigravity-oauth-token

# The production root orchestrator must reject a live refresher before reading
# a registry or invoking a provider, even when its advisory lock is root-held.
install -d -o 10001 -g 10001 -m 0700 /oauth
exec 8>/var/lib/antigravity/keyrings/.gateway-refresh.lock
flock -n 8
if /usr/local/bin/cpa-credentials-migrate -egress-proxy http://127.0.0.1:3128 \
    >/run/cpa-migrate/staging/report 2>/run/cpa-migrate/staging/diagnostic; then
    printf '%s\n' 'migration bypassed a running refresher lock' >&2
    exit 1
fi
test "$(cat /run/cpa-migrate/staging/diagnostic)" = 'cpa-credentials-migrate: refresher_still_running'
printf '%s\n' 'Synthetic encrypted Keyring round trip and live-refresher lock rejection passed.'
