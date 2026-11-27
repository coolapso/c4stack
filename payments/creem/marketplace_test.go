package creem

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coolapso/c4stack/payments"
)

// provisionRequest has the shape of the example in Creem's Agentic Marketplace guide.
const provisionRequest = `{
	"id": "evt_6b0c1c9e",
	"eventType": "marketplace.grant.requested",
	"created_at": 1763820000000,
	"object": {
		"storeId": "sto_1",
		"grantId": "grt_1",
		"operationId": "op_1",
		"action": "provision",
		"deadlineAt": "2026-11-23T14:00:00.000Z",
		"listingId": "lst_1",
		"productId": "prod_pack",
		"order": {
			"id": "ord_1",
			"status": "paid",
			"amountMinor": 1083,
			"currency": "eur",
			"transactionId": "tran_1",
			"paidAt": "2026-11-22T14:00:00.000Z"
		},
		"test": false
	}
}`

func TestParseGrantRequest(t *testing.T) {
	c := adapter(t, "")
	r, err := c.ParseGrantRequest(signed(provisionRequest), []byte(provisionRequest))
	if err != nil {
		t.Fatal(err)
	}
	want := payments.GrantRequest{
		EventID: "evt_6b0c1c9e", GrantID: "grt_1", OperationID: "op_1", Action: payments.GrantProvision,
		ProductID: "prod_pack", ListingID: "lst_1", OrderID: "ord_1", OrderPaid: true,
		AmountCents: 1083, Currency: "EUR",
		PaidAt:    time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC),
		Deadline:  time.Date(2026, 11, 23, 14, 0, 0, 0, time.UTC),
		CreatedAt: time.UnixMilli(1763820000000).UTC(),
	}
	if r != want {
		t.Fatalf("got  %+v\nwant %+v", r, want)
	}

	if _, err := c.ParseGrantRequest(signedWith("whsec_other", provisionRequest), []byte(provisionRequest)); !errors.Is(err, payments.ErrInvalid) {
		t.Fatalf("another secret: %v", err)
	}
	other := `{"id": "evt_2", "eventType": "checkout.completed", "object": {}}`
	if r, err := c.ParseGrantRequest(signed(other), []byte(other)); !errors.Is(err, payments.ErrIgnored) || r.EventID != "evt_2" {
		t.Fatalf("another event: %+v %v", r, err)
	}
}

func TestParseGrantRequestShapes(t *testing.T) {
	c := adapter(t, "")
	request := func(object string) string {
		return `{"id": "evt_1", "eventType": "marketplace.grant.requested", "object": ` + object + `}`
	}
	tests := []struct {
		name   string
		object string
		err    error
		check  func(payments.GrantRequest) bool
	}{
		{"a revocation names no order and no deadline",
			`{"grantId": "g", "operationId": "o", "action": "revoke", "deadlineAt": null, "reason": "chargeback"}`, nil,
			func(r payments.GrantRequest) bool {
				return r.Action == payments.GrantRevoke && r.Deadline.IsZero() && r.Reason == "chargeback"
			}},
		{"a credit revocation names its order",
			`{"grantId": "g", "operationId": "o", "action": "revoke_credit", "order": {"id": "ord_2", "status": "refunded", "amountMinor": 500, "currency": "EUR"}}`, nil,
			func(r payments.GrantRequest) bool { return r.OrderID == "ord_2" && !r.OrderPaid }},
		{"a credit revocation without its order",
			`{"grantId": "g", "operationId": "o", "action": "revoke_credit"}`, payments.ErrInvalid, nil},
		{"a test provision may name no order",
			`{"grantId": "g", "operationId": "o", "action": "provision", "test": true}`, nil,
			func(r payments.GrantRequest) bool { return r.Test && r.OrderID == "" }},
		{"a paid provision without its order",
			`{"grantId": "g", "operationId": "o", "action": "provision"}`, payments.ErrInvalid, nil},
		{"an amount written with a fraction",
			`{"grantId": "g", "operationId": "o", "action": "top_up", "order": {"id": "ord_3", "status": "paid", "amountMinor": 2000.0, "currency": "EUR"}}`, nil,
			func(r payments.GrantRequest) bool { return r.AmountCents == 2000 }},
		{"an amount in fractions of a cent",
			`{"grantId": "g", "operationId": "o", "action": "top_up", "order": {"id": "ord_3", "status": "paid", "amountMinor": 20.5, "currency": "EUR"}}`, payments.ErrInvalid, nil},
		{"an unknown action",
			`{"grantId": "g", "operationId": "o", "action": "refund", "order": {"id": "ord_4"}}`, payments.ErrInvalid, nil},
		{"no operation",
			`{"grantId": "g", "action": "top_up", "order": {"id": "ord_4"}}`, payments.ErrInvalid, nil},
		{"an unreadable deadline",
			`{"grantId": "g", "operationId": "o", "action": "top_up", "deadlineAt": "tomorrow", "order": {"id": "ord_4"}}`, payments.ErrInvalid, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := request(tt.object)
			r, err := c.ParseGrantRequest(signed(body), []byte(body))
			if !errors.Is(err, tt.err) {
				t.Fatalf("error %v, want %v", err, tt.err)
			}
			if tt.check != nil && !tt.check(r) {
				t.Fatalf("decoded %+v", r)
			}
		})
	}
}

