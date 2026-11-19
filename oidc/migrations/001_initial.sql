-- The OpenID Connect provider's tables. A sign-in request in progress is not stored: it
-- travels sealed in its URL, so nothing is written before someone has signed in. Every
-- statement is idempotent.

-- The keys that sign ID tokens. The private key is sealed with a key derived from the
-- app's secret key, so a copy of the database cannot sign tokens; sealed_with says
-- which secret key sealed it, so keys sealed with a replaced one are never used to sign.
-- Public keys are published from signs_from minus a day to signs_until plus a day.
CREATE TABLE IF NOT EXISTS oidc_keys (
    id text PRIMARY KEY,
    public_key bytea NOT NULL,
    private_key bytea NOT NULL,
    sealed_with bytea NOT NULL,
    signs_from timestamptz NOT NULL,
    signs_until timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (signs_until > signs_from)
);

-- Authorization codes, stored as their SHA-256. A code is taken once: used_at marks it,
-- and reused_at a second attempt, which revokes the access tokens it gave. Rows stay
-- until their access tokens have expired, so a late second attempt is still caught.
CREATE TABLE IF NOT EXISTS oidc_codes (
    code_hash bytea PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    client_id text NOT NULL,
    redirect_uri text NOT NULL,
    scopes text[] NOT NULL,
    nonce text NOT NULL,
    code_challenge text NOT NULL,
    auth_time timestamptz NOT NULL,
    email text NOT NULL,
    email_verified boolean NOT NULL,
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    reused_at timestamptz
);

CREATE INDEX IF NOT EXISTS idx_oidc_codes_account ON oidc_codes (account_id);

CREATE INDEX IF NOT EXISTS idx_oidc_codes_expires ON oidc_codes (expires_at);

-- Access tokens, which only read the userinfo endpoint. The token a client holds is the
-- ID stored here as its SHA-256, encrypted with the subject.
CREATE TABLE IF NOT EXISTS oidc_access_tokens (
    token_hash bytea PRIMARY KEY,
    code_hash bytea NOT NULL,
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    client_id text NOT NULL,
    scopes text[] NOT NULL,
    email text NOT NULL,
    email_verified boolean NOT NULL,
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_oidc_access_tokens_code ON oidc_access_tokens (code_hash);

CREATE INDEX IF NOT EXISTS idx_oidc_access_tokens_account ON oidc_access_tokens (account_id);

CREATE INDEX IF NOT EXISTS idx_oidc_access_tokens_expires ON oidc_access_tokens (expires_at);
