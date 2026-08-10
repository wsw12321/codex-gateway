-- Personal Codex Gateway initial schema.
-- PostgreSQL 17 is the supported database.  No optional extensions are needed.

CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    webauthn_user_id BYTEA NOT NULL UNIQUE,
    role TEXT NOT NULL DEFAULT 'member',
    status TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at TIMESTAMPTZ,
    last_login_at TIMESTAMPTZ,
    CONSTRAINT users_username_format CHECK (
        username = lower(username)
        AND username ~ '^[a-z0-9][a-z0-9._-]{2,63}$'
    ),
    CONSTRAINT users_webauthn_id_length CHECK (octet_length(webauthn_user_id) = 32),
    CONSTRAINT users_role_valid CHECK (role IN ('owner', 'member')),
    CONSTRAINT users_status_valid CHECK (status IN ('active', 'disabled')),
    CONSTRAINT users_disabled_consistent CHECK (
        (status = 'disabled' AND disabled_at IS NOT NULL)
        OR (status = 'active' AND disabled_at IS NULL)
    ),
    UNIQUE (id, status)
);

CREATE UNIQUE INDEX users_username_lower_key ON users (lower(username));
CREATE UNIQUE INDEX users_one_active_owner_key
    ON users ((role)) WHERE role = 'owner' AND status = 'active';

CREATE TABLE invitations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind TEXT NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE,
    inviter_id UUID REFERENCES users(id) ON DELETE RESTRICT,
    target_user_id UUID REFERENCES users(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ,
    used_by_user_id UUID REFERENCES users(id) ON DELETE RESTRICT,
    revoked_at TIMESTAMPTZ,
    source_ip INET,
    CONSTRAINT invitations_kind_valid CHECK (kind IN ('owner_bootstrap', 'member', 'recovery')),
    CONSTRAINT invitations_hash_length CHECK (octet_length(token_hash) = 32),
    CONSTRAINT invitations_expiry_valid CHECK (
        expires_at > created_at AND expires_at <= created_at + INTERVAL '24 hours'
    ),
    CONSTRAINT invitations_terminal_state CHECK (NOT (used_at IS NOT NULL AND revoked_at IS NOT NULL)),
    CONSTRAINT invitations_used_consistent CHECK (
        (used_at IS NULL AND used_by_user_id IS NULL)
        OR (used_at IS NOT NULL AND used_by_user_id IS NOT NULL)
    ),
    CONSTRAINT invitations_recovery_target CHECK (
        (kind = 'recovery' AND target_user_id IS NOT NULL)
        OR (kind <> 'recovery' AND target_user_id IS NULL)
    ),
    CONSTRAINT invitations_inviter_required CHECK (
        kind = 'owner_bootstrap' OR inviter_id IS NOT NULL
    )
);

CREATE INDEX invitations_active_expiry_idx
    ON invitations (expires_at) WHERE used_at IS NULL AND revoked_at IS NULL;
CREATE INDEX invitations_inviter_idx ON invitations (inviter_id, created_at DESC);

CREATE TABLE webauthn_credentials (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_id BYTEA NOT NULL UNIQUE,
    credential_json BYTEA NOT NULL,
    sign_count BIGINT NOT NULL DEFAULT 0,
    transports TEXT[] NOT NULL DEFAULT '{}',
    backup_eligible BOOLEAN NOT NULL DEFAULT false,
    backup_state BOOLEAN NOT NULL DEFAULT false,
    discoverable BOOLEAN NOT NULL DEFAULT true,
    aaguid UUID,
    nickname TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    CONSTRAINT webauthn_credential_id_nonempty CHECK (octet_length(credential_id) BETWEEN 1 AND 1023),
    CONSTRAINT webauthn_credential_json_nonempty CHECK (octet_length(credential_json) > 0),
    CONSTRAINT webauthn_sign_count_nonnegative CHECK (sign_count >= 0),
    CONSTRAINT webauthn_backup_consistent CHECK (NOT backup_state OR backup_eligible),
    UNIQUE (id, user_id)
);

CREATE INDEX webauthn_credentials_user_idx ON webauthn_credentials (user_id, created_at);

