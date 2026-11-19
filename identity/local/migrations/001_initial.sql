-- The users, sessions, passkeys and codes of C4Stack's own sign-in, and what keeps it
-- safe: the limits' counters, the passkey challenges already answered, and the security
-- notices waiting to be sent. Every statement is idempotent.

-- One user per address that confirmed a code. The address is stored in lower case.
CREATE TABLE IF NOT EXISTS local_users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email text NOT NULL UNIQUE,
    -- Device cookies issued before this don't count.
    devices_since timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS local_sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES local_users (id) ON DELETE CASCADE,
    -- SHA-256 of the cookie's token.
    token_hash bytea NOT NULL UNIQUE,
    -- How it signed in: 'code' or 'passkey'.
    method text NOT NULL,
    -- A label made from the user agent at sign-in, such as "Firefox on Linux".
    device text NOT NULL,
    -- The sign-in.
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_local_sessions_user_id
ON local_sessions (user_id);

CREATE TABLE IF NOT EXISTS local_passkeys (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES local_users (id) ON DELETE CASCADE,
    credential_id bytea NOT NULL UNIQUE,
    -- COSE.
    public_key bytea NOT NULL,
    sign_count bigint NOT NULL,
    aaguid uuid,
    transports text[] NOT NULL DEFAULT '{}',
    backup_eligible boolean NOT NULL,
    backed_up boolean NOT NULL,
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz
);

CREATE INDEX IF NOT EXISTS idx_local_passkeys_user_id
ON local_passkeys (user_id);

-- Codes waiting to be entered. A code is deleted when it is used, replaced or expired,
-- and with its user.
CREATE TABLE IF NOT EXISTS local_codes (
    id uuid PRIMARY KEY,
    -- 'sign_in' or 'new_email'.
    purpose text NOT NULL,
    -- Where it was sent.
    email text NOT NULL,
    -- sign_in: the address's user when the code was sent, if it had one.
    -- new_email: the user whose address changes.
    user_id uuid REFERENCES local_users (id) ON DELETE CASCADE,
    -- HMAC of the code's ID and the code.
    code_hash bytea NOT NULL,
    -- A change to an address another user has: the code is never sent and never matches.
    decoy boolean NOT NULL DEFAULT false,
    -- Which counters a wrong entry counts against: the address's shared ones when
    -- empty, else this browser's own.
    device text NOT NULL DEFAULT '',
    -- sign_in: the browser's attempt. A new code for the attempt ends its others.
    attempt text NOT NULL DEFAULT '',
    wrong_entries integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_local_codes_user_id
ON local_codes (user_id);

CREATE INDEX IF NOT EXISTS idx_local_codes_attempt
ON local_codes (attempt)
WHERE attempt <> '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_local_codes_new_email
ON local_codes (user_id)
WHERE purpose = 'new_email';

-- The limits' counters, in fixed windows that start at their first event. key is an
-- HMAC of the limit's name and what it counts, so the table holds no addresses.
CREATE TABLE IF NOT EXISTS local_limits (
    key bytea PRIMARY KEY,
    count integer NOT NULL,
    window_ends_at timestamptz NOT NULL
);

-- Passkey challenges already answered, kept until they expire.
CREATE TABLE IF NOT EXISTS local_used_challenges (
    challenge_hash bytea PRIMARY KEY,
    expires_at timestamptz NOT NULL
);

-- Security notices, written in the transaction that made the change. A notice is
-- deleted once the email provider accepts it, or after 3 days.
CREATE TABLE IF NOT EXISTS local_notices (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- For logs. No foreign key, so deleting the user doesn't drop a notice of a change
    -- made just before.
    user_id uuid NOT NULL,
    kind text NOT NULL,
    recipient text NOT NULL,
    -- What the email says: addresses, a passkey's name, the device, times.
    data jsonb NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_local_notices_next_attempt_at
ON local_notices (next_attempt_at);
