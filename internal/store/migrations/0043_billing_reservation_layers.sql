-- Funding rule 4 snapshots every usable funding source at admission as an
-- ordered layer (groups by membership priority, then one personal layer), and
-- the account that actually served the request selects the paying layer at
-- settlement. Rules 1-3 keep their existing settlement.
ALTER TABLE billing_reservations
    DROP CONSTRAINT billing_reservations_funding_rule_version_check,
    DROP CONSTRAINT billing_reservations_source_required,
    ADD CONSTRAINT billing_reservations_funding_rule_version_check CHECK (funding_rule_version IN (1,2,3,4)),
    ADD CONSTRAINT billing_reservations_source_required CHECK (billing_mode='internal_zero'
        OR day_period_id IS NOT NULL OR week_period_id IS NOT NULL OR month_period_id IS NOT NULL OR cash_lot_cutoff IS NOT NULL
        OR (funding_rule_version>=2 AND group_period_id IS NOT NULL)
        OR (funding_rule_version>=3 AND jsonb_array_length(ordered_funding_sources)>0));

COMMENT ON COLUMN billing_reservations.group_id IS
    'Rule 4: provisional first group layer until settlement binds the layer that paid.';

CREATE TABLE billing_reservation_layers (
    request_id TEXT NOT NULL,
    position INTEGER NOT NULL CHECK (position BETWEEN 1 AND 65),
    user_id UUID NOT NULL,
    group_id UUID,
    group_period_id UUID,
    all_accounts BOOLEAN NOT NULL,
    account_ids TEXT[] NOT NULL,
    PRIMARY KEY (request_id, position),
    FOREIGN KEY (request_id, user_id) REFERENCES billing_reservations(request_id, user_id) ON DELETE CASCADE,
    FOREIGN KEY (group_period_id, group_id) REFERENCES group_usage_periods(id, group_id) ON DELETE RESTRICT,
    CHECK ((group_id IS NULL) = (group_period_id IS NULL)),
    -- The personal layer is unrestricted (per-user eligibility still applies);
    -- a group layer lists the admission-time pool the user may use.
    CHECK (all_accounts = (group_id IS NULL)),
    CHECK ((all_accounts AND cardinality(account_ids) = 0)
        OR (NOT all_accounts AND cardinality(account_ids) BETWEEN 1 AND 256
            AND array_to_string(account_ids, ',') ~ '^[a-f0-9]{16}(,[a-f0-9]{16})*$'))
);
CREATE UNIQUE INDEX billing_reservation_layers_group_key
    ON billing_reservation_layers(request_id, group_id) WHERE group_id IS NOT NULL;
CREATE UNIQUE INDEX billing_reservation_layers_personal_key
    ON billing_reservation_layers(request_id) WHERE group_id IS NULL;
CREATE INDEX billing_reservation_layers_period_idx
    ON billing_reservation_layers(group_period_id) WHERE group_period_id IS NOT NULL;
