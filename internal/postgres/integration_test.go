//go:build integration

package postgres_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/coolapso/c4stack/internal/postgres"
	"github.com/coolapso/c4stack/internal/testdb"
)

var quiet = slog.New(slog.DiscardHandler)

func open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := postgres.Open(context.Background(), testdb.New(t), quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func sqlFiles(m map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range m {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func TestMigrateAppliesNamespacesInOrder(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	accounts := postgres.Set{Namespace: "accounts", Files: sqlFiles(map[string]string{
		"001_initial.sql": "CREATE TABLE accounts (id int PRIMARY KEY);\nCREATE INDEX ON accounts (id);",
	})}
	app := postgres.Set{Namespace: "app", Files: sqlFiles(map[string]string{
		"001_notes.sql": "CREATE TABLE notes (account_id int REFERENCES accounts (id) ON DELETE CASCADE, body text);",
	})}
	sets := []postgres.Set{accounts, app}

	checker, err := postgres.NewChecker(sets)
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.Check(ctx, db); !errors.Is(err, postgres.ErrSchemaNotReady) {
		t.Fatalf("check before migrate: %v", err)
	}
	if err := postgres.Migrate(ctx, db, sets, quiet); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Migrate(ctx, db, sets, quiet); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := checker.Check(ctx, db); err != nil {
		t.Fatalf("check after migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO accounts VALUES (1); INSERT INTO notes VALUES (1, 'kept')`); err != nil {
		t.Fatal(err)
	}

	// The next release adds a migration; an older binary keeps working against it.
	app.Files.(fstest.MapFS)["002_tags.sql"] = &fstest.MapFile{Data: []byte("ALTER TABLE notes ADD COLUMN tag text;")}
	next := []postgres.Set{accounts, app}
	nextChecker, _ := postgres.NewChecker(next)
	if err := nextChecker.Check(ctx, db); !errors.Is(err, postgres.ErrSchemaNotReady) {
		t.Fatalf("new binary before migrate: %v", err)
	}
	if err := postgres.Migrate(ctx, db, next, quiet); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := db.QueryRowContext(ctx, `SELECT body FROM notes WHERE tag IS NULL`).Scan(&body); err != nil || body != "kept" {
		t.Fatalf("data after upgrade: %q %v", body, err)
	}
	delete(app.Files.(fstest.MapFS), "002_tags.sql")
	oldChecker, _ := postgres.NewChecker([]postgres.Set{accounts, app})
	if err := oldChecker.Check(ctx, db); err != nil {
		t.Fatalf("old binary against newer schema: %v", err)
	}
	if err := postgres.Migrate(ctx, db, []postgres.Set{accounts, app}, quiet); err != nil {
		t.Fatalf("migrate from a rolled-back release: %v", err)
	}

	status, err := postgres.Report(ctx, db, []postgres.Set{accounts, app})
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 2 || len(status[1].Newer) != 1 || status[1].Newer[0] != 2 {
		t.Fatalf("status: %+v", status)
	}
}

func TestMigrateRefusesChangedHistory(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	files := sqlFiles(map[string]string{"001_a.sql": "CREATE TABLE a (id int);"})
	sets := []postgres.Set{{Namespace: "app", Files: files}}
	if err := postgres.Migrate(ctx, db, sets, quiet); err != nil {
		t.Fatal(err)
	}
	files["001_a.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE a (id bigint);")}
	if err := postgres.Migrate(ctx, db, sets, quiet); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed file: %v", err)
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	sets := []postgres.Set{{Namespace: "app", Files: sqlFiles(map[string]string{
		"001_a.sql": "CREATE TABLE a (id int);",
		"002_b.sql": "CREATE TABLE b (id int); SELECT broken_syntax(;",
	})}}
	if err := postgres.Migrate(ctx, db, sets, quiet); err == nil {
		t.Fatal("broken migration succeeded")
	}
	var b sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('b')::text`).Scan(&b); err != nil || b.Valid {
		t.Fatalf("table b exists after a failed migration: %v %v", b, err)
	}
	status, _ := postgres.Report(ctx, db, sets)
	if len(status[0].Applied) != 1 || len(status[0].Pending) != 1 {
		t.Fatalf("status: %+v", status)
	}
}

func TestConcurrentMigrateRuns(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	sets := []postgres.Set{{Namespace: "app", Files: sqlFiles(map[string]string{
		"001_a.sql": "CREATE TABLE a (id int);",
		"002_b.sql": "CREATE TABLE b (id int);",
	})}}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- postgres.Migrate(ctx, db, sets, quiet) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestMigrateAdoptsSingleHistory starts from a database whose app recorded its own
// migrations without namespaces, in the shape Searchbase's runner uses, and lists those
// files as one set beside a module that adopts the app's table.
func TestMigrateAdoptsSingleHistory(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	appFiles := map[string]string{
		"001_initial.sql": "CREATE TABLE IF NOT EXISTS accounts (id int PRIMARY KEY);",
		"002_notes.sql":   "CREATE TABLE notes (account_id int REFERENCES accounts (id), body text);",
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE schema_migrations (
		    version BIGINT PRIMARY KEY,
		    name TEXT NOT NULL UNIQUE,
		    checksum TEXT NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
		    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_initial.sql", "002_notes.sql"} {
		body := appFiles[name]
		if _, err := db.ExecContext(ctx, body); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			name[:3], strings.TrimSuffix(name[4:], ".sql"), hex.EncodeToString(sum[:])); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO accounts VALUES (1); INSERT INTO notes VALUES (1, 'kept')`); err != nil {
		t.Fatal(err)
	}

	legacy := postgres.Set{Namespace: "app", Files: sqlFiles(appFiles)}
	module := postgres.Set{Namespace: "accounts", Files: sqlFiles(map[string]string{
		"001_initial.sql": "CREATE TABLE IF NOT EXISTS accounts (id int PRIMARY KEY);\nALTER TABLE accounts ADD COLUMN IF NOT EXISTS email text;",
	})}
	sets := []postgres.Set{legacy, module}

	checker, _ := postgres.NewChecker(sets)
	if err := checker.Check(ctx, db); !errors.Is(err, postgres.ErrSchemaNotReady) {
		t.Fatalf("check before adoption: %v", err)
	}
	status, err := postgres.Report(ctx, db, sets)
	if err != nil {
		t.Fatal(err)
	}
	if len(status[0].Applied) != 2 || len(status[0].Pending) != 0 || len(status[1].Pending) != 1 {
		t.Fatalf("status before adoption: %+v", status)
	}

	if err := postgres.Migrate(ctx, db, []postgres.Set{module}, quiet); err == nil || !strings.Contains(err.Error(), "matches no listed migration") {
		t.Fatalf("adoption without the app's set: %v", err)
	}
	if err := postgres.Migrate(ctx, db, sets, quiet); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Migrate(ctx, db, sets, quiet); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := checker.Check(ctx, db); err != nil {
		t.Fatalf("check after adoption: %v", err)
	}
	var body string
	if err := db.QueryRowContext(ctx, `SELECT body FROM notes JOIN accounts ON accounts.id = notes.account_id WHERE accounts.email IS NULL`).Scan(&body); err != nil || body != "kept" {
		t.Fatalf("data after adoption: %q %v", body, err)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE namespace = 'app'`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("app history: %d %v", rows, err)
	}
}