CREATE TABLE recovery_codes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    batch_id UUID NOT NULL,
    code_hash BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    used_at TIMESTAMPTZ,
    CONSTRAINT recovery_codes_hash_length CHECK (octet_length(code_hash) = 32),
    UNIQUE (user_id, code_hash)
);

CREATE INDEX recovery_codes_unused_idx ON recovery_codes (user_id, batch_id)
    WHERE used_at IS NULL;

CREATE TABLE sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash BYTEA NOT NULL UNIQUE,
    csrf_secret BYTEA NOT NULL,
    source_ip INET,
    user_agent_hash BYTEA,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    idle_expires_at TIMESTAMPTZ NOT NULL,
    absolute_expires_at TIMESTAMPTZ NOT NULL,
    recently_verified_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    revoke_reason TEXT NOT NULL DEFAULT '',
    CONSTRAINT sessions_token_hash_length CHECK (octet_length(token_hash) = 32),
    CONSTRAINT sessions_csrf_secret_length CHECK (octet_length(csrf_secret) >= 32),
    CONSTRAINT sessions_user_agent_hash_length CHECK (
        user_agent_hash IS NULL OR octet_length(user_agent_hash) = 32
    ),
    CONSTRAINT sessions_expiry_order CHECK (
        idle_expires_at > created_at
        AND absolute_expires_at > created_at
        AND idle_expires_at <= absolute_expires_at
        AND absolute_expires_at <= created_at + INTERVAL '7 days'
    ),
    UNIQUE (id, user_id)
);

CREATE INDEX sessions_user_active_idx ON sessions (user_id, last_seen_at DESC)
    WHERE revoked_at IS NULL;
CREATE INDEX sessions_expiry_idx ON sessions (LEAST(idle_expires_at, absolute_expires_at))
    WHERE revoked_at IS NULL;

CREATE TABLE devices (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ,
    disabled_at TIMESTAMPTZ,
    CONSTRAINT devices_name_valid CHECK (char_length(name) BETWEEN 1 AND 128),
    CONSTRAINT devices_status_valid CHECK (status IN ('active', 'disabled')),
    CONSTRAINT devices_disabled_consistent CHECK (
        (status = 'disabled' AND disabled_at IS NOT NULL)
        OR (status = 'active' AND disabled_at IS NULL)
    ),
    UNIQUE (id, user_id)
);

CREATE UNIQUE INDEX devices_user_name_key ON devices (user_id, lower(name));
CREATE INDEX devices_user_status_idx ON devices (user_id, status, created_at);

CREATE TABLE projects (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    slug TEXT NOT NULL,
    name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at TIMESTAMPTZ,
    CONSTRAINT projects_slug_format CHECK (
        slug = lower(slug)
        AND slug ~ '^[a-z0-9][a-z0-9_-]{0,62}[a-z0-9]$|^[a-z0-9]$'
    ),
    CONSTRAINT projects_name_valid CHECK (char_length(name) BETWEEN 1 AND 128),
    CONSTRAINT projects_status_valid CHECK (status IN ('active', 'archived')),
    CONSTRAINT projects_archived_consistent CHECK (
        (status = 'archived' AND archived_at IS NOT NULL)
        OR (status = 'active' AND archived_at IS NULL)
    ),
    UNIQUE (id, user_id),
    UNIQUE (user_id, slug)
);

CREATE INDEX projects_user_status_idx ON projects (user_id, status, created_at);

