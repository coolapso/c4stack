//go:build integration

package credits

import (
	"context"
	"errors"
	"testing"

	"github.com/coolapso/c4stack/payments"
)

// inAccount runs fn with the account's credit locked in a transaction it commits when fn
// succeeds.
func (f *fixture) inAccount(t *testing.T, accountID string, fn func(tx *Tx) error) error {
	t.Helper()
	ctx := context.Background()
	sqlTx, err := f.a.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlTx.Rollback()
	tx, err := f.credits.Lock(ctx, sqlTx, accountID)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return sqlTx.Commit()
}

func (f *fixture) creditOrder(t *testing.T, accountID string, o Order) (string, bool, error) {
	t.Helper()
	var id string
	var credited bool
	err := f.inAccount(t, accountID, func(tx *Tx) error {
		var err error
		id, credited, err = tx.CreditOrder(o)
		return err
	})
	return id, credited, err
}

func (f *fixture) reverseOrder(t *testing.T, accountID, orderID, eventID string, disputed bool) (bool, error) {
	t.Helper()
	var reversed bool
	err := f.inAccount(t, accountID, func(tx *Tx) error {
		var err error
		reversed, err = tx.ReverseOrder("test", orderID, eventID, disputed)
		return err
	})
	return reversed, err
}

func marketplaceOrder(orderID string) Order {
	return Order{
		Provider: "test", OrderID: orderID, EventID: "evt_" + orderID,
		CreditCents: 1000, PriceCents: 1083, PaidCents: 1083,
		Description: "Credit bought on the marketplace",
	}
}

func TestCreditOrder(t *testing.T) {
	f := newFixture(t)
	ada := f.account(t, "ada")
	grace := f.account(t, "grace")

	id, credited, err := f.creditOrder(t, ada, marketplaceOrder("ord_1"))
	if err != nil || !credited || id == "" {
		t.Fatalf("credit: %q %v %v", id, credited, err)
	}
	if got := f.balance(t, ada); got != 1000 {
		t.Fatalf("balance %d", got)
	}
	var status string
	var fee, paid int64
	if err := f.a.DB().QueryRow(`
		SELECT provider_status, fee_amount_cents, paid_amount_cents
		FROM payment_intents
		WHERE id = $1 AND account_id = $2`, id, ada,
	).Scan(&status, &fee, &paid); err != nil || status != "paid" || fee != 83 || paid != 1083 {
		t.Fatalf("purchase: %s %d %d %v", status, fee, paid, err)
	}

	// The same order again changes nothing; another account's or other amounts conflict.
	again := marketplaceOrder("ord_1")
	again.EventID = "evt_again"
	if id2, credited, err := f.creditOrder(t, ada, again); err != nil || credited || id2 != id {
		t.Fatalf("repeat: %q %v %v", id2, credited, err)
	}
	if got := f.balance(t, ada); got != 1000 {
		t.Fatalf("balance after a repeat %d", got)
	}
	if _, _, err := f.creditOrder(t, grace, again); !errors.Is(err, ErrConflict) {
		t.Fatalf("another account's order: %v", err)
	}
	bigger := marketplaceOrder("ord_1")
	bigger.CreditCents = 2000
	bigger.PriceCents, bigger.PaidCents = 2100, 2100
	if _, _, err := f.creditOrder(t, ada, bigger); !errors.Is(err, ErrConflict) {
		t.Fatalf("other amounts: %v", err)
	}

	short := marketplaceOrder("ord_2")
	short.PaidCents = 1000
	if _, _, err := f.creditOrder(t, ada, short); !errors.Is(err, ErrInvalid) {
		t.Fatalf("paid less than the price: %v", err)
	}
	cheap := marketplaceOrder("ord_2")
	cheap.PriceCents, cheap.PaidCents = 900, 900
	if _, _, err := f.creditOrder(t, ada, cheap); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a price below the credit: %v", err)
	}
	if err := f.inAccount(t, "00000000-0000-0000-0000-000000000000", func(*Tx) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("locking an unknown account: %v", err)
	}
	if err := f.inAccount(t, "not-an-id", func(*Tx) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("locking a malformed ID: %v", err)
	}
}

