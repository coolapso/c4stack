package creem

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coolapso/c4stack/payments"
)

var _ payments.ChangeQuoter = (*Creem)(nil)

// QuoteChange says what a change would charge and refund, as Creem works it out, since
// Creem's API quotes nothing before it changes a subscription:
//   - Between products with the same billing period, the period stays, and the
//     difference in price for the time left is charged, or refunded.
//   - To another billing period, a new period starts when the change is made: the new
//     product's whole price is charged, and what was paid for the time left is refunded.
//
// With ProrateNone, nothing is charged or refunded for the time left; a new period is
// still charged in full. Tax is at the rate of the subscription's last payment, as
// Creem charged it.
func (c *Creem) QuoteChange(ctx context.Context, id string, ch payments.SubscriptionChange, at time.Time) (payments.ChangeQuote, error) {
	var s subscription
	if err := c.call(ctx, http.MethodGet, "/v1/subscriptions?"+url.Values{"subscription_id": {id}}.Encode(), nil, &s); err != nil {
		return payments.ChangeQuote{}, err
	}
	if len(s.Items) != 1 {
		return payments.ChangeQuote{}, fmt.Errorf("creem: subscription %s has %d items, not one", id, len(s.Items))
	}
	start, err1 := time.Parse(time.RFC3339, s.PeriodStart)
	end, err2 := time.Parse(time.RFC3339, s.PeriodEnd)
	if err1 != nil || err2 != nil || !end.After(start) {
		return payments.ChangeQuote{}, fmt.Errorf("creem: subscription %s has no current period", id)
	}
	if at.IsZero() {
		at = time.Now()
	}
	if at.Before(start) {
		at = start
	}
	if at.After(end) {
		at = end
	}

	fromID := s.Items[0].ProductID
	if fromID == "" {
		fromID = string(s.Product)
	}
	toID := ch.ProductID
	if toID == "" {
		toID = fromID
	}
	from, err := c.product(ctx, fromID)
	if err != nil {
		return payments.ChangeQuote{}, err
	}
	to, err := c.product(ctx, toID)
	if err != nil {
		return payments.ChangeQuote{}, err
	}
	if !strings.EqualFold(from.Currency, to.Currency) {
		return payments.ChangeQuote{}, fmt.Errorf("creem: products %s and %s have different currencies", fromID, toID)
	}
	fromPeriod, ok1 := from.period()
	toPeriod, ok2 := to.period()
	if !ok1 || !ok2 {
		return payments.ChangeQuote{}, fmt.Errorf("creem: products %s and %s must both be recurring", fromID, toID)
	}
	fromUnits := int64(max(s.Items[0].Units, 1))
	toUnits := fromUnits
	if ch.Units > 0 {
		toUnits = int64(ch.Units)
	}
	tax, taxed := s.LastTransaction.taxRate()
	left, length := end.Sub(at), end.Sub(start)
	price := to.Price * toUnits

	q := payments.ChangeQuote{Currency: strings.ToUpper(to.Currency), TaxIncluded: taxed}
	q.RenewalTax = tax(price)
	q.Renewal = price + q.RenewalTax
	if fromPeriod == toPeriod {
		q.RenewsAt = end
		if ch.Proration == payments.ProrateNone {
			return q, nil
		}
		switch diff := prorate(price-from.Price*fromUnits, left, length); {
		case diff > 0:
			q.ChargeTax = tax(diff)
			q.Charge = diff + q.ChargeTax
		case diff < 0:
			q.Refund = -diff + tax(-diff)
		}
		return q, nil
	}
	q.NewPeriod = true
	q.RenewsAt = toPeriod.after(at)
	q.Charge, q.ChargeTax = q.Renewal, q.RenewalTax
	if ch.Proration != payments.ProrateNone {
		unused := prorate(from.Price*fromUnits, left, length)
		q.Refund = unused + tax(unused)
	}
	return q, nil
}

// prorate returns amount for the part left of length, rounded to the nearest cent.
func prorate(amount int64, left, length time.Duration) int64 {
	if left >= length {
		return amount
	}
	return roundDiv(amount*int64(left/time.Second), int64(length/time.Second))
}

// roundDiv divides, rounding halves away from zero.
func roundDiv(a, b int64) int64 {
	if a < 0 {
		return -roundDiv(-a, b)
	}
	return (2*a + b) / (2 * b)
}

// payment is the part of a Creem transaction a quote reads. A subscription carries its
// last one, expanded, or as an ID.
type payment struct {
	ID         string `json:"id"`
	AmountPaid int64  `json:"amount_paid"`
	TaxAmount  int64  `json:"tax_amount"`
}

func (p *payment) UnmarshalJSON(data []byte) error {
	type plain payment
	return expanded(data, &p.ID, (*plain)(p))
}

// taxRate returns the tax on an amount at the payment's rate, and whether the payment
// says the rate. Creem's amount_paid includes the tax, whatever the transaction.
func (p *payment) taxRate() (func(int64) int64, bool) {
	none := func(int64) int64 { return 0 }
	if p == nil || p.AmountPaid <= p.TaxAmount || p.TaxAmount < 0 {
		return none, false
	}
	net := p.AmountPaid - p.TaxAmount
	return func(amount int64) int64 { return roundDiv(amount*p.TaxAmount, net) }, true
}

// product is the part of a Creem product a quote reads.
type product struct {
	Price         int64  `json:"price"`
	Currency      string `json:"currency"`
	BillingType   string `json:"billing_type"`
	Interval      string `json:"recurring_interval"`
	IntervalCount int    `json:"recurring_interval_count"`
}

// billingPeriod is a recurring product's period, as a calendar step.
type billingPeriod struct{ years, months, days int }

func (b billingPeriod) after(t time.Time) time.Time { return t.AddDate(b.years, b.months, b.days) }

func (p product) period() (billingPeriod, bool) {
	n := max(p.IntervalCount, 1)
	if p.BillingType != "recurring" {
		return billingPeriod{}, false
	}
	switch p.Interval {
	case "day":
		return billingPeriod{days: n}, true
	case "week":
		return billingPeriod{days: 7 * n}, true
	case "month":
		return billingPeriod{months: n}, true
	case "year":
		return billingPeriod{years: n}, true
	}
	return billingPeriod{}, false
}

// productTTL is how long a product's price is kept: a page that quotes several changes
// reads each product once, and a new price shows within minutes.
const productTTL = 10 * time.Minute

type productCache struct {
	mu      sync.Mutex
	entries map[string]cachedProduct
}

type cachedProduct struct {
	p  product
	at time.Time
}

// product reads a product, or the copy read within productTTL.
func (c *Creem) product(ctx context.Context, id string) (product, error) {
	if id == "" {
		return product{}, errors.New("creem: a subscription without a product")
	}
	c.products.mu.Lock()
	e, ok := c.products.entries[id]
	c.products.mu.Unlock()
	if ok && time.Since(e.at) < productTTL {
		return e.p, nil
	}
	var p product
	if err := c.call(ctx, http.MethodGet, "/v1/products?"+url.Values{"product_id": {id}}.Encode(), nil, &p); err != nil {
		return product{}, err
	}
	c.products.mu.Lock()
	c.products.entries[id] = cachedProduct{p: p, at: time.Now()}
	c.products.mu.Unlock()
	return p, nil
}
