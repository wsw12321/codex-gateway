-- Owner opt-in that lets members of a group read the five-hour and weekly
-- usage windows of the accounts listed in the group's selected (not "all
-- accounts") Codex and Claude pools. Every existing group starts disabled.
ALTER TABLE user_groups
    ADD COLUMN account_usage_visible BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN user_groups.account_usage_visible IS
    'When true, members may read usage windows of accounts in this group''s selected upstream account pools.';
