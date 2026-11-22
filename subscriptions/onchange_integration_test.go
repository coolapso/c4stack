//go:build integration

package subscriptions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/coolapso/c4stack/payments"
)

// The hooks see each state the module writes, in the transaction that writes it: a
// checkout that completes, a change, a cancellation, also when the provider can't be
// read back, and a webhook. A checkout that hasn't completed runs none, and a hook's
// error rolls the write back, so the provider sends the event again.
func TestOnChange(t *testing.T) {
	f := newFixture(t)
	var seen []string
	var fail error
	f.subs.OnChange(func(ctx context.Context, tx *sql.Tx, sub Subscription) error {
		if fail != nil {
			return fail
		}
		var written string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM subscriptions WHERE id = $1`, sub.ID).Scan(&written); err != nil {
			return err
		}
		seen = append(seen, sub.AccountID+" "+sub.Product.ID+" "+string(sub.Status)+" "+written)
		return nil
	})
	want := func(step string, lines ...string) {
		t.Helper()
		if len(seen) != len(lines) {
			t.Fatalf("%s: hooks saw %q, want %q", step, seen, lines)
		}
		for i := range lines {
			if seen[i] != lines[i] {
				t.Fatalf("%s: hooks saw %q, want %q", step, seen, lines)
			}
		}
		seen = nil
	}

	ann := f.account(t, "ann")
	plan := f.checkout(t, "ann", "plus-monthly", 0, "")
	want("a checkout")
	f.start(t, plan, "sub_plan", "prov_plus_m", 1)
	want("its completion", ann+" plus-monthly active active")

	if rec := f.do(http.MethodPost, "/account/subscriptions/"+plan+"/change", "ann", url.Values{"product": {"family-monthly"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("change: %d", rec.Code)
	}
	want("a change", ann+" family-monthly active active")

	// A hook's error leaves the state as it was, and the webhook asks for the event again.
	fail = errors.New("busy")
	event := payments.SubscriptionEvent{ID: "evt_due", Kind: payments.SubscriptionChanged, CreatedAt: time.Now(),
		State: payments.SubscriptionState{ID: "sub_plan", Status: payments.SubscriptionPastDue}}
	body, _ := json.Marshal(event)
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/webhooks/subscriptions", bytes.NewReader(body))
	req.Header.Set("X-Signature", "valid")
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	if status, _, _ := f.row(t, plan); rec.Code != http.StatusServiceUnavailable || status != "active" {
		t.Fatalf("a hook's error: %d, status %s", rec.Code, status)
	}
	fail = nil
	f.send(t, event)
	want("the event again", ann+" family-monthly past_due past_due")

	// A cancellation the provider can't confirm still runs them, with its fallback.
	f.pay.mu.Lock()
	f.pay.fail = errors.New("down")
	f.pay.mu.Unlock()
	if err := f.subs.Cancel(context.Background(), ann, plan); err != nil {
		t.Fatal(err)
	}
	want("a cancellation", ann+" family-monthly canceling canceling")
}
