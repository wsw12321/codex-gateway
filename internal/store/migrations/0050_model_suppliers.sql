ALTER TABLE upstream_accounts ADD COLUMN credential_kind text NOT NULL DEFAULT 'oauth'
  CHECK (credential_kind IN ('oauth', 'supplier'));

CREATE TABLE model_suppliers (
  account_id text PRIMARY KEY REFERENCES upstream_accounts(id),
  base_url text NOT NULL,
  key_ciphertext bytea NOT NULL,
  models jsonb NOT NULL CHECK (jsonb_typeof(models) = 'array' AND jsonb_array_length(models) > 0),
  revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0)
);
