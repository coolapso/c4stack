//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

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
	rec := b.do(http.MethodGet, "/account/api-keys", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("console: %d", rec.Code)
	}
	b.token = regexp.MustCompile(`<meta name="csrf-token" content="([^"]+)"`).FindStringSubmatch(rec.Body.String())[1]
	b.cookies = rec.Result().Cookies()
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
