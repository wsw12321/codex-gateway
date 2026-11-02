-- SS credentials are encrypted by Gateway; OAuth credentials remain in CPA.
CREATE TABLE egress_state (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0)
);
INSERT INTO egress_state(singleton) VALUES (true);

CREATE TABLE egress_servers (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    server TEXT NOT NULL CHECK (length(server) BETWEEN 1 AND 253),
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    cipher TEXT NOT NULL CHECK (cipher IN ('aes-128-gcm','aes-256-gcm','chacha20-ietf-poly1305')),
    password_ciphertext BYTEA NOT NULL CHECK (octet_length(password_ciphertext) > 28),
    udp BOOLEAN NOT NULL DEFAULT false,
    revision BIGINT NOT NULL CHECK (revision > 0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE upstream_account_egress (
    account_id TEXT PRIMARY KEY REFERENCES upstream_accounts(id) ON DELETE RESTRICT,
    node_id UUID REFERENCES egress_servers(id) ON DELETE RESTRICT,
    revision BIGINT NOT NULL CHECK (revision > 0)
);
CREATE INDEX upstream_account_egress_node_idx ON upstream_account_egress(node_id);

-- Onboarding intent survives Gateway/CPA restarts. No OAuth tokens are stored here.
CREATE TABLE egress_registrations (
    id UUID PRIMARY KEY,
    provider TEXT NOT NULL CHECK (provider IN ('codex','antigravity','anthropic')),
    -- Immutable intent survives deletion after an abandoned attempt's grace
    -- period. A delayed registration must fail rather than silently go global.
    node_id UUID,
    preserve_binding BOOLEAN NOT NULL DEFAULT true,
    account_id TEXT REFERENCES upstream_accounts(id) ON DELETE RESTRICT,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','prepared','committed')),
    revision BIGINT NOT NULL CHECK (revision > 0),
    actor_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    baseline_binding_revision BIGINT NOT NULL DEFAULT 0 CHECK (baseline_binding_revision >= 0),
    actor_session_id UUID REFERENCES sessions(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (NOT preserve_binding OR node_id IS NULL),
    CHECK ((state='pending' AND account_id IS NULL) OR (state IN ('prepared','committed') AND account_id IS NOT NULL))
);
CREATE INDEX egress_registrations_pending_idx ON egress_registrations(state,created_at);
