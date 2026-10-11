package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

func newEngine(t *testing.T, cfg Config) *gin.Engine {
	t.Helper()
	if cfg.PublicURL == "" {
		cfg.PublicURL = "https://app.example"
	}
	if cfg.SecretKey == "" {
		cfg.SecretKey = testSecret
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func request(r http.Handler, method, target string, body io.Reader, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestNewValidatesConfig(t *testing.T) {
	bad := []Config{
		{PublicURL: "http://app.example", SecretKey: testSecret},
		{PublicURL: "https://app.example/path", SecretKey: testSecret},
		{PublicURL: "https://app.example?x=1", SecretKey: testSecret},
		{PublicURL: "https://user@app.example", SecretKey: testSecret},
		{PublicURL: "ftp://app.example", SecretKey: testSecret},
		{PublicURL: "app.example", SecretKey: testSecret},
		{PublicURL: "https://app.example", SecretKey: "short"},
		{PublicURL: "https://app.example", SecretKey: testSecret, MaxBodyBytes: -1},
	}
	for _, cfg := range bad {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) succeeded, want an error", cfg)
		}
	}
	good := []string{"https://app.example", "https://app.example/", "http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"}
	for _, u := range good {
		if _, err := New(Config{PublicURL: u, SecretKey: testSecret}); err != nil {
			t.Errorf("New(%q): %v", u, err)
		}
	}
}

func TestHostCheck(t *testing.T) {
	r := newEngine(t, Config{PublicURL: "https://bücher.example"})
	r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, URL(c, "/x")) })

	for _, host := range []string{"xn--bcher-kva.example", "XN--BCHER-KVA.example:443"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "https://xn--bcher-kva.example/x" {
			t.Errorf("host %q: %d %q", host, rec.Code, rec.Body.String())
		}
	}
	for _, host := range []string{"evil.example", "xn--bcher-kva.example:8443", "10.0.0.12:8080"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("host %q: status %d, want 421", host, rec.Code)
		}
	}
}

