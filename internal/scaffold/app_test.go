//go:build apptest

package scaffold

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/cookiejar"
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
// a stand-in mail server: a visitor is sent to sign in, signs up with the code from the
// email, sees settings, and deletes their account. It needs Go, Task, OpenSSL and
// TEST_DATABASE_URL; task test:app provides them.
func TestStarterApp(t *testing.T) {
	dir := generateStarter(t, "")

	// Task colours its output when CI is set, even into a pipe, which splits "* dev:".
	list := shell(t, dir, []string{"NO_COLOR=1"}, "task", "--list")
	for _, task := range []string{"dev:", "build:", "check:", "test:integration:", "db:migrate:", "mail:up:", "framework:update:", "container:push:"} {
		if !strings.Contains(list, "* "+task) {
			t.Errorf("task --list lacks %s", task)
		}
	}
	shell(t, dir, nil, "task", "build")
	shell(t, dir, nil, "task", "test")
	shell(t, dir, nil, "task", "check")

	// The app's own settings reach the shared tasks.
	taskfile, err := os.ReadFile(filepath.Join(dir, "Taskfile.yml"))
	if err != nil {
		t.Fatal(err)
	}
	custom := strings.Replace(string(taskfile), "  IMAGE: acme\n", "  IMAGE: acme\n  TEST_DB_PORT: '55999'\n  PLATFORMS: linux/riscv64\n", 1)
	if err := os.WriteFile(filepath.Join(dir, "Taskfile.yml"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := shell(t, dir, nil, "task", "--dry", "test:db:up"); !strings.Contains(out, "127.0.0.1:55999:5432") {
		t.Errorf("TEST_DB_PORT did not reach the shared tasks:\n%s", out)
	}
	if out := shell(t, dir, nil, "task", "--dry", "container:push", "REGISTRY=registry.example"); !strings.Contains(out, "--platform linux/riscv64") {
		t.Errorf("PLATFORMS did not reach the shared tasks:\n%s", out)
	}
	if err := os.WriteFile(filepath.Join(dir, "Taskfile.yml"), taskfile, 0o644); err != nil {
		t.Fatal(err)
	}
	// The Taskfile's settings, with its new secret key, are enough to set the app up: help
	// lists the sign-in's commands. A setting in the shell wins over the Taskfile's.
	if out := shell(t, dir, nil, "task", "run", "--", "help"); !strings.Contains(out, "local set-email") {
		t.Errorf("help with the Taskfile's settings:\n%s", out)
	}
	if out := shell(t, dir, []string{"ACME_SMTP_URL=http://mail.example"}, "task", "run", "--", "help"); !strings.Contains(out, "smtp: the URL must be") {
		t.Errorf("help with the shell's SMTP URL:\n%s", out)
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
	migrateEnv := []string{"ACME_DATABASE_URL=" + databaseURL}
	shell(t, dir, migrateEnv, "task", "db:migrate")
	status := shell(t, dir, migrateEnv, "task", "run", "--", "migrate", "status")
	if !regexp.MustCompile(`accounts\s+2\s+-`).MatchString(status) || !regexp.MustCompile(`local\s+1\s+-`).MatchString(status) {
		t.Fatalf("migrate status:\n%s", status)
	}

	mail := newMailCatcher(t)
	port := freePort(t)
	public := "http://localhost:" + port
	stop := serve(t, dir, []string{
		"ACME_PUBLIC_URL=" + public,
		"ACME_ADDR=127.0.0.1:" + port,
		"ACME_DATABASE_URL=" + databaseURL,
		"ACME_SECRET_KEY=" + strings.Repeat("k", 32),
		"ACME_EMAIL_FROM=noreply@acme.example",
		"ACME_SMTP_URL=smtp://" + mail.addr,
		"ACME_TRUSTED_PROXIES=127.0.0.1, ::1",
	}, public)

	jar, _ := cookiejar.New(nil)
	b := &visitor{t: t, public: public, client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}

	if resp, body := b.get("/"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Welcome to Acme") {
		t.Fatalf("home: %d", resp.StatusCode)
	}
	if resp, _ := b.get("/account"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?next=%2Faccount" {
		t.Fatalf("visitor: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, body := b.get("/login?next=/account")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `autocomplete="username webauthn"`) {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	stylesheet := regexp.MustCompile(`href="(/assets/[0-9a-f]+/ui/base\.css)"`).FindStringSubmatch(body)
	if stylesheet == nil {
		t.Fatal("no default stylesheet link")
	}
	if resp, _ := b.get(stylesheet[1]); resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("stylesheet: %d", resp.StatusCode)
	}

	// Sign up with the code the email carries.
	if resp := b.post("/login", url.Values{"email": {"ada@example.com"}, "next": {"/account"}}); resp.Header.Get("Location") != "/login/code" {
		t.Fatalf("asking for a code: %d", resp.StatusCode)
	}
	message := mail.last(t)
	if !strings.Contains(message, "Subject: Your Acme sign-up code") || !strings.Contains(message, "From: \"Acme\" <noreply@acme.example>") {
		t.Fatalf("the email:\n%s", message)
	}
	code := regexp.MustCompile(`\b(\d{4}) (\d{4})\b`).FindStringSubmatch(message)
	if code == nil {
		t.Fatalf("no code in the email:\n%s", message)
	}
	if resp := b.post("/login/code", url.Values{"code": {code[1] + code[2]}}); resp.Header.Get("Location") != "/account" {
		t.Fatalf("entering the code: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, body := b.get("/account"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "You are signed in as ada@example.com") {
		t.Fatalf("account: %d", resp.StatusCode)
	}
	if resp, body := b.get("/account/settings"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Passkeys") || !strings.Contains(body, "Delete account") {
		t.Fatalf("settings: %d", resp.StatusCode)
	}

	b.get("/account/delete")
	if resp := b.post("/account/delete", url.Values{"confirmation": {"DELETE"}}); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account/deleted" {
		t.Fatalf("delete: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, body := b.get("/account/deleted"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Your account is deleted") {
		t.Fatalf("deleted page: %d", resp.StatusCode)
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rows int
	if err := db.QueryRowContext(context.Background(), `SELECT (SELECT count(*) FROM accounts) + (SELECT count(*) FROM local_users)`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("accounts and users after deletion: %d %v", rows, err)
	}
	if resp, _ := b.get("/account"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deleted session: %d", resp.StatusCode)
	}

	// Through the trusted proxy, each client is its own network: more clients than one
	// network's limit on codes all get theirs.
	b.get("/login")
	for i := range 31 {
		form := url.Values{"email": {fmt.Sprintf("user%d@example.com", i)}, "csrf_token": {b.csrf}}
		req, _ := http.NewRequest(http.MethodPost, public+"/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", public)
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i+1))
		resp, err := b.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("client %d through the proxy: %d", i, resp.StatusCode)
		}
	}
	stop()
}

// TestStarterAppOnHanko generates the starter app with Hanko's sign-in, and runs it
// against a stand-in for Hanko: a visitor is sent to Hanko's sign-in page, a signed-in
// user sees settings and deletes their account.
func TestStarterAppOnHanko(t *testing.T) {
	dir := generateStarter(t, "hanko")
	shell(t, dir, nil, "task", "build")
	databaseURL := testdb.New(t)
	shell(t, dir, []string{"ACME_DATABASE_URL=" + databaseURL}, "task", "db:migrate")

	hanko := newStandIn(t)
	port := freePort(t)
	public := "http://127.0.0.1:" + port
	stop := serve(t, dir, []string{
		"ACME_PUBLIC_URL=" + public,
		"ACME_ADDR=127.0.0.1:" + port,
		"ACME_DATABASE_URL=" + databaseURL,
		"ACME_SECRET_KEY=" + strings.Repeat("k", 32),
		"ACME_HANKO_API_URL=" + hanko.URL,
	}, public)

	jar, _ := cookiejar.New(nil)
	b := &visitor{t: t, public: public, client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	resp, body := b.get("/login")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `data-api-url="`+hanko.URL+`"`) {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "connect-src 'self' "+hanko.URL) {
		t.Errorf("login CSP: %q", resp.Header.Get("Content-Security-Policy"))
	}
	u, _ := url.Parse(public)
	jar.SetCookies(u, []*http.Cookie{{Name: "hanko", Value: "valid"}})
	if resp, body := b.get("/account"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "You are signed in as ada@example.com") {
		t.Fatalf("account: %d", resp.StatusCode)
	}
	b.get("/account/settings")
	if resp := b.post("/account/delete", url.Values{"confirmation": {"DELETE"}}); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account/deleted" {
		t.Fatalf("delete: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if !hanko.deleted() {
		t.Fatal("the identity was not deleted at the provider")
	}
	stop()
}

// generateStarter generates the starter app against this checkout, and tidies and
// generates its code.
func generateStarter(t *testing.T, identity string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	checkout := filepath.Join(filepath.Dir(file), "..", "..")
	dir := filepath.Join(t.TempDir(), "acme")
	if err := Generate(Options{Dir: dir, Name: "acme", Local: checkout, Identity: identity}); err != nil {
		t.Fatal(err)
	}
	shell(t, dir, nil, "go", "mod", "tidy")
	shell(t, dir, nil, "go", "tool", "templ", "generate")
	shell(t, dir, nil, "go", "mod", "tidy")
	return dir
}

func shell(t *testing.T, dir string, env []string, name string, args ...string) string {
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

// serve starts the built app with env until it is ready, and returns a function that
// stops it with SIGTERM and checks it exits cleanly.
func serve(t *testing.T, dir string, env []string, public string) func() {
	t.Helper()
	server := exec.Command(filepath.Join(dir, "bin", "acme"), "serve")
	server.Env = env
	var logs strings.Builder
	server.Stdout, server.Stderr = &logs, &logs
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			server.Process.Kill()
			server.Wait()
		}
		if t.Failed() {
			t.Logf("server logs:\n%s", logs.String())
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(public + "/health/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the app never became ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return func() {
		t.Helper()
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
}

// visitor is a browser with cookies. Its POSTs carry the page's CSRF token and Origin.
type visitor struct {
	t      *testing.T
	public string
	client *http.Client
	csrf   string
}

var csrfMeta = regexp.MustCompile(`<meta name="csrf-token" content="([^"]+)"`)

func (v *visitor) get(path string) (*http.Response, string) {
	v.t.Helper()
	resp, err := v.client.Get(v.public + path)
	if err != nil {
		v.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if m := csrfMeta.FindStringSubmatch(string(body)); m != nil {
		v.csrf = m[1]
	}
	return resp, string(body)
}

func (v *visitor) post(path string, form url.Values) *http.Response {
	v.t.Helper()
	form.Set("csrf_token", v.csrf)
	req, _ := http.NewRequest(http.MethodPost, v.public+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", v.public)
	resp, err := v.client.Do(req)
	if err != nil {
		v.t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// mailCatcher is a stand-in SMTP server on loopback that keeps the messages it gets.
type mailCatcher struct {
	addr     string
	mu       sync.Mutex
	messages []string
}

func newMailCatcher(t *testing.T) *mailCatcher {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	m := &mailCatcher{addr: ln.Addr().String()}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go m.serve(conn)
		}
	}()
	return m
}

func (m *mailCatcher) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	reply := func(line string) { io.WriteString(conn, line+"\r\n") }
	reply("220 catcher")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			reply("250 catcher")
		case cmd == "DATA":
			reply("354 go on")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			m.mu.Lock()
			m.messages = append(m.messages, b.String())
			m.mu.Unlock()
			reply("250 queued")
		case cmd == "QUIT":
			reply("221 bye")
			return
		default:
			reply("250 ok")
		}
	}
}

func (m *mailCatcher) last(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.messages) == 0 {
		t.Fatal("no email arrived")
	}
	msg := m.messages[len(m.messages)-1]
	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(msg)))
	if err != nil {
		return msg
	}
	return string(decoded)
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
