package payments

import (
	"context"
	"net/http"
	"time"
)

// Subscriber is a payment provider that also sells subscriptions, which the
// subscriptions module needs. Prices live at the provider, on its products; the app
// chooses a product and a number of units.
type Subscriber interface {
	Provider
	// CreateSubscriptionCheckout creates a hosted checkout that starts a subscription.
	CreateSubscriptionCheckout(ctx context.Context, c SubscriptionCheckout) (Session, error)
	// CompletedCheckout reads a checkout the app created, by its Session.ID, and returns
	// the subscription it started as a SubscriptionStarted event, as its webhook would.
	// It returns ErrIgnored while the checkout isn't completed.
	CompletedCheckout(ctx context.Context, id string) (SubscriptionEvent, error)
	// ParseSubscriptionWebhook verifies a webhook and decodes it. It returns ErrInvalid
	// for a bad signature or body, and ErrIgnored for events about anything but
	// subscriptions.
	ParseSubscriptionWebhook(header http.Header, body []byte) (SubscriptionEvent, error)
	// Subscription returns a subscription's state as the provider has it now.
	Subscription(ctx context.Context, id string) (SubscriptionState, error)
	// CancelSubscription cancels a subscription: at the end of its period, so it doesn't
	// renew, or at once.
	CancelSubscription(ctx context.Context, id string, atPeriodEnd bool) error
	// ChangeSubscription moves a subscription to another product, or another number of
	// units, or both.
	ChangeSubscription(ctx context.Context, id string, c SubscriptionChange) error
	// CustomerPortal returns a link to the provider's page where a customer manages
	// their payment methods and subscriptions.
	CustomerPortal(ctx context.Context, customerID string) (string, error)
}

// SubscriptionCheckout is a subscription to start.
type SubscriptionCheckout struct {
	// Reference identifies the subscription in the app; webhook events carry it back.
	Reference string
	// ProductID is the provider's product, which sets the price and the billing period.
	ProductID string
	// Units multiplies the product's price, such as seats or packs.
	Units int
	// Email prefills the buyer's address. It may be empty.
	Email string
	// SuccessURL is where the provider sends the buyer after paying.
	SuccessURL string
}

// SubscriptionChange is a change to a running subscription. A zero field stays as it is.
type SubscriptionChange struct {
	ProductID string
	Units     int
	// Proration says how the price difference is charged.
	Proration Proration
}

// Proration says how a change's price difference is charged.
type Proration int

const (
	// ProrateNow charges or credits the difference at once.
	ProrateNow Proration = iota
	// ProrateNextInvoice adds the difference to the next invoice.
	ProrateNextInvoice
	// ProrateNone changes the price from the next period, without a difference.
	ProrateNone
)

// SubscriptionStatus is where a subscription stands at the provider.
type SubscriptionStatus string

const (
	// SubscriptionActive is paid, and renews.
	SubscriptionActive SubscriptionStatus = "active"
	// SubscriptionTrialing is in a trial, and renews.
	SubscriptionTrialing SubscriptionStatus = "trialing"
	// SubscriptionPastDue failed a payment the provider is retrying.
	SubscriptionPastDue SubscriptionStatus = "past_due"
	// SubscriptionUnpaid failed its payments; the provider stopped retrying.
	SubscriptionUnpaid SubscriptionStatus = "unpaid"
	// SubscriptionPaused is paused, and may resume.
	SubscriptionPaused SubscriptionStatus = "paused"
	// SubscriptionCanceling is canceled at the end of its period, and runs until then.
	SubscriptionCanceling SubscriptionStatus = "canceling"
	// SubscriptionCanceled has ended.
	SubscriptionCanceled SubscriptionStatus = "canceled"
)

// SubscriptionState is a subscription as the provider has it.
type SubscriptionState struct {
	ID         string
	CustomerID string
	ProductID  string
	// Units is 0 when the provider didn't say.
	Units  int
	Status SubscriptionStatus
	// PeriodEnd is when the current period ends: the next renewal, or the end of a
	// canceled subscription. Zero when the provider didn't say.
	PeriodEnd time.Time
}

// SubscriptionEventKind is what a subscription webhook event reports.
type SubscriptionEventKind int

const (
	// SubscriptionStarted means a checkout started a subscription.
	SubscriptionStarted SubscriptionEventKind = iota + 1
	// SubscriptionChanged means a subscription's state changed: paid, renewed,
	// changed, canceled, failed or anything else the provider reports.
	SubscriptionChanged
	// SubscriptionRefunded means the merchant refunded a subscription's payment.
	SubscriptionRefunded
	// SubscriptionDisputed means the buyer's bank disputed a subscription's payment.
	SubscriptionDisputed
)

// SubscriptionEvent is a verified webhook event about a subscription.
type SubscriptionEvent struct {
	// ID is the provider's event ID.
	ID   string
	Kind SubscriptionEventKind
	// Reference is the SubscriptionCheckout.Reference, when the event carries it.
	Reference string
	// State is the subscription as the event describes it. Fields the event doesn't
	// carry are zero.
	State SubscriptionState
	// CreatedAt is when the provider created the event. Events can arrive out of order;
	// it says which is newer.
	CreatedAt time.Time
	// Review is set when the event can't be applied as it stands. It names why.
	Review string
}

// Live reports whether a subscription in status would charge again: it renews, or may
// resume. A canceling or canceled one won't.
func (s SubscriptionStatus) Live() bool {
	switch s {
	case SubscriptionActive, SubscriptionTrialing, SubscriptionPastDue, SubscriptionUnpaid, SubscriptionPaused:
		return true
	}
	return false
}
