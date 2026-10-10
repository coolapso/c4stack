// Package apptest gives integration tests an App on a fresh, disposable database.
//
// TEST_DATABASE_URL must point at a PostgreSQL server used only for tests, such as the
// one task test:integration starts. Each call creates a new database on that server,
// applies the migrations in cfg, and drops the database when the test ends.
package apptest

import (
	"context"
	"log/slog"
	"testing"

	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/internal/postgres"
	"github.com/coolapso/c4stack/internal/testdb"
)

// New returns an App whose database has cfg.Migrations applied. It does not run
// cfg.Setup: tests construct the modules they need, usually with test providers.
func New(t testing.TB, cfg app.Config) *app.App {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	db, err := postgres.Open(ctx, testdb.New(t), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sets := make([]postgres.Set, len(cfg.Migrations))
	for i, m := range cfg.Migrations {
		sets[i] = postgres.Set{Namespace: m.Namespace, Files: m.Files}
	}
	if err := postgres.Migrate(ctx, db, sets, logger); err != nil {
		t.Fatal(err)
	}
	a := app.New(cfg, db)
	a.SetLogger(logger)
	return a
}
