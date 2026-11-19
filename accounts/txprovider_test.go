//go:build integration

package accounts

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/app/apptest"
	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

// txIdentity is a test TxProvider: its users live in the test's own table, in the app's
// database, and its sessions name them.
type txIdentity struct {
	testIdentity
	hooks     identity.AccountHooks
	lent      int
	deleteErr error
	// ended makes SessionTx find the session gone, as after a recovery that committed
	// while the request waited.
	ended bool
}

func (p *txIdentity) SessionTx(ctx context.Context, tx *sql.Tx, r *http.Request) (identity.Identity, error) {
	if p.ended {
		return identity.Identity{}, identity.ErrNoSession
	}
	return p.Authenticate(r)
}

func (p *txIdentity) UseAccounts(h identity.AccountHooks) {
	p.hooks = h
	p.lent++
}

func (p *txIdentity) DeleteTx(ctx context.Context, tx *sql.Tx, subject string) error {
	if p.deleteErr != nil {
		return p.deleteErr
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM tx_users WHERE subject = $1`, subject)
	return err
}

var txUsers = app.Migrations{Namespace: "txtest", Files: fstest.MapFS{
	"001_users.sql": {Data: []byte(`
		CREATE TABLE tx_users (
		    subject text PRIMARY KEY,
		    email text NOT NULL UNIQUE
		);`)},
}}

type txFixture struct {
	a        *app.App
	accounts *Accounts
	idp      *txIdentity
	r        *gin.Engine
	hooked   []string
}

func newTxFixture(t *testing.T) *txFixture {
	t.Helper()
	f := &txFixture{idp: &txIdentity{testIdentity: testIdentity{name: "own", sessions: map[string]identity.Identity{}}}}
	f.a = apptest.New(t, app.Config{Name: "test", Migrations: []app.Migrations{Migrations, notes, txUsers}})
	var err error
	if f.accounts, err = New(f.a, f.idp, Config{}); err != nil {
		t.Fatal(err)
	}
	f.accounts.OnDelete(func(ctx context.Context, tx *sql.Tx, id string) error {
		f.hooked = append(f.hooked, id)
		return nil
	})
	if f.r, err = web.New(web.Config{PublicURL: "http://localhost:8080", SecretKey: strings.Repeat("s", 32)}); err != nil {
		t.Fatal(err)
	}
	f.r.GET("/token", func(c *gin.Context) { c.String(http.StatusOK, web.CSRFToken(c)) })
	f.accounts.Mount(f.r, ui.Default(ui.Design{Name: "Test"}, ui.NewAssets("/assets")))
	f.r.GET("/account", f.accounts.RequireSignIn, func(c *gin.Context) { c.String(http.StatusOK, Current(c).Email) })
	return f
}

// signUp creates a user and signs it in, in one transaction, as a TxProvider does.
func (f *txFixture) signUp(t *testing.T, subject, email string) error {
	t.Helper()
	tx, err := f.a.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO tx_users (subject, email) VALUES ($1, $2)`, subject, email); err != nil {
		t.Fatal(err)
	}
	id := identity.Identity{Subject: subject, Email: email, EmailVerified: true, SignedInAt: time.Now()}
	if err := f.idp.hooks.SignedIn(context.Background(), tx, id); err != nil {
		return err
	}
	f.idp.sessions[subject] = id
	return tx.Commit()
}

func (f *txFixture) do(method, path, session string, form url.Values) *http.Response {
	return (&fixture{r: f.r}).do(method, path, session, form, nil).Result()
}

func (f *txFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.a.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTxProviderGetsTheHooks(t *testing.T) {
	f := newTxFixture(t)
	if f.idp.lent != 1 || f.idp.hooks.SignedIn == nil || f.idp.hooks.AddressChanged == nil {
		t.Fatal("New didn't lend the provider its hooks")
	}
}

func TestSignUpInTheProvidersTransaction(t *testing.T) {
	f := newTxFixture(t)
	if err := f.signUp(t, "ann", "ann@example.com"); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE primary_email = 'ann@example.com'`); n != 1 {
		t.Fatalf("%d accounts", n)
	}
	// Signing in again finds the same account.
	tx, _ := f.a.DB().Begin()
	if err := f.idp.hooks.SignedIn(context.Background(), tx, identity.Identity{Subject: "ann", Email: "ann@example.com", EmailVerified: true}); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	if n := f.count(t, `SELECT count(*) FROM accounts`); n != 1 {
		t.Fatalf("%d accounts", n)
	}

	// The provider rolling back undoes the account too.
	tx, _ = f.a.DB().Begin()
	if err := f.idp.hooks.SignedIn(context.Background(), tx, identity.Identity{Subject: "bob", Email: "bob@example.com", EmailVerified: true}); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE primary_email = 'bob@example.com'`); n != 0 {
		t.Fatal("a rolled-back sign-up left an account")
	}

	// Unverified identities, and accounts being deleted, are refused.
	tx, _ = f.a.DB().Begin()
	defer tx.Rollback()
	err := f.idp.hooks.SignedIn(context.Background(), tx, identity.Identity{Subject: "u", Email: "u@example.com"})
	if !errors.Is(err, identity.ErrRefused) {
		t.Fatalf("unverified: %v", err)
	}
}

