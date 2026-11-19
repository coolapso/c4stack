//go:build integration

package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/coolapso/c4stack/payments"
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
	return identity.Identity{Subject: c.Value, Email: c.Value + "@example.com", EmailVerified: true, SignedInAt: time.Now()}, nil
}
func (testIdentity) Logout(http.ResponseWriter, *http.Request) error { return nil }
func (testIdentity) Delete(context.Context, *http.Request) error     { return nil }

// fakeProvider is a payment provider for subscriptions. Webhook bodies are JSON
// payments.SubscriptionEvents, accepted when X-Signature says "valid".
type fakeProvider struct {
	mu        sync.Mutex
	checkouts []payments.SubscriptionCheckout
	states    map[string]payments.SubscriptionState
	cancels   []string
	immediate []string
	changes   []payments.SubscriptionChange
	fail      error
	// failUnits fails a change's units step, after its product step went through.
	failUnits bool
}

func (p *fakeProvider) Name() string           { return "test" }
func (p *fakeProvider) CheckoutOrigin() string { return "https://pay.example" }
func (p *fakeProvider) CreateCheckout(context.Context, payments.Checkout) (payments.Session, error) {
	return payments.Session{}, errors.New("no credits here")
}
func (p *fakeProvider) ParseWebhook(http.Header, []byte) (payments.Event, error) {
	return payments.Event{}, payments.ErrIgnored
}
func (p *fakeProvider) CreateSubscriptionCheckout(_ context.Context, c payments.SubscriptionCheckout) (payments.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return payments.Session{}, p.fail
	}
	p.checkouts = append(p.checkouts, c)
	return payments.Session{ID: "co_" + c.Reference, URL: "https://pay.example/co_" + c.Reference}, nil
}
func (p *fakeProvider) ParseSubscriptionWebhook(h http.Header, body []byte) (payments.SubscriptionEvent, error) {
	if h.Get("X-Signature") != "valid" {
		return payments.SubscriptionEvent{}, payments.ErrInvalid
	}
	var e payments.SubscriptionEvent
	if err := json.Unmarshal(body, &e); err != nil || e.ID == "" {
		return payments.SubscriptionEvent{}, payments.ErrInvalid
	}
	if e.Kind == 0 {
		return e, payments.ErrIgnored
	}
	return e, nil
}
func (p *fakeProvider) Subscription(_ context.Context, id string) (payments.SubscriptionState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return payments.SubscriptionState{}, p.fail
	}
	return p.states[id], nil
}
func (p *fakeProvider) CancelSubscription(_ context.Context, id string, atPeriodEnd bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[id]
	if atPeriodEnd {
		p.cancels = append(p.cancels, id)
		st.Status = payments.SubscriptionCanceling
	} else {
		p.immediate = append(p.immediate, id)
		st.Status = payments.SubscriptionCanceled
	}
	p.states[id] = st
	return nil
}
func (p *fakeProvider) ChangeSubscription(_ context.Context, id string, c payments.SubscriptionChange) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.changes = append(p.changes, c)
	st := p.states[id]
	if c.ProductID != "" {
		st.ProductID = c.ProductID
	}
	p.states[id] = st
	if c.Units != 0 && p.failUnits {
		return errors.New("the units step failed")
	}
	if c.Units != 0 {
		st.Units = c.Units
	}
	p.states[id] = st
	return nil
}
func (p *fakeProvider) CustomerPortal(_ context.Context, customer string) (string, error) {
	return "https://pay.example/portal/" + customer, nil
}

var products = []Product{
	{ID: "plus-monthly", ProviderID: "prov_plus_m", Name: "Plus, monthly", Group: "plan"},
	{ID: "plus-yearly", ProviderID: "prov_plus_y", Name: "Plus, yearly", Group: "plan"},
	{ID: "family-monthly", ProviderID: "prov_family_m", Name: "Family, monthly", Group: "plan"},
	{ID: "vaults", ProviderID: "prov_vaults", Name: "More vaults", MaxUnits: 5},
	{ID: "vault-room", ProviderID: "prov_room", Name: "More room", MaxUnits: 10, Group: "vault-room"},
	{ID: "team", ProviderID: "prov_team", Name: "Team", MaxUnits: 10, Group: "plan"},
}

