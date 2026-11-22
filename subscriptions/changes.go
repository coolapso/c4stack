package subscriptions

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/coolapso/c4stack/payments"
)

// errNoPeriodEnd means a change that waits for the end of the period can't be
// recorded: the provider hasn't said when it ends.
var errNoPeriodEnd = errors.New("subscriptions: the provider hasn't said when the period ends")

const (
	// changeLead is how long before the end of its period a change that waits for it
	// is made: before the provider renews the subscription, which would charge the old
	// product for another period. What is left of the period, under an hour, is
	// refunded as for any change.
	changeLead = time.Hour
	// changeCheck is how often the module looks for changes due, well within
	// changeLead.
	changeCheck = 5 * time.Minute
	// refundWindow is how close a refund must be to a change the module made, by the
	// provider's time and the database's, to be that change's: a provider refunds a
	// change's unused time as it makes the change. Others go to review.
	refundWindow = 10 * time.Minute
)

// PendingChange is a change that waits for the end of the period paid for
// (Config.ChangeAtPeriodEnd).
type PendingChange struct {
	Product Product
	Units   int
	// At is the end of the period. The module makes the change in the hour before it.
	At time.Time
}

// Quote is what a change would do: when it happens, what it charges or refunds then,
// and what the subscription costs after it.
type Quote struct {
	// Product and Units are what the subscription would have.
	Product Product
	Units   int
	// At is zero for a change made at once. For a change that waits for the end of the
	// period (Config.ChangeAtPeriodEnd), it is the period's end: nothing is charged
	// before then.
	At time.Time
	// Priced is true when the provider quoted the amounts, as a payments.ChangeQuoter
	// does; they are zero otherwise.
	Priced bool
	// ChangeQuote is the provider's quote for the change, made now or at At.
	payments.ChangeQuote
}

// QuoteChange says what Change would do with the same request, without changing
// anything, so people can see what they agree to: when the change happens, and, when
// the provider quotes changes, what it charges or refunds and what the subscription
// costs after it. A change to what the subscription has already quotes nothing to pay.
func (s *Subscriptions) QuoteChange(ctx context.Context, accountID, id string, r ChangeRequest) (Quote, error) {
	if accountID == "" {
		return Quote{}, ErrNotFound
	}
	var providerID sql.NullString
	sub, err := s.scanLive(s.app.DB().QueryRowContext(ctx, `
		SELECT `+columns+`, s.provider_subscription_id
		FROM `+withPending+`
		WHERE s.id::text = $1 AND s.account_id::text = $2 AND s.status <> 'pending'`, id, accountID,
	), &providerID)
	if err != nil {
		return Quote{}, err
	}
	target, units, err := s.target(sub, r)
	if err != nil {
		return Quote{}, err
	}
	q := Quote{Product: target, Units: units}
	if s.waits(sub.Product, target) {
		current, err := s.payments.Subscription(ctx, providerID.String)
		if err != nil {
			return Quote{}, err
		}
		if current.PeriodEnd.IsZero() {
			return Quote{}, errNoPeriodEnd
		}
		q.At = current.PeriodEnd
	}
	quoter, ok := s.payments.(payments.ChangeQuoter)
	if !ok {
		return q, nil
	}
	ch := payments.SubscriptionChange{Units: units, Proration: s.proration}
	if target.ID != sub.Product.ID {
		ch.ProductID = target.ProviderID
	}
	if q.ChangeQuote, err = quoter.QuoteChange(ctx, providerID.String, ch, q.At); err != nil {
		return Quote{}, err
	}
	q.Priced = true
	return q, nil
}

// scanLive scans a live subscription with its provider's ID, which a quote needs.
func (s *Subscriptions) scanLive(row *sql.Row, providerID *sql.NullString) (Subscription, error) {
	var sub Subscription
	var product, status string
	var periodEnd, runAt sql.NullTime
	var pending sql.NullString
	var pendingUnits sql.NullInt64
	err := row.Scan(&sub.ID, &sub.AccountID, &product, &sub.Target, &sub.Units, &status, &periodEnd, &sub.CreatedAt,
		&pending, &pendingUnits, &runAt, providerID)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	if err != nil {
		return Subscription{}, err
	}
	sub.Product = s.product(product)
	sub.Status = payments.SubscriptionStatus(status)
	sub.PeriodEnd = periodEnd.Time
	if pending.Valid {
		sub.Pending = &PendingChange{Product: s.product(pending.String), Units: int(pendingUnits.Int64), At: runAt.Time}
	}
	switch {
	case !providerID.Valid:
		return Subscription{}, ErrNotFound
	case !sub.Live():
		return Subscription{}, ErrNotLive
	}
	return sub, nil
}