CREATE TABLE api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id TEXT NOT NULL UNIQUE,
    key_prefix TEXT NOT NULL,
    key_hash BYTEA NOT NULL UNIQUE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    device_id UUID NOT NULL,
    default_project_id UUID,
    name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    model_allowlist TEXT[] NOT NULL DEFAULT '{}',
    rpm_limit INTEGER,
    concurrent_limit INTEGER,
    daily_request_limit INTEGER,
    daily_token_limit BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    revoke_reason TEXT NOT NULL DEFAULT '',
    rotated_from_id UUID REFERENCES api_keys(id) ON DELETE SET NULL,
    CONSTRAINT api_keys_device_owner_fk FOREIGN KEY (device_id, user_id)
        REFERENCES devices(id, user_id) ON DELETE RESTRICT,
    CONSTRAINT api_keys_project_owner_fk FOREIGN KEY (default_project_id, user_id)
        REFERENCES projects(id, user_id) ON DELETE RESTRICT,
    CONSTRAINT api_keys_public_id_format CHECK (public_id ~ '^[A-Za-z0-9_-]{8,64}$'),
    CONSTRAINT api_keys_prefix_format CHECK (
        char_length(key_prefix) BETWEEN 8 AND 32
        AND key_prefix NOT LIKE '% %'
    ),
    CONSTRAINT api_keys_hash_length CHECK (octet_length(key_hash) = 32),
    CONSTRAINT api_keys_name_valid CHECK (char_length(name) BETWEEN 1 AND 128),
    CONSTRAINT api_keys_status_valid CHECK (status IN ('active', 'disabled', 'revoked')),
    CONSTRAINT api_keys_expiry_valid CHECK (
        expires_at > created_at AND expires_at <= created_at + INTERVAL '365 days'
    ),
    CONSTRAINT api_keys_revoked_consistent CHECK (
        (status = 'revoked' AND revoked_at IS NOT NULL)
        OR (status <> 'revoked' AND revoked_at IS NULL)
    ),
    CONSTRAINT api_keys_limits_positive CHECK (
        (rpm_limit IS NULL OR rpm_limit > 0)
        AND (concurrent_limit IS NULL OR concurrent_limit > 0)
        AND (daily_request_limit IS NULL OR daily_request_limit > 0)
        AND (daily_token_limit IS NULL OR daily_token_limit > 0)
    ),
    UNIQUE (id, user_id),
    UNIQUE (id, user_id, device_id)
);

CREATE INDEX api_keys_public_active_idx ON api_keys (public_id, expires_at)
    WHERE status = 'active';
CREATE INDEX api_keys_user_device_idx ON api_keys (user_id, device_id, created_at DESC);
CREATE INDEX api_keys_expiry_idx ON api_keys (expires_at) WHERE status = 'active';

CREATE TABLE usage_requests (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id TEXT NOT NULL UNIQUE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    device_id UUID NOT NULL,
    api_key_id UUID NOT NULL,
    key_prefix TEXT NOT NULL,
    project_id UUID,
    model TEXT NOT NULL,
    endpoint TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'in_progress',
    http_status SMALLINT,
    error_code TEXT,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    first_token_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    ttft_ms BIGINT,
    duration_ms BIGINT,
    input_tokens BIGINT NOT NULL DEFAULT 0,
    cached_input_tokens BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    reasoning_tokens BIGINT NOT NULL DEFAULT 0,
    request_bytes BIGINT NOT NULL DEFAULT 0,
    response_bytes BIGINT NOT NULL DEFAULT 0,
    upstream_request_id TEXT,
    CONSTRAINT usage_requests_request_id_valid CHECK (char_length(request_id) BETWEEN 16 AND 128),
    CONSTRAINT usage_requests_key_owner_fk FOREIGN KEY (api_key_id, user_id, device_id)
        REFERENCES api_keys(id, user_id, device_id) ON DELETE RESTRICT,
    CONSTRAINT usage_requests_project_owner_fk FOREIGN KEY (project_id, user_id)
        REFERENCES projects(id, user_id) ON DELETE RESTRICT,
    CONSTRAINT usage_requests_model_nonempty CHECK (char_length(model) BETWEEN 1 AND 128),
    CONSTRAINT usage_requests_endpoint_valid CHECK (
        endpoint IN ('responses', 'responses.compact', 'models')
    ),
    CONSTRAINT usage_requests_state_valid CHECK (
        state IN ('in_progress', 'completed', 'failed', 'cancelled')
    ),
    CONSTRAINT usage_requests_status_valid CHECK (
        http_status IS NULL OR http_status BETWEEN 100 AND 599
    ),
    CONSTRAINT usage_requests_time_order CHECK (
        (first_token_at IS NULL OR first_token_at >= requested_at)
        AND (completed_at IS NULL OR completed_at >= requested_at)
        AND (completed_at IS NULL OR first_token_at IS NULL OR first_token_at <= completed_at)
    ),
    CONSTRAINT usage_requests_metrics_nonnegative CHECK (
        input_tokens >= 0 AND cached_input_tokens >= 0
        AND output_tokens >= 0 AND reasoning_tokens >= 0
        AND request_bytes >= 0 AND response_bytes >= 0
        AND (ttft_ms IS NULL OR ttft_ms >= 0)
        AND (duration_ms IS NULL OR duration_ms >= 0)
        AND cached_input_tokens <= input_tokens
    ),
    CONSTRAINT usage_requests_completion_consistent CHECK (
        (state = 'in_progress' AND completed_at IS NULL)
        OR (state <> 'in_progress' AND completed_at IS NOT NULL)
    )
);