// An app served on several hostnames lists them in Origins: requests may address them,
// and forms may post from them. AnyHost accepts any host, but not forms from it.
func TestFurtherOrigins(t *testing.T) {
	for _, cfg := range []Config{
		{PublicURL: "https://app.example", SecretKey: testSecret, Origins: []string{"http://localhost:8080"}},
		{PublicURL: "https://app.example", SecretKey: testSecret, Origins: []string{"https://green.app.example/path"}},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) succeeded, want an error", cfg)
		}
	}

	r := newEngine(t, Config{Origins: []string{"https://green.app.example"}})
	r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, URL(c, "/x")) })
	r.POST("/submit", CSRF, func(c *gin.Context) { c.Status(http.StatusOK) })
	b := newBrowser(t, r)
	for host, want := range map[string]int{"app.example": 200, "green.app.example": 200, "evil.example": 421} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != want || want == 200 && rec.Body.String() != "https://app.example/x" {
			t.Errorf("host %q: %d %q", host, rec.Code, rec.Body.String())
		}
	}
	for origin, want := range map[string]int{"https://green.app.example": 200, "https://blue.app.example": 403} {
		if rec := b.post("/submit", url.Values{CSRFField: {b.token}}, http.Header{"Origin": {origin}}); rec.Code != want {
			t.Errorf("form from %s: %d, want %d", origin, rec.Code, want)
		}
	}

	anyHost := newEngine(t, Config{AnyHost: true})
	anyHost.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
	anyHost.POST("/submit", CSRF, func(c *gin.Context) { c.Status(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "tunnel.example"
	rec := httptest.NewRecorder()
	anyHost.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("any host: %d", rec.Code)
	}
	ab := newBrowser(t, anyHost)
	if rec := ab.post("/submit", url.Values{CSRFField: {ab.token}}, http.Header{"Origin": {"https://tunnel.example"}}); rec.Code != http.StatusForbidden {
		t.Errorf("a form from an unlisted host: %d", rec.Code)
	}
}

func TestRequestIDIsGenerated(t *testing.T) {
	r := newEngine(t, Config{})
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	rec := request(r, http.MethodGet, "https://app.example/", nil, http.Header{"X-Request-Id": {"forged"}})
	id := rec.Header().Get("X-Request-ID")
	if len(id) != 32 || id == "forged" {
		t.Fatalf("request ID %q", id)
	}
	if again := request(r, http.MethodGet, "https://app.example/", nil, nil).Header().Get("X-Request-ID"); again == id {
		t.Fatal("request IDs repeat")
	}
}

func TestRecoveryLogsNoSecrets(t *testing.T) {
	var logs bytes.Buffer
	r := newEngine(t, Config{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	r.GET("/boom", func(c *gin.Context) { panic("secret-panic-value") })

	rec := request(r, http.MethodGet, "https://app.example/boom", nil, http.Header{
		"Authorization": {"Bearer secret-api-key"},
		"Cookie":        {"hanko=secret-session"},
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	out := logs.String()
	if !strings.Contains(out, "request panic") || !strings.Contains(out, "/boom") {
		t.Fatalf("panic not logged: %s", out)
	}
	for _, secret := range []string{"secret-panic-value", "secret-api-key", "secret-session"} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q", secret)
		}
	}
}

func TestRequestLogOmitsQuery(t *testing.T) {
	var logs bytes.Buffer
	r := newEngine(t, Config{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	r.GET("/items/:id", func(c *gin.Context) { c.Status(http.StatusOK) })
	request(r, http.MethodGet, "https://app.example/items/42?token=secret", nil, nil)
	request(r, http.MethodGet, "https://app.example/missing?token=secret", nil, nil)
	out := logs.String()
	if !strings.Contains(out, "route=/items/:id") || !strings.Contains(out, "path=/missing") {
		t.Fatalf("unexpected log: %s", out)
	}
	if strings.Contains(out, "secret") {
		t.Fatalf("log contains the query string: %s", out)
	}
}

func TestSecurityHeadersAndCSP(t *testing.T) {
	r := newEngine(t, Config{CSP: CSP{Images: []string{"https://cdn.example"}}})
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
	g := r.Group("/login", AllowCSP(CSP{Connect: []string{"https://auth.example"}, Frames: []string{"https://frame.example"}}))
	g.GET("", func(c *gin.Context) { c.Status(http.StatusOK) })

	h := request(r, http.MethodGet, "https://app.example/", nil, nil).Header()
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'self'", "script-src 'self'", "img-src 'self' data: https://cdn.example",
		"connect-src 'self';", "frame-src 'none'", "frame-ancestors 'none'", "base-uri 'none'", "object-src 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Strict-Transport-Security") == "" {
		t.Errorf("missing security headers: %v", h)
	}

	login := request(r, http.MethodGet, "https://app.example/login", nil, nil).Header().Get("Content-Security-Policy")
	if !strings.Contains(login, "connect-src 'self' https://auth.example") || !strings.Contains(login, "frame-src https://frame.example") {
		t.Errorf("group CSP %q", login)
	}
	if !strings.Contains(login, "img-src 'self' data: https://cdn.example") {
		t.Errorf("group CSP lost the app-wide sources: %q", login)
	}
}

func TestNoHSTSOnLocalHTTP(t *testing.T) {
	r := newEngine(t, Config{PublicURL: "http://localhost:8080"})
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
	rec := request(r, http.MethodGet, "http://localhost:8080/", nil, nil)
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS sent over plain HTTP")
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "csrf=") || strings.Contains(rec.Header().Get("Set-Cookie"), "__Host-") {
		t.Fatalf("cookie %q", rec.Header().Get("Set-Cookie"))
	}
}

// browser simulates a browser session: it keeps cookies and reads the CSRF token
// from a page.
type browser struct {
	t      *testing.T
	r      http.Handler
	cookie string
	token  string
}

func newBrowser(t *testing.T, r *gin.Engine) *browser {
	r.GET("/form", func(c *gin.Context) { c.String(http.StatusOK, CSRFToken(c)) })
	b := &browser{t: t, r: r}
	rec := request(r, http.MethodGet, "https://app.example/form", nil, nil)
	b.token = rec.Body.String()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "__Host-csrf" {
			if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
				t.Fatalf("CSRF cookie attributes: %+v", c)
			}
			b.cookie = c.Name + "=" + c.Value
		}
	}
	if b.cookie == "" || b.token == "" {
		t.Fatal("no CSRF cookie or token")
	}
	return b
}

func (b *browser) post(path string, form url.Values, header http.Header) *httptest.ResponseRecorder {
	if header == nil {
		header = http.Header{}
	}
	header.Set("Cookie", b.cookie)
	header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, ok := header["Origin"]; !ok {
		header.Set("Origin", "https://app.example")
	}
	return request(b.r, http.MethodPost, "https://app.example"+path, strings.NewReader(form.Encode()), header)
}

func TestCSRF(t *testing.T) {
	r := newEngine(t, Config{MaxBodyBytes: 1024})
	r.POST("/submit", CSRF, func(c *gin.Context) { c.String(http.StatusOK, c.PostForm("name")) })
	b := newBrowser(t, r)

	ok := b.post("/submit", url.Values{"name": {"ada"}, CSRFField: {b.token}}, nil)
	if ok.Code != http.StatusOK || ok.Body.String() != "ada" {
		t.Fatalf("valid form: %d %q", ok.Code, ok.Body.String())
	}
	if rec := b.post("/submit", url.Values{}, http.Header{CSRFHeader: {b.token}}); rec.Code != http.StatusOK {
		t.Fatalf("header token: %d", rec.Code)
	}
	if rec := b.post("/submit", url.Values{"name": {"ada"}, CSRFField: {b.token}}, http.Header{"Origin": {"https://APP.example:443"}}); rec.Code != http.StatusOK {
		t.Fatalf("equivalent origin: %d", rec.Code)
	}

	rejected := map[string]*httptest.ResponseRecorder{
		"no token":       b.post("/submit", url.Values{}, nil),
		"wrong token":    b.post("/submit", url.Values{CSRFField: {"x" + b.token}}, nil),
		"foreign origin": b.post("/submit", url.Values{CSRFField: {b.token}}, http.Header{"Origin": {"https://evil.example"}}),
		"null origin":    b.post("/submit", url.Values{CSRFField: {b.token}}, http.Header{"Origin": {"null"}}),
		"no origin":      b.post("/submit", url.Values{CSRFField: {b.token}}, http.Header{"Origin": {""}}),
		"origin path":    b.post("/submit", url.Values{CSRFField: {b.token}}, http.Header{"Origin": {"https://app.example/"}}),
	}
	for name, rec := range rejected {
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, rec.Code)
		}
	}

	planted := &browser{t: t, r: r, cookie: "__Host-csrf=" + strings.Repeat("A", 43), token: b.token}
	if rec := planted.post("/submit", url.Values{CSRFField: {b.token}}, nil); rec.Code != http.StatusForbidden {
		t.Errorf("planted cookie: status %d", rec.Code)
	}

	big := b.post("/submit", url.Values{"name": {strings.Repeat("x", 2048)}, CSRFField: {b.token}}, nil)
	if big.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized form: status %d, want 413", big.Code)
	}
}

