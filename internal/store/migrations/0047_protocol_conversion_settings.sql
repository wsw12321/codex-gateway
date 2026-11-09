-- Owner switch for gateway-visible protocol conversion. When
-- claude_responses_enabled is false (the default), claude-* models are
-- accepted only on native /v1/messages; OpenAI Responses requests for them,
-- including user model routes to a Claude target, are rejected before
-- admission. Antigravity Gemini through /v1/responses is not governed here.
CREATE TABLE protocol_conversion_settings (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    claude_responses_enabled BOOLEAN NOT NULL DEFAULT false,
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO protocol_conversion_settings(singleton) VALUES (true);
