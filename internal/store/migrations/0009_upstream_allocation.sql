-- Allocation preferences are Gateway-owned and are never replaced by sidecar
-- metadata synchronization. Zero drains only new conversation assignments.
ALTER TABLE upstream_accounts
    ADD COLUMN allocation_weight INTEGER NOT NULL DEFAULT 1,
    ADD CONSTRAINT upstream_accounts_allocation_weight_nonnegative
        CHECK (allocation_weight >= 0);

COMMENT ON COLUMN upstream_accounts.allocation_weight IS
    'Relative target share of settled rolling 24-hour cost for new assignments; zero preserves existing bindings but accepts no new assignments.';

-- Match the statistics/selection request-time convention, including legacy
-- ledger rows that did not retain a request timestamp.
CREATE INDEX billing_ledger_upstream_account_requested_time_idx
    ON billing_ledger_entries
        (upstream_account_id, (COALESCE(usage_requested_at, created_at)))
    INCLUDE (actual_cost_usd)
    WHERE entry_type = 'usage_charge';
