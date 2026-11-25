-- Aggregation gateway, stage 2: tenant providers.
--
-- Ownership is no longer write-once. A removed credential releases its
-- account: the row keeps its last owner for history, but any later login of
-- the same upstream account may claim it. Only the Owner transfers an account
-- that still has a credential. Every ownership change runs through one store
-- function that names the reason in a transaction-local setting.
ALTER TABLE upstream_accounts ADD COLUMN credential_removed_at TIMESTAMPTZ;
COMMENT ON COLUMN upstream_accounts.credential_removed_at IS
    'Set when the credential is deleted. The account is released: no funding path uses it and the next login of it may claim it.';

-- Suppliers deleted before this migration kept their account row without a
-- supplier record; they are removed credentials.
UPDATE upstream_accounts a SET credential_removed_at = a.updated_at
WHERE a.credential_kind = 'supplier'
  AND NOT EXISTS (SELECT 1 FROM model_suppliers m WHERE m.account_id = a.id);

-- Tenants may run their own egress nodes. NULL is a platform (Owner) node.
ALTER TABLE egress_servers
    ADD COLUMN owner_user_id UUID REFERENCES users(id) ON DELETE RESTRICT;
CREATE INDEX egress_servers_owner_idx ON egress_servers(owner_user_id) WHERE owner_user_id IS NOT NULL;
COMMENT ON COLUMN egress_servers.owner_user_id IS
    'NULL: platform node. Otherwise the tenant that owns it; only accounts of the same owner bind to it.';

ALTER TABLE marketplace_settings
    ADD COLUMN max_egress_servers_per_user INTEGER NOT NULL DEFAULT 5
        CHECK (max_egress_servers_per_user BETWEEN 0 AND 32);

CREATE OR REPLACE FUNCTION guard_upstream_account_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.owner_user_id IS NOT DISTINCT FROM OLD.owner_user_id THEN
        RETURN NEW;
    END IF;
    IF COALESCE(current_setting('gateway.upstream_owner_change', true), '') NOT IN ('claim', 'transfer') THEN
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

-- An account binds only to a node of its own owner, or to the global egress.
CREATE FUNCTION guard_upstream_account_egress_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.node_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM upstream_accounts a JOIN egress_servers n ON n.id = NEW.node_id
        WHERE a.id = NEW.account_id AND n.owner_user_id IS NOT DISTINCT FROM a.owner_user_id) THEN
        RAISE EXCEPTION 'upstream account % cannot bind an egress node of another owner', NEW.account_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER upstream_account_egress_same_owner
    BEFORE INSERT OR UPDATE ON upstream_account_egress
    FOR EACH ROW EXECUTE FUNCTION guard_upstream_account_egress_owner();

-- A login or supplier creation may only choose a node of the person starting it.
CREATE FUNCTION guard_egress_registration_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.node_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM egress_servers n
        WHERE n.id = NEW.node_id AND n.owner_user_id IS NOT DISTINCT FROM NEW.tenant_user_id) THEN
        RAISE EXCEPTION 'egress registration % cannot choose a node of another owner', NEW.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER egress_registrations_same_owner
    BEFORE INSERT ON egress_registrations
    FOR EACH ROW EXECUTE FUNCTION guard_egress_registration_owner();

CREATE FUNCTION guard_egress_server_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.owner_user_id IS DISTINCT FROM OLD.owner_user_id THEN
        RAISE EXCEPTION 'egress node % owner is immutable', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER egress_servers_owner_immutable
    BEFORE UPDATE OF owner_user_id ON egress_servers
    FOR EACH ROW EXECUTE FUNCTION guard_egress_server_owner();
