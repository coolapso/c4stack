//go:build integration

package expiry

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/app/apptest"
	"github.com/coolapso/c4stack/credits"
	"github.com/coolapso/c4stack/email"
	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/payments"
)

type testIdentity struct{}

func (testIdentity) Name() string { return "test" }
func (testIdentity) Authenticate(*http.Request) (identity.Identity, error) {
	return identity.Identity{}, identity.ErrNoSession
}
func (testIdentity) Logout(http.ResponseWriter, *http.Request) error { return nil }
func (testIdentity) Delete(context.Context, *http.Request) error     { return nil }

type testPayments struct{}

func (testPayments) Name() string           { return "test" }
func (testPayments) CheckoutOrigin() string { return "https://pay.example" }
func (testPayments) CreateCheckout(context.Context, payments.Checkout) (payments.Session, error) {
	return payments.Session{}, nil
}
func (testPayments) ParseWebhook(http.Header, []byte) (payments.Event, error) {
	return payments.Event{}, payments.ErrInvalid
}

// outbox records sent mail; fail makes delivery fail.
type outbox struct {
	mu   sync.Mutex
	sent []email.Message
	fail bool
}

func (o *outbox) Send(_ context.Context, m email.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail {
		return errors.New("mail server down")
	}
	o.sent = append(o.sent, m)
	return nil
}

var holds = app.Migrations{Namespace: "app", Files: fstest.MapFS{"001_holds.sql": {Data: []byte(`
	CREATE TABLE holds (account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE, cents bigint NOT NULL);`)}}}

type fixture struct {
	a       *app.App
	credits *credits.Credits
	expiry  *Expiry
	mail    *outbox
	now     time.Time
}

