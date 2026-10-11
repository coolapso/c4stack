package credits

import (
	"database/sql"
	"testing"

	"github.com/coolapso/c4stack/payments"
)

func TestFee(t *testing.T) {
	p := Policy{FeeBasisPoints: 390, FeeFixedCents: 40}
	if got := p.Fee(1000); got != 83 {
		t.Fatalf("Fee(1000) = %d, want 83", got)
	}
	// The fee covers the provider's percentage on the whole charge plus its fixed fee.
	for _, credit := range []int64{500, 1000, 1234, 99_999_999} {
		fee := p.Fee(credit)
		charge := credit + fee
		if covered := (charge*390+9_999)/10_000 + 40; fee < covered {
			t.Errorf("Fee(%d) = %d does not cover %d", credit, fee, covered)
		}
	}
	if (Policy{}).Fee(1000) != 0 {
		t.Error("a policy without fees charged one")
	}
}

func TestParseEuros(t *testing.T) {
	good := map[string]int64{"5": 500, "5.5": 550, "5.50": 550, "€12,05": 1205, " 999999.99 ": 99_999_999, "0.01": 1}
	for in, want := range good {
		if got, ok := ParseEuros(in); !ok || got != want {
			t.Errorf("ParseEuros(%q) = %d, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "4.999", "1e6", "-5", "NaN", "1000000", "5.", ".5", "5 000"} {
		if _, ok := ParseEuros(in); ok {
			t.Errorf("ParseEuros(%q) accepted", in)
		}
	}
	if FormatEuros(-305) != "−€3.05" || FormatEuros(0) != "€0.00" {
		t.Errorf("FormatEuros: %q %q", FormatEuros(-305), FormatEuros(0))
	}
}

func TestDecide(t *testing.T) {
	account := &Tx{accountID: "account"}
	deleted := &Tx{}
	pending := intent{id: "i", creditCents: 1000, chargeCents: 1083, currency: "EUR", status: "pending", checkoutID: sql.NullString{String: "co_1", Valid: true}}
	created := pending
	created.status, created.checkoutID = "created", sql.NullString{}
	paid := pending
	paid.status, paid.paidCents = "paid", sql.NullInt64{Int64: 1310, Valid: true}
	paid.orderID = sql.NullString{String: "ord_1", Valid: true}
	refunded := paid
	refunded.status = "refunded"
	chargedBack := paid
	chargedBack.status = "charged_back"

	pay := payments.Event{Kind: payments.Paid, AmountCents: 1083, PaidCents: 1310, Currency: "EUR", CheckoutID: "co_1"}
	refund := payments.Event{Kind: payments.Refunded, AmountCents: 1310, Currency: "EUR"}
	with := func(e payments.Event, change func(*payments.Event)) payments.Event { change(&e); return e }

	cases := []struct {
		name    string
		t       *Tx
		in      intent
		e       payments.Event
		outcome string
		review  string
	}{
		{"payment", account, pending, pay, "credited", ""},
		{"payment before the checkout was recorded", account, created, pay, "credited", ""},
		{"repeated payment", account, paid, pay, "duplicate", ""},
		{"payment after refund", account, refunded, pay, "duplicate", ""},
		{"other amount", account, pending, with(pay, func(e *payments.Event) { e.AmountCents = 999 }), "", "amount_mismatch"},
		{"discount", account, pending, with(pay, func(e *payments.Event) { e.PaidCents = 0 }), "", "discounted"},
		{"other currency", account, pending, with(pay, func(e *payments.Event) { e.Currency = "USD" }), "", "currency_mismatch"},
		{"other checkout", account, pending, with(pay, func(e *payments.Event) { e.CheckoutID = "co_2" }), "", "checkout_mismatch"},
		{"payment after deletion", deleted, pending, pay, "paid", "paid_after_account_deleted"},
		{"refund", account, paid, refund, "reversed", ""},
		{"dispute", account, paid, with(refund, func(e *payments.Event) { e.Kind = payments.Disputed }), "reversed", ""},
		{"repeated refund", account, refunded, refund, "duplicate", ""},
		{"refund after dispute", account, chargedBack, refund, "duplicate", ""},
		{"refund of another checkout", account, paid, with(refund, func(e *payments.Event) { e.CheckoutID = "co_2" }), "", "checkout_mismatch"},
		{"refund of another order", account, paid, with(refund, func(e *payments.Event) { e.OrderID = "ord_2" }), "", "order_mismatch"},
		{"partial refund", account, paid, with(refund, func(e *payments.Event) { e.AmountCents = 500 }), "", "partial_reversal"},
		{"refund before payment", account, pending, refund, "", "reversal_before_payment"},
	}
	for _, c := range cases {
		got := decide(c.t, c.in, c.e)
		if got.outcome != c.outcome || got.review != c.review {
			t.Errorf("%s: outcome %q review %q, want %q %q", c.name, got.outcome, got.review, c.outcome, c.review)
		}
	}
}

func TestAvailable(t *testing.T) {
	cases := []struct{ balance, held, want int64 }{
		{1000, 0, 1000}, {1000, 300, 700}, {1000, 1500, 0}, {-200, 0, -200}, {-200, 100, -200}, {0, 50, 0},
	}
	for _, c := range cases {
		if got := available(c.balance, c.held); got != c.want {
			t.Errorf("available(%d, %d) = %d, want %d", c.balance, c.held, got, c.want)
		}
	}
}
