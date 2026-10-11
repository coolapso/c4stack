//go:build integration

package accounts

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/a-h/templ"
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

// testIdentity is a test provider: the "session" cookie names an identity.
type testIdentity struct {
	name      string
	mu        sync.Mutex
	sessions  map[string]identity.Identity
	deleteErr error
	deleted   []string
	logouts   int
}

func (p *testIdentity) Name() string { return p.name }

func (p *testIdentity) Authenticate(r *http.Request) (identity.Identity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, err := r.Cookie("session")
	if err != nil {
		return identity.Identity{}, identity.ErrNoSession
	}
	id, ok := p.sessions[c.Value]
	if !ok {
		return identity.Identity{}, identity.ErrNoSession
	}
	return id, nil
}

func (p *testIdentity) Logout(w http.ResponseWriter, r *http.Request) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logouts++
	http.SetCookie(w, &http.Cookie{Name: "session", Path: "/", MaxAge: -1})
	return nil
}

func (p *testIdentity) Delete(ctx context.Context, r *http.Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.deleteErr != nil {
		return p.deleteErr
	}
	c, _ := r.Cookie("session")
	id := p.sessions[c.Value]
	delete(p.sessions, c.Value)
	p.deleted = append(p.deleted, id.Subject)
	return nil
}

var notes = app.Migrations{Namespace: "app", Files: fstest.MapFS{
	"001_notes.sql": {Data: []byte(`
		CREATE TABLE notes (
		    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
		    body text NOT NULL
		);`)},
}}

type fixture struct {
	a        *app.App
	accounts *Accounts
	idp      *testIdentity
	r        *gin.Engine
	hookErr  error
	hooked   []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{idp: &testIdentity{name: "test", sessions: map[string]identity.Identity{
		"ada": {Subject: "ada", Email: "ada@example.com", EmailVerified: true, SignedInAt: time.Now()},
	}}}
	f.a = apptest.New(t, app.Config{Name: "test", Migrations: []app.Migrations{Migrations, notes}})
	var err error
	f.accounts, err = New(f.a, f.idp, Config{})
	if err != nil {
		t.Fatal(err)
	}
	f.accounts.OnDelete(func(ctx context.Context, tx *sql.Tx, id string) error {
		f.hooked = append(f.hooked, id)
		return f.hookErr
	})
	f.r, err = web.New(web.Config{PublicURL: "http://localhost:8080", SecretKey: strings.Repeat("s", 32)})
	if err != nil {
		t.Fatal(err)
	}
	f.r.GET("/token", func(c *gin.Context) { c.String(http.StatusOK, web.CSRFToken(c)) })
	layout := ui.Default(ui.Design{Name: "Test"}, ui.NewAssets("/assets"))
	f.accounts.Mount(f.r, layout)
	f.r.GET("/account", f.accounts.RequireSignIn, func(c *gin.Context) { c.String(http.StatusOK, Current(c).Email) })
	return f
}

