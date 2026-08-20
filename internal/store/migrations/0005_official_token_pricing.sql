-- OpenAI official-token-equivalent pricing v2. Existing usage and ledger rows
-- are not rewritten; defaults expose them as pricing_rule_version = 1.

ALTER TABLE usage_requests
    ADD COLUMN requested_model TEXT,
    ADD COLUMN requested_service_tier TEXT,
    ADD COLUMN actual_service_tier TEXT,
    ADD COLUMN cache_write_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN cache_write_tokens_present BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN pricing_rule_version INTEGER NOT NULL DEFAULT 1,
    ADD COLUMN pricing_service_tier TEXT,
    ADD COLUMN context_class TEXT,
    ADD COLUMN pricing_fallback_reason TEXT;

ALTER TABLE usage_requests DROP CONSTRAINT usage_requests_metrics_nonnegative;
ALTER TABLE usage_requests
    ADD CONSTRAINT usage_requests_metrics_nonnegative CHECK (
        input_tokens >= 0 AND cached_input_tokens >= 0
        AND cache_write_tokens >= 0 AND output_tokens >= 0 AND reasoning_tokens >= 0
        AND request_bytes >= 0 AND response_bytes >= 0
        AND (ttft_ms IS NULL OR ttft_ms >= 0)
        AND (duration_ms IS NULL OR duration_ms >= 0)
        AND cached_input_tokens + cache_write_tokens <= input_tokens
    ),
    ADD CONSTRAINT usage_requests_requested_model_valid CHECK (
        requested_model IS NULL OR char_length(requested_model) BETWEEN 1 AND 128
    ),
    ADD CONSTRAINT usage_requests_service_tiers_valid CHECK (
        (requested_service_tier IS NULL OR char_length(requested_service_tier) BETWEEN 1 AND 32)
        AND (actual_service_tier IS NULL OR char_length(actual_service_tier) BETWEEN 1 AND 32)
        AND (pricing_service_tier IS NULL OR pricing_service_tier IN
            ('standard', 'flex', 'fast', 'max_published'))
    ),
    ADD CONSTRAINT usage_requests_pricing_metadata_valid CHECK (
        pricing_rule_version IN (1, 2)
        AND (context_class IS NULL OR context_class IN ('short', 'long'))
        AND (pricing_fallback_reason IS NULL OR
             (char_length(pricing_fallback_reason) BETWEEN 1 AND 256
              AND pricing_fallback_reason ~ '^[a-z0-9_,.-]+$'))
    );

CREATE INDEX usage_requests_requested_model_time_idx
    ON usage_requests (requested_model, requested_at DESC);
CREATE INDEX usage_requests_pricing_fallback_idx
    ON usage_requests (pricing_fallback_reason, requested_at DESC)
    WHERE pricing_fallback_reason IS NOT NULL;

ALTER TABLE usage_daily
    ADD COLUMN cache_write_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage_daily DROP CONSTRAINT usage_daily_metrics_nonnegative;
ALTER TABLE usage_daily
    ADD CONSTRAINT usage_daily_metrics_nonnegative CHECK (
        request_count >= 0 AND error_count >= 0 AND error_count <= request_count
        AND input_tokens >= 0 AND cached_input_tokens >= 0 AND cache_write_tokens >= 0
        AND output_tokens >= 0 AND reasoning_tokens >= 0
        AND request_bytes >= 0 AND response_bytes >= 0
        AND ttft_count >= 0 AND ttft_sum_ms >= 0
        AND (p95_ttft_ms IS NULL OR p95_ttft_ms >= 0)
        AND duration_count >= 0 AND duration_sum_ms >= 0
        AND (p95_duration_ms IS NULL OR p95_duration_ms >= 0)
        AND cached_input_tokens + cache_write_tokens <= input_tokens
    );

ALTER TABLE usage_monthly
    ADD COLUMN cache_write_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage_monthly DROP CONSTRAINT usage_monthly_metrics_nonnegative;
ALTER TABLE usage_monthly
    ADD CONSTRAINT usage_monthly_metrics_nonnegative CHECK (
        request_count >= 0 AND error_count >= 0 AND error_count <= request_count
        AND input_tokens >= 0 AND cached_input_tokens >= 0 AND cache_write_tokens >= 0
        AND output_tokens >= 0 AND reasoning_tokens >= 0
        AND request_bytes >= 0 AND response_bytes >= 0
        AND (p95_ttft_ms IS NULL OR p95_ttft_ms >= 0)
        AND (p95_duration_ms IS NULL OR p95_duration_ms >= 0)
        AND cached_input_tokens + cache_write_tokens <= input_tokens
    );

