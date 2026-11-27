package credits

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/coolapso/c4stack/payments"
)

// ErrHeld means the app holds some of an account's credit for work in progress, so the
// credit cannot move until that work settles.
var ErrHeld = errors.New("credits: credit is held for work in progress")

// Lock locks an account inside a transaction the caller holds, and returns its credit
// in that transaction, as WithAccount does in a transaction of its own. It lets another
// module change credit together with its own rows, such as opening an account and
// crediting its first purchase at once. Lock the account before any app rows, do no
// remote calls until the transaction ends, and commit or roll it back yourself.
func (cr *Credits) Lock(ctx context.Context, tx *sql.Tx, accountID string) (*Tx, error) {
	var id string
	err := tx.QueryRowContext(ctx, `
		SELECT id
		FROM accounts
		WHERE id = $1
		FOR UPDATE`, accountID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &Tx{ctx: ctx, cr: cr, tx: tx, accountID: accountID}, nil
}

// Order is a purchase the payment provider sold outside the credits checkout, such as
// on its marketplace, for credit the app grants. The module that took it says what it
// bought: the provider's event never sets the credit.
type Order struct {
	// Provider names the payment provider, as payments.Provider.Name does.
	Provider string
	// OrderID is the provider's order, which a later refund names.
	OrderID string
	// EventID is the provider event that reported the order, recorded with its credit.
	EventID string
	// CreditCents is the credit the order buys, and PriceCents its price before taxes:
	// the credit plus the app's fee.
	CreditCents int64
	PriceCents  int64
	// PaidCents is what the order came to, at least PriceCents.
	PaidCents int64
	// PaidAt is when the buyer paid. Zero means now.
	PaidAt time.Time
	// Description says what the entry is for, such as "Credit bought on the marketplace".
	Description string
}

// CreditOrder records a paid order as the account's purchase and adds its credit, once.
// The purchase is like one made through the credits checkout: refunds reverse it, and
// account deletion keeps it without the account. Repeating an order the account already
// has changes nothing and returns false; an order recorded for another account or with
// other amounts returns ErrConflict.
func (t *Tx) CreditOrder(o Order) (intentID string, credited bool, err error) {
	switch {
	case o.Provider == "" || o.OrderID == "" || o.EventID == "":
		return "", false, ErrInvalid
	case o.CreditCents <= 0 || o.PriceCents < o.CreditCents || o.PriceCents > MaxChargeCents:
		return "", false, ErrInvalid
	case o.PaidCents < o.PriceCents:
		return "", false, ErrInvalid
	}
	if err := checkEntry("order", o.Description); err != nil {
		return "", false, err
	}
	var accountID sql.NullString
	var credit, price int64
	err = t.tx.QueryRowContext(t.ctx, `
		SELECT id, account_id, credit_amount_cents, checkout_amount_cents
		FROM payment_intents
		WHERE provider = $1 AND provider_order_id = $2
		FOR UPDATE`, o.Provider, o.OrderID,
	).Scan(&intentID, &accountID, &credit, &price)
	switch {
	case err == nil:
		if accountID.String != t.accountID || credit != o.CreditCents || price != o.PriceCents {
			return "", false, ErrConflict
		}
		return intentID, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", false, err
	}
	paidAt := o.PaidAt
	if paidAt.IsZero() {
		paidAt = time.Now()
	}
	if err := t.tx.QueryRowContext(t.ctx, `
		INSERT INTO payment_intents (
		    account_id, provider, provider_order_id, idempotency_key,
		    credit_amount_cents, fee_amount_cents, checkout_amount_cents, paid_amount_cents,
		    provider_status, paid_at
		)
		VALUES ($1, $2, $3, gen_random_uuid(), $4, $5, $6, $7, 'paid', $8)
		RETURNING id`,
		t.accountID, o.Provider, o.OrderID, o.CreditCents, o.PriceCents-o.CreditCents, o.PriceCents,
		o.PaidCents, paidAt,
	).Scan(&intentID); err != nil {
		return "", false, err
	}
	if err := t.payment(KindTopUp, o.CreditCents, intentID, o.EventID, o.Description); err != nil {
		return "", false, err
	}
	return intentID, true, nil
}

// ReverseOrder takes back the credit of one of the account's purchases, once: after the
// provider refunded it, or, when disputed, after the buyer's bank took the payment back.
// It returns false when the purchase was reversed before, such as by the payment
// webhook, and ErrNotFound when the account has no such paid purchase.
func (t *Tx) ReverseOrder(provider, orderID, eventID string, disputed bool) (bool, error) {
	if eventID == "" {
		return false, ErrInvalid
	}
	in := intent{}
	var status string
	err := t.tx.QueryRowContext(t.ctx, `
		SELECT id, credit_amount_cents, provider_status
		FROM payment_intents
		WHERE provider = $1 AND provider_order_id = $2 AND account_id = $3
		FOR UPDATE`, provider, orderID, t.accountID,
	).Scan(&in.id, &in.creditCents, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, ErrNotFound
	case err != nil:
		return false, err
	case status == "refunded" || status == "charged_back":
		return false, nil
	case status != "paid":
		return false, ErrNotFound
	}
	e := payments.Event{ID: eventID, Kind: payments.Refunded}
	if disputed {
		e.Kind = payments.Disputed
	}
	return true, t.cr.record(t, in, e, "reversed")
}

// Transfer moves an account's balance, positive or negative, and its purchases to
// another account, inside tx, such as when an account without an identity is claimed by
// its owner's: later refunds of those purchases take the credit back from the new
// account. It locks both accounts, and returns ErrHeld while the app holds any of the
// first account's credit for work in progress. Each account gets one transfer entry
// with the reference, so repeating a transfer moves nothing more. It returns the cents
// moved.
func (cr *Credits) Transfer(ctx context.Context, tx *sql.Tx, fromAccountID, toAccountID, reference, description string) (int64, error) {
	if fromAccountID == toAccountID {
		return 0, ErrInvalid
	}
	if err := checkEntry(reference, description); err != nil {
		return 0, err
	}
	if err := lockPair(ctx, tx, fromAccountID, toAccountID); err != nil {
		return 0, err
	}
	from := &Tx{ctx: ctx, cr: cr, tx: tx, accountID: fromAccountID}
	to := &Tx{ctx: ctx, cr: cr, tx: tx, accountID: toAccountID}

	var moved int64
	err := tx.QueryRowContext(ctx, `
		SELECT -amount_cents
		FROM credit_ledger
		WHERE account_id = $1 AND reference = $2 AND kind = 'transfer'`, fromAccountID, reference,
	).Scan(&moved)
	if err == nil {
		return moved, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	held, err := cr.holds(ctx, tx, fromAccountID)
	if err != nil {
		return 0, err
	}
	if held > 0 {
		return 0, ErrHeld
	}
	balance, err := from.Balance()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE payment_intents
		SET account_id = $2, updated_at = now()
		WHERE account_id = $1`, fromAccountID, toAccountID,
	); err != nil {
		return 0, err
	}
	if balance == 0 {
		return 0, nil
	}
	for _, e := range []struct {
		t     *Tx
		cents int64
	}{{from, -balance}, {to, balance}} {
		if done, err := e.t.recorded(KindTransfer, e.cents, reference); done || err != nil {
			if err == nil {
				err = ErrConflict
			}
			return 0, err
		}
		if err := e.t.insert(KindTransfer, e.cents, &reference, description); err != nil {
			return 0, err
		}
	}
	return balance, nil
}

// lockPair locks two accounts, in ID order so concurrent transfers can't deadlock. It
// returns ErrNotFound unless both exist.
func lockPair(ctx context.Context, tx *sql.Tx, a, b string) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id
		FROM accounts
		WHERE id IN ($1, $2)
		ORDER BY id
		FOR UPDATE`, a, b,
	)
	if isInvalidUUID(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	locked := 0
	for rows.Next() {
		locked++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if locked != 2 {
		return ErrNotFound
	}
	return nil
}

// Review records an event about a payment that cannot be applied as it stands, such as
// a marketplace order whose amount doesn't match its product, for an operator: the
// credits reviews command lists it. Recording the same event again changes nothing.
func (cr *Credits) Review(ctx context.Context, provider, eventID, reason string) error {
	if provider == "" || eventID == "" || reason == "" {
		return ErrInvalid
	}
	_, err := cr.app.DB().ExecContext(ctx, `
		INSERT INTO payment_reviews (provider, event_id, reason)
		VALUES ($1, $2, $3)
		ON CONFLICT (provider, event_id) DO NOTHING`, provider, eventID, reason,
	)
	return err
}
