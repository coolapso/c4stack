package subscriptions

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/coolapso/c4stack/payments"
)

// Review is a subscription event the module couldn't settle by itself and keeps for an
// operator, such as a dispute or a second subscription in a group. The operator acts
// at the provider or with Sync, then resolves it.
type Review struct {
	ID      int64
	EventID string
	// SubscriptionID and AccountID are empty when the event names no subscription the
	// module knows.
	SubscriptionID string
	AccountID      string
	Reason         string
	CreatedAt      time.Time
}

var (
	// ErrNoReview means no open review has the ID.
	ErrNoReview = errors.New("subscriptions: no open review has that ID")
	// ErrNotAtProvider means the provider doesn't know the subscription, such as one
	// whose checkout never finished.
	ErrNotAtProvider = errors.New("subscriptions: the subscription isn't known to the provider")
)

// Reviews returns the open reviews, the oldest first.
func (s *Subscriptions) Reviews(ctx context.Context) (_ []Review, err error) {
	rows, err := s.app.DB().QueryContext(ctx, `
		SELECT r.id, r.event_id, COALESCE(r.subscription_id::text, ''),
		       COALESCE(sub.account_id::text, ''), r.reason, r.created_at
		FROM subscription_reviews r
		LEFT JOIN subscriptions sub ON sub.id = r.subscription_id
		WHERE r.resolved_at IS NULL
		ORDER BY r.created_at, r.id`,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	var out []Review
	for rows.Next() {
		var r Review
		if err := rows.Scan(&r.ID, &r.EventID, &r.SubscriptionID, &r.AccountID, &r.Reason, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Resolve marks an open review handled, with a note on what was done. The note is
// required: it is all that is kept of how the review ended.
func (s *Subscriptions) Resolve(ctx context.Context, id int64, note string) error {
	note = strings.TrimSpace(note)
	if note == "" {
		return errors.New("subscriptions: resolving a review needs a note on what was done")
	}
	res, err := s.app.DB().ExecContext(ctx, `
		UPDATE subscription_reviews
		SET resolved_at = now(), resolution = $2
		WHERE id = $1 AND resolved_at IS NULL`, id, note,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNoReview
	}
	return nil
}

// Synced is what Sync found.
type Synced struct {
	// Status is the subscription's status at the provider, as applied.
	Status payments.SubscriptionStatus
	// Review is why the state needs an operator's look, empty when it doesn't.
	Review string
}

// Sync reads a subscription's state from the payment provider and applies it, such as
// after missed webhooks, running the OnChange hooks as a webhook would.
func (s *Subscriptions) Sync(ctx context.Context, id string) (Synced, error) {
	var providerID sql.NullString
	err := s.app.DB().QueryRowContext(ctx, `
		SELECT provider_subscription_id
		FROM subscriptions
		WHERE id::text = $1`, id,
	).Scan(&providerID)
	if errors.Is(err, sql.ErrNoRows) {
		return Synced{}, ErrNotFound
	}
	if err != nil {
		return Synced{}, err
	}
	if !providerID.Valid {
		return Synced{}, ErrNotAtProvider
	}
	state, err := s.payments.Subscription(ctx, providerID.String)
	if err != nil {
		return Synced{}, err
	}
	var out applied
	if err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = s.applyState(ctx, tx, id, state, time.Time{})
		return err
	}); err != nil {
		return Synced{}, err
	}
	return Synced{Status: state.Status, Review: out.review}, nil
}
