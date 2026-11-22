//go:build integration

package subscriptions

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coolapso/c4stack/payments"
)

// yearlyToMonthly makes a change from a yearly product to a monthly one wait for the
// end of the year.
func yearlyToMonthly(cfg *Config) {
	cfg.ChangeAtPeriodEnd = func(from, to Product) bool {
		return strings.HasSuffix(from.ID, "-yearly") && strings.HasSuffix(to.ID, "-monthly")
	}
}

// periodEnds sets when a subscription's period ends, at the provider and in the module.
func (f *fixture) periodEnds(t *testing.T, providerID, id string, end time.Time) {
	t.Helper()
	f.pay.mu.Lock()
	st := f.pay.states[providerID]
	st.PeriodEnd = end
	f.pay.states[providerID] = st
	f.pay.mu.Unlock()
	if _, err := f.a.DB().Exec(`UPDATE subscriptions SET period_end = $2 WHERE id = $1`, id, end); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) change(t *testing.T, subject, id, product string) {
	t.Helper()
	if rec := f.do(http.MethodPost, "/account/subscriptions/"+id+"/change", subject, url.Values{"product": {product}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("change to %s: %d\n%s", product, rec.Code, rec.Body.String())
	}
}

func TestChangeAtPeriodEnd(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, yearlyToMonthly)
	ann := f.account(t, "ann")
	plan := f.checkout(t, "ann", "plus-yearly", 0, "")
	f.start(t, plan, "sub_y", "prov_plus_y", 1)
	end := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	f.periodEnds(t, "sub_y", plan, end)

	// The quote says when, and is the provider's for then.
	q, err := f.subs.QuoteChange(ctx, ann, plan, ChangeRequest{Product: "plus-monthly"})
	if err != nil || !q.At.Equal(end) || !q.Priced || q.Product.ID != "plus-monthly" || q.Units != 1 || q.Charge != 100 {
		t.Fatalf("quote %+v, %v", q, err)
	}
	if got := f.pay.quotes[len(f.pay.quotes)-1]; got.id != "sub_y" || got.change.ProductID != "prov_plus_m" || !got.at.Equal(end) {
		t.Fatalf("quoted %+v", got)
	}
	// One that doesn't wait is quoted for now.
	if q, err := f.subs.QuoteChange(ctx, ann, plan, ChangeRequest{Product: "team", Units: 3}); err != nil || !q.At.IsZero() || q.Units != 3 {
		t.Fatalf("quote at once %+v, %v", q, err)
	}
	if got := f.pay.quotes[len(f.pay.quotes)-1]; !got.at.IsZero() || got.change.ProductID != "prov_team" || got.change.Units != 3 {
		t.Fatalf("quoted %+v", got)
	}

	// Choosing it changes nothing at the provider yet.
	f.change(t, "ann", plan, "plus-monthly")
	sub, err := f.subs.Get(ctx, ann, plan)
	if err != nil || sub.Product.ID != "plus-yearly" || sub.Pending == nil || sub.Pending.Product.ID != "plus-monthly" || !sub.Pending.At.Equal(end) {
		t.Fatalf("after choosing: %+v %+v, %v", sub, sub.Pending, err)
	}
	if len(f.pay.changes) != 0 {
		t.Fatalf("changed at once: %+v", f.pay.changes)
	}
	if page := f.do(http.MethodGet, "/account/settings", "ann", nil).Body.String(); !strings.Contains(page, "Changes to Plus, monthly on "+end.Format("2 January 2006")) {
		t.Fatalf("settings panel:\n%s", page)
	}
	// A month before the period ends, it isn't due.
	if err := f.subs.applyDue(ctx); err != nil || len(f.pay.changes) != 0 {
		t.Fatalf("applied early: %+v, %v", f.pay.changes, err)
	}
	// Keeping what it has drops it.
	f.change(t, "ann", plan, "plus-yearly")
	if sub, _ := f.subs.Get(ctx, ann, plan); sub.Pending != nil || len(f.pay.changes) != 0 {
		t.Fatalf("after keeping: %+v, changes %+v", sub.Pending, f.pay.changes)
	}

	// Less than an hour before the period ends, the module makes it. A new choice
	// replaced the first.
	end = time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	f.periodEnds(t, "sub_y", plan, end)
	f.change(t, "ann", plan, "plus-monthly")
	f.change(t, "ann", plan, "family-monthly")
	if n := f.count(t, `SELECT count(*) FROM subscription_changes WHERE applied_at IS NULL AND canceled_at IS NULL`); n != 1 {
		t.Fatalf("%d pending changes", n)
	}
	if err := f.subs.applyDue(ctx); err != nil {
		t.Fatal(err)
	}
	if c := f.pay.changes; len(c) != 1 || c[0].ProductID != "prov_family_m" || c[0].Proration != payments.ProrateNow {
		t.Fatalf("changes %+v", c)
	}
	if sub, _ := f.subs.Get(ctx, ann, plan); sub.Product.ID != "family-monthly" || sub.Pending != nil {
		t.Fatalf("after the change: %s %+v", sub.Product.ID, sub.Pending)
	}
	// Done once.
	if err := f.subs.applyDue(ctx); err != nil || len(f.pay.changes) != 1 {
		t.Fatalf("applied again: %+v, %v", f.pay.changes, err)
	}

	// The refund of the time the change left, and any later one, are only logged.
	f.send(t, payments.SubscriptionEvent{ID: "evt_refund_change", Kind: payments.SubscriptionRefunded,
		State: payments.SubscriptionState{ID: "sub_y"}, CreatedAt: time.Now()})
	f.send(t, payments.SubscriptionEvent{ID: "evt_refund_later", Kind: payments.SubscriptionRefunded,
		State: payments.SubscriptionState{ID: "sub_y"}, CreatedAt: time.Now().Add(24 * time.Hour)})
	if r := f.reviews(t); len(r) != 0 {
		t.Fatalf("reviews %v", r)
	}
}

