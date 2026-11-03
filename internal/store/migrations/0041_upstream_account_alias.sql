-- Owner-chosen labels that tell upstream accounts apart in pickers. Like the
-- allocation preferences, the alias is Gateway-owned and metadata
-- synchronization never replaces it. It must not hold a full email address.
ALTER TABLE upstream_accounts
    ADD COLUMN alias TEXT NOT NULL DEFAULT '',
    ADD CONSTRAINT upstream_accounts_alias_safe CHECK (
        char_length(alias) <= 40
        AND alias = btrim(alias)
        AND strpos(alias, '@') = 0
        AND alias !~ '[[:cntrl:]]'
    );

COMMENT ON COLUMN upstream_accounts.alias IS
    'Owner-chosen label shown beside the synchronized account metadata; empty when unset. Never an email address or credential.';
