-- Aggregation gateway, stage 5: permanently deleting a member who once held
-- tenant resources.
--
-- A released account (credential removed) keeps its last owner for usage
-- history. When information management permanently deletes that owner, the
-- account loses its owner and records when that happened. It still serves
-- nobody, it is in nobody's management scope (the platform's included), and
-- the next login of the same upstream account claims it like any released
-- account. Live accounts, egress nodes, plans and sales still block the
-- deletion; the store reports them as deletion blockers.
ALTER TABLE upstream_accounts
    ADD COLUMN owner_deleted_at TIMESTAMPTZ,
    ADD CONSTRAINT upstream_accounts_owner_deleted_released CHECK (
        owner_deleted_at IS NULL OR (owner_user_id IS NULL AND credential_removed_at IS NOT NULL));
COMMENT ON COLUMN upstream_accounts.owner_deleted_at IS
    'Set when the last owner of a released account was permanently deleted. Such an account belongs to no one, not the platform either, until a login claims it.';

CREATE OR REPLACE FUNCTION guard_upstream_account_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    reason TEXT := COALESCE(current_setting('gateway.upstream_owner_change', true), '');
BEGIN
    IF NEW.owner_user_id IS NOT DISTINCT FROM OLD.owner_user_id THEN
        RETURN NEW;
    END IF;
    IF reason = 'owner_deleted' THEN
        -- Only a released account loses its owner, and it is marked so that
        -- it never reads as a platform account.
        IF NEW.owner_user_id IS NOT NULL OR OLD.credential_removed_at IS NULL
           OR NEW.credential_removed_at IS NULL OR NEW.owner_deleted_at IS NULL THEN
            RAISE EXCEPTION 'upstream account % loses its owner only when it is released and its owner is deleted', OLD.id
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF reason NOT IN ('claim', 'transfer') THEN
        RAISE EXCEPTION 'upstream account % owner changes only through a claim or an Owner transfer', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.owner_user_id IS NOT NULL AND (
        EXISTS (SELECT 1 FROM group_upstream_accounts WHERE upstream_account_id = OLD.id)
        OR EXISTS (SELECT 1 FROM upstream_account_users WHERE upstream_account_id = OLD.id)
        OR EXISTS (SELECT 1 FROM user_upstream_access_accounts WHERE upstream_account_id = OLD.id)) THEN
        RAISE EXCEPTION 'upstream account % is in use by a platform-only structure', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM upstream_account_egress b JOIN egress_servers n ON n.id = b.node_id
               WHERE b.account_id = OLD.id AND n.owner_user_id IS DISTINCT FROM NEW.owner_user_id) THEN
        RAISE EXCEPTION 'upstream account % is bound to an egress node of another owner', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

-- User deletion and its candidate list look sales up by buyer.
CREATE INDEX marketplace_sales_buyer_user_idx ON marketplace_sales(buyer_user_id);
