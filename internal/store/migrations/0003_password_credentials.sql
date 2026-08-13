-- Optional password login credentials. Encoded Argon2id hashes are sensitive.

CREATE TABLE password_credentials (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    encoded_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    CONSTRAINT password_credentials_hash_length CHECK (
        octet_length(encoded_hash) BETWEEN 64 AND 512
    )
);

COMMENT ON TABLE password_credentials IS
    'Sensitive password verifier data; never expose through APIs, logs, audit metadata, or backups without encryption.';
COMMENT ON COLUMN password_credentials.encoded_hash IS
    'Sensitive Argon2id PHC verifier; never log or return to clients.';
