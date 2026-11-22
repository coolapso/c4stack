//go:build integration

package subscriptions

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/coolapso/c4stack/payments"
)

// TestReviewsResolveSync: the open reviews name their subscription and account when
// there is one; resolving needs a note and works once; Sync applies the provider's
// state, runs the OnChange hooks, and tells subscriptions the provider doesn't know.
func TestReviewsResolveSync(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var hooked []payments.SubscriptionStatus
	f.subs.OnChange(func(_ context.Context, _ *sql.Tx, sub Subscription) error {
		hooked = append(hooked, sub.Status)
		return nil
	})
	ann := f.account(t, "ann")
	ref := f.checkout(t, "ann", "plus-monthly", 0, "")
	f.start(t, ref, "sub_1", "prov_plus_m", 0)
	f.send(t, payments.SubscriptionEvent{ID: "evt_dispute", Kind: payments.SubscriptionDisputed, State: payments.SubscriptionState{ID: "sub_1"}})
	f.send(t, payments.SubscriptionEvent{ID: "evt_stranger", Kind: payments.SubscriptionChanged,
		State: payments.SubscriptionState{ID: "sub_stranger", ProductID: "prov_plus_m", Status: payments.SubscriptionActive}})

	reviews, err := f.subs.Reviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 2 {
		t.Fatalf("%d open reviews, want 2: %+v", len(reviews), reviews)
	}
	dispute, stranger := reviews[0], reviews[1]
	if dispute.EventID != "evt_dispute" || dispute.SubscriptionID != ref || dispute.AccountID != ann || dispute.CreatedAt.IsZero() {
		t.Fatalf("the dispute's review: %+v", dispute)
	}
	if stranger.EventID != "evt_stranger" || stranger.Reason != "unknown_subscription" || stranger.SubscriptionID != "" || stranger.AccountID != "" {
		t.Fatalf("the unknown subscription's review: %+v", stranger)
	}

	if err := f.subs.Resolve(ctx, dispute.ID, "  "); err == nil {
		t.Fatal("resolved a review without a note")
	}
	if err := f.subs.Resolve(ctx, stranger.ID+100, "nothing"); !errors.Is(err, ErrNoReview) {
		t.Fatalf("resolving a review that doesn't exist: %v", err)
	}
	if err := f.subs.Resolve(ctx, dispute.ID, " answered the dispute "); err != nil {
		t.Fatal(err)
	}
	if err := f.subs.Resolve(ctx, dispute.ID, "again"); !errors.Is(err, ErrNoReview) {
		t.Fatalf("resolving a review twice: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM subscription_reviews WHERE resolution = 'answered the dispute' AND resolved_at IS NOT NULL`); n != 1 {
		t.Fatal("the resolution wasn't recorded")
	}
	if reviews, err := f.subs.Reviews(ctx); err != nil || len(reviews) != 1 || reviews[0].ID != stranger.ID {
		t.Fatalf("open reviews after resolving one: %+v, %v", reviews, err)
	}

	// Sync.
	if _, err := f.subs.Sync(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("syncing a subscription that doesn't exist: %v", err)
	}
	unstarted := f.checkout(t, "ann", "vaults", 1, "")
	if _, err := f.subs.Sync(ctx, unstarted); !errors.Is(err, ErrNotAtProvider) {
		t.Fatalf("syncing a checkout that never finished: %v", err)
	}
	f.pay.mu.Lock()
	f.pay.states["sub_1"] = payments.SubscriptionState{ID: "sub_1", ProductID: "prov_plus_m", Status: payments.SubscriptionPastDue}
	f.pay.mu.Unlock()
	hooked = nil
	synced, err := f.subs.Sync(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if synced.Status != payments.SubscriptionPastDue || synced.Review != "" {
		t.Fatalf("synced: %+v", synced)
	}
	if status, _, _ := f.row(t, ref); status != "past_due" {
		t.Fatalf("after sync: %s", status)
	}
	if len(hooked) != 1 || hooked[0] != payments.SubscriptionPastDue {
		t.Fatalf("OnChange after sync: %v", hooked)
	}
}
