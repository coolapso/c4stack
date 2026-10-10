package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/a-h/templ"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

func get(r http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://app.example"+target, nil))
	return rec
}

func engine(t *testing.T) *gin.Engine {
	r, err := web.New(web.Config{PublicURL: "https://app.example", SecretKey: strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAssets(t *testing.T) {
	assets := NewAssets("/static/")
	if err := assets.Add("app", fstest.MapFS{"app.css": {Data: []byte("body{}")}, "img/logo.svg": {Data: []byte("<svg/>")}}); err != nil {
		t.Fatal(err)
	}
	if err := assets.Add("app", fstest.MapFS{"other.css": {Data: []byte("")}}); err == nil {
		t.Fatal("added the same prefix twice")
	}
	r := engine(t)
	r.GET("/static/*path", assets.Serve)

	url := assets.URL("app/app.css")
	if !strings.HasPrefix(url, "/static/") || !strings.HasSuffix(url, "/app/app.css") {
		t.Fatalf("URL %q", url)
	}
	rec := get(r, url)
	if rec.Code != http.StatusOK || rec.Body.String() != "body{}" || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("current hash: %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("content type %q", ct)
	}
	old := get(r, "/static/000000000000/app/app.css")
	if old.Code != http.StatusOK || old.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("old hash: %d %q", old.Code, old.Header().Get("Cache-Control"))
	}
	if get(r, "/static/x/app/missing.css").Code != http.StatusNotFound || get(r, "/static/nohash").Code != http.StatusNotFound {
		t.Fatal("unknown asset served")
	}
	if get(r, assets.URL("ui/htmx.js")).Code != http.StatusOK || get(r, assets.URL("app/img/logo.svg")).Code != http.StatusOK {
		t.Fatal("default or nested asset missing")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("URL of an unknown asset did not panic")
		}
	}()
	assets.URL("app/missing.css")
}

func TestDefaultLayout(t *testing.T) {
	assets := NewAssets("/assets")
	layout := Default(Design{
		Name:        "Acme",
		Nav:         []Link{{"Home", "/"}, {"Account", "/account"}},
		Stylesheets: []string{"/assets/x/app/app.css"},
		Head:        templ.Raw(`<link rel="icon" href="/favicon.svg">`),
	}, assets)
	r := engine(t)
	r.GET("/account/settings", func(c *gin.Context) {
		layout.Render(c, http.StatusOK, "Settings", templ.Raw("<p>body</p>"))
	})
	r.NoRoute(layout.NotFound)

	html := get(r, "/account/settings").Body.String()
	for _, want := range []string{
		"<title>Settings · Acme</title>",
		`<meta name="csrf-token" content="`,
		assets.URL("ui/base.css"),
		`href="/assets/x/app/app.css"`,
		`<link rel="icon" href="/favicon.svg">`,
		`<a href="/account" aria-current="page">Account</a>`,
		`<a href="/">Home</a>`,
		"<p>body</p>",
		`id="live-status"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Index(html, "ui/base.css") > strings.Index(html, "app/app.css") {
		t.Error("app stylesheet comes before the default one")
	}
	if rec := get(r, "/missing"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "Page not found") {
		t.Errorf("not found: %d", rec.Code)
	}
}
