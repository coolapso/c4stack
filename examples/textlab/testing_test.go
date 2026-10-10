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
	mu        sync.Mutex
	checkouts []payments.Checkout
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
	handler, err := build(a, config{PublicURL: publicURL, SecretKey: strings.Repeat("s", 32)}, providers{
		identity: testIdentity{},
		payments: pay,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, handler, pay
}
