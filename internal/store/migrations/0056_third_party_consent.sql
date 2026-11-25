-- Aggregation gateway: a buyer's consent to member-run API suppliers belongs
-- to the subscription.
--
-- A buyer confirms at purchase and renewal that a member-run API key supplier
-- sees the requests it serves. The seller may later change the plan's
-- provider scope without review, including adding such a supplier to a plan
-- that had none. From this migration a member-run supplier serves a seller
-- plan's subscription only while that subscription records the buyer's
-- consent; the buyer may give or withdraw it at any time. Existing
-- subscriptions start without it, so suppliers stop serving them until their
-- buyers consent (fail closed).
ALTER TABLE billing_subscriptions ADD COLUMN third_party_acknowledged_at TIMESTAMPTZ;
COMMENT ON COLUMN billing_subscriptions.third_party_acknowledged_at IS
    'When the buyer consented that member-run API suppliers of the seller plan serve this subscription. NULL: they do not.';
