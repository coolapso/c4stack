//go:build integration

package accounts

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/coolapso/c4stack/identity"
)

// createWithoutIdentity creates an account without an identity in a transaction of its
// own.
func (f *fixture) createWithoutIdentity(t *testing.T) Account {
	t.Helper()
	ctx := context.Background()
	tx, err := f.a.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	a, err := f.accounts.CreateWithoutIdentity(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *fixture) deleteWithoutIdentity(id string) error {
	ctx := context.Background()
	tx, err := f.a.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := f.accounts.DeleteWithoutIdentity(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

func TestAnAccountWithoutAnIdentity(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.createWithoutIdentity(t)
	if a.ID == "" || a.Email != "" || a.CreatedAt.IsZero() {
		t.Fatalf("created %+v", a)
	}

	got, err := f.accounts.Get(ctx, a.ID)
	if err != nil || got.Email != "" || got.ID != a.ID {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := f.accounts.ByEmail(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ByEmail of no address = %v", err)
	}
	signedIn, err := f.accounts.resolve(ctx, identity.Identity{Subject: "ann", Email: "ann@example.com", EmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	if signedIn.ID == a.ID {
		t.Fatal("a sign-in joined the account without an identity")
	}
	page, err := f.accounts.List(ctx, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ids := func(p AccountPage) []string {
		var out []string
		for _, x := range p.Accounts {
			out = append(out, x.ID)
		}
		return out
	}
	if !slices.Contains(ids(page), a.ID) || !slices.Contains(ids(page), signedIn.ID) {
		t.Fatalf("List = %v", ids(page))
	}
	if page, err = f.accounts.List(ctx, ListOptions{Search: "ann"}); err != nil || !slices.Equal(ids(page), []string{signedIn.ID}) {
		t.Fatalf("searching lists %v, %v", ids(page), err)
	}
	if n, err := f.accounts.Count(ctx); err != nil || n != 2 {
		t.Fatalf("Count = %d, %v", n, err)
	}
	// A second one: accounts without an address don't collide.
	f.createWithoutIdentity(t)
}

func TestDeleteWithoutIdentity(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.createWithoutIdentity(t)
	if err := f.deleteWithoutIdentity(a.ID); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.hooked, []string{a.ID}) {
		t.Fatalf("OnDelete hooks ran for %v", f.hooked)
	}
	if _, err := f.accounts.Get(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after deletion = %v", err)
	}
	if err := f.deleteWithoutIdentity(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting again = %v", err)
	}
	if err := f.deleteWithoutIdentity("not-an-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a malformed ID = %v", err)
	}

	// An account someone signs in to is only theirs to delete.
	signedIn, err := f.accounts.resolve(ctx, identity.Identity{Subject: "ann", Email: "ann@example.com", EmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.deleteWithoutIdentity(signedIn.ID); !errors.Is(err, ErrHasIdentity) {
		t.Fatalf("deleting a signed-in account = %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = $1`, signedIn.ID); n != 1 {
		t.Fatal("the signed-in account was deleted")
	}

	// A hook's failure rolls the deletion back.
	b := f.createWithoutIdentity(t)
	f.hookErr = errors.New("hook failed")
	if err := f.deleteWithoutIdentity(b.ID); err == nil {
		t.Fatal("a failing hook did not fail the deletion")
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE id = $1`, b.ID); n != 1 {
		t.Fatal("a failed deletion deleted the account")
	}
}
