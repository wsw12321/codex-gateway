-- Aggregation gateway, stage 1: upstream account ownership.
-- An account with owner_user_id NULL is a platform (Owner) account; every
-- existing account stays a platform account. A tenant account belongs to one
-- user and is never part of a platform-only structure: group pools, exclusive
-- grants and per-user account scopes. Nothing creates tenant accounts yet.
ALTER TABLE upstream_accounts
    ADD COLUMN owner_user_id UUID REFERENCES users(id) ON DELETE RESTRICT,
    ADD COLUMN admin_suspended BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX upstream_accounts_owner_idx
    ON upstream_accounts(owner_user_id, provider) WHERE owner_user_id IS NOT NULL;

COMMENT ON COLUMN upstream_accounts.owner_user_id IS
    'NULL: platform account. Otherwise the tenant that owns it; set once and never changed.';
COMMENT ON COLUMN upstream_accounts.admin_suspended IS
    'Owner moderation hold on a tenant account. The tenant cannot lift it.';

-- A login or import started by a tenant commits its account to that tenant.
ALTER TABLE egress_registrations
    ADD COLUMN tenant_user_id UUID REFERENCES users(id) ON DELETE CASCADE;

CREATE TABLE marketplace_settings (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    enabled BOOLEAN NOT NULL DEFAULT false,
    commission_rate NUMERIC(7,6) NOT NULL DEFAULT 0 CHECK (commission_rate >= 0 AND commission_rate <= 1),
    max_accounts_per_user INTEGER NOT NULL DEFAULT 20 CHECK (max_accounts_per_user BETWEEN 0 AND 256),
    max_plans_per_user INTEGER NOT NULL DEFAULT 20 CHECK (max_plans_per_user BETWEEN 0 AND 100),
    allow_member_suppliers BOOLEAN NOT NULL DEFAULT false,
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_by_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO marketplace_settings(singleton) VALUES (true);

CREATE FUNCTION reject_tenant_upstream_account() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM upstream_accounts
               WHERE id = NEW.upstream_account_id AND owner_user_id IS NOT NULL) THEN
        RAISE EXCEPTION 'tenant upstream account % cannot join a platform-only structure', NEW.upstream_account_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER group_upstream_accounts_platform_only
    BEFORE INSERT OR UPDATE ON group_upstream_accounts
    FOR EACH ROW EXECUTE FUNCTION reject_tenant_upstream_account();
CREATE TRIGGER upstream_account_users_platform_only
    BEFORE INSERT OR UPDATE ON upstream_account_users
    FOR EACH ROW EXECUTE FUNCTION reject_tenant_upstream_account();
CREATE TRIGGER user_upstream_access_accounts_platform_only
    BEFORE INSERT OR UPDATE ON user_upstream_access_accounts
    FOR EACH ROW EXECUTE FUNCTION reject_tenant_upstream_account();

-- Ownership is set once. A platform account already placed in a
-- platform-only structure can never become a tenant account.
CREATE FUNCTION guard_upstream_account_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.owner_user_id IS NOT DISTINCT FROM OLD.owner_user_id THEN
        RETURN NEW;
    END IF;
    IF OLD.owner_user_id IS NOT NULL THEN
        RAISE EXCEPTION 'upstream account % owner is immutable', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM group_upstream_accounts WHERE upstream_account_id = OLD.id)
       OR EXISTS (SELECT 1 FROM upstream_account_users WHERE upstream_account_id = OLD.id)
       OR EXISTS (SELECT 1 FROM user_upstream_access_accounts WHERE upstream_account_id = OLD.id) THEN
        RAISE EXCEPTION 'platform upstream account % is in use by a platform-only structure', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER upstream_accounts_owner_guard
    BEFORE UPDATE OF owner_user_id ON upstream_accounts
    FOR EACH ROW EXECUTE FUNCTION guard_upstream_account_owner();
