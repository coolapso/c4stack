-- Usage captured through C4Stack refers to its reservation by the ledger reference, so
-- reservation_id becomes optional for usage. Other kinds still never have one.
ALTER TABLE credit_ledger
DROP CONSTRAINT credit_ledger_usage_shape;

ALTER TABLE credit_ledger
ADD CONSTRAINT credit_ledger_usage_shape CHECK (
    (
        kind = 'usage'
        AND payment_intent_id IS NULL
        AND provider_event_id IS NULL
        AND amount_cents < 0
    )
    OR (kind <> 'usage' AND reservation_id IS NULL)
);