// A rejected request leaves its response unwritten, so the app can render an error page.
func TestCSRFRejectionsCanBeRendered(t *testing.T) {
	r := newEngine(t, Config{})
	render := func(c *gin.Context) {
		c.Next()
		if c.IsAborted() && !c.Writer.Written() {
			c.String(c.Writer.Status(), "error page")
		}
	}
	r.POST("/submit", render, CSRF, func(c *gin.Context) { c.Status(http.StatusOK) })
	b := newBrowser(t, r)
	if rec := b.post("/submit", url.Values{}, nil); rec.Code != http.StatusForbidden || rec.Body.String() != "error page" {
		t.Fatalf("rejection: %d %q", rec.Code, rec.Body.String())
	}
}

func TestMaxBody(t *testing.T) {
	r := newEngine(t, Config{MaxBodyBytes: 10})
	read := func(c *gin.Context) {
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusOK)
	}
	r.POST("/small", read)
	r.POST("/upload", MaxBody(100), read)

	if rec := request(r, http.MethodPost, "https://app.example/small", strings.NewReader(strings.Repeat("x", 11)), nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("small: %d", rec.Code)
	}
	if rec := request(r, http.MethodPost, "https://app.example/upload", strings.NewReader(strings.Repeat("x", 50)), nil); rec.Code != http.StatusOK {
		t.Errorf("upload: %d", rec.Code)
	}
	if rec := request(r, http.MethodPost, "https://app.example/upload", strings.NewReader(strings.Repeat("x", 101)), nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("large upload: %d", rec.Code)
	}
}

