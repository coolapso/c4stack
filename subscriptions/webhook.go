package subscriptions

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/coolapso/c4stack/payments"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

// Webhook receives the payment provider's webhooks. Mount it on a public route with
// neither CSRF nor sign-in, such as POST /webhooks/subscriptions. An event is taken once:
// a repeat answers 200 and changes nothing. One the module can't apply goes to review.
//
// It logs event and subscription IDs and outcomes only, never bodies or buyer details.
func (s *Subscriptions) Webhook(c *gin.Context) {
	log := web.Log(c).With("provider", s.payments.Name())
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusBadRequest)
		return
	}
	event, err := s.payments.ParseSubscriptionWebhook(c.Request.Header, body)
	switch {
	case errors.Is(err, payments.ErrIgnored):
		log.Info("subscription webhook ignored", "event_id", event.ID)
		c.Status(http.StatusOK)
		return
	case err != nil:
		log.Warn("subscription webhook rejected", "error", err)
		c.Status(http.StatusBadRequest)
		return
	}
	res, err := s.take(c.Request.Context(), event)
	log = log.With("event_id", event.ID, "subscription_id", res.subscriptionID)
	if err != nil {
		log.Error("subscription webhook failed; the provider retries it", "error", err)
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if res.review != "" {
		log.Error("subscription event needs review", "reason", res.review)
	} else {
		log.Info("subscription webhook processed", "outcome", res.outcome)
	}
	if res.cancel != "" {
		// Its review stays open for the operator either way.
		if err := s.payments.CancelSubscription(c.Request.Context(), res.cancel, false); err != nil {
			log.Error("canceling a deleted account's subscription failed; cancel it at the provider", "error", err)
		} else {
			log.Warn("canceled a deleted account's subscription")
		}
	}
	c.Status(http.StatusOK)
}

