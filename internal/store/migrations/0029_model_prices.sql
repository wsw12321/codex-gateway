-- Preserve the row and monotonically increasing version after restoring defaults.
CREATE TABLE billing_model_prices (
    model TEXT PRIMARY KEY CHECK (model ~ '^[A-Za-z0-9._:-]{1,128}$' AND model <> 'codex-auto-review'),
    override_price JSONB,
    structure_id TEXT NOT NULL CHECK (structure_id ~ '^[0-9a-f]{64}$'),
    version BIGINT NOT NULL CHECK (version > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (override_price IS NULL OR jsonb_typeof(override_price) = 'object')
);

ALTER TABLE billing_operations
    DROP CONSTRAINT billing_operations_type_valid,
    DROP CONSTRAINT billing_operations_target_consistent,
    ADD CONSTRAINT billing_operations_type_valid CHECK (
        operation_type IN ('recharge_rate','recharge','adjustment','subscription_set',
            'subscription_disable','model_multiplier','plan_create','plan_update',
            'plan_purchase','plan_renewal','model_price')
    ),
    ADD CONSTRAINT billing_operations_target_consistent CHECK (
        (operation_type IN ('recharge_rate','model_multiplier','plan_create','plan_update','model_price') AND target_user_id IS NULL)
        OR (operation_type NOT IN ('recharge_rate','model_multiplier','plan_create','plan_update','model_price') AND target_user_id IS NOT NULL)
    );
ALTER TABLE billing_ledger_entries
    DROP CONSTRAINT billing_ledger_entry_type_valid,
    ADD CONSTRAINT billing_ledger_entry_type_valid CHECK (
        entry_type IN ('recharge_rate','recharge','adjustment','subscription_set',
            'subscription_disable','subscription_renewal','usage_charge','model_multiplier',
            'plan_create','plan_update','plan_purchase','plan_renewal','model_price')
    );

COMMENT ON TABLE billing_model_prices IS
    'Durable base-price overrides. NULL override_price restores deployment prices but retains the concurrency version. Structure mismatches fail admission closed.';
