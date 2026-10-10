//go:build integration

package apikeys

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/app/apptest"
	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

// testIdentity signs in whoever the "session" cookie names.
type testIdentity struct{}

func (testIdentity) Name() string { return "test" }
func (testIdentity) Authenticate(r *http.Request) (identity.Identity, error) {
	c, err := r.Cookie("session")
	if err != nil {
		return identity.Identity{}, identity.ErrNoSession
	}
	return identity.Identity{Subject: c.Value, SignedInAt: time.Now()}, nil
}
func (testIdentity) Logout(http.ResponseWriter, *http.Request) error { return nil }
func (testIdentity) Delete(context.Context, *http.Request) error     { return nil }

type fixture struct {
	a        *app.App
	accounts *accounts.Accounts
	keys     *Keys
	r        *gin.Engine
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	cfg := app.Config{Name: "test", Migrations: []app.Migrations{accounts.Migrations, Migrations}}
	f := &fixture{a: apptest.New(t, cfg)}
	var err error
	if f.accounts, err = accounts.New(f.a, testIdentity{}, accounts.Config{}); err != nil {
		t.Fatal(err)
	}
	if f.keys, err = New(f.a, Config{Prefix: "acme"}); err != nil {
		t.Fatal(err)
	}
	if f.r, err = web.New(web.Config{PublicURL: "http://localhost:8080", SecretKey: strings.Repeat("s", 32)}); err != nil {
		t.Fatal(err)
	}
	layout := ui.Default(ui.Design{Name: "Test"}, ui.NewAssets("/assets"))
	f.r.GET("/token", func(c *gin.Context) { c.String(http.StatusOK, web.CSRFToken(c)) })
	f.keys.Mount(f.r.Group("/keys", f.accounts.RequireSignIn), layout)
	f.r.POST("/api/echo", f.keys.RequireKey, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"account": Current(c).AccountID, "authorization": c.GetHeader("Authorization")})
	})
	return f
}

func (f *fixture) account(t *testing.T, subject string) string {
	t.Helper()
	// Signing in once creates the account.
	rec := f.browser(http.MethodGet, "/keys", subject, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign in: %d", rec.Code)
	}
	var id string
	if err := f.a.DB().QueryRow(`SELECT id FROM accounts WHERE subject = $1`, subject).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) browser(method, path, session string, form url.Values, header http.Header) *httptest.ResponseRecorder {
	tok := httptest.NewRecorder()
	f.r.ServeHTTP(tok, httptest.NewRequest(http.MethodGet, "http://localhost:8080/token", nil))
	if form == nil {
		form = url.Values{}
	}
	form.Set(web.CSRFField, tok.Body.String())
	req := httptest.NewRequest(method, "http://localhost:8080"+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://localhost:8080")
	for k, v := range header {
		req.Header[k] = v
	}
	for _, c := range tok.Result().Cookies() {
		req.AddCookie(c)
	}
	req.AddCookie(&http.Cookie{Name: "session", Value: session})
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) api(header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/echo", nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	return rec
}

func TestKeyLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada := f.account(t, "ada")
	grace := f.account(t, "grace")

	raw, key, err := f.keys.Create(ctx, ada, " Production ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "acme_") || len(raw) != 48 || key.Name != "Production" || !strings.HasSuffix(raw, key.Suffix) {
		t.Fatalf("raw %q key %+v", raw, key)
	}
	var stored int
	if err := f.a.DB().QueryRow(`SELECT count(*) FROM api_keys k WHERE k::text LIKE '%' || $1 || '%'`, raw).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("the raw key is stored: %d %v", stored, err)
	}

	got, err := f.keys.Authenticate(ctx, raw)
	if err != nil || got.ID != key.ID || got.AccountID != ada {
		t.Fatalf("authenticate: %+v %v", got, err)
	}
	if keys, _ := f.keys.List(ctx, ada); len(keys) != 1 || keys[0].LastUsedAt == nil {
		t.Fatalf("last use not recorded: %+v", keys)
	}
	if _, err := f.keys.Authenticate(ctx, "evil_"+raw[len("acme_"):]); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("a key with another prefix authenticated")
	}

	// Changing the prefix keeps issued keys working.
	renamed, err := New(f.a, Config{Prefix: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := renamed.Authenticate(ctx, raw); err != nil {
		t.Fatalf("old key after a prefix change: %v", err)
	}

	if err := f.keys.Revoke(ctx, grace, key.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account revoked the key: %v", err)
	}
	if err := f.keys.Revoke(ctx, ada, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed ID: %v", err)
	}
	if err := f.keys.Revoke(ctx, ada, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.keys.Authenticate(ctx, raw); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("a revoked key authenticated")
	}
	if err := f.keys.Revoke(ctx, ada, key.ID); err != nil {
		t.Fatalf("revoking twice: %v", err)
	}
}

func TestKeysAreDeletedWithTheAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada := f.account(t, "ada")
	raw, _, _ := f.keys.Create(ctx, ada, "ci")
	if _, err := f.a.DB().Exec(`DELETE FROM accounts WHERE id = $1`, ada); err != nil {
		t.Fatal(err)
	}
	if _, err := f.keys.Authenticate(ctx, raw); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("a deleted account's key authenticated")
	}
}

