//go:build integration

package credits

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/app/apptest"
	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/payments"
	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

// testPayments is a payment provider for tests. Webhook bodies are JSON-encoded
// payments.Events; the signature header must say "valid".
type testPayments struct {
	mu        sync.Mutex
	checkouts []payments.Checkout
}

func (p *testPayments) Name() string           { return "test" }
func (p *testPayments) CheckoutOrigin() string { return "https://pay.example" }

func (p *testPayments) CreateCheckout(ctx context.Context, c payments.Checkout) (payments.Session, error) {
	p.mu.Lock()
	p.checkouts = append(p.checkouts, c)
	p.mu.Unlock()
	id := "co_" + c.Reference
	return payments.Session{ID: id, URL: "https://pay.example/" + id}, nil
}

func (p *testPayments) ParseWebhook(h http.Header, body []byte) (payments.Event, error) {
	if h.Get("X-Signature") != "valid" {
		return payments.Event{}, payments.ErrInvalid
	}
	var e payments.Event
	if err := json.Unmarshal(body, &e); err != nil {
		return payments.Event{}, payments.ErrInvalid
	}
	if e.Kind == 0 {
		return e, payments.ErrIgnored
	}
	return e, nil
}

type testIdentity struct{}

func (testIdentity) Name() string { return "test" }
func (testIdentity) Authenticate(r *http.Request) (identity.Identity, error) {
	c, err := r.Cookie("session")
	if err != nil {
		return identity.Identity{}, identity.ErrNoSession
	}
	return identity.Identity{Subject: c.Value, Email: c.Value + "@example.com", EmailVerified: true, SignedInAt: time.Now()}, nil
}
func (testIdentity) Logout(http.ResponseWriter, *http.Request) error { return nil }
func (testIdentity) Delete(context.Context, *http.Request) error     { return nil }

// jobs is an app table holding reservations, the way an app keeps holds.
var jobs = app.Migrations{Namespace: "app", Files: fstest.MapFS{"001_jobs.sql": {Data: []byte(`
	CREATE TABLE jobs (
	    id text PRIMARY KEY,
	    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
	    reserved_cents bigint NOT NULL,
	    done boolean NOT NULL DEFAULT false
	);`)}}}

