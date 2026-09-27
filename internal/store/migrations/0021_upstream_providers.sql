-- Keep stable account IDs and immutable attribution while isolating account
-- management, eligibility, and allocation by upstream provider.
ALTER TABLE upstream_accounts
    ADD COLUMN provider TEXT NOT NULL DEFAULT 'codex',
    ADD COLUMN display_name TEXT NOT NULL DEFAULT '',
    ADD CONSTRAINT upstream_accounts_provider_valid
        CHECK (provider IN ('codex', 'antigravity')),
    ADD CONSTRAINT upstream_accounts_display_name_safe
        CHECK (display_name = '' OR display_name ~ '^[a-z0-9][a-z0-9_-]{0,31}$');

CREATE INDEX upstream_accounts_provider_status_idx
    ON upstream_accounts (provider, status, lower(masked_email), id);

COMMENT ON COLUMN upstream_accounts.provider IS
    'Owning upstream service; existing accounts belong to Codex. Stable IDs remain globally unique for immutable billing attribution.';
COMMENT ON COLUMN upstream_accounts.display_name IS
    'Non-secret local account slot label; never an email address or credential.';
