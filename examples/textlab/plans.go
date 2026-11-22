package main

import (
	"fmt"
	"time"

	"github.com/coolapso/c4stack/payments"
	"github.com/coolapso/c4stack/subscriptions"
)

// The plans, as subscriptions in one group: an account has one plan at a time, and
// changes between them. Without a plan, an analysis costs analyseCost.
const planGroup = "plan"

// plan is a plan as the overview offers it.
type plan struct {
	Name string
	// Gives is what the plan changes, in a sentence.
	Gives string
	// Monthly and Yearly are its products' IDs, one per billing period.
	Monthly, Yearly string
	// PerMonth and PerYear are its prices, before VAT, as Creem's products have them.
	PerMonth, PerYear string
	// AnalyseCost is what an analysis costs on it, in credits (EUR cents).
	AnalyseCost int64
}

var plans = []plan{
	{Name: "Pro", Gives: "Every analysis costs half.", Monthly: "pro-monthly", Yearly: "pro-yearly", PerMonth: "€5", PerYear: "€50", AnalyseCost: 1},
	{Name: "Max", Gives: "Analyses are free.", Monthly: "max-monthly", Yearly: "max-yearly", PerMonth: "€15", PerYear: "€150", AnalyseCost: 0},
}

// planProducts are the plans' products, with Creem's.
func planProducts(cfg config) []subscriptions.Product {
	return []subscriptions.Product{
		{ID: "pro-monthly", ProviderID: cfg.CreemProMonthly, Name: "Pro, monthly", Group: planGroup},
		{ID: "pro-yearly", ProviderID: cfg.CreemProYearly, Name: "Pro, yearly", Group: planGroup},
		{ID: "max-monthly", ProviderID: cfg.CreemMaxMonthly, Name: "Max, monthly", Group: planGroup},
		{ID: "max-yearly", ProviderID: cfg.CreemMaxYearly, Name: "Max, yearly", Group: planGroup},
	}
}

// changeAtPeriodEnd makes a change from a yearly plan to a monthly one wait for the end
// of the year paid for, rather than refund most of it. Every other change happens at
// once.
func changeAtPeriodEnd(from, to subscriptions.Product) bool {
	return yearly(from.ID) && !yearly(to.ID)
}

// planOf returns the plan a product belongs to.
func planOf(productID string) (plan, bool) {
	for _, p := range plans {
		if productID != "" && (p.Monthly == productID || p.Yearly == productID) {
			return p, true
		}
	}
	return plan{}, false
}

// yearly reports whether a product is a plan's yearly one.
func yearly(productID string) bool {
	p, ok := planOf(productID)
	return ok && p.Yearly == productID
}

// gives is what a product's plan gives, in a sentence.
func gives(productID string) string {
	p, _ := planOf(productID)
	return p.Gives
}

// period is a product's billing period, in words: "year" or "month".
func period(productID string) string {
	if yearly(productID) {
		return "year"
	}
	return "month"
}

// price is what a product costs, for people: "€5 a month".
func price(productID string) string {
	p, ok := planOf(productID)
	switch {
	case !ok:
		return ""
	case p.Yearly == productID:
		return p.PerYear + " a year"
	}
	return p.PerMonth + " a month"
}

// inEffect reports whether a plan gives what it gives: paid for, whether it renews or
// ends at the period's end, and while the provider retries a failed payment. One that
// ends counts only until its period does, in case the webhook that ends it never
// arrives.
func inEffect(s subscriptions.Subscription, now time.Time) bool {
	switch s.Status {
	case payments.SubscriptionActive, payments.SubscriptionTrialing, payments.SubscriptionPastDue:
		return true
	case payments.SubscriptionCanceling:
		return s.PeriodEnd.After(now)
	}
	return false
}

// planView is what the overview says about the account's plan.
type planView struct {
	// Current is the account's plan, or nil without one.
	Current *subscriptions.Subscription
	// Changes are the products the current plan can change to: none once it ends.
	Changes []subscriptions.Product
}

// newPlanView finds the account's plan in its subscriptions, newest first: the newest
// that hasn't ended. Only one can charge again, since the module keeps one live
// subscription per group.
func newPlanView(list []subscriptions.Subscription, products []subscriptions.Product) planView {
	var v planView
	for _, s := range list {
		if s.Product.Group == planGroup && s.Status != payments.SubscriptionCanceled {
			v.Current = &s
			break
		}
	}
	if v.Current != nil && v.Current.Live() {
		for _, p := range products {
			if p.Group == planGroup && p.ID != v.Current.Product.ID {
				v.Changes = append(v.Changes, p)
			}
		}
	}
	return v
}