ALTER TABLE billing_reservations
    ALTER COLUMN input_usd_per_million DROP NOT NULL,
    ALTER COLUMN cached_input_usd_per_million DROP NOT NULL,
    ALTER COLUMN output_usd_per_million DROP NOT NULL,
    ADD COLUMN pricing_rule_version INTEGER NOT NULL DEFAULT 1,
    ADD COLUMN billing_mode TEXT NOT NULL DEFAULT 'legacy',
    ADD COLUMN pricing_catalog_as_of DATE,
    ADD COLUMN pricing_model TEXT,
    ADD COLUMN pricing_snapshot JSONB,
    ADD COLUMN cache_write_mode TEXT,
    ADD COLUMN requested_service_tier TEXT,
    ADD COLUMN actual_service_tier TEXT,
    ADD COLUMN pricing_service_tier TEXT,
    ADD COLUMN actual_model TEXT,
    ADD COLUMN context_class TEXT,
    ADD COLUMN actual_cache_write_tokens BIGINT,
    ADD COLUMN applied_input_usd_per_million NUMERIC(30,12),
    ADD COLUMN applied_cached_input_usd_per_million NUMERIC(30,12),
    ADD COLUMN applied_cache_write_usd_per_million NUMERIC(30,12),
    ADD COLUMN applied_output_usd_per_million NUMERIC(30,12),
    ADD COLUMN pricing_fallback_reason TEXT;

ALTER TABLE billing_reservations
    DROP CONSTRAINT billing_reservations_prices_nonnegative,
    DROP CONSTRAINT billing_reservations_source_required,
    DROP CONSTRAINT billing_reservations_actuals_valid,
    DROP CONSTRAINT billing_reservations_terminal_consistent;

ALTER TABLE billing_reservations
    ADD CONSTRAINT billing_reservations_prices_nonnegative CHECK (
        (input_usd_per_million IS NULL OR input_usd_per_million >= 0)
        AND (cached_input_usd_per_million IS NULL OR cached_input_usd_per_million >= 0)
        AND (output_usd_per_million IS NULL OR output_usd_per_million >= 0)
        AND (applied_input_usd_per_million IS NULL OR applied_input_usd_per_million >= 0)
        AND (applied_cached_input_usd_per_million IS NULL OR applied_cached_input_usd_per_million >= 0)
        AND (applied_cache_write_usd_per_million IS NULL OR applied_cache_write_usd_per_million >= 0)
        AND (applied_output_usd_per_million IS NULL OR applied_output_usd_per_million >= 0)
    ),
    ADD CONSTRAINT billing_reservations_pricing_version_valid CHECK (
        pricing_rule_version IN (1, 2)
        AND billing_mode IN ('legacy', 'openai_api_token_equivalent', 'internal_zero')
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
             AND cache_write_mode IN ('separate', 'included_in_input'))
        )
    ),
    ADD CONSTRAINT billing_reservations_source_required CHECK (
        billing_mode = 'internal_zero'
        OR day_period_id IS NOT NULL OR week_period_id IS NOT NULL
        OR month_period_id IS NOT NULL OR cash_lot_cutoff IS NOT NULL
    ),
    ADD CONSTRAINT billing_reservations_pricing_metadata_valid CHECK (
        (requested_service_tier IS NULL OR char_length(requested_service_tier) BETWEEN 1 AND 32)
        AND (actual_service_tier IS NULL OR char_length(actual_service_tier) BETWEEN 1 AND 32)
        AND (pricing_service_tier IS NULL OR pricing_service_tier IN
            ('standard', 'flex', 'fast', 'max_published'))
        AND (actual_model IS NULL OR char_length(actual_model) BETWEEN 1 AND 128)
        AND (context_class IS NULL OR context_class IN ('short', 'long'))
        AND (pricing_fallback_reason IS NULL OR
             (char_length(pricing_fallback_reason) BETWEEN 1 AND 256
              AND pricing_fallback_reason ~ '^[a-z0-9_,.-]+$'))
    ),
    ADD CONSTRAINT billing_reservations_actuals_valid CHECK (
        (actual_input_tokens IS NULL OR actual_input_tokens >= 0)
        AND (actual_cached_input_tokens IS NULL OR actual_cached_input_tokens >= 0)
        AND (actual_cache_write_tokens IS NULL OR actual_cache_write_tokens >= 0)
        AND (actual_output_tokens IS NULL OR actual_output_tokens >= 0)
        AND (actual_input_tokens IS NULL OR actual_cached_input_tokens IS NULL
             OR actual_cache_write_tokens IS NULL
             OR actual_cached_input_tokens + actual_cache_write_tokens <= actual_input_tokens)
        AND (actual_cost_usd IS NULL OR actual_cost_usd >= 0)
        AND (charged_usd IS NULL OR charged_usd >= 0)
        AND (uncovered_usd IS NULL OR uncovered_usd >= 0)
        AND (actual_cost_usd IS NULL OR charged_usd IS NULL OR uncovered_usd IS NULL
             OR actual_cost_usd = charged_usd + uncovered_usd)
    ),
    ADD CONSTRAINT billing_reservations_terminal_consistent CHECK (
        (state = 'reserved' AND actual_input_tokens IS NULL
            AND actual_cached_input_tokens IS NULL AND actual_cache_write_tokens IS NULL
            AND actual_output_tokens IS NULL AND actual_cost_usd IS NULL
            AND charged_usd IS NULL AND uncovered_usd IS NULL AND settled_at IS NULL
            AND actual_service_tier IS NULL AND pricing_service_tier IS NULL
            AND actual_model IS NULL AND context_class IS NULL
            AND applied_input_usd_per_million IS NULL
            AND applied_cached_input_usd_per_million IS NULL
            AND applied_cache_write_usd_per_million IS NULL
            AND applied_output_usd_per_million IS NULL
            AND pricing_fallback_reason IS NULL)
        OR (state = 'released' AND actual_input_tokens IS NULL
            AND actual_cached_input_tokens IS NULL AND actual_cache_write_tokens IS NULL
            AND actual_output_tokens IS NULL AND actual_cost_usd IS NULL
            AND charged_usd IS NULL AND uncovered_usd IS NULL AND settled_at IS NOT NULL
            AND actual_service_tier IS NULL AND pricing_service_tier IS NULL
            AND actual_model IS NULL AND context_class IS NULL
            AND applied_input_usd_per_million IS NULL
            AND applied_cached_input_usd_per_million IS NULL
            AND applied_cache_write_usd_per_million IS NULL
            AND applied_output_usd_per_million IS NULL
            AND pricing_fallback_reason IS NULL)
        OR (state = 'settled' AND actual_input_tokens IS NOT NULL
            AND actual_cached_input_tokens IS NOT NULL AND actual_output_tokens IS NOT NULL
            AND actual_cost_usd IS NOT NULL AND charged_usd IS NOT NULL
            AND uncovered_usd IS NOT NULL AND settled_at IS NOT NULL
            AND (pricing_rule_version = 1 OR
                (actual_cache_write_tokens IS NOT NULL
                 AND pricing_service_tier IS NOT NULL AND context_class IS NOT NULL
                 AND applied_input_usd_per_million IS NOT NULL
                 AND applied_cached_input_usd_per_million IS NOT NULL
                 AND applied_cache_write_usd_per_million IS NOT NULL
                 AND applied_output_usd_per_million IS NOT NULL)))
    );

