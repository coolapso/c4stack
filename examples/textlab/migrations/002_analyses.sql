-- Analyses, one per account and Idempotency-Key. A retried request gets the stored
-- result without a new charge; the same key with another text is refused. Only a
-- hash of the text is kept.
CREATE TABLE analyses (
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    idempotency_key text NOT NULL,
    request_hash bytea NOT NULL,
    result jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, idempotency_key)
);
