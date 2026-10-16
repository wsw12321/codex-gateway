-- CPA v8.0.30 model-catalog cutover.  This migration only changes the
-- manageable model catalog and its authorization snapshots.  Prices,
-- allowlists, balances, usage, ledger rows and admission snapshots are
-- deliberately left untouched; pricing is merged by the offline pricing
-- migration and is not a database concern.
LOCK TABLE model_access_defaults IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE billing_model_multipliers IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE cpa_v830_model_config_backup (
    setting TEXT NOT NULL CHECK (setting IN ('default', 'user', 'multiplier')),
    model TEXT NOT NULL,
    subject_id TEXT NOT NULL DEFAULT '',
    before_value JSONB,
    applied_value JSONB,
    captured_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (setting, model, subject_id)
);
COMMENT ON TABLE cpa_v830_model_config_backup IS
    'CPA v8.0.30 cutover snapshots. Rollback compares applied values before restoring and never contains credentials, prices, balances or usage.';

CREATE TEMPORARY TABLE cpa_v830_removed_models (model TEXT PRIMARY KEY) ON COMMIT DROP;
INSERT INTO cpa_v830_removed_models(model) VALUES
    ('gemini-pro-agent'),
    ('gemini-3.1-pro-low'),
    ('gemini-3.6-flash-high'),
    ('gemini-3.7-flash-high'),
    ('gemini-3.7-flash');

CREATE TEMPORARY TABLE cpa_v830_added_models (model TEXT PRIMARY KEY) ON COMMIT DROP;
INSERT INTO cpa_v830_added_models(model) VALUES
    ('claude-haiku-5-5'),
    ('gemini-4-argon-high');

-- Capture every row that this cutover can touch.  A NULL before_value records
-- that the row did not exist and lets the conditional rollback disable the
-- newly introduced authorization without deleting historical snapshots.
INSERT INTO cpa_v830_model_config_backup (setting, model, before_value)
SELECT 'default', models.model, to_jsonb(d)
FROM (
    SELECT model FROM cpa_v830_removed_models
    UNION
    SELECT model FROM cpa_v830_added_models
) models
LEFT JOIN model_access_defaults d USING (model);

INSERT INTO cpa_v830_model_config_backup (setting, model, subject_id, before_value)
SELECT 'user', models.model, u.id::text, to_jsonb(a)
FROM users u
CROSS JOIN (
    SELECT model FROM cpa_v830_removed_models
    UNION
    SELECT model FROM cpa_v830_added_models
) models
LEFT JOIN user_model_access a ON a.user_id = u.id AND a.model = models.model;

INSERT INTO cpa_v830_model_config_backup (setting, model, before_value)
SELECT 'multiplier', models.model, to_jsonb(m)
FROM (
    SELECT model FROM cpa_v830_removed_models
    UNION
    SELECT model FROM cpa_v830_added_models
) models
LEFT JOIN billing_model_multipliers m USING (model);

-- Removed upstream IDs remain queryable in historical usage and billing, but
-- are no longer part of the active catalog.  Keep the existing default and
-- user authorization values unchanged for a controlled rollback.
UPDATE model_access_defaults d
SET catalog_active = false,
    updated_at = GREATEST(d.updated_at, now()),
    updated_by_user_id = NULL
FROM cpa_v830_removed_models r
WHERE d.model = r.model;

-- New models follow the same semantics as SyncModelAccessCatalog: a missing
-- default is enabled for new users, while an existing row keeps its explicit
-- enabled choice.  Existing rows are only reactivated in the catalog.
INSERT INTO model_access_defaults (model, enabled, catalog_active)
SELECT model, true, true
FROM cpa_v830_added_models
ON CONFLICT (model) DO UPDATE
SET catalog_active = true;

-- Snapshot authorization for every existing user.  DO NOTHING preserves any
-- explicit owner choice if an interrupted/manual rollout created a row first.
INSERT INTO user_model_access (user_id, model, enabled)
SELECT u.id, d.model, d.enabled
FROM users u
JOIN model_access_defaults d ON d.catalog_active
JOIN cpa_v830_added_models a ON a.model = d.model
ON CONFLICT (user_id, model) DO NOTHING;

-- Multipliers are intentionally not inserted: an absent row is the immutable
-- multiplier 1 at admission, and inserting a row would require inventing an
-- owner identity. Existing rows were captured above and are unchanged; the
-- applied snapshot below records effective 1 for missing rows.

UPDATE cpa_v830_model_config_backup b
SET applied_value = to_jsonb(d)
FROM model_access_defaults d
WHERE b.setting = 'default' AND b.model = d.model;

UPDATE cpa_v830_model_config_backup b
SET applied_value = to_jsonb(a)
FROM user_model_access a
WHERE b.setting = 'user' AND b.model = a.model
  AND b.subject_id = a.user_id::text;

UPDATE cpa_v830_model_config_backup b
SET applied_value = COALESCE(
        to_jsonb(m),
        jsonb_build_object('model', b.model, 'multiplier', '1')
    )
FROM billing_model_multipliers m
WHERE b.setting = 'multiplier' AND b.model = m.model;

-- LEFT JOIN rows with no persisted setting do not participate in the UPDATE
-- above. Record their effective admission multiplier explicitly so the
-- applied snapshot is complete while preserving the missing-row semantics.
UPDATE cpa_v830_model_config_backup b
SET applied_value = jsonb_build_object('model', b.model, 'multiplier', '1')
WHERE b.setting = 'multiplier' AND b.applied_value IS NULL;
