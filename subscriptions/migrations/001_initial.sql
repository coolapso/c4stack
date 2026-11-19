-- Subscriptions and their webhook events. Every statement is idempotent.

-- A subscription an account started. It is 'pending' from the checkout until the
-- provider confirms it, then follows the provider's state. product is the app's
-- product ID; target is what the subscription is for, in the app's words, such as a
-- vault. state_at is the provider's time of the state shown, so an older event that
-- arrives late never overwrites it. account_id becomes NULL when the account is
-- deleted: the record stays, without its link.
CREATE TABLE IF NOT EXISTS subscriptions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid REFERENCES accounts (id) ON DELETE SET NULL,
    provider text NOT NULL,
    provider_subscription_id text,
    provider_customer_id text,
    product text NOT NULL,
    target text NOT NULL DEFAULT '' CHECK (char_length(target) <= 128),
    units integer NOT NULL CHECK (units > 0),
    status text NOT NULL CHECK (status IN (
        'pending', 'active', 'trialing', 'past_due', 'unpaid', 'paused', 'canceling', 'canceled'
    )),
    period_end timestamptz,
    state_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_subscription_id)
);

CREATE INDEX IF NOT EXISTS idx_subscriptions_account
ON subscriptions (account_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_subscriptions_pending
ON subscriptions (created_at) WHERE status = 'pending';

-- Webhook events taken, so each is applied once.
CREATE TABLE IF NOT EXISTS subscription_events (
    provider text NOT NULL,
    event_id text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, event_id)
);

-- Events that could not be applied as they stand. An operator decides what to do and
-- resolves them with the subscriptions commands.
CREATE TABLE IF NOT EXISTS subscription_reviews (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider text NOT NULL,
    event_id text NOT NULL,
    subscription_id uuid REFERENCES subscriptions (id) ON DELETE SET NULL,
    reason text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    resolution text,
    UNIQUE (provider, event_id)
);

CREATE INDEX IF NOT EXISTS idx_subscription_reviews_open
ON subscription_reviews (created_at) WHERE resolved_at IS NULL;
