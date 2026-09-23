ALTER TABLE usage_requests
    DROP CONSTRAINT usage_requests_state_valid;

ALTER TABLE usage_requests
    ADD CONSTRAINT usage_requests_state_valid CHECK (
        state IN ('in_progress', 'completed', 'degraded', 'failed', 'cancelled')
    );
