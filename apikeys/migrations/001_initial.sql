-- API keys. Only the SHA-256 of each key is stored, plus its last characters for
-- display; the raw key is shown once and never stored. Every statement is idempotent,
-- so the module can adopt the table when an app created it before it used C4Stack.
CREATE TABLE IF NOT EXISTS api_keys (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    name text NOT NULL CHECK (length(trim(name)) > 0),
    key_hash bytea NOT NULL UNIQUE,
    key_suffix text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at timestamptz
);

CREATE INDEX IF NOT EXISTS idx_api_keys_account_created
ON api_keys (account_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_api_keys_active_hash
ON api_keys (key_hash) WHERE revoked_at IS NULL;
