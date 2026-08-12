-- Exact USD billing, rolling subscriptions, and immutable billing history.
-- Existing users intentionally start with a zero cash balance. Historical usage
-- is not backfilled into billing reservations and is never charged retroactively.

CREATE TABLE billing_settings (
    singleton BOOLEAN PRIMARY KEY DEFAULT true,
    usd_per_cny NUMERIC(30,12) NOT NULL DEFAULT 1.000000000000,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by_user_id UUID REFERENCES users(id) ON DELETE RESTRICT,
    CONSTRAINT billing_settings_singleton CHECK (singleton),
    CONSTRAINT billing_settings_rate_positive CHECK (usd_per_cny > 0)
);

INSERT INTO billing_settings (singleton) VALUES (true);

CREATE TABLE billing_accounts (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE RESTRICT,
    balance_usd NUMERIC(30,12) NOT NULL DEFAULT 0.000000000000,
    next_cash_lot_sequence BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_accounts_balance_nonnegative CHECK (balance_usd >= 0),
    CONSTRAINT billing_accounts_sequence_positive CHECK (next_cash_lot_sequence > 0)
);

CREATE TABLE billing_operations (
    operation_id UUID PRIMARY KEY,
    operation_type TEXT NOT NULL,
    actor_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    target_user_id UUID REFERENCES users(id) ON DELETE RESTRICT,
    reason TEXT NOT NULL,
    request_fingerprint BYTEA NOT NULL,
    result_ledger_entry_id BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_operations_type_valid CHECK (
        operation_type IN ('recharge_rate', 'recharge', 'adjustment',
                           'subscription_set', 'subscription_disable')
    ),
    CONSTRAINT billing_operations_reason_valid CHECK (
        char_length(btrim(reason)) BETWEEN 1 AND 500
    ),
    CONSTRAINT billing_operations_fingerprint_length CHECK (
        octet_length(request_fingerprint) = 32
    ),
    CONSTRAINT billing_operations_target_consistent CHECK (
        (operation_type = 'recharge_rate' AND target_user_id IS NULL)
        OR (operation_type <> 'recharge_rate' AND target_user_id IS NOT NULL)
    )
);

CREATE INDEX billing_operations_target_idx
    ON billing_operations (target_user_id, created_at DESC);

CREATE TABLE billing_subscriptions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    tier TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT true,
    allowance_usd NUMERIC(30,12) NOT NULL,
    current_period_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at TIMESTAMPTZ,
    CONSTRAINT billing_subscriptions_tier_valid CHECK (tier IN ('day', 'week', 'month')),
    CONSTRAINT billing_subscriptions_allowance_positive CHECK (allowance_usd > 0),
    CONSTRAINT billing_subscriptions_enabled_consistent CHECK (
        (enabled AND disabled_at IS NULL) OR (NOT enabled AND disabled_at IS NOT NULL)
    ),
    UNIQUE (user_id, tier),
    UNIQUE (id, user_id, tier)
);

CREATE TABLE billing_subscription_periods (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscription_id UUID NOT NULL,
    user_id UUID NOT NULL,
    tier TEXT NOT NULL,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    allowance_usd NUMERIC(30,12) NOT NULL,
    remaining_usd NUMERIC(30,12) NOT NULL,
    forfeited_usd NUMERIC(30,12) NOT NULL DEFAULT 0.000000000000,
    closed_at TIMESTAMPTZ,
    close_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_periods_subscription_fk
        FOREIGN KEY (subscription_id, user_id, tier)
        REFERENCES billing_subscriptions(id, user_id, tier) ON DELETE RESTRICT,
    CONSTRAINT billing_periods_tier_valid CHECK (tier IN ('day', 'week', 'month')),
    CONSTRAINT billing_periods_duration_valid CHECK (
        (tier = 'day' AND ends_at = starts_at + INTERVAL '24 hours')
        OR (tier = 'week' AND ends_at = starts_at + INTERVAL '7 days')
        OR (tier = 'month' AND ends_at = starts_at + INTERVAL '31 days')
    ),
    CONSTRAINT billing_periods_amount_valid CHECK (
        allowance_usd > 0 AND remaining_usd >= 0 AND remaining_usd <= allowance_usd
        AND forfeited_usd >= 0
        AND remaining_usd + forfeited_usd <= allowance_usd
    ),
    CONSTRAINT billing_periods_close_consistent CHECK (
        (closed_at IS NULL AND close_reason IS NULL)
        OR (closed_at IS NOT NULL AND closed_at >= starts_at
            AND char_length(btrim(close_reason)) > 0)
    ),
    UNIQUE (id, tier),
    UNIQUE (id, user_id, tier),
    UNIQUE (id, subscription_id, user_id, tier)
);

