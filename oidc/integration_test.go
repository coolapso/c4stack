//go:build integration

package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/app/apptest"
	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
	jose "github.com/go-jose/go-jose/v4"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

const (
	publicURL   = "http://localhost:8080"
	redirectURI = "https://notes.example.com/callback"
	testSecret  = "ssssssssssssssssssssssssssssssss"
)

// testIdentity signs in whoever the "session" cookie names. The "signed-in" cookie,
// Unix seconds, sets when they signed in; without it, it was now.
type testIdentity struct{}

func (testIdentity) Name() string { return "test" }
func (testIdentity) Authenticate(r *http.Request) (identity.Identity, error) {
	c, err := r.Cookie("session")
	if err != nil {
		return identity.Identity{}, identity.ErrNoSession
	}
	at := time.Now()
	if s, err := r.Cookie("signed-in"); err == nil {
		n, _ := strconv.ParseInt(s.Value, 10, 64)
		at = time.Unix(n, 0)
	}
	return identity.Identity{Subject: c.Value, Email: c.Value + "@example.com", EmailVerified: true, SignedInAt: at}, nil
}
func (testIdentity) Logout(http.ResponseWriter, *http.Request) error { return nil }
func (testIdentity) Delete(context.Context, *http.Request) error     { return nil }

type fixture struct {
	t        *testing.T
	a        *app.App
	accounts *accounts.Accounts
	p        *Provider
	r        *gin.Engine
	secret   string
	other    string
	clients  map[string]Client
	mu       sync.Mutex
}

func newFixture(t *testing.T, change ...func(*Config)) *fixture {
	t.Helper()
	return newFixtureWith(t, accounts.Config{}, change...)
}

func newFixtureWith(t *testing.T, ac accounts.Config, change ...func(*Config)) *fixture {
	t.Helper()
	f := &fixture{t: t, a: apptest.New(t, app.Config{Name: "test", Migrations: []app.Migrations{accounts.Migrations, Migrations}})}
	var err error
	if f.accounts, err = accounts.New(f.a, testIdentity{}, ac); err != nil {
		t.Fatal(err)
	}
	var notesHash, otherHash []byte
	f.secret, notesHash = NewSecret()
	f.other, otherHash = NewSecret()
	f.clients = map[string]Client{
		"notes": {ID: "notes", SecretHash: notesHash, RedirectURIs: []string{redirectURI, "http://notes.example.com/insecure"}},
		"other": {ID: "other", SecretHash: otherHash, RedirectURIs: []string{"https://other.example.com/callback"}},
	}
	cfg := Config{Issuer: publicURL, SecretKey: testSecret, Client: f.client}
	for _, c := range change {
		c(&cfg)
	}
	f.mount(cfg)
	return f
}

// mount builds the provider and its routes on a new engine, over the fixture's database.
func (f *fixture) mount(cfg Config) {
	f.t.Helper()
	var err error
	if f.p, err = New(f.a, f.accounts, cfg); err != nil {
		f.t.Fatal(err)
	}
	if f.r, err = web.New(web.Config{PublicURL: publicURL, SecretKey: testSecret}); err != nil {
		f.t.Fatal(err)
	}
	f.p.Mount(f.r, ui.Default(ui.Design{Name: "Test"}, ui.NewAssets("/assets")))
}

func (f *fixture) client(_ context.Context, id string) (Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.clients[id]
	if !ok {
		return Client{}, ErrUnknownClient
	}
	return c, nil
}

// browser keeps cookies between requests, as a browser does.
type browser struct {
	f       *fixture
	cookies map[string]*http.Cookie
}

func (f *fixture) browser(session string) *browser {
	b := &browser{f: f, cookies: map[string]*http.Cookie{}}
	if session != "" {
		b.cookies["session"] = &http.Cookie{Name: "session", Value: session}
	}
	return b
}

