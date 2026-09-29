-- Catalog versions invalidate renewal bindings without locking subscriber rows.
CREATE TABLE billing_plans (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 100),
    price_usd NUMERIC(30,12) NOT NULL CHECK (price_usd > 0),
    tier TEXT NOT NULL CHECK (tier IN ('day','week','month')),
    allowance_usd NUMERIC(30,12) NOT NULL CHECK (allowance_usd > 0),
    min_period_count INTEGER NOT NULL DEFAULT 1 CHECK (min_period_count BETWEEN 1 AND 99),
    active BOOLEAN NOT NULL DEFAULT false,
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);

-- Never reuse a configuration version, including after expired history cleanup.
CREATE SEQUENCE billing_subscription_config_versions AS BIGINT;
ALTER TABLE billing_subscriptions
    ADD COLUMN plan_id UUID REFERENCES billing_plans(id) ON DELETE RESTRICT,
    ADD COLUMN plan_version BIGINT,
    ADD COLUMN config_version BIGINT NOT NULL DEFAULT nextval('billing_subscription_config_versions'),
    ADD CONSTRAINT billing_subscription_binding_consistent CHECK (
        (plan_id IS NULL AND plan_version IS NULL)
        OR (plan_id IS NOT NULL AND plan_version IS NOT NULL AND plan_version > 0)
    ),
    ADD CONSTRAINT billing_subscription_config_version_positive CHECK (config_version > 0),
    DROP CONSTRAINT billing_subscriptions_period_count_valid,
    ADD CONSTRAINT billing_subscriptions_period_count_valid CHECK (period_count >= 0);
ALTER TABLE billing_subscription_periods
    DROP CONSTRAINT billing_periods_period_count_valid,
    ADD CONSTRAINT billing_periods_period_count_valid CHECK (period_count >= 0);
ALTER TABLE billing_subscription_operation_snapshots
    DROP CONSTRAINT billing_subscription_operation_snapshots_period_count_valid,
    ADD CONSTRAINT billing_subscription_operation_snapshots_period_count_valid CHECK (period_count >= 0);

-- Complete write responses live in immutable ledger history; existing replay
-- reconstruction remains available for operations committed before this migration.
ALTER TABLE billing_ledger_entries ADD COLUMN transaction_snapshot JSONB;
ALTER TABLE billing_operations
    DROP CONSTRAINT billing_operations_type_valid,
    DROP CONSTRAINT billing_operations_target_consistent,
    ADD CONSTRAINT billing_operations_type_valid CHECK (
        operation_type IN ('recharge_rate','recharge','adjustment','subscription_set',
            'subscription_disable','model_multiplier','plan_create','plan_update',
            'plan_purchase','plan_renewal')
    ),
    ADD CONSTRAINT billing_operations_target_consistent CHECK (
        (operation_type IN ('recharge_rate','model_multiplier','plan_create','plan_update') AND target_user_id IS NULL)
        OR (operation_type NOT IN ('recharge_rate','model_multiplier','plan_create','plan_update') AND target_user_id IS NOT NULL)
    );
ALTER TABLE billing_ledger_entries
    DROP CONSTRAINT billing_ledger_entry_type_valid,
    ADD CONSTRAINT billing_ledger_entry_type_valid CHECK (
        entry_type IN ('recharge_rate','recharge','adjustment','subscription_set',
            'subscription_disable','subscription_renewal','usage_charge','model_multiplier',
            'plan_create','plan_update','plan_purchase','plan_renewal')
    );

COMMENT ON COLUMN billing_subscriptions.plan_version IS
    'Renewal is valid only while the catalog plan is active and its version matches this binding.';
COMMENT ON COLUMN billing_ledger_entries.transaction_snapshot IS
    'Immutable configuration and complete response snapshot for plan sales, plan edits, and subscription administrative writes.';
