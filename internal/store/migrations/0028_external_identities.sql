-- Bindings are local metadata. Provider credentials and refresh tokens are not
-- persisted. A relink receives a new ID so old sessions cannot become valid.
CREATE TABLE external_identities (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    issuer TEXT NOT NULL,
    subject TEXT NOT NULL,
    masked_email TEXT NOT NULL DEFAULT '',
    linked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    unlinked_at TIMESTAMPTZ,
    CONSTRAINT external_identities_issuer_valid CHECK (char_length(issuer) BETWEEN 1 AND 2048),
    CONSTRAINT external_identities_subject_valid CHECK (char_length(subject) BETWEEN 1 AND 255),
    CONSTRAINT external_identities_email_valid CHECK (char_length(masked_email) <= 320),
    CONSTRAINT external_identities_time_order CHECK (unlinked_at IS NULL OR unlinked_at >= linked_at),
    UNIQUE (id, user_id)
);

CREATE UNIQUE INDEX external_identities_active_subject_key
    ON external_identities (issuer, subject) WHERE unlinked_at IS NULL;
CREATE UNIQUE INDEX external_identities_active_user_key
    ON external_identities (user_id) WHERE unlinked_at IS NULL;

ALTER TABLE sessions ADD COLUMN external_identity_id UUID;
ALTER TABLE sessions ADD CONSTRAINT sessions_external_identity_owner_fk
    FOREIGN KEY (external_identity_id, user_id) REFERENCES external_identities(id, user_id)
    ON DELETE CASCADE;
CREATE INDEX sessions_external_identity_idx ON sessions (external_identity_id)
    WHERE external_identity_id IS NOT NULL;

COMMENT ON TABLE external_identities IS
    'Local OIDC bindings. Only masked display email is stored; no provider tokens or passwords.';
COMMENT ON COLUMN sessions.external_identity_id IS
    'NULL for local login. Unlink revokes all sessions issued through this exact binding.';