func (b *browser) get(target string) *httptest.ResponseRecorder {
	if strings.HasPrefix(target, "/") {
		target = publicURL + target
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for _, c := range b.cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	b.f.r.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return rec
}

// rp is a client's side of one sign-in.
type rp struct {
	clientID, redirect, verifier, state, nonce string
}

func newRP(clientID, redirect string) *rp {
	return &rp{clientID: clientID, redirect: redirect, verifier: newToken() + "-v", state: newToken(), nonce: newToken()}
}

func (c *rp) authorizeURL(extra url.Values) string {
	sum := sha256.Sum256([]byte(c.verifier))
	v := url.Values{
		"client_id":             {c.clientID},
		"redirect_uri":          {c.redirect},
		"response_type":         {"code"},
		"scope":                 {"openid profile email"},
		"state":                 {c.state},
		"nonce":                 {c.nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	for k, vs := range extra {
		if len(vs) == 1 && vs[0] == "" {
			v.Del(k)
		} else {
			v[k] = vs
		}
	}
	return authorizePath + "?" + v.Encode()
}

func location(t *testing.T, rec *httptest.ResponseRecorder) *url.URL {
	t.Helper()
	if rec.Code < 300 || rec.Code > 399 {
		t.Fatalf("status %d, want a redirect:\n%s", rec.Code, rec.Body.String())
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// authorize starts a sign-in and returns where the provider sent the browser.
func (b *browser) authorize(t *testing.T, c *rp, extra url.Values) *url.URL {
	t.Helper()
	return location(t, b.get(c.authorizeURL(extra)))
}

// signIn runs a sign-in to the redirect and returns the redirect's query.
func (b *browser) signIn(t *testing.T, c *rp, extra url.Values) url.Values {
	t.Helper()
	cont := b.authorize(t, c, extra)
	if cont.Path != continuePath {
		t.Fatalf("authorize sent the browser to %s", cont)
	}
	back := location(t, b.get(cont.String()))
	if !strings.HasPrefix(back.String(), c.redirect+"?") {
		t.Fatalf("continue sent the browser to %s", back)
	}
	q := back.Query()
	if q.Get("state") != c.state {
		t.Fatalf("state %q, want %q", q.Get("state"), c.state)
	}
	return q
}

func (b *browser) code(t *testing.T, c *rp) string {
	t.Helper()
	q := b.signIn(t, c, nil)
	if q.Get("code") == "" {
		t.Fatalf("no code: %v", q)
	}
	return q.Get("code")
}

type tokens struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
	Error       string `json:"error"`
}

// exchange posts a code to the token endpoint, with the client's secret in basic auth.
func (f *fixture) exchange(clientID, secret string, form url.Values) (*httptest.ResponseRecorder, tokens) {
	form.Set("grant_type", "authorization_code")
	req := httptest.NewRequest(http.MethodPost, publicURL+tokenPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if clientID != "" {
		req.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(secret))
	}
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	var tk tokens
	_ = json.Unmarshal(rec.Body.Bytes(), &tk)
	return rec, tk
}

func (f *fixture) redeem(c *rp, code string) (*httptest.ResponseRecorder, tokens) {
	return f.exchange(c.clientID, f.secret, url.Values{"code": {code}, "redirect_uri": {c.redirect}, "code_verifier": {c.verifier}})
}

func (f *fixture) get(target string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, publicURL+target, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	return rec
}

// verify checks an ID token's signature with the published keys and returns its claims.
func (f *fixture) verify(t *testing.T, token string) map[string]any {
	t.Helper()
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(f.get(keysPath, nil).Body.Bytes(), &set); err != nil {
		t.Fatal(err)
	}
	jws, err := jose.ParseSignedCompact(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatal(err)
	}
	keys := set.Key(jws.Signatures[0].Header.KeyID)
	if len(keys) != 1 {
		t.Fatalf("the key set has %d keys with the token's ID", len(keys))
	}
	payload, err := jws.Verify(keys[0])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	claims["kid"] = jws.Signatures[0].Header.KeyID
	return claims
}

func (f *fixture) accountOf(t *testing.T, subject string) string {
	t.Helper()
	var id string
	if err := f.a.DB().QueryRow(`SELECT account_id FROM account_identities WHERE subject = $1`, subject).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.a.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDiscovery(t *testing.T) {
	f := newFixture(t)
	rec := f.get("/.well-known/openid-configuration", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"issuer":                                publicURL,
		"authorization_endpoint":                publicURL + authorizePath,
		"token_endpoint":                        publicURL + tokenPath,
		"userinfo_endpoint":                     publicURL + userinfoPath,
		"jwks_uri":                              publicURL + keysPath,
		"grant_types_supported":                 "[authorization_code]",
		"response_types_supported":              "[code]",
		"response_modes_supported":              "[query]",
		"code_challenge_methods_supported":      "[S256]",
		"id_token_signing_alg_values_supported": "[RS256]",
		"token_endpoint_auth_methods_supported": "[client_secret_basic client_secret_post]",
		"scopes_supported":                      "[openid email]",
	} {
		got := d[k]
		if s, ok := got.([]any); ok {
			parts := make([]string, len(s))
			for i, x := range s {
				parts[i], _ = x.(string)
			}
			got = "[" + strings.Join(parts, " ") + "]"
		}
		if got != want {
			t.Errorf("%s = %v, want %s", k, got, want)
		}
	}
	for _, k := range []string{
		"introspection_endpoint", "revocation_endpoint", "end_session_endpoint",
		"device_authorization_endpoint", "check_session_iframe", "registration_endpoint",
	} {
		if _, ok := d[k]; ok {
			t.Errorf("advertises %s", k)
		}
	}
	// Endpoints the provider doesn't offer aren't served either.
	for _, path := range []string{"/oidc/introspect", "/oidc/revoke", "/oidc/end_session", "/oidc/authorize/callback?id=x"} {
		if rec := f.get(path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", path, rec.Code)
		}
	}
	// No CORS: these endpoints are for servers.
	rec = f.get("/.well-known/openid-configuration", http.Header{"Origin": {"https://evil.example"}})
	if v := rec.Header().Get("Access-Control-Allow-Origin"); v != "" {
		t.Errorf("Access-Control-Allow-Origin: %q", v)
	}
}

func TestSignIn(t *testing.T) {
	f := newFixture(t)
	b := f.browser("ann")
	c := newRP("notes", redirectURI)
	code := b.code(t, c)
	account := f.accountOf(t, "ann")

	rec, tk := f.redeem(c, code)
	if rec.Code != http.StatusOK {
		t.Fatalf("token: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("token response Cache-Control %q", rec.Header().Get("Cache-Control"))
	}
	if tk.TokenType != "Bearer" || tk.Scope != "openid email" || tk.ExpiresIn < 590 || tk.ExpiresIn > 600 {
		t.Errorf("token response %+v", tk)
	}
	claims := f.verify(t, tk.IDToken)
	for k, want := range map[string]any{
		"iss": publicURL, "sub": account, "nonce": c.nonce, "email": "ann@example.com", "email_verified": true,
	} {
		if claims[k] != want {
			t.Errorf("%s = %v, want %v", k, claims[k], want)
		}
	}
	if aud, _ := claims["aud"].([]any); len(aud) != 1 || aud[0] != "notes" {
		if claims["aud"] != "notes" {
			t.Errorf("aud = %v", claims["aud"])
		}
	}
	exp, _ := claims["exp"].(float64)
	if left := time.Until(time.Unix(int64(exp), 0)); left < 9*time.Minute || left > 11*time.Minute {
		t.Errorf("the ID token expires in %s", left)
	}
	if at, _ := claims["auth_time"].(float64); time.Since(time.Unix(int64(at), 0)) > time.Minute {
		t.Errorf("auth_time %v", claims["auth_time"])
	}
	for _, k := range []string{"at_hash", "c_hash", "iat"} {
		if claims[k] == nil {
			t.Errorf("no %s", k)
		}
	}
	if claims["azp"] != "notes" {
		t.Errorf("azp = %v", claims["azp"])
	}

	rec = f.get(userinfoPath, http.Header{"Authorization": {"Bearer " + tk.AccessToken}})
	var info map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &info)
	if rec.Code != http.StatusOK || info["sub"] != account || info["email"] != "ann@example.com" || info["email_verified"] != true {
		t.Fatalf("userinfo: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("userinfo Cache-Control %q", rec.Header().Get("Cache-Control"))
	}
	// Not in the URL, where logs and history keep it.
	if rec := f.get(userinfoPath+"?access_token="+url.QueryEscape(tk.AccessToken), nil); rec.Code != http.StatusBadRequest {
		t.Errorf("userinfo with the token in the URL: %d %s", rec.Code, rec.Body.String())
	}
	// Signing in again keeps the subject.
	c2 := newRP("notes", redirectURI)
	_, tk2 := f.redeem(c2, b.code(t, c2))
	if f.verify(t, tk2.IDToken)["sub"] != account {
		t.Error("the subject changed")
	}
}

func TestWithoutEmailScope(t *testing.T) {
	f := newFixture(t)
	b := f.browser("ann")
	c := newRP("notes", redirectURI)
	q := b.signIn(t, c, url.Values{"scope": {"openid"}})
	_, tk := f.redeem(c, q.Get("code"))
	claims := f.verify(t, tk.IDToken)
	if _, ok := claims["email"]; ok || claims["sub"] == nil {
		t.Errorf("claims %v", claims)
	}
	rec := f.get(userinfoPath, http.Header{"Authorization": {"Bearer " + tk.AccessToken}})
	if strings.Contains(rec.Body.String(), "email") {
		t.Errorf("userinfo %s", rec.Body.String())
	}
}

func TestCodeOnce(t *testing.T) {
	f := newFixture(t)
	c := newRP("notes", redirectURI)
	code := f.browser("ann").code(t, c)
	rec, tk := f.redeem(c, code)
	if rec.Code != http.StatusOK {
		t.Fatalf("first exchange: %d", rec.Code)
	}
	rec, again := f.redeem(c, code)
	if rec.Code != http.StatusBadRequest || again.Error != "invalid_grant" {
		t.Fatalf("second exchange: %d %s", rec.Code, rec.Body.String())
	}
	// A code used twice may have leaked: what it gave is revoked.
	rec = f.get(userinfoPath, http.Header{"Authorization": {"Bearer " + tk.AccessToken}})
	if rec.Code == http.StatusOK {
		t.Fatalf("userinfo still answers after a second use: %s", rec.Body.String())
	}
}

func TestConcurrentExchange(t *testing.T) {
	f := newFixture(t)
	c := newRP("notes", redirectURI)
	code := f.browser("ann").code(t, c)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 8 {
		wg.Go(func() {
			if rec, _ := f.redeem(c, code); rec.Code == http.StatusOK {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d exchanges succeeded", ok)
	}
	// The others revoked the winner's access token.
	if n := f.count(t, `SELECT count(*) FROM oidc_access_tokens`); n != 0 {
		t.Errorf("%d access tokens left", n)
	}
}

func TestCodeChecks(t *testing.T) {
	f := newFixture(t)
	b := f.browser("ann")
	c := newRP("notes", redirectURI)
	code := b.code(t, c)
	for name, try := range map[string]func() (*httptest.ResponseRecorder, tokens){
		"no verifier": func() (*httptest.ResponseRecorder, tokens) {
			return f.exchange("notes", f.secret, url.Values{"code": {code}, "redirect_uri": {redirectURI}})
		},
		"wrong secret": func() (*httptest.ResponseRecorder, tokens) {
			return f.exchange("notes", f.other, url.Values{"code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {c.verifier}})
		},
		"no secret": func() (*httptest.ResponseRecorder, tokens) {
			return f.exchange("", "", url.Values{"client_id": {"notes"}, "code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {c.verifier}})
		},
	} {
		if rec, tk := try(); rec.Code == http.StatusOK || tk.IDToken != "" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// None of those took the code.
	if rec, _ := f.redeem(c, code); rec.Code != http.StatusOK {
		t.Fatalf("the code was taken by a refused exchange: %d %s", rec.Code, rec.Body.String())
	}
	for name, try := range map[string]func(code string) (*httptest.ResponseRecorder, tokens){
		"wrong verifier": func(code string) (*httptest.ResponseRecorder, tokens) {
			return f.exchange("notes", f.secret, url.Values{"code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {newToken()}})
		},
		"wrong redirect URI": func(code string) (*httptest.ResponseRecorder, tokens) {
			return f.exchange("notes", f.secret, url.Values{"code": {code}, "redirect_uri": {"https://notes.example.com/other"}, "code_verifier": {c.verifier}})
		},
		"another client": func(code string) (*httptest.ResponseRecorder, tokens) {
			return f.exchange("other", f.other, url.Values{"code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {c.verifier}})
		},
		"client secret post": func(code string) (*httptest.ResponseRecorder, tokens) {
			return f.exchange("", "", url.Values{"client_id": {"notes"}, "client_secret": {f.secret}, "code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {c.verifier}})
		},
		"expired": func(code string) (*httptest.ResponseRecorder, tokens) {
			h := sha256.Sum256([]byte(code))
			if _, err := f.a.DB().Exec(`UPDATE oidc_codes SET expires_at = now() WHERE code_hash = $1`, h[:]); err != nil {
				t.Fatal(err)
			}
			return f.redeem(c, code)
		},
	} {
		c.state = newToken()
		rec, tk := try(b.code(t, c))
		if name == "client secret post" {
			if rec.Code != http.StatusOK {
				t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code == http.StatusOK || tk.IDToken != "" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// Removing a redirect URI stops its codes already issued.
	c.state = newToken()
	code = b.code(t, c)
	f.mu.Lock()
	notes := f.clients["notes"]
	f.clients["notes"] = Client{ID: "notes", SecretHash: notes.SecretHash, RedirectURIs: []string{"https://notes.example.com/new"}}
	f.mu.Unlock()
	if rec, tk := f.redeem(c, code); rec.Code == http.StatusOK || tk.IDToken != "" {
		t.Errorf("a code for a removed redirect URI: %d %s", rec.Code, rec.Body.String())
	}
	f.mu.Lock()
	f.clients["notes"] = notes
	f.mu.Unlock()

	// Grants the provider doesn't offer.
	for _, grant := range []string{"refresh_token", "client_credentials", "urn:ietf:params:oauth:grant-type:jwt-bearer", "password"} {
		form := url.Values{"grant_type": {grant}, "refresh_token": {"x"}, "assertion": {"x.y.z"}}
		req := httptest.NewRequest(http.MethodPost, publicURL+tokenPath, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth("notes", f.secret)
		rec := httptest.NewRecorder()
		f.r.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "access_token") {
			t.Errorf("%s: %d %s", grant, rec.Code, rec.Body.String())
		}
	}
}

func TestAuthorizeRefusals(t *testing.T) {
	f := newFixture(t)
	b := f.browser("ann")
	// Without a valid client and redirect URI, the provider answers itself.
	for name, extra := range map[string]url.Values{
		"unknown client":       {"client_id": {"nobody"}},
		"unregistered":         {"redirect_uri": {"https://evil.example/callback"}},
		"http, not loopback":   {"redirect_uri": {"http://notes.example.com/insecure"}},
		"no redirect URI":      {"redirect_uri": {""}},
		"another client's URI": {"redirect_uri": {"https://other.example.com/callback"}},
	} {
		rec := b.get(newRP("notes", redirectURI).authorizeURL(extra))
		if rec.Code >= 300 && rec.Code < 400 {
			t.Errorf("%s: redirected to %s", name, rec.Header().Get("Location"))
		}
	}
	// With them, it answers the client.
	for name, want := range map[string]struct {
		extra url.Values
		err   string
	}{
		"no PKCE":        {url.Values{"code_challenge": {""}, "code_challenge_method": {""}}, "invalid_request"},
		"plain PKCE":     {url.Values{"code_challenge_method": {"plain"}}, "invalid_request"},
		"prompt none":    {url.Values{"prompt": {"none"}}, "login_required"},
		"no openid":      {url.Values{"scope": {"email"}}, "invalid_scope"},
		"fragment":       {url.Values{"response_mode": {"fragment"}}, "invalid_request"},
		"implicit":       {url.Values{"response_type": {"id_token"}}, "unsupported_response_type"},
		"request_uri":    {url.Values{"request_uri": {"https://evil.example/r"}}, "request_uri_not_supported"},
		"repeated":       {url.Values{"nonce": {"a", "b"}}, "invalid_request"},
		"long state":     {url.Values{"state": {strings.Repeat("s", maxState+1)}}, "invalid_request"},
		"request object": {url.Values{"request": {"eyJhbGciOiJub25lIn0.e30."}}, "request_not_supported"},
	} {
		c := newRP("notes", redirectURI)
		u := b.authorize(t, c, want.extra)
		// A request for the fragment, as implicit requests, hears back in the fragment.
		q := u.Query()
		if u.Fragment != "" {
			q, _ = url.ParseQuery(u.Fragment)
		}
		u.RawQuery, u.Fragment = "", ""
		state := c.state
		if s := want.extra.Get("state"); s != "" {
			state = s
		}
		if u.String() != redirectURI || q.Get("error") != want.err || q.Get("state") != state || q.Get("code") != "" {
			t.Errorf("%s: sent to %s with %v, want error %s", name, u, q, want.err)
		}
	}
}

func TestContinue(t *testing.T) {
	f := newFixture(t)
	c := newRP("notes", redirectURI)

	// Signed out: through the sign-in pages, then back.
	b := f.browser("")
	cont := b.authorize(t, c, nil)
	u := location(t, b.get(cont.String()))
	if u.Path != "/login" || u.Query().Get("next") != cont.RequestURI() {
		t.Fatalf("signed out: sent to %s", u)
	}
	b.cookies["session"] = &http.Cookie{Name: "session", Value: "ann"}
	if u := location(t, b.get(cont.String())); u.Query().Get("code") == "" {
		t.Fatalf("after signing in: sent to %s", u)
	}

	// Another browser can't continue someone else's request.
	cont = f.browser("ann").authorize(t, c, nil)
	victim := f.browser("bob")
	if rec := victim.get(cont.String()); rec.Code != http.StatusBadRequest {
		t.Fatalf("another browser: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	victim.get(c.authorizeURL(nil)) // the victim's own browser value
	if rec := victim.get(cont.String()); rec.Code != http.StatusBadRequest {
		t.Fatalf("another browser with its own value: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	b = f.browser("ann")
	cont = b.authorize(t, c, nil)
	q := cont.Query()
	id := q.Get("id")
	q.Set("id", id[:len(id)-4]+"AAAA")
	if rec := b.get(continuePath + "?" + q.Encode()); rec.Code != http.StatusBadRequest {
		t.Errorf("changed request: %d", rec.Code)
	}
	if rec := b.get(continuePath); rec.Code != http.StatusBadRequest {
		t.Errorf("no request: %d", rec.Code)
	}
	// The client dropped the redirect URI meanwhile.
	f.mu.Lock()
	notes := f.clients["notes"]
	f.clients["notes"] = Client{ID: "notes", SecretHash: notes.SecretHash, RedirectURIs: []string{"https://notes.example.com/new"}}
	f.mu.Unlock()
	if rec := b.get(cont.String()); rec.Code != http.StatusBadRequest {
		t.Errorf("dropped redirect URI: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	f.mu.Lock()
	f.clients["notes"] = notes
	f.mu.Unlock()
	// The request still works, once.
	if u := location(t, b.get(cont.String())); u.Query().Get("code") == "" {
		t.Fatalf("sent to %s", u)
	}
}

func TestAllow(t *testing.T) {
	var asked []string
	f := newFixture(t, func(cfg *Config) {
		cfg.Allow = func(_ context.Context, clientID string, a accounts.Account) error {
			asked = append(asked, clientID+" "+a.Email)
			if a.Email == "bob@example.com" {
				return errors.New("not invited")
			}
			return nil
		}
	})
	c := newRP("notes", redirectURI)
	cont := f.browser("bob").authorize(t, c, nil)
	_ = cont
	b := f.browser("bob")
	u := location(t, b.get(location(t, b.get(c.authorizeURL(nil))).String()))
	if u.Query().Get("error") != "access_denied" || u.Query().Get("state") != c.state || u.Query().Get("code") != "" {
		t.Fatalf("refused account: sent to %s", u)
	}
	if code := f.browser("ann").code(t, c); code == "" {
		t.Fatal("allowed account got no code")
	}
	if strings.Join(asked, ",") != "notes bob@example.com,notes ann@example.com" {
		t.Errorf("asked %v", asked)
	}
}

func TestMaxAge(t *testing.T) {
	f := newFixture(t, func(cfg *Config) {
		cfg.Reauthenticate = func(next string) string { return "/login/confirm?next=" + url.QueryEscape(next) }
	})
	c := newRP("notes", redirectURI)
	b := f.browser("ann")
	hourAgo := time.Now().Add(-time.Hour).Unix()
	b.cookies["signed-in"] = &http.Cookie{Name: "signed-in", Value: strconv.FormatInt(hourAgo, 10)}

	// A recent enough sign-in passes.
	if q := b.signIn(t, c, url.Values{"max_age": {"7200"}}); q.Get("code") == "" {
		t.Fatalf("max_age 7200: %v", q)
	}
	for _, extra := range []url.Values{{"max_age": {"60"}}, {"prompt": {"login"}}} {
		cont := b.authorize(t, c, extra)
		u := location(t, b.get(cont.String()))
		if u.Path != "/login/confirm" {
			t.Fatalf("%v: sent to %s", extra, u)
		}
		next, _ := url.Parse(u.Query().Get("next"))
		if next.Path != continuePath || next.Query().Get("again") != "1" {
			t.Fatalf("%v: next %s", extra, next)
		}
		// Still not signed in again: the client hears it.
		if back := location(t, b.get(next.String())); back.Query().Get("error") != "login_required" {
			t.Fatalf("%v: still old: sent to %s", extra, back)
		}
		// Signed in again: a code, and the new auth_time.
		b.cookies["signed-in"].Value = strconv.FormatInt(time.Now().Unix(), 10)
		back := location(t, b.get(next.String()))
		if back.Query().Get("code") == "" {
			t.Fatalf("%v: after signing in again: sent to %s", extra, back)
		}
		_, tk := f.redeem(c, back.Query().Get("code"))
		if at, _ := f.verify(t, tk.IDToken)["auth_time"].(float64); time.Since(time.Unix(int64(at), 0)) > time.Minute {
			t.Errorf("%v: auth_time %v", extra, at)
		}
		b.cookies["signed-in"].Value = strconv.FormatInt(hourAgo, 10)
	}

	// prompt=login wants a sign-in after the request: one made seconds before it is sent
	// to sign in again too.
	b.cookies["signed-in"].Value = strconv.FormatInt(time.Now().Add(-10*time.Second).Unix(), 10)
	if u := location(t, b.get(b.authorize(t, c, url.Values{"prompt": {"login"}}).String())); u.Path != "/login/confirm" {
		t.Fatalf("a sign-in just before the request: sent to %s", u)
	}

	// Without a way to sign in again, the client hears login_required.
	f2 := newFixture(t)
	b2 := f2.browser("ann")
	b2.cookies["signed-in"] = &http.Cookie{Name: "signed-in", Value: strconv.FormatInt(hourAgo, 10)}
	if q := b2.signIn(t, c, url.Values{"prompt": {"login"}}); q.Get("error") != "login_required" {
		t.Fatalf("no Reauthenticate: %v", q)
	}
}

func TestIDTokenHint(t *testing.T) {
	f := newFixture(t)
	c := newRP("notes", redirectURI)
	_, bob := f.redeem(c, f.browser("bob").code(t, c))
	c = newRP("notes", redirectURI)
	b := f.browser("ann")
	if q := b.signIn(t, c, url.Values{"id_token_hint": {bob.IDToken}}); q.Get("error") != "login_required" {
		t.Fatalf("hint for another account: %v", q)
	}
	_, ann := f.redeem(c, f.browser("ann").code(t, c))
	if q := b.signIn(t, c, url.Values{"id_token_hint": {ann.IDToken}}); q.Get("code") == "" {
		t.Fatalf("hint for the same account: %v", q)
	}
	// A hint the provider issued to another client isn't this client's to use.
	o := newRP("other", "https://other.example.com/callback")
	_, annOther := f.exchange("other", f.other, url.Values{"code": {b.code(t, o)}, "redirect_uri": {o.redirect}, "code_verifier": {o.verifier}})
	if annOther.IDToken == "" {
		t.Fatal("no ID token for the other client")
	}
	if u := b.authorize(t, c, url.Values{"id_token_hint": {annOther.IDToken}}); u.Query().Get("error") != "invalid_request" {
		t.Fatalf("hint from another client: sent to %s", u)
	}
	if u := b.authorize(t, c, url.Values{"id_token_hint": {"not.a.token"}}); u.Query().Get("error") != "login_required" {
		t.Fatalf("broken hint: sent to %s", u)
	}
}

func TestKeys(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := newRP("notes", redirectURI)
	signIn := func() string {
		t.Helper()
		c.state = newToken()
		_, tk := f.redeem(c, f.browser("ann").code(t, c))
		return f.verify(t, tk.IDToken)["kid"].(string)
	}
	first := signIn()
	if n := f.count(t, `SELECT count(*) FROM oidc_keys`); n != 1 {
		t.Fatalf("%d keys after the first sign-in", n)
	}
	// The private key is sealed: the database holds no PKCS#8 key.
	var sealed []byte
	if err := f.a.DB().QueryRow(`SELECT private_key FROM oidc_keys`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "\x02\x01\x00\x30") {
		t.Error("the private key looks unsealed")
	}
	shift := func(d time.Duration) {
		t.Helper()
		if _, err := f.a.DB().Exec(`
			UPDATE oidc_keys
			SET signs_from = signs_from - make_interval(secs => $1),
			    signs_until = signs_until - make_interval(secs => $1)`, d.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	// A day and a half before the key's end, the next one is published.
	shift(keyLifetime - 36*time.Hour)
	if err := f.p.maintain(ctx); err != nil {
		t.Fatal(err)
	}
	keys, err := f.p.keySet(ctx)
	if err != nil || len(keys) != 2 {
		t.Fatalf("%d keys published, %v", len(keys), err)
	}
	if signIn() != first {
		t.Fatal("the next key signed before its turn")
	}
	// Its turn: it signs, and the old one stays published for a day.
	shift(36 * time.Hour)
	second := signIn()
	if second == first {
		t.Fatal("the old key still signs")
	}
	if keys, _ := f.p.keySet(ctx); len(keys) < 2 {
		t.Fatalf("%d keys published after the turn", len(keys))
	}
	shift(keyOverlap + time.Minute)
	if err := f.p.maintain(ctx); err != nil {
		t.Fatal(err)
	}
	var ids []string
	keys, _ = f.p.keySet(ctx)
	for _, k := range keys {
		ids = append(ids, k.ID())
	}
	if strings.Contains(strings.Join(ids, ","), first) {
		t.Errorf("the old key is still published: %v", ids)
	}
}

func TestSecretKeyChange(t *testing.T) {
	f := newFixture(t)
	c := newRP("notes", redirectURI)
	_, tk := f.redeem(c, f.browser("ann").code(t, c))
	before := f.verify(t, tk.IDToken)["kid"].(string)
	pending := f.browser("ann")
	cont := pending.authorize(t, c, nil)

	f.mount(Config{Issuer: publicURL, SecretKey: strings.Repeat("n", 32), Client: f.client})
	// Requests sealed with the old key end.
	if rec := pending.get(cont.String()); rec.Code != http.StatusBadRequest {
		t.Errorf("old request: %d", rec.Code)
	}
	c.state = newToken()
	_, tk = f.redeem(c, f.browser("ann").code(t, c))
	after := f.verify(t, tk.IDToken)["kid"].(string)
	if after == before {
		t.Fatal("a key sealed with the old secret key still signs")
	}
	// The old key stays published, for tokens it signed.
	keys, _ := f.p.keySet(context.Background())
	if len(keys) != 2 {
		t.Errorf("%d keys published", len(keys))
	}
}

func TestDeletionAndMaintenance(t *testing.T) {
	f := newFixture(t)
	c := newRP("notes", redirectURI)
	_, _ = f.redeem(c, f.browser("ann").code(t, c))
	c.state = newToken()
	_ = f.browser("bob").code(t, c)
	if _, err := f.a.DB().Exec(`DELETE FROM accounts WHERE id = $1`, f.accountOf(t, "ann")); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM oidc_access_tokens`); n != 0 {
		t.Errorf("%d access tokens left after the account's deletion", n)
	}
	if n := f.count(t, `SELECT count(*) FROM oidc_codes`); n != 1 {
		t.Errorf("%d codes, want bob's", n)
	}
	if _, err := f.a.DB().Exec(`UPDATE oidc_codes SET expires_at = now() - interval '11 minutes'`); err != nil {
		t.Fatal(err)
	}
	if err := f.p.maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM oidc_codes`); n != 0 {
		t.Errorf("%d expired codes left", n)
	}
}

// TestUserinfoSubject checks userinfo against the stored token's account, not only the
// subject the token carries: a token that names another subject is refused.
func TestUserinfoSubject(t *testing.T) {
	f := newFixture(t)
	c := newRP("notes", redirectURI)
	_, _ = f.redeem(c, f.browser("ann").code(t, c))
	ann, bob := f.accountOf(t, "ann"), ""
	c.state = newToken()
	_ = f.browser("bob").code(t, c)
	bob = f.accountOf(t, "bob")
	id := newToken()
	h := sha256.Sum256([]byte(id))
	if _, err := f.a.DB().Exec(`
		INSERT INTO oidc_access_tokens (token_hash, code_hash, account_id, client_id, scopes, email, email_verified, name, expires_at)
		VALUES ($1, $2, $3, 'notes', '{openid,email}', 'ann@example.com', true, '', now() + interval '5 minutes')`,
		h[:], []byte("x"), ann); err != nil {
		t.Fatal(err)
	}
	for subject, want := range map[string]int{ann: http.StatusOK, bob: http.StatusForbidden} {
		token, err := f.p.op.Crypto().Encrypt(id + ":" + subject)
		if err != nil {
			t.Fatal(err)
		}
		if rec := f.get(userinfoPath, http.Header{"Authorization": {"Bearer " + token}}); rec.Code != want {
			t.Errorf("token naming %s: %d %s, want %d", subject, rec.Code, rec.Body.String(), want)
		}
	}
}

// TestReuseDuringExchange: a second use that arrives while the first exchange is between
// taking the code and storing its access token stops that token.
func TestReuseDuringExchange(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := newRP("notes", redirectURI)
	code := f.browser("ann").code(t, c)
	s := &storage{p: f.p}
	g, err := s.AuthRequestByCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthRequestByCode(ctx, code); err == nil {
		t.Fatal("the code was taken twice")
	}
	if _, _, err := s.CreateAccessToken(ctx, g); err == nil {
		t.Fatal("an access token was stored for a code used twice")
	}
}

// TestLogs: nothing a client or a person sent reaches the logs: not the client's state,
// codes, tokens, the client's secret, or addresses.
func TestLogs(t *testing.T) {
	var buf safeBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	f := newFixture(t)
	b := f.browser("ann")
	const sentinel = "STATE-SENTINEL-42"
	var secrets []string
	// Refused requests of every kind, then a sign-in and a reused code.
	for _, extra := range []url.Values{
		{"code_challenge": {""}, "state": {sentinel}},
		{"prompt": {"none"}, "state": {sentinel}},
		{"response_type": {"token"}, "state": {sentinel}},
		{"request": {"x.y.z"}, "state": {sentinel}},
		{"redirect_uri": {"https://evil.example/cb"}, "state": {sentinel}},
	} {
		b.get(newRP("notes", redirectURI).authorizeURL(extra))
	}
	c := newRP("notes", redirectURI)
	code := b.code(t, c)
	_, tk := f.redeem(c, code)
	f.redeem(c, code)
	f.exchange("notes", "wrong-"+f.secret, url.Values{"code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {c.verifier}})
	f.get(userinfoPath, http.Header{"Authorization": {"Bearer " + tk.AccessToken + "x"}})
	secrets = append(secrets, sentinel, code, tk.AccessToken, tk.IDToken, f.secret, c.verifier, c.state, c.nonce, "ann@example.com")
	logs := buf.String()
	if !strings.Contains(logs, "oidc") {
		t.Fatalf("nothing was logged; the test isn't seeing the logs:\n%s", logs)
	}
	for _, s := range secrets {
		if strings.Contains(logs, s) {
			t.Errorf("the logs contain %q:\n%s", s, logs)
		}
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestNames: with names on, the profile scope gives the account's name; an account
// without one gives it first.
func TestNames(t *testing.T) {
	f := newFixtureWith(t, accounts.Config{Names: true})
	var d map[string]any
	_ = json.Unmarshal(f.get("/.well-known/openid-configuration", nil).Body.Bytes(), &d)
	if fmt.Sprint(d["scopes_supported"]) != "[openid email profile]" || !strings.Contains(fmt.Sprint(d["claims_supported"]), "name") {
		t.Errorf("discovery: scopes %v, claims %v", d["scopes_supported"], d["claims_supported"])
	}
	b := f.browser("ann")
	c := newRP("notes", redirectURI)
	if u := location(t, b.get(b.authorize(t, c, nil).String())); u.Path != "/account/name" {
		t.Fatalf("without a name: sent to %s", u)
	}
	if _, err := f.a.DB().Exec(`UPDATE accounts SET name = 'Ann Example'`); err != nil {
		t.Fatal(err)
	}
	_, tk := f.redeem(c, b.code(t, c))
	if tk.Scope != "openid email profile" || f.verify(t, tk.IDToken)["name"] != "Ann Example" {
		t.Fatalf("scope %q, claims %v", tk.Scope, f.verify(t, tk.IDToken))
	}
	var info map[string]any
	_ = json.Unmarshal(f.get(userinfoPath, http.Header{"Authorization": {"Bearer " + tk.AccessToken}}).Body.Bytes(), &info)
	if info["name"] != "Ann Example" {
		t.Errorf("userinfo %v", info)
	}
	// Without the profile scope, no name.
	c = newRP("notes", redirectURI)
	q := b.signIn(t, c, url.Values{"scope": {"openid email"}})
	_, tk = f.redeem(c, q.Get("code"))
	if _, ok := f.verify(t, tk.IDToken)["name"]; ok {
		t.Error("a name without the profile scope")
	}
	// With names off, profile isn't granted, whatever the database holds.
	off := newFixture(t)
	if _, err := off.a.DB().Exec(`UPDATE accounts SET name = 'Ann Example'`); err != nil {
		t.Fatal(err)
	}
	c = newRP("notes", redirectURI)
	_, tk = off.redeem(c, off.browser("ann").code(t, c))
	if tk.Scope != "openid email" {
		t.Errorf("names off: scope %q", tk.Scope)
	}
	if _, ok := off.verify(t, tk.IDToken)["name"]; ok {
		t.Error("names off: a name")
	}
}