CREATE INDEX usage_requests_requested_idx ON usage_requests (requested_at DESC);
CREATE INDEX usage_requests_user_time_idx ON usage_requests (user_id, requested_at DESC);
CREATE INDEX usage_requests_device_time_idx ON usage_requests (device_id, requested_at DESC);
CREATE INDEX usage_requests_key_time_idx ON usage_requests (api_key_id, requested_at DESC);
CREATE INDEX usage_requests_project_time_idx ON usage_requests (project_id, requested_at DESC);
CREATE INDEX usage_requests_model_time_idx ON usage_requests (model, requested_at DESC);
CREATE INDEX usage_requests_state_time_idx ON usage_requests (state, requested_at DESC);
CREATE INDEX usage_requests_retention_idx ON usage_requests (completed_at)
    WHERE completed_at IS NOT NULL;

CREATE TABLE usage_daily (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    usage_day DATE NOT NULL,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    device_id UUID NOT NULL,
    api_key_id UUID NOT NULL,
    project_id UUID,
    model TEXT NOT NULL,
    endpoint TEXT NOT NULL,
    status_class SMALLINT NOT NULL,
    error_code TEXT,
    request_count BIGINT NOT NULL DEFAULT 0,
    error_count BIGINT NOT NULL DEFAULT 0,
    input_tokens BIGINT NOT NULL DEFAULT 0,
    cached_input_tokens BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    reasoning_tokens BIGINT NOT NULL DEFAULT 0,
    request_bytes BIGINT NOT NULL DEFAULT 0,
    response_bytes BIGINT NOT NULL DEFAULT 0,
    ttft_count BIGINT NOT NULL DEFAULT 0,
    ttft_sum_ms NUMERIC(24,0) NOT NULL DEFAULT 0,
    p95_ttft_ms BIGINT,
    duration_count BIGINT NOT NULL DEFAULT 0,
    duration_sum_ms NUMERIC(24,0) NOT NULL DEFAULT 0,
    p95_duration_ms BIGINT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT usage_daily_key_owner_fk FOREIGN KEY (api_key_id, user_id, device_id)
        REFERENCES api_keys(id, user_id, device_id) ON DELETE RESTRICT,
    CONSTRAINT usage_daily_project_owner_fk FOREIGN KEY (project_id, user_id)
        REFERENCES projects(id, user_id) ON DELETE RESTRICT,
    CONSTRAINT usage_daily_endpoint_valid CHECK (
        endpoint IN ('responses', 'responses.compact', 'models')
    ),
    CONSTRAINT usage_daily_status_class_valid CHECK (status_class BETWEEN 0 AND 5),
    CONSTRAINT usage_daily_metrics_nonnegative CHECK (
        request_count >= 0 AND error_count >= 0 AND error_count <= request_count
        AND input_tokens >= 0 AND cached_input_tokens >= 0
        AND output_tokens >= 0 AND reasoning_tokens >= 0
        AND request_bytes >= 0 AND response_bytes >= 0
        AND ttft_count >= 0 AND ttft_sum_ms >= 0
        AND (p95_ttft_ms IS NULL OR p95_ttft_ms >= 0)
        AND duration_count >= 0 AND duration_sum_ms >= 0
        AND (p95_duration_ms IS NULL OR p95_duration_ms >= 0)
        AND cached_input_tokens <= input_tokens
    ),
    CONSTRAINT usage_daily_dimensions_key UNIQUE NULLS NOT DISTINCT
        (usage_day, user_id, device_id, api_key_id, project_id, model, endpoint, status_class, error_code)
);

