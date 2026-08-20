-- Durable API key history, encrypted secret storage, and hard deletion.
-- Historical references intentionally retain only non-secret ownership and
-- display metadata. Authentication material remains exclusive to api_keys.

CREATE TABLE api_key_history (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    device_id UUID NOT NULL,
    key_prefix TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT api_key_history_device_owner_fk FOREIGN KEY (device_id, user_id)
        REFERENCES devices(id, user_id) ON DELETE RESTRICT,
    CONSTRAINT api_key_history_prefix_format CHECK (
        char_length(key_prefix) BETWEEN 8 AND 32
        AND key_prefix NOT LIKE '% %'
    ),
    UNIQUE (id, user_id),
    UNIQUE (id, user_id, device_id)
);

CREATE INDEX api_key_history_user_device_idx
    ON api_key_history (user_id, device_id, created_at DESC);

CREATE OR REPLACE FUNCTION gateway_reject_api_key_history_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'API key history is immutable' USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER api_key_history_immutable
    BEFORE UPDATE OR DELETE ON api_key_history
    FOR EACH ROW EXECUTE FUNCTION gateway_reject_api_key_history_mutation();

INSERT INTO api_key_history (id, user_id, device_id, key_prefix, created_at)
SELECT id, user_id, device_id, key_prefix, created_at
FROM api_keys;

ALTER TABLE api_keys
    ADD COLUMN secret_ciphertext BYTEA,
    ADD CONSTRAINT api_keys_secret_ciphertext_nonempty CHECK (
        secret_ciphertext IS NULL OR octet_length(secret_ciphertext) > 0
    );

COMMENT ON COLUMN api_keys.secret_ciphertext IS
    'Versioned AES-256-GCM API key ciphertext. Never expose through ordinary management queries, logs, audit metadata, or unencrypted backups.';

ALTER TABLE api_keys
    DROP CONSTRAINT api_keys_rotated_from_id_fkey,
    ADD CONSTRAINT api_keys_rotated_from_history_fk
        FOREIGN KEY (rotated_from_id) REFERENCES api_key_history(id) ON DELETE SET NULL,
    ADD CONSTRAINT api_keys_history_owner_fk
        FOREIGN KEY (id, user_id, device_id)
        REFERENCES api_key_history(id, user_id, device_id) ON DELETE RESTRICT;

ALTER TABLE usage_requests
    DROP CONSTRAINT usage_requests_key_owner_fk,
    ADD CONSTRAINT usage_requests_key_owner_fk
        FOREIGN KEY (api_key_id, user_id, device_id)
        REFERENCES api_key_history(id, user_id, device_id) ON DELETE RESTRICT;

ALTER TABLE usage_daily
    DROP CONSTRAINT usage_daily_key_owner_fk,
    ADD CONSTRAINT usage_daily_key_owner_fk
        FOREIGN KEY (api_key_id, user_id, device_id)
        REFERENCES api_key_history(id, user_id, device_id) ON DELETE RESTRICT;

ALTER TABLE usage_monthly
    DROP CONSTRAINT usage_monthly_key_owner_fk,
    ADD CONSTRAINT usage_monthly_key_owner_fk
        FOREIGN KEY (api_key_id, user_id)
        REFERENCES api_key_history(id, user_id) ON DELETE RESTRICT;

ALTER TABLE billing_reservations
    DROP CONSTRAINT billing_reservations_key_owner_fk,
    ADD CONSTRAINT billing_reservations_key_owner_fk
        FOREIGN KEY (api_key_id, user_id)
        REFERENCES api_key_history(id, user_id) ON DELETE RESTRICT;

ALTER TABLE quota_reservations
    DROP CONSTRAINT quota_reservations_key_owner_fk,
    ADD CONSTRAINT quota_reservations_key_owner_fk
        FOREIGN KEY (api_key_id, user_id)
        REFERENCES api_key_history(id, user_id) ON DELETE RESTRICT;

ALTER TABLE concurrency_leases
    DROP CONSTRAINT concurrency_leases_key_owner_fk,
    ADD CONSTRAINT concurrency_leases_key_owner_fk
        FOREIGN KEY (api_key_id, user_id)
        REFERENCES api_key_history(id, user_id) ON DELETE RESTRICT;

ALTER TABLE audit_events
    DROP CONSTRAINT audit_events_key_owner_fk,
    ADD CONSTRAINT audit_events_key_owner_fk
        FOREIGN KEY (actor_api_key_id, actor_user_id)
        REFERENCES api_key_history(id, user_id) ON DELETE RESTRICT;

-- Revoked credentials become historical references only. All durable usage,
-- quota, billing and audit rows above already point at api_key_history.
DELETE FROM api_keys WHERE status = 'revoked';

ALTER TABLE api_keys
    DROP CONSTRAINT api_keys_status_valid,
    DROP CONSTRAINT api_keys_revoked_consistent,
    DROP COLUMN revoked_at,
    DROP COLUMN revoke_reason,
    ADD CONSTRAINT api_keys_status_valid CHECK (status IN ('active', 'disabled'));

COMMENT ON TABLE api_key_history IS
    'Non-secret durable API key identity used by usage, quota, billing and audit history after active credentials are deleted.';
