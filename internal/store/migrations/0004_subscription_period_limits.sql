-- Forward-only finite subscription periods. Existing subscriptions retain
-- their current allowance, balance and period boundaries; enabled rows become
-- one-period subscriptions and stop at the already scheduled period end.

ALTER TABLE billing_subscriptions
    ADD COLUMN period_count INTEGER,
    ADD COLUMN current_period_number INTEGER,
    ADD COLUMN expires_at TIMESTAMPTZ;

ALTER TABLE billing_subscription_periods
    ADD COLUMN period_number INTEGER,
    ADD COLUMN period_count INTEGER;

UPDATE billing_subscription_periods
SET period_number = 1,
    period_count = 1;

UPDATE billing_subscriptions s
SET period_count = 1,
    current_period_number = 1,
    expires_at = p.ends_at
FROM billing_subscription_periods p
WHERE p.id = s.current_period_id;

-- A subscription without a current period could never have supplied quota.
-- Give disabled legacy rows a deterministic finite snapshot, while failing
-- closed for an impossible enabled row instead of silently granting a period.
UPDATE billing_subscriptions
SET period_count = 1,
    current_period_number = 1,
    expires_at = COALESCE(disabled_at, updated_at, created_at)
WHERE current_period_id IS NULL AND NOT enabled;

UPDATE billing_subscription_periods p
SET closed_at = p.ends_at,
    close_reason = 'expired'
FROM billing_subscriptions s
WHERE s.current_period_id = p.id
  AND s.enabled
  AND p.ends_at <= now()
  AND p.closed_at IS NULL;

UPDATE billing_subscriptions s
SET enabled = false,
    disabled_at = p.ends_at
FROM billing_subscription_periods p
WHERE s.current_period_id = p.id
  AND s.enabled
  AND p.ends_at <= now();

ALTER TABLE billing_subscriptions
    ALTER COLUMN period_count SET NOT NULL,
    ALTER COLUMN current_period_number SET NOT NULL,
    ADD CONSTRAINT billing_subscriptions_period_count_valid CHECK (
        period_count BETWEEN 0 AND 99
    ),
    ADD CONSTRAINT billing_subscriptions_period_number_valid CHECK (
        current_period_number >= 1
        AND (period_count = 0 OR current_period_number <= period_count)
    ),
    ADD CONSTRAINT billing_subscriptions_expiry_consistent CHECK (
        (period_count = 0 AND expires_at IS NULL)
        OR (period_count > 0 AND expires_at IS NOT NULL)
    );

ALTER TABLE billing_subscription_periods
    ALTER COLUMN period_number SET NOT NULL,
    ALTER COLUMN period_count SET NOT NULL,
    ADD CONSTRAINT billing_periods_period_count_valid CHECK (
        period_count BETWEEN 0 AND 99
    ),
    ADD CONSTRAINT billing_periods_period_number_valid CHECK (
        period_number >= 1
        AND (period_count = 0 OR period_number <= period_count)
    );

-- Most subscription operations can reconstruct their immutable response from
-- the referenced period snapshot. Legacy disabled subscriptions may have no
-- current period, so preserve their period configuration beside the immutable
-- ledger entry instead of consulting mutable subscription state during replay.
CREATE TABLE billing_subscription_operation_snapshots (
    ledger_entry_id BIGINT PRIMARY KEY
        REFERENCES billing_ledger_entries(id) ON DELETE RESTRICT,
    subscription_id UUID NOT NULL
        REFERENCES billing_subscriptions(id) ON DELETE RESTRICT,
    period_count INTEGER NOT NULL,
    current_period_number INTEGER NOT NULL,
    expires_at TIMESTAMPTZ,
    CONSTRAINT billing_subscription_operation_snapshots_period_count_valid CHECK (
        period_count BETWEEN 0 AND 99
    ),
    CONSTRAINT billing_subscription_operation_snapshots_period_number_valid CHECK (
        current_period_number >= 1
        AND (period_count = 0 OR current_period_number <= period_count)
    ),
    CONSTRAINT billing_subscription_operation_snapshots_expiry_consistent CHECK (
        (period_count = 0 AND expires_at IS NULL)
        OR (period_count > 0 AND expires_at IS NOT NULL)
    )
);

INSERT INTO billing_subscription_operation_snapshots
    (ledger_entry_id, subscription_id, period_count, current_period_number, expires_at)
SELECT l.id, s.id, s.period_count, s.current_period_number, s.expires_at
FROM billing_ledger_entries l
JOIN billing_subscriptions s
  ON s.user_id = l.user_id AND s.tier = l.subscription_tier
WHERE l.entry_type IN ('subscription_set', 'subscription_disable')
  AND l.subscription_period_id IS NULL;

CREATE TRIGGER billing_subscription_operation_snapshots_immutable
    BEFORE UPDATE OR DELETE ON billing_subscription_operation_snapshots
    FOR EACH ROW EXECUTE FUNCTION gateway_reject_billing_ledger_mutation();

CREATE INDEX billing_subscriptions_expiry_idx
    ON billing_subscriptions (expires_at, user_id)
    WHERE enabled AND period_count > 0;

COMMENT ON COLUMN billing_subscriptions.period_count IS
    'Configured number of fixed periods; zero means unlimited.';
COMMENT ON COLUMN billing_subscriptions.current_period_number IS
    'One-based current period number, advanced across idle periods.';
COMMENT ON COLUMN billing_subscriptions.expires_at IS
    'Final scheduled expiry for finite subscriptions; null for unlimited subscriptions.';
COMMENT ON COLUMN billing_subscription_periods.period_count IS
    'Immutable configured period-count snapshot for idempotent operation replay.';
COMMENT ON COLUMN billing_subscription_periods.period_number IS
    'Immutable one-based period number snapshot.';
COMMENT ON TABLE billing_subscription_operation_snapshots IS
    'Immutable period configuration for subscription operation ledger entries that have no period reference.';