func TestChangesThatWaitAreDropped(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, yearlyToMonthly)
	ann := f.account(t, "ann")
	soon := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)

	// Canceling drops the change.
	plan := f.checkout(t, "ann", "plus-yearly", 0, "")
	f.start(t, plan, "sub_1", "prov_plus_y", 1)
	f.periodEnds(t, "sub_1", plan, soon)
	f.change(t, "ann", plan, "plus-monthly")
	if rec := f.do(http.MethodPost, "/account/subscriptions/"+plan+"/cancel", "ann", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("cancel: %d", rec.Code)
	}
	if sub, _ := f.subs.Get(ctx, ann, plan); sub.Status != payments.SubscriptionCanceling || sub.Pending != nil {
		t.Fatalf("after canceling: %s %+v", sub.Status, sub.Pending)
	}
	// A canceling subscription can't be quoted a change.
	if _, err := f.subs.QuoteChange(ctx, ann, plan, ChangeRequest{Product: "plus-monthly"}); !errors.Is(err, ErrNotLive) {
		t.Fatalf("quote for a canceling subscription: %v", err)
	}
	f.send(t, payments.SubscriptionEvent{ID: "evt_end_1", Kind: payments.SubscriptionChanged,
		State: payments.SubscriptionState{ID: "sub_1", Status: payments.SubscriptionCanceled}})

	// A subscription that changed at the provider meanwhile drops it.
	plan = f.checkout(t, "ann", "plus-yearly", 0, "")
	f.start(t, plan, "sub_2", "prov_plus_y", 1)
	f.periodEnds(t, "sub_2", plan, soon)
	f.change(t, "ann", plan, "plus-monthly")
	f.pay.mu.Lock()
	st := f.pay.states["sub_2"]
	st.ProductID = "prov_team"
	f.pay.states["sub_2"] = st
	f.pay.mu.Unlock()
	f.send(t, payments.SubscriptionEvent{ID: "evt_team", Kind: payments.SubscriptionChanged,
		State: payments.SubscriptionState{ID: "sub_2", ProductID: "prov_team", Status: payments.SubscriptionActive}})
	if err := f.subs.applyDue(ctx); err != nil {
		t.Fatal(err)
	}
	if sub, _ := f.subs.Get(ctx, ann, plan); sub.Product.ID != "team" || sub.Pending != nil || len(f.pay.changes) != 0 {
		t.Fatalf("after a change elsewhere: %s %+v, changes %+v", sub.Product.ID, sub.Pending, f.pay.changes)
	}
	// So does one that ended.
	f.change(t, "ann", plan, "plus-yearly")
	f.change(t, "ann", plan, "plus-monthly")
	f.send(t, payments.SubscriptionEvent{ID: "evt_end_2", Kind: payments.SubscriptionChanged,
		State: payments.SubscriptionState{ID: "sub_2", Status: payments.SubscriptionCanceled}})
	if err := f.subs.applyDue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM subscription_changes WHERE applied_at IS NULL AND canceled_at IS NULL`); n != 0 {
		t.Fatalf("%d pending changes", n)
	}
	if c := f.pay.changes; len(c) != 1 || c[0].ProductID != "prov_plus_y" {
		t.Fatalf("changes %+v", c)
	}

	// Another account's subscription can't be quoted.
	f.account(t, "bob")
	if _, err := f.subs.QuoteChange(ctx, f.account(t, "bob"), plan, ChangeRequest{Product: "plus-monthly"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("quote for another account: %v", err)
	}
	// Without the period's end, a change that waits can't be recorded.
	plan = f.checkout(t, "bob", "plus-yearly", 0, "")
	f.start(t, plan, "sub_3", "prov_plus_y", 1)
	if rec := f.do(http.MethodPost, "/account/subscriptions/"+plan+"/change", "bob", url.Values{"product": {"plus-monthly"}}); rec.Code != http.StatusBadGateway {
		t.Fatalf("a change without a period end: %d", rec.Code)
	}
}