func TestReverseOrder(t *testing.T) {
	f := newFixture(t)
	ada := f.account(t, "ada")
	for _, o := range []string{"ord_1", "ord_2", "ord_3"} {
		if _, _, err := f.creditOrder(t, ada, marketplaceOrder(o)); err != nil {
			t.Fatal(err)
		}
	}

	if reversed, err := f.reverseOrder(t, ada, "ord_1", "evt_refund_1", false); err != nil || !reversed {
		t.Fatalf("refund: %v %v", reversed, err)
	}
	if reversed, err := f.reverseOrder(t, ada, "ord_1", "evt_refund_1b", false); err != nil || reversed {
		t.Fatalf("a second refund: %v %v", reversed, err)
	}
	if reversed, err := f.reverseOrder(t, ada, "ord_2", "evt_dispute_2", true); err != nil || !reversed {
		t.Fatalf("dispute: %v %v", reversed, err)
	}
	if got := f.balance(t, ada); got != 1000 {
		t.Fatalf("balance %d", got)
	}
	var kinds []string
	rows, err := f.a.DB().Query(`
		SELECT l.kind || ':' || i.provider_status
		FROM credit_ledger l
		JOIN payment_intents i ON i.id = l.payment_intent_id
		WHERE l.kind IN ('refund', 'chargeback')
		ORDER BY i.provider_order_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 2 || kinds[0] != "refund:refunded" || kinds[1] != "chargeback:charged_back" {
		t.Fatalf("reversals %v", kinds)
	}

	// Whichever comes first reverses: the payment webhook, then the module.
	if code := f.webhook(payments.Event{ID: "evt_webhook_3", Kind: payments.Refunded, OrderID: "ord_3",
		AmountCents: 1083, PaidCents: 1083, Currency: "EUR"}, "valid"); code != 200 {
		t.Fatalf("webhook: %d", code)
	}
	if reversed, err := f.reverseOrder(t, ada, "ord_3", "evt_module_3", false); err != nil || reversed {
		t.Fatalf("after the webhook: %v %v", reversed, err)
	}
	if got := f.balance(t, ada); got != 0 {
		t.Fatalf("balance %d", got)
	}
	if _, err := f.reverseOrder(t, ada, "ord_unknown", "evt_x", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown order: %v", err)
	}
	grace := f.account(t, "grace")
	if _, err := f.reverseOrder(t, grace, "ord_3", "evt_y", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account's order: %v", err)
	}
}

func TestTransfer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada := f.account(t, "ada")

	// An account without an identity buys credit and spends some of it.
	tx, err := f.a.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := f.accounts.CreateWithoutIdentity(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.creditOrder(t, agent.ID, marketplaceOrder("ord_1")); err != nil {
		t.Fatal(err)
	}
	if err := f.credits.Debit(ctx, agent.ID, 300, "search-1", "Search"); err != nil {
		t.Fatal(err)
	}

	transfer := func(from, to, reference string) (int64, error) {
		var moved int64
		tx, err := f.a.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if moved, err = f.credits.Transfer(ctx, tx, from, to, reference, "Credit from your marketplace account"); err != nil {
			return 0, err
		}
		return moved, tx.Commit()
	}

	// Credit held for work in progress doesn't move.
	if _, err := f.a.DB().Exec(`INSERT INTO jobs (id, account_id, reserved_cents) VALUES ('job', $1, 50)`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := transfer(agent.ID, ada, "claim:1"); !errors.Is(err, ErrHeld) {
		t.Fatalf("with a hold: %v", err)
	}
	if _, err := f.a.DB().Exec(`UPDATE jobs SET done = true`); err != nil {
		t.Fatal(err)
	}

	if moved, err := transfer(agent.ID, ada, "claim:1"); err != nil || moved != 700 {
		t.Fatalf("moved %d: %v", moved, err)
	}
	if a, b := f.balance(t, agent.ID), f.balance(t, ada); a != 0 || b != 700 {
		t.Fatalf("balances %d %d", a, b)
	}
	if moved, err := transfer(agent.ID, ada, "claim:1"); err != nil || moved != 700 {
		t.Fatalf("repeating: %d %v", moved, err)
	}
	if a, b := f.balance(t, agent.ID), f.balance(t, ada); a != 0 || b != 700 {
		t.Fatalf("balances after repeating %d %d", a, b)
	}
	entries, _, err := f.credits.History(ctx, ada, 1, 10)
	if err != nil || len(entries) != 1 || entries[0].Kind != KindTransfer || Label(Entry{Kind: KindTransfer}) != "Credit moved" {
		t.Fatalf("history %+v %v", entries, err)
	}

	// The purchase moved too, so its refund takes the credit from the new account.
	if reversed, err := f.reverseOrder(t, ada, "ord_1", "evt_refund", false); err != nil || !reversed {
		t.Fatalf("refund after the transfer: %v %v", reversed, err)
	}
	if got := f.balance(t, ada); got != -300 {
		t.Fatalf("balance after the refund %d", got)
	}

	if _, err := transfer(ada, ada, "claim:2"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("to itself: %v", err)
	}
	if _, err := transfer(agent.ID, "00000000-0000-0000-0000-000000000000", "claim:3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("to an unknown account: %v", err)
	}
	if _, err := transfer("not-an-id", ada, "claim:4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("from a malformed ID: %v", err)
	}
	// A negative balance moves too.
	if moved, err := transfer(ada, agent.ID, "back:1"); err != nil || moved != -300 {
		t.Fatalf("moving a debt: %d %v", moved, err)
	}
}

func TestReview(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for range 2 {
		if err := f.credits.Review(ctx, "test", "evt_1", "marketplace_amount"); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.reviews(t); len(got) != 1 || got[0] != "marketplace_amount" {
		t.Fatalf("reviews %v", got)
	}
	if err := f.credits.Review(ctx, "", "evt_2", "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("without a provider: %v", err)
	}
}
