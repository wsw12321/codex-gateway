-- Preserve all admission snapshots and charges while identifying new Gemini
-- API-equivalent charges separately from the historical OpenAI mode.
ALTER TABLE billing_reservations
    DROP CONSTRAINT billing_reservations_pricing_version_valid,
    ADD CONSTRAINT billing_reservations_pricing_version_valid CHECK (
        pricing_rule_version IN (1, 2)
        AND billing_mode IN ('legacy', 'openai_api_token_equivalent',
                             'gemini_api_token_equivalent', 'internal_zero')
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
    );

-- A new AGY model requires an explicit grant. Do not inherit old aliases,
-- per-model multipliers, or restricted API key allowlists. Catalog sync may
-- activate these rows later, but it preserves their disabled defaults.
LOCK TABLE model_access_defaults IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE;

INSERT INTO model_access_defaults (model, enabled, catalog_active)
VALUES
    ('gemini-3.8-flash-high', false, false),
    ('gemini-3.8-flash-medium', false, false),
    ('gemini-3.7-flash-high', false, false),
    ('gemini-3.7-flash-medium', false, false),
    ('gemini-3.6-flash-high', false, false),
    ('gemini-3.6-flash-medium', false, false),
    ('gemini-3.1-pro-high', false, false)
ON CONFLICT (model) DO UPDATE
SET enabled = false, catalog_active = false,
    updated_at = GREATEST(model_access_defaults.updated_at, now()),
    updated_by_user_id = NULL;

INSERT INTO user_model_access (user_id, model, enabled)
SELECT u.id, d.model, false
FROM users u CROSS JOIN model_access_defaults d
WHERE d.model IN (
    'gemini-3.8-flash-high', 'gemini-3.8-flash-medium',
    'gemini-3.7-flash-high', 'gemini-3.7-flash-medium',
    'gemini-3.6-flash-high', 'gemini-3.6-flash-medium',
    'gemini-3.1-pro-high'
)
ON CONFLICT (user_id, model) DO UPDATE
SET enabled = false,
    updated_at = GREATEST(user_model_access.updated_at, now()),
    updated_by_user_id = NULL;

UPDATE model_access_defaults
SET catalog_active = false, updated_at = GREATEST(updated_at, now())
WHERE model = 'gemini-3.1-pro-preview';
