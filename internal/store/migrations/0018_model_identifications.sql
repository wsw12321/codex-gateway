-- A single recent result and current run state per stable account and requested model.
-- Probe answers, response hashes, and upstream usage are intentionally absent.
CREATE TABLE model_identifications (
    account_id TEXT NOT NULL REFERENCES upstream_accounts(id) ON DELETE CASCADE,
    requested_model TEXT NOT NULL,
    conclusion TEXT,
    closest_model TEXT,
    match_level TEXT,
    fit DOUBLE PRECISION,
    margin DOUBLE PRECISION,
    reference_version TEXT,
    completed_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    run_id UUID NOT NULL,
    run_status TEXT NOT NULL,
    run_progress SMALLINT NOT NULL DEFAULT 0,
    run_started_at TIMESTAMPTZ NOT NULL,
    run_finished_at TIMESTAMPTZ,
    run_error_code TEXT,
    PRIMARY KEY (account_id, requested_model),
    CONSTRAINT model_identifications_requested_model_safe CHECK (
        char_length(requested_model) BETWEEN 1 AND 200
        AND requested_model !~ '[[:cntrl:]]'
    ),
    CONSTRAINT model_identifications_result_values_safe CHECK (
        (conclusion IS NULL OR (char_length(conclusion) BETWEEN 1 AND 200
                                AND conclusion !~ '[[:cntrl:]]'))
        AND (closest_model IS NULL OR (char_length(closest_model) BETWEEN 1 AND 200
                                    AND closest_model !~ '[[:cntrl:]]'))
        AND (match_level IS NULL OR match_level ~ '^[a-z][a-z0-9_]{0,63}$')
        AND (reference_version IS NULL OR (char_length(reference_version) BETWEEN 1 AND 128
                                        AND reference_version !~ '[[:cntrl:]]'))
        AND (run_error_code IS NULL OR run_error_code ~ '^[a-z][a-z0-9_]{0,63}$')
    ),
    CONSTRAINT model_identifications_result_complete CHECK (
        (conclusion IS NULL AND closest_model IS NULL AND match_level IS NULL AND fit IS NULL
         AND margin IS NULL AND reference_version IS NULL
         AND completed_at IS NULL AND expires_at IS NULL)
        OR
        (conclusion IS NOT NULL AND closest_model IS NOT NULL AND match_level IS NOT NULL
         AND fit IS NOT NULL AND margin IS NOT NULL
         AND reference_version IS NOT NULL AND completed_at IS NOT NULL
         AND expires_at IS NOT NULL AND expires_at > completed_at)
    ),
    CONSTRAINT model_identifications_status_valid CHECK (
        run_status IN ('running', 'succeeded', 'failed')
        AND run_progress BETWEEN 0 AND 3
        AND ((run_status = 'running' AND run_finished_at IS NULL
              AND run_error_code IS NULL)
          OR (run_status = 'succeeded' AND run_finished_at IS NOT NULL
              AND run_error_code IS NULL AND run_progress = 3)
          OR (run_status = 'failed' AND run_finished_at IS NOT NULL
              AND run_error_code IS NOT NULL))
    )
);

-- A partial unique index makes the one-running-run rule independent of
-- which Gateway replica accepted the request.
CREATE UNIQUE INDEX model_identifications_one_running_idx
    ON model_identifications (run_status) WHERE run_status = 'running';

CREATE UNIQUE INDEX model_identifications_run_id_idx
    ON model_identifications (run_id);

CREATE INDEX model_identifications_expires_at_idx
    ON model_identifications (expires_at) WHERE expires_at IS NOT NULL;

COMMENT ON TABLE model_identifications IS
    'Latest statistical model match and current diagnostic run state only; never store probe prompts or answers here.';
