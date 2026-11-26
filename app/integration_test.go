//go:build integration

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coolapso/c4stack/internal/testdb"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
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
	if code := run(context.Background(), cfg, []string{"migrate"}, &out, &errOut, nil); code != 0 {
		t.Fatalf("migrate: exit %d: %s", code, errOut.String())
	}
	out.Reset()
	if code := run(context.Background(), cfg, []string{"migrate", "status"}, &out, &errOut, nil); code != 0 {
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
	if code := run(context.Background(), cfg, []string{"accounts", "delete-local", "abc"}, &out, &errOut, nil); code != 0 || len(got) != 1 || got[0] != "abc" {
		t.Fatalf("exit %d, args %v: %s", code, got, errOut.String())
	}
	if code := run(context.Background(), cfg, []string{"accounts", "purge"}, &out, &errOut, nil); code != 2 {
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
	go func() { done <- run(ctx, cfg, []string{"serve"}, &bytes.Buffer{}, &errOut, nil) }()

	ready := func() int {
		resp, err := http.Get("http://" + addr + "/health/ready")
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	waitUntil(t, func() bool { return ready() == http.StatusServiceUnavailable })

	if code := run(context.Background(), cfg, []string{"migrate"}, &bytes.Buffer{}, &errOut, nil); code != 0 {
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

// TestServeSeveralServers: a public server and an internal one start together and stop
// together; one that can't start stops the others, and serve fails.
func TestServeSeveralServers(t *testing.T) {
	public, internal := freeAddr(t), freeAddr(t)
	sum := sha256.Sum256([]byte("caller-token"))
	setup := func(internalAddr string) func(context.Context, *App) error {
		return func(ctx context.Context, a *App) error {
			r, err := web.New(web.Config{PublicURL: "http://" + public, SecretKey: strings.Repeat("k", 32)})
			if err != nil {
				return err
			}
			r.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })
			in, err := web.NewInternal(web.InternalConfig{TokenHashes: []string{hex.EncodeToString(sum[:])}})
			if err != nil {
				return err
			}
			in.GET("/v1/ping", func(c *gin.Context) { c.Status(http.StatusNoContent) })
			a.Serve(web.Server{Addr: public, Handler: r, HealthPath: "/health", DrainDelay: time.Millisecond})
			a.Serve(web.Server{Addr: internalAddr, Handler: in, DrainDelay: time.Millisecond})
			return nil
		}
	}
	cfg := Config{Name: "acme", DatabaseURL: testdb.New(t), Migrations: migrations(), Setup: setup(internal)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var errOut bytes.Buffer
	go func() { done <- run(ctx, cfg, []string{"serve"}, &bytes.Buffer{}, &errOut, nil) }()

	get := func(url, token string) int {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	waitUntil(t, func() bool { return get("http://"+public+"/", "") == http.StatusNoContent })
	waitUntil(t, func() bool { return get("http://"+internal+"/v1/ping", "caller-token") == http.StatusNoContent })
	if code := get("http://"+internal+"/v1/ping", ""); code != http.StatusUnauthorized {
		t.Fatalf("internal without a token: %d", code)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("serve exit %d: %s", code, errOut.String())
	}
	if get("http://"+public+"/", "") != 0 || get("http://"+internal+"/v1/ping", "caller-token") != 0 {
		t.Fatal("a server still answers after serve returned")
	}

	// The internal address is taken: serve fails, and the public server doesn't stay up.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	cfg.Setup = setup(taken.Addr().String())
	errOut.Reset()
	if code := run(context.Background(), cfg, []string{"serve"}, &bytes.Buffer{}, &errOut, nil); code == 0 {
		t.Fatal("serve succeeded with its internal address taken")
	}
	if !strings.Contains(errOut.String(), taken.Addr().String()) {
		t.Fatalf("the error doesn't name the address: %s", errOut.String())
	}
	if get("http://"+public+"/", "") != 0 {
		t.Fatal("the public server stayed up")
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
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
