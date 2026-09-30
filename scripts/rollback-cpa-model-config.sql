-- Run with psql -v ON_ERROR_STOP=1 during the maintenance window, using the
-- compatible rollback binary and complete restored pricing catalog. Restore
-- only values that were not changed by an Owner after the CPA v8 migration.
BEGIN;
LOCK TABLE model_access_defaults IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE billing_model_multipliers IN SHARE ROW EXCLUSIVE MODE;

UPDATE model_access_defaults d
SET enabled = COALESCE((b.before_value->>'enabled')::boolean, false),
    updated_by_user_id = (b.before_value->>'updated_by_user_id')::uuid,
    updated_at = GREATEST(d.updated_at, now())
FROM cpa_v8_model_config_backup b
WHERE b.setting = 'default' AND b.model = d.model
  AND (to_jsonb(d) - 'catalog_active') = (b.applied_value - 'catalog_active');

UPDATE user_model_access a
SET enabled = COALESCE((b.before_value->>'enabled')::boolean, false),
    updated_by_user_id = (b.before_value->>'updated_by_user_id')::uuid,
    updated_at = GREATEST(a.updated_at, now())
FROM cpa_v8_model_config_backup b
WHERE b.setting = 'user' AND b.model = a.model AND b.subject_id = a.user_id::text
  AND to_jsonb(a) = b.applied_value;

UPDATE billing_model_multipliers m
SET multiplier = (b.before_value->>'multiplier')::numeric,
    updated_by_user_id = (b.before_value->>'updated_by_user_id')::uuid,
    updated_at = GREATEST(m.updated_at, now())
FROM cpa_v8_model_config_backup b
WHERE b.setting = 'multiplier' AND b.model = m.model
  AND b.before_value IS NOT NULL AND to_jsonb(m) = b.applied_value;

-- No new multiplier rows are inserted by the upgrade. Missing values still
-- mean 1, and new values saved since cutover must remain untouched.
COMMIT;
