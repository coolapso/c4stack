package hanko

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

func newHanko(t *testing.T, f *fakeHanko) *Hanko {
	t.Helper()
	h, err := New(Config{APIURL: f.url(), SignedInPath: "/account"})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func withSession(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if token != "" {
		r.AddCookie(&http.Cookie{Name: "hanko", Value: token})
	}
	return r
}

func TestNewValidatesConfig(t *testing.T) {
	for _, api := range []string{"", "tenant.hanko.io", "http://tenant.hanko.io", "https://u:p@tenant.hanko.io", "https://tenant.hanko.io?x=1"} {
		if _, err := New(Config{APIURL: api}); err == nil {
			t.Errorf("New(%q) succeeded", api)
		}
	}
	if _, err := New(Config{APIURL: "https://tenant.hanko.io", SignedInPath: "//evil.example"}); err == nil {
		t.Error("accepted a signed-in path on another site")
	}
	if _, err := New(Config{APIURL: "http://localhost:8000/hanko/"}); err != nil {
		t.Error(err)
	}
}

func TestAuthenticate(t *testing.T) {
	f := newFakeHanko(t, "")
	h := newHanko(t, f)

	id, err := h.Authenticate(withSession("valid-token"))
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "user-1" || id.Email != "ada@example.com" || !id.EmailVerified || !id.Recent(time.Now()) {
		t.Fatalf("identity %+v", id)
	}
	for _, token := range []string{"", "expired-token"} {
		if _, err := h.Authenticate(withSession(token)); !errors.Is(err, identity.ErrNoSession) {
			t.Errorf("token %q: %v", token, err)
		}
	}

	f.srv.Close()
	if _, err := h.Authenticate(withSession("valid-token")); err == nil || errors.Is(err, identity.ErrNoSession) {
		t.Fatalf("unreachable Hanko: %v", err)
	}
}

func TestAuthenticateKeepsResponseBodiesOutOfErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "secret-internal-detail")
	}))
	defer srv.Close()
	h, _ := New(Config{APIURL: srv.URL})
	_, err := h.Authenticate(withSession("valid-token"))
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error %v", err)
	}
}

func TestLogoutClearsTheCookieEvenWhenHankoFails(t *testing.T) {
	f := newFakeHanko(t, "")
	h := newHanko(t, f)

	rec := httptest.NewRecorder()
	if err := h.Logout(rec, withSession("valid-token")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "hanko=;") || f.sessions["valid-token"] != "" {
		t.Fatalf("cookie %q, sessions %v", rec.Header().Get("Set-Cookie"), f.sessions)
	}

	f.sessions["other-token"] = "user-1"
	f.logoutErr = true
	rec = httptest.NewRecorder()
	if err := h.Logout(rec, withSession("other-token")); err == nil {
		t.Fatal("a failed remote logout reported success")
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "hanko=;") {
		t.Fatal("cookie kept after a failed remote logout")
	}
}

func TestDelete(t *testing.T) {
	f := newFakeHanko(t, "/hanko")
	h := newHanko(t, f)
	if err := h.Delete(context.Background(), withSession("valid-token")); err != nil {
		t.Fatal(err)
	}
	if !f.deleted {
		t.Fatal("Hanko did not delete the user")
	}
	if err := h.Delete(context.Background(), withSession("valid-token")); err == nil {
		t.Fatal("deleting with a dead session succeeded")
	}

	g := newFakeHanko(t, "")
	h = newHanko(t, g)
	g.failNext["account_delete"] = http.StatusInternalServerError
	if err := h.Delete(context.Background(), withSession("valid-token")); err == nil || g.deleted {
		t.Fatalf("failed deletion: %v (deleted %v)", err, g.deleted)
	}
}

func TestFlowURLStaysOnHanko(t *testing.T) {
	h, _ := New(Config{APIURL: "https://auth.example/hanko"})
	cases := map[string]string{
		"/profile?action=x@1":                    "https://auth.example/hanko/profile?action=x@1",
		"/hanko/profile?action=x@1":              "https://auth.example/hanko/profile?action=x@1",
		"https://auth.example/hanko/profile?a=1": "https://auth.example/hanko/profile?a=1",
	}
	for href, want := range cases {
		if got, err := h.flowURL(href); err != nil || got != want {
			t.Errorf("flowURL(%q) = %q, %v", href, got, err)
		}
	}
	for _, href := range []string{"https://evil.example/profile", "//evil.example/profile", "http://auth.example/profile", "https://user@auth.example/x"} {
		if _, err := h.flowURL(href); err == nil {
			t.Errorf("flowURL(%q) accepted another origin", href)
		}
	}
}

// app serves the sign-in page and the profile UI the way accounts mounts them: the
// profile routes sit behind a middleware that puts the identity in the context.
type app struct {
	srv    *httptest.Server
	client *http.Client
	email  string
}

