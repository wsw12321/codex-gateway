ALTER TABLE usage_requests
    ADD COLUMN conversation_hash TEXT;

ALTER TABLE usage_requests
    ADD CONSTRAINT usage_requests_conversation_hash_valid CHECK (
        conversation_hash IS NULL OR conversation_hash ~ '^conv-[a-f0-9]{32}$'
    );

COMMENT ON COLUMN usage_requests.conversation_hash IS
    'Opaque caller-scoped canonical sidecar session hash for owner monitoring; never a raw session ID or request content.';
