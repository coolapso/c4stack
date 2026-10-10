//go:build integration

package app

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coolapso/c4stack/internal/testdb"
	"github.com/coolapso/c4stack/web"
)

func migrations() []Migrations {
	return []Migrations{
		{Namespace: "accounts", Files: fstest.MapFS{"001_initial.sql": {Data: []byte("CREATE TABLE accounts (id int PRIMARY KEY);")}}},
		{Namespace: "app", Files: fstest.MapFS{"001_notes.sql": {Data: []byte("CREATE TABLE notes (account_id int REFERENCES accounts (id));")}}},
	}
}

func TestMigrateNeedsOnlyTheDatabase(t *testing.T) {
	cfg := Config{
		Name:        "acme",
		DatabaseURL: testdb.New(t),
		Migrations:  migrations(),
		Setup: func(context.Context, *App) error {
			t.Error("migrate ran Setup")
			return errors.New("provider configuration missing")
		},
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), cfg, []string{"migrate"}, &out, &errOut); code != 0 {
		t.Fatalf("migrate: exit %d: %s", code, errOut.String())
	}
	out.Reset()
	if code := run(context.Background(), cfg, []string{"migrate", "status"}, &out, &errOut); code != 0 {
		t.Fatalf("migrate status: exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "accounts") || !strings.Contains(out.String(), "app") {
		t.Fatalf("status output:\n%s", out.String())
	}
}

func TestOperatorCommands(t *testing.T) {
	var got []string
	cfg := Config{
		Name:        "acme",
		DatabaseURL: testdb.New(t),
		Migrations:  migrations(),
		Setup: func(ctx context.Context, a *App) error {
			a.Command(Command{Name: "accounts delete-local", Run: func(ctx context.Context, args []string) error {
				if a.DB() == nil {
					return errors.New("no database")
				}
				got = args
				return nil
			}})
			return nil
		},
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), cfg, []string{"accounts", "delete-local", "abc"}, &out, &errOut); code != 0 || len(got) != 1 || got[0] != "abc" {
		t.Fatalf("exit %d, args %v: %s", code, got, errOut.String())
	}
	if code := run(context.Background(), cfg, []string{"accounts", "purge"}, &out, &errOut); code != 2 {
		t.Fatalf("unknown command: exit %d", code)
	}
}

func TestServeReadinessFollowsSchema(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	jobRan := make(chan struct{}, 1)
	cfg := Config{
		Name:        "acme",
		DatabaseURL: testdb.New(t),
		Migrations:  migrations(),
		Setup: func(ctx context.Context, a *App) error {
			r, err := web.New(web.Config{PublicURL: "http://" + addr, SecretKey: strings.Repeat("k", 32)})
			if err != nil {
				return err
			}
			a.Every("probe", 10*time.Millisecond, func(context.Context) error {
				select {
				case jobRan <- struct{}{}:
				default:
				}
				return nil
			})
			a.Serve(web.Server{Addr: addr, Handler: r, HealthPath: "/health", DrainDelay: time.Millisecond})
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var errOut bytes.Buffer
	go func() { done <- run(ctx, cfg, []string{"serve"}, &bytes.Buffer{}, &errOut) }()

	ready := func() int {
		resp, err := http.Get("http://" + addr + "/health/ready")
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	waitUntil(t, func() bool { return ready() == http.StatusServiceUnavailable })

	if code := run(context.Background(), cfg, []string{"migrate"}, &bytes.Buffer{}, &errOut); code != 0 {
		t.Fatalf("migrate: %s", errOut.String())
	}
	waitUntil(t, func() bool { return ready() == http.StatusNoContent })
	select {
	case <-jobRan:
	case <-time.After(5 * time.Second):
		t.Fatal("background job did not run")
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("serve exit %d: %s", code, errOut.String())
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(25 * time.Millisecond)
	}
}
