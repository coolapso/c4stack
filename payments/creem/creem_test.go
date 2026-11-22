package creem

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coolapso/c4stack/payments"
)

const secret = "whsec_test"

func adapter(t *testing.T, api string) *Creem {
	t.Helper()
	c, err := New(Config{APIKey: "creem_test_key", WebhookSecret: secret, ProductID: "prod_credits", APIURL: api})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func signed(body string) http.Header {
	return signedWith(secret, body)
}

func signedWith(key, body string) http.Header {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(body))
	return http.Header{"Creem-Signature": {hex.EncodeToString(mac.Sum(nil))}}
}

// Creem gives each webhook endpoint its own secret: with SubscriptionWebhookSecret set,
// each webhook accepts only its own endpoint's signatures.
func TestWebhookSecrets(t *testing.T) {
	c, err := New(Config{APIKey: "creem_test_key", WebhookSecret: secret, SubscriptionWebhookSecret: "whsec_subscriptions", ProductID: "prod_credits"})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"id": "evt_1", "eventType": "credits.granted", "object": {}}`
	if _, err := c.ParseWebhook(signed(body), []byte(body)); !errors.Is(err, payments.ErrIgnored) {
		t.Errorf("credits webhook with its secret: %v", err)
	}
	if _, err := c.ParseWebhook(signedWith("whsec_subscriptions", body), []byte(body)); !errors.Is(err, payments.ErrInvalid) {
		t.Errorf("credits webhook with the subscriptions' secret: %v", err)
	}
	if _, err := c.ParseSubscriptionWebhook(signedWith("whsec_subscriptions", body), []byte(body)); !errors.Is(err, payments.ErrIgnored) {
		t.Errorf("subscriptions webhook with its secret: %v", err)
	}
	if _, err := c.ParseSubscriptionWebhook(signed(body), []byte(body)); !errors.Is(err, payments.ErrInvalid) {
		t.Errorf("subscriptions webhook with the credits' secret: %v", err)
	}
}

func TestNew(t *testing.T) {
	if _, err := New(Config{APIKey: "k"}); err == nil {
		t.Error("accepted a config without a webhook secret")
	}
	// Only credit checkouts need the product.
	noProduct, err := New(Config{APIKey: "k", WebhookSecret: "s"})
	if err != nil {
		t.Fatalf("a config without a credits product: %v", err)
	}
	if _, err := noProduct.CreateCheckout(context.Background(), payments.Checkout{Reference: "r", AmountCents: 500}); err == nil {
		t.Error("a credit checkout without a product")
	}
	if _, err := New(Config{APIKey: "k", WebhookSecret: "s", ProductID: "p", CheckoutOrigin: "http://pay.example"}); err == nil {
		t.Error("accepted a plain-HTTP checkout origin")
	}
	live, _ := New(Config{APIKey: "creem_live", WebhookSecret: "s", ProductID: "p"})
	custom, _ := New(Config{APIKey: "creem_live", WebhookSecret: "s", ProductID: "p", CheckoutOrigin: "https://pay.example"})
	test, _ := New(Config{APIKey: "creem_test_x", WebhookSecret: "s", ProductID: "p", CheckoutOrigin: "https://pay.example"})
	if live.api != "https://api.creem.io" || test.api != "https://test-api.creem.io" {
		t.Errorf("APIs %q %q", live.api, test.api)
	}
	if live.CheckoutOrigin() != DefaultCheckoutOrigin || custom.CheckoutOrigin() != "https://pay.example" || test.CheckoutOrigin() != "https://www.creem.io" {
		t.Errorf("origins %q %q %q", live.CheckoutOrigin(), custom.CheckoutOrigin(), test.CheckoutOrigin())
	}
}

func TestCreateCheckout(t *testing.T) {
	var got map[string]any
	checkoutURL := "https://creem.io/test/checkout/prod_credits/ch_1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/checkouts" || r.Header.Get("X-Api-Key") != "creem_test_key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]string{"id": "ch_1", "checkout_url": checkoutURL})
	}))
	defer srv.Close()
	c := adapter(t, srv.URL)

	s, err := c.CreateCheckout(context.Background(), payments.Checkout{Reference: "intent-1", AmountCents: 1083, Email: "ada@example.com", SuccessURL: "https://app.example/credits"})
	// creem.io redirects to www.creem.io, which the page's form-action allows: the
	// browser goes there directly.
	if err != nil || s.ID != "ch_1" || s.URL != "https://www.creem.io/test/checkout/prod_credits/ch_1" {
		t.Fatalf("session %+v, %v", s, err)
	}
	if got["product_id"] != "prod_credits" || got["request_id"] != "intent-1" || got["custom_price"] != float64(1083) ||
		got["metadata"].(map[string]any)["intent_id"] != "intent-1" || got["customer"].(map[string]any)["email"] != "ada@example.com" ||
		len(got) != 6 {
		t.Errorf("request %v", got)
	}

	checkoutURL = "https://attacker.example/checkout"
	if _, err := c.CreateCheckout(context.Background(), payments.Checkout{Reference: "x", AmountCents: 500}); err == nil {
		t.Error("accepted a checkout URL on another origin")
	}
}

func TestCreateCheckoutKeepsResponseBodiesOutOfErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"message":"buyer ada@example.com rejected"}`)
	}))
	defer srv.Close()
	_, err := adapter(t, srv.URL).CreateCheckout(context.Background(), payments.Checkout{Reference: "x", AmountCents: 500})
	if err == nil || strings.Contains(err.Error(), "ada@example.com") {
		t.Fatalf("error %v", err)
	}
}