type fixture struct {
	a        *app.App
	accounts *accounts.Accounts
	subs     *Subscriptions
	pay      *fakeProvider
	r        *gin.Engine
	clock    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		a:     apptest.New(t, app.Config{Name: "test", Migrations: []app.Migrations{accounts.Migrations, Migrations}}),
		pay:   &fakeProvider{states: map[string]payments.SubscriptionState{}},
		clock: time.Now().Add(-time.Hour),
	}
	var err error
	if f.accounts, err = accounts.New(f.a, testIdentity{}, accounts.Config{}); err != nil {
		t.Fatal(err)
	}
	if f.subs, err = New(f.a, f.accounts, Config{Payments: f.pay, Products: products}); err != nil {
		t.Fatal(err)
	}
	if f.r, err = web.New(web.Config{PublicURL: "http://localhost:8080", SecretKey: strings.Repeat("s", 32)}); err != nil {
		t.Fatal(err)
	}
	layout := ui.Default(ui.Design{Name: "Test"}, ui.NewAssets("/assets"))
	f.r.GET("/token", func(c *gin.Context) { c.String(http.StatusOK, web.CSRFToken(c)) })
	f.accounts.Mount(f.r, layout)
	f.subs.Mount(f.r.Group("/account/subscriptions", f.accounts.RequireSignIn), layout)
	f.r.POST("/webhooks/subscriptions", f.subs.Webhook)
	return f
}

