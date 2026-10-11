package credits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/payments"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrAmount means a purchase amount is outside the policy's limits.
	ErrAmount = errors.New("credits: the purchase amount is outside the allowed range")
	// ErrPurchasesOff means the module has no payment provider.
	ErrPurchasesOff = errors.New("credits: buying credit is not available")
)

// Quote is what buying an amount of credit costs.
type Quote struct {
	CreditCents int64
	FeeCents    int64
	ChargeCents int64
}

// Fee returns the fee for buying creditCents. It is grossed up so that a provider
// charging FeeBasisPoints and FeeFixedCents on the total is covered by the fee.
func (p Policy) Fee(creditCents int64) int64 {
	if p.FeeBasisPoints == 0 && p.FeeFixedCents == 0 {
		return 0
	}
	num := creditCents*p.FeeBasisPoints + p.FeeFixedCents*10_000
	den := 10_000 - p.FeeBasisPoints
	return (num + den - 1) / den
}

// Quote prices creditCents of credit, or returns ErrAmount outside the policy's limits.
func (cr *Credits) Quote(creditCents int64) (Quote, error) {
	if cr.payments == nil {
		return Quote{}, ErrPurchasesOff
	}
	if creditCents < cr.policy.MinPurchaseCents || creditCents > cr.policy.MaxPurchaseCents {
		return Quote{}, ErrAmount
	}
	fee := cr.policy.Fee(creditCents)
	if creditCents+fee > MaxChargeCents {
		return Quote{}, ErrAmount
	}
	return Quote{CreditCents: creditCents, FeeCents: fee, ChargeCents: creditCents + fee}, nil
}

// Purchasing reports whether buying credit is available.
func (cr *Credits) Purchasing() bool {
	return cr.payments != nil
}

// Purchase is a started purchase: the buyer pays at CheckoutURL.
type Purchase struct {
	Quote
	IntentID    string
	CheckoutURL string
}

// Purchase prices creditCents, records the purchase, and creates a checkout at the
// payment provider. The webhook later credits what the purchase records.
func (cr *Credits) Purchase(ctx context.Context, account accounts.Account, creditCents int64, successURL string) (Purchase, error) {
	quote, err := cr.Quote(creditCents)
	if err != nil {
		return Purchase{}, err
	}
	p := Purchase{Quote: quote}
	err = cr.app.DB().QueryRowContext(ctx, `
		INSERT INTO payment_intents (
		    account_id, provider, idempotency_key,
		    credit_amount_cents, fee_amount_cents, checkout_amount_cents, provider_status
		)
		VALUES ($1, $2, gen_random_uuid(), $3, $4, $5, 'created')
		RETURNING id`,
		account.ID, cr.payments.Name(), quote.CreditCents, quote.FeeCents, quote.ChargeCents,
	).Scan(&p.IntentID)
	if err != nil {
		return Purchase{}, fmt.Errorf("credits: record purchase: %w", err)
	}
	session, err := cr.payments.CreateCheckout(ctx, payments.Checkout{
		Reference:   p.IntentID,
		AmountCents: quote.ChargeCents,
		Email:       account.Email,
		SuccessURL:  successURL,
	})
	if err != nil {
		return Purchase{}, err
	}
	if _, err := cr.app.DB().ExecContext(ctx, `
		UPDATE payment_intents
		SET provider_checkout_id = $2, provider_status = 'pending', updated_at = now()
		WHERE id = $1 AND provider_checkout_id IS NULL`, p.IntentID, session.ID,
	); err != nil {
		return Purchase{}, fmt.Errorf("credits: record checkout: %w", err)
	}
	p.CheckoutURL = session.URL
	return p, nil
}

// PaymentStatus returns the provider status of an account's purchase: created,
// pending, paid, refunded, charged_back, failed, canceled or expired. A purchase of
// another account, or none, returns ErrNotFound.
func (cr *Credits) PaymentStatus(ctx context.Context, accountID, intentID string) (string, error) {
	var status string
	err := cr.app.DB().QueryRowContext(ctx, `
		SELECT provider_status
		FROM payment_intents
		WHERE id = $1 AND account_id = $2`, intentID, accountID,
	).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return "", ErrNotFound
	}
	return status, err
}

var euroPattern = regexp.MustCompile(`^([0-9]{1,6})(?:[.,]([0-9]{1,2}))?$`)

// ParseEuros reads an amount such as "12", "12.5" or "€12,50" as cents.
func ParseEuros(s string) (int64, bool) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "€"), "€"))
	m := euroPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	euros, _ := strconv.ParseInt(m[1], 10, 64)
	cents := int64(0)
	if m[2] != "" {
		cents, _ = strconv.ParseInt((m[2] + "0")[:2], 10, 64)
	}
	return euros*100 + cents, true
}

// FormatEuros renders cents as an amount such as "€12.50" or "−€3.00".
func FormatEuros(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "−", -cents
	}
	return fmt.Sprintf("%s€%d.%02d", sign, cents/100, cents%100)
}

// isInvalidUUID reports PostgreSQL's error for a malformed UUID parameter.
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}
