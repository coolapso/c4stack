-- Credit moved between accounts, such as a claimed marketplace account's to its owner's.
-- One transfer entry takes the balance from one account and another adds it to the other,
-- both with the same reference.
ALTER TABLE credit_ledger
DROP CONSTRAINT IF EXISTS credit_ledger_kind_check;

ALTER TABLE credit_ledger
ADD CONSTRAINT credit_ledger_kind_check CHECK (kind IN (
    'top_up', 'usage', 'refund', 'chargeback', 'adjustment', 'expiry', 'forfeit', 'transfer'
));

-- A ledger an app created before it used C4Stack may lack the shape constraint; it keeps
-- its own constraints, which allow transfers.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'credit_ledger'::regclass AND conname = 'credit_ledger_shape'
    ) THEN
        ALTER TABLE credit_ledger DROP CONSTRAINT credit_ledger_shape;
        ALTER TABLE credit_ledger ADD CONSTRAINT credit_ledger_shape CHECK (CASE kind
            WHEN 'top_up' THEN amount_cents > 0 AND payment_intent_id IS NOT NULL
            WHEN 'refund' THEN amount_cents < 0 AND payment_intent_id IS NOT NULL
            WHEN 'chargeback' THEN amount_cents < 0 AND payment_intent_id IS NOT NULL
            WHEN 'adjustment' THEN payment_intent_id IS NULL AND provider_event_id IS NULL
            WHEN 'transfer' THEN payment_intent_id IS NULL AND provider_event_id IS NULL
                AND reference IS NOT NULL
            ELSE amount_cents < 0 AND payment_intent_id IS NULL AND provider_event_id IS NULL
        END);
    END IF;
END
$$;
