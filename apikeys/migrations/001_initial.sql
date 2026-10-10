-- API keys. Only a hash of each key is stored, plus its prefix and last characters
-- for display; the raw key is shown once and never stored.
CREATE TABLE api_keys (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
    prefix text NOT NULL,
    suffix text NOT NULL,
    hash bytea NOT NULL UNIQUE CHECK (length(hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at timestamptz
);

CREATE INDEX api_keys_account_created ON api_keys (account_id, created_at DESC);
