-- Preserve existing group periods and total usage. Member accounting starts
-- empty and includes only group-funded charges accepted under funding rule 2.
ALTER TABLE user_groups ADD COLUMN member_limit_usd NUMERIC(30,12)
    CHECK (member_limit_usd IS NULL OR member_limit_usd >= 0);
ALTER TABLE group_usage_periods ADD COLUMN member_limit_usd NUMERIC(30,12)
    CHECK (member_limit_usd IS NULL OR member_limit_usd >= 0);

CREATE TABLE group_member_usage (
    period_id UUID NOT NULL REFERENCES group_usage_periods(id) ON DELETE RESTRICT,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    used_usd NUMERIC(30,12) NOT NULL DEFAULT 0 CHECK (used_usd >= 0),
    PRIMARY KEY (period_id, user_id)
);
CREATE INDEX group_member_usage_user_idx ON group_member_usage(user_id);

-- The default protects reservations written before the gateway upgrade. New
-- admission explicitly writes rule 2; old in-flight requests settle as before.
ALTER TABLE billing_reservations
    ADD COLUMN funding_rule_version INTEGER NOT NULL DEFAULT 1 CHECK (funding_rule_version IN (1,2)),
    ADD COLUMN group_charged_usd NUMERIC(30,12),
    ADD COLUMN personal_charged_usd NUMERIC(30,12);
UPDATE billing_reservations SET group_charged_usd=0, personal_charged_usd=charged_usd
WHERE state='settled';
ALTER TABLE billing_reservations
    DROP CONSTRAINT billing_reservations_source_required,
    ADD CONSTRAINT billing_reservations_source_required CHECK (
        billing_mode='internal_zero'
        OR day_period_id IS NOT NULL OR week_period_id IS NOT NULL
        OR month_period_id IS NOT NULL OR cash_lot_cutoff IS NOT NULL
        OR (funding_rule_version=2 AND group_period_id IS NOT NULL)
    ),
    ADD CONSTRAINT billing_reservations_funding_split_valid CHECK (
        (state='settled' AND group_charged_usd IS NOT NULL AND personal_charged_usd IS NOT NULL
         AND group_charged_usd>=0 AND personal_charged_usd>=0
         AND charged_usd=group_charged_usd+personal_charged_usd)
        OR (state<>'settled' AND group_charged_usd IS NULL AND personal_charged_usd IS NULL)
    );

ALTER TABLE billing_ledger_entries
    ADD COLUMN group_charged_usd NUMERIC(30,12),
    ADD COLUMN personal_charged_usd NUMERIC(30,12);
-- This metadata-only backfill identifies historical personal payments without
-- changing costs, cash debits or group totals. Restore ledger immutability in
-- the same migration transaction before allowing application writes.
ALTER TABLE billing_ledger_entries DISABLE TRIGGER billing_ledger_entries_immutable;
UPDATE billing_ledger_entries SET group_charged_usd=0, personal_charged_usd=charged_usd
WHERE entry_type='usage_charge' AND charged_usd IS NOT NULL;
ALTER TABLE billing_ledger_entries ENABLE TRIGGER billing_ledger_entries_immutable;
ALTER TABLE billing_ledger_entries ADD CONSTRAINT billing_ledger_funding_split_valid CHECK (
    (entry_type='usage_charge' AND charged_usd IS NOT NULL
     AND group_charged_usd IS NOT NULL AND personal_charged_usd IS NOT NULL
     AND group_charged_usd>=0 AND personal_charged_usd>=0
     AND charged_usd=group_charged_usd+personal_charged_usd)
    OR (entry_type<>'usage_charge' AND group_charged_usd IS NULL AND personal_charged_usd IS NULL)
    OR (entry_type='usage_charge' AND charged_usd IS NULL
        AND group_charged_usd IS NULL AND personal_charged_usd IS NULL)
);

ALTER TABLE billing_charge_allocations
    ADD COLUMN group_period_id UUID REFERENCES group_usage_periods(id) ON DELETE RESTRICT,
    DROP CONSTRAINT billing_allocations_source_valid,
    DROP CONSTRAINT billing_allocations_source_consistent,
    ADD CONSTRAINT billing_allocations_source_valid CHECK (source_type IN ('group','day','week','month','cash')),
    ADD CONSTRAINT billing_allocations_source_consistent CHECK (
        (source_type='group' AND group_period_id IS NOT NULL
         AND subscription_period_id IS NULL AND cash_credit_lot_id IS NULL)
        OR (source_type='cash' AND cash_credit_lot_id IS NOT NULL
            AND subscription_period_id IS NULL AND group_period_id IS NULL)
        OR (source_type IN ('day','week','month') AND subscription_period_id IS NOT NULL
            AND cash_credit_lot_id IS NULL AND group_period_id IS NULL)
    );

COMMENT ON COLUMN group_usage_periods.used_usd IS
    'Preserved historical full costs plus actual group payments under funding rule 2; legacy in-flight requests still add full cost.';
COMMENT ON TABLE group_member_usage IS
    'Actual group-funded amounts under funding rule 2, independent of membership and removable request/ledger history.';
COMMENT ON COLUMN group_usage_periods.member_limit_usd IS
    'Member limit shared with this group period; null means unlimited, zero disables group payments. Limits-only edits keep existing member usage.';
