ALTER TABLE users DROP CONSTRAINT users_status_valid;
ALTER TABLE users ADD CONSTRAINT users_status_valid CHECK (status IN ('active','disabled','pending'));
ALTER TABLE users DROP CONSTRAINT users_disabled_consistent;
ALTER TABLE users ADD CONSTRAINT users_disabled_consistent CHECK (
    (status = 'disabled' AND disabled_at IS NOT NULL)
    OR (status IN ('active','pending') AND disabled_at IS NULL)
);

ALTER TABLE invitations
    ADD COLUMN max_uses INTEGER NOT NULL DEFAULT 1 CHECK (max_uses > 0),
    ADD COLUMN requires_approval BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN group_id UUID REFERENCES user_groups(id) ON DELETE RESTRICT;
ALTER TABLE invitations DROP CONSTRAINT invitations_kind_valid;
ALTER TABLE invitations ADD CONSTRAINT invitations_kind_valid
    CHECK (kind IN ('owner_bootstrap','member','recovery','group'));
ALTER TABLE invitations DROP CONSTRAINT invitations_expiry_valid;
ALTER TABLE invitations ADD CONSTRAINT invitations_expiry_valid CHECK (
    expires_at > created_at AND (kind IN ('member','group') OR expires_at <= created_at + INTERVAL '24 hours')
);
ALTER TABLE invitations ADD CONSTRAINT invitations_group_valid
    CHECK ((kind = 'group') = (group_id IS NOT NULL));
ALTER TABLE invitations ADD CONSTRAINT invitations_single_identity_use CHECK (
    kind IN ('member','group') OR (max_uses = 1 AND NOT requires_approval)
);
-- Registration and group invitations survive deletion of their creator.
-- New invitation creation still requires an inviter in the store transaction.
ALTER TABLE invitations DROP CONSTRAINT invitations_inviter_required;

CREATE TABLE invitation_applications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    invitation_id UUID NOT NULL REFERENCES invitations(id) ON DELETE RESTRICT,
    user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    username TEXT NOT NULL,
    display_name TEXT NOT NULL,
    registered_at TIMESTAMPTZ NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    status TEXT NOT NULL CHECK (status IN ('pending','approved')),
    reviewed_at TIMESTAMPTZ,
    reviewed_by UUID REFERENCES users(id) ON DELETE SET NULL,
    UNIQUE (invitation_id,user_id),
    CHECK (status <> 'pending' OR (user_id IS NOT NULL AND reviewed_at IS NULL AND reviewed_by IS NULL))
);
CREATE INDEX invitation_applications_page_idx ON invitation_applications(invitation_id,applied_at DESC,id);
CREATE INDEX invitation_applications_user_idx ON invitation_applications(user_id) WHERE user_id IS NOT NULL;
CREATE INDEX invitations_group_page_idx ON invitations(group_id,created_at DESC,id);

INSERT INTO invitation_applications(invitation_id,user_id,username,display_name,registered_at,applied_at,status,reviewed_at)
SELECT i.id,u.id,u.username,u.display_name,u.created_at,i.used_at,'approved',i.used_at
FROM invitations i JOIN users u ON u.id=i.used_by_user_id
WHERE i.kind='member' AND i.used_at IS NOT NULL;
-- Uses of member invitations are now accounted for only by applications.
UPDATE invitations SET used_at=NULL,used_by_user_id=NULL WHERE kind='member';
