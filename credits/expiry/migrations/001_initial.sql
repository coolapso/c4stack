-- Expiry warnings, at most one per account, each about the account's last activity
-- when it was sent. A warning counts once its provider reports it delivered. Credit
-- then expires no sooner than expires_at: the notice period the warning was sent with,
-- counted from delivery, and never before the date the warning announced. Newer
-- activity makes a warning obsolete. The statements are idempotent, as in every module.
CREATE TABLE IF NOT EXISTS credit_expiry_warnings (
    account_id uuid PRIMARY KEY REFERENCES accounts (id) ON DELETE CASCADE,
    activity_at timestamptz NOT NULL,
    -- The provider's ID for the message, or NULL for a warning recorded without one.
    message_id text,
    sent_at timestamptz NOT NULL,
    notice_days integer NOT NULL CHECK (notice_days > 0),
    announced_expiry timestamptz NOT NULL,
    delivered_at timestamptz,
    expires_at timestamptz,
    CHECK ((delivered_at IS NULL) = (expires_at IS NULL)),
    CHECK (expires_at >= announced_expiry)
);