// do sends a request as the browser holding session; POSTs carry a valid CSRF token.
func (f *fixture) do(method, path, session string, form url.Values, header http.Header) *httptest.ResponseRecorder {
	tok := httptest.NewRecorder()
	f.r.ServeHTTP(tok, httptest.NewRequest(http.MethodGet, "http://localhost:8080/token", nil))
	var csrf *http.Cookie
	for _, c := range tok.Result().Cookies() {
		if c.Name == "csrf" {
			csrf = c
		}
	}
	if form == nil {
		form = url.Values{}
	}
	if method == http.MethodPost && form.Get(web.CSRFField) == "" {
		form.Set(web.CSRFField, tok.Body.String())
	}
	req := httptest.NewRequest(method, "http://localhost:8080"+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://localhost:8080")
	for k, v := range header {
		req.Header[k] = v
	}
	req.AddCookie(csrf)
	if session != "" {
		req.AddCookie(&http.Cookie{Name: "session", Value: session})
	}
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.a.DB().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestResolveCreatesOneAccountUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	id := identity.Identity{Subject: "grace", Email: "grace@example.com", EmailVerified: true}
	ids := make(chan string, 20)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			a, err := f.accounts.resolve(context.Background(), id)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- a.ID
		})
	}
	wg.Wait()
	close(ids)
	first := <-ids
	for other := range ids {
		if other != first {
			t.Fatalf("two accounts for one identity: %s, %s", first, other)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM accounts`); n != 1 {
		t.Fatalf("%d accounts", n)
	}
}

func TestNewIdentitiesJoinTheAccountWithTheirEmail(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, err := f.accounts.resolve(ctx, identity.Identity{Subject: "one", Email: "same@example.com", EmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := f.accounts.resolve(ctx, identity.Identity{Subject: "two", Email: "same@example.com", EmailVerified: true})
	other := &Accounts{app: f.a, idp: &testIdentity{name: "other"}}
	c, _ := other.resolve(ctx, identity.Identity{Subject: "one", Email: "same@example.com", EmailVerified: true})
	if a.ID == "" || b.ID != a.ID || c.ID != a.ID {
		t.Fatalf("identities with one email got different accounts: %q %q %q", a.ID, b.ID, c.ID)
	}
	if n := f.count(t, `SELECT count(*) FROM account_identities WHERE account_id = $1`, a.ID); n != 3 {
		t.Fatalf("%d identities", n)
	}
	d, _ := f.accounts.resolve(ctx, identity.Identity{Subject: "three", Email: "else@example.com", EmailVerified: true})
	if d.ID == a.ID {
		t.Fatal("another address joined the account")
	}
}

func TestEmailChanges(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, _ := f.accounts.resolve(ctx, identity.Identity{Subject: "s", Email: "v@example.com", EmailVerified: true})
	if err := f.accounts.setEmail(ctx, a.ID, "s", "new@example.com"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.accounts.Get(ctx, a.ID); got.Email != "new@example.com" {
		t.Fatalf("email %q", got.Email)
	}
	if n := f.count(t, `SELECT count(*) FROM account_identities WHERE email = 'new@example.com' AND email_verified`); n != 1 {
		t.Fatal("the identity kept the old address")
	}
	if _, err := f.accounts.Get(ctx, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid ID: %v", err)
	}
}

func TestRequireSignIn(t *testing.T) {
	f := newFixture(t)
	if rec := f.do(http.MethodGet, "/account/settings?tab=1", "", nil, nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?next=%2Faccount%2Fsettings%3Ftab%3D1" {
		t.Fatalf("no session: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := f.do(http.MethodGet, "/account", "", nil, http.Header{"Hx-Request": {"true"}}); rec.Code != http.StatusUnauthorized || rec.Header().Get("HX-Redirect") == "" {
		t.Fatalf("HTMX without session: %d %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	rec := f.do(http.MethodGet, "/account/settings", "ada", nil, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), "ada@example.com") {
		t.Fatalf("signed in: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec := f.do(http.MethodPost, "/account/delete", "ada", url.Values{web.CSRFField: {"forged"}}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("forged CSRF token: %d", rec.Code)
	}

	f.idp.sessions["unverified"] = identity.Identity{Subject: "u", Email: "u@example.com", SignedInAt: time.Now()}
	if rec := f.do(http.MethodGet, "/account", "unverified", nil, nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/auth/email-unverified" {
		t.Fatalf("unverified: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if f.count(t, `SELECT count(*) FROM accounts WHERE primary_email = 'u@example.com'`) != 0 {
		t.Fatal("an unverified identity got an account")
	}
	if rec := f.do(http.MethodGet, "/auth/email-unverified", "unverified", nil, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Confirm your email first") {
		t.Fatalf("unverified page: %d", rec.Code)
	}

	headless, err := New(f.a, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	f.r.GET("/headless", headless.RequireSignIn, func(c *gin.Context) { c.Status(http.StatusOK) })
	if rec := f.do(http.MethodGet, "/headless", "ada", nil, nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("sign-in without a provider: %d", rec.Code)
	}
}

func TestDeleteAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	account, _ := f.accounts.resolve(ctx, f.idp.sessions["ada"])
	if _, err := f.a.DB().ExecContext(ctx, `INSERT INTO notes VALUES ($1, 'personal')`, account.ID); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(http.MethodGet, "/account/delete", "ada", nil, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Type DELETE to confirm") {
		t.Fatalf("delete page: %d", rec.Code)
	}
	if rec := f.do(http.MethodPost, "/account/delete", "ada", url.Values{"confirmation": {"delete"}}, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong confirmation: %d", rec.Code)
	}

	old := f.idp.sessions["ada"]
	old.SignedInAt = time.Now().Add(-identity.RecentSignIn - time.Minute)
	f.idp.sessions["stale"] = old
	if rec := f.do(http.MethodPost, "/account/delete", "stale", url.Values{"confirmation": {"DELETE"}}, nil); rec.Code != http.StatusForbidden || len(f.idp.deleted) != 0 {
		t.Fatalf("old session: %d, deleted %v", rec.Code, f.idp.deleted)
	}

	f.idp.deleteErr = errors.New("provider unavailable")
	if rec := f.do(http.MethodPost, "/account/delete", "ada", url.Values{"confirmation": {"DELETE"}}, nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("provider failure: %d", rec.Code)
	}
	if f.count(t, `SELECT count(*) FROM accounts WHERE deletion_started_at IS NULL`) != 1 || len(f.hooked) != 0 {
		t.Fatal("the account changed although the provider failed")
	}
	f.idp.deleteErr = nil

	rec := f.do(http.MethodPost, "/account/delete", "ada", url.Values{"confirmation": {"DELETE"}}, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account/deleted" {
		t.Fatalf("delete: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := f.do(http.MethodGet, "/account/deleted", "", nil, nil); !strings.Contains(rec.Body.String(), "Your account is deleted") {
		t.Fatalf("deleted page: %d", rec.Code)
	}
	if f.count(t, `SELECT count(*) FROM accounts`) != 0 || f.count(t, `SELECT count(*) FROM notes`) != 0 || f.count(t, `SELECT count(*) FROM account_identities`) != 0 {
		t.Fatal("account data remains")
	}
	if len(f.hooked) != 1 || f.hooked[0] != account.ID {
		t.Fatalf("hooks %v", f.hooked)
	}

	// A request that validated its session before the deletion cannot recreate it.
	if _, err := f.accounts.resolve(ctx, identity.Identity{Subject: "ada", Email: "ada@example.com", EmailVerified: true}); !errors.Is(err, errDeleted) {
		t.Fatalf("recreated a deleted identity: %v", err)
	}
	f.idp.sessions["ghost"] = identity.Identity{Subject: "ada", Email: "ada@example.com", EmailVerified: true, SignedInAt: time.Now()}
	if rec := f.do(http.MethodGet, "/account", "ghost", nil, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("deleted identity: %d", rec.Code)
	}

	// The address signs up again with a new identity and gets a new, empty account.
	again, err := f.accounts.resolve(ctx, identity.Identity{Subject: "ada-2", Email: "ada@example.com", EmailVerified: true})
	if err != nil || again.ID == account.ID {
		t.Fatalf("sign-up after deletion: %q %v", again.ID, err)
	}
}

func TestLocalFailureAfterProviderDeletion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	account, _ := f.accounts.resolve(ctx, f.idp.sessions["ada"])
	f.hookErr = errors.New("app table locked")

	rec := f.do(http.MethodPost, "/account/delete", "ada", url.Values{"confirmation": {"DELETE"}}, nil)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "has not finished") {
		t.Fatalf("local failure: %d", rec.Code)
	}
	if len(f.idp.deleted) != 1 || f.count(t, `SELECT count(*) FROM accounts WHERE deletion_started_at IS NOT NULL`) != 1 {
		t.Fatal("expected the identity deleted and the account kept, marked")
	}

	// An operator finishes it.
	f.hookErr = nil
	if err := f.accounts.deleteLocalCommand(ctx, []string{account.ID}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `SELECT count(*) FROM accounts`) != 0 {
		t.Fatal("delete-local left the account")
	}
	if err := f.accounts.deleteLocalCommand(ctx, []string{account.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second run: %v", err)
	}
}

func TestSignUpFinishesAConfirmedDeletion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	account, _ := f.accounts.resolve(ctx, f.idp.sessions["ada"])
	if _, err := f.a.DB().ExecContext(ctx, `INSERT INTO notes VALUES ($1, 'personal')`, account.ID); err != nil {
		t.Fatal(err)
	}
	f.hookErr = errors.New("app table locked")
	f.do(http.MethodPost, "/account/delete", "ada", url.Values{"confirmation": {"DELETE"}}, nil)
	f.hookErr = nil
	if f.count(t, `SELECT count(*) FROM accounts WHERE deletion_confirmed_at IS NOT NULL`) != 1 {
		t.Fatal("the provider's confirmation was not recorded")
	}

	again, err := f.accounts.resolve(ctx, identity.Identity{Subject: "ada-2", Email: "ada@example.com", EmailVerified: true})
	if err != nil || again.ID == account.ID {
		t.Fatalf("sign-up joined the deleted account: %q %v", again.ID, err)
	}
	if f.count(t, `SELECT count(*) FROM notes`) != 0 || f.count(t, `SELECT count(*) FROM accounts`) != 1 || len(f.hooked) != 2 {
		t.Fatalf("the unfinished deletion was not finished: hooks %v", f.hooked)
	}
}

// A sign-up must not delete an account whose deletion the provider has not confirmed,
// even when it comes from another provider while the first is still deciding.
func TestSignUpWaitsForAnUnconfirmedDeletion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	account, _ := f.accounts.resolve(ctx, f.idp.sessions["ada"])
	if _, err := f.a.DB().ExecContext(ctx, `INSERT INTO notes VALUES ($1, 'personal')`, account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.DB().ExecContext(ctx, `UPDATE accounts SET deletion_started_at = now()`); err != nil {
		t.Fatal(err)
	}
	other := &Accounts{app: f.a, idp: &testIdentity{name: "other"}, cfg: f.accounts.cfg}
	if _, err := other.resolve(ctx, identity.Identity{Subject: "x", Email: "ada@example.com", EmailVerified: true}); !errors.Is(err, errDeletionPending) {
		t.Fatalf("sign-up during an unconfirmed deletion: %v", err)
	}
	if f.count(t, `SELECT count(*) FROM notes`) != 1 || len(f.hooked) != 0 {
		t.Fatal("a sign-up deleted an account the provider had not confirmed deleting")
	}
	f.idp.sessions["newcomer"] = identity.Identity{Subject: "newcomer", Email: "ada@example.com", EmailVerified: true, SignedInAt: time.Now()}
	if rec := f.do(http.MethodGet, "/account", "newcomer", nil, nil); rec.Header().Get("Location") != "/account/deleted?unfinished=1" {
		t.Fatalf("sign-up page: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// The provider refuses; the owner signs in again and keeps everything.
	if got, err := f.accounts.resolve(ctx, f.idp.sessions["ada"]); err != nil || got.ID != account.ID {
		t.Fatalf("owner's sign-in: %v", err)
	}
	if b, _ := other.resolve(ctx, identity.Identity{Subject: "x", Email: "ada@example.com", EmailVerified: true}); b.ID != account.ID {
		t.Fatal("after the mark cleared, a new identity did not join the account")
	}
}

func TestAnotherIdentityFinishesAConfirmedDeletion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	account, _ := f.accounts.resolve(ctx, f.idp.sessions["ada"])
	second := identity.Identity{Subject: "ada-elsewhere", Email: "ada@example.com", EmailVerified: true}
	f.accounts.resolve(ctx, second)
	if _, err := f.a.DB().ExecContext(ctx, `UPDATE accounts SET deletion_started_at = now(), deletion_confirmed_at = now()`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.accounts.resolve(ctx, second); !errors.Is(err, errDeleted) {
		t.Fatalf("sign-in to a confirmed deletion: %v", err)
	}
	if _, err := f.accounts.Get(ctx, account.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("the deletion was not finished")
	}
}

// When an identity's verified address changes, the account's primary address follows,
// so the old address no longer leads a new identity to the account.
func TestThePrimaryAddressFollowsItsIdentity(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, _ := f.accounts.resolve(ctx, identity.Identity{Subject: "a", Email: "old@example.com", EmailVerified: true})
	moved, err := f.accounts.resolve(ctx, identity.Identity{Subject: "a", Email: "new@example.com", EmailVerified: true})
	if err != nil || moved.ID != a.ID || moved.Email != "new@example.com" {
		t.Fatalf("after the change: %+v %v", moved, err)
	}
	b, _ := f.accounts.resolve(ctx, identity.Identity{Subject: "b", Email: "old@example.com", EmailVerified: true})
	if b.ID == "" || b.ID == a.ID {
		t.Fatal("the old address led a new identity to the account")
	}
	c, _ := f.accounts.resolve(ctx, identity.Identity{Subject: "c", Email: "new@example.com", EmailVerified: true})
	if c.ID != a.ID {
		t.Fatal("the new address did not lead to the account")
	}

	// The new address already belongs to another account: the primary address stays,
	// and the old address is turned away rather than joined.
	d, _ := f.accounts.resolve(ctx, identity.Identity{Subject: "d", Email: "d@example.com", EmailVerified: true})
	f.accounts.resolve(ctx, identity.Identity{Subject: "d", Email: "taken@example.com", EmailVerified: true})
	f.accounts.resolve(ctx, identity.Identity{Subject: "t", Email: "taken@example.com", EmailVerified: true})
	f.accounts.resolve(ctx, identity.Identity{Subject: "d", Email: "taken@example.com", EmailVerified: true})
	if got, _ := f.accounts.Get(ctx, d.ID); got.Email != "taken@example.com" {
		t.Fatalf("d's primary address: %q", got.Email)
	}
	e, _ := f.accounts.resolve(ctx, identity.Identity{Subject: "e", Email: "e@example.com", EmailVerified: true})
	f.accounts.resolve(ctx, identity.Identity{Subject: "e", Email: "c@example.com", EmailVerified: true})
	f.accounts.resolve(ctx, identity.Identity{Subject: "c2", Email: "c@example.com", EmailVerified: true})
	f.accounts.resolve(ctx, identity.Identity{Subject: "e", Email: "taken@example.com", EmailVerified: true})
	if got, _ := f.accounts.Get(ctx, e.ID); got.Email != "c@example.com" {
		t.Fatalf("e's primary address: %q", got.Email)
	}
	if _, err := f.accounts.resolve(ctx, identity.Identity{Subject: "late", Email: "e@example.com", EmailVerified: true}); err != nil {
		t.Fatalf("a free old address: %v", err)
	}
}

func TestSignInClearsAStaleDeletionMark(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	account, _ := f.accounts.resolve(ctx, f.idp.sessions["ada"])
	if _, err := f.a.DB().ExecContext(ctx, `UPDATE accounts SET deletion_started_at = now()`); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.accounts.resolve(ctx, f.idp.sessions["ada"]); got.ID != account.ID {
		t.Fatal("the signed-in identity lost its account")
	}
	if f.count(t, `SELECT count(*) FROM accounts WHERE deletion_started_at IS NULL`) != 1 {
		t.Fatal("the mark remains")
	}
	b, _ := f.accounts.resolve(ctx, identity.Identity{Subject: "ada-2", Email: "ada@example.com", EmailVerified: true})
	if b.ID != account.ID {
		t.Fatal("a new identity did not join the account")
	}
}

// An identity whose provider reports a new, unverified address cannot sign in, and its
// old address stops counting as verified, so it leads no new identity to the account.
func TestAnUnverifiedChangeExposesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.idp.sessions["a"] = identity.Identity{Subject: "a", Email: "old@example.com", EmailVerified: true, SignedInAt: time.Now()}
	a, _ := f.accounts.resolve(ctx, f.idp.sessions["a"])

	f.idp.sessions["a"] = identity.Identity{Subject: "a", Email: "new@example.com", SignedInAt: time.Now()}
	if rec := f.do(http.MethodGet, "/account", "a", nil, nil); rec.Header().Get("Location") != "/auth/email-unverified" {
		t.Fatalf("unverified change: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if b, err := f.accounts.resolve(ctx, identity.Identity{Subject: "b", Email: "old@example.com", EmailVerified: true}); b.ID == a.ID || !errors.Is(err, errStaleAddress) {
		t.Fatalf("a new identity with the old address: %q %v", b.ID, err)
	}

	// Once the identity verifies the new address, it becomes the account's.
	moved, err := f.accounts.resolve(ctx, identity.Identity{Subject: "a", Email: "new@example.com", EmailVerified: true})
	if err != nil || moved.ID != a.ID || moved.Email != "new@example.com" {
		t.Fatalf("after verification: %+v %v", moved, err)
	}
	c, err := f.accounts.resolve(ctx, identity.Identity{Subject: "c", Email: "old@example.com", EmailVerified: true})
	if err != nil || c.ID == a.ID {
		t.Fatalf("the freed old address: %q %v", c.ID, err)
	}
}

func TestDeletionSurvivesABrowserDisconnect(t *testing.T) {
	f := newFixture(t)
	f.r.POST("/gone", f.accounts.RequireSignIn, func(c *gin.Context) {
		ctx, cancel := context.WithCancel(c.Request.Context())
		cancel()
		c.Request = c.Request.WithContext(ctx)
		if err := f.accounts.Delete(c); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.Status(http.StatusNoContent)
	})
	if rec := f.do(http.MethodPost, "/gone", "ada", nil, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("disconnected deletion: %d %s", rec.Code, rec.Body.String())
	}
}

func TestLogout(t *testing.T) {
	f := newFixture(t)
	if rec := f.do(http.MethodPost, "/logout", "ada", url.Values{"next": {"/bye"}}, nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/bye" {
		t.Fatalf("logout: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := f.do(http.MethodPost, "/logout", "ada", url.Values{"next": {"//evil.example"}}, nil); rec.Header().Get("Location") != "/" {
		t.Fatalf("foreign next: %q", rec.Header().Get("Location"))
	}
	if rec := f.do(http.MethodPost, "/logout", "ada", url.Values{web.CSRFField: {"forged"}}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("forged CSRF token: %d", rec.Code)
	}
	if f.idp.logouts != 2 {
		t.Fatalf("logouts %d", f.idp.logouts)
	}
}

// An app can move the pages, mount them under a prefix and render them in its own
// layout.
func TestPagesInACustomLayoutAndPlace(t *testing.T) {
	f := newFixture(t)
	custom := ui.Layout{
		Assets: ui.NewAssets("/static"),
		Page: func(c *gin.Context, title string, body templ.Component) templ.Component {
			return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
				io.WriteString(w, `<html><body class="my-own-layout"><h1 class="brand">`+title+`</h1>`)
				if err := body.Render(ctx, w); err != nil {
					return err
				}
				_, err := io.WriteString(w, `</body></html>`)
				return err
			})
		},
	}
	moved, err := New(f.a, f.idp, Config{Paths: Paths{
		Login: "/me/sign-in", Logout: "/me/sign-out", Unverified: "/me/confirm",
		Settings: "/me/preferences", Delete: "/me/close", Deleted: "/me/closed", EmailChanged: "/me/moved",
	}})
	if err != nil {
		t.Fatal(err)
	}
	moved.Mount(f.r.Group("/me"), custom)

	body := f.do(http.MethodGet, "/me/preferences", "ada", nil, nil).Body.String()
	for _, want := range []string{`class="my-own-layout"`, `action="/me/sign-out"`, `href="/me/close"`, "ada@example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if rec := f.do(http.MethodGet, "/me/preferences", "", nil, nil); rec.Header().Get("Location") != "/me/sign-in?next=%2Fme%2Fpreferences" {
		t.Fatalf("visitor: %q", rec.Header().Get("Location"))
	}
	f.idp.sessions["unverified"] = identity.Identity{Subject: "u", Email: "u@example.com", SignedInAt: time.Now()}
	if rec := f.do(http.MethodGet, "/me/preferences", "unverified", nil, nil); rec.Header().Get("Location") != "/me/confirm" {
		t.Fatalf("unverified: %q", rec.Header().Get("Location"))
	}
	if rec := f.do(http.MethodPost, "/me/sign-out", "ada", nil, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("moved logout: %d", rec.Code)
	}
	if rec := f.do(http.MethodPost, "/me/close", "ada", url.Values{"confirmation": {"DELETE"}}, nil); rec.Header().Get("Location") != "/me/closed" {
		t.Fatalf("moved deletion: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	defer func() {
		if recover() == nil {
			t.Error("mounting a page outside the group's prefix did not panic")
		}
	}()
	f.accounts.Mount(f.r.Group("/elsewhere"), custom)
}

// An app can serve one account page itself and keep the others.
func TestReplaceOnePage(t *testing.T) {
	f := newFixture(t)
	r, _ := web.New(web.Config{PublicURL: "http://localhost:8080", SecretKey: strings.Repeat("s", 32)})
	f.r = r
	r.GET("/token", func(c *gin.Context) { c.String(http.StatusOK, web.CSRFToken(c)) })
	p := f.accounts.Pages(ui.Default(ui.Design{Name: "Test"}, ui.NewAssets("/assets")))
	paths := f.accounts.cfg.Paths
	r.GET(paths.Settings, f.accounts.RequireSignIn, func(c *gin.Context) { c.String(http.StatusOK, "my settings") })
	r.GET(paths.Delete, f.accounts.RequireSignIn, p.DeleteForm)
	r.POST(paths.Delete, f.accounts.RequireSignIn, p.Delete)
	r.GET(paths.Deleted, p.Deleted)
	if body := f.do(http.MethodGet, "/account/settings", "ada", nil, nil).Body.String(); body != "my settings" {
		t.Fatalf("own page: %q", body)
	}
	if rec := f.do(http.MethodGet, "/account/delete", "ada", nil, nil); !strings.Contains(rec.Body.String(), "Type DELETE to confirm") {
		t.Fatalf("kept page: %d", rec.Code)
	}
}
