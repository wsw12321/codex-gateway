-- Anthropic Messages keeps pricing schema v2 and immutable admission snapshots.
-- TTL counters are only reported observations, never inferred cache writes.

ALTER TABLE usage_requests
    ADD COLUMN cache_write_5m_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN cache_write_1h_tokens BIGINT NOT NULL DEFAULT 0,
    ADD CONSTRAINT usage_requests_cache_write_ttl_nonnegative CHECK (
        cache_write_5m_tokens >= 0 AND cache_write_1h_tokens >= 0
        AND cache_write_5m_tokens <= cache_write_tokens
        AND cache_write_1h_tokens <= cache_write_tokens - cache_write_5m_tokens
    );
ALTER TABLE usage_requests
    DROP CONSTRAINT usage_requests_endpoint_valid,
    ADD CONSTRAINT usage_requests_endpoint_valid CHECK (
        endpoint IN ('responses', 'responses.compact', 'models',
                     'gemini.generateContent', 'gemini.streamGenerateContent',
                     'messages', 'messages.count_tokens')
    );

ALTER TABLE usage_daily
    ADD COLUMN cache_write_5m_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN cache_write_1h_tokens BIGINT NOT NULL DEFAULT 0,
    ADD CONSTRAINT usage_daily_cache_write_ttl_nonnegative CHECK (
        cache_write_5m_tokens >= 0 AND cache_write_1h_tokens >= 0
        AND cache_write_5m_tokens <= cache_write_tokens
        AND cache_write_1h_tokens <= cache_write_tokens - cache_write_5m_tokens
    );
ALTER TABLE usage_daily
    DROP CONSTRAINT usage_daily_endpoint_valid,
    ADD CONSTRAINT usage_daily_endpoint_valid CHECK (
        endpoint IN ('responses', 'responses.compact', 'models',
                     'gemini.generateContent', 'gemini.streamGenerateContent',
                     'messages', 'messages.count_tokens')
    );

ALTER TABLE usage_monthly
    ADD COLUMN cache_write_5m_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN cache_write_1h_tokens BIGINT NOT NULL DEFAULT 0,
    ADD CONSTRAINT usage_monthly_cache_write_ttl_nonnegative CHECK (
        cache_write_5m_tokens >= 0 AND cache_write_1h_tokens >= 0
        AND cache_write_5m_tokens <= cache_write_tokens
        AND cache_write_1h_tokens <= cache_write_tokens - cache_write_5m_tokens
    );
ALTER TABLE usage_monthly
    DROP CONSTRAINT usage_monthly_endpoint_valid,
    ADD CONSTRAINT usage_monthly_endpoint_valid CHECK (
        endpoint IN ('responses', 'responses.compact', 'models',
                     'gemini.generateContent', 'gemini.streamGenerateContent',
                     'messages', 'messages.count_tokens')
    );

ALTER TABLE usage_requests
    ADD COLUMN cache_write_ttl_present BOOLEAN NOT NULL DEFAULT false,
    ADD CONSTRAINT usage_requests_cache_write_ttl_consistent CHECK (
        (cache_write_ttl_present AND cache_write_tokens_present
         AND cache_write_1h_tokens = cache_write_tokens - cache_write_5m_tokens)
        OR (NOT cache_write_ttl_present AND cache_write_5m_tokens = 0 AND cache_write_1h_tokens = 0)
    );

