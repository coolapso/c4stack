-- Changes the module makes to subscriptions at the provider. Every statement is
-- idempotent.

-- A change made at once is recorded when the provider has made it (applied_at), so a
-- refund the provider makes for it, of what was paid for the time left, is known as
-- the change's and doesn't go to review. A change that waits for the end of the period
-- paid for (Config.ChangeAtPeriodEnd) has run_at, the period's end, and is made
-- shortly before then; until it is made (applied_at) or dropped (canceled_at), it is
-- the subscription's pending change, at most one. from_product is the product it
-- changes from: a subscription that changed some other way meanwhile drops it.
CREATE TABLE IF NOT EXISTS subscription_changes (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    subscription_id uuid NOT NULL REFERENCES subscriptions (id) ON DELETE CASCADE,
    from_product text NOT NULL,
    product text NOT NULL,
    units integer NOT NULL CHECK (units > 0),
    run_at timestamptz,
    applied_at timestamptz,
    canceled_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_subscription_changes_pending
ON subscription_changes (subscription_id)
WHERE applied_at IS NULL AND canceled_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_subscription_changes_due
ON subscription_changes (run_at)
WHERE applied_at IS NULL AND canceled_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_subscription_changes_applied
ON subscription_changes (subscription_id, applied_at)
WHERE applied_at IS NOT NULL;
