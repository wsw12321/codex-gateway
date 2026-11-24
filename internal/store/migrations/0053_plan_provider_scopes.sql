-- Aggregation gateway, stage 3: plan provider scopes, funding rule 5 and
-- self-use.
--
-- A plan names the accounts its subscriptions may pay for: every account of
-- its seller ("all") or a listed subset ("selected"). The platform is the
-- seller of every plan that exists today. A subscription remembers the plan
-- it was bought from for good, so the plan's current scope keeps applying
-- after an Owner edit unbinds it from renewal.
--
-- Funding rule 5 extends rule 4 with two layer kinds ahead of the personal
-- layer: the user's own accounts (self-use, charged nothing) and one layer per
-- subscription whose plan restricts its accounts.

-- Stage 4 publishes seller plans; every existing plan is a platform plan.
ALTER TABLE billing_plans
    ADD COLUMN seller_user_id UUID REFERENCES users(id) ON DELETE RESTRICT;
CREATE INDEX billing_plans_seller_idx ON billing_plans(seller_user_id) WHERE seller_user_id IS NOT NULL;
COMMENT ON COLUMN billing_plans.seller_user_id IS
    'NULL: platform plan. Otherwise the member that publishes it; its subscriptions only pay for that member''s accounts.';

CREATE FUNCTION guard_billing_plan_seller() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.seller_user_id IS DISTINCT FROM OLD.seller_user_id THEN
        RAISE EXCEPTION 'billing plan % seller is immutable', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER billing_plans_seller_immutable
    BEFORE UPDATE OF seller_user_id ON billing_plans
    FOR EACH ROW EXECUTE FUNCTION guard_billing_plan_seller();

CREATE TABLE billing_plan_provider_scopes (
    plan_id UUID PRIMARY KEY REFERENCES billing_plans(id) ON DELETE CASCADE,
    mode TEXT NOT NULL CHECK (mode IN ('all', 'selected')),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by_user_id UUID REFERENCES users(id) ON DELETE SET NULL
);
COMMENT ON TABLE billing_plan_provider_scopes IS
    'Which accounts a plan''s subscriptions pay for. Read at every admission, so a change applies from the next request.';
INSERT INTO billing_plan_provider_scopes (plan_id, mode) SELECT id, 'all' FROM billing_plans;

CREATE TABLE billing_plan_provider_accounts (
    plan_id UUID NOT NULL REFERENCES billing_plan_provider_scopes(plan_id) ON DELETE CASCADE,
    account_id TEXT NOT NULL REFERENCES upstream_accounts(id) ON DELETE CASCADE,
    PRIMARY KEY (plan_id, account_id)
);
CREATE INDEX billing_plan_provider_accounts_account_idx ON billing_plan_provider_accounts(account_id);
COMMENT ON TABLE billing_plan_provider_accounts IS
    'Accounts of a "selected" scope. A listed account that later changes owner or is released stays listed but no longer serves the plan.';

-- A scope lists only accounts of the plan's seller that still hold a
-- credential: platform accounts for a platform plan.
CREATE FUNCTION guard_billing_plan_provider_account() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM billing_plans p JOIN upstream_accounts a ON a.id = NEW.account_id
        WHERE p.id = NEW.plan_id AND a.owner_user_id IS NOT DISTINCT FROM p.seller_user_id
          AND a.credential_removed_at IS NULL) THEN
        RAISE EXCEPTION 'upstream account % does not belong to the seller of plan %', NEW.account_id, NEW.plan_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER billing_plan_provider_accounts_seller
    BEFORE INSERT OR UPDATE ON billing_plan_provider_accounts
    FOR EACH ROW EXECUTE FUNCTION guard_billing_plan_provider_account();

-- The plan a subscription was bought from. plan_id is the renewal binding and
-- an Owner edit clears it; source_plan_id never changes.
ALTER TABLE billing_subscriptions
    ADD COLUMN source_plan_id UUID REFERENCES billing_plans(id) ON DELETE RESTRICT;
UPDATE billing_subscriptions SET source_plan_id = plan_id WHERE plan_id IS NOT NULL;
CREATE INDEX billing_subscriptions_source_plan_idx ON billing_subscriptions(source_plan_id) WHERE source_plan_id IS NOT NULL;
COMMENT ON COLUMN billing_subscriptions.source_plan_id IS
    'Plan this subscription was bought from; immutable. Its current provider scope decides which accounts the subscription pays for.';

CREATE FUNCTION guard_billing_subscription_source_plan() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.source_plan_id IS DISTINCT FROM OLD.source_plan_id THEN
        RAISE EXCEPTION 'billing subscription % source plan is immutable', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER billing_subscriptions_source_plan_immutable
    BEFORE UPDATE OF source_plan_id ON billing_subscriptions
    FOR EACH ROW EXECUTE FUNCTION guard_billing_subscription_source_plan();

ALTER TABLE billing_accounts
    ADD COLUMN self_use_enabled BOOLEAN NOT NULL DEFAULT true;
