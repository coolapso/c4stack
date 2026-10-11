-- Credit expires after 12 months without account activity, at least 30 days
-- after a warning email. Account deletion forfeits the remaining balance.
-- Both are recorded as ledger debits so the yearly donation total is the sum
-- of expiry and forfeit entries.
ALTER TABLE credit_ledger
DROP CONSTRAINT credit_ledger_kind_check;

ALTER TABLE credit_ledger
ADD CONSTRAINT credit_ledger_kind_check CHECK (
    kind IN ('top_up', 'usage', 'refund', 'chargeback', 'adjustment', 'expiry', 'forfeit')
);

ALTER TABLE credit_ledger
ADD CONSTRAINT credit_ledger_unused_credit_shape CHECK (
    kind NOT IN ('expiry', 'forfeit')
    OR (
        amount_cents < 0
        AND payment_intent_id IS NULL
        AND provider_event_id IS NULL
        AND reservation_id IS NULL
    )
);

ALTER TABLE accounts
ADD COLUMN credit_expiry_warned_at TIMESTAMPTZ;
