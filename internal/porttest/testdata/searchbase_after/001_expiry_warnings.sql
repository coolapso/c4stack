-- Warnings Searchbase sent before the move become the expiry module's, delivered when
-- Searchbase sent them, with the deadline Searchbase announced.
INSERT INTO credit_expiry_warnings (
    account_id, activity_at, sent_at, announced_expiry, delivered_at, expires_at
)
SELECT a.id, c.last_activity, a.credit_expiry_warned_at,
       GREATEST(c.last_activity + interval '12 months', a.credit_expiry_warned_at + interval '30 days'),
       a.credit_expiry_warned_at,
       GREATEST(c.last_activity + interval '12 months', a.credit_expiry_warned_at + interval '30 days')
FROM accounts a
JOIN (
    SELECT account_id,
           max(created_at) FILTER (WHERE kind IN ('top_up', 'usage', 'adjustment')) AS last_activity
    FROM credit_ledger
    WHERE account_id IS NOT NULL
    GROUP BY account_id
) c ON c.account_id = a.id
WHERE a.credit_expiry_warned_at >= c.last_activity
ON CONFLICT (account_id) DO NOTHING;