// do sends a request as the browser holding session; POSTs carry a valid CSRF token.
func (f *fixture) do(method, path, session string, form url.Values) *httptest.ResponseRecorder {
	tok := httptest.NewRecorder()
	f.r.ServeHTTP(tok, httptest.NewRequest(http.MethodGet, "http://localhost:8080/token", nil))
	if form == nil {
		form = url.Values{}
	}
	if method == http.MethodPost {
		form.Set(web.CSRFField, tok.Body.String())
	}
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

// account signs the subject in once, which creates its account, and returns its ID.
func (f *fixture) account(t *testing.T, subject string) string {
	t.Helper()
	f.do(http.MethodGet, "/account/settings", subject, nil)
	var id string
	if err := f.a.DB().QueryRow(`SELECT account_id FROM account_identities WHERE subject = $1`, subject).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// checkout starts a subscription through the page and returns its reference.
func (f *fixture) checkout(t *testing.T, subject, product string, units int, target string) string {
	t.Helper()
	form := url.Values{"product": {product}, "target": {target}, "next": {"/account/settings"}}
	if units != 0 {
		form.Set("units", strconv.Itoa(units))
	}
	rec := f.do(http.MethodPost, "/account/subscriptions", subject, form)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "https://pay.example/co_") {
		t.Fatalf("checkout %s: %d %q\n%s", product, rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	return strings.TrimPrefix(rec.Header().Get("Location"), "https://pay.example/co_")
}

// send delivers an event, a tick of the provider's clock after the last one unless it
// says when.
func (f *fixture) send(t *testing.T, e payments.SubscriptionEvent) *httptest.ResponseRecorder {
	t.Helper()
	if e.CreatedAt.IsZero() {
		f.clock = f.clock.Add(time.Second)
		e.CreatedAt = f.clock
	}
	body, _ := json.Marshal(e)
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/webhooks/subscriptions", strings.NewReader(string(body)))
	req.Header.Set("X-Signature", "valid")
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook %s: %d", e.ID, rec.Code)
	}
	return rec
}

// start sends the checkout's completion: the provider's subscription, active.
func (f *fixture) start(t *testing.T, ref, providerID, product string, units int) {
	t.Helper()
	st := payments.SubscriptionState{ID: providerID, CustomerID: "cust_" + providerID, ProductID: product, Units: units, Status: payments.SubscriptionActive}
	f.pay.mu.Lock()
	f.pay.states[providerID] = st
	f.pay.mu.Unlock()
	f.send(t, payments.SubscriptionEvent{ID: "evt_start_" + ref, Kind: payments.SubscriptionStarted, Reference: ref, State: st})
}

func (f *fixture) row(t *testing.T, id string) (status string, units int, product string) {
	t.Helper()
	if err := f.a.DB().QueryRow(`SELECT status, units, product FROM subscriptions WHERE id = $1`, id).Scan(&status, &units, &product); err != nil {
		t.Fatal(err)
	}
	return status, units, product
}

func (f *fixture) reviews(t *testing.T) []string {
	t.Helper()
	rows, err := f.a.DB().Query(`SELECT reason FROM subscription_reviews ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSubscribe(t *testing.T) {
	f := newFixture(t)
	account := f.account(t, "ann")
	ref := f.checkout(t, "ann", "plus-monthly", 0, "")
	c := f.pay.checkouts[0]
	if c.Reference != ref || c.ProductID != "prov_plus_m" || c.Units != 1 || c.Email != "ann@example.com" || c.SuccessURL != "http://localhost:8080/account/settings" {
		t.Fatalf("checkout %+v", c)
	}
	// Pending until the provider confirms it; the settings page shows nothing yet.
	if subs, _ := f.subs.List(context.Background(), account); len(subs) != 0 {
		t.Fatalf("listed a pending subscription: %+v", subs)
	}
	if page := f.do(http.MethodGet, "/account/settings", "ann", nil).Body.String(); strings.Contains(page, "subscriptions-title") {
		t.Fatal("a panel before any subscription")
	}

	f.start(t, ref, "sub_1", "prov_plus_m", 1)
	end := time.Date(2027, 1, 2, 10, 0, 0, 0, time.UTC)
	f.send(t, payments.SubscriptionEvent{ID: "evt_paid", Kind: payments.SubscriptionChanged, Reference: ref,
		State: payments.SubscriptionState{ID: "sub_1", ProductID: "prov_plus_m", Status: payments.SubscriptionActive, PeriodEnd: end}})
	subs, err := f.subs.List(context.Background(), account)
	if err != nil || len(subs) != 1 || subs[0].Product.ID != "plus-monthly" || subs[0].Status != payments.SubscriptionActive || !subs[0].PeriodEnd.Equal(end) || subs[0].Units != 1 {
		t.Fatalf("List = %+v, %v", subs, err)
	}
	page := f.do(http.MethodGet, "/account/settings", "ann", nil).Body.String()
	if !strings.Contains(page, "Plus, monthly") || !strings.Contains(page, "Renews on 2 January 2027") || !strings.Contains(page, `action="/account/subscriptions/`+ref+`/cancel"`) {
		t.Fatalf("settings panel:\n%s", page)
	}
	// An event is taken once.
	f.send(t, payments.SubscriptionEvent{ID: "evt_paid", Kind: payments.SubscriptionChanged, Reference: ref,
		State: payments.SubscriptionState{ID: "sub_1", Status: payments.SubscriptionPastDue}})
	if status, _, _ := f.row(t, ref); status != "active" {
		t.Fatalf("a repeated event changed the status to %s", status)
	}
	// Another account can't see or touch it.
	other := f.account(t, "bob")
	if _, err := f.subs.Get(context.Background(), other, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account's Get: %v", err)
	}
	if rec := f.do(http.MethodPost, "/account/subscriptions/"+ref+"/cancel", "bob", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("another account's cancel: %d", rec.Code)
	}
}

func TestOutOfOrder(t *testing.T) {
	f := newFixture(t)
	f.account(t, "ann")
	ref := f.checkout(t, "ann", "plus-monthly", 0, "")
	base := time.Now().Add(-time.Minute)
	// The cancellation arrives before the checkout's completion, which carries an older
	// state, and before an older renewal.
	f.send(t, payments.SubscriptionEvent{ID: "evt_cancel", Kind: payments.SubscriptionChanged, Reference: ref, CreatedAt: base.Add(3 * time.Second),
		State: payments.SubscriptionState{ID: "sub_1", ProductID: "prov_plus_m", Status: payments.SubscriptionCanceling}})
	f.send(t, payments.SubscriptionEvent{ID: "evt_start", Kind: payments.SubscriptionStarted, Reference: ref, CreatedAt: base,
		State: payments.SubscriptionState{ID: "sub_1", CustomerID: "cust_1", ProductID: "prov_plus_m", Units: 1, Status: payments.SubscriptionActive}})
	f.send(t, payments.SubscriptionEvent{ID: "evt_renew", Kind: payments.SubscriptionChanged, CreatedAt: base.Add(time.Second),
		State: payments.SubscriptionState{ID: "sub_1", Status: payments.SubscriptionActive}})
	if status, _, _ := f.row(t, ref); status != "canceling" {
		t.Fatalf("an older event overwrote the status: %s", status)
	}
	var customer string
	if err := f.a.DB().QueryRow(`SELECT provider_customer_id FROM subscriptions WHERE id = $1`, ref).Scan(&customer); err != nil || customer != "cust_1" {
		t.Fatalf("the late event's customer wasn't recorded: %q, %v", customer, err)
	}
	if r := f.reviews(t); len(r) != 0 {
		t.Fatalf("reviews %v", r)
	}
}

func TestReviews(t *testing.T) {
	f := newFixture(t)
	f.account(t, "ann")
	ref := f.checkout(t, "ann", "plus-monthly", 0, "")
	f.start(t, ref, "sub_1", "prov_plus_m", 1)
	want := []string{}
	check := func(name string, e payments.SubscriptionEvent, reason string) {
		t.Helper()
		before := len(f.reviews(t))
		f.send(t, e)
		got := f.reviews(t)
		if reason == "" {
			if len(got) != before {
				t.Errorf("%s: reviewed %v", name, got[before:])
			}
			return
		}
		want = append(want, reason)
		if len(got) != before+1 || got[len(got)-1] != reason {
			t.Errorf("%s: reviews %v, want %s", name, got, reason)
		}
	}
	check("unknown subscription of ours", payments.SubscriptionEvent{ID: "e1", Kind: payments.SubscriptionChanged,
		State: payments.SubscriptionState{ID: "sub_unknown", ProductID: "prov_plus_m", Status: payments.SubscriptionActive}}, "unknown_subscription")
	check("not ours", payments.SubscriptionEvent{ID: "e2", Kind: payments.SubscriptionChanged,
		State: payments.SubscriptionState{ID: "sub_other_app", ProductID: "prov_elsewhere", Status: payments.SubscriptionActive}}, "")
	check("provider's own reason", payments.SubscriptionEvent{ID: "e3", Kind: payments.SubscriptionChanged, Reference: ref, Review: "several_items",
		State: payments.SubscriptionState{ID: "sub_1"}}, "several_items")
	check("refund", payments.SubscriptionEvent{ID: "e4", Kind: payments.SubscriptionRefunded, State: payments.SubscriptionState{ID: "sub_1"}}, "refunded")
	check("dispute", payments.SubscriptionEvent{ID: "e5", Kind: payments.SubscriptionDisputed, Reference: ref, State: payments.SubscriptionState{ID: "sub_1"}}, "disputed")
	check("reference for another subscription", payments.SubscriptionEvent{ID: "e6", Kind: payments.SubscriptionChanged, Reference: ref,
		State: payments.SubscriptionState{ID: "sub_2", Status: payments.SubscriptionActive}}, "reference_mismatch")
	check("unknown product", payments.SubscriptionEvent{ID: "e7", Kind: payments.SubscriptionChanged, Reference: ref,
		State: payments.SubscriptionState{ID: "sub_1", ProductID: "prov_gold", Status: payments.SubscriptionActive}}, "unknown_product")
	if _, _, product := f.row(t, ref); product != "plus-monthly" {
		t.Fatalf("an unknown product was applied: %s", product)
	}
	_ = want
}

func TestGroups(t *testing.T) {
	f := newFixture(t)
	account := f.account(t, "ann")
	// Two checkouts for one plan, both started before either completed: the second to
	// complete needs a look, since the account pays twice.
	plus := f.checkout(t, "ann", "plus-monthly", 0, "")
	family := f.checkout(t, "ann", "family-monthly", 0, "")
	f.start(t, plus, "sub_plus", "prov_plus_m", 1)
	f.start(t, family, "sub_family", "prov_family_m", 1)
	if r := f.reviews(t); len(r) != 1 || r[0] != "second_in_group" {
		t.Fatalf("reviews %v", r)
	}
	// With a live plan, another plan can't start: change it instead.
	if _, err := f.subs.Checkout(context.Background(), account, CheckoutRequest{Product: "plus-yearly"}); !errors.Is(err, ErrAlreadySubscribed) {
		t.Fatalf("a second plan: %v", err)
	}
	if rec := f.do(http.MethodPost, "/account/subscriptions", "ann", url.Values{"product": {"plus-yearly"}}); rec.Code != http.StatusConflict {
		t.Fatalf("a second plan through the page: %d", rec.Code)
	}
	// Products without a group, and groups per target, are fine.
	f.checkout(t, "ann", "vaults", 3, "")
	room1 := f.checkout(t, "ann", "vault-room", 2, "vault-1")
	f.checkout(t, "ann", "vault-room", 1, "vault-2")
	f.start(t, room1, "sub_room1", "prov_room", 2)
	if _, err := f.subs.Checkout(context.Background(), account, CheckoutRequest{Product: "vault-room", Target: "vault-1"}); !errors.Is(err, ErrAlreadySubscribed) {
		t.Fatalf("a second room pack for one vault: %v", err)
	}
	for name, r := range map[string]CheckoutRequest{
		"unknown product":   {Product: "gold"},
		"too many units":    {Product: "vaults", Units: 6},
		"no units":          {Product: "vaults", Units: -1},
		"control in target": {Product: "vault-room", Target: "a\x00b"},
		"long target":       {Product: "vault-room", Target: strings.Repeat("v", 129)},
	} {
		if _, err := f.subs.Checkout(context.Background(), account, r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCancelAndChange(t *testing.T) {
	f := newFixture(t)
	f.account(t, "ann")
	plan := f.checkout(t, "ann", "plus-monthly", 0, "")
	f.start(t, plan, "sub_plan", "prov_plus_m", 1)
	packs := f.checkout(t, "ann", "vaults", 2, "")
	f.start(t, packs, "sub_packs", "prov_vaults", 2)

	// More packs, and a bigger plan; the provider's state is read back at once.
	if rec := f.do(http.MethodPost, "/account/subscriptions/"+packs+"/change", "ann", url.Values{"units": {"4"}, "next": {"/account/settings"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("units: %d", rec.Code)
	}
	if _, units, _ := f.row(t, packs); units != 4 {
		t.Fatalf("units after the change: %d", units)
	}
	if rec := f.do(http.MethodPost, "/account/subscriptions/"+plan+"/change", "ann", url.Values{"product": {"family-monthly"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("plan: %d", rec.Code)
	}
	if _, _, product := f.row(t, plan); product != "family-monthly" {
		t.Fatalf("product after the change: %s", product)
	}
	if c := f.pay.changes; len(c) != 2 || c[0].Units != 4 || c[1].ProductID != "prov_family_m" || c[1].Proration != payments.ProrateNow {
		t.Fatalf("changes %+v", c)
	}
	// Out of the group, or past the units, is refused.
	for name, form := range map[string]url.Values{
		"another group": {"product": {"vaults"}},
		"too many":      {"units": {"9"}},
	} {
		if rec := f.do(http.MethodPost, "/account/subscriptions/"+plan+"/change", "ann", form); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}

	// Canceling: at the end of the period; the panel says when it ends.
	end := time.Date(2027, 3, 4, 0, 0, 0, 0, time.UTC)
	f.pay.mu.Lock()
	st := f.pay.states["sub_plan"]
	st.PeriodEnd = end
	f.pay.states["sub_plan"] = st
	f.pay.mu.Unlock()
	if rec := f.do(http.MethodPost, "/account/subscriptions/"+plan+"/cancel", "ann", url.Values{"next": {"/account/settings"}}); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account/settings" {
		t.Fatalf("cancel: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if status, _, _ := f.row(t, plan); status != "canceling" || len(f.pay.cancels) != 1 {
		t.Fatalf("after canceling: %s, cancels %v", status, f.pay.cancels)
	}
	if page := f.do(http.MethodGet, "/account/settings", "ann", nil).Body.String(); !strings.Contains(page, "Ends on 4 March 2027") {
		t.Fatalf("settings panel:\n%s", page)
	}
	// Canceling again, or changing a canceling one, does nothing.
	f.do(http.MethodPost, "/account/subscriptions/"+plan+"/cancel", "ann", nil)
	if len(f.pay.cancels) != 1 {
		t.Fatalf("canceled twice: %v", f.pay.cancels)
	}
	// When the provider can't be read back, the cancellation still shows.
	f.pay.mu.Lock()
	f.pay.fail = errors.New("down")
	f.pay.mu.Unlock()
	if err := f.subs.Cancel(context.Background(), f.account(t, "ann"), packs); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := f.row(t, packs); status != "canceling" {
		t.Fatalf("fallback status: %s", status)
	}
}

func TestDeletionWaitsForCancellation(t *testing.T) {
	f := newFixture(t)
	account := f.account(t, "ann")
	ref := f.checkout(t, "ann", "plus-monthly", 0, "")
	f.start(t, ref, "sub_1", "prov_plus_m", 1)
	rec := f.do(http.MethodPost, "/account/delete", "ann", url.Values{"confirmation": {"DELETE"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Cancel it on your account settings page first") {
		t.Fatalf("deletion with a live subscription: %d", rec.Code)
	}
	if err := f.subs.Cancel(context.Background(), account, ref); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(http.MethodPost, "/account/delete", "ann", url.Values{"confirmation": {"DELETE"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("deletion after canceling: %d", rec.Code)
	}
	// The record stays, without its account, target or customer; if it comes back to
	// life, it is canceled and needs a look.
	var linked, customer bool
	var providerID string
	if err := f.a.DB().QueryRow(`SELECT account_id IS NOT NULL, provider_customer_id IS NOT NULL, provider_subscription_id FROM subscriptions WHERE id = $1`, ref).Scan(&linked, &customer, &providerID); err != nil || linked || customer || providerID != "sub_1" {
		t.Fatalf("the record after deletion: linked %v, customer %v, provider %q, %v", linked, customer, providerID, err)
	}
	// Reading the subscription back after canceling isn't ordered against events, so an
	// event of the provider's, a moment after the last, still applies; it is canceled
	// at once.
	f.send(t, payments.SubscriptionEvent{ID: "evt_resumed", Kind: payments.SubscriptionChanged, Reference: ref,
		State: payments.SubscriptionState{ID: "sub_1", Status: payments.SubscriptionActive}})
	if r := f.reviews(t); len(r) != 1 || r[0] != "deleted_account" {
		t.Fatalf("reviews %v", r)
	}
	if len(f.pay.immediate) != 1 || f.pay.immediate[0] != "sub_1" {
		t.Fatalf("immediate cancels %v", f.pay.immediate)
	}
}

func TestBilling(t *testing.T) {
	f := newFixture(t)
	f.account(t, "ann")
	if rec := f.do(http.MethodGet, "/account/subscriptions/billing", "ann", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("billing before paying: %d", rec.Code)
	}
	ref := f.checkout(t, "ann", "plus-monthly", 0, "")
	f.start(t, ref, "sub_1", "prov_plus_m", 1)
	if rec := f.do(http.MethodGet, "/account/subscriptions/billing", "ann", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "https://pay.example/portal/cust_sub_1" {
		t.Fatalf("billing: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestWebhookChecks(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/webhooks/subscriptions", strings.NewReader(`{"ID":"evt_1","Kind":2}`))
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsigned: %d", rec.Code)
	}
	f.send(t, payments.SubscriptionEvent{ID: "evt_other"}) // ignored: no kind
	if n := f.count(t, `SELECT count(*) FROM subscription_events`); n != 0 {
		t.Fatalf("an ignored event was recorded: %d", n)
	}
}

func (f *fixture) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := f.a.DB().QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCleanupAndCommands(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.account(t, "ann")
	old := f.checkout(t, "ann", "plus-monthly", 0, "")
	fresh := f.checkout(t, "ann", "vaults", 1, "")
	recent := f.checkout(t, "ann", "vault-room", 1, "vault-9")
	if _, err := f.a.DB().Exec(`UPDATE subscriptions SET created_at = now() - interval '31 days' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.DB().Exec(`UPDATE subscriptions SET created_at = now() - interval '3 days' WHERE id = $1`, recent); err != nil {
		t.Fatal(err)
	}
	f.start(t, fresh, "sub_1", "prov_vaults", 1)
	if _, err := f.a.DB().Exec(`UPDATE subscription_events SET received_at = now() - interval '31 days'`); err != nil {
		t.Fatal(err)
	}
	if err := f.subs.cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM subscriptions`); n != 2 {
		t.Fatalf("%d subscriptions after cleanup, want the started one and the 3-day-old checkout", n)
	}
	if n := f.count(t, `SELECT count(*) FROM subscription_events`); n != 0 {
		t.Fatalf("%d old events after cleanup", n)
	}
	// sync reads the provider's state, such as after missed webhooks.
	f.pay.mu.Lock()
	f.pay.states["sub_1"] = payments.SubscriptionState{ID: "sub_1", ProductID: "prov_vaults", Units: 3, Status: payments.SubscriptionPastDue}
	f.pay.mu.Unlock()
	if err := f.subs.syncCommand(ctx, []string{fresh}); err != nil {
		t.Fatal(err)
	}
	if status, units, _ := f.row(t, fresh); status != "past_due" || units != 3 {
		t.Fatalf("after sync: %s, %d", status, units)
	}
	f.send(t, payments.SubscriptionEvent{ID: "evt_refund", Kind: payments.SubscriptionRefunded, State: payments.SubscriptionState{ID: "sub_1"}})
	var id string
	if err := f.a.DB().QueryRow(`SELECT id FROM subscription_reviews`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := f.subs.resolveCommand(ctx, []string{id, "refunded", "by", "hand"}); err != nil {
		t.Fatal(err)
	}
	if err := f.subs.resolveCommand(ctx, []string{id, "again"}); err == nil {
		t.Fatal("resolved a review twice")
	}
	if n := f.count(t, `SELECT count(*) FROM subscription_reviews WHERE resolution = 'refunded by hand'`); n != 1 {
		t.Fatal("the resolution wasn't recorded")
	}
}

// TestDeletedAccountCheckout: a checkout completed after its account was deleted is
// canceled at once, and goes to review for its payment.
func TestDeletedAccountCheckout(t *testing.T) {
	f := newFixture(t)
	f.account(t, "ann")
	ref := f.checkout(t, "ann", "plus-monthly", 0, "")
	if rec := f.do(http.MethodPost, "/account/delete", "ann", url.Values{"confirmation": {"DELETE"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("deletion with a pending checkout: %d", rec.Code)
	}
	f.start(t, ref, "sub_late", "prov_plus_m", 1)
	if r := f.reviews(t); len(r) != 1 || r[0] != "deleted_account" {
		t.Fatalf("reviews %v", r)
	}
	if len(f.pay.immediate) != 1 || f.pay.immediate[0] != "sub_late" {
		t.Fatalf("immediate cancels %v", f.pay.immediate)
	}
}

// TestConcurrentActivations: two checkouts for one plan completing at the same moment
// still see each other. The first activation holds its transaction open; the second
// must wait for it, then find it.
func TestConcurrentActivations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.account(t, "ann")
	a := f.checkout(t, "ann", "plus-monthly", 0, "")
	b := f.checkout(t, "ann", "family-monthly", 0, "")
	active := func(id, product string) payments.SubscriptionState {
		return payments.SubscriptionState{ID: "sub_" + id, ProductID: product, Units: 1, Status: payments.SubscriptionActive}
	}
	first, err := f.a.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback()
	if out, err := f.subs.applyState(ctx, first, a, active(a, "prov_plus_m"), time.Now()); err != nil || out.review != "" {
		t.Fatalf("first activation: %+v, %v", out, err)
	}
	second := make(chan applied, 1)
	go func() {
		tx, err := f.a.DB().BeginTx(ctx, nil)
		if err != nil {
			second <- applied{review: err.Error()}
			return
		}
		defer tx.Rollback()
		out, err := f.subs.applyState(ctx, tx, b, active(b, "prov_family_m"), time.Now())
		if err != nil {
			out.review = err.Error()
		}
		_ = tx.Commit()
		second <- out
	}()
	select {
	case out := <-second:
		t.Fatalf("the second activation didn't wait for the first: %+v", out)
	case <-time.After(300 * time.Millisecond):
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}
	if out := <-second; out.review != "second_in_group" {
		t.Fatalf("the second activation: %+v", out)
	}
}

func TestChangeLimitsAndRetries(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	account := f.account(t, "ann")
	team := f.checkout(t, "ann", "team", 5, "")
	f.start(t, team, "sub_team", "prov_team", 5)
	// A product's limit holds for the units the subscription keeps.
	if err := f.subs.Change(ctx, account, team, ChangeRequest{Product: "plus-monthly"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("five units onto a one-unit product: %v", err)
	}
	// A change that half happened: the product went through, the units didn't.
	f.pay.mu.Lock()
	f.pay.failUnits = true
	f.pay.mu.Unlock()
	if err := f.subs.Change(ctx, account, team, ChangeRequest{Product: "plus-monthly", Units: 1}); err == nil {
		t.Fatal("the failed units step wasn't reported")
	}
	f.pay.mu.Lock()
	f.pay.failUnits = false
	f.pay.mu.Unlock()
	// The retry sends only what isn't so already: no second upgrade, no second proration.
	if err := f.subs.Change(ctx, account, team, ChangeRequest{Product: "plus-monthly", Units: 1}); err != nil {
		t.Fatal(err)
	}
	c := f.pay.changes
	if len(c) != 2 || c[0].ProductID != "prov_plus_m" || c[1].ProductID != "" || c[1].Units != 1 {
		t.Fatalf("changes %+v", c)
	}
	if _, units, product := f.row(t, team); product != "plus-monthly" || units != 1 {
		t.Fatalf("after the retry: %s, %d", product, units)
	}
	// While another change holds the subscription, a second one waits its turn.
	tx, err := f.a.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT 1 FROM subscriptions WHERE id = $1 FOR UPDATE`, team); err != nil {
		t.Fatal(err)
	}
	if err := f.subs.Change(ctx, account, team, ChangeRequest{Product: "family-monthly"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("a change during another: %v", err)
	}
	if err := f.subs.Cancel(ctx, account, team); !errors.Is(err, ErrBusy) {
		t.Fatalf("a cancel during a change: %v", err)
	}
}

// TestProviderChangesWithinRules: a product or units the app doesn't allow, from the
// provider's side, aren't applied, and go to review; the rest of the state applies.
func TestProviderChangesWithinRules(t *testing.T) {
	f := newFixture(t)
	f.account(t, "ann")
	// A checkout completed with another product than it asked for.
	pending := f.checkout(t, "ann", "plus-monthly", 0, "")
	f.send(t, payments.SubscriptionEvent{ID: "evt_mismatch", Kind: payments.SubscriptionStarted, Reference: pending,
		State: payments.SubscriptionState{ID: "sub_1", ProductID: "prov_family_m", Units: 1, Status: payments.SubscriptionActive}})
	if status, _, product := f.row(t, pending); product != "plus-monthly" || status != "active" {
		t.Fatalf("mismatched checkout: %s, %s", status, product)
	}
	// A move within the group applies; out of it doesn't.
	f.send(t, payments.SubscriptionEvent{ID: "evt_upgrade", Kind: payments.SubscriptionChanged, Reference: pending,
		State: payments.SubscriptionState{ID: "sub_1", ProductID: "prov_plus_y", Status: payments.SubscriptionActive}})
	if _, _, product := f.row(t, pending); product != "plus-yearly" {
		t.Fatalf("a move within the group: %s", product)
	}
	f.send(t, payments.SubscriptionEvent{ID: "evt_cross", Kind: payments.SubscriptionChanged, Reference: pending,
		State: payments.SubscriptionState{ID: "sub_1", ProductID: "prov_vaults", Units: 2, Status: payments.SubscriptionPastDue}})
	if status, units, product := f.row(t, pending); product != "plus-yearly" || units != 1 || status != "past_due" {
		t.Fatalf("a move out of the group: %s, %d, %s", product, units, status)
	}
	f.send(t, payments.SubscriptionEvent{ID: "evt_units", Kind: payments.SubscriptionChanged, Reference: pending,
		State: payments.SubscriptionState{ID: "sub_1", Units: 7, Status: payments.SubscriptionActive}})
	if status, units, _ := f.row(t, pending); units != 1 || status != "active" {
		t.Fatalf("too many units: %d, %s", units, status)
	}
	if r := f.reviews(t); strings.Join(r, ",") != "product_mismatch,product_change,too_many_units" {
		t.Fatalf("reviews %v", r)
	}
}
