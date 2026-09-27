ALTER TABLE usage_requests
    DROP CONSTRAINT usage_requests_endpoint_valid,
    ADD CONSTRAINT usage_requests_endpoint_valid CHECK (
        endpoint IN ('responses', 'responses.compact', 'models',
                     'gemini.generateContent', 'gemini.streamGenerateContent')
    );

ALTER TABLE usage_daily
    DROP CONSTRAINT usage_daily_endpoint_valid,
    ADD CONSTRAINT usage_daily_endpoint_valid CHECK (
        endpoint IN ('responses', 'responses.compact', 'models',
                     'gemini.generateContent', 'gemini.streamGenerateContent')
    );

ALTER TABLE usage_monthly
    DROP CONSTRAINT usage_monthly_endpoint_valid,
    ADD CONSTRAINT usage_monthly_endpoint_valid CHECK (
        endpoint IN ('responses', 'responses.compact', 'models',
                     'gemini.generateContent', 'gemini.streamGenerateContent')
    );
