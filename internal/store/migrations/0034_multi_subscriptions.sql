-- Stop the old gateway before applying this migration. The synchronized model
-- catalog is copied once by InitializeBillingSubscriptionModels before serving.
UPDATE billing_subscriptions SET plan_id=NULL,plan_version=NULL,
    config_version=nextval('billing_subscription_config_versions');
DELETE FROM billing_plans;

ALTER TABLE billing_accounts ADD COLUMN cash_priority INTEGER NOT NULL DEFAULT 100
    CHECK (cash_priority BETWEEN 1 AND 100);
ALTER TABLE billing_subscriptions
    DROP CONSTRAINT billing_subscriptions_user_id_tier_key,
    DROP CONSTRAINT billing_subscriptions_tier_valid,
    DROP CONSTRAINT billing_subscriptions_current_period_fk,
    ADD COLUMN name TEXT NOT NULL DEFAULT 'Subscription',
    ADD COLUMN period_days INTEGER,
    ADD COLUMN model_ids JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN models_initialized BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN charge_enabled BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN priority INTEGER NOT NULL DEFAULT 1 CHECK (priority BETWEEN 1 AND 100),
    ADD CONSTRAINT billing_subscriptions_tier_valid CHECK (tier IN ('day','week','month','custom')),
    ADD CONSTRAINT billing_subscriptions_name_valid CHECK (char_length(btrim(name)) BETWEEN 1 AND 100),
    ADD CONSTRAINT billing_subscriptions_models_valid CHECK (jsonb_typeof(model_ids)='array' AND (NOT models_initialized OR jsonb_array_length(model_ids)>0)),
    ADD CONSTRAINT billing_subscriptions_id_user_key UNIQUE(id,user_id);
UPDATE billing_subscriptions s SET
    name=CASE tier WHEN 'day' THEN 'Daily subscription' WHEN 'week' THEN 'Weekly subscription' ELSE 'Monthly subscription' END,
    period_days=CASE tier WHEN 'day' THEN 1 WHEN 'week' THEN 7 ELSE 31 END,
    charge_enabled=NOT CASE tier WHEN 'day' THEN a.day_source_disabled WHEN 'week' THEN a.week_source_disabled ELSE a.month_source_disabled END
FROM billing_accounts a WHERE a.user_id=s.user_id;
ALTER TABLE billing_subscriptions ALTER COLUMN period_days SET NOT NULL,
    ADD CONSTRAINT billing_subscriptions_days_valid CHECK ((tier='day' AND period_days=1) OR (tier='week' AND period_days=7) OR (tier='month' AND period_days=31) OR (tier='custom' AND period_days BETWEEN 1 AND 3650));

ALTER TABLE billing_plans DROP CONSTRAINT billing_plans_tier_check,
    ADD COLUMN period_days INTEGER NOT NULL,
    ADD COLUMN model_ids JSONB NOT NULL,
    ADD COLUMN deleted_at TIMESTAMPTZ,
    ADD CONSTRAINT billing_plans_tier_check CHECK (tier IN ('day','week','month','custom')),
    ADD CONSTRAINT billing_plans_days_valid CHECK ((tier='day' AND period_days=1) OR (tier='week' AND period_days=7) OR (tier='month' AND period_days=31) OR (tier='custom' AND period_days BETWEEN 1 AND 3650)),
    ADD CONSTRAINT billing_plans_models_valid CHECK (jsonb_typeof(model_ids)='array' AND jsonb_array_length(model_ids)>0);

