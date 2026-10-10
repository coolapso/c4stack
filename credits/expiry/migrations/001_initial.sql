-- Expiry warnings in progress, one per account. A row is about one moment of last
-- activity: newer activity makes it obsolete. It is deleted when the credit expires.
CREATE TABLE credit_expiry_notices (
    account_id uuid PRIMARY KEY REFERENCES accounts (id) ON DELETE CASCADE,
    activity_at timestamptz NOT NULL,
    attempted_at timestamptz NOT NULL,
    warned_at timestamptz,
    expires_at timestamptz,
    CHECK ((warned_at IS NULL) = (expires_at IS NULL))
);
