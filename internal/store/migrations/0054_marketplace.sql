-- Aggregation gateway, stage 4: seller plans, review, the plan marketplace
-- and the sale split.
--
-- A member publishes a plan (billing_plans.seller_user_id, stage 3). It is
-- sold only after the Owner approves it: draft -> pending -> approved or
-- rejected. Changing what a buyer pays for or receives (name, description,
-- price, allowance, period, models, minimum count) sends an approved or
-- rejected plan back to pending; the provider scope never does. Every plan
-- that exists today is a platform plan and stays approved.
ALTER TABLE billing_plans
    ADD COLUMN review_status TEXT NOT NULL DEFAULT 'approved'
        CHECK (review_status IN ('draft', 'pending', 'approved', 'rejected')),
    ADD COLUMN review_note TEXT NOT NULL DEFAULT '' CHECK (char_length(review_note) <= 500),
    ADD COLUMN reviewed_by_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN reviewed_at TIMESTAMPTZ,
    ADD COLUMN submitted_at TIMESTAMPTZ,
    ADD COLUMN description TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 1000),
    ADD COLUMN admin_suspended BOOLEAN NOT NULL DEFAULT false,
    ADD CONSTRAINT billing_plans_platform_approved CHECK (
        seller_user_id IS NOT NULL OR (review_status = 'approved' AND NOT admin_suspended));
CREATE INDEX billing_plans_review_idx ON billing_plans(submitted_at, id)
    WHERE review_status = 'pending' AND deleted_at IS NULL;
COMMENT ON COLUMN billing_plans.review_status IS
    'Seller plans: sold only while approved. Platform plans are always approved.';
COMMENT ON COLUMN billing_plans.admin_suspended IS
    'Owner takedown of a seller plan. The seller cannot list it again until the Owner lifts it.';

-- One row per plan sale or renewal of a seller plan. Amounts are exact: net is
-- what the seller receives, commission what the platform keeps at the rate in
-- force at the sale.
CREATE TABLE marketplace_sales (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    operation_id UUID NOT NULL UNIQUE,
    kind TEXT NOT NULL CHECK (kind IN ('purchase', 'renewal')),
    plan_id UUID NOT NULL REFERENCES billing_plans(id) ON DELETE RESTRICT,
    plan_version BIGINT NOT NULL CHECK (plan_version > 0),
    seller_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    buyer_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    -- What the seller sees instead of the buyer: the order in which buyers
    -- first bought from this seller.
    buyer_number INTEGER NOT NULL CHECK (buyer_number > 0),
    subscription_id UUID NOT NULL,
    period_count INTEGER NOT NULL CHECK (period_count BETWEEN 1 AND 99),
    gross_usd NUMERIC(30,12) NOT NULL CHECK (gross_usd > 0),
    commission_rate NUMERIC(7,6) NOT NULL CHECK (commission_rate BETWEEN 0 AND 1),
    commission_usd NUMERIC(30,12) NOT NULL CHECK (commission_usd >= 0),
    net_usd NUMERIC(30,12) NOT NULL CHECK (net_usd >= 0),
    buyer_ledger_entry_id BIGINT REFERENCES billing_ledger_entries(id) ON DELETE SET NULL,
    seller_ledger_entry_id BIGINT REFERENCES billing_ledger_entries(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (gross_usd = commission_usd + net_usd),
    CHECK (seller_user_id <> buyer_user_id)
);
CREATE INDEX marketplace_sales_seller_idx ON marketplace_sales(seller_user_id, created_at DESC, id DESC);
CREATE INDEX marketplace_sales_buyer_idx ON marketplace_sales(seller_user_id, buyer_user_id);
CREATE INDEX marketplace_sales_plan_idx ON marketplace_sales(plan_id, created_at DESC);
CREATE INDEX marketplace_sales_created_idx ON marketplace_sales(created_at DESC, id DESC);
COMMENT ON TABLE marketplace_sales IS
    'Seller plan sales. Sellers see buyers only as buyer_number; ledger references clear when information cleanup removes the entries.';

-- The seller's net becomes a cash lot of its own, recorded as plan_sale_income.
ALTER TABLE billing_ledger_entries DROP CONSTRAINT billing_ledger_entry_type_valid,
    ADD CONSTRAINT billing_ledger_entry_type_valid CHECK (entry_type IN (
        'recharge_rate','recharge','adjustment','subscription_set','subscription_disable',
        'subscription_renewal','usage_charge','model_multiplier','plan_create','plan_update',
        'plan_delete','plan_purchase','plan_renewal','model_price','subscription_preferences',
        'cash_preferences','cash_models','plan_sale_income'));
