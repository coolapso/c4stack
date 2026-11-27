-- Credit sold on a payment provider's marketplace, such as Creem's Agentic Marketplace,
-- where AI agents buy it and receive an API key. Every statement is idempotent.

-- A grant is one buyer's access: the account a provision opened, its API key and the
-- link that claims it. account_id follows the account when its owner claims it, and
-- becomes NULL when the account is deleted. ended_at records when an unclaimed account
-- ended: revoked, expired or a test.
CREATE TABLE IF NOT EXISTS marketplace_grants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider text NOT NULL,
    grant_id text NOT NULL,
    account_id uuid REFERENCES accounts (id) ON DELETE SET NULL,
    key_id uuid REFERENCES api_keys (id) ON DELETE SET NULL,
    order_id text,
    claim_token_hash bytea UNIQUE,
    test boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    claimed_at timestamptz,
    revoked_at timestamptz,
    ended_at timestamptz,
    UNIQUE (provider, grant_id)
);

CREATE INDEX IF NOT EXISTS marketplace_grants_account
ON marketplace_grants (account_id) WHERE account_id IS NOT NULL;

-- Every request answered, by its operation, so a repeat gets the same answer. answer
-- holds the reply without credentials; secret holds a provision's credentials, sealed,
-- until secret_until, the request's deadline, and then nothing.
CREATE TABLE IF NOT EXISTS marketplace_operations (
    provider text NOT NULL,
    operation_id text NOT NULL,
    grant_ref uuid REFERENCES marketplace_grants (id) ON DELETE CASCADE,
    event_id text NOT NULL,
    action text NOT NULL CHECK (action IN ('provision', 'top_up', 'revoke', 'revoke_credit')),
    order_id text,
    status text NOT NULL CHECK (status IN ('ready', 'failed')),
    answer jsonb NOT NULL,
    secret bytea,
    secret_until timestamptz,
    test boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, operation_id),
    CHECK ((secret IS NULL) = (secret_until IS NULL))
);

CREATE INDEX IF NOT EXISTS marketplace_operations_grant
ON marketplace_operations (grant_ref, created_at);

CREATE INDEX IF NOT EXISTS marketplace_operations_secret
ON marketplace_operations (secret_until) WHERE secret IS NOT NULL;
