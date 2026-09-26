-- Safe task metadata only. Raw prompts, answers and upstream error bodies never
-- enter diagnostic records.
ALTER TABLE model_identifications
    ADD COLUMN run_stage TEXT NOT NULL DEFAULT 'preflight',
    ADD COLUMN run_probe_index SMALLINT NOT NULL DEFAULT 0,
    ADD COLUMN run_updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    ADD COLUMN run_actor_id UUID,
    ADD COLUMN run_error_source TEXT,
    ADD COLUMN run_upstream_status SMALLINT,
    ADD COLUMN run_retry_after INTEGER;

UPDATE model_identifications SET
    run_stage = CASE WHEN run_status = 'succeeded' THEN 'saving' ELSE 'preflight' END,
    run_probe_index = run_progress,
    run_updated_at = COALESCE(run_finished_at, run_started_at);

ALTER TABLE model_identifications
    ALTER COLUMN run_updated_at SET NOT NULL,
    ADD CONSTRAINT model_identifications_diagnostic_metadata_safe CHECK (
        run_stage IN ('preflight', 'probing', 'validating', 'scoring', 'saving')
        AND run_probe_index BETWEEN 0 AND 3
        AND (run_error_source IS NULL OR run_error_source IN
            ('gateway', 'sidecar', 'upstream', 'validation', 'storage'))
        AND (run_upstream_status IS NULL OR run_upstream_status BETWEEN 100 AND 599)
        AND (run_retry_after IS NULL OR run_retry_after BETWEEN 0 AND 3600)
    );
