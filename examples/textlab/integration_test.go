//go:build integration

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coolapso/c4stack/payments"
	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

const origin = "http://localhost:8080"

// browser sends requests as the signed-in user ada, with a valid CSRF token.
type browser struct {
	t       *testing.T
	handler http.Handler
	cookies []*http.Cookie
	token   string
}

func newBrowser(t *testing.T, h http.Handler) *browser {
	b := &browser{t: t, handler: h}
	// Textlab asks a new account for its name first.
	rec := b.do(http.MethodGet, "/account/api-keys", nil, nil)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/account/name?") {
		t.Fatalf("console without a name: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = b.do(http.MethodGet, rec.Header().Get("Location"), nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("name page: %d", rec.Code)
	}
	b.token = regexp.MustCompile(`<meta name="csrf-token" content="([^"]+)"`).FindStringSubmatch(rec.Body.String())[1]
	b.cookies = rec.Result().Cookies()
	if rec := b.do(http.MethodPost, "/account/name", url.Values{"name": {"Ada Lovelace"}, "next": {"/account/api-keys"}}, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving the name: %d", rec.Code)
	}
	if rec := b.do(http.MethodGet, "/account/api-keys", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("console: %d", rec.Code)
	}
	return b
}

func (b *browser) do(method, path string, form url.Values, header http.Header) *httptest.ResponseRecorder {
	if form == nil {
		form = url.Values{}
	}
	if method == http.MethodPost {
		form.Set("csrf_token", b.token)
	}
	req := httptest.NewRequest(method, origin+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	for k, v := range header {
		req.Header[k] = v
	}
	req.AddCookie(&http.Cookie{Name: "session", Value: "ada"})
	for _, c := range b.cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	b.handler.ServeHTTP(rec, req)
	return rec
}

func api(h http.Handler, method, path, key string, body any, header http.Header) *httptest.ResponseRecorder {
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(method, origin+path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	return out
}

func TestTextlab(t *testing.T) {
	a, h, pay := newTextlab(t, origin)
	b := newBrowser(t, h)

	// The default keys page creates a key inside Textlab's own layout.
	rec := b.do(http.MethodPost, "/account/api-keys", url.Values{"name": {"CI"}}, http.Header{"Hx-Request": {"true"}})
	key := regexp.MustCompile(`tl_[A-Za-z0-9_-]{43}`).FindString(rec.Body.String())
	if key == "" {
		t.Fatalf("no key in %s", rec.Body.String())
	}
	if page := b.do(http.MethodGet, "/account/api-keys", nil, nil).Body.String(); !strings.Contains(page, `class="console"`) || !strings.Contains(page, "CI") {
		t.Fatal("the keys page is not in the console layout")
	}

	// Without credit, the API refuses work.
	analyse := func(idempotencyKey string) *httptest.ResponseRecorder {
		return api(h, http.MethodPost, "/api/v1/analyse", key, map[string]string{"text": "one two\nthree"}, http.Header{"Idempotency-Key": {idempotencyKey}})
	}
	if rec := analyse("first"); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("analyse without credit: %d", rec.Code)
	}

	// Buy €10 of credit: the checkout redirect, then the provider's signed webhook.
	rec = b.do(http.MethodPost, "/account/credits", url.Values{"amount": {"10"}}, nil)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "https://pay.example/") {
		t.Fatalf("checkout: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	checkout := pay.last()
	req := httptest.NewRequest(http.MethodPost, origin+"/webhooks/payments", bytes.NewReader(paidEvent(checkout)))
	req.Header.Set("X-Signature", "valid")
	webhook := httptest.NewRecorder()
	h.ServeHTTP(webhook, req)
	if webhook.Code != http.StatusOK {
		t.Fatalf("webhook: %d", webhook.Code)
	}
	balance := func() map[string]any { return decode(t, api(h, http.MethodGet, "/api/v1/balance", key, nil, nil)) }
	if got := balance(); got["available_cents"] != float64(1000) {
		t.Fatalf("balance after purchase %v", got)
	}

	// An Idempotency-Key binds one request to one charge.
	if got := decode(t, analyse("first")); got["words"] != float64(3) || got["lines"] != float64(2) {
		t.Fatalf("analyse %v", got)
	}
	if got := decode(t, analyse("first")); got["words"] != float64(3) || got["charged_cents"] != float64(2) {
		t.Fatalf("retried analysis %v", got)
	}
	other := api(h, http.MethodPost, "/api/v1/analyse", key, map[string]string{"text": "a different text"}, http.Header{"Idempotency-Key": {"first"}})
	if other.Code != http.StatusConflict {
		t.Fatalf("another text with the same key: %d", other.Code)
	}
	if rec := analyse(""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing key: %d", rec.Code)
	}
	if got := balance(); got["available_cents"] != float64(998) {
		t.Fatalf("balance after a repeated request %v", got)
	}

	// A job holds its cost until it completes, then is charged once.
	text := strings.Repeat("x", 250)
	rec = api(h, http.MethodPost, "/api/v1/jobs", key, map[string]string{"text": text}, nil)
	job := decode(t, rec)
	if rec.Code != http.StatusAccepted || job["reserved_cents"] != float64(3) {
		t.Fatalf("job %d %v", rec.Code, job)
	}
	if got := balance(); got["held_cents"] != float64(3) || got["available_cents"] != float64(995) {
		t.Fatalf("balance with a running job %v", got)
	}
	id := job["id"].(string)
	for range 2 {
		if rec := api(h, http.MethodPost, "/api/v1/jobs/"+id+"/complete", key, nil, nil); rec.Code != http.StatusOK {
			t.Fatalf("complete: %d %s", rec.Code, rec.Body.String())
		}
	}
	if got := balance(); got["held_cents"] != float64(0) || got["balance_cents"] != float64(995) {
		t.Fatalf("balance after the job %v", got)
	}
	if rec := api(h, http.MethodPost, "/api/v1/jobs/00000000-0000-0000-0000-000000000000/complete", key, nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job: %d", rec.Code)
	}
	if page := b.do(http.MethodGet, "/account/jobs", nil, nil).Body.String(); !strings.Contains(page, "Done") {
		t.Fatal("the jobs page does not show the job")
	}

	// Deleting the account removes its jobs and keys and forfeits its credit.
	if rec := b.do(http.MethodPost, "/account/delete", url.Values{"confirmation": {"DELETE"}}, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", rec.Code)
	}
	var jobs, keys int
	a.DB().QueryRow(`SELECT count(*) FROM jobs`).Scan(&jobs)
	a.DB().QueryRow(`SELECT count(*) FROM api_keys`).Scan(&keys)
	if jobs != 0 || keys != 0 {
		t.Fatalf("after deletion: %d jobs, %d keys", jobs, keys)
	}
	if rec := api(h, http.MethodGet, "/api/v1/balance", key, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a deleted account's key: %d", rec.Code)
	}
}

// TestOIDC signs in to the test app's OpenID Connect client as ada, through Textlab's
// own wiring. The oidc module's tests cover the protocol.
func TestOIDC(t *testing.T) {
	_, h, _ := newTextlab(t, origin)
	newBrowser(t, h) // ada signs up and gives her name
	if got := decode(t, api(h, http.MethodGet, "/.well-known/openid-configuration", "", nil, nil)); got["issuer"] != origin {
		t.Fatalf("discovery %v", got)
	}
	var cookies []*http.Cookie
	get := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, origin+strings.TrimPrefix(target, origin), nil)
		req.AddCookie(&http.Cookie{Name: "session", Value: "ada"})
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		cookies = append(cookies, rec.Result().Cookies()...)
		return rec
	}
	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	redirect := "https://notes.example.com/callback"
	rec := get("/oidc/authorize?" + url.Values{
		"client_id": {"notes"}, "redirect_uri": {redirect}, "response_type": {"code"},
		"scope": {"openid email profile"}, "state": {"s1"}, "nonce": {"n1"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}.Encode())
	rec = get(rec.Header().Get("Location"))
	back, _ := url.Parse(rec.Header().Get("Location"))
	if !strings.HasPrefix(back.String(), redirect+"?") || back.Query().Get("state") != "s1" {
		t.Fatalf("sent to %s", back)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "redirect_uri": {redirect}, "code_verifier": {verifier}}
	req := httptest.NewRequest(http.MethodPost, origin+"/oidc/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("notes", testClientSecret)
	tok := httptest.NewRecorder()
	h.ServeHTTP(tok, req)
	idToken, _ := decode(t, tok)["id_token"].(string)
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		t.Fatalf("token response %s", tok.Body.String())
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil || claims["email"] != "ada@example.com" || claims["name"] != "Ada Lovelace" || claims["nonce"] != "n1" {
		t.Fatalf("claims %s", payload)
	}
}

// TestPlans sells the plans: the overview offers them, the checkout goes to the
// provider, and once the provider's webhook starts one, an analysis costs what the plan
// says. A change asks first, with the provider's quote; one from a yearly plan to a
// monthly one waits for the end of the year, unless the account keeps what it has.
func TestPlans(t *testing.T) {
	_, h, pay := newTextlab(t, origin)
	b := newBrowser(t, h)
	keyRec := b.do(http.MethodPost, "/account/api-keys", url.Values{"name": {"Plans"}}, http.Header{"Hx-Request": {"true"}})
	key := regexp.MustCompile(`tl_[A-Za-z0-9_-]{43}`).FindString(keyRec.Body.String())
	page := b.do(http.MethodGet, "/account", nil, nil)
	if body := page.Body.String(); !strings.Contains(body, `value="pro-monthly"`) || !strings.Contains(body, `value="max-yearly"`) ||
		!strings.Contains(page.Header().Get("Content-Security-Policy"), "https://pay.example") {
		t.Fatalf("overview without a plan: %s", page.Header().Get("Content-Security-Policy"))
	}
	rec := b.do(http.MethodPost, "/account/subscriptions", url.Values{"product": {"pro-monthly"}, "next": {"/account"}}, nil)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "https://pay.example/co_") {
		t.Fatalf("checkout: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	c := pay.subscriptionCheckouts[0]
	if c.ProductID != "prod_pro_monthly" || c.Email != "ada@example.com" || c.SuccessURL != origin+"/account/subscriptions/started?next=%2Faccount" {
		t.Fatalf("checkout %+v", c)
	}
	webhook := func(e payments.SubscriptionEvent) {
		t.Helper()
		body, _ := json.Marshal(e)
		req := httptest.NewRequest(http.MethodPost, origin+"/webhooks/subscriptions", bytes.NewReader(body))
		req.Header.Set("X-Signature", "valid")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("webhook %s: %d", e.ID, rec.Code)
		}
	}
	periodEnd := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	webhook(payments.SubscriptionEvent{ID: "evt_start", Kind: payments.SubscriptionStarted, Reference: c.Reference, CreatedAt: time.Now(),
		State: payments.SubscriptionState{ID: "sub_1", CustomerID: "cust_ada", ProductID: "prod_pro_monthly", Units: 1, Status: payments.SubscriptionActive, PeriodEnd: periodEnd}})
	if page := b.do(http.MethodGet, "/account", nil, nil).Body.String(); !strings.Contains(page, "Pro, monthly") || !strings.Contains(page, "Renews on "+day(periodEnd)) {
		t.Fatal("the overview doesn't show the plan")
	}
	if page := b.do(http.MethodGet, "/account/settings", nil, nil).Body.String(); !strings.Contains(page, "Pro, monthly") {
		t.Fatal("the settings page doesn't list the subscription")
	}
	// Buy credit, then analyse at Pro's price.
	b.do(http.MethodPost, "/account/credits", url.Values{"amount": {"10"}}, nil)
	paid := httptest.NewRequest(http.MethodPost, origin+"/webhooks/payments", bytes.NewReader(paidEvent(pay.last())))
	paid.Header.Set("X-Signature", "valid")
	h.ServeHTTP(httptest.NewRecorder(), paid)
	analyse := func(idempotencyKey string) float64 {
		t.Helper()
		got := decode(t, api(h, http.MethodPost, "/api/v1/analyse", key, map[string]string{"text": "one two"}, http.Header{"Idempotency-Key": {idempotencyKey}}))
		cost, ok := got["charged_cents"].(float64)
		if !ok {
			t.Fatalf("analysis %s: %v", idempotencyKey, got)
		}
		return cost
	}
	if cost := analyse("pro-1"); cost != 1 {
		t.Fatalf("analysis on Pro: %v", cost)
	}
	// While it renews, the account can't be deleted: the delete page lists it.
	if body := b.do(http.MethodGet, "/account/delete", nil, nil).Body.String(); !strings.Contains(body, "No subscription renews") || !strings.Contains(body, "You have a subscription that renews.") || strings.Contains(body, "delete-confirmation") {
		t.Fatalf("delete page with a plan: %s", body)
	}
	if rec := b.do(http.MethodPost, "/account/delete", url.Values{"confirmation": {"DELETE"}}, nil); rec.Code != http.StatusConflict {
		t.Fatalf("deletion with a plan: %d", rec.Code)
	}

	// A change asks first, with the provider's quote, then makes it at the provider.
	page = b.do(http.MethodGet, "/account/plan/change/max-monthly", nil, nil)
	if body := page.Body.String(); page.Code != http.StatusOK || !strings.Contains(body, "Pay €10.00 and change") || !strings.Contains(body, "€20.00 a month, plus VAT") {
		t.Fatalf("change page: %d %s", page.Code, body)
	}
	action := regexp.MustCompile(`/account/subscriptions/[0-9a-f-]{36}/change`).FindString(page.Body.String())
	change := func(product string) {
		t.Helper()
		rec := b.do(http.MethodPost, action, url.Values{"product": {product}, "next": {"/account"}}, nil)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account" {
			t.Fatalf("change to %s: %d %q", product, rec.Code, rec.Header().Get("Location"))
		}
	}
	change("max-monthly")
	if changes := pay.testChanges(); len(changes) != 1 || changes[0].ProductID != "prod_max_monthly" {
		t.Fatalf("changes at the provider: %+v", changes)
	}
	if cost := analyse("max-1"); cost != 0 {
		t.Fatalf("analysis on Max: %v", cost)
	}

	// To yearly at once; back to monthly waits for the end of the year paid for.
	change("max-yearly")
	page = b.do(http.MethodGet, "/account/plan/change/pro-monthly", nil, nil)
	if body := page.Body.String(); !strings.Contains(body, "Nothing changes now") || !strings.Contains(body, "Change on "+day(periodEnd)) {
		t.Fatalf("waiting change page: %s", body)
	}
	change("pro-monthly")
	if changes := pay.testChanges(); len(changes) != 2 {
		t.Fatalf("a waiting change reached the provider: %+v", changes)
	}
	overview := b.do(http.MethodGet, "/account", nil, nil).Body.String()
	if !strings.Contains(overview, "Changes to Pro, monthly on "+day(periodEnd)) || !strings.Contains(overview, "Keep Max, yearly") {
		t.Fatal("the overview doesn't show the waiting change")
	}
	if cost := analyse("max-2"); cost != 0 {
		t.Fatalf("analysis while Max waits to change: %v", cost)
	}
	change("max-yearly")
	if overview := b.do(http.MethodGet, "/account", nil, nil).Body.String(); strings.Contains(overview, "Changes to") {
		t.Fatal("keeping the plan didn't drop the waiting change")
	}

	// Canceled, it counts until its period ends, and not after, even if the webhook that
	// ends it never comes.
	for _, tc := range []struct {
		end  time.Time
		cost float64
	}{{time.Now().Add(time.Hour), 0}, {time.Now().Add(-time.Hour), 2}} {
		webhook(payments.SubscriptionEvent{ID: "evt_cancel_" + tc.end.Format(time.RFC3339Nano), Kind: payments.SubscriptionChanged,
			Reference: c.Reference, CreatedAt: time.Now(),
			State: payments.SubscriptionState{ID: "sub_1", Status: payments.SubscriptionCanceling, PeriodEnd: tc.end}})
		if cost := analyse("after-" + tc.end.Format(time.RFC3339Nano)); cost != tc.cost {
			t.Fatalf("canceling, ending %s: %v", tc.end, cost)
		}
	}
}

// TestInternalAPI: with an internal address, Textlab serves the subscription reviews to
// a caller with the token, and only to it; without one, it serves no internal API.
func TestInternalAPI(t *testing.T) {
	if _, h, _ := newTextlabWith(t, origin, nil); h.internal != nil {
		t.Fatal("an internal API without TEXTLAB_INTERNAL_ADDR")
	}
	sum := sha256.Sum256([]byte("staff-app-token"))
	_, h, _ := newTextlabWith(t, origin, func(c *config) {
		c.InternalAddr = "127.0.0.1:0"
		c.InternalTokenHashes = []string{fmt.Sprintf("%x", sum)}
	})
	call := func(method, path, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://textlab-internal:8081"+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.internal.ServeHTTP(rec, req)
		return rec
	}
	if rec := call(http.MethodGet, "/v1/reviews", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without the token: %d", rec.Code)
	}
	if got := decode(t, call(http.MethodGet, "/v1/reviews", "staff-app-token", "")); fmt.Sprint(got["reviews"]) != "[]" {
		t.Fatalf("no reviews yet: %v", got)
	}

	// A renewal of a plan no account has goes to review.
	body, _ := json.Marshal(payments.SubscriptionEvent{ID: "evt_stranger", Kind: payments.SubscriptionChanged, CreatedAt: time.Now(),
		State: payments.SubscriptionState{ID: "sub_stranger", ProductID: "prod_pro_monthly", Units: 1, Status: payments.SubscriptionActive}})
	req := httptest.NewRequest(http.MethodPost, origin+"/webhooks/subscriptions", bytes.NewReader(body))
	req.Header.Set("X-Signature", "valid")
	h.public.ServeHTTP(httptest.NewRecorder(), req)

	reviews, _ := decode(t, call(http.MethodGet, "/v1/reviews", "staff-app-token", ""))["reviews"].([]any)
	if len(reviews) != 1 {
		t.Fatalf("reviews: %v", reviews)
	}
	review := reviews[0].(map[string]any)
	if review["event_id"] != "evt_stranger" || review["reason"] != "unknown_subscription" || review["account_id"] != nil {
		t.Fatalf("the review: %v", review)
	}
	resolve := fmt.Sprintf("/v1/reviews/%v/resolve", review["id"])
	if rec := call(http.MethodPost, resolve, "staff-app-token", `{"note":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("resolving without a note: %d", rec.Code)
	}
	if rec := call(http.MethodPost, resolve, "staff-app-token", `{"note":"not ours: a test at Creem"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("resolving: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodPost, resolve, "staff-app-token", `{"note":"again"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("resolving twice: %d", rec.Code)
	}
	if got := decode(t, call(http.MethodGet, "/v1/reviews", "staff-app-token", "")); fmt.Sprint(got["reviews"]) != "[]" {
		t.Fatalf("reviews after resolving: %v", got)
	}
}