ALTER TABLE billing_reservations
    ADD COLUMN actual_cache_write_5m_tokens BIGINT,
    ADD COLUMN actual_cache_write_1h_tokens BIGINT,
    ADD COLUMN actual_cache_write_ttl_present BOOLEAN,
    ADD COLUMN applied_cache_write_5m_usd_per_million NUMERIC(30,12),
    ADD COLUMN applied_cache_write_1h_usd_per_million NUMERIC(30,12),
    ADD CONSTRAINT billing_reservations_cache_write_ttl_valid CHECK (
        CASE WHEN actual_cache_write_ttl_present IS TRUE THEN
            actual_cache_write_tokens IS NOT NULL
            AND actual_cache_write_5m_tokens IS NOT NULL AND actual_cache_write_5m_tokens >= 0
            AND actual_cache_write_1h_tokens IS NOT NULL AND actual_cache_write_1h_tokens >= 0
            AND actual_cache_write_5m_tokens <= actual_cache_write_tokens
            AND actual_cache_write_1h_tokens = actual_cache_write_tokens - actual_cache_write_5m_tokens
        ELSE actual_cache_write_5m_tokens IS NULL AND actual_cache_write_1h_tokens IS NULL END
    ),
    ADD CONSTRAINT billing_reservations_cache_write_ttl_prices_valid CHECK (
        (applied_cache_write_5m_usd_per_million IS NULL AND applied_cache_write_1h_usd_per_million IS NULL)
        OR (cache_write_mode = 'separate_by_ttl'
            AND applied_cache_write_5m_usd_per_million IS NOT NULL AND applied_cache_write_5m_usd_per_million >= 0
            AND applied_cache_write_1h_usd_per_million IS NOT NULL AND applied_cache_write_1h_usd_per_million >= 0)
    );

ALTER TABLE billing_ledger_entries
    ADD COLUMN cache_write_5m_tokens BIGINT,
    ADD COLUMN cache_write_1h_tokens BIGINT,
    ADD COLUMN cache_write_ttl_present BOOLEAN,
    ADD COLUMN applied_cache_write_5m_usd_per_million NUMERIC(30,12),
    ADD COLUMN applied_cache_write_1h_usd_per_million NUMERIC(30,12),
    ADD CONSTRAINT billing_ledger_entries_cache_write_ttl_valid CHECK (
        CASE WHEN cache_write_ttl_present IS TRUE THEN
            cache_write_tokens IS NOT NULL
            AND cache_write_5m_tokens IS NOT NULL AND cache_write_5m_tokens >= 0
            AND cache_write_1h_tokens IS NOT NULL AND cache_write_1h_tokens >= 0
            AND cache_write_5m_tokens <= cache_write_tokens
            AND cache_write_1h_tokens = cache_write_tokens - cache_write_5m_tokens
        ELSE cache_write_5m_tokens IS NULL AND cache_write_1h_tokens IS NULL END
    ),
    ADD CONSTRAINT billing_ledger_entries_cache_write_ttl_prices_valid CHECK (
        (applied_cache_write_5m_usd_per_million IS NULL AND applied_cache_write_1h_usd_per_million IS NULL)
        OR (cache_write_mode = 'separate_by_ttl'
            AND applied_cache_write_5m_usd_per_million IS NOT NULL AND applied_cache_write_5m_usd_per_million >= 0
            AND applied_cache_write_1h_usd_per_million IS NOT NULL AND applied_cache_write_1h_usd_per_million >= 0)
    );

ALTER TABLE billing_reservations
    ADD CONSTRAINT billing_reservations_cache_write_ttl_terminal CHECK (
        CASE WHEN state = 'settled' AND cache_write_mode = 'separate_by_ttl' THEN
            actual_cache_write_ttl_present IS NOT NULL
            AND applied_cache_write_5m_usd_per_million IS NOT NULL
            AND applied_cache_write_1h_usd_per_million IS NOT NULL
        WHEN state <> 'settled' THEN
            actual_cache_write_ttl_present IS NULL
            AND applied_cache_write_5m_usd_per_million IS NULL
            AND applied_cache_write_1h_usd_per_million IS NULL
        ELSE true END
    );
ALTER TABLE billing_ledger_entries
    ADD CONSTRAINT billing_ledger_cache_write_ttl_charge CHECK (
        CASE WHEN entry_type = 'usage_charge' AND cache_write_mode = 'separate_by_ttl' THEN
            cache_write_ttl_present IS NOT NULL
            AND applied_cache_write_5m_usd_per_million IS NOT NULL
            AND applied_cache_write_1h_usd_per_million IS NOT NULL
        ELSE true END
    );