type fixture struct {
	a        *app.App
	accounts *accounts.Accounts
	credits  *Credits
	pay      *testPayments
	r        *gin.Engine
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{pay: &testPayments{}}
	f.a = apptest.New(t, app.Config{Name: "test", Migrations: []app.Migrations{accounts.Migrations, Migrations, jobs}})
	var err error
	if f.accounts, err = accounts.New(f.a, testIdentity{}, accounts.Config{}); err != nil {
		t.Fatal(err)
	}
	f.credits, err = New(f.a, f.accounts, Config{
		Payments: f.pay,
		Policy:   Policy{MinPurchaseCents: 500, MaxPurchaseCents: 100_000, FeeBasisPoints: 390, FeeFixedCents: 40},
		Holds: func(ctx context.Context, tx *sql.Tx, accountID string) (int64, error) {
			var held int64
			err := tx.QueryRowContext(ctx, `
				SELECT COALESCE(sum(reserved_cents), 0)
				FROM jobs
				WHERE account_id = $1 AND NOT done`, accountID,
			).Scan(&held)
			return held, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.r, err = web.New(web.Config{PublicURL: "http://localhost:8080", SecretKey: strings.Repeat("s", 32)}); err != nil {
		t.Fatal(err)
	}
	f.r.GET("/token", func(c *gin.Context) { c.String(http.StatusOK, web.CSRFToken(c)) })
	f.credits.Mount(f.r.Group("/account", f.accounts.RequireSignIn), ui.Default(ui.Design{Name: "Test"}, ui.NewAssets("/assets")))
	f.r.POST("/webhooks/payments", f.credits.Webhook)
	f.r.POST("/delete-account", f.accounts.RequireSignIn, func(c *gin.Context) {
		if err := f.accounts.Delete(c); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.Status(http.StatusNoContent)
	})
	return f
}

func (f *fixture) deleteAccount(t *testing.T, subject string) {
	t.Helper()
	if rec := f.browser(http.MethodPost, "/delete-account", subject, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete account: %d %s", rec.Code, rec.Body.String())
	}
}

// account creates an account by signing in as subject.
func (f *fixture) account(t *testing.T, subject string) string {
	t.Helper()
	f.browser(http.MethodGet, "/account/credits", subject, nil)
	var id string
	if err := f.a.DB().QueryRow(`SELECT account_id FROM account_identities WHERE subject = $1`, subject).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) browser(method, path, session string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	tok := httptest.NewRecorder()
	f.r.ServeHTTP(tok, httptest.NewRequest(http.MethodGet, "http://localhost:8080/token", nil))
	if form == nil {
		form = url.Values{}
	}
	form.Set(web.CSRFField, tok.Body.String())
	req := httptest.NewRequest(method, "http://localhost:8080"+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://localhost:8080")
	for _, c := range tok.Result().Cookies() {
		req.AddCookie(c)
	}
	req.AddCookie(&http.Cookie{Name: "session", Value: session})
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) webhook(e payments.Event, signature string) int {
	body, _ := json.Marshal(e)
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/webhooks/payments", strings.NewReader(string(body)))
	req.Header.Set("X-Signature", signature)
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	return rec.Code
}

func (f *fixture) balance(t *testing.T, accountID string) int64 {
	t.Helper()
	s, err := f.credits.Summary(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	return s.BalanceCents
}

func (f *fixture) reviews(t *testing.T) []string {
	t.Helper()
	rows, err := f.a.DB().Query(`SELECT reason FROM payment_reviews ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var reasons []string
	for rows.Next() {
		var r string
		rows.Scan(&r)
		reasons = append(reasons, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return reasons
}

func TestDebitsAreIdempotentAndBounded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada := f.account(t, "ada")
	if err := f.credits.Adjust(ctx, ada, 1000, "welcome", "Welcome credit"); err != nil {
		t.Fatal(err)
	}
	if err := f.credits.Adjust(ctx, ada, 1000, "welcome", "Welcome credit"); err != nil {
		t.Fatalf("repeated adjustment: %v", err)
	}
	if err := f.credits.Debit(ctx, ada, 100, "job:1", "Search"); err != nil {
		t.Fatal(err)
	}
	if err := f.credits.Debit(ctx, ada, 100, "job:1", "Search"); err != nil {
		t.Fatalf("repeated debit: %v", err)
	}
	if err := f.credits.Debit(ctx, ada, 101, "job:1", "Search"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed amount: %v", err)
	}
	if err := f.credits.Debit(ctx, ada, 901, "job:2", ""); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("overdraft: %v", err)
	}
	if got := f.balance(t, ada); got != 900 {
		t.Fatalf("balance %d", got)
	}
	for _, ref := range []string{"", strings.Repeat("r", 129)} {
		if err := f.credits.Debit(ctx, ada, 1, ref, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("reference %q: %v", ref, err)
		}
	}
	if err := f.credits.Debit(ctx, "not-an-account", 1, "x", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown account: %v", err)
	}
	// A recorded debit can be repeated after the balance ran out.
	if err := f.credits.Debit(ctx, ada, 900, "job:3", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.credits.Debit(ctx, ada, 100, "job:1", "Search"); err != nil {
		t.Fatalf("repeated debit at zero balance: %v", err)
	}
}

func TestConcurrentDebitsNeverOverdraw(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada := f.account(t, "ada")
	f.credits.Adjust(ctx, ada, 1000, "seed", "")
	var ok atomic.Int64
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			err := f.credits.Debit(ctx, ada, 100, "job:"+string(rune('A'+i)), "")
			if err == nil {
				ok.Add(1)
			} else if !errors.Is(err, ErrInsufficient) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 10 || f.balance(t, ada) != 0 {
		t.Fatalf("%d debits succeeded, balance %d", ok.Load(), f.balance(t, ada))
	}
}

func TestHoldsAndSharedTransactions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada := f.account(t, "ada")
	f.credits.Adjust(ctx, ada, 1000, "seed", "")

	reserve := func(id string, cents int64) error {
		return f.credits.WithAccount(ctx, ada, func(tx *Tx) error {
			avail, err := tx.Available()
			if err != nil {
				return err
			}
			if cents > avail {
				return ErrInsufficient
			}
			_, err = tx.SQL().ExecContext(ctx, `INSERT INTO jobs (id, account_id, reserved_cents) VALUES ($1, $2, $3)`, id, ada, cents)
			return err
		})
	}
	if err := reserve("a", 700); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.credits.Summary(ctx, ada); s.HeldCents != 700 || s.AvailableCents != 300 {
		t.Fatalf("summary %+v", s)
	}
	if err := f.credits.Debit(ctx, ada, 301, "direct", ""); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("a debit ignored the hold: %v", err)
	}
	// Capturing the job releases its hold and debits in one transaction.
	err := f.credits.WithAccount(ctx, ada, func(tx *Tx) error {
		if _, err := tx.SQL().ExecContext(ctx, `UPDATE jobs SET done = true WHERE id = 'a'`); err != nil {
			return err
		}
		return tx.Debit(650, "job:a", "Job a")
	})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := f.credits.Summary(ctx, ada); s.BalanceCents != 350 || s.HeldCents != 0 {
		t.Fatalf("after settling %+v", s)
	}
	// An error rolls back the app's rows and the ledger together.
	failure := errors.New("upstream failed")
	err = f.credits.WithAccount(ctx, ada, func(tx *Tx) error {
		tx.SQL().ExecContext(ctx, `INSERT INTO jobs (id, account_id, reserved_cents) VALUES ('b', $1, 100)`, ada)
		if err := tx.Debit(50, "job:b", ""); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) || f.balance(t, ada) != 350 {
		t.Fatalf("rollback: %v, balance %d", err, f.balance(t, ada))
	}
	var jobsLeft int
	f.a.DB().QueryRow(`SELECT count(*) FROM jobs WHERE id = 'b'`).Scan(&jobsLeft)
	if jobsLeft != 0 {
		t.Fatal("the app's row survived the rollback")
	}
}

func TestLedgerIsAppendOnlyAndAnonymisedOnDeletion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada := f.account(t, "ada")
	f.credits.Adjust(ctx, ada, 1000, "seed", "Personal note")
	for _, stmt := range []string{
		`UPDATE credit_ledger SET amount_cents = 5`,
		`UPDATE credit_ledger SET description = 'changed'`,
		`DELETE FROM credit_ledger`,
	} {
		if _, err := f.a.DB().Exec(stmt); err == nil {
			t.Errorf("%s succeeded", stmt)
		}
	}
	history, _, _ := f.credits.History(ctx, ada, 1, 10)
	if len(history) != 1 || history[0].Description != "Personal note" || history[0].Kind != KindAdjustment {
		t.Fatalf("history %+v", history)
	}

	if err := f.credits.Debit(ctx, ada, 300, "job", "Job"); err != nil {
		t.Fatal(err)
	}
	f.deleteAccount(t, "ada")
	var total, withAccount, withDescription int64
	f.a.DB().QueryRow(`
		SELECT COALESCE(sum(amount_cents), 0),
		       count(*) FILTER (WHERE account_id IS NOT NULL),
		       count(*) FILTER (WHERE description <> '')
		FROM credit_ledger`).Scan(&total, &withAccount, &withDescription)
	if total != 0 || withAccount != 0 || withDescription != 0 {
		t.Fatalf("after deletion: sum %d, linked %d, described %d", total, withAccount, withDescription)
	}
}

func TestPurchaseAndWebhooks(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada := f.account(t, "ada")
	account, _ := f.accounts.Get(ctx, ada)

	if _, err := f.credits.Purchase(ctx, account, 499, "https://app.example/credits"); !errors.Is(err, ErrAmount) {
		t.Fatalf("below minimum: %v", err)
	}
	p, err := f.credits.Purchase(ctx, account, 1000, "https://app.example/credits")
	if err != nil {
		t.Fatal(err)
	}
	if p.ChargeCents != 1083 || p.CheckoutURL != "https://pay.example/co_"+p.IntentID {
		t.Fatalf("purchase %+v", p)
	}
	if c := f.pay.checkouts[0]; c.AmountCents != 1083 || c.Reference != p.IntentID || c.Email != "ada@example.com" {
		t.Fatalf("checkout %+v", c)
	}

	paid := payments.Event{ID: "evt_paid", Kind: payments.Paid, Reference: p.IntentID, CheckoutID: "co_" + p.IntentID, OrderID: "ord_1", AmountCents: 1083, PaidCents: 1310, Currency: "EUR"}
	if code := f.webhook(paid, "forged"); code != http.StatusBadRequest {
		t.Fatalf("forged webhook: %d", code)
	}
	if code := f.webhook(payments.Event{ID: "evt_other"}, "valid"); code != http.StatusOK {
		t.Fatalf("ignored event: %d", code)
	}
	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			if code := f.webhook(paid, "valid"); code != http.StatusOK {
				t.Errorf("paid: %d", code)
			}
		})
	}
	wg.Wait()
	if got := f.balance(t, ada); got != 1000 {
		t.Fatalf("balance after repeated payment events: %d", got)
	}

	// Spend everything, then the refund takes the balance below zero.
	f.credits.Debit(ctx, ada, 1000, "spend", "")
	partial := payments.Event{ID: "evt_partial", Kind: payments.Refunded, Reference: p.IntentID, AmountCents: 500, Currency: "EUR"}
	f.webhook(partial, "valid")
	refund := payments.Event{ID: "evt_refund", Kind: payments.Refunded, Reference: p.IntentID, AmountCents: 1310, Currency: "EUR"}
	f.webhook(refund, "valid")
	f.webhook(refund, "valid")
	dispute := payments.Event{ID: "evt_dispute", Kind: payments.Disputed, OrderID: "ord_1", AmountCents: 1310, Currency: "EUR"}
	f.webhook(dispute, "valid")
	if got := f.balance(t, ada); got != -1000 {
		t.Fatalf("balance after refund: %d", got)
	}
	if got := f.reviews(t); len(got) != 1 || got[0] != "partial_reversal" {
		t.Fatalf("reviews %v", got)
	}
}

func TestWebhooksThatNeedAnOperator(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada := f.account(t, "ada")
	account, _ := f.accounts.Get(ctx, ada)
	p, _ := f.credits.Purchase(ctx, account, 1000, "https://app.example/credits")

	f.webhook(payments.Event{ID: "evt_early", Kind: payments.Refunded, Reference: p.IntentID, AmountCents: 1083, Currency: "EUR"}, "valid")
	f.webhook(payments.Event{ID: "evt_unknown", Kind: payments.Paid, Reference: "00000000-0000-0000-0000-000000000000", AmountCents: 1083, PaidCents: 1083, Currency: "EUR"}, "valid")
	f.webhook(payments.Event{ID: "evt_cheap", Kind: payments.Paid, Reference: p.IntentID, AmountCents: 1, PaidCents: 1, Currency: "EUR"}, "valid")
	f.webhook(payments.Event{ID: "evt_flagged", Kind: payments.Paid, Reference: p.IntentID, Review: "checkout_not_paid"}, "valid")
	f.webhook(payments.Event{ID: "evt_cheap", Kind: payments.Paid, Reference: p.IntentID, AmountCents: 1, PaidCents: 1, Currency: "EUR"}, "valid")
	if got := f.balance(t, ada); got != 0 {
		t.Fatalf("balance %d", got)
	}
	want := []string{"reversal_before_payment", "unknown_payment", "amount_mismatch", "checkout_not_paid"}
	if got := f.reviews(t); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reviews %v", got)
	}

	// A payment that completes after its account was deleted is recorded for a refund.
	q, _ := f.credits.Purchase(ctx, account, 2000, "https://app.example/credits")
	f.deleteAccount(t, "ada")
	if code := f.webhook(payments.Event{ID: "evt_late", Kind: payments.Paid, Reference: q.IntentID, AmountCents: q.ChargeCents, PaidCents: q.ChargeCents, Currency: "EUR"}, "valid"); code != http.StatusOK {
		t.Fatalf("late payment: %d", code)
	}
	f.webhook(payments.Event{ID: "evt_late_refund", Kind: payments.Refunded, Reference: q.IntentID, AmountCents: q.ChargeCents, Currency: "EUR"}, "valid")
	var status string
	f.a.DB().QueryRow(`SELECT provider_status FROM payment_intents WHERE id = $1`, q.IntentID).Scan(&status)
	if got := f.reviews(t); got[len(got)-1] != "paid_after_account_deleted" || status != "refunded" {
		t.Fatalf("late payment: reviews %v, status %q", got, status)
	}

	if err := f.credits.reviewsCommand(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.credits.resolveCommand(ctx, []string{"1", "Refunded in the Creem dashboard"}); err != nil {
		t.Fatal(err)
	}
	if err := f.credits.resolveCommand(ctx, []string{"1", "again"}); err == nil {
		t.Fatal("resolved a review twice")
	}
}

func TestCaptureSettlesAHeldCharge(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada := f.account(t, "ada")
	f.credits.Adjust(ctx, ada, 100, "seed", "")
	err := f.credits.WithAccount(ctx, ada, func(tx *Tx) error {
		_, err := tx.SQL().ExecContext(ctx, `INSERT INTO jobs (id, account_id, reserved_cents) VALUES ('a', $1, 100)`, ada)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// A correction lowers the balance while the job runs; the finished job is still
	// charged in full.
	f.credits.Adjust(ctx, ada, -60, "correction", "")
	capture := func() error {
		return f.credits.WithAccount(ctx, ada, func(tx *Tx) error {
			if _, err := tx.SQL().ExecContext(ctx, `UPDATE jobs SET done = true WHERE id = 'a'`); err != nil {
				return err
			}
			return tx.Capture(100, "job:a", "Job a")
		})
	}
	if err := capture(); err != nil {
		t.Fatal(err)
	}
	if err := capture(); err != nil {
		t.Fatalf("repeated capture: %v", err)
	}
	if s, _ := f.credits.Summary(ctx, ada); s.BalanceCents != -60 || s.AvailableCents != -60 {
		t.Fatalf("after capture %+v", s)
	}
	history, _, _ := f.credits.History(ctx, ada, 1, 10)
	if history[0].Kind != KindUsage || history[0].Reference != "job:a" || Label(history[0]) != "Job a" {
		t.Fatalf("history %+v", history)
	}
}

func TestHistoryPages(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada := f.account(t, "ada")
	for i := range 25 {
		f.credits.Adjust(ctx, ada, int64(i+1), "seed:"+string(rune('a'+i)), "")
	}
	first, more, _ := f.credits.History(ctx, ada, 1, 10)
	if len(first) != 10 || !more || first[0].AmountCents != 25 {
		t.Fatalf("first page: %d entries, more %v, newest %d", len(first), more, first[0].AmountCents)
	}
	last, more, _ := f.credits.History(ctx, ada, 3, 10)
	if len(last) != 5 || more || last[4].AmountCents != 1 {
		t.Fatalf("last page: %d entries, more %v", len(last), more)
	}
	if same, _, _ := f.credits.History(ctx, ada, 0, 10); same[0].ID != first[0].ID {
		t.Fatal("page 0 is not the first page")
	}
	if _, _, err := f.credits.History(ctx, ada, 1, 101); err == nil {
		t.Fatal("accepted a page of 101")
	}
}

func TestCreditsPage(t *testing.T) {
	f := newFixture(t)
	ada := f.account(t, "ada")
	f.credits.Adjust(context.Background(), ada, 1234, "seed", "Welcome credit")

	rec := f.browser(http.MethodGet, "/account/credits", "ada", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "€12.34") || !strings.Contains(rec.Body.String(), `hx-get="/account/credits/summary"`) {
		t.Fatalf("page: %d", rec.Code)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https://pay.example") {
		t.Errorf("CSP %q", csp)
	}
	if body := f.browser(http.MethodGet, "/account/credits/summary", "ada", nil).Body.String(); !strings.Contains(body, "Welcome credit") || !strings.Contains(body, "+€12.34") {
		t.Errorf("transactions: %s", body)
	}
	if body := f.browser(http.MethodGet, "/account/credits/balance", "ada", nil).Body.String(); !strings.Contains(body, "€12.34") {
		t.Errorf("balance: %s", body)
	}
	if body := f.browser(http.MethodGet, "/account/credits/quote?amount=10", "ada", nil).Body.String(); !strings.Contains(body, "€10.83") {
		t.Errorf("quote: %s", body)
	}
	if body := f.browser(http.MethodGet, "/account/credits/quote?amount=1", "ada", nil).Body.String(); !strings.Contains(body, "Buy between") {
		t.Errorf("small quote: %s", body)
	}
	if rec := f.browser(http.MethodGet, "/account/credits/payment-status", "ada", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("status without a purchase: %d", rec.Code)
	}

	rec = f.browser(http.MethodPost, "/account/credits", "ada", url.Values{"amount": {"10"}})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "https://pay.example/") {
		t.Fatalf("checkout: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	c := f.pay.checkouts[0]
	if c.AmountCents != 1083 || c.SuccessURL != "http://localhost:8080/account/credits" {
		t.Fatalf("checkout %+v", c)
	}
	var pending *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == pendingCookie {
			pending = cookie
		}
	}
	if pending == nil {
		t.Fatal("no pending payment cookie")
	}
	if body := f.browser(http.MethodGet, "/account/credits/payment-status", "ada", nil, pending).Body.String(); !strings.Contains(body, "every 2s") {
		t.Fatalf("pending status: %s", body)
	}
	if rec := f.browser(http.MethodGet, "/account/credits/payment-status", "grace", nil, pending); rec.Code != http.StatusNoContent {
		t.Fatalf("another account's purchase: %d", rec.Code)
	}
	f.webhook(payments.Event{ID: "evt", Kind: payments.Paid, Reference: c.Reference, AmountCents: 1083, PaidCents: 1083, Currency: "EUR"}, "valid")
	rec = f.browser(http.MethodGet, "/account/credits/payment-status", "ada", nil, pending)
	if !strings.Contains(rec.Body.String(), "Payment confirmed") || rec.Header().Get("HX-Trigger") != "credits-confirmed" {
		t.Fatalf("confirmed status: %q %s", rec.Header().Get("HX-Trigger"), rec.Body.String())
	}

	if rec := f.browser(http.MethodPost, "/account/credits", "ada", url.Values{"amount": {"lots"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad amount: %d", rec.Code)
	}
	htmx := f.browserHTMX(http.MethodPost, "/account/credits", "ada", url.Values{"amount": {"1"}})
	if htmx.Code != http.StatusUnprocessableEntity || !strings.Contains(htmx.Body.String(), `id="checkout-error"`) {
		t.Fatalf("HTMX bad amount: %d %s", htmx.Code, htmx.Body.String())
	}
}

func (f *fixture) browserHTMX(method, path, session string, form url.Values) *httptest.ResponseRecorder {
	tok := httptest.NewRecorder()
	f.r.ServeHTTP(tok, httptest.NewRequest(http.MethodGet, "http://localhost:8080/token", nil))
	form.Set(web.CSRFField, tok.Body.String())
	req := httptest.NewRequest(method, "http://localhost:8080"+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://localhost:8080")
	req.Header.Set("HX-Request", "true")
	for _, c := range tok.Result().Cookies() {
		req.AddCookie(c)
	}
	req.AddCookie(&http.Cookie{Name: "session", Value: session})
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	return rec
}

func TestWithoutPayments(t *testing.T) {
	a := apptest.New(t, app.Config{Name: "test", Migrations: []app.Migrations{accounts.Migrations, Migrations}})
	accts, err := accounts.New(a, nil, accounts.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cr, err := New(a, accts, Config{})
	if err != nil {
		t.Fatalf("credits without payments: %v", err)
	}
	if _, err := cr.Quote(1000); !errors.Is(err, ErrPurchasesOff) || cr.Purchasing() {
		t.Fatalf("quote: %v", err)
	}
	r := gin.New()
	r.POST("/webhook", cr.Webhook)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("{}")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("webhook: %d", rec.Code)
	}
}