CREATE INDEX usage_daily_user_day_idx ON usage_daily (user_id, usage_day DESC);
CREATE INDEX usage_daily_project_day_idx ON usage_daily (project_id, usage_day DESC);
CREATE INDEX usage_daily_model_day_idx ON usage_daily (model, usage_day DESC);

CREATE TABLE usage_monthly (
    usage_month DATE NOT NULL,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    device_id UUID NOT NULL,
    api_key_id UUID NOT NULL,
    project_id UUID,
    model TEXT NOT NULL,
    endpoint TEXT NOT NULL,
    status_class SMALLINT NOT NULL,
    error_code TEXT,
    request_count BIGINT NOT NULL,
    error_count BIGINT NOT NULL,
    input_tokens BIGINT NOT NULL,
    cached_input_tokens BIGINT NOT NULL,
    output_tokens BIGINT NOT NULL,
    reasoning_tokens BIGINT NOT NULL,
    request_bytes BIGINT NOT NULL,
    response_bytes BIGINT NOT NULL,
    p95_ttft_ms BIGINT,
    p95_duration_ms BIGINT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT usage_monthly_month_aligned CHECK (
        usage_month = date_trunc('month', usage_month)::date
    ),
    CONSTRAINT usage_monthly_device_owner_fk FOREIGN KEY (device_id, user_id)
        REFERENCES devices(id, user_id) ON DELETE RESTRICT,
    CONSTRAINT usage_monthly_key_owner_fk FOREIGN KEY (api_key_id, user_id)
        REFERENCES api_keys(id, user_id) ON DELETE RESTRICT,
    CONSTRAINT usage_monthly_project_owner_fk FOREIGN KEY (project_id, user_id)
        REFERENCES projects(id, user_id) ON DELETE RESTRICT,
    CONSTRAINT usage_monthly_endpoint_valid CHECK (
        endpoint IN ('responses', 'responses.compact', 'models')
    ),
    CONSTRAINT usage_monthly_status_class_valid CHECK (status_class BETWEEN 0 AND 5),
    CONSTRAINT usage_monthly_metrics_nonnegative CHECK (
        request_count >= 0 AND error_count >= 0 AND error_count <= request_count
        AND input_tokens >= 0 AND cached_input_tokens >= 0
        AND output_tokens >= 0 AND reasoning_tokens >= 0
        AND request_bytes >= 0 AND response_bytes >= 0
        AND (p95_ttft_ms IS NULL OR p95_ttft_ms >= 0)
        AND (p95_duration_ms IS NULL OR p95_duration_ms >= 0)
        AND cached_input_tokens <= input_tokens
    ),
    CONSTRAINT usage_monthly_dimensions_key UNIQUE NULLS NOT DISTINCT
        (usage_month, user_id, device_id, api_key_id, project_id, model, endpoint, status_class, error_code)
);

CREATE INDEX usage_monthly_user_month_idx ON usage_monthly (user_id, usage_month DESC);
CREATE INDEX usage_monthly_project_month_idx ON usage_monthly (project_id, usage_month DESC);
CREATE INDEX usage_monthly_model_month_idx ON usage_monthly (model, usage_month DESC);