ALTER TABLE billing_subscriptions
    ADD CONSTRAINT billing_subscriptions_current_period_fk
    FOREIGN KEY (current_period_id, id, user_id, tier)
    REFERENCES billing_subscription_periods(id, subscription_id, user_id, tier)
    ON DELETE RESTRICT;

CREATE INDEX billing_periods_user_time_idx
    ON billing_subscription_periods (user_id, starts_at DESC);
CREATE INDEX billing_periods_open_idx
    ON billing_subscription_periods (subscription_id, ends_at)
    WHERE closed_at IS NULL;

CREATE TABLE billing_ledger_entries (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id UUID REFERENCES users(id) ON DELETE RESTRICT,
    operation_id UUID UNIQUE REFERENCES billing_operations(operation_id) ON DELETE RESTRICT,
    entry_type TEXT NOT NULL,
    amount_usd NUMERIC(30,12) NOT NULL DEFAULT 0.000000000000,
    cash_delta_usd NUMERIC(30,12) NOT NULL DEFAULT 0.000000000000,
    balance_after_usd NUMERIC(30,12),
    cny_amount NUMERIC(30,12),
    usd_per_cny_snapshot NUMERIC(30,12),
    subscription_tier TEXT,
    subscription_period_id UUID REFERENCES billing_subscription_periods(id) ON DELETE RESTRICT,
    request_id TEXT,
    model TEXT,
    input_tokens BIGINT,
    cached_input_tokens BIGINT,
    output_tokens BIGINT,
    actual_cost_usd NUMERIC(30,12),
    charged_usd NUMERIC(30,12),
    uncovered_usd NUMERIC(30,12),
    reason TEXT NOT NULL DEFAULT '',
    actor_user_id UUID REFERENCES users(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_ledger_entry_type_valid CHECK (
        entry_type IN ('recharge_rate', 'recharge', 'adjustment',
                       'subscription_set', 'subscription_disable',
                       'subscription_renewal', 'usage_charge')
    ),
    CONSTRAINT billing_ledger_balance_nonnegative CHECK (
        balance_after_usd IS NULL OR balance_after_usd >= 0
    ),
    CONSTRAINT billing_ledger_cny_nonnegative CHECK (cny_amount IS NULL OR cny_amount >= 0),
    CONSTRAINT billing_ledger_rate_positive CHECK (
        usd_per_cny_snapshot IS NULL OR usd_per_cny_snapshot > 0
    ),
    CONSTRAINT billing_ledger_tier_valid CHECK (
        subscription_tier IS NULL OR subscription_tier IN ('day', 'week', 'month')
    ),
    CONSTRAINT billing_ledger_tokens_nonnegative CHECK (
        (input_tokens IS NULL OR input_tokens >= 0)
        AND (cached_input_tokens IS NULL OR cached_input_tokens >= 0)
        AND (output_tokens IS NULL OR output_tokens >= 0)
        AND (input_tokens IS NULL OR cached_input_tokens IS NULL OR cached_input_tokens <= input_tokens)
    ),
    CONSTRAINT billing_ledger_costs_nonnegative CHECK (
        (actual_cost_usd IS NULL OR actual_cost_usd >= 0)
        AND (charged_usd IS NULL OR charged_usd >= 0)
        AND (uncovered_usd IS NULL OR uncovered_usd >= 0)
    ),
    CONSTRAINT billing_ledger_request_id_valid CHECK (
        request_id IS NULL OR char_length(request_id) BETWEEN 16 AND 128
    ),
    UNIQUE (id, operation_id),
    UNIQUE (id, user_id)
);

ALTER TABLE billing_operations
    ADD CONSTRAINT billing_operations_result_ledger_fk
    FOREIGN KEY (result_ledger_entry_id, operation_id)
    REFERENCES billing_ledger_entries(id, operation_id)
    ON DELETE RESTRICT;

CREATE UNIQUE INDEX billing_ledger_usage_request_key
    ON billing_ledger_entries (request_id) WHERE entry_type = 'usage_charge';
CREATE UNIQUE INDEX billing_ledger_period_event_key
    ON billing_ledger_entries (subscription_period_id, entry_type)
    WHERE subscription_period_id IS NOT NULL
      AND entry_type IN ('subscription_set', 'subscription_renewal');
CREATE INDEX billing_ledger_user_time_idx
    ON billing_ledger_entries (user_id, created_at DESC, id DESC);

CREATE TABLE billing_cash_credit_lots (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    lot_sequence BIGINT NOT NULL,
    source_ledger_entry_id BIGINT NOT NULL UNIQUE,
    original_usd NUMERIC(30,12) NOT NULL,
    remaining_usd NUMERIC(30,12) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_cash_lots_sequence_positive CHECK (lot_sequence > 0),
    CONSTRAINT billing_cash_lots_amount_valid CHECK (
        original_usd > 0 AND remaining_usd >= 0 AND remaining_usd <= original_usd
    ),
    CONSTRAINT billing_cash_lots_source_user_fk
        FOREIGN KEY (source_ledger_entry_id, user_id)
        REFERENCES billing_ledger_entries(id, user_id) ON DELETE RESTRICT,
    UNIQUE (user_id, lot_sequence)
);

CREATE INDEX billing_cash_lots_available_idx
    ON billing_cash_credit_lots (user_id, lot_sequence) WHERE remaining_usd > 0;

CREATE TABLE billing_reservations (
    request_id TEXT PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    api_key_id UUID NOT NULL,
    requested_model TEXT NOT NULL,
    input_usd_per_million NUMERIC(30,12) NOT NULL,
    cached_input_usd_per_million NUMERIC(30,12) NOT NULL,
    output_usd_per_million NUMERIC(30,12) NOT NULL,
    day_period_id UUID REFERENCES billing_subscription_periods(id) ON DELETE RESTRICT,
    week_period_id UUID REFERENCES billing_subscription_periods(id) ON DELETE RESTRICT,
    month_period_id UUID REFERENCES billing_subscription_periods(id) ON DELETE RESTRICT,
    day_period_tier TEXT GENERATED ALWAYS AS ('day'::TEXT) STORED,
    week_period_tier TEXT GENERATED ALWAYS AS ('week'::TEXT) STORED,
    month_period_tier TEXT GENERATED ALWAYS AS ('month'::TEXT) STORED,
    cash_lot_cutoff BIGINT,
    state TEXT NOT NULL DEFAULT 'reserved',
    actual_input_tokens BIGINT,
    actual_cached_input_tokens BIGINT,
    actual_output_tokens BIGINT,
    actual_cost_usd NUMERIC(30,12),
    charged_usd NUMERIC(30,12),
    uncovered_usd NUMERIC(30,12),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at TIMESTAMPTZ,
    CONSTRAINT billing_reservations_key_owner_fk FOREIGN KEY (api_key_id, user_id)
        REFERENCES api_keys(id, user_id) ON DELETE RESTRICT,
    CONSTRAINT billing_reservations_day_period_fk
        FOREIGN KEY (day_period_id, user_id, day_period_tier)
        REFERENCES billing_subscription_periods(id, user_id, tier) ON DELETE RESTRICT,
    CONSTRAINT billing_reservations_week_period_fk
        FOREIGN KEY (week_period_id, user_id, week_period_tier)
        REFERENCES billing_subscription_periods(id, user_id, tier) ON DELETE RESTRICT,
    CONSTRAINT billing_reservations_month_period_fk
        FOREIGN KEY (month_period_id, user_id, month_period_tier)
        REFERENCES billing_subscription_periods(id, user_id, tier) ON DELETE RESTRICT,
    CONSTRAINT billing_reservations_request_id_valid CHECK (
        char_length(request_id) BETWEEN 16 AND 128
    ),
    CONSTRAINT billing_reservations_model_valid CHECK (
        char_length(requested_model) BETWEEN 1 AND 128
    ),
    CONSTRAINT billing_reservations_prices_nonnegative CHECK (
        input_usd_per_million >= 0 AND cached_input_usd_per_million >= 0
        AND output_usd_per_million >= 0
    ),
    CONSTRAINT billing_reservations_cutoff_positive CHECK (
        cash_lot_cutoff IS NULL OR cash_lot_cutoff > 0
    ),
    CONSTRAINT billing_reservations_source_required CHECK (
        day_period_id IS NOT NULL OR week_period_id IS NOT NULL
        OR month_period_id IS NOT NULL OR cash_lot_cutoff IS NOT NULL
    ),
    CONSTRAINT billing_reservations_state_valid CHECK (
        state IN ('reserved', 'settled', 'released')
    ),
    CONSTRAINT billing_reservations_actuals_valid CHECK (
        (actual_input_tokens IS NULL OR actual_input_tokens >= 0)
        AND (actual_cached_input_tokens IS NULL OR actual_cached_input_tokens >= 0)
        AND (actual_output_tokens IS NULL OR actual_output_tokens >= 0)
        AND (actual_input_tokens IS NULL OR actual_cached_input_tokens IS NULL
             OR actual_cached_input_tokens <= actual_input_tokens)
        AND (actual_cost_usd IS NULL OR actual_cost_usd >= 0)
        AND (charged_usd IS NULL OR charged_usd >= 0)
        AND (uncovered_usd IS NULL OR uncovered_usd >= 0)
        AND (actual_cost_usd IS NULL OR charged_usd IS NULL OR uncovered_usd IS NULL
             OR actual_cost_usd = charged_usd + uncovered_usd)
    ),
    CONSTRAINT billing_reservations_terminal_consistent CHECK (
        (state = 'reserved' AND actual_input_tokens IS NULL
            AND actual_cached_input_tokens IS NULL AND actual_output_tokens IS NULL
            AND actual_cost_usd IS NULL AND charged_usd IS NULL
            AND uncovered_usd IS NULL AND settled_at IS NULL)
        OR (state = 'released' AND actual_input_tokens IS NULL
            AND actual_cached_input_tokens IS NULL AND actual_output_tokens IS NULL
            AND actual_cost_usd IS NULL AND charged_usd IS NULL
            AND uncovered_usd IS NULL AND settled_at IS NOT NULL)
        OR (state = 'settled' AND actual_input_tokens IS NOT NULL
            AND actual_cached_input_tokens IS NOT NULL AND actual_output_tokens IS NOT NULL
            AND actual_cost_usd IS NOT NULL AND charged_usd IS NOT NULL
            AND uncovered_usd IS NOT NULL AND settled_at IS NOT NULL)
    )
);

CREATE INDEX billing_reservations_unsettled_idx
    ON billing_reservations (created_at) WHERE state = 'reserved';
CREATE INDEX billing_reservations_user_time_idx
    ON billing_reservations (user_id, created_at DESC);

CREATE TABLE billing_charge_allocations (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id TEXT NOT NULL REFERENCES billing_reservations(request_id) ON DELETE RESTRICT,
    allocation_order INTEGER NOT NULL,
    source_type TEXT NOT NULL,
    subscription_period_id UUID REFERENCES billing_subscription_periods(id) ON DELETE RESTRICT,
    cash_credit_lot_id BIGINT REFERENCES billing_cash_credit_lots(id) ON DELETE RESTRICT,
    amount_usd NUMERIC(30,12) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_allocations_order_positive CHECK (allocation_order > 0),
    CONSTRAINT billing_allocations_source_valid CHECK (
        source_type IN ('day', 'week', 'month', 'cash')
    ),
    CONSTRAINT billing_allocations_amount_positive CHECK (amount_usd > 0),
    CONSTRAINT billing_allocations_source_consistent CHECK (
        (source_type = 'cash' AND cash_credit_lot_id IS NOT NULL
         AND subscription_period_id IS NULL)
        OR (source_type <> 'cash' AND subscription_period_id IS NOT NULL
            AND cash_credit_lot_id IS NULL)
    ),
    CONSTRAINT billing_allocations_period_tier_fk
        FOREIGN KEY (subscription_period_id, source_type)
        REFERENCES billing_subscription_periods(id, tier) ON DELETE RESTRICT,
    UNIQUE (request_id, allocation_order)
);

CREATE INDEX billing_allocations_request_idx
    ON billing_charge_allocations (request_id, allocation_order);

-- The billing ledger is append-only. Corrections are represented by explicit
-- positive or negative adjustment entries instead of modifying history.
CREATE OR REPLACE FUNCTION gateway_reject_billing_ledger_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'billing ledger entries are immutable' USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER billing_ledger_entries_immutable
    BEFORE UPDATE OR DELETE ON billing_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION gateway_reject_billing_ledger_mutation();

CREATE OR REPLACE FUNCTION gateway_create_billing_account() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO billing_accounts (user_id) VALUES (NEW.id) ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$;

CREATE TRIGGER users_create_billing_account
    AFTER INSERT ON users FOR EACH ROW EXECUTE FUNCTION gateway_create_billing_account();

INSERT INTO billing_accounts (user_id)
SELECT id FROM users ON CONFLICT DO NOTHING;

CREATE TRIGGER billing_accounts_set_updated_at BEFORE UPDATE ON billing_accounts
    FOR EACH ROW EXECUTE FUNCTION gateway_set_updated_at();
CREATE TRIGGER billing_subscriptions_set_updated_at BEFORE UPDATE ON billing_subscriptions
    FOR EACH ROW EXECUTE FUNCTION gateway_set_updated_at();

COMMENT ON TABLE billing_operations IS
    'Idempotency records only. request_fingerprint is a SHA-256 digest of allowlisted billing inputs; raw HTTP payloads and secrets are forbidden.';
COMMENT ON TABLE billing_ledger_entries IS
    'Immutable billing metadata only. Prompt and response content, credentials, cookies, API keys and OAuth tokens are forbidden.';
COMMENT ON TABLE billing_reservations IS
    'Request billing metadata and admission-time price/source bindings only; request and response content is forbidden.';
