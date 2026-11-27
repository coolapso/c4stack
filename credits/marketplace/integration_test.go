//go:build integration

package marketplace

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
	"testing"
	"testing/fstest"
	"time"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/apikeys"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/app/apptest"
	"github.com/coolapso/c4stack/credits"
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

// testMarket is a marketplace provider for tests. A webhook body is a testBody; the
// signature header must say "valid".
type testMarket struct{}

type testBody struct {
	Grant *payments.GrantRequest `json:"grant,omitempty"`
	Event *payments.Event        `json:"event,omitempty"`
}

func (testMarket) Name() string           { return "test" }
func (testMarket) CheckoutOrigin() string { return "https://pay.example" }
func (testMarket) CreateCheckout(context.Context, payments.Checkout) (payments.Session, error) {
	return payments.Session{ID: "co", URL: "https://pay.example/co"}, nil
}

func (testMarket) parse(h http.Header, body []byte) (testBody, error) {
	var b testBody
	if h.Get("X-Signature") != "valid" || json.Unmarshal(body, &b) != nil {
		return b, payments.ErrInvalid
	}
	return b, nil
}

func (p testMarket) ParseWebhook(h http.Header, body []byte) (payments.Event, error) {
	b, err := p.parse(h, body)
	if err != nil {
		return payments.Event{}, err
	}
	if b.Event == nil {
		return payments.Event{}, payments.ErrIgnored
	}
	return *b.Event, nil
}

func (p testMarket) ParseGrantRequest(h http.Header, body []byte) (payments.GrantRequest, error) {
	b, err := p.parse(h, body)
	if err != nil {
		return payments.GrantRequest{}, err
	}
	if b.Grant == nil {
		return payments.GrantRequest{}, payments.ErrIgnored
	}
	return *b.Grant, nil
}

