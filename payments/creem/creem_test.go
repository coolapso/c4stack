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
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return http.Header{"Creem-Signature": {hex.EncodeToString(mac.Sum(nil))}}
}

func TestNew(t *testing.T) {
	if _, err := New(Config{APIKey: "k", WebhookSecret: "s"}); err == nil {
		t.Error("accepted a config without a product")
	}
	if _, err := New(Config{APIKey: "k", WebhookSecret: "s", ProductID: "p", CheckoutOrigin: "http://pay.example"}); err == nil {
		t.Error("accepted a plain-HTTP checkout origin")
	}
	live, _ := New(Config{APIKey: "creem_live", WebhookSecret: "s", ProductID: "p"})
	test, _ := New(Config{APIKey: "creem_test_x", WebhookSecret: "s", ProductID: "p", CheckoutOrigin: "https://pay.example"})
	if live.api != "https://api.creem.io" || test.api != "https://test-api.creem.io" || test.CheckoutOrigin() != "https://pay.example" {
		t.Errorf("APIs %q %q, origin %q", live.api, test.api, test.CheckoutOrigin())
	}
}

func TestCreateCheckout(t *testing.T) {
	var got map[string]any
	checkoutURL := "https://www.creem.io/checkout/ch_1"
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
	if err != nil || s.ID != "ch_1" || s.URL != checkoutURL {
		t.Fatalf("session %+v, %v", s, err)
	}
	if got["product_id"] != "prod_credits" || got["request_id"] != "intent-1" || got["custom_price"] != float64(1083) ||
		got["metadata"].(map[string]any)["reference"] != "intent-1" || got["customer"].(map[string]any)["email"] != "ada@example.com" {
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

const paid = `{"id":"evt_paid","eventType":"checkout.completed","object":{
	"id":"ch_1","request_id":"intent-1","status":"completed","metadata":{"reference":"intent-1"},
	"product":{"id":"prod_credits"},
	"order":{"id":"ord_1","amount":1083,"amount_paid":1310,"currency":"eur","status":"paid","type":"onetime"}}}`

func TestParsePaid(t *testing.T) {
	c := adapter(t, "")
	e, err := c.ParseWebhook(signed(paid), []byte(paid))
	if err != nil {
		t.Fatal(err)
	}
	want := payments.Event{ID: "evt_paid", Kind: payments.Paid, Reference: "intent-1", CheckoutID: "ch_1", OrderID: "ord_1", AmountCents: 1083, PaidCents: 1310, Currency: "EUR"}
	if e != want {
		t.Fatalf("event %+v", e)
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
		"missing reference": {`"metadata":{"reference":"intent-1"}`, `"metadata":{}`, nil, "missing_reference"},
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
	refund := `{"id":"evt_refund","eventType":"refund.created","object":{"id":"ref_1","status":"succeeded",
		"refund_amount":1310,"refund_currency":"EUR","checkout":{"id":"ch_1","request_id":"intent-1","product":"prod_credits"},
		"order":{"id":"ord_1"}}}`
	e, err := c.ParseWebhook(signed(refund), []byte(refund))
	if err != nil || e.Kind != payments.Refunded || e.Reference != "intent-1" || e.AmountCents != 1310 || e.OrderID != "ord_1" || e.Review != "" {
		t.Fatalf("refund %+v %v", e, err)
	}
	pending := strings.Replace(refund, `"succeeded"`, `"pending"`, 1)
	if e, _ := c.ParseWebhook(signed(pending), []byte(pending)); e.Review != "refund_not_succeeded" {
		t.Errorf("pending refund %+v", e)
	}
	dispute := `{"id":"evt_dispute","eventType":"dispute.created","object":{"id":"dis_1","amount":1310,"currency":"eur",
		"checkout":"ch_1","order":"ord_1"}}`
	e, err = c.ParseWebhook(signed(dispute), []byte(dispute))
	if err != nil || e.Kind != payments.Disputed || e.CheckoutID != "ch_1" || e.OrderID != "ord_1" || e.Currency != "EUR" {
		t.Fatalf("dispute %+v %v", e, err)
	}
	other := `{"id":"evt_x","eventType":"subscription.active","object":{"id":"sub_1"}}`
	if _, err := c.ParseWebhook(signed(other), []byte(other)); !errors.Is(err, payments.ErrIgnored) {
		t.Errorf("subscription event: %v", err)
	}
}
