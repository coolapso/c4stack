//go:build integration

// Package porttest proves that Searchbase Cloud, the first app built on these
// conventions, moves onto C4Stack with one ordinary release. Its migrate job converges
// the database without moving or copying data; its sign-ins, keys, balances, purchases
// and expiry warnings carry over; and the code it replaces keeps working against the
// converged database while a deployment switches over.
//
// testdata/searchbase holds Searchbase's migrations 001 to 004 as Searchbase applied
// them, and 005, the one migration Searchbase adds for the move.
package porttest_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/apikeys"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/credits"
	"github.com/coolapso/c4stack/credits/expiry"
	"github.com/coolapso/c4stack/email"
	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/internal/postgres"
	"github.com/coolapso/c4stack/internal/testdb"
	"github.com/coolapso/c4stack/payments/creem"
	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

//go:embed testdata/searchbase/*.sql
var testdata embed.FS

var quiet = slog.New(slog.DiscardHandler)

func files(t *testing.T, dir string) fs.FS {
	t.Helper()
	sub, err := fs.Sub(testdata, dir)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func searchbaseFiles(t *testing.T) fs.FS { return files(t, "testdata/searchbase") }

// config lists Searchbase's own history first, because it created the tables the
// modules adopt.
func config(t *testing.T) app.Config {
	return app.Config{Name: "searchbase", Migrations: []app.Migrations{
		{Namespace: "searchbase", Files: searchbaseFiles(t)},
		accounts.Migrations, apikeys.Migrations, credits.Migrations, expiry.Migrations,
	}}
}

func sets(cfg app.Config) []postgres.Set {
	var out []postgres.Set
	for _, m := range cfg.Migrations {
		out = append(out, postgres.Set{Namespace: m.Namespace, Files: m.Files})
	}
	return out
}

func open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := postgres.Open(context.Background(), testdb.New(t), quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// applySearchbase applies Searchbase's migrations 001 to 004 the way its own runner
// did: one transaction, recorded in a history without namespaces.
func applySearchbase(t *testing.T, db *sql.DB) {
	t.Helper()
	files := searchbaseFiles(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec(t, tx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		    version BIGINT PRIMARY KEY,
		    name TEXT NOT NULL UNIQUE,
		    checksum TEXT NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
		    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	for i, name := range []string{"001_initial_schema.sql", "002_gateway_billing.sql", "003_initial_service_prices.sql", "004_credit_expiry.sql"} {
		body, err := fs.ReadFile(files, name)
		if err != nil {
			t.Fatal(err)
		}
		exec(t, tx, string(body))
		sum := sha256.Sum256(body)
		exec(t, tx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			i+1, strings.TrimSuffix(name[4:], ".sql"), hex.EncodeToString(sum[:]))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func exec(t *testing.T, db execer, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
}

func scalar[T any](t *testing.T, db *sql.DB, query string, args ...any) T {
	t.Helper()
	var v T
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&v); err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
	return v
}

// The search price Searchbase's migration 003 inserts.
const searchPrice = "a4cd8a3d-d811-4ce6-9bad-4f9e2549ac4e"

// world is the data Searchbase wrote before the move.
type world struct {
	ada, grace     string
	rawKey         string
	paidIntent     string // a purchase of €25.00 for €26.28, paid
	inflightIntent string // a checkout opened for €10.00 but not paid yet
}

// seed writes data with Searchbase's own statements.
func seed(t *testing.T, db *sql.DB) world {
	t.Helper()
	var w world
	account := func(email, subject string) string {
		id := scalar[string](t, db, `INSERT INTO accounts (primary_email) VALUES ($1) RETURNING id`, email)
		exec(t, db, `
			INSERT INTO account_identities (account_id, provider, subject, email, email_verified)
			VALUES ($1, 'hanko', $2, $3, true)`, id, subject, email)
		return id
	}
	w.ada = account("ada@example.com", "sub-ada")
	w.grace = account("grace@example.com", "sub-grace")

	w.rawKey = "sb_" + base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	hash := sha256.Sum256([]byte(w.rawKey))
	exec(t, db, `
		INSERT INTO api_keys (account_id, name, key_hash, key_suffix, created_at, last_used_at, revoked_at)
		VALUES ($1, 'Research agent', $2, $3, now(), NULL, NULL)`, w.ada, hash[:], w.rawKey[len(w.rawKey)-6:])

	intent := func(credit, fee int64) string {
		return scalar[string](t, db, `
			INSERT INTO payment_intents (
				account_id, idempotency_key, credit_amount_cents, fee_amount_cents, checkout_amount_cents, provider_status
			) VALUES ($1, gen_random_uuid(), $2, $3, $4, 'created')
			RETURNING id`, w.ada, credit, fee, credit+fee)
	}
	w.paidIntent = intent(2500, 128)
	exec(t, db, `
		UPDATE payment_intents
		SET provider_checkout_id = 'ch_1', provider_status = 'pending', updated_at = now()
		WHERE id = $1 AND account_id = $2 AND provider_checkout_id IS NULL`, w.paidIntent, w.ada)
	exec(t, db, `
		WITH updated AS (
			UPDATE payment_intents
			SET provider_status = 'paid', updated_at = now(), paid_at = now() - interval '1 day',
				provider_order_id = 'ord_1', paid_amount_cents = 2628
			WHERE id = $1 AND provider_checkout_id = 'ch_1'
			RETURNING id, account_id, credit_amount_cents
		)
		INSERT INTO credit_ledger (account_id, payment_intent_id, provider_event_id, amount_cents, kind, created_at)
		SELECT account_id, id, 'evt_paid', credit_amount_cents, 'top_up', now() - interval '1 day'
		FROM updated
		ON CONFLICT DO NOTHING`, w.paidIntent)
	w.inflightIntent = intent(1000, 59)
	exec(t, db, `
		UPDATE payment_intents
		SET provider_checkout_id = 'ch_2', provider_status = 'pending', updated_at = now()
		WHERE id = $1`, w.inflightIntent)

	// One captured search, and one still running.
	captured := reserve(t, db, w.ada)
	exec(t, db, `UPDATE credit_reservations SET status = 'captured', settled_at = statement_timestamp() WHERE id = $1`, captured)
	exec(t, db, `
		INSERT INTO credit_ledger (account_id, reservation_id, amount_cents, kind)
		VALUES ($1, $2, -3, 'usage')`, w.ada, captured)
	exec(t, db, `
		INSERT INTO usage_events (
			reservation_id, request_id, operation, transport, outcome, status_class, duration_ms,
			reserved_amount_cents, charged_amount_cents, created_at
		) VALUES ($1, gen_random_uuid(), 'search', 'rest', 'success', 2, 120, 3, 3, statement_timestamp())`, captured)
	reserve(t, db, w.ada)

	// Grace's credit went unused for 13 months, and she was warned 31 days ago.
	exec(t, db, `
		INSERT INTO credit_ledger (account_id, amount_cents, kind, created_at)
		VALUES ($1, 700, 'adjustment', now() - interval '13 months')`, w.grace)
	exec(t, db, `UPDATE accounts SET credit_expiry_warned_at = now() - interval '31 days' WHERE id = $1`, w.grace)
	return w
}

// reserve holds a search's price the way Searchbase's gateway does.
func reserve(t *testing.T, db *sql.DB, accountID string) string {
	t.Helper()
	return scalar[string](t, db, `
		INSERT INTO credit_reservations (
			id, account_id, api_key_id, service_price_id, amount_cents, status, created_at, expires_at
		)
		SELECT gen_random_uuid(), $1, (SELECT id FROM api_keys WHERE account_id = $1 LIMIT 1), $2, 3,
		       'pending', statement_timestamp(), statement_timestamp() + interval '5 minutes'
		RETURNING id`, accountID, searchPrice)
}

// holds is Searchbase's Holds: pending reservations that have not expired.
func holds(ctx context.Context, tx *sql.Tx, accountID string) (int64, error) {
	var held int64
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount_cents), 0)
		FROM credit_reservations
		WHERE account_id = $1 AND status = 'pending' AND expires_at > statement_timestamp()`, accountID,
	).Scan(&held)
	return held, err
}

// hankoSessions signs in whoever the hanko cookie names, as provider "hanko".
type hankoSessions struct {
	mu       sync.Mutex
	sessions map[string]identity.Identity
}

func (h *hankoSessions) Name() string { return "hanko" }
func (h *hankoSessions) Authenticate(r *http.Request) (identity.Identity, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, err := r.Cookie("hanko")
	if err != nil {
		return identity.Identity{}, identity.ErrNoSession
	}
	id, ok := h.sessions[c.Value]
	if !ok {
		return identity.Identity{}, identity.ErrNoSession
	}
	return id, nil
}
func (h *hankoSessions) Logout(http.ResponseWriter, *http.Request) error { return nil }
func (h *hankoSessions) Delete(_ context.Context, r *http.Request) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, _ := r.Cookie("hanko")
	delete(h.sessions, c.Value)
	return nil
}

// outbox delivers every message as it is sent.
type outbox struct{ sent []email.Message }

func (o *outbox) Send(_ context.Context, m email.Message) (string, error) {
	o.sent = append(o.sent, m)
	return fmt.Sprint(len(o.sent)), nil
}

func (o *outbox) Delivery(context.Context, string) (email.Delivery, time.Time, error) {
	return email.Delivered, time.Now(), nil
}

const webhookSecret = "whsec_test"

// ported is Searchbase after the move: its database migrated by C4Stack, and the
// modules wired the way its web server wires them.
type ported struct {
	db      *sql.DB
	a       *app.App
	accts   *accounts.Accounts
	keys    *apikeys.Keys
	credits *credits.Credits
	expiry  *expiry.Expiry
	hanko   *hankoSessions
	mail    *outbox
	r       *gin.Engine
}

func port(t *testing.T, db *sql.DB) *ported {
	t.Helper()
	ctx := context.Background()
	cfg := config(t)
	if err := postgres.Migrate(ctx, db, sets(cfg), quiet); err != nil {
		t.Fatal(err)
	}
	p := &ported{db: db, a: app.New(cfg, db), mail: &outbox{}, hanko: &hankoSessions{sessions: map[string]identity.Identity{}}}
	p.a.SetLogger(quiet)
	var err error
	if p.accts, err = accounts.New(p.a, p.hanko, accounts.Config{}); err != nil {
		t.Fatal(err)
	}
	if p.keys, err = apikeys.New(p.a, apikeys.Config{Prefix: "sb"}); err != nil {
		t.Fatal(err)
	}
	payments, err := creem.New(creem.Config{APIKey: "creem_test_key", WebhookSecret: webhookSecret, ProductID: "prod_credits"})
	if err != nil {
		t.Fatal(err)
	}
	p.credits, err = credits.New(p.a, p.accts, credits.Config{
		Payments: payments,
		Policy:   credits.Policy{MinPurchaseCents: 500, MaxPurchaseCents: 99_999_999, FeeBasisPoints: 390, FeeFixedCents: 30},
		Holds:    holds,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.expiry, err = expiry.New(p.a, p.credits, expiry.Config{
		Sender:     p.mail,
		Policy:     expiry.Policy{InactiveMonths: 12, NoticeDays: 30},
		AppName:    "Searchbase Cloud",
		CreditsURL: "https://searchbase.example/account/credits",
	})
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	if p.r, err = web.New(web.Config{PublicURL: "http://localhost:8080", SecretKey: strings.Repeat("s", 32)}); err != nil {
		t.Fatal(err)
	}
	layout := ui.Default(ui.Design{Name: "Searchbase Cloud"}, ui.NewAssets("/assets"))
	p.r.GET("/token", func(c *gin.Context) { c.String(http.StatusOK, web.CSRFToken(c)) })
	p.accts.Mount(p.r, layout)
	account := p.r.Group("/account", p.accts.RequireSignIn)
	p.keys.Mount(account.Group("/api-keys"), layout)
	p.credits.Mount(account.Group("/credits"), layout)
	p.r.POST("/webhooks/creem/payments", p.credits.Webhook)
	return p
}

func (p *ported) signIn(subject, email string) {
	p.hanko.mu.Lock()
	defer p.hanko.mu.Unlock()
	p.hanko.sessions[subject] = identity.Identity{Subject: subject, Email: email, EmailVerified: true, SignedInAt: time.Now()}
}

func (p *ported) browser(method, path, session string, form url.Values) *httptest.ResponseRecorder {
	tok := httptest.NewRecorder()
	p.r.ServeHTTP(tok, httptest.NewRequest(http.MethodGet, "http://localhost:8080/token", nil))
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
	req.AddCookie(&http.Cookie{Name: "hanko", Value: session})
	rec := httptest.NewRecorder()
	p.r.ServeHTTP(rec, req)
	return rec
}

// webhook delivers a Creem event, signed as Creem signs it.
func (p *ported) webhook(t *testing.T, body string) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/webhooks/creem/payments", strings.NewReader(body))
	req.Header.Set("creem-signature", hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	p.r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook: %d", rec.Code)
	}
}

func checkoutEvent(eventID, intentID, checkoutID, orderID string, cents int64) string {
	return fmt.Sprintf(`{"id":%q,"eventType":"checkout.completed","created_at":%d,"object":{
		"id":%q,"request_id":%q,"status":"completed","metadata":{"intent_id":%q},"product":{"id":"prod_credits"},
		"order":{"id":%q,"amount":%d,"amount_paid":%d,"currency":"EUR","status":"paid","type":"onetime"}}}`,
		eventID, time.Now().UnixMilli(), checkoutID, intentID, intentID, orderID, cents, cents)
}

func (p *ported) summary(t *testing.T, accountID string) credits.Summary {
	t.Helper()
	s, err := p.credits.Summary(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSearchbaseMovesOntoC4Stack(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	applySearchbase(t, db)
	w := seed(t, db)
	p := port(t, db)

	// The history now has namespaces, with Searchbase's own files under one.
	if n := scalar[int](t, db, `SELECT count(*) FROM schema_migrations WHERE namespace = 'searchbase'`); n != 5 {
		t.Fatalf("Searchbase's history has %d entries", n)
	}
	status, err := postgres.Report(ctx, db, sets(config(t)))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range status {
		if len(s.Pending) > 0 {
			t.Fatalf("%s has pending migrations: %v", s.Namespace, s.Pending)
		}
	}

	// Existing sign-ins find their accounts.
	p.signIn("sub-ada", "ada@example.com")
	if rec := p.browser(http.MethodGet, "/account/settings", "sub-ada", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ada@example.com") {
		t.Fatalf("settings: %d", rec.Code)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM accounts`); n != 2 {
		t.Fatalf("%d accounts after the first sign-in", n)
	}

	// Existing API keys keep working.
	key, err := p.keys.Authenticate(ctx, w.rawKey)
	if err != nil || key.AccountID != w.ada || key.Display() != "sb_••••"+w.rawKey[len(w.rawKey)-6:] {
		t.Fatalf("existing key: %+v %v", key, err)
	}

	// Balances and holds carry over: €25.00 bought, one search captured, one held.
	if s := p.summary(t, w.ada); s.BalanceCents != 2497 || s.HeldCents != 3 || s.AvailableCents != 2494 {
		t.Fatalf("summary %+v", s)
	}
	history, _, err := p.credits.History(ctx, w.ada, 1, 10)
	if err != nil || len(history) != 2 || credits.Label(history[0]) != "Usage" || credits.Label(history[1]) != "Credit purchase" {
		t.Fatalf("history %+v %v", history, err)
	}

	// A replayed payment event changes nothing; a checkout Searchbase opened is credited
	// when it is paid; a refund of an old purchase applies.
	p.webhook(t, checkoutEvent("evt_paid", w.paidIntent, "ch_1", "ord_1", 2628))
	if s := p.summary(t, w.ada); s.BalanceCents != 2497 {
		t.Fatalf("balance after a replayed payment: %d", s.BalanceCents)
	}
	p.webhook(t, checkoutEvent("evt_inflight", w.inflightIntent, "ch_2", "ord_2", 1059))
	if s := p.summary(t, w.ada); s.BalanceCents != 3497 {
		t.Fatalf("balance after the in-flight checkout: %d", s.BalanceCents)
	}
	p.webhook(t, fmt.Sprintf(`{"id":"evt_refund","eventType":"refund.created","object":{"id":"ref_1","status":"succeeded",
		"refund_amount":2628,"refund_currency":"EUR","checkout":{"id":"ch_1","request_id":%q,"product":"prod_credits"},
		"order":{"id":"ord_1"}}}`, w.paidIntent))
	if s := p.summary(t, w.ada); s.BalanceCents != 997 {
		t.Fatalf("balance after the refund: %d", s.BalanceCents)
	}
	if got := scalar[string](t, db, `SELECT provider_status FROM payment_intents WHERE id = $1`, w.paidIntent); got != "refunded" {
		t.Fatalf("refunded purchase: %s", got)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM payment_reviews`); n != 0 {
		t.Fatalf("%d payment reviews", n)
	}

	// The gateway captures a held search through the module.
	held := reserve(t, db, w.ada)
	err = p.credits.WithAccount(ctx, w.ada, func(tx *credits.Tx) error {
		if err := p.keys.LockActive(ctx, tx.SQL(), w.ada, key.ID); err != nil {
			return err
		}
		if _, err := tx.SQL().ExecContext(ctx, `
			UPDATE credit_reservations
			SET status = 'captured', settled_at = statement_timestamp()
			WHERE id = $1 AND status = 'pending'`, held,
		); err != nil {
			return err
		}
		return tx.Capture(3, held, "Search")
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := p.summary(t, w.ada); s.BalanceCents != 994 {
		t.Fatalf("balance after a capture: %d", s.BalanceCents)
	}

	// Searchbase never learned whether its warnings arrived, so Grace's warning from
	// before the move does not count: she is warned again, and her credit expires a
	// full notice period after that warning is delivered. Ada is not due.
	r, err := p.expiry.Run(ctx)
	if err != nil || r.Expired != 0 || r.Warned != 1 || len(p.mail.sent) != 1 || p.mail.sent[0].To != "grace@example.com" {
		t.Fatalf("expiry: %+v %v", r, err)
	}
	if _, err := p.expiry.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if p.summary(t, w.grace).BalanceCents != 700 ||
		!scalar[bool](t, db, `SELECT expires_at > now() + interval '29 days' FROM credit_expiry_warnings WHERE account_id = $1`, w.grace) {
		t.Fatal("Grace's credit is not kept for a full notice period after the new warning")
	}

	// A new identity with Ada's address, such as after a change of provider, joins her
	// account.
	p.signIn("sub-ada-2", "ada@example.com")
	p.browser(http.MethodGet, "/account/settings", "sub-ada-2", nil)
	if n := scalar[int](t, db, `SELECT count(*) FROM account_identities WHERE account_id = $1`, w.ada); n != 2 {
		t.Fatalf("Ada has %d identities", n)
	}

	searchbaseKeepsWorking(t, db, w)

	// Deletion anonymises what Searchbase must keep, and the address can start over.
	if rec := p.browser(http.MethodPost, "/account/delete", "sub-ada", url.Values{"confirmation": {"DELETE"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM credit_ledger WHERE account_id IS NULL AND description <> ''`); n != 0 {
		t.Fatalf("%d anonymised entries kept a description", n)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM credit_reservations WHERE account_id IS NOT NULL OR api_key_id IS NOT NULL`); n != 0 {
		t.Fatalf("%d reservations still refer to the account", n)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM usage_events`); n != 1 {
		t.Fatalf("usage events: %d", n)
	}
	p.signIn("sub-ada-3", "ada@example.com")
	p.browser(http.MethodGet, "/account/settings", "sub-ada-3", nil)
	again := scalar[string](t, db, `SELECT account_id FROM account_identities WHERE subject = 'sub-ada-3'`)
	if again == w.ada || p.summary(t, again).BalanceCents != 0 {
		t.Fatal("the address did not start over with an empty account")
	}
}

// searchbaseKeepsWorking runs the statements of the Searchbase release before the move
// against the converged database, as its other deployment does during a switch.
func searchbaseKeepsWorking(t *testing.T, db *sql.DB, w world) {
	t.Helper()
	// A first sign-in.
	id := scalar[string](t, db, `INSERT INTO accounts (primary_email) VALUES ('new@example.com') RETURNING id`)
	exec(t, db, `
		INSERT INTO account_identities (account_id, provider, subject, email, email_verified)
		VALUES ($1, 'hanko', 'sub-new', 'new@example.com', true)`, id)
	// A purchase and its payment.
	intent := scalar[string](t, db, `
		INSERT INTO payment_intents (
			account_id, idempotency_key, credit_amount_cents, fee_amount_cents, checkout_amount_cents, provider_status
		) VALUES ($1, gen_random_uuid(), 500, 50, 550, 'created')
		RETURNING id`, id)
	exec(t, db, `UPDATE payment_intents SET provider_checkout_id = 'ch_old', provider_status = 'pending' WHERE id = $1`, intent)
	exec(t, db, `
		WITH updated AS (
			UPDATE payment_intents
			SET provider_status = 'paid', updated_at = now(), paid_at = now(),
				provider_order_id = 'ord_old', paid_amount_cents = 550
			WHERE id = $1 AND provider_checkout_id = 'ch_old'
			RETURNING id, account_id, credit_amount_cents
		)
		INSERT INTO credit_ledger (account_id, payment_intent_id, provider_event_id, amount_cents, kind)
		SELECT account_id, id, 'evt_old', credit_amount_cents, 'top_up'
		FROM updated
		ON CONFLICT DO NOTHING`, intent)
	// A gateway capture.
	exec(t, db, `INSERT INTO api_keys (account_id, name, key_hash, key_suffix) VALUES ($1, 'old', $2, 'xxxxxx')`, id, []byte("hash-of-an-old-release-key-0000"))
	reservation := reserve(t, db, id)
	exec(t, db, `UPDATE credit_reservations SET status = 'captured', settled_at = statement_timestamp() WHERE id = $1`, reservation)
	exec(t, db, `INSERT INTO credit_ledger (account_id, reservation_id, amount_cents, kind) VALUES ($1, $2, -3, 'usage')`, id, reservation)
	// An expiry and a deletion with its forfeit.
	exec(t, db, `INSERT INTO credit_ledger (account_id, amount_cents, kind) VALUES ($1, -1, 'expiry')`, id)
	exec(t, db, `
		WITH locked AS (
			SELECT id FROM accounts WHERE id = $1 FOR UPDATE
		), balance AS (
			SELECT COALESCE(SUM(amount_cents), 0) AS cents
			FROM credit_ledger
			WHERE account_id = (SELECT id FROM locked)
		)
		INSERT INTO credit_ledger (account_id, amount_cents, kind)
		SELECT (SELECT id FROM locked), -cents, 'forfeit'
		FROM balance
		WHERE cents > 0`, id)
	exec(t, db, `DELETE FROM accounts WHERE id = $1`, id)
}

// TestSearchbaseSchemaConverges compares a ported database with a new one created
// from the same migrations, and checks that the modules' tables have every column a
// new C4Stack app has.
func TestSearchbaseSchemaConverges(t *testing.T) {
	ctx := context.Background()
	cfg := config(t)

	portedDB := open(t)
	applySearchbase(t, portedDB)
	if err := postgres.Migrate(ctx, portedDB, sets(cfg), quiet); err != nil {
		t.Fatal(err)
	}
	fresh := open(t)
	if err := postgres.Migrate(ctx, fresh, sets(cfg), quiet); err != nil {
		t.Fatal(err)
	}
	for _, q := range schemaQueries {
		got, want := describe(t, portedDB, q), describe(t, fresh, q)
		if len(want) == 0 {
			t.Fatalf("nothing to compare:\n%s", q)
		}
		if !slices.Equal(got, want) {
			t.Errorf("the ported schema differs from a new one:\nported: %v\nnew:    %v", diff(got, want), diff(want, got))
		}
	}

	modulesOnly := open(t)
	if err := postgres.Migrate(ctx, modulesOnly, sets(app.Config{Migrations: cfg.Migrations[1:]}), quiet); err != nil {
		t.Fatal(err)
	}
	ported := describe(t, portedDB, columns)
	for _, c := range describe(t, modulesOnly, columns) {
		if !slices.Contains(ported, c) {
			t.Errorf("the ported database lacks the module column %s", c)
		}
	}
}

const columns = `
	SELECT table_name || '.' || column_name || ' ' || data_type || ' ' || is_nullable
	FROM information_schema.columns
	WHERE table_schema = 'public' AND table_name <> 'schema_migrations'
	ORDER BY 1`

var schemaQueries = []string{
	`SELECT table_name || '.' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || COALESCE(column_default, '')
	FROM information_schema.columns
	WHERE table_schema = 'public' AND table_name <> 'schema_migrations'
	ORDER BY table_name, ordinal_position`,
	`SELECT conrelid::regclass::text || ' ' || conname || ' ' || pg_get_constraintdef(oid)
	FROM pg_constraint
	WHERE connamespace = 'public'::regnamespace AND conrelid <> 'schema_migrations'::regclass
	ORDER BY 1`,
	`SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND tablename <> 'schema_migrations' ORDER BY 1`,
	`SELECT tgrelid::regclass::text || ' ' || pg_get_triggerdef(oid) FROM pg_trigger WHERE NOT tgisinternal ORDER BY 1`,
}

func describe(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// diff returns the lines of a that b lacks.
func diff(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}
