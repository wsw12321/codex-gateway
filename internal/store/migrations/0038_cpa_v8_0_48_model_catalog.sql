-- CPA v8.0.48 model-catalog cutover. Upstream removed gpt-5.5 from every
-- Codex catalog; no model is added. This migration only retires that ID from
-- the manageable catalog and records authorization snapshots. Prices,
-- allowlists, balances, usage, ledger rows, subscription scopes and admission
-- snapshots are deliberately left untouched; pricing is merged by the offline
-- pricing migration and is not a database concern.
LOCK TABLE model_access_defaults IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE billing_model_multipliers IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE cpa_v848_model_config_backup (
    setting TEXT NOT NULL CHECK (setting IN ('default', 'user', 'multiplier')),
    model TEXT NOT NULL,
    subject_id TEXT NOT NULL DEFAULT '',
    before_value JSONB,
    applied_value JSONB,
    captured_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (setting, model, subject_id)
);
COMMENT ON TABLE cpa_v848_model_config_backup IS
    'CPA v8.0.48 cutover snapshots. Rollback compares applied values before restoring and never contains credentials, prices, balances or usage.';

CREATE TEMPORARY TABLE cpa_v848_removed_models (model TEXT PRIMARY KEY) ON COMMIT DROP;
INSERT INTO cpa_v848_removed_models(model) VALUES
    ('gpt-5.5');

-- Capture every row that this cutover can touch. A NULL before_value records
-- that the row did not exist, so the conditional rollback leaves it absent.
INSERT INTO cpa_v848_model_config_backup (setting, model, before_value)
SELECT 'default', r.model, to_jsonb(d)
FROM cpa_v848_removed_models r
LEFT JOIN model_access_defaults d USING (model);

INSERT INTO cpa_v848_model_config_backup (setting, model, subject_id, before_value)
SELECT 'user', r.model, u.id::text, to_jsonb(a)
FROM users u
CROSS JOIN cpa_v848_removed_models r
LEFT JOIN user_model_access a ON a.user_id = u.id AND a.model = r.model;

INSERT INTO cpa_v848_model_config_backup (setting, model, before_value)
SELECT 'multiplier', r.model, to_jsonb(m)
FROM cpa_v848_removed_models r
LEFT JOIN billing_model_multipliers m USING (model);

-- The removed ID remains queryable in historical usage and billing, but is no
-- longer part of the active catalog. Keep the existing default and user
-- authorization values unchanged for a controlled rollback.
UPDATE model_access_defaults d
SET catalog_active = false,
    updated_at = GREATEST(d.updated_at, now()),
    updated_by_user_id = NULL
FROM cpa_v848_removed_models r
WHERE d.model = r.model;

UPDATE cpa_v848_model_config_backup b
SET applied_value = to_jsonb(d)
FROM model_access_defaults d
WHERE b.setting = 'default' AND b.model = d.model;

UPDATE cpa_v848_model_config_backup b
SET applied_value = to_jsonb(a)
FROM user_model_access a
WHERE b.setting = 'user' AND b.model = a.model
  AND b.subject_id = a.user_id::text;

UPDATE cpa_v848_model_config_backup b
SET applied_value = to_jsonb(m)
FROM billing_model_multipliers m
WHERE b.setting = 'multiplier' AND b.model = m.model;

-- A missing multiplier row is the immutable multiplier 1 at admission. Record
-- that effective value so the applied snapshot is complete.
UPDATE cpa_v848_model_config_backup b
SET applied_value = jsonb_build_object('model', b.model, 'multiplier', '1')
WHERE b.setting = 'multiplier' AND b.applied_value IS NULL;