// Creem's support places a revocation's reason at the top level of the payload; the
// object's still counts first.
func TestRevocationReasonAtEitherLevel(t *testing.T) {
	c := adapter(t, "")
	for body, want := range map[string]string{
		`{"id": "evt_1", "eventType": "marketplace.grant.requested", "reason": "chargeback",
		  "object": {"grantId": "g", "operationId": "o", "action": "revoke"}}`: "chargeback",
		`{"id": "evt_1", "eventType": "marketplace.grant.requested", "reason": "chargeback",
		  "object": {"grantId": "g", "operationId": "o", "action": "revoke", "reason": "refund"}}`: "refund",
		`{"id": "evt_1", "eventType": "marketplace.grant.requested",
		  "object": {"grantId": "g", "operationId": "o", "action": "revoke"}}`: "",
	} {
		r, err := c.ParseGrantRequest(signed(body), []byte(body))
		if err != nil || r.Reason != want {
			t.Errorf("reason %q, want %q: %v", r.Reason, want, err)
		}
	}
}

func TestGrantReply(t *testing.T) {
	c := adapter(t, "")
	body, err := c.GrantReply(payments.GrantAnswer{
		Status:      payments.GrantReady,
		Env:         map[string]string{"ACME_API_KEY": "acme_secret", "ACME_CLAIM_URL": "https://acme.example/claim?token=t"},
		KeyHint:     "x7Qa9z",
		Entitlement: map[string]any{"credit": "€10.00"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "ready" || got["key_hint"] != "x7Qa9z" || got["env"].(map[string]any)["ACME_API_KEY"] != "acme_secret" ||
		got["entitlement"].(map[string]any)["credit"] != "€10.00" || len(got) != 4 {
		t.Fatalf("ready reply %s", body)
	}
	if body, err := c.GrantReply(payments.GrantAnswer{Status: payments.GrantReady}); err != nil || string(body) != `{"status":"ready"}` {
		t.Fatalf("a bare ready reply: %s %v", body, err)
	}
	if body, err := c.GrantReply(payments.GrantAnswer{Status: payments.GrantPending}); err != nil || string(body) != `{"status":"pending"}` {
		t.Fatalf("a pending reply: %s %v", body, err)
	}
	if body, err := c.GrantReply(payments.GrantAnswer{Status: payments.GrantFailed, FailureReason: "Unknown product"}); err != nil ||
		string(body) != `{"status":"failed","failure_reason":"Unknown product"}` {
		t.Fatalf("a failed reply: %s %v", body, err)
	}

	refused := map[string]payments.GrantAnswer{
		"a failure without a reason":    {Status: payments.GrantFailed},
		"a failure with credentials":    {Status: payments.GrantFailed, FailureReason: "x", Env: map[string]string{"A": "b"}},
		"a long failure reason":         {Status: payments.GrantFailed, FailureReason: strings.Repeat("x", 201)},
		"a ready answer with a reason":  {Status: payments.GrantReady, FailureReason: "x"},
		"a pending answer with more":    {Status: payments.GrantPending, KeyHint: "x"},
		"a lowercase credential name":   {Status: payments.GrantReady, Env: map[string]string{"api_key": "x"}},
		"a credential that isn't ASCII": {Status: payments.GrantReady, Env: map[string]string{"KEY": "clé"}},
		"an empty credential":           {Status: payments.GrantReady, Env: map[string]string{"KEY": ""}},
		"a long key hint":               {Status: payments.GrantReady, KeyHint: "123456789"},
		"a large entitlement":           {Status: payments.GrantReady, Entitlement: map[string]any{"x": strings.Repeat("x", 4096)}},
		"an unknown status":             {Status: "done"},
	}
	many := map[string]string{}
	for i := range 17 {
		many["KEY_"+string(rune('A'+i))] = "x"
	}
	refused["seventeen credentials"] = payments.GrantAnswer{Status: payments.GrantReady, Env: many}
	for name, a := range refused {
		if _, err := c.GrantReply(a); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
