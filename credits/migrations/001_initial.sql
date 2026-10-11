-- Credit purchases and the credit ledger. Every statement is idempotent, so the module
-- can adopt these tables when an app created them before it used C4Stack.

-- A purchase the server priced. Payment webhooks credit what this row says, never what
-- the webhook says. account_id becomes NULL when the account is deleted.
CREATE TABLE IF NOT EXISTS payment_intents (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid REFERENCES accounts (id) ON DELETE SET NULL,
    provider text NOT NULL,
    provider_checkout_id text UNIQUE,
    provider_order_id text UNIQUE,
    -- A random key unique to the purchase.
    idempotency_key uuid NOT NULL UNIQUE,
    credit_amount_cents bigint NOT NULL CHECK (credit_amount_cents > 0),
    fee_amount_cents bigint NOT NULL CHECK (fee_amount_cents >= 0),
    checkout_amount_cents bigint NOT NULL CHECK (
        checkout_amount_cents = credit_amount_cents + fee_amount_cents
    ),
    paid_amount_cents bigint CHECK (paid_amount_cents >= checkout_amount_cents),
    currency char(3) NOT NULL DEFAULT 'EUR' CHECK (currency = 'EUR'),
    provider_status text NOT NULL DEFAULT 'created' CHECK (provider_status IN (
        'created', 'pending', 'paid', 'refunded', 'charged_back', 'failed', 'canceled', 'expired'
    )),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    paid_at timestamptz
);

CREATE INDEX IF NOT EXISTS idx_payment_intents_account_created
ON payment_intents (account_id, created_at DESC);

-- Every change to an account's credit, in EUR cents. Entries are never changed or
-- deleted; corrections are new entries. The one exception: deleting the account sets
-- account_id to NULL, and the trigger below clears the description.
CREATE TABLE IF NOT EXISTS credit_ledger (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid REFERENCES accounts (id) ON DELETE SET NULL,
    payment_intent_id uuid REFERENCES payment_intents (id) ON DELETE SET NULL,
    provider_event_id text UNIQUE,
    amount_cents bigint NOT NULL CHECK (amount_cents <> 0),
    kind text NOT NULL CHECK (kind IN (
        'top_up', 'usage', 'refund', 'chargeback', 'adjustment', 'expiry', 'forfeit'
    )),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT credit_ledger_shape CHECK (CASE kind
        WHEN 'top_up' THEN amount_cents > 0 AND payment_intent_id IS NOT NULL
        WHEN 'refund' THEN amount_cents < 0 AND payment_intent_id IS NOT NULL
        WHEN 'chargeback' THEN amount_cents < 0 AND payment_intent_id IS NOT NULL
        WHEN 'adjustment' THEN payment_intent_id IS NULL AND provider_event_id IS NULL
        ELSE amount_cents < 0 AND payment_intent_id IS NULL AND provider_event_id IS NULL
    END)
);

-- The reference makes the app's debits and an operator's adjustments idempotent per
-- account. The description says what an entry was for.
ALTER TABLE credit_ledger
ADD COLUMN IF NOT EXISTS reference text CHECK (char_length(reference) BETWEEN 1 AND 128);

ALTER TABLE credit_ledger
ADD COLUMN IF NOT EXISTS description text NOT NULL DEFAULT '' CHECK (char_length(description) <= 160);

CREATE UNIQUE INDEX IF NOT EXISTS credit_ledger_reference
ON credit_ledger (account_id, reference)
WHERE account_id IS NOT NULL AND reference IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_credit_ledger_account_created_id
ON credit_ledger (account_id, created_at DESC, id DESC)
INCLUDE (amount_cents, kind)
WHERE account_id IS NOT NULL;

-- A payment is credited at most once, and reversed at most once.
CREATE UNIQUE INDEX IF NOT EXISTS idx_credit_ledger_top_up_per_payment
ON credit_ledger (payment_intent_id) WHERE kind = 'top_up';

CREATE UNIQUE INDEX IF NOT EXISTS idx_credit_ledger_reversal_per_payment
ON credit_ledger (payment_intent_id) WHERE kind IN ('refund', 'chargeback');

CREATE OR REPLACE FUNCTION credit_ledger_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE'
        AND OLD.account_id IS NOT NULL
        AND NEW.account_id IS NULL
        AND NEW.id = OLD.id
        AND NEW.payment_intent_id IS NOT DISTINCT FROM OLD.payment_intent_id
        AND NEW.provider_event_id IS NOT DISTINCT FROM OLD.provider_event_id
        AND NEW.kind = OLD.kind
        AND NEW.amount_cents = OLD.amount_cents
        AND NEW.reference IS NOT DISTINCT FROM OLD.reference
        AND NEW.created_at = OLD.created_at
    THEN
        NEW.description := '';
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'credit_ledger entries cannot be changed or deleted';
END
$$;

DROP TRIGGER IF EXISTS credit_ledger_append_only ON credit_ledger;

CREATE TRIGGER credit_ledger_append_only
BEFORE UPDATE OR DELETE ON credit_ledger
FOR EACH ROW EXECUTE FUNCTION credit_ledger_append_only();

-- Payment events that could not be applied as they stand. An operator decides what to
-- do and resolves them with the credits commands.
CREATE TABLE IF NOT EXISTS payment_reviews (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider text NOT NULL,
    event_id text NOT NULL,
    payment_intent_id uuid REFERENCES payment_intents (id),
    reason text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    resolution text,
    UNIQUE (provider, event_id)
);

CREATE INDEX IF NOT EXISTS payment_reviews_open
ON payment_reviews (created_at) WHERE resolved_at IS NULL;