// result is what taking an event did.
type result struct {
	subscriptionID string
	outcome        string
	review         string
	// cancel is the provider's ID of a subscription to cancel at once.
	cancel string
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// take applies an event once, in one transaction with the record that it was taken.
func (s *Subscriptions) take(ctx context.Context, e payments.SubscriptionEvent) (result, error) {
	var res result
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res = result{}
		taken, err := tx.ExecContext(ctx, `
			INSERT INTO subscription_events (provider, event_id)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, s.payments.Name(), e.ID,
		)
		if err != nil {
			return err
		}
		if n, err := taken.RowsAffected(); err != nil || n == 0 {
			res.outcome = "repeated"
			return err
		}
		id, providerID, err := s.find(ctx, tx, e)
		if err != nil {
			return err
		}
		res.subscriptionID = id
		review := func(reason string) error {
			res.review = reason
			return s.review(ctx, tx, e.ID, id, reason)
		}
		switch {
		case id == "":
			if _, ours := s.byProvider[e.State.ProductID]; ours {
				return review("unknown_subscription")
			}
			res.outcome = "not ours"
			return nil
		case providerID != "" && e.State.ID != "" && providerID != e.State.ID:
			return review("reference_mismatch")
		case e.Review != "":
			return review(e.Review)
		case e.Kind == payments.SubscriptionRefunded:
			ours, err := refundOfChange(ctx, tx, id, e.CreatedAt)
			if err != nil {
				return err
			}
			if !ours {
				return review("refunded")
			}
			res.outcome = "refund of a change"
			return nil
		case e.Kind == payments.SubscriptionDisputed:
			return review("disputed")
		}
		if providerID == "" && e.State.ID != "" {
			var other bool
			if err := tx.QueryRowContext(ctx, `
				SELECT EXISTS (
				    SELECT 1
				    FROM subscriptions
				    WHERE provider = $1 AND provider_subscription_id = $2 AND id <> $3
				)`, s.payments.Name(), e.State.ID, id,
			).Scan(&other); err != nil {
				return err
			}
			if other {
				return review("linked_twice")
			}
		}
		out, err := s.applyState(ctx, tx, id, e.State, e.CreatedAt)
		if err != nil {
			return err
		}
		if out.cancel {
			res.cancel = providerID
			if res.cancel == "" {
				res.cancel = e.State.ID
			}
		}
		if out.review != "" {
			return review(out.review)
		}
		res.outcome = "applied"
		return nil
	})
	return res, err
}

// find locks the subscription an event is about, by the app's reference or the
// provider's ID, and returns its ID and the provider's, or empty strings.
func (s *Subscriptions) find(ctx context.Context, tx *sql.Tx, e payments.SubscriptionEvent) (id, providerID string, err error) {
	var pid sql.NullString
	switch {
	case uuidPattern.MatchString(e.Reference):
		err = tx.QueryRowContext(ctx, `
			SELECT id, provider_subscription_id
			FROM subscriptions
			WHERE id = $1 AND provider = $2
			FOR UPDATE`, e.Reference, s.payments.Name(),
		).Scan(&id, &pid)
	case e.State.ID != "":
		err = tx.QueryRowContext(ctx, `
			SELECT id, provider_subscription_id
			FROM subscriptions
			WHERE provider = $1 AND provider_subscription_id = $2
			FOR UPDATE`, s.payments.Name(), e.State.ID,
		).Scan(&id, &pid)
	default:
		return "", "", nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return id, pid.String, err
}

// applied is what applying a state did: the reason an operator must look, if any, and
// whether the subscription must be canceled at once.
type applied struct {
	review string
	cancel bool
}

// applyState applies a provider's state of a subscription, taken at the provider's
// time at, unless the module holds a newer one; the provider's and customer's IDs are
// recorded either way. A zero at is a direct read, or an event without a time: it
// applies, and leaves the order of events as it was.
//
// A product or a number of units the app doesn't allow isn't applied: a checkout that
// completed with another product than its own, a move out of the subscription's group,
// or more units than the product's limit, such as from a change made at the provider.
// The rest of the state still applies, and the event goes to review.
func (s *Subscriptions) applyState(ctx context.Context, tx *sql.Tx, id string, st payments.SubscriptionState, at time.Time) (applied, error) {
	var accountID sql.NullString
	var product, target, status string
	var units int
	var stateAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `
		SELECT account_id, product, target, status, units, state_at
		FROM subscriptions
		WHERE id = $1
		FOR UPDATE`, id,
	).Scan(&accountID, &product, &target, &status, &units, &stateAt); err != nil {
		return applied{}, err
	}
	var next Product
	if st.ProductID != "" {
		p, ok := s.byProvider[st.ProductID]
		if !ok {
			return applied{review: "unknown_product"}, nil
		}
		next = p
	}
	if !at.IsZero() && stateAt.Valid && at.Before(stateAt.Time) {
		// An older event that arrived late: only the IDs it brings are new.
		_, err := tx.ExecContext(ctx, `
			UPDATE subscriptions
			SET provider_subscription_id = COALESCE(provider_subscription_id, NULLIF($2, '')),
			    provider_customer_id = COALESCE(provider_customer_id, NULLIF($3, '')),
			    updated_at = now()
			WHERE id = $1`, id, st.ID, st.CustomerID,
		)
		return applied{}, err
	}
	var out applied
	if next.ID != "" && next.ID != product {
		current := s.products[product]
		switch {
		case status == "pending":
			out.review = "product_mismatch"
		case current.Group == "" || current.Group != next.Group:
			out.review = "product_change"
		default:
			product = next.ID
		}
	}
	if st.Units > 0 {
		limit := s.products[product].MaxUnits
		if limit == 0 {
			limit = 1
		}
		if st.Units > limit {
			if out.review == "" {
				out.review = "too_many_units"
			}
		} else {
			units = st.Units
		}
	}
	if st.Status != "" {
		status = string(st.Status)
	}
	var periodEnd *time.Time
	if !st.PeriodEnd.IsZero() {
		periodEnd = &st.PeriodEnd
	}
	var newStateAt *time.Time
	if !at.IsZero() {
		newStateAt = &at
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE subscriptions
		SET provider_subscription_id = COALESCE(provider_subscription_id, NULLIF($2, '')),
		    provider_customer_id = COALESCE(NULLIF($3, ''), provider_customer_id),
		    product = $4,
		    units = $5,
		    status = $6,
		    period_end = COALESCE($7, period_end),
		    state_at = COALESCE($8, state_at),
		    updated_at = now()
		WHERE id = $1`, id, st.ID, st.CustomerID, product, units, status, periodEnd, newStateAt,
	); err != nil {
		return applied{}, err
	}
	if !payments.SubscriptionStatus(status).Live() {
		return out, nil
	}
	// The account was deleted, yet the subscription would charge again, such as a
	// checkout completed after the deletion: it is canceled at once, and the operator
	// decides about its payment.
	if !accountID.Valid {
		return applied{review: "deleted_account", cancel: true}, nil
	}
	// Two checkouts for one group both completed: the account pays twice. Activations
	// in one group take turns, so two at once see each other.
	if g := s.products[product].Group; g != "" {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			"subscriptions:"+accountID.String+":"+g+":"+target); err != nil {
			return applied{}, err
		}
		taken, err := s.groupTaken(ctx, tx, accountID.String, g, target, id)
		if err != nil {
			return applied{}, err
		}
		if taken {
			return applied{review: "second_in_group"}, nil
		}
	}
	return out, nil
}

// review records an event for an operator.
func (s *Subscriptions) review(ctx context.Context, tx *sql.Tx, eventID, subscriptionID, reason string) error {
	var sub *string
	if subscriptionID != "" {
		sub = &subscriptionID
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO subscription_reviews (provider, event_id, subscription_id, reason)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, s.payments.Name(), eventID, sub, reason,
	)
	return err
}
