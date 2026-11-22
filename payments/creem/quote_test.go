package creem

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coolapso/c4stack/payments"
)

// quoteAPI answers a subscription and the products it can change between, as Creem's
// test API did.
func quoteAPI(t *testing.T, subscription string, reads *int) *Creem {
	t.Helper()
	products := map[string]string{
		"prod_plus_m":   `{"id": "prod_plus_m", "price": 500, "currency": "EUR", "billing_type": "recurring", "recurring_interval": "month", "recurring_interval_count": 1}`,
		"prod_plus_y":   `{"id": "prod_plus_y", "price": 5000, "currency": "EUR", "billing_type": "recurring", "recurring_interval": "year", "recurring_interval_count": 1}`,
		"prod_family_y": `{"id": "prod_family_y", "price": 15000, "currency": "EUR", "billing_type": "recurring", "recurring_interval": "year", "recurring_interval_count": 1}`,
		"prod_usd":      `{"id": "prod_usd", "price": 5000, "currency": "USD", "billing_type": "recurring", "recurring_interval": "year"}`,
		"prod_once":     `{"id": "prod_once", "price": 5000, "currency": "EUR", "billing_type": "onetime"}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/subscriptions":
			_, _ = io.WriteString(w, subscription)
		case "/v1/products":
			if reads != nil {
				*reads++
			}
			p, ok := products[r.URL.Query().Get("product_id")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, p)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return adapter(t, srv.URL)
}

func sub(product, start, end, last string) string {
	return fmt.Sprintf(`{"id": "sub_1", "status": "active", "product": {"id": %q},
		"items": [{"id": "sitem_1", "product_id": %q, "units": 1}],
		"current_period_start_date": %q, "current_period_end_date": %q, "last_transaction": %s}`,
		product, product, start, end, last)
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The amounts are what Creem's test API charged and refunded for these changes, with
// Swedish VAT (25%).
func TestQuoteChange(t *testing.T) {
	const yearly = `{"id": "tran_1", "amount": 5000, "amount_paid": 6250, "tax_amount": 1250}`
	const monthly = `{"id": "tran_2", "amount": 500, "amount_paid": 625, "tax_amount": 125}`
	ctx := context.Background()
	for name, tc := range map[string]struct {
		sub    string
		change payments.SubscriptionChange
		at     string
		want   payments.ChangeQuote
	}{
		// Plus to Family, yearly, 24 seconds into the year: €100 + €25 VAT.
		"same period, more": {
			sub:    sub("prod_plus_y", "2026-11-21T23:49:46.000Z", "2027-11-21T23:49:46.000Z", yearly),
			change: payments.SubscriptionChange{ProductID: "prod_family_y"},
			at:     "2026-11-21T23:50:10Z",
			want: payments.ChangeQuote{Currency: "EUR", TaxIncluded: true, Charge: 12500, ChargeTax: 2500,
				Renewal: 18750, RenewalTax: 3750, RenewsAt: at(t, "2027-11-21T23:49:46Z")},
		},
		// Family to Plus, halfway through the year: half of €100, and its VAT, back.
		"same period, less": {
			sub:    sub("prod_family_y", "2027-01-01T00:00:00.000Z", "2028-01-01T00:00:00.000Z", yearly),
			change: payments.SubscriptionChange{ProductID: "prod_plus_y"},
			at:     "2027-07-02T12:00:00Z",
			want: payments.ChangeQuote{Currency: "EUR", TaxIncluded: true, Refund: 6250,
				Renewal: 6250, RenewalTax: 1250, RenewsAt: at(t, "2028-01-01T00:00:00Z")},
		},
		// Plus monthly to yearly, 12½ minutes into the month: a new year, and the month
		// back, which rounds to all of it.
		"to a longer period": {
			sub:    sub("prod_plus_m", "2026-11-21T23:37:14.000Z", "2026-12-21T23:37:14.000Z", monthly),
			change: payments.SubscriptionChange{ProductID: "prod_plus_y"},
			at:     "2026-11-21T23:49:46Z",
			want: payments.ChangeQuote{Currency: "EUR", TaxIncluded: true, NewPeriod: true, Charge: 6250, ChargeTax: 1250, Refund: 625,
				Renewal: 6250, RenewalTax: 1250, RenewsAt: at(t, "2027-11-21T23:49:46Z")},
		},
		// Yearly to monthly as the year ends: a month, and nothing back.
		"to a shorter period, at its end": {
			sub:    sub("prod_plus_y", "2026-11-21T23:49:46.000Z", "2027-11-21T23:49:46.000Z", yearly),
			change: payments.SubscriptionChange{ProductID: "prod_plus_m"},
			at:     "2027-11-21T23:49:46Z",
			want: payments.ChangeQuote{Currency: "EUR", TaxIncluded: true, NewPeriod: true, Charge: 625, ChargeTax: 125,
				Renewal: 625, RenewalTax: 125, RenewsAt: at(t, "2027-12-21T23:49:46Z")},
		},
		"no proration": {
			sub:    sub("prod_plus_y", "2026-11-21T23:49:46.000Z", "2027-11-21T23:49:46.000Z", yearly),
			change: payments.SubscriptionChange{ProductID: "prod_family_y", Proration: payments.ProrateNone},
			at:     "2027-05-01T00:00:00Z",
			want: payments.ChangeQuote{Currency: "EUR", TaxIncluded: true,
				Renewal: 18750, RenewalTax: 3750, RenewsAt: at(t, "2027-11-21T23:49:46Z")},
		},
		// Without a payment to take the rate from, the amounts are before tax.
		"no payment yet": {
			sub:    sub("prod_plus_y", "2026-11-21T23:49:46.000Z", "2027-11-21T23:49:46.000Z", `null`),
			change: payments.SubscriptionChange{ProductID: "prod_family_y"},
			at:     "2026-11-21T23:49:46Z",
			want: payments.ChangeQuote{Currency: "EUR", Charge: 10000,
				Renewal: 15000, RenewsAt: at(t, "2027-11-21T23:49:46Z")},
		},
		// A last transaction given as an ID says no rate either.
		"payment as an ID": {
			sub:    sub("prod_plus_y", "2026-11-21T23:49:46.000Z", "2027-11-21T23:49:46.000Z", `"tran_1"`),
			change: payments.SubscriptionChange{Units: 2},
			at:     "2026-11-21T23:49:46Z",
			want: payments.ChangeQuote{Currency: "EUR", Charge: 5000,
				Renewal: 10000, RenewsAt: at(t, "2027-11-21T23:49:46Z")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := quoteAPI(t, tc.sub, nil).QuoteChange(ctx, "sub_1", tc.change, at(t, tc.at))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("quote\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestQuoteChangeRefusals(t *testing.T) {
	ctx := context.Background()
	period := func(product string) string {
		return sub(product, "2026-11-21T23:49:46.000Z", "2027-11-21T23:49:46.000Z", `null`)
	}
	for name, tc := range map[string]struct{ sub, to string }{
		"another currency": {period("prod_plus_y"), "prod_usd"},
		"not recurring":    {period("prod_plus_y"), "prod_once"},
		"unknown product":  {period("prod_plus_y"), "prod_gone"},
		"no period":        {sub("prod_plus_y", "", "", `null`), "prod_family_y"},
		"two items": {`{"id": "sub_1", "items": [{"product_id": "prod_plus_y"}, {"product_id": "prod_plus_m"}],
			"current_period_start_date": "2026-11-21T23:49:46.000Z", "current_period_end_date": "2027-11-21T23:49:46.000Z"}`, "prod_family_y"},
	} {
		if _, err := quoteAPI(t, tc.sub, nil).QuoteChange(ctx, "sub_1", payments.SubscriptionChange{ProductID: tc.to}, time.Time{}); err == nil {
			t.Errorf("%s: quoted", name)
		}
	}
	// A page quoting several changes reads each product once.
	reads := 0
	c := quoteAPI(t, period("prod_plus_y"), &reads)
	for range 3 {
		if _, err := c.QuoteChange(ctx, "sub_1", payments.SubscriptionChange{ProductID: "prod_family_y"}, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 2 {
		t.Fatalf("read products %d times", reads)
	}
}
