// Package testdb creates disposable databases for integration tests.
//
// TEST_DATABASE_URL must point at a PostgreSQL server used only for tests, such as the
// one task test:integration starts. Each call creates a fresh database on that server
// and drops it when the test ends; the database named in the URL is never written to.
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	adminOnce sync.Once
	admin     *sql.DB
	adminErr  error
)

// New creates an empty database and returns its URL.
func New(t testing.TB) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Fatal("TEST_DATABASE_URL is not set; run integration tests with task test:integration")
	}
	adminOnce.Do(func() {
		admin, adminErr = sql.Open("pgx", base)
	})
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	name := "test_" + hex.EncodeToString(raw)
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}