func TestSignUpRefusedForAnAccountBeingDeleted(t *testing.T) {
	f := newTxFixture(t)
	if _, err := f.a.DB().Exec(`INSERT INTO accounts (primary_email, deletion_started_at) VALUES ('ann@example.com', now())`); err != nil {
		t.Fatal(err)
	}
	if err := f.signUp(t, "ann", "ann@example.com"); !errors.Is(err, identity.ErrRefused) {
		t.Fatalf("err %v", err)
	}
}

func TestAddressChangedInTheProvidersTransaction(t *testing.T) {
	f := newTxFixture(t)
	f.signUp(t, "ann", "ann@example.com")
	f.signUp(t, "bob", "bob@example.com")
	ctx := context.Background()

	tx, _ := f.a.DB().Begin()
	if err := f.idp.hooks.AddressChanged(ctx, tx, "ann", "bob@example.com"); !errors.Is(err, identity.ErrAddressTaken) {
		t.Fatalf("taken address: %v", err)
	}
	tx.Rollback()

	tx, _ = f.a.DB().Begin()
	if err := f.idp.hooks.AddressChanged(ctx, tx, "ann", "ann@example.org"); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE primary_email = 'ann@example.org'`); n != 0 {
		t.Fatal("a rolled-back change took effect")
	}

	tx, _ = f.a.DB().Begin()
	if err := f.idp.hooks.AddressChanged(ctx, tx, "ann", "ann@example.org"); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	if n := f.count(t, `
		SELECT count(*)
		FROM accounts a
		JOIN account_identities i ON i.account_id = a.id
		WHERE a.primary_email = 'ann@example.org' AND i.email = 'ann@example.org' AND i.email_verified`); n != 1 {
		t.Fatal("the account and identity don't have the new address")
	}
}

func TestRequireSignInNeverWritesATxProvidersAddress(t *testing.T) {
	f := newTxFixture(t)
	f.signUp(t, "ann", "ann@example.com")
	tx, _ := f.a.DB().Begin()
	f.idp.hooks.AddressChanged(context.Background(), tx, "ann", "ann@example.org")
	tx.Commit()
	// A request that authenticated before the change still carries the old address.
	f.idp.sessions["ann"] = identity.Identity{Subject: "ann", Email: "ann@example.com", EmailVerified: true, SignedInAt: time.Now()}
	if res := f.do(http.MethodGet, "/account", "ann", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE primary_email = 'ann@example.org'`); n != 1 {
		t.Fatal("the stale request wrote the old address back")
	}
	if n := f.count(t, `SELECT count(*) FROM account_identities WHERE email = 'ann@example.com'`); n != 0 {
		t.Fatal("the stale request wrote the old address to the identity")
	}
}

func TestDeleteWithATxProvider(t *testing.T) {
	f := newTxFixture(t)
	f.signUp(t, "ann", "ann@example.com")
	f.idp.sessions["ann"] = identity.Identity{Subject: "ann", Email: "ann@example.com", EmailVerified: true, SignedInAt: time.Now()}

	f.idp.deleteErr = errors.New("the provider failed")
	res := f.do(http.MethodPost, "/account/delete", "ann", url.Values{"confirmation": {"DELETE"}})
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("failing provider: %d", res.StatusCode)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE deletion_started_at IS NOT NULL OR deletion_confirmed_at IS NOT NULL`); n != 0 {
		t.Fatal("a TxProvider's deletion set marks")
	}
	if n := f.count(t, `SELECT count(*) FROM accounts`) + f.count(t, `SELECT count(*) FROM tx_users`); n != 2 {
		t.Fatal("a failed deletion deleted something")
	}

	f.idp.deleteErr = nil
	res = f.do(http.MethodPost, "/account/delete", "ann", url.Values{"confirmation": {"DELETE"}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/account/deleted" {
		t.Fatalf("status %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM accounts`:           0,
		`SELECT count(*) FROM tx_users`:           0,
		`SELECT count(*) FROM deleted_identities`: 1,
	} {
		if n := f.count(t, query); n != want {
			t.Errorf("%s: %d, want %d", query, n, want)
		}
	}
	if len(f.hooked) != 1 || f.idp.logouts != 1 {
		t.Fatalf("hooks %v, logouts %d", f.hooked, f.idp.logouts)
	}
}

func TestDeletionRechecksTheSessionInItsTransaction(t *testing.T) {
	f := newTxFixture(t)
	f.signUp(t, "ann", "ann@example.com")
	f.idp.ended = true
	res := f.do(http.MethodPost, "/account/delete", "ann", url.Values{"confirmation": {"DELETE"}})
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d", res.StatusCode)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts`) + f.count(t, `SELECT count(*) FROM tx_users`); n != 2 {
		t.Fatal("a deletion whose session ended deleted something")
	}
}

func TestDeleteLocalDeletesATxProvidersUsers(t *testing.T) {
	f := newTxFixture(t)
	f.signUp(t, "ann", "ann@example.com")
	var accountID string
	f.a.DB().QueryRow(`SELECT id FROM accounts`).Scan(&accountID)
	if err := f.accounts.deleteLocal(context.Background(), accountID); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM tx_users`); n != 0 {
		t.Fatal("accounts delete-local left the provider's user")
	}
}
