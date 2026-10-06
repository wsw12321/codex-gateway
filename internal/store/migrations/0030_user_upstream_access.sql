-- Missing rules retain the default of all accounts. An explicit selected rule
-- remains meaningful with no members: that provider is disabled for the user.
CREATE TABLE user_upstream_access (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK (provider IN ('codex', 'antigravity')),
    mode TEXT NOT NULL CHECK (mode IN ('all', 'selected')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, provider)
);

-- Enforce provider isolation even for writes outside the application.
ALTER TABLE upstream_accounts ADD CONSTRAINT upstream_accounts_id_provider_key
    UNIQUE (id, provider);

CREATE TABLE user_upstream_access_accounts (
    user_id UUID NOT NULL,
    provider TEXT NOT NULL,
    upstream_account_id TEXT NOT NULL,
    PRIMARY KEY (user_id, provider, upstream_account_id),
    FOREIGN KEY (user_id, provider) REFERENCES user_upstream_access(user_id, provider) ON DELETE CASCADE,
    FOREIGN KEY (upstream_account_id, provider) REFERENCES upstream_accounts(id, provider) ON DELETE CASCADE
);
CREATE INDEX user_upstream_access_accounts_account_idx
    ON user_upstream_access_accounts (upstream_account_id, provider);

COMMENT ON TABLE user_upstream_access IS
    'Local per-user account scope, intersected with account exclusivity and all existing request restrictions. Metadata snapshots never replace these rules.';