func TestRequireKey(t *testing.T) {
	f := newFixture(t)
	ada := f.account(t, "ada")
	raw, _, _ := f.keys.Create(context.Background(), ada, "ci")

	rec := f.api(http.Header{"Authorization": {"bEaReR " + raw}, "Cookie": {"session=grace"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ada) {
		t.Fatalf("valid key: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), raw) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("the Authorization header reached the handler, or the response is cacheable")
	}
	for name, header := range map[string]http.Header{
		"missing":     {},
		"wrong type":  {"Authorization": {"Basic " + raw}},
		"two headers": {"Authorization": {"Bearer " + raw, "Bearer " + raw}},
		"extra field": {"Authorization": {"Bearer " + raw + " x"}},
		"unknown":     {"Authorization": {"Bearer acme_" + strings.Repeat("A", 43)}},
		"cookie only": {"Cookie": {"session=ada"}},
	} {
		rec := f.api(header)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s: %d %q", name, rec.Code, rec.Body.String())
		}
	}
}

func TestKeysPage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada := f.account(t, "ada")

	htmx := http.Header{"Hx-Request": {"true"}}
	rec := f.browser(http.MethodPost, "/keys", "ada", url.Values{"name": {"Laptop"}}, htmx)
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Retarget") != "#key-modal" {
		t.Fatalf("create: %d %q", rec.Code, rec.Header().Get("HX-Retarget"))
	}
	raw := regexp.MustCompile(`acme_[A-Za-z0-9_-]{43}`).FindString(rec.Body.String())
	if raw == "" || !strings.Contains(rec.Body.String(), `hx-swap-oob="true"`) || !strings.Contains(rec.Body.String(), "data-ephemeral") {
		t.Fatalf("create response:\n%s", rec.Body.String())
	}
	if _, err := f.keys.Authenticate(ctx, raw); err != nil {
		t.Fatalf("the shown key does not work: %v", err)
	}
	if page := f.browser(http.MethodGet, "/keys", "ada", nil, nil).Body.String(); strings.Contains(page, raw) || !strings.Contains(page, "Laptop") {
		t.Fatal("the list page shows the raw key, or misses the new key")
	}

	if rec := f.browser(http.MethodPost, "/keys", "ada", url.Values{"name": {""}}, htmx); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty name: %d", rec.Code)
	}
	if rec := f.browser(http.MethodPost, "/keys", "ada", url.Values{"name": {"Server"}}, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Your new API key") {
		t.Fatalf("create without JavaScript: %d", rec.Code)
	}

	keys, _ := f.keys.List(ctx, ada)
	f.account(t, "grace")
	if rec := f.browser(http.MethodPost, "/keys/"+keys[0].ID+"/revoke", "grace", nil, htmx); rec.Code != http.StatusOK {
		t.Fatalf("revoking another account's key: %d", rec.Code)
	}
	if after, _ := f.keys.List(ctx, ada); after[0].RevokedAt != nil {
		t.Fatal("another account revoked the key")
	}
	rec = f.browser(http.MethodPost, "/keys/"+keys[0].ID+"/revoke", "ada", nil, htmx)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Revoked") {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := f.browser(http.MethodPost, "/keys/"+keys[1].ID+"/revoke", "ada", nil, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke without JavaScript: %d", rec.Code)
	}
}
