-- Accounts map an identity provider's users to local IDs. They are found by provider
-- and subject only, never by email.
CREATE TABLE accounts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider text NOT NULL CHECK (provider <> ''),
    subject text NOT NULL CHECK (subject <> ''),
    -- The provider's verified address, or NULL when there is none.
    email text CHECK (email <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, subject)
);

-- A hash of each deleted identity, so a request that validated its session just
-- before the deletion cannot create the account again.
CREATE TABLE deleted_identities (
    identity_hash bytea PRIMARY KEY,
    deleted_at timestamptz NOT NULL DEFAULT now()
);