// status is where the account's plan stands.
func (v planView) status() string {
	s := v.Current
	if s.Pending != nil && s.Live() {
		return "Changes to " + s.Pending.Product.Name + " on " + day(s.Pending.At) + ", when the " + period(s.Product.ID) + " paid for ends."
	}
	switch s.Status {
	case payments.SubscriptionActive, payments.SubscriptionTrialing:
		if !s.PeriodEnd.IsZero() {
			return "Renews on " + day(s.PeriodEnd) + "."
		}
		return "Active."
	case payments.SubscriptionPastDue:
		return "A payment failed, and Creem will try again."
	case payments.SubscriptionUnpaid:
		return "A payment failed."
	case payments.SubscriptionPaused:
		return "Paused."
	case payments.SubscriptionCanceling:
		if !s.PeriodEnd.IsZero() {
			return "Canceled: it ends on " + day(s.PeriodEnd) + "."
		}
		return "Canceled: it ends with the period paid for."
	}
	return "Starting."
}

// day is a date as people read it, in UTC as Creem's periods are.
func day(t time.Time) string { return t.UTC().Format("2 January 2006") }

// money is an amount in cents, as people read it: "€6.25".
func money(cents int64, currency string) string {
	amount := fmt.Sprintf("%d.%02d", cents/100, cents%100)
	if currency == "" || currency == "EUR" {
		return "€" + amount
	}
	return amount + " " + currency
}

// changeView is the page that asks before a change: what it does, and what it costs.
type changeView struct {
	// From is the account's plan, and Quote what changing it would do.
	From  subscriptions.Subscription
	Quote subscriptions.Quote
}

// waits reports whether the change waits for the end of the period paid for.
func (v changeView) waits() bool { return !v.Quote.At.IsZero() }

// vat says whether the amounts include VAT.
func (v changeView) vat() string {
	if v.Quote.TaxIncluded {
		return "VAT included"
	}
	return "plus VAT"
}

// renewal is what the new plan costs each period: "€50.00 a year, plus VAT".
func (v changeView) renewal() string {
	if !v.Quote.Priced {
		return price(v.Quote.Product.ID) + ", plus VAT"
	}
	return money(v.Quote.Renewal, v.Quote.Currency) + " a " + period(v.Quote.Product.ID) + ", " + v.vat()
}

// summary is what the change does, in a sentence or two.
func (v changeView) summary() []string {
	q := v.Quote
	from, to := v.From.Product.Name, q.Product.Name
	switch {
	case v.waits():
		return []string{
			"Nothing changes now: " + from + " stays until " + day(q.At) + ", the end of the " + period(v.From.Product.ID) + " paid for.",
			"Then " + to + " starts, at " + v.renewal() + ".",
		}
	case !q.Priced:
		return []string{to + " is " + price(q.Product.ID) + ", plus VAT. The difference for the rest of this period is charged, or refunded, at once."}
	case q.NewPeriod:
		out := []string{"A new " + period(q.Product.ID) + " of " + to + " starts today: " + money(q.Charge, q.Currency) + " now, " + v.vat() + ", renewing on " + day(q.RenewsAt) + "."}
		if q.Refund > 0 {
			out = append(out, "What's left of the "+period(v.From.Product.ID)+" of "+from+", about "+money(q.Refund, q.Currency)+", goes back to the card.")
		}
		return out
	case q.Charge > 0:
		return []string{
			money(q.Charge, q.Currency) + " now, " + v.vat() + ": the difference for the rest of this period, to " + day(q.RenewsAt) + ".",
			"Then it renews at " + v.renewal() + ".",
		}
	case q.Refund > 0:
		return []string{
			money(q.Refund, q.Currency) + " goes back to the card now, " + v.vat() + ": the difference for the rest of this period, to " + day(q.RenewsAt) + ".",
			"Then it renews at " + v.renewal() + ".",
		}
	}
	return []string{"Nothing to pay now. It renews on " + day(q.RenewsAt) + " at " + v.renewal() + "."}
}

// action is the button's words: they say when the change costs money at once.
func (v changeView) action() string {
	switch {
	case v.waits():
		return "Change on " + day(v.Quote.At)
	case v.Quote.Priced && v.Quote.Charge > 0:
		return "Pay " + money(v.Quote.Charge, v.Quote.Currency) + " and change"
	}
	return "Change plan"
}
