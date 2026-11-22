//go:build integration || browser

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/app/apptest"
	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/payments"
)

// testClientSecret is the secret of the OpenID Connect client the test app has.
const testClientSecret = "test-client-secret"

// testIdentity signs in whoever the "session" cookie names, with a verified address.
type testIdentity struct{}

func (testIdentity) Name() string { return "test" }

func (testIdentity) Authenticate(r *http.Request) (identity.Identity, error) {
	c, err := r.Cookie("session")
	if err != nil {
		return identity.Identity{}, identity.ErrNoSession
	}
	return identity.Identity{Subject: c.Value, Email: c.Value + "@example.com", EmailVerified: true, SignedInAt: time.Now()}, nil
}

func (testIdentity) Logout(w http.ResponseWriter, _ *http.Request) error {
	http.SetCookie(w, &http.Cookie{Name: "session", Path: "/", MaxAge: -1})
	return nil
}

func (testIdentity) Delete(context.Context, *http.Request) error { return nil }

// testPayments records checkouts. Webhook bodies are JSON payments.Events, accepted
// when the X-Signature header says "valid".
type testPayments struct {
	mu                    sync.Mutex
	checkouts             []payments.Checkout
	subscriptionCheckouts []payments.SubscriptionCheckout
	// subscriptions are the provider's, by ID: what webhooks said, and changes made.
	subscriptions map[string]payments.SubscriptionState
	changes       []payments.SubscriptionChange
}

func (p *testPayments) Name() string           { return "test" }
func (p *testPayments) CheckoutOrigin() string { return "https://pay.example" }

func (p *testPayments) CreateCheckout(_ context.Context, c payments.Checkout) (payments.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checkouts = append(p.checkouts, c)
	return payments.Session{ID: "co_" + c.Reference, URL: "https://pay.example/co_" + c.Reference}, nil
}

func (p *testPayments) last() payments.Checkout {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checkouts[len(p.checkouts)-1]
}

func (p *testPayments) ParseWebhook(h http.Header, body []byte) (payments.Event, error) {
	if h.Get("X-Signature") != "valid" {
		return payments.Event{}, payments.ErrInvalid
	}
	var e payments.Event
	if err := json.Unmarshal(body, &e); err != nil {
		return payments.Event{}, payments.ErrInvalid
	}
	return e, nil
}

// The subscription side of testPayments: checkouts are recorded, webhook bodies are
// JSON payments.SubscriptionEvents with the same signature, and the provider's state is
// what the webhooks said, with the changes made since. A subscription it doesn't know
// is canceling.
func (p *testPayments) CreateSubscriptionCheckout(_ context.Context, c payments.SubscriptionCheckout) (payments.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subscriptionCheckouts = append(p.subscriptionCheckouts, c)
	return payments.Session{ID: "co_" + c.Reference, URL: "https://pay.example/co_" + c.Reference}, nil
}

func (p *testPayments) ParseSubscriptionWebhook(h http.Header, body []byte) (payments.SubscriptionEvent, error) {
	if h.Get("X-Signature") != "valid" {
		return payments.SubscriptionEvent{}, payments.ErrInvalid
	}
	var e payments.SubscriptionEvent
	if err := json.Unmarshal(body, &e); err != nil || e.ID == "" {
		return payments.SubscriptionEvent{}, payments.ErrInvalid
	}
	if id := e.State.ID; id != "" {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.subscriptions == nil {
			p.subscriptions = map[string]payments.SubscriptionState{}
		}
		s := p.subscriptions[id]
		s.ID = id
		if e.State.ProductID != "" {
			s.ProductID, s.Units = e.State.ProductID, e.State.Units
		}
		if e.State.CustomerID != "" {
			s.CustomerID = e.State.CustomerID
		}
		if e.State.Status != "" {
			s.Status = e.State.Status
		}
		if !e.State.PeriodEnd.IsZero() {
			s.PeriodEnd = e.State.PeriodEnd
		}
		p.subscriptions[id] = s
	}
	return e, nil
}

func (p *testPayments) CompletedCheckout(context.Context, string) (payments.SubscriptionEvent, error) {
	return payments.SubscriptionEvent{}, payments.ErrIgnored
}

func (p *testPayments) Subscription(_ context.Context, id string) (payments.SubscriptionState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.subscriptions[id]; ok {
		return s, nil
	}
	return payments.SubscriptionState{ID: id, Status: payments.SubscriptionCanceling}, nil
}

func (p *testPayments) CancelSubscription(context.Context, string, bool) error { return nil }

func (p *testPayments) ChangeSubscription(_ context.Context, id string, ch payments.SubscriptionChange) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.changes = append(p.changes, ch)
	s := p.subscriptions[id]
	if ch.ProductID != "" {
		s.ProductID = ch.ProductID
	}
	if ch.Units != 0 {
		s.Units = ch.Units
	}
	p.subscriptions[id] = s
	return nil
}

// QuoteChange quotes €10.00 now, plus VAT, and €20.00 a period after, renewing at the
// subscription's period end.
func (p *testPayments) QuoteChange(_ context.Context, id string, _ payments.SubscriptionChange, _ time.Time) (payments.ChangeQuote, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return payments.ChangeQuote{Currency: "EUR", Charge: 1000, ChargeTax: 250, Renewal: 2000, RenewalTax: 500, RenewsAt: p.subscriptions[id].PeriodEnd}, nil
}

// testChanges returns the changes made at the provider.
func (p *testPayments) testChanges() []payments.SubscriptionChange {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]payments.SubscriptionChange(nil), p.changes...)
}

func (p *testPayments) CustomerPortal(_ context.Context, customer string) (string, error) {
	return "https://pay.example/portal/" + customer, nil
}

// paidEvent is what the payment provider sends once a checkout is paid.
func paidEvent(c payments.Checkout) []byte {
	body, _ := json.Marshal(payments.Event{
		ID: "evt_" + c.Reference, Kind: payments.Paid, Reference: c.Reference, CheckoutID: "co_" + c.Reference,
		AmountCents: c.AmountCents, PaidCents: c.AmountCents, Currency: "EUR",
	})
	return body
}

// newTextlab wires Textlab on a fresh database with test providers.
func newTextlab(t *testing.T, publicURL string) (*app.App, http.Handler, *testPayments) {
	t.Helper()
	a := apptest.New(t, app.Config{Name: "textlab", Migrations: appMigrations})
	pay := &testPayments{}
	handler, err := build(a, config{
		PublicURL:        publicURL,
		SecretKey:        strings.Repeat("s", 32),
		OIDCClientID:     "notes",
		OIDCClientSecret: testClientSecret,
		OIDCRedirectURIs: []string{"https://notes.example.com/callback"},
		CreemProMonthly:  "prod_pro_monthly",
		CreemProYearly:   "prod_pro_yearly",
		CreemMaxMonthly:  "prod_max_monthly",
		CreemMaxYearly:   "prod_max_yearly",
	}, providers{
		identity: testIdentity{},
		payments: pay,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, handler, pay
}