CREATE TABLE billing_subscription_entitlements (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscription_id UUID NOT NULL,
    user_id UUID NOT NULL,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ,
    tier TEXT NOT NULL CHECK (tier IN ('day','week','month','custom')),
    period_days INTEGER NOT NULL CHECK (period_days BETWEEN 1 AND 3650),
    allowance_usd NUMERIC(30,12) NOT NULL CHECK (allowance_usd>0),
    model_ids JSONB NOT NULL CHECK (jsonb_typeof(model_ids)='array'),
    period_count INTEGER NOT NULL CHECK (period_count>=0),
    first_period_number INTEGER NOT NULL DEFAULT 1 CHECK (first_period_number>=1),
    plan_snapshot JSONB,
    superseded_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY(subscription_id,user_id) REFERENCES billing_subscriptions(id,user_id) ON DELETE CASCADE,
    UNIQUE(id,subscription_id,user_id),
    CHECK ((period_count=0 AND ends_at IS NULL) OR (period_count>0 AND ends_at>starts_at)),
    CHECK ((tier='day' AND period_days=1) OR (tier='week' AND period_days=7) OR (tier='month' AND period_days=31) OR tier='custom')
);
CREATE INDEX billing_entitlements_current_idx ON billing_subscription_entitlements(subscription_id,starts_at) WHERE superseded_at IS NULL;
ALTER TABLE billing_subscription_periods
    DROP CONSTRAINT billing_periods_subscription_fk,
    DROP CONSTRAINT billing_periods_tier_valid,
    DROP CONSTRAINT billing_periods_duration_valid,
    ADD COLUMN entitlement_id UUID,
    ADD COLUMN period_days INTEGER,
    ADD CONSTRAINT billing_periods_subscription_fk FOREIGN KEY(subscription_id,user_id) REFERENCES billing_subscriptions(id,user_id) ON DELETE RESTRICT,
    ADD CONSTRAINT billing_periods_entitlement_fk FOREIGN KEY(entitlement_id,subscription_id,user_id) REFERENCES billing_subscription_entitlements(id,subscription_id,user_id) ON DELETE RESTRICT,
    ADD CONSTRAINT billing_periods_id_user_key UNIQUE(id,user_id),
    ADD CONSTRAINT billing_periods_owner_key UNIQUE(id,subscription_id,user_id),
    ADD CONSTRAINT billing_periods_tier_valid CHECK (tier IN ('day','week','month','custom'));
UPDATE billing_subscription_periods SET period_days=CASE tier WHEN 'day' THEN 1 WHEN 'week' THEN 7 ELSE 31 END;
ALTER TABLE billing_subscription_periods ALTER COLUMN period_days SET NOT NULL,
    ADD CONSTRAINT billing_periods_duration_valid CHECK (period_days BETWEEN 1 AND 3650 AND ends_at=starts_at+period_days*INTERVAL '24 hours');
ALTER TABLE billing_subscriptions ADD CONSTRAINT billing_subscriptions_current_period_fk
    FOREIGN KEY(current_period_id,id,user_id) REFERENCES billing_subscription_periods(id,subscription_id,user_id) ON DELETE RESTRICT;
-- The current legacy period remains exactly as it was, including its ID and
-- balance. Its segment starts there and retains its original period number.
INSERT INTO billing_subscription_entitlements(subscription_id,user_id,starts_at,ends_at,tier,period_days,allowance_usd,model_ids,period_count,first_period_number,created_at)
SELECT s.id,s.user_id,p.starts_at,s.expires_at,s.tier,s.period_days,s.allowance_usd,'[]',
    CASE WHEN s.period_count=0 THEN 0 ELSE s.period_count-s.current_period_number+1 END,
    s.current_period_number,s.created_at
FROM billing_subscriptions s JOIN billing_subscription_periods p ON p.id=s.current_period_id;
UPDATE billing_subscription_periods p SET entitlement_id=e.id FROM billing_subscription_entitlements e
WHERE p.subscription_id=e.subscription_id AND p.starts_at>=e.starts_at;

ALTER TABLE billing_reservations
    DROP CONSTRAINT billing_reservations_funding_rule_version_check,
    DROP CONSTRAINT billing_reservations_source_required,
    ADD COLUMN ordered_funding_sources JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(ordered_funding_sources)='array'),
    ADD CONSTRAINT billing_reservations_funding_rule_version_check CHECK (funding_rule_version IN (1,2,3)),
    ADD CONSTRAINT billing_reservations_source_required CHECK (billing_mode='internal_zero'
        OR day_period_id IS NOT NULL OR week_period_id IS NOT NULL OR month_period_id IS NOT NULL OR cash_lot_cutoff IS NOT NULL
        OR (funding_rule_version>=2 AND group_period_id IS NOT NULL)
        OR (funding_rule_version=3 AND jsonb_array_length(ordered_funding_sources)>0)),
    ADD CONSTRAINT billing_reservations_request_user_key UNIQUE(request_id,user_id);
