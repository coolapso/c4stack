-- A purchase the server priced. Payment webhooks credit what this row says, never what
-- the webhook says. account_id becomes NULL when the account is deleted.
CREATE TABLE payment_intents (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid REFERENCES accounts (id) ON DELETE SET NULL,
    provider text NOT NULL,
    credit_cents bigint NOT NULL CHECK (credit_cents > 0),
    fee_cents bigint NOT NULL CHECK (fee_cents >= 0),
    charge_cents bigint NOT NULL CHECK (charge_cents = credit_cents + fee_cents),
    currency text NOT NULL DEFAULT 'EUR' CHECK (currency = 'EUR'),
    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'paid', 'refunded', 'disputed')),
    checkout_id text,
    order_id text,
    paid_cents bigint CHECK (paid_cents >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    paid_at timestamptz,
    reversed_at timestamptz,
    UNIQUE (provider, checkout_id)
);

CREATE INDEX payment_intents_account ON payment_intents (account_id, created_at DESC);
CREATE INDEX payment_intents_order ON payment_intents (provider, order_id);

-- Every change to an account's credit, in EUR cents. Entries are never changed or
-- deleted; corrections are new entries. The one exception: deleting the account sets
-- account_id to NULL and clears the description.
CREATE TABLE credit_ledger (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id uuid REFERENCES accounts (id) ON DELETE SET NULL,
    intent_id uuid REFERENCES payment_intents (id),
    kind text NOT NULL
        CHECK (kind IN ('purchase', 'refund', 'dispute', 'debit', 'adjustment', 'expiry', 'forfeit')),
    amount_cents bigint NOT NULL CHECK (amount_cents <> 0),
    reference text NOT NULL CHECK (char_length(reference) BETWEEN 1 AND 128),
    description text NOT NULL DEFAULT '' CHECK (char_length(description) <= 160),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (CASE kind
        WHEN 'purchase' THEN amount_cents > 0 AND intent_id IS NOT NULL
        WHEN 'refund' THEN amount_cents < 0 AND intent_id IS NOT NULL
        WHEN 'dispute' THEN amount_cents < 0 AND intent_id IS NOT NULL
        WHEN 'adjustment' THEN intent_id IS NULL
        ELSE amount_cents < 0 AND intent_id IS NULL
    END)
);

CREATE INDEX credit_ledger_account ON credit_ledger (account_id, id DESC);
-- A reference is used once per account, which makes every mutation idempotent.
CREATE UNIQUE INDEX credit_ledger_reference ON credit_ledger (account_id, reference)
    WHERE account_id IS NOT NULL;
-- A payment is credited at most once, and reversed at most once.
CREATE UNIQUE INDEX credit_ledger_purchase ON credit_ledger (intent_id)
    WHERE kind = 'purchase';
CREATE UNIQUE INDEX credit_ledger_reversal ON credit_ledger (intent_id)
    WHERE kind IN ('refund', 'dispute');

CREATE FUNCTION credit_ledger_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE'
        AND OLD.account_id IS NOT NULL
        AND NEW.account_id IS NULL
        AND NEW.id = OLD.id
        AND NEW.intent_id IS NOT DISTINCT FROM OLD.intent_id
        AND NEW.kind = OLD.kind
        AND NEW.amount_cents = OLD.amount_cents
        AND NEW.reference = OLD.reference
        AND NEW.created_at = OLD.created_at
    THEN
        NEW.description := '';
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'credit_ledger entries cannot be changed or deleted';
END
$$;

CREATE TRIGGER credit_ledger_append_only
    BEFORE UPDATE OR DELETE ON credit_ledger
    FOR EACH ROW EXECUTE FUNCTION credit_ledger_append_only();

-- Payment events that could not be applied as they stand. An operator decides what to
-- do and resolves them with the credits commands.
CREATE TABLE payment_reviews (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider text NOT NULL,
    event_id text NOT NULL,
    intent_id uuid REFERENCES payment_intents (id),
    reason text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    resolution text,
    UNIQUE (provider, event_id)
);

CREATE INDEX payment_reviews_open ON payment_reviews (created_at) WHERE resolved_at IS NULL;
