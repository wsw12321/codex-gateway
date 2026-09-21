-- Local access preferences survive authoritative sidecar metadata snapshots.
ALTER TABLE upstream_accounts ADD COLUMN access_mode TEXT NOT NULL DEFAULT 'shared'
    CHECK (access_mode IN ('shared', 'exclusive'));

CREATE TABLE upstream_account_users (
    upstream_account_id TEXT NOT NULL REFERENCES upstream_accounts(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    PRIMARY KEY (upstream_account_id, user_id)
);
CREATE INDEX upstream_account_users_user_idx ON upstream_account_users(user_id, upstream_account_id);