CREATE TABLE billing_reservation_sources (
    request_id TEXT NOT NULL,
    position INTEGER NOT NULL CHECK (position>0),
    user_id UUID NOT NULL,
    subscription_period_id UUID,
    cash_lot_cutoff BIGINT CHECK (cash_lot_cutoff>0),
    PRIMARY KEY(request_id,position),
    FOREIGN KEY(request_id,user_id) REFERENCES billing_reservations(request_id,user_id) ON DELETE CASCADE,
    FOREIGN KEY(subscription_period_id,user_id) REFERENCES billing_subscription_periods(id,user_id) ON DELETE RESTRICT,
    CHECK ((subscription_period_id IS NULL)<>(cash_lot_cutoff IS NULL))
);
CREATE INDEX billing_reservation_sources_period_idx ON billing_reservation_sources(subscription_period_id) WHERE subscription_period_id IS NOT NULL;
ALTER TABLE billing_charge_allocations
    DROP CONSTRAINT billing_allocations_period_tier_fk,
    DROP CONSTRAINT billing_allocations_source_valid,
    DROP CONSTRAINT billing_allocations_source_consistent,
    ADD CONSTRAINT billing_allocations_source_valid CHECK (source_type IN ('group','day','week','month','subscription','cash')),
    ADD CONSTRAINT billing_allocations_source_consistent CHECK (
        (source_type='group' AND group_period_id IS NOT NULL AND subscription_period_id IS NULL AND cash_credit_lot_id IS NULL)
        OR (source_type='cash' AND cash_credit_lot_id IS NOT NULL AND subscription_period_id IS NULL AND group_period_id IS NULL)
        OR (source_type IN ('day','week','month','subscription') AND subscription_period_id IS NOT NULL AND cash_credit_lot_id IS NULL AND group_period_id IS NULL));
ALTER TABLE billing_ledger_entries DROP CONSTRAINT billing_ledger_tier_valid,
    ADD CONSTRAINT billing_ledger_tier_valid CHECK (subscription_tier IS NULL OR subscription_tier IN ('day','week','month','custom'));
ALTER TABLE billing_operations DROP CONSTRAINT billing_operations_type_valid,DROP CONSTRAINT billing_operations_target_consistent,
    ADD CONSTRAINT billing_operations_type_valid CHECK (operation_type IN ('recharge_rate','recharge','adjustment','subscription_set','subscription_disable','model_multiplier','plan_create','plan_update','plan_delete','plan_purchase','plan_renewal','model_price','subscription_preferences','cash_preferences')),
    ADD CONSTRAINT billing_operations_target_consistent CHECK (
      (operation_type IN ('recharge_rate','model_multiplier','plan_create','plan_update','plan_delete','model_price') AND target_user_id IS NULL)
      OR (operation_type NOT IN ('recharge_rate','model_multiplier','plan_create','plan_update','plan_delete','model_price') AND target_user_id IS NOT NULL));
ALTER TABLE billing_ledger_entries DROP CONSTRAINT billing_ledger_entry_type_valid,
    ADD CONSTRAINT billing_ledger_entry_type_valid CHECK (entry_type IN ('recharge_rate','recharge','adjustment','subscription_set','subscription_disable','subscription_renewal','usage_charge','model_multiplier','plan_create','plan_update','plan_delete','plan_purchase','plan_renewal','model_price','subscription_preferences','cash_preferences'));
COMMENT ON COLUMN billing_subscriptions.plan_version IS 'Version at the last purchase; editing a plan retains renewal eligibility at its current quoted version.';
