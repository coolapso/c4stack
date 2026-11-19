package oidc

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseIssuer(t *testing.T) {
	for raw, want := range map[string]string{
		"https://account.example.com":  "https://account.example.com",
		"https://account.example.com/": "https://account.example.com",
		"http://localhost:8080":        "http://localhost:8080",
		"http://127.0.0.1:8080":        "http://127.0.0.1:8080",
		"http://[::1]:8080":            "http://[::1]:8080",
	} {
		got, _, err := parseIssuer(raw)
		if err != nil || got != want {
			t.Errorf("parseIssuer(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"", "account.example.com", "http://account.example.com", "https://account.example.com/auth",
		"https://account.example.com?x=1", "https://account.example.com#x", "https://u:p@account.example.com",
		"ftp://account.example.com",
	} {
		if _, _, err := parseIssuer(raw); err == nil {
			t.Errorf("parseIssuer(%q) accepted", raw)
		}
	}
}

func TestValidRedirectURI(t *testing.T) {
	for _, ok := range []string{
		"https://notes.example.com/api/v1/auth/oidc/callback",
		"https://notes.example.com/cb?tenant=1",
		"http://localhost:8080/callback",
		"http://127.0.0.1/callback",
	} {
		if !validRedirectURI(ok) {
			t.Errorf("refused %q", ok)
		}
	}
	for _, bad := range []string{
		"http://notes.example.com/callback", "https://notes.example.com/cb#frag", "https://notes.example.com/cb#",
		"/callback", "notes://callback", "https://user@notes.example.com/cb", "",
	} {
		if validRedirectURI(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestSecrets(t *testing.T) {
	secret, hash := NewSecret()
	other, _ := NewSecret()
	if len(secret) < 43 || secret == other {
		t.Fatalf("secrets %q and %q", secret, other)
	}
	c := Client{ID: "notes", SecretHash: hash}
	if !secretMatches(c, secret) {
		t.Fatal("the secret doesn't match its hash")
	}
	for _, wrong := range []string{"", other, secret + "x"} {
		if secretMatches(c, wrong) {
			t.Errorf("%q matches", wrong)
		}
	}
	if secretMatches(Client{ID: "notes"}, "") {
		t.Error("an empty secret matches a client without a hash")
	}
}

func TestSeal(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	sealed := seal(key, "a", []byte("hello"))
	if plain, err := open(key, "a", sealed); err != nil || string(plain) != "hello" {
		t.Fatalf("open = %q, %v", plain, err)
	}
	if _, err := open(key, "b", sealed); err == nil {
		t.Error("opened under another label")
	}
	if _, err := open([]byte(strings.Repeat("x", 32)), "a", sealed); err == nil {
		t.Error("opened with another key")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := open(key, "a", sealed); err == nil {
		t.Error("opened a changed value")
	}
}

func TestValidChallenge(t *testing.T) {
	sum := sha256.Sum256([]byte("verifier"))
	good := base64.RawURLEncoding.EncodeToString(sum[:])
	if !validChallenge(good) {
		t.Fatal("refused an S256 challenge")
	}
	for _, bad := range []string{"", "verifier", good[:42], good + "A", strings.ReplaceAll(good, good[:1], "+")} {
		if bad != good && validChallenge(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func testProvider() *Provider {
	return &Provider{requestKey: []byte(strings.Repeat("r", 32))}
}

func authQuery() url.Values {
	sum := sha256.Sum256([]byte("verifier"))
	return url.Values{
		"client_id":             {"notes"},
		"redirect_uri":          {"https://notes.example.com/callback"},
		"scope":                 {"openid email profile offline_access"},
		"response_type":         {"code"},
		"state":                 {"state"},
		"nonce":                 {"nonce"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
}

func TestParseRequest(t *testing.T) {
	r, e := parseRequest(authQuery())
	if e != nil {
		t.Fatal(e)
	}
	// Only the scopes the provider grants are kept.
	if strings.Join(r.Scopes, " ") != "openid email" || r.State != "state" || r.Nonce != "nonce" || r.MaxAge != nil {
		t.Errorf("request %+v", r)
	}
	for name, want := range map[string]struct {
		change func(url.Values)
		err    string
	}{
		"repeated":      {func(q url.Values) { q.Add("scope", "openid") }, "invalid_request"},
		"request":       {func(q url.Values) { q.Set("request", "x.y.z") }, "request_not_supported"},
		"request_uri":   {func(q url.Values) { q.Set("request_uri", "https://x.example") }, "request_uri_not_supported"},
		"no openid":     {func(q url.Values) { q.Set("scope", "email") }, "invalid_scope"},
		"implicit":      {func(q url.Values) { q.Set("response_type", "id_token") }, "unsupported_response_type"},
		"hybrid":        {func(q url.Values) { q.Set("response_type", "code id_token") }, "unsupported_response_type"},
		"fragment":      {func(q url.Values) { q.Set("response_mode", "fragment") }, "invalid_request"},
		"form post":     {func(q url.Values) { q.Set("response_mode", "form_post") }, "invalid_request"},
		"prompt none":   {func(q url.Values) { q.Set("prompt", "none") }, "login_required"},
		"none and more": {func(q url.Values) { q.Set("prompt", "none login") }, "invalid_request"},
		"bad max_age":   {func(q url.Values) { q.Set("max_age", "-1") }, "invalid_request"},
		"no PKCE":       {func(q url.Values) { q.Del("code_challenge"); q.Del("code_challenge_method") }, "invalid_request"},
		"plain PKCE":    {func(q url.Values) { q.Set("code_challenge_method", "plain") }, "invalid_request"},
		"bad challenge": {func(q url.Values) { q.Set("code_challenge", "short") }, "invalid_request"},
		"long state":    {func(q url.Values) { q.Set("state", strings.Repeat("s", maxState+1)) }, "invalid_request"},
		"long nonce":    {func(q url.Values) { q.Set("nonce", strings.Repeat("n", maxNonce+1)) }, "invalid_request"},
	} {
		q := authQuery()
		want.change(q)
		if _, e := parseRequest(q); e == nil || string(e.ErrorType) != want.err {
			t.Errorf("%s: %v, want %s", name, e, want.err)
		}
	}
	q := authQuery()
	q.Set("max_age", "300")
	if r, _ := parseRequest(q); r == nil || r.MaxAge == nil || *r.MaxAge != 300 {
		t.Errorf("max_age: %+v", r)
	}
	q.Set("prompt", "login consent")
	if r, _ := parseRequest(q); r == nil || r.MaxAge == nil || *r.MaxAge != 0 {
		t.Errorf("prompt=login: %+v", r)
	}
}

func testRequest(t *testing.T) *request {
	t.Helper()
	r, e := parseRequest(authQuery())
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256([]byte("browser"))
	r.ClientID, r.RedirectURI, r.Browser, r.Created = "notes", "https://notes.example.com/callback", h[:], time.Now().UnixMilli()
	r.Hint = "subject"
	return r
}

func TestRequestSealing(t *testing.T) {
	p := testProvider()
	r := testRequest(t)
	id, err := p.sealRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.openRequest(id)
	if err != nil || got.ClientID != "notes" || got.Hint != "subject" || got.State != "state" {
		t.Fatalf("openRequest = %+v, %v", got, err)
	}
	other := &Provider{requestKey: []byte(strings.Repeat("o", 32))}
	if _, err := other.openRequest(id); err == nil {
		t.Error("another secret key opened the request")
	}
	if _, err := p.openRequest(id[:len(id)-2]); err == nil {
		t.Error("opened a cut request")
	}
	for _, age := range []time.Duration{requestLifetime + time.Second, -2 * time.Minute} {
		old := *r
		old.Created = time.Now().Add(-age).UnixMilli()
		id, _ := p.sealRequest(&old)
		if _, err := p.openRequest(id); err == nil {
			t.Errorf("opened a request made %s ago", age)
		}
	}
	// A request sealed for something else, such as a signing key, doesn't open.
	plain, _ := json.Marshal(r)
	foreign := base64.RawURLEncoding.EncodeToString(seal(p.requestKey, keyLabel("x"), plain))
	if _, err := p.openRequest(foreign); err == nil {
		t.Error("opened a value sealed under another label")
	}
}

func TestSignedInSince(t *testing.T) {
	created := time.Now().Add(-25 * time.Minute)
	zero, minute := uint(0), uint(60)
	r := &request{Created: created.UnixMilli(), MaxAge: &zero}
	near := func(got, want time.Time) bool { return got.Sub(want).Abs() < time.Second }
	// prompt=login: a sign-in after the request began; the clocks' allowance only once
	// the person was sent to sign in again.
	if got := signedInSince(r, false); !near(got, created) {
		t.Errorf("max age 0: %s, want the request's start", got)
	}
	if got := signedInSince(r, true); !near(got, created.Add(-clockSkew)) {
		t.Errorf("max age 0, again: %s", got)
	}
	// A positive max age counts back from now, however old the request.
	r.MaxAge = &minute
	if got := signedInSince(r, false); !near(got, time.Now().Add(-time.Minute)) {
		t.Errorf("max age 60 on an old request: %s ago", time.Since(got))
	}
	if got := signedInSince(r, true); !near(got, time.Now().Add(-time.Minute-clockSkew)) {
		t.Errorf("max age 60, again: %s ago", time.Since(got))
	}
}
