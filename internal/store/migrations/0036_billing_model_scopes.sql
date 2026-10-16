-- Model scopes for cash and group funding.  Scopes are initialized by the
-- gateway after synchronizing the authoritative manageable model catalog.
-- Keeping initialization separate from this forward-only schema change makes
-- an empty or unavailable catalog fail closed during startup.
ALTER TABLE billing_settings
    ADD COLUMN model_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN models_initialized BOOLEAN NOT NULL DEFAULT false,
    ADD CONSTRAINT billing_settings_models_valid CHECK (
        jsonb_typeof(model_ids) = 'array'
        AND (NOT models_initialized OR jsonb_array_length(model_ids) > 0)
    );

ALTER TABLE user_groups
    ADD COLUMN model_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN models_initialized BOOLEAN NOT NULL DEFAULT false,
    ADD CONSTRAINT user_groups_models_valid CHECK (
        jsonb_typeof(model_ids) = 'array'
        AND (NOT models_initialized OR jsonb_array_length(model_ids) > 0)
    );

ALTER TABLE billing_operations DROP CONSTRAINT billing_operations_type_valid,
    DROP CONSTRAINT billing_operations_target_consistent,
    ADD CONSTRAINT billing_operations_type_valid CHECK (operation_type IN (
        'recharge_rate','recharge','adjustment','subscription_set','subscription_disable',
        'model_multiplier','plan_create','plan_update','plan_delete','plan_purchase',
        'plan_renewal','model_price','subscription_preferences','cash_preferences',
        'cash_models')),
    ADD CONSTRAINT billing_operations_target_consistent CHECK (
        (operation_type IN ('recharge_rate','model_multiplier','plan_create','plan_update',
          'plan_delete','model_price','cash_models') AND target_user_id IS NULL)
        OR (operation_type NOT IN ('recharge_rate','model_multiplier','plan_create','plan_update',
          'plan_delete','model_price','cash_models') AND target_user_id IS NOT NULL));

ALTER TABLE billing_ledger_entries DROP CONSTRAINT billing_ledger_entry_type_valid,
    ADD CONSTRAINT billing_ledger_entry_type_valid CHECK (entry_type IN (
        'recharge_rate','recharge','adjustment','subscription_set','subscription_disable',
        'subscription_renewal','usage_charge','model_multiplier','plan_create','plan_update',
        'plan_delete','plan_purchase','plan_renewal','model_price','subscription_preferences',
        'cash_preferences','cash_models'));
