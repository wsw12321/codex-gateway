-- Preferences only control the funding sources bound to new requests. They do
-- not alter existing reservations, subscription periods, or monetary history.
ALTER TABLE billing_accounts
    ADD COLUMN day_source_disabled BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN week_source_disabled BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN month_source_disabled BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN cash_source_disabled BOOLEAN NOT NULL DEFAULT false;
