-- One-time business configuration migration. Pricing itself is supplied by
-- GATEWAY_USAGE_PRICING_JSON; cpa-pricing-migrate merges its complete catalog.
-- Never rewrite API key allowlists, balances, usage, or admission snapshots.
LOCK TABLE model_access_defaults IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE billing_model_multipliers IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE cpa_v8_model_config_backup (
    setting TEXT NOT NULL CHECK (setting IN ('default', 'user', 'multiplier')),
    model TEXT NOT NULL,
    subject_id TEXT NOT NULL DEFAULT '',
    before_value JSONB,
    applied_value JSONB,
    captured_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (setting, model, subject_id)
);
COMMENT ON TABLE cpa_v8_model_config_backup IS
    'CPA v8 cutover configuration only; rollback compares applied values before restoring. Contains no credentials, prices, balances or usage.';

CREATE TEMPORARY TABLE cpa_v8_target_models (model TEXT PRIMARY KEY) ON COMMIT DROP;
INSERT INTO cpa_v8_target_models VALUES
    ('gemini-pro-agent'), ('gemini-3.1-pro-low'), ('gemini-3-flash'),
    ('gemini-3.6-flash-high'), ('gemini-3.7-flash-high'), ('gemini-3.8-flash-high'),
    ('gemini-3.1-flash-lite'), ('gemini-3.5-flash-lite'), ('gpt-6.1-sol');

INSERT INTO cpa_v8_model_config_backup (setting, model, before_value)
SELECT 'default', t.model, to_jsonb(d)
FROM cpa_v8_target_models t LEFT JOIN model_access_defaults d USING (model);

INSERT INTO cpa_v8_model_config_backup (setting, model, subject_id, before_value)
SELECT 'user', t.model, u.id::text, to_jsonb(a)
FROM users u CROSS JOIN cpa_v8_target_models t
LEFT JOIN user_model_access a ON a.user_id = u.id AND a.model = t.model;

INSERT INTO cpa_v8_model_config_backup (setting, model, before_value)
SELECT 'multiplier', t.model, to_jsonb(m)
FROM cpa_v8_target_models t LEFT JOIN billing_model_multipliers m USING (model);

INSERT INTO model_access_defaults (model, enabled, catalog_active)
SELECT model, true, false FROM cpa_v8_target_models
ON CONFLICT (model) DO UPDATE
SET enabled = true, catalog_active = false,
    updated_at = GREATEST(model_access_defaults.updated_at, now()),
    updated_by_user_id = NULL;

INSERT INTO user_model_access (user_id, model, enabled)
SELECT u.id, t.model, true FROM users u CROSS JOIN cpa_v8_target_models t
ON CONFLICT (user_id, model) DO UPDATE
SET enabled = true, updated_at = GREATEST(user_model_access.updated_at, now()),
    updated_by_user_id = NULL;

-- An absent row already means 1. Preserve the existing owner attribution,
-- with the migration and before/applied snapshots identifying this reset.
UPDATE billing_model_multipliers m
SET multiplier = 1, updated_at = GREATEST(m.updated_at, now())
FROM cpa_v8_target_models t WHERE m.model = t.model;

-- Retain retired permissions and model history. Current availability is
-- derived from the full configured catalog when the gateway starts.
UPDATE model_access_defaults SET catalog_active = false
WHERE model IN (
    'gemini-3.1-pro-high', 'gemini-3.6-flash-medium', 'gemini-3.7-flash-medium', 'gemini-3.8-flash-medium',
    'gemini-3.1-pro-preview', 'gemini-3.1-pro-preview-customtools', 'gemini-3.1-flash-lite-preview',
    'gemini-3.6-flash', 'gemini-3.7-flash', 'gemini-3.8-flash'
);

UPDATE cpa_v8_model_config_backup b SET applied_value = to_jsonb(d)
FROM model_access_defaults d WHERE b.setting = 'default' AND b.model = d.model;
UPDATE cpa_v8_model_config_backup b SET applied_value = to_jsonb(a)
FROM user_model_access a
WHERE b.setting = 'user' AND b.model = a.model AND b.subject_id = a.user_id::text;
UPDATE cpa_v8_model_config_backup b SET applied_value = to_jsonb(m)
FROM billing_model_multipliers m WHERE b.setting = 'multiplier' AND b.model = m.model;