func TestParseWebhookVerifiesSignatures(t *testing.T) {
	c := adapter(t, "")
	body := `{"id":"evt_1","eventType":"checkout.completed","object":{}}`
	for name, header := range map[string]http.Header{
		"missing":   {},
		"not hex":   {"Creem-Signature": {"zz"}},
		"short":     {"Creem-Signature": {"abcd"}},
		"other key": {"Creem-Signature": {strings.Repeat("0", 64)}},
	} {
		if _, err := c.ParseWebhook(header, []byte(body)); !errors.Is(err, payments.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.ParseWebhook(signed(body), []byte(body+" ")); !errors.Is(err, payments.ErrInvalid) {
		t.Error("accepted a changed body")
	}
	for _, bad := range []string{`not json`, `{"eventType":"checkout.completed","object":{}}`, `{"id":"e","eventType":"x"}`} {
		if _, err := c.ParseWebhook(signed(bad), []byte(bad)); !errors.Is(err, payments.ErrInvalid) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

// paid is a payment in SEK of a price in EUR: Creem gives the order's amount_paid and
// tax_amount in SEK, though the order says EUR.
const paid = `{"id":"evt_paid","eventType":"checkout.completed","created_at":1760000000123,"object":{
	"id":"ch_1","request_id":"intent-1","status":"completed","metadata":{"intent_id":"intent-1"},
	"product":{"id":"prod_credits"},
	"order":{"id":"ord_1","amount":1083,"sub_total":1083,"tax_amount":3097,"amount_due":15487,"amount_paid":15487,
		"currency":"eur","transaction":"tran_1","status":"paid","type":"onetime"}}}`

func TestParsePaid(t *testing.T) {
	c := adapter(t, "")
	e, err := c.ParseWebhook(signed(paid), []byte(paid))
	if err != nil {
		t.Fatal(err)
	}
	want := payments.Event{ID: "evt_paid", Kind: payments.Paid, Reference: "intent-1", CheckoutID: "ch_1", OrderID: "ord_1", AmountCents: 1083, PaidCents: 1083, Currency: "EUR",
		CreatedAt: time.UnixMilli(1760000000123).UTC()}
	if e != want {
		t.Fatalf("event %+v", e)
	}
	discounted := strings.Replace(paid, `"sub_total":1083`, `"sub_total":1083,"discount":"dis_1","discount_amount":200`, 1)
	if e, err := c.ParseWebhook(signed(discounted), []byte(discounted)); err != nil || e.AmountCents != 1083 || e.PaidCents != 883 {
		t.Errorf("discount code: %+v %v", e, err)
	}

	cases := map[string]struct {
		from, to string
		err      error
		review   string
	}{
		"product as ID":     {`"product":{"id":"prod_credits"}`, `"product":"prod_credits"`, nil, ""},
		"foreign product":   {`"product":{"id":"prod_credits"}`, `"product":"prod_other"`, payments.ErrIgnored, ""},
		"subscription":      {`"type":"onetime"`, `"type":"recurring"`, payments.ErrIgnored, ""},
		"not paid":          {`"status":"paid"`, `"status":"pending"`, nil, "checkout_not_paid"},
		"missing reference": {`"metadata":{"intent_id":"intent-1"}`, `"metadata":{}`, nil, "missing_reference"},
	}
	for name, tc := range cases {
		body := strings.Replace(paid, tc.from, tc.to, 1)
		e, err := c.ParseWebhook(signed(body), []byte(body))
		if !errors.Is(err, tc.err) || (err == nil && e.Review != tc.review) {
			t.Errorf("%s: %+v %v", name, e, err)
		}
	}
}

func TestParseReversals(t *testing.T) {
	c := adapter(t, "")
	// A refund of the SEK payment, in SEK, with the transaction it refunds.
	refund := `{"id":"evt_refund","eventType":"refund.created","object":{"id":"ref_1","status":"succeeded",
		"refund_amount":15487,"refund_currency":"SEK",
		"transaction":{"id":"tran_1","amount":12390,"amount_paid":15487,"currency":"SEK","tax_amount":3097,"status":"refunded"},
		"checkout":{"id":"ch_1","request_id":"intent-1","product":"prod_credits"},
		"order":{"id":"ord_1","amount":1083,"currency":"EUR"}}}`
	e, err := c.ParseWebhook(signed(refund), []byte(refund))
	if err != nil || e.Kind != payments.Refunded || e.Reference != "intent-1" || e.OrderID != "ord_1" || e.Review != "" ||
		e.AmountCents != 15487 || e.PaidCents != 15487 || e.Currency != "SEK" {
		t.Fatalf("refund %+v %v", e, err)
	}
	partial := strings.Replace(refund, `"refund_amount":15487`, `"refund_amount":7000`, 1)
	if e, _ := c.ParseWebhook(signed(partial), []byte(partial)); e.AmountCents != 7000 || e.PaidCents != 15487 {
		t.Errorf("partial refund %+v", e)
	}
	// Without the transaction, or in another currency, it says nothing of what was paid.
	for name, body := range map[string]string{
		"transaction as ID":   strings.Replace(refund, `{"id":"tran_1","amount":12390,"amount_paid":15487,"currency":"SEK","tax_amount":3097,"status":"refunded"}`, `"tran_1"`, 1),
		"in another currency": strings.Replace(refund, `"refund_currency":"SEK"`, `"refund_currency":"EUR"`, 1),
	} {
		if e, err := c.ParseWebhook(signed(body), []byte(body)); err != nil || e.PaidCents != 0 {
			t.Errorf("%s: %+v %v", name, e, err)
		}
	}
	pending := strings.Replace(refund, `"succeeded"`, `"pending"`, 1)
	if e, _ := c.ParseWebhook(signed(pending), []byte(pending)); e.Review != "refund_not_succeeded" {
		t.Errorf("pending refund %+v", e)
	}
	dispute := `{"id":"evt_dispute","eventType":"dispute.created","object":{"id":"dis_1","amount":1310,"currency":"eur",
		"transaction":{"id":"tran_1","amount":1083,"amount_paid":1310,"currency":"EUR","tax_amount":227,"status":"chargeback"},
		"checkout":"ch_1","order":"ord_1"}}`
	e, err = c.ParseWebhook(signed(dispute), []byte(dispute))
	if err != nil || e.Kind != payments.Disputed || e.CheckoutID != "ch_1" || e.OrderID != "ord_1" || e.Currency != "EUR" ||
		e.AmountCents != 1310 || e.PaidCents != 1310 {
		t.Fatalf("dispute %+v %v", e, err)
	}
	other := `{"id":"evt_x","eventType":"subscription.active","object":{"id":"sub_1"}}`
	if _, err := c.ParseWebhook(signed(other), []byte(other)); !errors.Is(err, payments.ErrIgnored) {
		t.Errorf("subscription event: %v", err)
	}
}
