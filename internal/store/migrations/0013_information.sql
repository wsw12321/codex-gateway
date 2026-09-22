-- Cleanup is explicit, durable, and resumable. Deployment never creates a job.
CREATE TABLE information_cleanup_jobs (
    id UUID PRIMARY KEY,
    actor_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    retention_days INTEGER NOT NULL CHECK (retention_days > 0),
    cutoff TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','completed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    report JSONB NOT NULL DEFAULT '{}',
    last_error TEXT NOT NULL DEFAULT '',
    CHECK (cutoff = date_trunc('day', cutoff AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'),
    CHECK ((status = 'completed') = (completed_at IS NOT NULL))
);
CREATE UNIQUE INDEX information_cleanup_one_active ON information_cleanup_jobs ((true))
    WHERE status IN ('pending','running');

-- A tombstone holds neither money nor a user reference. It permanently rejects
-- reuse of a removed billing operation ID instead of replaying its effects.
CREATE TABLE billing_operation_tombstones (
    operation_id UUID PRIMARY KEY,
    cleaned_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Only exact rows approved inside a running cleanup transaction may be
-- deleted. A session flag alone can never disable the immutable-row guards.
CREATE TABLE information_cleanup_authorizations (
    transaction_id BIGINT NOT NULL,
    job_id UUID NOT NULL REFERENCES information_cleanup_jobs(id) ON DELETE RESTRICT,
    table_name TEXT NOT NULL CHECK (table_name IN
        ('billing_ledger_entries','billing_subscription_operation_snapshots')),
    row_id BIGINT NOT NULL,
    PRIMARY KEY (transaction_id, table_name, row_id)
);
CREATE OR REPLACE FUNCTION gateway_reject_billing_ledger_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE approved_id BIGINT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF TG_TABLE_NAME = 'billing_ledger_entries' THEN
            approved_id := OLD.id;
        ELSIF TG_TABLE_NAME = 'billing_subscription_operation_snapshots' THEN
            approved_id := OLD.ledger_entry_id;
        END IF;
        IF EXISTS (
            SELECT 1 FROM information_cleanup_authorizations a
            JOIN information_cleanup_jobs j ON j.id = a.job_id
            WHERE a.transaction_id = txid_current()
              AND a.table_name = TG_TABLE_NAME AND a.row_id = approved_id
              AND j.status = 'running'
        ) THEN RETURN OLD; END IF;
    END IF;
    RAISE EXCEPTION 'billing ledger entries are immutable' USING ERRCODE = '55000';
END;
$$;

-- Both ends of this immutable operation/result pair are removed together.
-- Deferral removes the cycle without allowing an intermediate UPDATE.
ALTER TABLE billing_operations DROP CONSTRAINT billing_operations_result_ledger_fk;
ALTER TABLE billing_operations ADD CONSTRAINT billing_operations_result_ledger_fk
    FOREIGN KEY (result_ledger_entry_id, operation_id)
    REFERENCES billing_ledger_entries(id, operation_id) DEFERRABLE INITIALLY IMMEDIATE;
ALTER TABLE billing_ledger_entries DROP CONSTRAINT billing_ledger_entries_operation_id_fkey;
ALTER TABLE billing_ledger_entries ADD CONSTRAINT billing_ledger_entries_operation_id_fkey
    FOREIGN KEY (operation_id) REFERENCES billing_operations(operation_id)
    DEFERRABLE INITIALLY IMMEDIATE;

CREATE INDEX billing_ledger_retention_idx ON billing_ledger_entries (created_at, id);
CREATE INDEX billing_cash_lots_retention_idx ON billing_cash_credit_lots (created_at, id)
    WHERE remaining_usd = 0;