CREATE TABLE audit_events (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_user_id UUID REFERENCES users(id) ON DELETE RESTRICT,
    actor_session_id UUID,
    actor_api_key_id UUID,
    event_type TEXT NOT NULL,
    severity TEXT NOT NULL DEFAULT 'info',
    success BOOLEAN NOT NULL DEFAULT true,
    source_ip INET,
    subject_type TEXT NOT NULL DEFAULT '',
    subject_id TEXT NOT NULL DEFAULT '',
    request_id TEXT,
    metadata JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT audit_events_request_id_valid CHECK (
        request_id IS NULL OR char_length(request_id) BETWEEN 16 AND 128
    ),
    CONSTRAINT audit_events_session_owner_fk FOREIGN KEY (actor_session_id, actor_user_id)
        REFERENCES sessions(id, user_id) ON DELETE SET NULL (actor_session_id),
    CONSTRAINT audit_events_key_owner_fk FOREIGN KEY (actor_api_key_id, actor_user_id)
        REFERENCES api_keys(id, user_id) ON DELETE SET NULL (actor_api_key_id),
    CONSTRAINT audit_events_actor_owner_required CHECK (
        (actor_session_id IS NULL OR actor_user_id IS NOT NULL)
        AND (actor_api_key_id IS NULL OR actor_user_id IS NOT NULL)
    ),
    CONSTRAINT audit_events_type_valid CHECK (event_type ~ '^[a-z0-9_.-]{3,96}$'),
    CONSTRAINT audit_events_severity_valid CHECK (severity IN ('info', 'warning', 'critical')),
    CONSTRAINT audit_events_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE INDEX audit_events_time_idx ON audit_events (occurred_at DESC);
CREATE INDEX audit_events_actor_idx ON audit_events (actor_user_id, occurred_at DESC);
CREATE INDEX audit_events_type_idx ON audit_events (event_type, occurred_at DESC);
CREATE INDEX audit_events_security_idx ON audit_events (severity, occurred_at DESC)
    WHERE severity <> 'info' OR success = false;

CREATE TABLE alerts (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    alert_type TEXT NOT NULL,
    severity TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'open',
    user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    request_id TEXT,
    dedupe_key TEXT,
    title TEXT NOT NULL,
    details JSONB NOT NULL DEFAULT '{}',
    occurrence_count BIGINT NOT NULL DEFAULT 1,
    last_occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    acknowledged_at TIMESTAMPTZ,
    acknowledged_by UUID REFERENCES users(id) ON DELETE SET NULL,
    resolved_at TIMESTAMPTZ,
    CONSTRAINT alerts_request_id_valid CHECK (
        request_id IS NULL OR char_length(request_id) BETWEEN 16 AND 128
    ),
    CONSTRAINT alerts_type_valid CHECK (alert_type ~ '^[a-z0-9_.-]{3,96}$'),
    CONSTRAINT alerts_severity_valid CHECK (severity IN ('info', 'warning', 'critical')),
    CONSTRAINT alerts_status_valid CHECK (status IN ('open', 'acknowledged', 'resolved')),
    CONSTRAINT alerts_details_object CHECK (jsonb_typeof(details) = 'object'),
    CONSTRAINT alerts_count_positive CHECK (occurrence_count > 0),
    CONSTRAINT alerts_state_consistent CHECK (
        (status = 'open' AND acknowledged_at IS NULL AND resolved_at IS NULL)
        OR (status = 'acknowledged' AND acknowledged_at IS NOT NULL AND resolved_at IS NULL)
        OR (status = 'resolved' AND resolved_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX alerts_open_dedupe_key
    ON alerts (dedupe_key) WHERE status <> 'resolved' AND dedupe_key IS NOT NULL;
CREATE INDEX alerts_status_time_idx ON alerts (status, severity, last_occurred_at DESC);

-- quota_locks serializes related key/user/global quota decisions in a fixed order.
-- The application uses SELECT ... FOR UPDATE and never stores secrets here.
CREATE TABLE quota_locks (
    scope_type TEXT NOT NULL,
    scope_id TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT quota_locks_scope_valid CHECK (scope_type IN ('global', 'user', 'key')),
    PRIMARY KEY (scope_type, scope_id)
);

CREATE TABLE quota_counters (
    quota_day DATE NOT NULL,
    scope_type TEXT NOT NULL,
    scope_id UUID NOT NULL,
    requests_reserved BIGINT NOT NULL DEFAULT 0,
    requests_completed BIGINT NOT NULL DEFAULT 0,
    tokens_reserved BIGINT NOT NULL DEFAULT 0,
    tokens_used BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT quota_counters_scope_valid CHECK (scope_type IN ('user', 'key')),
    CONSTRAINT quota_counters_nonnegative CHECK (
        requests_reserved >= 0 AND requests_completed >= 0
        AND requests_completed <= requests_reserved
        AND tokens_reserved >= 0 AND tokens_used >= 0
    ),
    PRIMARY KEY (quota_day, scope_type, scope_id)
);

CREATE INDEX quota_counters_retention_idx ON quota_counters (quota_day);

CREATE TABLE quota_rate_windows (
    window_start TIMESTAMPTZ NOT NULL,
    scope_type TEXT NOT NULL,
    scope_id UUID NOT NULL,
    request_count BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT quota_rate_windows_scope_valid CHECK (scope_type IN ('user', 'key')),
    CONSTRAINT quota_rate_windows_count_nonnegative CHECK (request_count >= 0),
    CONSTRAINT quota_rate_windows_minute_aligned CHECK (
        window_start = date_trunc('minute', window_start)
    ),
    PRIMARY KEY (window_start, scope_type, scope_id)
);

CREATE INDEX quota_rate_windows_retention_idx ON quota_rate_windows (window_start);

CREATE TABLE quota_reservations (
    request_id TEXT PRIMARY KEY,
    quota_day DATE NOT NULL,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    api_key_id UUID NOT NULL,
    reserved_tokens BIGINT NOT NULL,
    actual_tokens BIGINT,
    state TEXT NOT NULL DEFAULT 'reserved',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at TIMESTAMPTZ,
    CONSTRAINT quota_reservations_request_id_valid CHECK (
        char_length(request_id) BETWEEN 16 AND 128
    ),
    CONSTRAINT quota_reservations_key_owner_fk FOREIGN KEY (api_key_id, user_id)
        REFERENCES api_keys(id, user_id) ON DELETE RESTRICT,
    CONSTRAINT quota_reservations_tokens_valid CHECK (
        reserved_tokens >= 0 AND (actual_tokens IS NULL OR actual_tokens >= 0)
    ),
    CONSTRAINT quota_reservations_state_valid CHECK (state IN ('reserved', 'settled', 'released')),
    CONSTRAINT quota_reservations_state_consistent CHECK (
        (state = 'reserved' AND actual_tokens IS NULL AND settled_at IS NULL)
        OR (state = 'settled' AND actual_tokens IS NOT NULL AND settled_at IS NOT NULL)
        OR (state = 'released' AND actual_tokens IS NULL AND settled_at IS NOT NULL)
    )
);

CREATE INDEX quota_reservations_day_idx ON quota_reservations (quota_day, state);
CREATE INDEX quota_reservations_key_idx ON quota_reservations (api_key_id, created_at DESC);

CREATE TABLE concurrency_leases (
    request_id TEXT PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    api_key_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_expires_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT concurrency_leases_request_id_valid CHECK (
        char_length(request_id) BETWEEN 16 AND 128
    ),
    CONSTRAINT concurrency_leases_key_owner_fk FOREIGN KEY (api_key_id, user_id)
        REFERENCES api_keys(id, user_id) ON DELETE RESTRICT,
    CONSTRAINT concurrency_leases_expiry_valid CHECK (lease_expires_at > created_at)
);

CREATE INDEX concurrency_leases_user_idx ON concurrency_leases (user_id, lease_expires_at);
CREATE INDEX concurrency_leases_key_idx ON concurrency_leases (api_key_id, lease_expires_at);
CREATE INDEX concurrency_leases_expiry_idx ON concurrency_leases (lease_expires_at);

CREATE OR REPLACE FUNCTION gateway_set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$;

CREATE TRIGGER users_set_updated_at BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION gateway_set_updated_at();
CREATE TRIGGER devices_set_updated_at BEFORE UPDATE ON devices
    FOR EACH ROW EXECUTE FUNCTION gateway_set_updated_at();
CREATE TRIGGER projects_set_updated_at BEFORE UPDATE ON projects
    FOR EACH ROW EXECUTE FUNCTION gateway_set_updated_at();
CREATE TRIGGER alerts_set_updated_at BEFORE UPDATE ON alerts
    FOR EACH ROW EXECUTE FUNCTION gateway_set_updated_at();

COMMENT ON TABLE usage_requests IS
    'Metadata only. Prompt, source code, response bodies, cookies, API keys and OAuth tokens are forbidden.';
COMMENT ON COLUMN audit_events.metadata IS
    'Non-sensitive structured metadata only; callers must apply an allowlist.';
COMMENT ON COLUMN alerts.details IS
    'Non-sensitive structured metadata only; callers must apply an allowlist.';
