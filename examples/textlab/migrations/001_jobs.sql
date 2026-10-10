-- Text jobs. A pending job holds its reserved credit until it completes.
CREATE TABLE jobs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    input text NOT NULL,
    reserved_cents bigint NOT NULL CHECK (reserved_cents > 0),
    charged_cents bigint CHECK (charged_cents >= 0),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done')),
    created_at timestamptz NOT NULL DEFAULT now(),
    done_at timestamptz
);

CREATE INDEX jobs_account ON jobs (account_id, created_at DESC);
CREATE INDEX jobs_pending ON jobs (account_id) WHERE status = 'pending';
