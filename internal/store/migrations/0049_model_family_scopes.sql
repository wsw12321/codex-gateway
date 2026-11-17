-- Billing model scopes may name a whole model family ("family:gpt",
-- "family:gemini", "family:claude") alongside exact model IDs; a family covers
-- the models priced later as well. The Gateway classifies models the way it
-- routes them (internal/config/model_family.go).
--
-- One-time conversion. The catalog read here is the one the previous binary
-- synchronized, i.e. the catalog the Owner saved these scopes against. A scope
-- that already names every active model of a family gains that family's token;
-- its exact models are kept, so the scope only ever grows. Rows are judged one
-- by one, so a subscription and its current entitlement convert alike. Plan
-- snapshots inside entitlements and ledger rows are history and stay as they
-- were. The family rule is frozen here: claude-* and gemini-* by prefix,
-- every other model except the internal governance model is GPT.
DO $$
DECLARE
    family RECORD;
    changed BIGINT;
    total BIGINT := 0;
    counts JSONB := '{}'::jsonb;
BEGIN
    FOR family IN
        SELECT m.family AS id, 'family:' || m.family AS token,
            jsonb_agg(m.model ORDER BY m.model COLLATE "C") AS members
        FROM (
            SELECT model, CASE
                WHEN model LIKE 'claude-%' THEN 'claude'
                WHEN model LIKE 'gemini-%' THEN 'gemini'
                ELSE 'gpt' END AS family
            FROM model_access_defaults
            WHERE catalog_active AND model <> 'codex-auto-review' AND model NOT LIKE 'family:%'
        ) m
        GROUP BY m.family
        ORDER BY m.family
    LOOP
        -- Renewal forms carry the configuration version; a changed scope is a
        -- new configuration.
        UPDATE billing_subscriptions SET
            model_ids = (SELECT jsonb_agg(v ORDER BY v COLLATE "C")
                FROM jsonb_array_elements_text(model_ids || to_jsonb(family.token)) AS e(v)),
            config_version = nextval('billing_subscription_config_versions')
        WHERE models_initialized AND NOT model_ids ? family.token AND model_ids @> family.members;
        GET DIAGNOSTICS changed = ROW_COUNT;
        IF changed > 0 THEN
            counts := jsonb_set(counts, ARRAY['subscriptions'], COALESCE(counts->'subscriptions', '{}'::jsonb) || jsonb_build_object(family.id, changed));
            total := total + changed;
        END IF;

        UPDATE billing_subscription_entitlements SET
            model_ids = (SELECT jsonb_agg(v ORDER BY v COLLATE "C")
                FROM jsonb_array_elements_text(model_ids || to_jsonb(family.token)) AS e(v))
        WHERE superseded_at IS NULL AND NOT model_ids ? family.token AND model_ids @> family.members;
        GET DIAGNOSTICS changed = ROW_COUNT;
        IF changed > 0 THEN
            counts := jsonb_set(counts, ARRAY['entitlements'], COALESCE(counts->'entitlements', '{}'::jsonb) || jsonb_build_object(family.id, changed));
            total := total + changed;
        END IF;

        -- A purchase must quote the current plan version, as after an Owner edit.
        UPDATE billing_plans SET
            model_ids = (SELECT jsonb_agg(v ORDER BY v COLLATE "C")
                FROM jsonb_array_elements_text(model_ids || to_jsonb(family.token)) AS e(v)),
            version = version + 1,
            updated_at = now()
        WHERE deleted_at IS NULL AND NOT model_ids ? family.token AND model_ids @> family.members;
        GET DIAGNOSTICS changed = ROW_COUNT;
        IF changed > 0 THEN
            counts := jsonb_set(counts, ARRAY['plans'], COALESCE(counts->'plans', '{}'::jsonb) || jsonb_build_object(family.id, changed));
            total := total + changed;
        END IF;

        UPDATE user_groups SET
            model_ids = (SELECT jsonb_agg(v ORDER BY v COLLATE "C")
                FROM jsonb_array_elements_text(model_ids || to_jsonb(family.token)) AS e(v))
        WHERE models_initialized AND NOT model_ids ? family.token AND model_ids @> family.members;
        GET DIAGNOSTICS changed = ROW_COUNT;
        IF changed > 0 THEN
            counts := jsonb_set(counts, ARRAY['groups'], COALESCE(counts->'groups', '{}'::jsonb) || jsonb_build_object(family.id, changed));
            total := total + changed;
        END IF;

        UPDATE billing_settings SET
            model_ids = (SELECT jsonb_agg(v ORDER BY v COLLATE "C")
                FROM jsonb_array_elements_text(model_ids || to_jsonb(family.token)) AS e(v))
        WHERE singleton AND models_initialized AND NOT model_ids ? family.token AND model_ids @> family.members;
        GET DIAGNOSTICS changed = ROW_COUNT;
        IF changed > 0 THEN
            counts := jsonb_set(counts, ARRAY['cash'], COALESCE(counts->'cash', '{}'::jsonb) || jsonb_build_object(family.id, changed));
            total := total + changed;
        END IF;
    END LOOP;

    IF total > 0 THEN
        INSERT INTO audit_events(event_type, subject_type, subject_id, metadata)
        VALUES ('billing.model_families_converted', 'billing_model_scopes', '0049_model_family_scopes',
            jsonb_build_object('rows', total, 'scopes', counts));
    END IF;
END
$$;