ALTER TABLE billing_reservations
    DROP CONSTRAINT billing_reservations_pricing_version_valid,
    ADD CONSTRAINT billing_reservations_pricing_version_valid CHECK (
        pricing_rule_version IN (1, 2)
        AND billing_mode IN ('legacy', 'openai_api_token_equivalent',
                             'gemini_api_token_equivalent', 'anthropic_api_token_equivalent', 'internal_zero')
        AND (
            (pricing_rule_version = 1 AND billing_mode = 'legacy'
             AND input_usd_per_million IS NOT NULL
             AND cached_input_usd_per_million IS NOT NULL
             AND output_usd_per_million IS NOT NULL
             AND pricing_catalog_as_of IS NULL AND pricing_model IS NULL
             AND pricing_snapshot IS NULL AND cache_write_mode IS NULL)
            OR
            (pricing_rule_version = 2 AND billing_mode <> 'legacy'
             AND input_usd_per_million IS NULL
             AND cached_input_usd_per_million IS NULL
             AND output_usd_per_million IS NULL
             AND pricing_catalog_as_of IS NOT NULL
             AND pricing_model IS NOT NULL
             AND char_length(pricing_model) BETWEEN 1 AND 128
             AND pricing_snapshot IS NOT NULL
             AND jsonb_typeof(pricing_snapshot) = 'object'
             AND cache_write_mode IN ('separate', 'included_in_input', 'separate_by_ttl'))
        )
    );


ALTER TABLE billing_ledger_entries DROP CONSTRAINT billing_ledger_pricing_metadata_valid;
ALTER TABLE billing_ledger_entries
    ADD CONSTRAINT billing_ledger_pricing_metadata_valid CHECK (
        pricing_rule_version IN (1, 2)
        AND (actual_model IS NULL OR char_length(actual_model) BETWEEN 1 AND 128)
        AND (cache_write_mode IS NULL OR cache_write_mode IN ('separate', 'included_in_input', 'separate_by_ttl'))
        AND (requested_service_tier IS NULL OR char_length(requested_service_tier) BETWEEN 1 AND 32)
        AND (actual_service_tier IS NULL OR char_length(actual_service_tier) BETWEEN 1 AND 32)
        AND (pricing_service_tier IS NULL OR pricing_service_tier IN
            ('standard', 'flex', 'fast', 'max_published'))
        AND (context_class IS NULL OR context_class IN ('short', 'long'))
        AND (pricing_fallback_reason IS NULL OR
             (char_length(pricing_fallback_reason) BETWEEN 1 AND 256
              AND pricing_fallback_reason ~ '^[a-z0-9_,.-]+$'))
        AND (applied_input_usd_per_million IS NULL OR applied_input_usd_per_million >= 0)
        AND (applied_cached_input_usd_per_million IS NULL OR applied_cached_input_usd_per_million >= 0)
        AND (applied_cache_write_usd_per_million IS NULL OR applied_cache_write_usd_per_million >= 0)
        AND (applied_output_usd_per_million IS NULL OR applied_output_usd_per_million >= 0)
        AND (pricing_rule_version = 1 OR entry_type <> 'usage_charge' OR
            (usage_requested_at IS NOT NULL AND actual_model IS NOT NULL
             AND cache_write_tokens IS NOT NULL AND cache_write_mode IS NOT NULL
             AND pricing_service_tier IS NOT NULL AND context_class IS NOT NULL
             AND pricing_catalog_as_of IS NOT NULL
             AND applied_input_usd_per_million IS NOT NULL
             AND applied_cached_input_usd_per_million IS NOT NULL
             AND applied_cache_write_usd_per_million IS NOT NULL
             AND applied_output_usd_per_million IS NOT NULL))
    );

COMMENT ON COLUMN usage_requests.cache_write_tokens IS
    'Total cache creation tokens including all TTLs; included in normalized input_tokens.';
COMMENT ON COLUMN usage_requests.cache_write_ttl_present IS
    'True only when the upstream reported a complete five-minute and one-hour breakdown.';
