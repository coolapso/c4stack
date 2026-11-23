//go:build integration

package subscriptions

import (
	"context"
	"strings"
	"testing"
)

// TestCurrent: every account's subscriptions that haven't ended, by account, the newest
// first: those that would charge again, and canceled ones until their period ends; not
// ended ones, checkouts that never finished, nor a deleted account's.
func TestCurrent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	refs := map[string]string{}
	for _, who := range []string{"ann", "bob", "cid", "dee", "eve", "fay", "gus"} {
		f.account(t, who)
		refs[who] = f.checkout(t, who, "plus-monthly", 0, "")
		if who != "eve" {
			f.start(t, refs[who], "sub_"+who, "prov_plus_m", 0)
		}
	}
	// Ann also has more vaults, newer than her plan.
	vaults := f.checkout(t, "ann", "vaults", 3, "")
	f.start(t, vaults, "sub_ann_vaults", "prov_vaults", 3)
	if _, err := f.a.DB().ExecContext(ctx, `UPDATE subscriptions SET created_at = created_at + interval '1 minute' WHERE id = $1`, vaults); err != nil {
		t.Fatal(err)
	}
	for who, set := range map[string]string{
		"bob": `status = 'canceling', period_end = now() + interval '3 days'`,
		"cid": `status = 'canceling', period_end = now() - interval '1 hour'`,
		"dee": `status = 'canceled'`,
		"fay": `account_id = NULL`,
		"gus": `status = 'past_due'`,
	} {
		if _, err := f.a.DB().ExecContext(ctx, `UPDATE subscriptions SET `+set+` WHERE id = $1`, refs[who]); err != nil {
			t.Fatal(err)
		}
	}

	got, err := f.subs.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner := map[string]string{}
	for who, ref := range refs {
		owner[ref] = who
	}
	owner[vaults] = "ann's vaults"
	var names []string
	for i, s := range got {
		names = append(names, owner[s.ID]+" "+string(s.Status))
		if s.AccountID == "" || s.Product.ID == "" || (i > 0 && got[i-1].AccountID > s.AccountID) {
			t.Fatalf("subscription %d: %+v", i, s)
		}
	}
	want := map[string]bool{"ann's vaults active": true, "ann active": true, "bob canceling": true, "gus past_due": true}
	if len(got) != len(want) {
		t.Fatalf("current: %v", names)
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("current: %v", names)
		}
	}
	// Ann's, together, the newest first.
	joined := strings.Join(names, ", ")
	if !strings.Contains(joined, "ann's vaults active, ann active") {
		t.Fatalf("ann's not together, the newest first: %v", names)
	}
}
