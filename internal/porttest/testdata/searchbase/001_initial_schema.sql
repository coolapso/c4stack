CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    primary_email TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS account_identities (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    subject TEXT NOT NULL,
    email TEXT NOT NULL,
    email_verified BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(provider, subject)
);

CREATE TABLE IF NOT EXISTS api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (length(trim(name)) > 0),
    key_hash BYTEA NOT NULL UNIQUE,
    key_suffix TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS payment_intents (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID REFERENCES accounts(id) ON DELETE SET NULL,
    provider TEXT NOT NULL DEFAULT 'creem' CHECK (provider = 'creem'),
    provider_checkout_id TEXT UNIQUE,
    provider_order_id TEXT UNIQUE,
    idempotency_key UUID NOT NULL UNIQUE,
    credit_amount_cents BIGINT NOT NULL CHECK (credit_amount_cents >= 500),
    fee_amount_cents BIGINT NOT NULL CHECK (fee_amount_cents >= 0),
    checkout_amount_cents BIGINT NOT NULL CHECK (
        checkout_amount_cents = credit_amount_cents + fee_amount_cents
    ),
    paid_amount_cents BIGINT CHECK (paid_amount_cents >= checkout_amount_cents),
    currency CHAR(3) NOT NULL DEFAULT 'EUR' CHECK (currency = 'EUR'),
    provider_status TEXT NOT NULL DEFAULT 'created' CHECK (provider_status IN (
        'created', 'pending', 'paid', 'refunded', 'charged_back', 'failed', 'canceled', 'expired'
    )),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS credit_ledger (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID REFERENCES accounts(id) ON DELETE SET NULL,
    payment_intent_id UUID REFERENCES payment_intents(id) ON DELETE SET NULL,
    provider_event_id TEXT UNIQUE,
    amount_cents BIGINT NOT NULL CHECK (amount_cents <> 0),
    kind TEXT NOT NULL CHECK (kind IN ('top_up', 'usage', 'refund', 'chargeback', 'adjustment')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_account_identities_account_id
ON account_identities(account_id);

CREATE INDEX IF NOT EXISTS idx_api_keys_account_created
ON api_keys(account_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_payment_intents_account_created
ON payment_intents(account_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_credit_ledger_account_created
ON credit_ledger(account_id, created_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS idx_credit_ledger_top_up_per_payment
ON credit_ledger(payment_intent_id) WHERE kind = 'top_up';

CREATE UNIQUE INDEX IF NOT EXISTS idx_credit_ledger_reversal_per_payment
ON credit_ledger(payment_intent_id) WHERE kind IN ('refund', 'chargeback');
