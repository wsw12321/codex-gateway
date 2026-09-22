-- Deletion receipts contain identifiers only. No user FK may prevent later
-- deletion of a principal, and no authentication or monetary data is stored.
CREATE TABLE information_user_deletions (
    operation_id UUID PRIMARY KEY,
    actor_user_id_snapshot UUID NOT NULL,
    request_fingerprint BYTEA NOT NULL CHECK (octet_length(request_fingerprint) = 32),
    user_ids JSONB NOT NULL CHECK (jsonb_typeof(user_ids) = 'array'),
    transaction_id BIGINT NOT NULL,
    approved_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE audit_events
    ADD COLUMN actor_user_id_snapshot TEXT,
    ADD COLUMN actor_session_id_snapshot TEXT,
    ADD COLUMN actor_api_key_id_snapshot TEXT;

CREATE OR REPLACE FUNCTION gateway_reject_api_key_history_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND EXISTS (
        SELECT 1 FROM information_user_deletions d
        WHERE d.operation_id::text = current_setting('gateway.user_deletion_operation', true)
          AND d.transaction_id = txid_current()
          AND d.approved_at IS NOT NULL AND d.completed_at IS NULL
          AND d.user_ids ? OLD.user_id::text
    ) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'API key history is immutable' USING ERRCODE = '55000';
END;
$$;

COMMENT ON TABLE information_user_deletions IS
    'Durable idempotency receipts for Owner deletion of users without accounting history. Approved targets and immutable-key deletion are restricted to the same transaction.';