// start is when the tests' accounts last used their credit.
var start = time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{mail: &outbox{}, now: start}
	f.a = apptest.New(t, app.Config{Name: "test", Migrations: []app.Migrations{accounts.Migrations, credits.Migrations, Migrations, holds}})
	accts, err := accounts.New(f.a, testIdentity{}, accounts.Config{})
	if err != nil {
		t.Fatal(err)
	}
	f.credits, err = credits.New(f.a, accts, credits.Config{
		Payments: testPayments{},
		Policy:   credits.Policy{MinPurchaseCents: 500, MaxPurchaseCents: 10_000},
		Holds: func(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
			var held int64
			err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(cents), 0) FROM holds WHERE account_id = $1`, id).Scan(&held)
			return held, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.expiry, err = New(f.a, f.credits, Config{
		Sender:     f.mail,
		Policy:     Policy{InactiveMonths: 12, NoticeDays: 30},
		AppName:    "Acme",
		CreditsURL: "https://acme.example/account/credits",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.expiry.now = func() time.Time { return f.now }
	return f
}

// account creates an account with credit last used at start.
func (f *fixture) account(t *testing.T, email string, cents int64) string {
	t.Helper()
	var id string
	var addr sql.NullString
	if email != "" {
		addr = sql.NullString{String: email, Valid: true}
	}
	if err := f.a.DB().QueryRow(`INSERT INTO accounts (provider, subject, email) VALUES ('test', $1, $2) RETURNING id`, t.Name()+email, addr).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.DB().Exec(`
		INSERT INTO credit_ledger (account_id, kind, amount_cents, reference, created_at)
		VALUES ($1, 'adjustment', $2, 'seed', $3)`, id, cents, start); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) run(t *testing.T) Result {
	t.Helper()
	r, err := f.expiry.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fixture) balance(t *testing.T, id string) int64 {
	s, err := f.credits.Summary(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s.BalanceCents
}

func TestWarnThenExpire(t *testing.T) {
	f := newFixture(t)
	ada := f.account(t, "ada@example.com", 1234)

	f.now = start.AddDate(0, 10, 0)
	if r := f.run(t); r.Warned != 0 {
		t.Fatalf("warned too early: %+v", r)
	}
	// Twelve calendar months after 29 February 2024 is 28 February 2025.
	f.now = time.Date(2025, 1, 30, 0, 0, 0, 0, time.UTC)
	if r := f.run(t); r.Warned != 1 {
		t.Fatalf("no warning: %+v", r)
	}
	if r := f.run(t); r.Warned != 0 {
		t.Fatalf("warned twice: %+v", r)
	}
	m := f.mail.sent[0]
	if m.To != "ada@example.com" || !strings.Contains(m.Subject, "Acme") || !strings.Contains(m.Subject, "1 March 2025") ||
		!strings.Contains(m.Text, "€12.34") || !strings.Contains(m.Text, "https://acme.example/account/credits") {
		t.Fatalf("warning %+v", m)
	}

	// The notice runs from delivery: nothing expires on the nominal date.
	f.now = time.Date(2025, 2, 28, 12, 0, 0, 0, time.UTC)
	if r := f.run(t); r.Expired != 0 {
		t.Fatalf("expired before the notice ran out: %+v", r)
	}
	f.now = time.Date(2025, 3, 2, 0, 0, 0, 0, time.UTC)
	if r := f.run(t); r.Expired != 1 || f.balance(t, ada) != 0 {
		t.Fatalf("not expired: %+v, balance %d", r, f.balance(t, ada))
	}
	if r := f.run(t); r != (Result{}) {
		t.Fatalf("second run after expiry: %+v", r)
	}
	history, _ := f.credits.History(context.Background(), ada, 0, 5)
	if history[0].Kind != "expiry" || history[0].AmountCents != -1234 {
		t.Fatalf("history %+v", history)
	}
}

func TestActivityStartsOver(t *testing.T) {
	f := newFixture(t)
	ada := f.account(t, "ada@example.com", 1000)
	f.now = start.AddDate(0, 11, 15)
	f.run(t)
	if len(f.mail.sent) != 1 {
		t.Fatal("no warning")
	}
	// The user spends some credit after the warning.
	if _, err := f.a.DB().Exec(`
		INSERT INTO credit_ledger (account_id, kind, amount_cents, reference, created_at)
		VALUES ($1, 'debit', -100, 'job', $2)`, ada, f.now); err != nil {
		t.Fatal(err)
	}
	f.now = start.AddDate(0, 13, 0)
	if r := f.run(t); r.Expired != 0 || f.balance(t, ada) != 900 {
		t.Fatalf("expired despite new activity: %+v", r)
	}
}

func TestHoldsDeferExpiry(t *testing.T) {
	f := newFixture(t)
	ada := f.account(t, "ada@example.com", 1000)
	f.now = start.AddDate(0, 11, 15)
	f.run(t)
	f.a.DB().Exec(`INSERT INTO holds VALUES ($1, 200)`, ada)
	f.now = start.AddDate(0, 12, 20)
	if r := f.run(t); r.Expired != 0 {
		t.Fatalf("expired while held: %+v", r)
	}
	f.a.DB().Exec(`DELETE FROM holds`)
	if r := f.run(t); r.Expired != 1 || f.balance(t, ada) != 0 {
		t.Fatalf("after the hold: %+v", r)
	}
}

func TestFailedDeliveryNeverStartsTheNotice(t *testing.T) {
	f := newFixture(t)
	ada := f.account(t, "ada@example.com", 1000)
	f.account(t, "", 1000) // no verified address: never warned, never expires
	f.mail.fail = true
	f.now = start.AddDate(0, 11, 15)
	if r := f.run(t); r.Failed != 1 || r.Warned != 0 {
		t.Fatalf("failed delivery: %+v", r)
	}
	f.mail.fail = false
	f.now = f.now.Add(time.Hour)
	if r := f.run(t); r.Warned != 0 {
		t.Fatalf("retried within a day: %+v", r)
	}
	f.now = start.AddDate(0, 12, 1)
	if r := f.run(t); r.Warned != 1 || r.Expired != 0 {
		t.Fatalf("retry: %+v", r)
	}
	// The deadline is a full notice after delivery, past the nominal date.
	f.now = start.AddDate(0, 12, 15)
	if r := f.run(t); r.Expired != 0 {
		t.Fatalf("expired before the full notice: %+v", r)
	}
	f.now = start.AddDate(0, 13, 5)
	if r := f.run(t); r.Expired != 1 || f.balance(t, ada) != 0 {
		t.Fatalf("expiry after a late warning: %+v", r)
	}
	if len(f.mail.sent) != 1 {
		t.Fatalf("sent %d warnings", len(f.mail.sent))
	}
}

func TestStaleNoticesAreRenewed(t *testing.T) {
	f := newFixture(t)
	ada := f.account(t, "ada@example.com", 1000)
	f.now = start.AddDate(0, 11, 15)
	f.run(t)
	// The app did not run for months; the old deadline is long past.
	f.now = start.AddDate(0, 16, 0)
	if r := f.run(t); r.Warned != 1 || r.Expired != 0 || f.balance(t, ada) != 1000 {
		t.Fatalf("stale notice: %+v", r)
	}
}

func TestAnnouncedDeadlinesNeverShorten(t *testing.T) {
	f := newFixture(t)
	ada := f.account(t, "ada@example.com", 1000)
	f.now = start.AddDate(0, 11, 15)
	f.run(t)
	f.expiry.cfg.Policy.NoticeDays = 1
	f.now = start.AddDate(0, 12, 1)
	if r := f.run(t); r.Expired != 0 || f.balance(t, ada) != 1000 {
		t.Fatalf("a shorter notice applied to an announced deadline: %+v", r)
	}
}

func TestOneRunAtATime(t *testing.T) {
	f := newFixture(t)
	conn, err := f.a.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, int64(lockKey))
	if r := f.run(t); !r.Skipped {
		t.Fatalf("ran while another run held the lock: %+v", r)
	}
}

func TestNewNeedsASender(t *testing.T) {
	f := newFixture(t)
	if _, err := New(f.a, f.credits, Config{Policy: Policy{InactiveMonths: 12, NoticeDays: 30}, AppName: "Acme", CreditsURL: "https://acme.example"}); err == nil {
		t.Fatal("expiry without an email sender")
	}
}
