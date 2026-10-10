//go:build apptest

package scaffold

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coolapso/c4stack/internal/testdb"
)

// TestStarterApp generates the starter app against this checkout, runs its main tasks
// through the shared Taskfile, then runs the binary against a disposable database and
// a stand-in for Hanko: a visitor is sent to sign in, a signed-in user sees settings
// and deletes their account. It needs Go, Task and TEST_DATABASE_URL; task test:app
// provides them.
func TestStarterApp(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	checkout := filepath.Join(filepath.Dir(file), "..", "..")
	dir := filepath.Join(t.TempDir(), "acme")
	if err := Generate(Options{Dir: dir, Name: "acme", Local: checkout}); err != nil {
		t.Fatal(err)
	}
	sh := func(env []string, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
		return string(out)
	}
	sh(nil, "go", "mod", "tidy")
	sh(nil, "go", "tool", "templ", "generate")
	sh(nil, "go", "mod", "tidy")

	list := sh(nil, "task", "--list")
	for _, task := range []string{"dev:", "build:", "check:", "test:integration:", "db:migrate:", "framework:update:", "container:push:"} {
		if !strings.Contains(list, "* "+task) {
			t.Errorf("task --list lacks %s", task)
		}
	}
	sh(nil, "task", "build")
	sh(nil, "task", "test")
	sh(nil, "task", "check")

	// The app's own settings reach the shared tasks.
	taskfile, err := os.ReadFile(filepath.Join(dir, "Taskfile.yml"))
	if err != nil {
		t.Fatal(err)
	}
	custom := strings.Replace(string(taskfile), "  IMAGE: acme\n", "  IMAGE: acme\n  TEST_DB_PORT: '55999'\n  PLATFORMS: linux/riscv64\n", 1)
	if err := os.WriteFile(filepath.Join(dir, "Taskfile.yml"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := sh(nil, "task", "--dry", "test:db:up"); !strings.Contains(out, "127.0.0.1:55999:5432") {
		t.Errorf("TEST_DB_PORT did not reach the shared tasks:\n%s", out)
	}
	if out := sh(nil, "task", "--dry", "container:push", "REGISTRY=registry.example"); !strings.Contains(out, "--platform linux/riscv64") {
		t.Errorf("PLATFORMS did not reach the shared tasks:\n%s", out)
	}
	if err := os.WriteFile(filepath.Join(dir, "Taskfile.yml"), taskfile, 0o644); err != nil {
		t.Fatal(err)
	}

	// Subdirectories run shared tasks from the app's root.
	if err := os.MkdirAll(filepath.Join(dir, "static", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("task", "build")
	cmd.Dir = filepath.Join(dir, "static", "deep")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("task build from a subdirectory: %v\n%s", err, out)
	}

	databaseURL := testdb.New(t)
	migrateEnv := []string{"ACME_DATABASE_URL=" + databaseURL, "ACME_HANKO_API_URL=", "ACME_SECRET_KEY="}
	sh(migrateEnv, "task", "db:migrate")
	status := sh(migrateEnv, "task", "run", "--", "migrate", "status")
	if !regexp.MustCompile(`accounts\s+1\s+-`).MatchString(status) {
		t.Fatalf("migrate status:\n%s", status)
	}

	hanko := newStandIn(t)
	port := freePort(t)
	public := "http://127.0.0.1:" + port
	server := exec.Command(filepath.Join(dir, "bin", "acme"), "serve")
	server.Env = []string{
		"ACME_PUBLIC_URL=" + public,
		"ACME_ADDR=127.0.0.1:" + port,
		"ACME_DATABASE_URL=" + databaseURL,
		"ACME_SECRET_KEY=" + strings.Repeat("k", 32),
		"ACME_HANKO_API_URL=" + hanko.URL,
	}
	var logs strings.Builder
	server.Stdout, server.Stderr = &logs, &logs
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			server.Process.Kill()
			server.Wait()
		}
		if t.Failed() {
			t.Logf("server logs:\n%s", logs.String())
		}
	}()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string, cookies ...*http.Cookie) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodGet, public+path, nil)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		resp, err := client.Do(req)
		if err != nil {
			return &http.Response{StatusCode: 0}, ""
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		if resp, _ := get("/health/ready"); resp.StatusCode == http.StatusNoContent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the app never became ready")
		}
		time.Sleep(100 * time.Millisecond)
	}

	if resp, body := get("/"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Welcome to Acme") {
		t.Fatalf("home: %d", resp.StatusCode)
	}
	if resp, _ := get("/account"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?next=%2Faccount" {
		t.Fatalf("visitor: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, body := get("/login")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `data-api-url="`+hanko.URL+`"`) {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "connect-src 'self' "+hanko.URL) {
		t.Errorf("login CSP: %q", resp.Header.Get("Content-Security-Policy"))
	}
	stylesheet := regexp.MustCompile(`href="(/assets/[0-9a-f]+/ui/base\.css)"`).FindStringSubmatch(body)
	if stylesheet == nil {
		t.Fatal("no default stylesheet link")
	}
	if resp, _ := get(stylesheet[1]); resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("stylesheet: %d", resp.StatusCode)
	}

	session := &http.Cookie{Name: "hanko", Value: "valid"}
	resp, body = get("/account", session)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "ada@example.com") || !strings.Contains(body, "Delete account") {
		t.Fatalf("settings: %d", resp.StatusCode)
	}
	var csrf *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "csrf" {
			csrf = c
		}
	}
	token := regexp.MustCompile(`<meta name="csrf-token" content="([^"]+)"`).FindStringSubmatch(body)
	if csrf == nil || token == nil {
		t.Fatal("no CSRF cookie or token on the settings page")
	}

	form := url.Values{"confirmation": {"DELETE"}, "csrf_token": {token[1]}}
	req, _ := http.NewRequest(http.MethodPost, public+"/account/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", public)
	req.AddCookie(session)
	req.AddCookie(csrf)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	deleted, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(deleted), "Your account was deleted") {
		t.Fatalf("delete: %d\n%s", resp.StatusCode, deleted)
	}
	if !hanko.deleted() {
		t.Fatal("the identity was not deleted at the provider")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var accounts int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM accounts`).Scan(&accounts); err != nil || accounts != 0 {
		t.Fatalf("accounts after deletion: %d %v", accounts, err)
	}
	if resp, _ := get("/account", session); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deleted session: %d", resp.StatusCode)
	}

	server.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- server.Wait() }()
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatalf("serve exited with %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not stop after SIGTERM")
	}
}

// standIn answers the Hanko API calls the starter app makes.
type standIn struct {
	*httptest.Server
	mu   sync.Mutex
	gone bool
}

func newStandIn(t *testing.T) *standIn {
	s := &standIn{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/validate", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionToken string `json:"session_token"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		valid := body.SessionToken == "valid" && !s.gone
		s.mu.Unlock()
		if !valid {
			json.NewEncoder(w).Encode(map[string]any{"is_valid": false})
			return
		}
		fmt.Fprintf(w, `{"is_valid":true,"claims":{"subject":"user-1","issued_at":%q,"email":{"address":"ada@example.com","is_verified":true}}}`,
			time.Now().UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /profile", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "":
			io.WriteString(w, `{"name":"profile_init","status":200,"csrf_token":"1","actions":{"register_client_capabilities":{"href":"/profile?action=capabilities"}}}`)
		case "capabilities":
			io.WriteString(w, `{"name":"profile_init","status":200,"csrf_token":"2","actions":{"account_delete":{"href":"/profile?action=delete"}}}`)
		case "delete":
			s.mu.Lock()
			s.gone = true
			s.mu.Unlock()
			io.WriteString(w, `{"name":"account_deleted","status":200}`)
		}
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *standIn) deleted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gone
}

func freePort(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}