func TestSealedCookie(t *testing.T) {
	r := newEngine(t, Config{})
	r.GET("/set", func(c *gin.Context) {
		SetSealedCookie(c, "flow", []byte("state"), time.Minute)
		SetSealedCookie(c, "expired", []byte("old"), -time.Second)
	})
	r.GET("/get/:name", func(c *gin.Context) {
		v, err := SealedCookie(c, c.Param("name"))
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		c.String(http.StatusOK, string(v))
	})

	rec := request(r, http.MethodGet, "https://app.example/set", nil, nil)
	cookies := map[string]string{}
	for _, c := range rec.Result().Cookies() {
		cookies[c.Name] = c.Value
	}
	flow := cookies["__Host-flow"]
	if flow == "" || strings.Contains(flow, "state") {
		t.Fatalf("sealed cookie %q", flow)
	}
	get := func(name, cookie string) *httptest.ResponseRecorder {
		return request(r, http.MethodGet, "https://app.example/get/"+name, nil, http.Header{"Cookie": {cookie}})
	}
	if rec := get("flow", "__Host-flow="+flow); rec.Code != http.StatusOK || rec.Body.String() != "state" {
		t.Fatalf("read back: %d %q", rec.Code, rec.Body.String())
	}
	if rec := get("other", "__Host-other="+flow); rec.Code != http.StatusNotFound {
		t.Error("a sealed value was accepted under another cookie name")
	}
	tampered := []byte(flow)
	tampered[len(tampered)-2] ^= 1
	if rec := get("flow", "__Host-flow="+string(tampered)); rec.Code != http.StatusNotFound {
		t.Error("tampered cookie accepted")
	}
	if rec := get("expired", "__Host-expired="+cookies["__Host-expired"]); rec.Code != http.StatusNotFound {
		t.Error("expired cookie accepted")
	}
}

func TestServeHealthAndGracefulShutdown(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	r := newEngine(t, Config{PublicURL: "http://localhost:1"})
	r.GET("/slow", func(c *gin.Context) {
		close(started)
		<-release
		c.String(http.StatusOK, "done")
	})

	ctx, cancel := context.WithCancel(context.Background())
	addr := freeAddr(t)
	readyErr := error(nil)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Server{
			Addr:            addr,
			Handler:         r,
			HealthPath:      "/health",
			DrainDelay:      300 * time.Millisecond,
			ShutdownTimeout: 5 * time.Second,
			Logger:          slog.New(slog.DiscardHandler),
		}, func(context.Context) error { return readyErr })
	}()
	base := "http://" + addr
	waitFor(t, func() bool { return status(base+"/health/ready") == http.StatusNoContent })

	if got := status(base + "/health/live"); got != http.StatusNoContent {
		t.Fatalf("live: %d", got)
	}
	readyErr = context.DeadlineExceeded
	if got := status(base + "/health/ready"); got != http.StatusServiceUnavailable {
		t.Fatalf("ready with a failing check: %d", got)
	}
	readyErr = nil

	slow := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, base+"/slow", nil)
		req.Host = "localhost:1"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			slow <- err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		slow <- string(body)
	}()
	<-started
	cancel()

	waitFor(t, func() bool { return status(base+"/health/ready") == http.StatusServiceUnavailable })
	close(release)
	if got := <-slow; got != "done" {
		t.Fatalf("in-flight request: %q", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.Listener.Addr().String()
	srv.Close()
	return addr
}

func status(target string) int {
	resp, err := http.Get(target)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