func (testMarket) GrantReply(a payments.GrantAnswer) ([]byte, error) {
	return json.Marshal(a)
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

// work is the app's table: holds for requests in progress, and a history the claim
// hook moves.
var work = app.Migrations{Namespace: "app", Files: fstest.MapFS{"001_work.sql": {Data: []byte(`
	CREATE TABLE work (
	    id text PRIMARY KEY,
	    account_id uuid REFERENCES accounts (id) ON DELETE SET NULL,
	    reserved_cents bigint NOT NULL DEFAULT 0,
	    done boolean NOT NULL DEFAULT false
	);`)}}}

type fixture struct {
	a        *app.App
	accounts *accounts.Accounts
	credits  *credits.Credits
	keys     *apikeys.Keys
	m        *Marketplace
	r        *gin.Engine
	mu       sync.Mutex
	answered []Answered
}

const product = "prod_10"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	f.a = apptest.New(t, app.Config{Name: "test", Migrations: []app.Migrations{
		accounts.Migrations, apikeys.Migrations, credits.Migrations, Migrations, work,
	}})
	var err error
	if f.accounts, err = accounts.New(f.a, testIdentity{}, accounts.Config{}); err != nil {
		t.Fatal(err)
	}
	if f.keys, err = apikeys.New(f.a, apikeys.Config{Prefix: "acme"}); err != nil {
		t.Fatal(err)
	}
	f.credits, err = credits.New(f.a, f.accounts, credits.Config{
		Payments: testMarket{},
		Policy:   credits.Policy{MinPurchaseCents: 500, MaxPurchaseCents: 100_000, FeeBasisPoints: 390, FeeFixedCents: 40},
		Holds: func(ctx context.Context, tx *sql.Tx, accountID string) (int64, error) {
			var held int64
			err := tx.QueryRowContext(ctx, `
				SELECT COALESCE(sum(reserved_cents), 0)
				FROM work
				WHERE account_id = $1 AND NOT done`, accountID,
			).Scan(&held)
			return held, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.m, err = New(f.a, f.accounts, f.credits, Config{
		Payments:       testMarket{},
		Products:       []Product{{ProviderID: product, CreditCents: 1000, PriceCents: 1083}},
		Keys:           f.keys,
		KeyName:        "Marketplace agent",
		KeyEnv:         "ACME_API_KEY",
		ClaimEnv:       "ACME_CLAIM_URL",
		ClaimURL:       "http://localhost:8080/account/claim",
		SealKey:        []byte(strings.Repeat("k", 32)),
		InactiveMonths: 12,
		OnAnswer: func(_ context.Context, a Answered) {
			f.mu.Lock()
			f.answered = append(f.answered, a)
			f.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.m.OnClaim(func(ctx context.Context, tx *sql.Tx, from, to string) error {
		_, err := tx.ExecContext(ctx, `UPDATE work SET account_id = $2 WHERE account_id = $1`, from, to)
		return err
	})
	if f.r, err = web.New(web.Config{PublicURL: "http://localhost:8080", SecretKey: strings.Repeat("s", 32)}); err != nil {
		t.Fatal(err)
	}
	f.r.GET("/token", func(c *gin.Context) { c.String(http.StatusOK, web.CSRFToken(c)) })
	f.r.POST("/webhooks/payments", f.m.Webhook)
	f.m.Mount(f.r.Group("/account/claim", f.accounts.RequireSignIn), ui.Default(ui.Design{Name: "Test"}, ui.NewAssets("/assets")))
	f.r.GET("/account", f.accounts.RequireSignIn, func(c *gin.Context) { c.String(http.StatusOK, accounts.Current(c).ID) })
	return f
}

// reply is a decoded answer.
type reply = payments.GrantAnswer

func (f *fixture) ask(t *testing.T, r payments.GrantRequest) reply {
	t.Helper()
	code, a := f.send(t, testBody{Grant: &r})
	if code != http.StatusOK {
		t.Fatalf("%s %s: %d", r.Action, r.OperationID, code)
	}
	return a
}

func (f *fixture) send(t *testing.T, b testBody) (int, reply) {
	t.Helper()
	body, _ := json.Marshal(b)
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/webhooks/payments", strings.NewReader(string(body)))
	req.Header.Set("X-Signature", "valid")
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	var a reply
	if rec.Code == http.StatusOK && b.Grant != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
			t.Fatalf("reply %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, a
}

func provision(grantID, operationID, orderID string) payments.GrantRequest {
	return payments.GrantRequest{
		EventID: "evt_" + operationID, GrantID: grantID, OperationID: operationID, Action: payments.GrantProvision,
		ProductID: product, OrderID: orderID, OrderPaid: true, AmountCents: 1083, Currency: "EUR",
		Deadline: time.Now().Add(24 * time.Hour),
	}
}

func topUp(grantID, operationID, orderID string) payments.GrantRequest {
	r := provision(grantID, operationID, orderID)
	r.Action = payments.GrantTopUp
	return r
}

func (f *fixture) accountOf(t *testing.T, raw string) string {
	t.Helper()
	key, err := f.keys.Authenticate(context.Background(), raw)
	if err != nil {
		t.Fatalf("the key doesn't authenticate: %v", err)
	}
	return key.AccountID
}

func (f *fixture) balance(t *testing.T, accountID string) int64 {
	t.Helper()
	s, err := f.credits.Summary(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	return s.BalanceCents
}

func (f *fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.a.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) reviews(t *testing.T) []string {
	t.Helper()
	rows, err := f.a.DB().Query(`SELECT reason FROM payment_reviews ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestProvision(t *testing.T) {
	f := newFixture(t)
	a := f.ask(t, provision("grt_1", "op_1", "ord_1"))
	raw := a.Env["ACME_API_KEY"]
	link := a.Env["ACME_CLAIM_URL"]
	if a.Status != payments.GrantReady || !strings.HasPrefix(raw, "acme_") || len(a.Env) != 2 ||
		!strings.HasPrefix(link, "http://localhost:8080/account/claim?token=") || !strings.HasSuffix(raw, a.KeyHint) {
		t.Fatalf("answer %+v", a)
	}
	if a.Entitlement["credit"] != "€10.00" || !strings.Contains(a.Entitlement["claim"].(string), "ACME_CLAIM_URL") {
		t.Fatalf("entitlement %+v", a.Entitlement)
	}
	agent := f.accountOf(t, raw)
	if got := f.balance(t, agent); got != 1000 {
		t.Fatalf("balance %d", got)
	}
	if acct, err := f.accounts.Get(context.Background(), agent); err != nil || acct.Email != "" {
		t.Fatalf("account %+v %v", acct, err)
	}
	if n := f.count(t, `SELECT count(*) FROM payment_intents WHERE account_id = $1 AND provider_order_id = 'ord_1' AND provider_status = 'paid'`, agent); n != 1 {
		t.Fatal("the order is not a paid purchase")
	}

	// A repeat gets the same answer, and changes nothing.
	again := f.ask(t, provision("grt_1", "op_1", "ord_1"))
	if again.Env["ACME_API_KEY"] != raw || again.Env["ACME_CLAIM_URL"] != link || again.KeyHint != a.KeyHint {
		t.Fatalf("a repeat answered %+v", again)
	}
	if got := f.balance(t, agent); got != 1000 {
		t.Fatalf("balance after a repeat %d", got)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts`); n != 1 {
		t.Fatalf("%d accounts", n)
	}
	// The credentials are stored sealed, never as they are.
	if n := f.count(t, `SELECT count(*) FROM marketplace_operations o WHERE o::text LIKE '%' || $1 || '%' OR o::text LIKE '%' || $2 || '%'`,
		raw, strings.TrimPrefix(link, "http://localhost:8080/account/claim?token=")); n != 0 {
		t.Fatal("the key or the claim token is stored in the clear")
	}
	if len(f.answered) != 2 || f.answered[0].Repeat || !f.answered[1].Repeat || f.answered[1].Status != payments.GrantReady {
		t.Fatalf("answered %+v", f.answered)
	}
}

func TestConcurrentDeliveriesOpenOneAccount(t *testing.T) {
	f := newFixture(t)
	var wg sync.WaitGroup
	keys := make([]string, 4)
	for i := range keys {
		wg.Go(func() {
			keys[i] = f.ask(t, provision("grt_1", "op_1", "ord_1")).Env["ACME_API_KEY"]
		})
	}
	wg.Wait()
	for _, k := range keys {
		if k == "" || k != keys[0] {
			t.Fatalf("keys %v", keys)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM accounts`); n != 1 {
		t.Fatalf("%d accounts", n)
	}
}

func TestRefusedRequests(t *testing.T) {
	f := newFixture(t)
	unknown := provision("grt_1", "op_1", "ord_1")
	unknown.ProductID = "prod_other"
	cheap := provision("grt_2", "op_2", "ord_2")
	cheap.AmountCents = 1000
	dollars := provision("grt_3", "op_3", "ord_3")
	dollars.Currency = "USD"
	unpaid := provision("grt_4", "op_4", "ord_4")
	unpaid.OrderPaid = false
	for _, r := range []payments.GrantRequest{unknown, cheap, dollars, unpaid} {
		if a := f.ask(t, r); a.Status != payments.GrantFailed || a.FailureReason == "" || len(a.Env) != 0 {
			t.Fatalf("%s answered %+v", r.OperationID, a)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM accounts`); n != 0 {
		t.Fatalf("refused orders opened %d accounts", n)
	}
	// A repeat of a refusal is refused the same way.
	if a := f.ask(t, cheap); a.Status != payments.GrantFailed {
		t.Fatalf("a repeated refusal answered %+v", a)
	}
	// An active buyer's second provision is refused; a top-up is how to add credit.
	f.ask(t, provision("grt_5", "op_5", "ord_5"))
	if a := f.ask(t, provision("grt_5", "op_6", "ord_6")); a.Status != payments.GrantFailed {
		t.Fatalf("a second provision answered %+v", a)
	}
	want := []string{"marketplace_unknown_product", "marketplace_order_mismatch", "marketplace_order_mismatch",
		"marketplace_order_mismatch", "marketplace_repeated_provision"}
	if got := f.reviews(t); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("reviews %v", got)
	}
	if a := f.ask(t, topUp("grt_unknown", "op_7", "ord_7")); a.Status != payments.GrantFailed {
		t.Fatalf("a top-up without an account answered %+v", a)
	}
}

func TestTopUpsAndTheirRefunds(t *testing.T) {
	f := newFixture(t)
	agent := f.accountOf(t, f.ask(t, provision("grt_1", "op_1", "ord_1")).Env["ACME_API_KEY"])
	for range 2 {
		if a := f.ask(t, topUp("grt_1", "op_2", "ord_2")); a.Status != payments.GrantReady || len(a.Env) != 0 {
			t.Fatalf("top-up answered %+v", a)
		}
	}
	if got := f.balance(t, agent); got != 2000 {
		t.Fatalf("balance %d", got)
	}
	revokeCredit := payments.GrantRequest{EventID: "evt_op_3", GrantID: "grt_1", OperationID: "op_3",
		Action: payments.GrantRevokeCredit, OrderID: "ord_2", Reason: "refund"}
	if a := f.ask(t, revokeCredit); a.Status != payments.GrantReady {
		t.Fatalf("revoke_credit answered %+v", a)
	}
	if got := f.balance(t, agent); got != 1000 {
		t.Fatalf("balance after the refund %d", got)
	}
	// The payment webhook's refund of the same order changes nothing more.
	code, _ := f.send(t, testBody{Event: &payments.Event{ID: "evt_refund", Kind: payments.Refunded, OrderID: "ord_2",
		AmountCents: 1083, PaidCents: 1083, Currency: "EUR"}})
	if code != http.StatusOK {
		t.Fatalf("payment webhook %d", code)
	}
	if got := f.balance(t, agent); got != 1000 {
		t.Fatalf("balance after the webhook %d", got)
	}
	// A refund of an order never delivered has nothing to take back.
	unknown := revokeCredit
	unknown.OperationID, unknown.EventID, unknown.OrderID = "op_4", "evt_op_4", "ord_unknown"
	if a := f.ask(t, unknown); a.Status != payments.GrantReady {
		t.Fatalf("revoke_credit of an unknown order answered %+v", a)
	}
}

func TestRevoke(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	raw := f.ask(t, provision("grt_1", "op_1", "ord_1")).Env["ACME_API_KEY"]
	agent := f.accountOf(t, raw)
	if err := f.credits.Debit(ctx, agent, 300, "search-1", "Search"); err != nil {
		t.Fatal(err)
	}
	f.ask(t, topUp("grt_1", "op_2", "ord_2"))
	revoke := payments.GrantRequest{EventID: "evt_op_3", GrantID: "grt_1", OperationID: "op_3",
		Action: payments.GrantRevoke, Reason: "chargeback"}
	if a := f.ask(t, revoke); a.Status != payments.GrantReady {
		t.Fatalf("revoke answered %+v", a)
	}
	if _, err := f.keys.Authenticate(ctx, raw); !errors.Is(err, apikeys.ErrInvalidKey) {
		t.Fatalf("the key still works: %v", err)
	}
	if _, err := f.accounts.Get(ctx, agent); !errors.Is(err, accounts.ErrNotFound) {
		t.Fatalf("the unclaimed account remains: %v", err)
	}
	// The provision was charged back and the rest forfeited, without the account.
	var kinds string
	if err := f.a.DB().QueryRow(`
		SELECT string_agg(kind || ' ' || amount_cents, ', ' ORDER BY created_at, kind)
		FROM credit_ledger`).Scan(&kinds); err != nil {
		t.Fatal(err)
	}
	if kinds != "top_up 1000, usage -300, top_up 1000, chargeback -1000, forfeit -700" {
		t.Fatalf("ledger %s", kinds)
	}
	if n := f.count(t, `SELECT count(*) FROM credit_ledger WHERE account_id IS NOT NULL`); n != 0 {
		t.Fatal("the ledger keeps the deleted account")
	}
	if a := f.ask(t, topUp("grt_1", "op_4", "ord_4")); a.Status != payments.GrantFailed {
		t.Fatalf("a top-up after the revoke answered %+v", a)
	}
	if a := f.ask(t, revoke); a.Status != payments.GrantReady {
		t.Fatalf("a repeated revoke answered %+v", a)
	}
	// A revoke for a grant never provisioned has nothing to end.
	unknown := revoke
	unknown.GrantID, unknown.OperationID, unknown.EventID = "grt_unknown", "op_5", "evt_op_5"
	if a := f.ask(t, unknown); a.Status != payments.GrantReady {
		t.Fatalf("revoking an unknown grant answered %+v", a)
	}
	// A grant whose account ended can be provisioned again.
	if a := f.ask(t, provision("grt_1", "op_6", "ord_6")); a.Status != payments.GrantReady || len(a.Env) != 2 {
		t.Fatalf("provisioning again answered %+v", a)
	}
}

func TestReviewRequests(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	test := payments.GrantRequest{EventID: "evt_t1", GrantID: "grt_test", OperationID: "op_t1",
		Action: payments.GrantProvision, Test: true}
	a := f.ask(t, test)
	if a.Status != payments.GrantReady || len(a.Env) != 2 {
		t.Fatalf("test provision answered %+v", a)
	}
	agent := f.accountOf(t, a.Env["ACME_API_KEY"])
	if got := f.balance(t, agent); got != 0 {
		t.Fatalf("a test opened %d cents", got)
	}
	topUpTest := payments.GrantRequest{EventID: "evt_t2", GrantID: "grt_test", OperationID: "op_t2",
		Action: payments.GrantTopUp, Test: true}
	if a := f.ask(t, topUpTest); a.Status != payments.GrantReady || f.balance(t, agent) != 0 {
		t.Fatalf("test top-up answered %+v", a)
	}
	revokeTest := payments.GrantRequest{EventID: "evt_t3", GrantID: "grt_test", OperationID: "op_t3",
		Action: payments.GrantRevoke, Test: true, Reason: "test"}
	if a := f.ask(t, revokeTest); a.Status != payments.GrantReady {
		t.Fatalf("test revoke answered %+v", a)
	}
	if _, err := f.accounts.Get(ctx, agent); !errors.Is(err, accounts.ErrNotFound) {
		t.Fatalf("the test account remains: %v", err)
	}

	// A test whose revoke never came is removed by the command.
	left := test
	left.GrantID, left.OperationID, left.EventID = "grt_left", "op_t4", "evt_t4"
	f.ask(t, left)
	if err := f.m.removeTestsCommand(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts`); n != 0 {
		t.Fatalf("%d accounts left", n)
	}
}

// browser sends a request as the person the session names; POSTs carry a CSRF token.
func (f *fixture) browser(method, path, session string, form url.Values) *httptest.ResponseRecorder {
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
	if session != "" {
		req.AddCookie(&http.Cookie{Name: "session", Value: session})
	}
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	return rec
}

func TestClaim(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.ask(t, provision("grt_1", "op_1", "ord_1"))
	raw, link := a.Env["ACME_API_KEY"], a.Env["ACME_CLAIM_URL"]
	agent := f.accountOf(t, raw)
	token := strings.TrimPrefix(link, "http://localhost:8080/account/claim?token=")
	if _, err := f.a.DB().Exec(`INSERT INTO work (id, account_id, done) VALUES ('w1', $1, true)`, agent); err != nil {
		t.Fatal(err)
	}

	// Someone not signed in signs in first, and comes back to the link.
	rec := f.browser(http.MethodGet, "/account/claim?token="+token, "", nil)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), url.QueryEscape("token="+token)) {
		t.Fatalf("signed out: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = f.browser(http.MethodGet, "/account/claim?token="+token, "ada", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "€10.00") || !strings.Contains(rec.Body.String(), "1 API key") {
		t.Fatalf("claim page: %d %s", rec.Code, rec.Body.String())
	}
	ada := f.browser(http.MethodGet, "/account", "ada", nil).Body.String()

	// Requests in progress make it wait.
	if _, err := f.a.DB().Exec(`INSERT INTO work (id, account_id, reserved_cents) VALUES ('w2', $1, 7)`, agent); err != nil {
		t.Fatal(err)
	}
	if rec := f.browser(http.MethodPost, "/account/claim", "ada", url.Values{"token": {token}}); rec.Code != http.StatusConflict {
		t.Fatalf("claiming during a request: %d", rec.Code)
	}
	if _, err := f.a.DB().Exec(`UPDATE work SET done = true`); err != nil {
		t.Fatal(err)
	}

	rec = f.browser(http.MethodPost, "/account/claim", "ada", url.Values{"token": {token}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account" {
		t.Fatalf("claim: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if got := f.accountOf(t, raw); got != ada {
		t.Fatalf("the key authenticates for %s, not the owner", got)
	}
	if got := f.balance(t, ada); got != 1000 {
		t.Fatalf("the owner's balance %d", got)
	}
	if _, err := f.accounts.Get(ctx, agent); !errors.Is(err, accounts.ErrNotFound) {
		t.Fatalf("the claimed account remains: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM work WHERE account_id = $1`, ada); n != 2 {
		t.Fatalf("the hook moved %d rows", n)
	}
	if s, err := f.m.Status(ctx, ada); err != nil || !s.Marketplace || !s.Claimed || !s.ExpiresAt.IsZero() {
		t.Fatalf("status %+v %v", s, err)
	}
	// The link works once.
	if rec := f.browser(http.MethodGet, "/account/claim?token="+token, "ada", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("a used link: %d", rec.Code)
	}
	if err := f.m.Claim(ctx, token, ada); !errors.Is(err, ErrClaimInvalid) {
		t.Fatalf("claiming again: %v", err)
	}

	// The agent's later top-ups credit the owner, and a refund takes back from them.
	f.ask(t, topUp("grt_1", "op_2", "ord_2"))
	if got := f.balance(t, ada); got != 2000 {
		t.Fatalf("the owner's balance after a top-up %d", got)
	}
	f.ask(t, payments.GrantRequest{EventID: "evt_op_3", GrantID: "grt_1", OperationID: "op_3", Action: payments.GrantRevoke, Reason: "refund"})
	if got := f.balance(t, ada); got != 1000 {
		t.Fatalf("the owner's balance after the refund %d", got)
	}
	if _, err := f.keys.Authenticate(ctx, raw); !errors.Is(err, apikeys.ErrInvalidKey) {
		t.Fatalf("the revoked grant's key works: %v", err)
	}
	if _, err := f.accounts.Get(ctx, ada); err != nil {
		t.Fatalf("revoking deleted the owner's account: %v", err)
	}
}

func TestNewClaimLink(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	old := strings.TrimPrefix(f.ask(t, provision("grt_1", "op_1", "ord_1")).Env["ACME_CLAIM_URL"], "http://localhost:8080/account/claim?token=")
	link, err := f.m.NewClaimLink(ctx, "grt_1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Claimable(ctx, old); !errors.Is(err, ErrClaimInvalid) {
		t.Fatalf("the old link: %v", err)
	}
	c, err := f.m.Claimable(ctx, strings.TrimPrefix(link, "http://localhost:8080/account/claim?token="))
	if err != nil || c.BalanceCents != 1000 || c.Keys != 1 {
		t.Fatalf("the new link: %+v %v", c, err)
	}
	if _, err := f.m.NewClaimLink(ctx, "grt_unknown"); !errors.Is(err, ErrClaimInvalid) {
		t.Fatalf("an unknown grant: %v", err)
	}
}

func TestRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	idle := f.accountOf(t, f.ask(t, provision("grt_idle", "op_1", "ord_1")).Env["ACME_API_KEY"])
	held := f.accountOf(t, f.ask(t, provision("grt_held", "op_2", "ord_2")).Env["ACME_API_KEY"])
	claimed := f.ask(t, provision("grt_claimed", "op_3", "ord_3")).Env["ACME_CLAIM_URL"]
	ada := f.browser(http.MethodGet, "/account", "ada", nil).Body.String()
	if err := f.m.Claim(ctx, strings.TrimPrefix(claimed, "http://localhost:8080/account/claim?token="), ada); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.DB().Exec(`INSERT INTO work (id, account_id, reserved_cents) VALUES ('w', $1, 5)`, held); err != nil {
		t.Fatal(err)
	}
	s, err := f.m.Status(ctx, idle)
	if err != nil || !s.Marketplace || s.Claimed || s.ExpiresAt.Before(time.Now().AddDate(1, 0, -1)) || s.ExpiresAt.After(time.Now().AddDate(1, 0, 1)) {
		t.Fatalf("status %+v %v", s, err)
	}

	// Before the year is up, nothing expires; the sealed credentials stay until the deadline.
	if r, err := f.m.Run(ctx); err != nil || r != (Result{}) {
		t.Fatalf("an early run: %+v %v", r, err)
	}
	f.m.now = func() time.Time { return time.Now().AddDate(1, 0, 1) }
	r, err := f.m.Run(ctx)
	if err != nil || r.SecretsWiped != 3 || r.Expired != 1 || r.ExpiredCents != 1000 || r.Failed != 0 {
		t.Fatalf("run: %+v %v", r, err)
	}
	if _, err := f.accounts.Get(ctx, idle); !errors.Is(err, accounts.ErrNotFound) {
		t.Fatalf("the idle account remains: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM credit_ledger WHERE kind = 'expiry' AND amount_cents = -1000 AND account_id IS NULL`); n != 1 {
		t.Fatal("the idle account's credit didn't expire")
	}
	if f.balance(t, held) != 1000 || f.balance(t, ada) != 1000 {
		t.Fatal("held or claimed credit expired")
	}
	// Once the credentials are wiped, a repeat can't be answered the same way.
	if a := f.ask(t, provision("grt_held", "op_2", "ord_2")); a.Status != payments.GrantPending {
		t.Fatalf("a repeat after the deadline answered %+v", a)
	}
	// The hold settles; the next run expires it.
	if _, err := f.a.DB().Exec(`UPDATE work SET done = true`); err != nil {
		t.Fatal(err)
	}
	if r, err := f.m.Run(ctx); err != nil || r.Expired != 1 {
		t.Fatalf("the next run: %+v %v", r, err)
	}
	if s, err := f.m.Status(ctx, ada); err != nil || !s.Claimed {
		t.Fatalf("the owner's status: %+v %v", s, err)
	}
}

func TestOtherEventsGoToCredits(t *testing.T) {
	f := newFixture(t)
	code, _ := f.send(t, testBody{Event: &payments.Event{ID: "evt_1", Kind: payments.Paid, Reference: "unknown", AmountCents: 500, Currency: "EUR"}})
	if code != http.StatusOK {
		t.Fatalf("a payment event: %d", code)
	}
	if got := f.reviews(t); len(got) != 1 || got[0] != "unknown_payment" {
		t.Fatalf("reviews %v", got)
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/webhooks/payments", strings.NewReader(`{}`))
	req.Header.Set("X-Signature", "forged")
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a forged request: %d", rec.Code)
	}
}
