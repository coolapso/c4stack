//go:build integration

package local

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/app/apptest"
	"github.com/coolapso/c4stack/email"
	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	stdout = io.Discard // what the operator commands print
	m.Run()
}

const publicURL = "http://localhost:8080"

// testSender captures the emails the provider sends.
type testSender struct {
	mu    sync.Mutex
	mails []email.Message
	fail  error
}

func (s *testSender) Send(_ context.Context, m email.Message) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return "", s.fail
	}
	s.mails = append(s.mails, m)
	return "id", nil
}

func (s *testSender) failWith(err error) {
	s.mu.Lock()
	s.fail = err
	s.mu.Unlock()
}

// to returns the emails sent to an address.
func (s *testSender) to(address string) []email.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []email.Message
	for _, m := range s.mails {
		if m.To == address {
			out = append(out, m)
		}
	}
	return out
}

func (s *testSender) last(t *testing.T, address string) email.Message {
	t.Helper()
	mails := s.to(address)
	if len(mails) == 0 {
		t.Fatalf("no email to %s", address)
	}
	return mails[len(mails)-1]
}

var codePattern = regexp.MustCompile(`\b(\d{4}) (\d{4})\b`)

// code returns the code in the last email to an address.
func (s *testSender) code(t *testing.T, address string) string {
	t.Helper()
	m := codePattern.FindStringSubmatch(s.last(t, address).Text)
	if m == nil {
		t.Fatalf("no code in the last email to %s", address)
	}
	return m[1] + m[2]
}

type fixture struct {
	t        *testing.T
	a        *app.App
	l        *Local
	accounts *accounts.Accounts
	mail     *testSender
	srv      *httptest.Server
	clock    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWith(t, nil)
}