COMMENT ON COLUMN billing_accounts.self_use_enabled IS
    'While the marketplace is enabled, the user''s own accounts form the first funding layer and are charged nothing.';

-- Rule 5. A reservation may rest on layers alone: self-use has no funding
-- source, and a restricted subscription is a layer rather than a personal
-- source. A request served by the user's own account settles at zero charge
-- and zero uncovered cost: that is what self-use means, and the only rule 5
-- settlement that does not satisfy actual = charged + uncovered.
ALTER TABLE billing_reservations
    DROP CONSTRAINT billing_reservations_funding_rule_version_check,
    DROP CONSTRAINT billing_reservations_source_required,
    DROP CONSTRAINT billing_reservations_actuals_valid,
    ADD CONSTRAINT billing_reservations_funding_rule_version_check CHECK (funding_rule_version IN (1,2,3,4,5)),
    ADD CONSTRAINT billing_reservations_source_required CHECK (billing_mode='internal_zero'
        OR day_period_id IS NOT NULL OR week_period_id IS NOT NULL OR month_period_id IS NOT NULL OR cash_lot_cutoff IS NOT NULL
        OR (funding_rule_version>=2 AND group_period_id IS NOT NULL)
        OR (funding_rule_version>=3 AND jsonb_array_length(ordered_funding_sources)>0)
        OR funding_rule_version=5),
    ADD CONSTRAINT billing_reservations_actuals_valid CHECK (
        (actual_input_tokens IS NULL OR actual_input_tokens >= 0)
        AND (actual_cached_input_tokens IS NULL OR actual_cached_input_tokens >= 0)
        AND (actual_cache_write_tokens IS NULL OR actual_cache_write_tokens >= 0)
        AND (actual_output_tokens IS NULL OR actual_output_tokens >= 0)
        AND (actual_input_tokens IS NULL OR actual_cached_input_tokens IS NULL
             OR actual_cache_write_tokens IS NULL
             OR actual_cached_input_tokens + actual_cache_write_tokens <= actual_input_tokens)
        AND (actual_cost_usd IS NULL OR actual_cost_usd >= 0)
        AND (charged_usd IS NULL OR charged_usd >= 0)
        AND (uncovered_usd IS NULL OR uncovered_usd >= 0)
        AND (actual_cost_usd IS NULL OR charged_usd IS NULL OR uncovered_usd IS NULL
             OR actual_cost_usd = charged_usd + uncovered_usd
             OR (funding_rule_version = 5 AND charged_usd = 0 AND uncovered_usd = 0))
    );
COMMENT ON COLUMN billing_reservations.funding_rule_version IS
    'Rule 5 adds self-use and restricted subscription layers; a rule 5 settlement with nothing charged and nothing uncovered is self-use.';

-- Layer kinds. A rule 4 layer is a group or the personal layer; rule 5 adds
-- self-use and restricted subscription layers, which carry an account pool
-- without a group.
ALTER TABLE billing_reservation_layers
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'group',
    ADD COLUMN subscription_id UUID,
    ADD COLUMN subscription_period_id UUID;
UPDATE billing_reservation_layers SET kind = 'personal' WHERE group_id IS NULL;
ALTER TABLE billing_reservation_layers
    ALTER COLUMN kind DROP DEFAULT,
    DROP CONSTRAINT billing_reservation_layers_check1,
    ADD CONSTRAINT billing_reservation_layers_kind_valid CHECK (kind IN ('group', 'subscription', 'self', 'personal')),
    ADD CONSTRAINT billing_reservation_layers_kind_consistent CHECK (
        (kind = 'group' AND group_id IS NOT NULL AND NOT all_accounts AND subscription_id IS NULL)
        OR (kind = 'personal' AND group_id IS NULL AND all_accounts AND subscription_id IS NULL)
        OR (kind = 'self' AND group_id IS NULL AND NOT all_accounts AND subscription_id IS NULL)
        OR (kind = 'subscription' AND group_id IS NULL AND NOT all_accounts AND subscription_id IS NOT NULL)),
    ADD CONSTRAINT billing_reservation_layers_subscription_period_consistent CHECK (
        (subscription_id IS NULL) = (subscription_period_id IS NULL)),
    ADD CONSTRAINT billing_reservation_layers_subscription_fk
        FOREIGN KEY (subscription_period_id, subscription_id, user_id)
        REFERENCES billing_subscription_periods(id, subscription_id, user_id) ON DELETE RESTRICT;
DROP INDEX billing_reservation_layers_personal_key;
CREATE UNIQUE INDEX billing_reservation_layers_personal_key
    ON billing_reservation_layers(request_id) WHERE kind = 'personal';
CREATE UNIQUE INDEX billing_reservation_layers_self_key
    ON billing_reservation_layers(request_id) WHERE kind = 'self';
CREATE UNIQUE INDEX billing_reservation_layers_subscription_key
    ON billing_reservation_layers(request_id, subscription_id) WHERE subscription_id IS NOT NULL;
CREATE INDEX billing_reservation_layers_subscription_period_idx
    ON billing_reservation_layers(subscription_period_id) WHERE subscription_period_id IS NOT NULL;
