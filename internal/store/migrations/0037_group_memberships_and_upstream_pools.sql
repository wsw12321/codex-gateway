-- Multi-group memberships and per-provider group upstream account policies.
-- The legacy billing_accounts.group_id is retained as a compatibility mirror;
-- new code must read group_memberships.
CREATE TABLE group_memberships (
    group_id UUID NOT NULL REFERENCES user_groups(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    priority INTEGER NOT NULL DEFAULT 100 CHECK (priority BETWEEN 1 AND 100),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, user_id)
);
CREATE INDEX IF NOT EXISTS group_memberships_user_priority_idx
    ON group_memberships(user_id, priority, group_id);
CREATE INDEX IF NOT EXISTS group_memberships_group_priority_idx
    ON group_memberships(group_id, priority, user_id);
INSERT INTO group_memberships(group_id,user_id,priority,created_at,updated_at)
SELECT group_id,user_id,100,updated_at,updated_at
FROM billing_accounts
WHERE group_id IS NOT NULL
ON CONFLICT (group_id,user_id) DO NOTHING;

CREATE TABLE group_upstream_account_policies (
    group_id UUID NOT NULL REFERENCES user_groups(id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK (provider IN ('codex','antigravity','anthropic')),
    strategy TEXT NOT NULL DEFAULT 'all' CHECK (strategy IN ('all','selected')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, provider)
);
CREATE TABLE group_upstream_accounts (
    group_id UUID NOT NULL REFERENCES user_groups(id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK (provider IN ('codex','antigravity','anthropic')),
    upstream_account_id TEXT NOT NULL REFERENCES upstream_accounts(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, provider, upstream_account_id)
);
CREATE INDEX IF NOT EXISTS group_upstream_accounts_account_idx
    ON group_upstream_accounts(provider, upstream_account_id);
