-- Per-model authorization is snapshotted for each user. Catalog membership is
-- kept separately from the default so models removed from a later pricing
-- catalog can retain history without remaining effective.

CREATE TABLE model_access_defaults (
    model TEXT PRIMARY KEY,
    enabled BOOLEAN NOT NULL DEFAULT true,
    catalog_active BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by_user_id UUID REFERENCES users(id) ON DELETE RESTRICT,
    CONSTRAINT model_access_defaults_model_valid CHECK (
        char_length(model) BETWEEN 1 AND 128
        AND model ~ '^[A-Za-z0-9._:-]{1,128}$'
    ),
    CONSTRAINT model_access_defaults_time_order CHECK (updated_at >= created_at)
);

CREATE INDEX model_access_defaults_active_idx
    ON model_access_defaults (model) WHERE catalog_active;

CREATE TABLE user_model_access (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    model TEXT NOT NULL REFERENCES model_access_defaults(model) ON DELETE RESTRICT,
    enabled BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by_user_id UUID REFERENCES users(id) ON DELETE RESTRICT,
    PRIMARY KEY (user_id, model),
    CONSTRAINT user_model_access_time_order CHECK (updated_at >= created_at)
);

CREATE INDEX user_model_access_model_enabled_idx
    ON user_model_access (model, enabled, user_id);

CREATE OR REPLACE FUNCTION gateway_snapshot_model_access_defaults() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO user_model_access (user_id, model, enabled)
    SELECT NEW.id, d.model, d.enabled
    FROM model_access_defaults d
    WHERE d.catalog_active
    ON CONFLICT (user_id, model) DO NOTHING;
    RETURN NEW;
END;
$$;

CREATE TRIGGER users_snapshot_model_access_defaults
    AFTER INSERT ON users
    FOR EACH ROW EXECUTE FUNCTION gateway_snapshot_model_access_defaults();

COMMENT ON TABLE model_access_defaults IS
    'New-user model authorization defaults synchronized with the manageable pricing catalog. Inactive rows are retained history and are not effective.';
COMMENT ON TABLE user_model_access IS
    'Per-user authorization snapshots. Updates to new-user defaults never rewrite existing rows.';
COMMENT ON COLUMN user_model_access.updated_by_user_id IS
    'Owner who last changed this authorization; NULL denotes registration or catalog synchronization.';
