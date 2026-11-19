package subscriptions

import (
	"strings"
	"testing"
	"time"

	"github.com/coolapso/c4stack/payments"
)

func TestDescribe(t *testing.T) {
	end := time.Date(2027, 1, 2, 10, 0, 0, 0, time.UTC)
	for status, want := range map[payments.SubscriptionStatus]string{
		payments.SubscriptionActive:    "Renews on 2 January 2027",
		payments.SubscriptionTrialing:  "Trial, renews on 2 January 2027",
		payments.SubscriptionPastDue:   "A payment failed; it will be tried again",
		payments.SubscriptionUnpaid:    "A payment failed",
		payments.SubscriptionPaused:    "Paused",
		payments.SubscriptionCanceling: "Ends on 2 January 2027",
		payments.SubscriptionCanceled:  "Ended",
	} {
		if got := describe(Subscription{Status: status, PeriodEnd: end}); got != want {
			t.Errorf("%s: %q, want %q", status, got, want)
		}
	}
	if got := describe(Subscription{Status: payments.SubscriptionActive}); got != "Active" {
		t.Errorf("active without a period end: %q", got)
	}
}

func TestLabel(t *testing.T) {
	if got := label(Subscription{Product: Product{Name: "Plus", MaxUnits: 1}, Units: 1}); got != "Plus" {
		t.Errorf("one unit: %q", got)
	}
	if got := label(Subscription{Product: Product{Name: "More vaults", MaxUnits: 5}, Units: 3}); got != "More vaults × 3" {
		t.Errorf("packs: %q", got)
	}
}

func TestValidTarget(t *testing.T) {
	for _, ok := range []string{"", "vault-1", strings.Repeat("é", 128)} {
		if !validTarget(ok) {
			t.Errorf("refused %q", ok)
		}
	}
	for _, bad := range []string{strings.Repeat("v", 129), "a\x00b", "a\u202eb", "\xff"} {
		if validTarget(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestLocalPath(t *testing.T) {
	for p, want := range map[string]bool{"/account/settings": true, "//evil.example": false, "/\\evil.example": false, "https://x": false, "/a\nb": false, "": false} {
		if localPath(p) != want {
			t.Errorf("localPath(%q) = %v", p, !want)
		}
	}
}

func TestLive(t *testing.T) {
	for status, want := range map[payments.SubscriptionStatus]bool{
		payments.SubscriptionActive: true, payments.SubscriptionTrialing: true, payments.SubscriptionPastDue: true,
		payments.SubscriptionUnpaid: true, payments.SubscriptionPaused: true,
		payments.SubscriptionCanceling: false, payments.SubscriptionCanceled: false, "pending": false,
	} {
		if (Subscription{Status: status}).Live() != want {
			t.Errorf("%s: live %v", status, !want)
		}
		// liveStatuses, the SQL, must agree.
		if strings.Contains(liveStatuses, "'"+string(status)+"'") != want {
			t.Errorf("%s: liveStatuses disagrees", status)
		}
	}
}