// dropPending drops a subscription's pending change, if it has one.
func dropPending(ctx context.Context, tx *sql.Tx, subscriptionID string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE subscription_changes
		SET canceled_at = now()
		WHERE subscription_id = $1 AND applied_at IS NULL AND canceled_at IS NULL`, subscriptionID,
	)
	return err
}

// dueChange is a pending change whose period ends within changeLead.
type dueChange struct {
	id             int64
	subscriptionID string
}

// applyDue makes the changes whose period ends within changeLead.
func (s *Subscriptions) applyDue(ctx context.Context) error {
	all, err := s.dueChanges(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range all {
		if err := s.applyPending(ctx, d.id, d.subscriptionID); err != nil {
			s.app.Log().Error("making a subscription change that waited for the end of its period failed; it is tried again",
				"subscription_id", d.subscriptionID, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Subscriptions) dueChanges(ctx context.Context) (_ []dueChange, err error) {
	rows, err := s.app.DB().QueryContext(ctx, `
		SELECT id, subscription_id::text
		FROM subscription_changes
		WHERE applied_at IS NULL AND canceled_at IS NULL AND run_at <= now() + make_interval(secs => $1)
		ORDER BY run_at
		LIMIT 100`, changeLead.Seconds(),
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	var all []dueChange
	for rows.Next() {
		var d dueChange
		if err := rows.Scan(&d.id, &d.subscriptionID); err != nil {
			return nil, err
		}
		all = append(all, d)
	}
	return all, rows.Err()
}

// applyPending makes one change that waited. A subscription that ended, or changed some
// other way meanwhile, drops it. One that the provider already renewed still gets it,
// and the provider refunds the new period's unused time.
func (s *Subscriptions) applyPending(ctx context.Context, changeID int64, subscriptionID string) error {
	l, err := s.lockAny(ctx, "", subscriptionID)
	switch {
	case errors.Is(err, ErrBusy):
		// A change to it is going through: the next check sees what it left.
		return nil
	case errors.Is(err, ErrNotLive), errors.Is(err, ErrNotFound):
		_, err := s.app.DB().ExecContext(ctx, `
			UPDATE subscription_changes
			SET canceled_at = now()
			WHERE id = $1 AND applied_at IS NULL AND canceled_at IS NULL`, changeID,
		)
		return err
	case err != nil:
		return err
	}
	defer func() { _ = l.tx.Rollback() }()
	var from, product string
	var units int
	var runAt time.Time
	err = l.tx.QueryRowContext(ctx, `
		SELECT from_product, product, units, run_at
		FROM subscription_changes
		WHERE id = $1 AND applied_at IS NULL AND canceled_at IS NULL`, changeID,
	).Scan(&from, &product, &units, &runAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	log := s.app.Log().With("subscription_id", subscriptionID)
	target, sold := s.products[product]
	if l.sub.Product.ID != from || !sold {
		log.Warn("dropped a subscription change that waited: the subscription changed some other way, or the app no longer sells its product",
			"from", from, "now", l.sub.Product.ID, "to", product)
		if err := dropPending(ctx, l.tx, l.sub.ID); err != nil {
			return err
		}
		return l.tx.Commit()
	}
	if l.sub.PeriodEnd.After(runAt.Add(changeLead)) {
		log.Warn("a subscription renewed before its change was made; the change refunds the new period's unused time")
	}
	changeErr, err := s.apply(ctx, l, target, units, changeID)
	if err != nil {
		return err
	}
	if changeErr != nil {
		// Nothing is recorded as made: the next check tries again, and sends only what
		// the provider doesn't have yet.
		return changeErr
	}
	if err := l.tx.Commit(); err != nil {
		return err
	}
	log.Info("made a subscription change that waited for the end of its period", "to", product)
	return nil
}

// refundOfChange reports whether a refund made at at is a change's the module made to
// the subscription: one of the time it left unused.
func refundOfChange(ctx context.Context, tx *sql.Tx, subscriptionID string, at time.Time) (bool, error) {
	if at.IsZero() {
		return false, nil
	}
	var ours bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM subscription_changes
		    WHERE subscription_id = $1 AND applied_at IS NOT NULL
		        AND applied_at BETWEEN $2::timestamptz - make_interval(secs => $3) AND $2::timestamptz + make_interval(secs => $3)
		)`, subscriptionID, at, refundWindow.Seconds(),
	).Scan(&ours)
	return ours, err
}
