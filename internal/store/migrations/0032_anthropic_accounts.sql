-- Credentials stay in CPA. Gateway stores only opaque verified identity indexes.
ALTER TABLE upstream_accounts DROP CONSTRAINT upstream_accounts_provider_valid,
    ADD CONSTRAINT upstream_accounts_provider_valid
        CHECK (provider IN ('codex', 'antigravity', 'anthropic'));
ALTER TABLE user_upstream_access DROP CONSTRAINT user_upstream_access_provider_check,
    ADD CONSTRAINT user_upstream_access_provider_check
        CHECK (provider IN ('codex', 'antigravity', 'anthropic'));
-- No scope rows are inserted: missing rows continue to mean all accounts.
