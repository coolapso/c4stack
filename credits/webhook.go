package credits

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"

	"github.com/coolapso/c4stack/payments"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

// Webhook receives payment provider webhooks. Mount it on a public route with neither
// CSRF nor sign-in, such as POST /webhooks/payments. docs/credits.md lists its outcomes.
//
// It logs event and intent IDs and outcomes only, never bodies or buyer details.
func (cr *Credits) Webhook(c *gin.Context) {
	log := web.Log(c).With("provider", cr.payments.Name())
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusBadRequest)
		return
	}
	event, err := cr.payments.ParseWebhook(c.Request.Header, body)
	switch {
	case errors.Is(err, payments.ErrIgnored):
		log.Info("payment webhook ignored", "event_id", event.ID)
		c.Status(http.StatusOK)
		return
	case err != nil:
		log.Warn("payment webhook rejected", "error", err)
		c.Status(http.StatusBadRequest)
		return
	}
	result, err := cr.apply(c.Request.Context(), event)
	log = log.With("event_id", event.ID, "intent_id", result.intentID)
	if err != nil {
		log.Error("payment webhook failed; the provider retries it", "error", err)
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if result.review != "" {
		log.Error("payment needs review", "reason", result.review)
	} else {
		log.Info("payment webhook processed", "outcome", result.outcome)
	}
	c.Status(http.StatusOK)
}

// applied describes what a webhook event did.
type applied struct {
	intentID string
	outcome  string // credited, paid, reversed, duplicate or recorded
	review   string // why an operator must look at the event, if they must
}

// intent is the stored purchase an event refers to.
type intent struct {
	id          string
	accountID   sql.NullString
	creditCents int64
	chargeCents int64
	currency    string
	status      string
	checkoutID  sql.NullString
	paidCents   sql.NullInt64
}

var errNoIntent = errors.New("credits: no matching payment")

// apply processes a verified event. Grants come from the stored intent, never from the
// event. Anything that does not fit goes to review instead of a guessed mutation.
func (cr *Credits) apply(ctx context.Context, e payments.Event) (applied, error) {
	found, err := cr.findIntent(ctx, e)
	if errors.Is(err, errNoIntent) {
		return cr.toReview(ctx, e, "", "unknown_payment")
	}
	if err != nil {
		return applied{}, err
	}
	if e.Review != "" {
		return cr.toReview(ctx, e, found.id, e.Review)
	}
	var result applied
	err = cr.lockIntent(ctx, found, func(t *Tx, in intent) error {
		result = decide(t, in, e)
		return cr.record(t, in, e, result.outcome)
	})
	if err != nil {
		return applied{}, err
	}
	if result.review != "" {
		review, err := cr.toReview(ctx, e, found.id, result.review)
		review.outcome = result.outcome
		return review, err
	}
	return result, nil
}

// decide compares an event with the locked intent. t has no account when the
// account was deleted.
func decide(t *Tx, in intent, e payments.Event) applied {
	r := applied{intentID: in.id}
	review := func(reason string) applied { r.review = reason; return r }
	if e.Currency != in.currency {
		return review("currency_mismatch")
	}
	if e.Kind == payments.Paid {
		switch {
		case in.status != "pending":
			r.outcome = "duplicate"
		case e.AmountCents != in.chargeCents:
			return review("amount_mismatch")
		case e.PaidCents < in.chargeCents:
			return review("discounted")
		case in.checkoutID.Valid && e.CheckoutID != "" && e.CheckoutID != in.checkoutID.String:
			return review("checkout_mismatch")
		case t.accountID == "":
			// Record the payment so a later refund matches it, and have an operator
			// refund it: there is no account to credit.
			r.outcome = "paid"
			r.review = "paid_after_account_deleted"
		default:
			r.outcome = "credited"
		}
		return r
	}
	switch in.status {
	case "pending":
		return review("reversal_before_payment")
	case "refunded", "disputed":
		r.outcome = "duplicate"
	default:
		if e.AmountCents < in.paidCents.Int64 {
			return review("partial_reversal")
		}
		r.outcome = "reversed"
	}
	return r
}

