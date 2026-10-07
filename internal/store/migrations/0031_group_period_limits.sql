-- Existing groups keep their exact current period and all accumulated usage.
-- The saved period (not the original group anchor) becomes the single term.
ALTER TABLE user_groups
    ADD COLUMN period_count INTEGER NOT NULL DEFAULT 1 CHECK (period_count BETWEEN 0 AND 99),
    ADD COLUMN current_period_number INTEGER NOT NULL DEFAULT 1 CHECK (current_period_number >= 1),
    ADD COLUMN expires_at TIMESTAMPTZ;

UPDATE user_groups g SET expires_at=p.ends_at
FROM group_usage_periods p WHERE p.id=g.current_period_id;

ALTER TABLE user_groups
    ADD CONSTRAINT user_groups_period_number_valid
        CHECK (period_count=0 OR current_period_number<=period_count),
    ADD CONSTRAINT user_groups_expiration_valid
        CHECK ((period_count=0 AND expires_at IS NULL) OR (period_count>0 AND expires_at IS NOT NULL));

COMMENT ON COLUMN user_groups.period_count IS
    'Total cycles in this term, 0 for unlimited; changing only the count preserves the current period and usage.';
COMMENT ON COLUMN user_groups.current_period_number IS
    'One-based cycle number including idle cycles skipped before expiry; an explicit schedule reset starts at 1.';
COMMENT ON COLUMN user_groups.expires_at IS
    'Exclusive final deadline derived from the saved current period end; null only for unlimited cycles.';
