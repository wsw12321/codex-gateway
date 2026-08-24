-- Durable, non-secret ChatGPT upstream account metadata and attribution.
-- OAuth credentials remain owned by the isolated compatibility sidecar.

CREATE TABLE upstream_accounts (
    id TEXT PRIMARY KEY,
    masked_email TEXT NOT NULL DEFAULT '',
    plan TEXT NOT NULL DEFAULT 'unknown',
    status TEXT NOT NULL DEFAULT 'unavailable',
    last_synced_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT upstream_accounts_id_safe CHECK (
        id ~ '^[a-f0-9]{16}$'
    ),
    CONSTRAINT upstream_accounts_email_masked CHECK (
        masked_email = '' OR (
            char_length(masked_email) BETWEEN 7 AND 254
            AND masked_email ~ '^[A-Za-z0-9][*]{3}@[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$'
        )
    ),
    CONSTRAINT upstream_accounts_plan_safe CHECK (
        plan IN ('plus', 'pro', 'unknown')
    ),
    CONSTRAINT upstream_accounts_status_valid CHECK (
        status IN ('available', 'unavailable')
    ),
    CONSTRAINT upstream_accounts_time_order CHECK (
        updated_at >= created_at
    )
);

CREATE INDEX upstream_accounts_status_idx
    ON upstream_accounts (status, lower(masked_email), id);

COMMENT ON TABLE upstream_accounts IS
    'Non-secret stable upstream account indexes and masked display metadata. OAuth tokens and full email addresses must never be stored here.';
COMMENT ON COLUMN upstream_accounts.masked_email IS
    'Sidecar-provided masked email only; empty for a trace-created placeholder pending synchronization.';
COMMENT ON COLUMN upstream_accounts.last_synced_at IS
    'Last OAuth/auth metadata synchronization reported by the sidecar; availability observations use updated_at.';

ALTER TABLE usage_requests
    ADD COLUMN upstream_account_id TEXT,
    ADD CONSTRAINT usage_requests_upstream_account_fk
        FOREIGN KEY (upstream_account_id) REFERENCES upstream_accounts(id) ON DELETE RESTRICT;

CREATE INDEX usage_requests_upstream_account_time_idx
    ON usage_requests (upstream_account_id, requested_at DESC);

ALTER TABLE usage_daily
    ADD COLUMN upstream_account_id TEXT,
    ADD CONSTRAINT usage_daily_upstream_account_fk
        FOREIGN KEY (upstream_account_id) REFERENCES upstream_accounts(id) ON DELETE RESTRICT,
    DROP CONSTRAINT usage_daily_dimensions_key,
    ADD CONSTRAINT usage_daily_dimensions_key UNIQUE NULLS NOT DISTINCT
        (usage_day, user_id, device_id, api_key_id, project_id, upstream_account_id,
         model, endpoint, status_class, error_code);

CREATE INDEX usage_daily_upstream_account_day_idx
    ON usage_daily (upstream_account_id, usage_day DESC);

ALTER TABLE usage_monthly
    ADD COLUMN upstream_account_id TEXT,
    ADD CONSTRAINT usage_monthly_upstream_account_fk
        FOREIGN KEY (upstream_account_id) REFERENCES upstream_accounts(id) ON DELETE RESTRICT,
    DROP CONSTRAINT usage_monthly_dimensions_key,
    ADD CONSTRAINT usage_monthly_dimensions_key UNIQUE NULLS NOT DISTINCT
        (usage_month, user_id, device_id, api_key_id, project_id, upstream_account_id,
         model, endpoint, status_class, error_code);

CREATE INDEX usage_monthly_upstream_account_month_idx
    ON usage_monthly (upstream_account_id, usage_month DESC);

ALTER TABLE billing_ledger_entries
    ADD COLUMN upstream_account_id TEXT,
    ADD CONSTRAINT billing_ledger_upstream_account_fk
        FOREIGN KEY (upstream_account_id) REFERENCES upstream_accounts(id) ON DELETE RESTRICT;

CREATE INDEX billing_ledger_upstream_account_time_idx
    ON billing_ledger_entries (upstream_account_id, created_at DESC, id DESC)
    WHERE entry_type = 'usage_charge';

COMMENT ON COLUMN billing_ledger_entries.upstream_account_id IS
    'Immutable final upstream account attribution copied from terminal usage metadata.';
