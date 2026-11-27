//go:build integration

package apikeys

import (
	"context"
	"errors"
	"testing"
)

func TestKeysInACallersTransaction(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada := f.account(t, "ada")

	// A key created in a transaction that rolls back never existed.
	tx, err := f.a.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := f.keys.CreateTx(ctx, tx, ada, "Agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.keys.Authenticate(ctx, raw); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("a rolled-back key authenticates: %v", err)
	}

	// One that commits works, and revoking it in a transaction stops it once that
	// commits.
	if tx, err = f.a.DB().BeginTx(ctx, nil); err != nil {
		t.Fatal(err)
	}
	raw, key, err := f.keys.CreateTx(ctx, tx, ada, "Agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := f.keys.Authenticate(ctx, raw); err != nil || got.ID != key.ID || got.AccountID != ada {
		t.Fatalf("authenticate: %+v %v", got, err)
	}
	if tx, err = f.a.DB().BeginTx(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.keys.RevokeTx(ctx, tx, ada, key.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.keys.RevokeTx(ctx, tx, ada, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking an unknown key: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.keys.Authenticate(ctx, raw); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("a revoked key authenticates: %v", err)
	}
	if _, _, err := f.keys.CreateTx(ctx, nil, ada, ""); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("an empty name: %v", err)
	}
}

func TestMoveKeys(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada := f.account(t, "ada")
	grace := f.account(t, "grace")
	active, _, err := f.keys.Create(ctx, ada, "Active")
	if err != nil {
		t.Fatal(err)
	}
	_, revoked, err := f.keys.Create(ctx, ada, "Revoked")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.keys.Revoke(ctx, ada, revoked.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.keys.Create(ctx, grace, "Grace's own"); err != nil {
		t.Fatal(err)
	}

	tx, err := f.a.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := f.keys.Move(ctx, tx, ada, grace)
	if err != nil || n != 2 {
		t.Fatalf("moved %d: %v", n, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := f.keys.Authenticate(ctx, active); err != nil || got.AccountID != grace {
		t.Fatalf("a moved key authenticates as %+v: %v", got, err)
	}
	if keys, err := f.keys.List(ctx, grace); err != nil || len(keys) != 3 {
		t.Fatalf("grace has %d keys: %v", len(keys), err)
	}
	if keys, err := f.keys.List(ctx, ada); err != nil || len(keys) != 0 {
		t.Fatalf("ada kept %d keys: %v", len(keys), err)
	}
}