CREATE INDEX billing_reservations_fallback_idx
    ON billing_reservations (pricing_fallback_reason, settled_at DESC)
    WHERE pricing_fallback_reason IS NOT NULL;

ALTER TABLE billing_ledger_entries
    ADD COLUMN usage_requested_at TIMESTAMPTZ,
    ADD COLUMN actual_model TEXT,
    ADD COLUMN cache_write_tokens BIGINT,
    ADD COLUMN cache_write_mode TEXT,
    ADD COLUMN requested_service_tier TEXT,
    ADD COLUMN actual_service_tier TEXT,
    ADD COLUMN pricing_service_tier TEXT,
    ADD COLUMN context_class TEXT,
    ADD COLUMN pricing_rule_version INTEGER NOT NULL DEFAULT 1,
    ADD COLUMN pricing_catalog_as_of DATE,
    ADD COLUMN applied_input_usd_per_million NUMERIC(30,12),
    ADD COLUMN applied_cached_input_usd_per_million NUMERIC(30,12),
    ADD COLUMN applied_cache_write_usd_per_million NUMERIC(30,12),
    ADD COLUMN applied_output_usd_per_million NUMERIC(30,12),
    ADD COLUMN pricing_fallback_reason TEXT;

ALTER TABLE billing_ledger_entries DROP CONSTRAINT billing_ledger_tokens_nonnegative;
ALTER TABLE billing_ledger_entries
    ADD CONSTRAINT billing_ledger_tokens_nonnegative CHECK (
        (input_tokens IS NULL OR input_tokens >= 0)
        AND (cached_input_tokens IS NULL OR cached_input_tokens >= 0)
        AND (cache_write_tokens IS NULL OR cache_write_tokens >= 0)
        AND (output_tokens IS NULL OR output_tokens >= 0)
        AND (input_tokens IS NULL OR cached_input_tokens IS NULL
             OR cache_write_tokens IS NULL
             OR cached_input_tokens + cache_write_tokens <= input_tokens)
    ),
    ADD CONSTRAINT billing_ledger_pricing_metadata_valid CHECK (
        pricing_rule_version IN (1, 2)
        AND (actual_model IS NULL OR char_length(actual_model) BETWEEN 1 AND 128)
        AND (cache_write_mode IS NULL OR cache_write_mode IN ('separate', 'included_in_input'))
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

CREATE INDEX billing_ledger_usage_requested_idx
    ON billing_ledger_entries (usage_requested_at DESC, id DESC)
    WHERE entry_type = 'usage_charge';
CREATE INDEX billing_ledger_pricing_dimensions_idx
    ON billing_ledger_entries (pricing_service_tier, context_class, usage_requested_at DESC)
    WHERE entry_type = 'usage_charge' AND pricing_rule_version = 2;

COMMENT ON COLUMN billing_reservations.pricing_snapshot IS
    'Immutable v2 selected-model price matrix and fallback policy captured at admission.';
COMMENT ON COLUMN billing_ledger_entries.usage_requested_at IS
    'Original usage request time retained after detailed usage metadata expires.';
