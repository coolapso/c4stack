package marketplace

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Result counts what one run of the hourly job did. The next run tries again whatever
// failed.
type Result struct {
	// Skipped means another run held the lock, so this one did nothing.
	Skipped bool
	// SecretsWiped counts provisions whose sealed credentials passed their deadline
	// and were wiped.
	SecretsWiped int
	// Expired counts unclaimed accounts that went unused for InactiveMonths and were
	// deleted, and ExpiredCents the credit that expired with them.
	Expired      int
	ExpiredCents int64
	// Failed counts accounts whose expiry failed.
	Failed int
}

// lockKey identifies the advisory lock that keeps runs from overlapping, across
// replicas and deployments that share the database.
const lockKey = 739_084_311_982_417_603

const batchSize = 100

// Run wipes sealed credentials past their deadline and expires unclaimed accounts that
// went unused for InactiveMonths, once, and reports the outcome to Config.OnRun.
func (m *Marketplace) Run(ctx context.Context) (Result, error) {
	r, err := m.run(ctx)
	if m.cfg.OnRun != nil {
		m.cfg.OnRun(ctx, r, err)
	}
	return r, err
}

func (m *Marketplace) run(ctx context.Context) (Result, error) {
	conn, err := m.app.DB().Conn(ctx)
	if err != nil {
		return Result{}, err
	}
	defer conn.Close()
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, int64(lockKey)).Scan(&locked); err != nil {
		return Result{}, err
	}
	if !locked {
		return Result{Skipped: true}, nil
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(lockKey))
	}()

	now := m.now().UTC()
	var r Result
	res, err := m.app.DB().ExecContext(ctx, `
		UPDATE marketplace_operations
		SET secret = NULL, secret_until = NULL
		WHERE secret IS NOT NULL AND secret_until <= $1`, now,
	)
	if err != nil {
		return r, err
	}
	wiped, _ := res.RowsAffected()
	r.SecretsWiped = int(wiped)
	return r, m.expireAll(ctx, now, &r)
}

// inactive is an unclaimed grant whose account went unused for $1 months before $2.
const inactive = `
	g.account_id IS NOT NULL AND g.claimed_at IS NULL AND g.ended_at IS NULL AND NOT g.test
	AND ` + lastActivity + ` + make_interval(months => $1) <= $2`

// expireAll expires every inactive unclaimed account.
func (m *Marketplace) expireAll(ctx context.Context, now time.Time, r *Result) error {
	after := ""
	for {
		batch, err := m.inactiveGrants(ctx, now, after)
		if err != nil {
			return err
		}
		for _, id := range batch {
			cents, done, err := m.expire(ctx, now, id)
			switch {
			case err != nil:
				r.Failed++
				m.app.Log().Warn("expiring an unclaimed marketplace account failed", "grant", id, "error", err)
			case done:
				r.Expired++
				r.ExpiredCents += cents
			}
		}
		if len(batch) < batchSize {
			return nil
		}
		after = batch[len(batch)-1]
	}
}

// inactiveGrants returns the next batch of inactive unclaimed grants, after the grant
// after.
func (m *Marketplace) inactiveGrants(ctx context.Context, now time.Time, after string) ([]string, error) {
	rows, err := m.app.DB().QueryContext(ctx, `
		SELECT g.id
		FROM marketplace_grants g
		WHERE `+inactive+` AND g.id::text > $3
		ORDER BY g.id::text
		LIMIT $4`, m.cfg.InactiveMonths, now, after, batchSize,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var batch []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		batch = append(batch, id)
	}
	return batch, rows.Err()
}

// expire checks again, under the grant's and the account's locks, that the account is
// still unclaimed and unused, and that the app holds none of its credit; then it expires
// the credit and deletes the account. It returns the credit expired and whether the
// account ended.
func (m *Marketplace) expire(ctx context.Context, now time.Time, grantRef string) (int64, bool, error) {
	tx, err := m.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	var g grant
	err = tx.QueryRowContext(ctx, `
		SELECT g.id, g.account_id
		FROM marketplace_grants g
		WHERE g.id = $3 AND `+inactive+`
		FOR UPDATE OF g`, m.cfg.InactiveMonths, now, grantRef,
	).Scan(&g.id, &g.accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil // used, claimed or ended meanwhile
	}
	if err != nil {
		return 0, false, err
	}
	t, err := m.credits.Lock(ctx, tx, g.accountID.String)
	if err != nil {
		return 0, false, err
	}
	var still bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM marketplace_grants g
		    WHERE g.id = $3 AND `+inactive+`
		)`, m.cfg.InactiveMonths, now, grantRef,
	).Scan(&still); err != nil || !still {
		return 0, false, err // used while the lock was awaited
	}
	balance, err := t.Balance()
	if err != nil {
		return 0, false, err
	}
	avail, err := t.Available()
	if err != nil {
		return 0, false, err
	}
	if balance > 0 && avail < balance {
		return 0, false, nil // held for work in progress: the next run tries again
	}
	var expired int64
	if balance > 0 {
		if err := t.Expire(balance); err != nil {
			return 0, false, err
		}
		expired = balance
	}
	if err := m.end(ctx, tx, g); err != nil {
		return 0, false, err
	}
	return expired, true, tx.Commit()
}