func newApp(t *testing.T, f *fakeHanko) *app {
	t.Helper()
	h := newHanko(t, f)
	a := &app{}
	a.srv = httptest.NewUnstartedServer(nil)
	public := "http://" + a.srv.Listener.Addr().String()
	r, err := web.New(web.Config{PublicURL: public, SecretKey: strings.Repeat("s", 32)})
	if err != nil {
		t.Fatal(err)
	}
	assets := ui.NewAssets("/assets")
	layout := ui.Default(ui.Design{Name: "Acme"}, assets)
	r.GET("/assets/*path", assets.Serve)
	h.Mount(r, layout)
	signedIn := r.Group("/account/settings", func(c *gin.Context) {
		id, err := h.Authenticate(c.Request)
		if err != nil {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Request = c.Request.WithContext(identity.NewContext(c.Request.Context(), id))
	})
	h.MountProfile(signedIn, layout, "/logout", "/account/email-changed", func(c *gin.Context, email string) error {
		a.email = email
		return nil
	})
	signedIn.GET("", func(c *gin.Context) { layout.Render(c, http.StatusOK, "Settings", h.ProfileSection(c)) })
	a.srv.Config.Handler = r
	a.srv.Start()
	t.Cleanup(a.srv.Close)

	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(public)
	jar.SetCookies(u, []*http.Cookie{{Name: "hanko", Value: "valid-token", Path: "/"}})
	a.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return a
}

func (a *app) get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	resp, err := a.client.Get(a.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func (a *app) post(t *testing.T, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, a.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestLoginPage(t *testing.T) {
	f := newFakeHanko(t, "")
	a := newApp(t, f)

	resp, _ := a.get(t, "/login?next=/account/keys")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account/keys" {
		t.Fatalf("signed-in visit: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	delete(f.sessions, "valid-token")
	resp, body := a.get(t, "/login?next=//evil.example")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{`<hanko-auth id="hanko-auth"`, `data-api-url="` + f.url() + `"`, `data-next="/account"`} {
		if !strings.Contains(body, want) {
			t.Errorf("login page lacks %q", want)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self' "+f.srv.URL) || !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
		t.Errorf("CSP %q", csp)
	}
}

func TestProfileSection(t *testing.T) {
	f := newFakeHanko(t, "")
	a := newApp(t, f)
	resp, body := a.get(t, "/account/settings")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{"ada@example.com", `data-recent="true"`, `data-authorize="/account/settings/passkeys/authorize"`, `hx-post="/account/settings/email"`, `action="/logout"`, `value="/login?next=%2Faccount%2Fsettings"`} {
		if !strings.Contains(body, want) {
			t.Errorf("profile section lacks %q", want)
		}
	}
}

func TestPasskeyChangesNeedARecentSignIn(t *testing.T) {
	f := newFakeHanko(t, "")
	a := newApp(t, f)
	if resp, _ := a.post(t, "/account/settings/passkeys/authorize", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("recent session: %d", resp.StatusCode)
	}
	f.issuedAt = time.Now().Add(-identity.RecentSignIn - time.Minute)
	if resp, _ := a.post(t, "/account/settings/passkeys/authorize", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session: %d", resp.StatusCode)
	}
}

func TestEmailChange(t *testing.T) {
	f := newFakeHanko(t, "")
	a := newApp(t, f)

	if resp, body := a.post(t, "/account/settings/email", url.Values{"email": {"ADA@example.com"}}); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "already your email") {
		t.Fatalf("same address: %d %s", resp.StatusCode, body)
	}
	if resp, body := a.post(t, "/account/settings/email", url.Values{"email": {"taken@example.com"}}); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "already in use") {
		t.Fatalf("taken address: %d %s", resp.StatusCode, body)
	}

	resp, body := a.post(t, "/account/settings/email", url.Values{"email": {"new@example.com"}})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("HX-Retarget") != "#profile-modal" || !strings.Contains(body, "Verify your new email") {
		t.Fatalf("start: %d %q %s", resp.StatusCode, resp.Header.Get("HX-Retarget"), body)
	}
	if _, body := a.post(t, "/account/settings/email/verify", url.Values{"code": {"000000"}}); !strings.Contains(body, "That code is not right") {
		t.Fatalf("wrong code: %s", body)
	}
	if _, body := a.post(t, "/account/settings/email/resend", nil); !strings.Contains(body, "We sent a new code") {
		t.Fatalf("resend: %s", body)
	}

	// The old address cannot be removed on the first try; the retry continues.
	f.failNext["email_delete"] = http.StatusInternalServerError
	if _, body := a.post(t, "/account/settings/email/verify", url.Values{"code": {"123456"}}); !strings.Contains(body, "a final step did not finish") {
		t.Fatalf("partial cleanup: %s", body)
	}
	if a.email != "new@example.com" {
		t.Fatalf("local email %q", a.email)
	}
	resp, _ = a.post(t, "/account/settings/email/cleanup", nil)
	if resp.Header.Get("HX-Redirect") != "/account/email-changed" {
		t.Fatalf("cleanup: %d %q", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
	if len(f.emails) != 1 || f.emails[0].Address != "new@example.com" || !f.emails[0].IsPrimary {
		t.Errorf("emails %+v", f.emails)
	}
	if len(f.others) != 0 {
		t.Errorf("other sessions remain: %v", f.others)
	}
	if f.sessions["valid-token"] != "" {
		t.Error("the current session was not ended")
	}
	if strings.Count(strings.Join(f.calls, ","), "email_delete") != 2 {
		t.Errorf("calls %v", f.calls)
	}
}

func TestEmailChangeStateBelongsToTheUser(t *testing.T) {
	f := newFakeHanko(t, "")
	a := newApp(t, f)
	a.post(t, "/account/settings/email", url.Values{"email": {"new@example.com"}})

	f.sessions["valid-token"] = "user-2"
	if _, body := a.post(t, "/account/settings/email/verify", url.Values{"code": {"123456"}}); !strings.Contains(body, "no longer available") {
		t.Fatalf("another user's change: %s", body)
	}
}
