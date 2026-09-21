CREATE TABLE user_groups (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    limit_usd NUMERIC(30,12) NOT NULL CHECK (limit_usd >= 0),
    period TEXT NOT NULL CHECK (period IN ('day','week','month','custom')),
    custom_days INTEGER NOT NULL DEFAULT 0,
    starts_at TIMESTAMPTZ NOT NULL,
    current_period_id UUID,
    archived_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK ((period = 'custom' AND custom_days BETWEEN 1 AND 106751)
        OR (period <> 'custom' AND custom_days = 0))
);

CREATE TABLE group_usage_periods (
    id UUID PRIMARY KEY,
    group_id UUID NOT NULL REFERENCES user_groups(id) ON DELETE RESTRICT,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    limit_usd NUMERIC(30,12) NOT NULL CHECK (limit_usd >= 0),
    used_usd NUMERIC(30,12) NOT NULL DEFAULT 0 CHECK (used_usd >= 0),
    closed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (id, group_id),
    CHECK (ends_at > starts_at)
);
ALTER TABLE user_groups ADD CONSTRAINT user_groups_current_period_fk
    FOREIGN KEY (current_period_id, id) REFERENCES group_usage_periods(id, group_id) ON DELETE RESTRICT;
ALTER TABLE billing_accounts ADD COLUMN group_id UUID REFERENCES user_groups(id) ON DELETE RESTRICT;
CREATE INDEX billing_accounts_group_idx ON billing_accounts(group_id) WHERE group_id IS NOT NULL;
CREATE INDEX group_usage_periods_group_idx ON group_usage_periods(group_id, starts_at DESC);

ALTER TABLE billing_reservations
    ADD COLUMN group_id UUID REFERENCES user_groups(id) ON DELETE RESTRICT,
    ADD COLUMN group_period_id UUID,
    ADD CONSTRAINT billing_reservations_group_period_fk
        FOREIGN KEY (group_period_id, group_id) REFERENCES group_usage_periods(id, group_id) ON DELETE RESTRICT,
    ADD CONSTRAINT billing_reservations_group_consistent CHECK ((group_id IS NULL) = (group_period_id IS NULL));
ALTER TABLE billing_ledger_entries
    ADD COLUMN group_id UUID REFERENCES user_groups(id) ON DELETE RESTRICT,
    ADD COLUMN group_period_id UUID,
    ADD CONSTRAINT billing_ledger_group_period_fk
        FOREIGN KEY (group_period_id, group_id) REFERENCES group_usage_periods(id, group_id) ON DELETE RESTRICT,
    ADD CONSTRAINT billing_ledger_group_consistent CHECK ((group_id IS NULL) = (group_period_id IS NULL));
CREATE INDEX billing_ledger_group_period_idx ON billing_ledger_entries(group_period_id, user_id)
    WHERE group_period_id IS NOT NULL;

CREATE TABLE group_operations (
    operation_id TEXT PRIMARY KEY CHECK (char_length(operation_id) BETWEEN 1 AND 128),
    actor_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    action TEXT NOT NULL,
    request_fingerprint BYTEA NOT NULL,
    response JSONB,
    created_at TIMESTAMPTZ NOT NULL
);
COMMENT ON COLUMN billing_reservations.group_period_id IS
    'Admission-time group period; settlement uses this immutable binding even after membership or period changes.';
COMMENT ON COLUMN group_usage_periods.used_usd IS
    'Full settled request cost, including uncovered personal charges. In-flight requests may exceed the limit.';
