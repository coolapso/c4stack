-- When the account's latest expiry warning was delivered. A warning covers the
-- activity before it; newer activity needs a new one. The statement is idempotent, so
-- the module can adopt the column when an app added it before it used C4Stack.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credit_expiry_warned_at timestamptz;
