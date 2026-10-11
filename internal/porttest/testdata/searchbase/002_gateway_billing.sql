CREATE TABLE service_prices (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    operation TEXT NOT NULL CHECK (operation IN ('search', 'fetch', 'fetch_js')),
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    effective_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(operation, effective_at)
);

CREATE FUNCTION reject_service_price_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'service_prices rows are append-only' USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER service_prices_append_only
BEFORE UPDATE OR DELETE ON service_prices
FOR EACH ROW EXECUTE FUNCTION reject_service_price_mutation();

CREATE TABLE credit_reservations (
    id UUID PRIMARY KEY,
    account_id UUID REFERENCES accounts(id) ON DELETE SET NULL,
    api_key_id UUID REFERENCES api_keys(id) ON DELETE SET NULL,
    service_price_id UUID NOT NULL REFERENCES service_prices(id) ON DELETE RESTRICT,
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'captured', 'released', 'expired')),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    settled_at TIMESTAMPTZ,
    CHECK (expires_at > created_at),
    CHECK (
        (status = 'pending' AND settled_at IS NULL)
        OR (status IN ('captured', 'released', 'expired') AND settled_at IS NOT NULL)
    )
);

ALTER TABLE credit_ledger
ADD COLUMN reservation_id UUID REFERENCES credit_reservations(id) ON DELETE RESTRICT;

ALTER TABLE credit_ledger
ADD CONSTRAINT credit_ledger_reservation_unique UNIQUE (reservation_id);

ALTER TABLE credit_ledger
ADD CONSTRAINT credit_ledger_usage_shape CHECK (
    (
        kind = 'usage'
        AND reservation_id IS NOT NULL
        AND payment_intent_id IS NULL
        AND provider_event_id IS NULL
        AND amount_cents < 0
    )
    OR (kind <> 'usage' AND reservation_id IS NULL)
);

CREATE TABLE usage_events (
    reservation_id UUID PRIMARY KEY REFERENCES credit_reservations(id) ON DELETE RESTRICT,
    request_id UUID NOT NULL,
    trace_id TEXT CHECK (
        trace_id IS NULL
        OR (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> repeat('0', 32))
    ),
    operation TEXT NOT NULL CHECK (operation IN ('search', 'fetch', 'fetch_js')),
    transport TEXT NOT NULL CHECK (transport IN ('rest', 'mcp_sse', 'mcp_streamable_http')),
    outcome TEXT NOT NULL CHECK (outcome IN ('success', 'failure')),
    error_class TEXT CHECK (error_class IS NULL OR error_class ~ '^[a-z][a-z0-9_]{0,62}$'),
    status_class SMALLINT CHECK (status_class IS NULL OR status_class BETWEEN 1 AND 5),
    duration_ms BIGINT NOT NULL CHECK (duration_ms >= 0),
    reserved_amount_cents BIGINT NOT NULL CHECK (reserved_amount_cents > 0),
    charged_amount_cents BIGINT NOT NULL CHECK (
        charged_amount_cents >= 0 AND charged_amount_cents <= reserved_amount_cents
    ),
    country_code TEXT CHECK (country_code IS NULL OR country_code ~ '^[A-Z]{2}$'),
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (
        (
            outcome = 'success'
            AND error_class IS NULL
            AND status_class = 2
            AND charged_amount_cents = reserved_amount_cents
        )
        OR (
            outcome = 'failure'
            AND error_class IS NOT NULL
            AND charged_amount_cents = 0
        )
    )
);

CREATE INDEX idx_api_keys_active_hash
ON api_keys(key_hash) WHERE revoked_at IS NULL;

CREATE INDEX idx_credit_reservations_live_account
ON credit_reservations(account_id, expires_at)
INCLUDE (amount_cents)
WHERE status = 'pending' AND account_id IS NOT NULL;

CREATE INDEX idx_credit_reservations_expiry
ON credit_reservations(expires_at, id) WHERE status = 'pending';

CREATE INDEX idx_credit_reservations_account_history
ON credit_reservations(account_id, settled_at DESC NULLS LAST, id DESC)
WHERE account_id IS NOT NULL;

CREATE INDEX idx_credit_reservations_api_key
ON credit_reservations(api_key_id) WHERE api_key_id IS NOT NULL;

CREATE INDEX idx_credit_ledger_account_created_id
ON credit_ledger(account_id, created_at DESC, id DESC)
INCLUDE (amount_cents, kind)
WHERE account_id IS NOT NULL;