// newFixtureWith is newFixture with configure changing the provider's Config.
func newFixtureWith(t *testing.T, configure func(*Config)) *fixture {
	t.Helper()
	f := &fixture{t: t, mail: &testSender{}, clock: time.Now()}
	f.a = apptest.New(t, app.Config{Name: "test", Migrations: []app.Migrations{accounts.Migrations, Migrations}})
	cfg := Config{
		AppName:      "Acme",
		PublicURL:    publicURL,
		SecretKey:    strings.Repeat("s", 32),
		Sender:       f.mail,
		SignedInPath: "/account",
	}
	if configure != nil {
		configure(&cfg)
	}
	var err error
	f.l, err = New(f.a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.l.now = func() time.Time { return f.clock }
	f.accounts, err = accounts.New(f.a, f.l, accounts.Config{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := web.New(web.Config{
		PublicURL:      publicURL,
		SecretKey:      strings.Repeat("s", 32),
		AnyHost:        true,
		TrustedProxies: []string{"127.0.0.1"},
		Logger:         slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	assets := ui.NewAssets("/assets")
	layout := ui.Default(ui.Design{Name: "Acme"}, assets)
	r.GET("/assets/*path", assets.Serve)
	f.l.Mount(r, layout)
	f.accounts.Mount(r, layout)
	r.GET("/account", f.accounts.RequireSignIn, func(c *gin.Context) {
		id, _ := identity.FromContext(c.Request.Context())
		c.String(http.StatusOK, "account=%s email=%s method=%s", accounts.Current(c).ID, id.Email, id.Method)
	})
	r.GET("/test/device", func(c *gin.Context) {
		d, ok := readDevice(c)
		if !ok {
			c.Status(http.StatusNotFound)
			return
		}
		c.JSON(http.StatusOK, d)
	})
	f.srv = httptest.NewServer(r)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) db() *sql.DB { return f.a.DB() }

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db().Exec(query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db().QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) userID(address string) string {
	f.t.Helper()
	var id string
	if err := f.db().QueryRow(`SELECT id FROM local_users WHERE email = $1`, address).Scan(&id); err != nil {
		f.t.Fatalf("user %s: %v", address, err)
	}
	return id
}

// browser is a browser with its own cookies, network and user agent.
type browser struct {
	t      *testing.T
	f      *fixture
	client *http.Client
	ip     string
	ua     string
	csrf   string
}

func (f *fixture) browser(ip string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{
		t:  f.t,
		f:  f,
		ip: ip,
		ua: "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0",
		client: &http.Client{
			Jar: jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

type response struct {
	status   int
	location string
	body     string
}

func (b *browser) do(req *http.Request) response {
	b.t.Helper()
	req.Header.Set("X-Forwarded-For", b.ip)
	req.Header.Set("User-Agent", b.ua)
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	r := response{status: resp.StatusCode, location: resp.Header.Get("Location"), body: string(body)}
	if m := csrfPattern.FindStringSubmatch(r.body); m != nil {
		b.csrf = html.UnescapeString(m[1])
	}
	return r
}

var csrfPattern = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func (b *browser) get(path string) response {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, b.f.srv.URL+path, nil)
	return b.do(req)
}

func (b *browser) token() string {
	b.t.Helper()
	if b.csrf == "" {
		b.get("/login?next=/account")
	}
	return b.csrf
}

func (b *browser) post(path string, form url.Values) response {
	b.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set(web.CSRFField, b.token())
	req, _ := http.NewRequest(http.MethodPost, b.f.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", publicURL)
	return b.do(req)
}

func (b *browser) postJSON(path string, body any) (int, map[string]any, []byte) {
	b.t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		b.t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, b.f.srv.URL+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", publicURL)
	req.Header.Set(web.CSRFHeader, b.token())
	r := b.do(req)
	var out map[string]any
	_ = json.Unmarshal([]byte(r.body), &out)
	return r.status, out, []byte(r.body)
}

// askCode posts an address to the sign-in form.
func (b *browser) askCode(address string) response {
	b.t.Helper()
	b.get("/login?next=/account")
	return b.post("/login", url.Values{"email": {address}, "next": {"/account"}})
}

// enter posts a code.
func (b *browser) enter(code string) response {
	b.t.Helper()
	return b.post("/login/code", url.Values{"code": {code}})
}

// signIn signs in with a code and returns the user's ID.
func (b *browser) signIn(address string) string {
	b.t.Helper()
	if r := b.askCode(address); r.status != http.StatusSeeOther || r.location != "/login/code" {
		b.t.Fatalf("asking for a code: %d %s %s", r.status, r.location, r.body)
	}
	address, _ = normalizeAddress(address)
	r := b.enter(b.f.mail.code(b.t, address))
	if r.status != http.StatusSeeOther || r.location != "/account" {
		b.t.Fatalf("entering the code: %d %s %s", r.status, r.location, r.body)
	}
	return b.f.userID(address)
}

// account returns what /account says, or "" when the browser isn't signed in.
func (b *browser) account() string {
	b.t.Helper()
	r := b.get("/account")
	if r.status != http.StatusOK {
		return ""
	}
	return r.body
}

func (b *browser) cookie(name string) *http.Cookie {
	u, _ := url.Parse(b.f.srv.URL)
	for _, c := range b.client.Jar.Cookies(u) {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// clone returns a browser with a copy of this one's cookies, as a thief would have.
func (b *browser) clone(ip string) *browser {
	c := b.f.browser(ip)
	u, _ := url.Parse(b.f.srv.URL)
	c.client.Jar.SetCookies(u, b.client.Jar.Cookies(u))
	c.csrf = b.csrf
	return c
}

// backdateSessions makes the user's sessions older, as if they signed in d ago.
func (f *fixture) backdateSessions(userID string, d time.Duration) {
	f.t.Helper()
	f.exec(`UPDATE local_sessions SET created_at = created_at - make_interval(secs => $2) WHERE user_id = $1`, userID, d.Seconds())
}

func contains(t *testing.T, r response, want string) {
	t.Helper()
	if !strings.Contains(html.UnescapeString(r.body), want) {
		t.Fatalf("the page does not say %q:\n%d %s", want, r.status, r.body)
	}
}

func wantStatus(t *testing.T, r response, status int) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s %s", r.status, status, r.location, r.body)
	}
}

var errSend = errors.New("the provider refused the message")
