-- Gateway-owned quota safety guard for Codex and Claude upstream accounts.
-- When the remaining share of the five-hour or weekly window falls to the
-- Owner's safety percentage, the account leaves allocation until five minutes
-- after that window resets. CPA's own quota lock is separate and unchanged.
CREATE TABLE upstream_quota_guard_settings (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    safety_percent SMALLINT NOT NULL DEFAULT 5 CHECK (safety_percent BETWEEN 0 AND 50),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO upstream_quota_guard_settings(singleton) VALUES (true);

-- One row per guarded account, created on its first check or manual change.
-- The row is effectively limited only while restore_at is NULL or in the
-- future, so automatic recovery needs no timer and survives restarts.
-- Account metadata synchronization never writes this table.
CREATE TABLE upstream_quota_guard_states (
    account_id TEXT PRIMARY KEY REFERENCES upstream_accounts(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'available' CHECK (status IN ('available', 'limited')),
    source TEXT NOT NULL DEFAULT 'auto' CHECK (source IN ('auto', 'manual')),
    limited_window TEXT CHECK (limited_window IN ('five_hour', 'seven_day')),
    reset_at TIMESTAMPTZ,
    restore_at TIMESTAMPTZ,
    changed_at TIMESTAMPTZ,
    observed_at TIMESTAMPTZ,
    five_hour_used_percent SMALLINT CHECK (five_hour_used_percent BETWEEN 0 AND 100),
    five_hour_reset_at TIMESTAMPTZ,
    seven_day_used_percent SMALLINT CHECK (seven_day_used_percent BETWEEN 0 AND 100),
    seven_day_reset_at TIMESTAMPTZ,
    auto_checked_at TIMESTAMPTZ,
    next_check_at TIMESTAMPTZ,
    CONSTRAINT upstream_quota_guard_available_clear CHECK (
        status = 'limited' OR (limited_window IS NULL AND reset_at IS NULL AND restore_at IS NULL)
    ),
    CONSTRAINT upstream_quota_guard_restore_after_reset CHECK (
        reset_at IS NULL OR restore_at IS NULL OR restore_at > reset_at
    )
);

COMMENT ON TABLE upstream_quota_guard_states IS
    'Gateway-owned quota guard per Codex/Claude account: last observed five-hour and weekly usage, the limited state and its automatic restore time, and the automatic check schedule. No credentials or upstream text.';
COMMENT ON COLUMN upstream_quota_guard_states.auto_checked_at IS
    'Last automatic quota check attempt, successful or not. Automatic checks of one account are at least ten minutes apart.';
