package creem

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coolapso/c4stack/payments"
)

// fakeAPI records the requests it gets and answers from a table of paths.
type fakeAPI struct {
	mu       sync.Mutex
	requests []string
	bodies   []map[string]any
	answers  map[string]string
}

func (f *fakeAPI) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "creem_test_key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, key+"?"+r.URL.RawQuery)
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		f.bodies = append(f.bodies, body)
		answer, ok := f.answers[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const subscriptionJSON = `{"id": "sub_1", "object": "subscription", "status": "active",
	"product": {"id": "prod_plus"}, "customer": {"id": "cust_1", "email": "ann@example.com"},
	"items": [{"id": "sitem_1", "product_id": "prod_plus", "price_id": "pprice_1", "units": 2}],
	"current_period_end_date": "2026-12-19T10:00:00.000Z", "metadata": {"reference": "ref-1"}}`

func TestCreateSubscriptionCheckout(t *testing.T) {
	api := &fakeAPI{answers: map[string]string{
		"POST /v1/checkouts": `{"id": "ch_1", "checkout_url": "https://creem.io/test/checkout/prod_plus/ch_1"}`,
	}}
	c := adapter(t, api.server(t).URL)
	s, err := c.CreateSubscriptionCheckout(context.Background(), payments.SubscriptionCheckout{
		Reference: "ref-1", ProductID: "prod_plus", Units: 3, Email: "ann@example.com", SuccessURL: "https://app.example/ok",
	})
	if err != nil || s.ID != "ch_1" || s.URL != "https://www.creem.io/test/checkout/prod_plus/ch_1" {
		t.Fatalf("session %+v, %v", s, err)
	}
	b := api.bodies[0]
	meta, _ := b["metadata"].(map[string]any)
	customer, _ := b["customer"].(map[string]any)
	if b["product_id"] != "prod_plus" || b["request_id"] != "ref-1" || b["units"] != float64(3) || meta["reference"] != "ref-1" ||
		customer["email"] != "ann@example.com" || b["custom_price"] != nil {
		t.Fatalf("body %v", b)
	}
	if _, err := c.CreateSubscriptionCheckout(context.Background(), payments.SubscriptionCheckout{Reference: "r", ProductID: "p"}); err == nil {
		t.Error("a checkout without units")
	}
	api.answers["POST /v1/checkouts"] = `{"id": "ch_2", "checkout_url": "https://evil.example/ch_2"}`
	if _, err := c.CreateSubscriptionCheckout(context.Background(), payments.SubscriptionCheckout{Reference: "r", ProductID: "p", Units: 1}); err == nil {
		t.Error("accepted a checkout URL on another origin")
	}
}

func TestSubscriptionCalls(t *testing.T) {
	api := &fakeAPI{answers: map[string]string{
		"GET /v1/subscriptions":                subscriptionJSON,
		"POST /v1/subscriptions/sub_1/cancel":  `{}`,
		"POST /v1/subscriptions/sub_1/upgrade": `{}`,
		"POST /v1/subscriptions/sub_1":         `{}`,
		"POST /v1/customers/billing":           `{"customer_portal_link": "https://creem.io/portal/cust_1?token=t"}`,
	}}
	c := adapter(t, api.server(t).URL)
	ctx := context.Background()
	st, err := c.Subscription(ctx, "sub_1")
	want := payments.SubscriptionState{ID: "sub_1", CustomerID: "cust_1", ProductID: "prod_plus", Units: 2,
		Status: payments.SubscriptionActive, PeriodEnd: time.Date(2026, 12, 19, 10, 0, 0, 0, time.UTC)}
	if err != nil || st != want {
		t.Fatalf("Subscription = %+v, %v", st, err)
	}
	if err := c.CancelSubscription(ctx, "sub_1", true); err != nil || api.bodies[1]["mode"] != "scheduled" {
		t.Fatalf("scheduled cancel: %v %v", err, api.bodies[1])
	}
	if err := c.CancelSubscription(ctx, "sub_1", false); err != nil || api.bodies[2]["mode"] != "immediate" {
		t.Fatalf("immediate cancel: %v %v", err, api.bodies[2])
	}
	if err := c.ChangeSubscription(ctx, "sub_1", payments.SubscriptionChange{ProductID: "prod_family", Units: 4, Proration: payments.ProrateNextInvoice}); err != nil {
		t.Fatal(err)
	}
	upgrade, update := api.bodies[3], api.bodies[5]
	items, _ := update["items"].([]any)
	item, _ := items[0].(map[string]any)
	if upgrade["product_id"] != "prod_family" || upgrade["update_behavior"] != "proration-charge" ||
		item["id"] != "sitem_1" || item["price_id"] != "pprice_1" || item["units"] != float64(4) || update["update_behavior"] != "proration-charge" {
		t.Fatalf("change: upgrade %v, update %v", upgrade, update)
	}
	if link, err := c.CustomerPortal(ctx, "cust_1"); err != nil || link != "https://creem.io/portal/cust_1?token=t" || api.bodies[6]["customer_id"] != "cust_1" {
		t.Fatalf("portal %q, %v", link, err)
	}
	// Errors carry the status, never the IDs in a query or the answer.
	delete(api.answers, "GET /v1/subscriptions")
	_, err = c.Subscription(ctx, "sub_secret")
	if err == nil || strings.Contains(err.Error(), "sub_secret") || !strings.Contains(err.Error(), "404") {
		t.Errorf("error %v", err)
	}
}

func TestParseSubscriptionWebhook(t *testing.T) {
	c := adapter(t, "http://unused.example")
	parse := func(body string) (payments.SubscriptionEvent, error) {
		return c.ParseSubscriptionWebhook(signed(body), []byte(body))
	}
	checkout := `{"id": "evt_1", "eventType": "checkout.completed", "created_at": 1765000000000, "object": {
		"id": "ch_1", "request_id": "ref-1", "status": "completed", "units": 3, "metadata": {"reference": "ref-1"},
		"order": {"id": "ord_1", "type": "recurring", "status": "paid"},
		"subscription": {"id": "sub_1", "status": "active", "product": "prod_plus", "customer": "cust_1", "metadata": {"reference": "ref-1"}}}}`
	e, err := parse(checkout)
	if err != nil || e.Kind != payments.SubscriptionStarted || e.Reference != "ref-1" || e.Review != "" ||
		e.State.ID != "sub_1" || e.State.CustomerID != "cust_1" || e.State.ProductID != "prod_plus" || e.State.Units != 3 ||
		e.State.Status != payments.SubscriptionActive || !e.CreatedAt.Equal(time.UnixMilli(1765000000000)) {
		t.Fatalf("checkout: %+v, %v", e, err)
	}
	if e, _ := parse(strings.Replace(checkout, `"metadata": {"reference": "ref-1"},
		"order"`, `"metadata": {},
		"order"`, 1)); e.Review != "missing_reference" {
		t.Errorf("checkout without the reference's metadata: %+v", e)
	}
	if _, err := parse(strings.Replace(checkout, `"type": "recurring"`, `"type": "onetime"`, 1)); !errors.Is(err, payments.ErrIgnored) {
		t.Errorf("a one-time checkout: %v", err)
	}

	paid := `{"id": "evt_2", "eventType": "subscription.paid", "created_at": 1765000001000, "object": ` + subscriptionJSON + `}`
	e, err = parse(paid)
	if err != nil || e.Kind != payments.SubscriptionChanged || e.Reference != "ref-1" || e.State.Units != 2 ||
		!e.State.PeriodEnd.Equal(time.Date(2026, 12, 19, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("paid: %+v, %v", e, err)
	}
	for creem, want := range map[string]payments.SubscriptionStatus{
		"scheduled_cancel": payments.SubscriptionCanceling, "canceled": payments.SubscriptionCanceled,
		"past_due": payments.SubscriptionPastDue, "unpaid": payments.SubscriptionUnpaid,
		"paused": payments.SubscriptionPaused, "trialing": payments.SubscriptionTrialing,
	} {
		if e, _ := parse(strings.Replace(paid, `"status": "active"`, `"status": "`+creem+`"`, 1)); e.State.Status != want || e.Review != "" {
			t.Errorf("%s: %+v", creem, e)
		}
	}
	if e, _ := parse(strings.Replace(paid, `"status": "active"`, `"status": "frozen"`, 1)); e.Review != "unknown_status" {
		t.Errorf("unknown status: %+v", e)
	}
	// After a change, the item names the product the subscription has now.
	changed := strings.Replace(paid, `"items": [{"id": "sitem_1", "product_id": "prod_plus"`, `"items": [{"id": "sitem_1", "product_id": "prod_family"`, 1)
	if e, _ := parse(changed); e.State.ProductID != "prod_family" || e.Review != "" {
		t.Errorf("the item's product: %+v", e)
	}
	twoItems := strings.Replace(paid, `"units": 2}]`, `"units": 2}, {"id": "sitem_2", "product_id": "prod_x", "units": 1}]`, 1)
	if e, _ := parse(twoItems); e.Review != "several_items" {
		t.Errorf("two items: %+v", e)
	}

	refund := `{"id": "evt_3", "eventType": "refund.created", "object": {"id": "ref_1", "status": "succeeded",
		"subscription": {"id": "sub_1", "status": "canceled", "metadata": {"reference": "ref-1"}}}}`
	if e, err := parse(refund); err != nil || e.Kind != payments.SubscriptionRefunded || e.State.ID != "sub_1" || e.Reference != "ref-1" {
		t.Fatalf("refund: %+v, %v", e, err)
	}
	// The credits side ignores a subscription's refund.
	if _, err := c.ParseWebhook(signed(refund), []byte(refund)); !errors.Is(err, payments.ErrIgnored) {
		t.Errorf("credits parsed a subscription's refund: %v", err)
	}
	creditRefund := `{"id": "evt_4", "eventType": "refund.created", "object": {"id": "ref_2", "status": "succeeded",
		"checkout": {"id": "ch_9", "product": "prod_credits"}}}`
	if _, err := parse(creditRefund); !errors.Is(err, payments.ErrIgnored) {
		t.Errorf("a credit refund: %v", err)
	}
	if _, err := parse(`{"id": "evt_5", "eventType": "credits.granted", "object": {}}`); !errors.Is(err, payments.ErrIgnored) {
		t.Errorf("another event: %v", err)
	}
	if _, err := c.ParseSubscriptionWebhook(signed(paid), []byte(paid+" ")); !errors.Is(err, payments.ErrInvalid) {
		t.Errorf("a changed body: %v", err)
	}
}

func TestCompletedCheckout(t *testing.T) {
	api := &fakeAPI{answers: map[string]string{"GET /v1/checkouts": `{"id": "ch_1", "status": "completed",
		"request_id": "ref-1", "units": 1, "metadata": {"reference": "ref-1"},
		"order": {"id": "ord_1", "type": "recurring", "status": "paid"},
		"subscription": {"id": "sub_1", "status": "active", "product": "prod_plus", "customer": "cust_1"}}`}}
	c := adapter(t, api.server(t).URL)
	e, err := c.CompletedCheckout(context.Background(), "ch_1")
	if err != nil || e.ID != "checkout:ch_1" || e.Kind != payments.SubscriptionStarted || e.Reference != "ref-1" ||
		e.State.ID != "sub_1" || e.State.Status != payments.SubscriptionActive || e.Review != "" {
		t.Fatalf("completed: %+v, %v", e, err)
	}
	if !strings.HasPrefix(api.requests[0], "GET /v1/checkouts?checkout_id=ch_1") {
		t.Fatalf("request %v", api.requests)
	}
	api.answers["GET /v1/checkouts"] = `{"id": "ch_2", "status": "pending", "request_id": "ref-2"}`
	if _, err := c.CompletedCheckout(context.Background(), "ch_2"); !errors.Is(err, payments.ErrIgnored) {
		t.Fatalf("an open checkout: %v", err)
	}
}
