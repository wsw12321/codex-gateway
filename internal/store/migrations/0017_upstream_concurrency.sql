-- Gateway-owned per-account admission limits. A limit of one preserves the
-- historical behavior while allowing owners to raise the limit explicitly.
ALTER TABLE upstream_accounts
    ADD COLUMN concurrent_limit INTEGER NOT NULL DEFAULT 1,
    ADD CONSTRAINT upstream_accounts_concurrent_limit_positive
        CHECK (concurrent_limit > 0);

COMMENT ON COLUMN upstream_accounts.concurrent_limit IS
    'Maximum number of requests concurrently in flight through this upstream account.';
