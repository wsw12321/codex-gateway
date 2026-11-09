-- Per-user model routes. A request whose top-level model equals source_model
-- is admitted, priced, funded and forwarded as target_model; usage keeps the
-- client's model in usage_requests.requested_model. Routes are one hop only:
-- the store rejects a rule set in which a target is also a source.
CREATE TABLE user_model_routes (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_model TEXT NOT NULL,
    target_model TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, source_model),
    CONSTRAINT user_model_routes_models_valid CHECK (
        char_length(source_model) BETWEEN 1 AND 128
        AND char_length(target_model) BETWEEN 1 AND 128
        AND source_model ~ '^[A-Za-z0-9._:-]+$'
        AND target_model ~ '^[A-Za-z0-9._:-]+$'
        AND source_model <> target_model
    )
);

COMMENT ON TABLE user_model_routes IS
    'User-managed model aliases. Permission, allowlist and pricing checks apply to target_model.';
