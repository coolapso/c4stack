-- Accounts, and the identities that sign in to them. A sign-in finds its account by the
-- identity's provider and subject; a new identity joins the account that has its
-- verified email address. Every statement is idempotent, so the module can adopt
-- these tables when an app created them before it used C4Stack.
CREATE TABLE IF NOT EXISTS accounts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    primary_email text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- deletion_started_at is set before the identity provider is asked to delete the
-- account, and cleared if it refuses. deletion_confirmed_at is set once it confirms.
-- A sign-up with the account's address finishes a confirmed deletion, and waits while
-- one is unconfirmed.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS deletion_started_at timestamptz;
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS deletion_confirmed_at timestamptz;

CREATE TABLE IF NOT EXISTS account_identities (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    provider text NOT NULL,
    subject text NOT NULL,
    email text NOT NULL,
    email_verified boolean NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, subject)
);

CREATE INDEX IF NOT EXISTS idx_account_identities_account_id
ON account_identities (account_id);

-- A hash of each deleted identity, so a request that validated its session just
-- before the deletion cannot create the account again.
CREATE TABLE IF NOT EXISTS deleted_identities (
    identity_hash bytea PRIMARY KEY,
    deleted_at timestamptz NOT NULL DEFAULT now()
);
