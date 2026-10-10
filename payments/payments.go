// Package payments defines the payment provider interface the credits module uses: a
// hosted checkout for a server-priced amount, and verified webhook events.
package payments

import (
	"context"
	"errors"
	"net/http"
)

var (
	// ErrInvalid means a webhook failed signature verification or is malformed.
	ErrInvalid = errors.New("payments: invalid webhook")
	// ErrIgnored means a valid webhook is not about a payment this app takes, such as
	// another product's order or an event type the app does not use.
	ErrIgnored = errors.New("payments: ignored webhook")
)

// Checkout is a payment to collect. The server sets the amount; the browser never
// chooses it.
type Checkout struct {
	// Reference identifies the purchase in the app; webhook events carry it back.
	Reference string
	// AmountCents is what the buyer pays, in EUR cents.
	AmountCents int64
	// Email prefills the buyer's address. It may be empty.
	Email string
	// SuccessURL is where the provider sends the buyer after paying.
	SuccessURL string
}

// Session is a created checkout.
type Session struct {
	ID  string
	URL string
}

// EventKind is what a webhook event reports.
type EventKind int

const (
	// Paid means the buyer paid a checkout.
	Paid EventKind = iota + 1
	// Refunded means the merchant refunded a payment.
	Refunded
	// Disputed means the buyer's bank disputed a payment.
	Disputed
)

// Event is a verified webhook event about one of the app's payments.
type Event struct {
	// ID is the provider's event ID.
	ID   string
	Kind EventKind
	// Reference is the Checkout.Reference of the payment, when the event carries it.
	Reference string
	// CheckoutID and OrderID identify the payment at the provider.
	CheckoutID string
	OrderID    string
	// AmountCents is the checkout's price for a Paid event, and the reversed amount
	// for a refund or dispute.
	AmountCents int64
	// PaidCents is what the buyer actually paid, which taxes or discounts can change.
	PaidCents int64
	// Currency is the ISO code, such as "EUR".
	Currency string
	// Review is set when the event is about one of the app's payments but cannot be
	// applied as it stands, such as a refund that has not succeeded. It names why.
	Review string
}

// Provider is a payment provider such as Creem.
type Provider interface {
	// Name identifies the provider in stored payments, such as "creem".
	Name() string
	// CheckoutOrigin is the origin of hosted checkout pages, which pages that start a
	// checkout must allow as a form target.
	CheckoutOrigin() string
	// CreateCheckout creates a hosted checkout for c.
	CreateCheckout(ctx context.Context, c Checkout) (Session, error)
	// ParseWebhook verifies a webhook and decodes it. It returns ErrInvalid for a bad
	// signature or body, and ErrIgnored for events the app does not act on.
	ParseWebhook(header http.Header, body []byte) (Event, error)
}
