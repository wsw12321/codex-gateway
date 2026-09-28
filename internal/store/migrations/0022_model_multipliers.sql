-- Multipliers change only future admission snapshots. Existing amounts and
-- configured base-price snapshots are deliberately left untouched.
CREATE TABLE billing_model_multipliers (
    model TEXT PRIMARY KEY,
    multiplier NUMERIC(30,12) NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    CONSTRAINT billing_model_multipliers_model_valid CHECK (
        char_length(model) BETWEEN 1 AND 128 AND model = btrim(model)
        AND model <> 'codex-auto-review'
    ),
    CONSTRAINT billing_model_multipliers_positive CHECK (
        multiplier > 0 AND multiplier < 1000000000000000000
    )
);

ALTER TABLE billing_reservations
    ADD COLUMN pricing_multiplier NUMERIC(30,12) NOT NULL DEFAULT 1,
    ADD CONSTRAINT billing_reservations_multiplier_positive CHECK (
        pricing_multiplier > 0 AND pricing_multiplier < 1000000000000000000
    );

ALTER TABLE billing_ledger_entries
    ADD COLUMN pricing_multiplier NUMERIC(30,12) NOT NULL DEFAULT 1,
    ADD CONSTRAINT billing_ledger_multiplier_positive CHECK (
        pricing_multiplier > 0 AND pricing_multiplier < 1000000000000000000
    );

ALTER TABLE billing_operations
    DROP CONSTRAINT billing_operations_type_valid,
    DROP CONSTRAINT billing_operations_target_consistent,
    ADD CONSTRAINT billing_operations_type_valid CHECK (
        operation_type IN ('recharge_rate', 'recharge', 'adjustment',
                           'subscription_set', 'subscription_disable', 'model_multiplier')
    ),
    ADD CONSTRAINT billing_operations_target_consistent CHECK (
        (operation_type IN ('recharge_rate', 'model_multiplier') AND target_user_id IS NULL)
        OR (operation_type NOT IN ('recharge_rate', 'model_multiplier') AND target_user_id IS NOT NULL)
    );

ALTER TABLE billing_ledger_entries
    DROP CONSTRAINT billing_ledger_entry_type_valid,
    ADD CONSTRAINT billing_ledger_entry_type_valid CHECK (
        entry_type IN ('recharge_rate', 'recharge', 'adjustment',
                       'subscription_set', 'subscription_disable',
                       'subscription_renewal', 'usage_charge', 'model_multiplier')
    );

COMMENT ON TABLE billing_model_multipliers IS
    'Current per-pricing-model multipliers; missing models use 1. Retained when a model leaves the configured catalog.';
COMMENT ON COLUMN billing_reservations.pricing_multiplier IS
    'Immutable multiplier captured in the admission transaction; settlement and recovery never read the current model setting.';
COMMENT ON COLUMN billing_ledger_entries.pricing_multiplier IS
    'Admission multiplier for usage charges, or the saved multiplier for a model_multiplier administrative operation. Base prices remain unscaled.';