// record applies an outcome to the intent and, while the account exists, the ledger.
func (cr *Credits) record(t *Tx, in intent, e payments.Event, outcome string) error {
	switch outcome {
	case "credited", "paid":
		if _, err := t.tx.ExecContext(t.ctx, `
			UPDATE payment_intents
			SET status = 'paid', order_id = $2, paid_cents = $3, paid_at = now()
			WHERE id = $1`, in.id, e.OrderID, e.PaidCents,
		); err != nil {
			return err
		}
		if outcome == "paid" {
			return nil
		}
		return t.insert("purchase", in.creditCents, "purchase:"+in.id, "Credit purchase", &in.id)
	case "reversed":
		kind, status, description := "refund", "refunded", "Purchase refunded"
		if e.Kind == payments.Disputed {
			kind, status, description = "dispute", "disputed", "Purchase disputed"
		}
		if _, err := t.tx.ExecContext(t.ctx, `
			UPDATE payment_intents
			SET status = $2, reversed_at = now()
			WHERE id = $1`, in.id, status,
		); err != nil {
			return err
		}
		if t.accountID == "" {
			return nil // the deleted account's credit was already forfeited
		}
		return t.insert(kind, -in.creditCents, kind+":"+in.id, description, &in.id)
	}
	return nil
}

// lockIntent locks the intent's account, then the intent, and runs fn inside that
// transaction. For an intent whose account was deleted, fn gets a Tx without an
// account.
func (cr *Credits) lockIntent(ctx context.Context, found intent, fn func(t *Tx, in intent) error) error {
	run := func(t *Tx) error {
		in := intent{id: found.id}
		err := t.tx.QueryRowContext(ctx, `
			SELECT account_id, credit_cents, charge_cents, currency, status, checkout_id, paid_cents
			FROM payment_intents
			WHERE id = $1
			FOR UPDATE`, found.id,
		).Scan(&in.accountID, &in.creditCents, &in.chargeCents, &in.currency, &in.status, &in.checkoutID, &in.paidCents)
		if err != nil {
			return err
		}
		return fn(t, in)
	}
	if found.accountID.Valid {
		err := cr.WithAccount(ctx, found.accountID.String, run)
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		// The account was deleted since the intent was read.
	}
	tx, err := cr.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := run(&Tx{ctx: ctx, cr: cr, tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

// findIntent finds the intent an event refers to: by reference, then checkout, then
// order.
func (cr *Credits) findIntent(ctx context.Context, e payments.Event) (intent, error) {
	lookups := []struct {
		column, value string
	}{
		{"id::text", e.Reference},
		{"checkout_id", e.CheckoutID},
		{"order_id", e.OrderID},
	}
	for _, l := range lookups {
		if l.value == "" {
			continue
		}
		in := intent{}
		err := cr.app.DB().QueryRowContext(ctx, `
			SELECT id, account_id
			FROM payment_intents
			WHERE provider = $1 AND `+l.column+` = $2`, cr.payments.Name(), l.value,
		).Scan(&in.id, &in.accountID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		return in, err
	}
	return intent{}, errNoIntent
}

// toReview records an event an operator must look at. Recording the same event again
// changes nothing, so provider retries are harmless.
func (cr *Credits) toReview(ctx context.Context, e payments.Event, intentID, reason string) (applied, error) {
	var id sql.NullString
	if intentID != "" {
		id = sql.NullString{String: intentID, Valid: true}
	}
	_, err := cr.app.DB().ExecContext(ctx, `
		INSERT INTO payment_reviews (provider, event_id, intent_id, reason)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (provider, event_id) DO NOTHING`,
		cr.payments.Name(), e.ID, id, reason,
	)
	return applied{intentID: intentID, outcome: "recorded", review: reason}, err
}
