//go:build browser

package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/app/apptest"
	"github.com/coolapso/c4stack/identity/local"
	"github.com/gin-gonic/gin"
)

// TestOIDCSignInBrowser signs in to a client on another origin through Textlab's
// OpenID Connect provider, in headless Chromium: first as a new account, which signs up
// with an emailed code and gives its name, then again with a code after signing out.
// Each time the browser must arrive back at the client with a code. The sign-in pages'
// forms lead there through the provider's redirects, and a browser stops a form's
// navigation at any origin its page's form-action doesn't allow: the button then spins
// forever. Any console error, uncaught exception or CSP violation fails the test.
func TestOIDCSignInBrowser(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// The client: another origin, whose callback only says where the browser arrived.
	client := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><title>Notes</title><p id=arrived>Signed in</p>"))
	}))
	defer client.Close()
	redirect := client.URL + "/callback"

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	public := "http://localhost:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	a := apptest.New(t, app.Config{Name: "textlab", Migrations: appMigrations})
	mail := &mailbox{}
	idp, err := local.New(a, local.Config{AppName: "Textlab", PublicURL: public, SecretKey: strings.Repeat("s", 32), Sender: mail, SignedInPath: "/account"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := build(a, config{
		PublicURL: public, SecretKey: strings.Repeat("s", 32),
		OIDCClientID: "notes", OIDCClientSecret: "notes-secret", OIDCRedirectURIs: []string{redirect},
	}, providers{identity: idp, login: idp, payments: &testPayments{}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h.public}}
	srv.Start()
	defer srv.Close()

	alloc, cancel := chromedp.NewExecAllocator(context.Background(), append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true), chromedp.NoSandbox)...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var mu sync.Mutex
	var problems []string
	messages := chromedp.Console(ctx)
	go func() {
		for m, err := range messages {
			if err != nil {
				return
			}
			if m.Type == chromedp.ConsoleError || m.IsException() {
				mu.Lock()
				problems = append(problems, m.String())
				mu.Unlock()
			}
		}
	}()

	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	authorize := public + "/oidc/authorize?" + url.Values{
		"client_id": {"notes"}, "redirect_uri": {redirect}, "response_type": {"code"},
		"scope": {"openid email profile"}, "state": {"s1"}, "nonce": {"n1"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}.Encode()

	// arrived waits until the browser is back at the client with a code.
	arrived := func(step string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			at, _ := chromedp.Run(ctx, chromedp.Location())
			if u, err := url.Parse(at); err == nil && strings.HasPrefix(at, redirect+"?") && u.Query().Get("code") != "" && u.Query().Get("state") == "s1" {
				return
			}
			if time.Now().After(deadline) {
				mu.Lock()
				defer mu.Unlock()
				t.Fatalf("%s: never got back to the client, still at %s\n%s", step, at, strings.Join(problems, "\n"))
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	signInWithCode := func() {
		t.Helper()
		if err := chromedp.Do(ctx,
			chromedp.Navigate(authorize),
			chromedp.WaitVisible(`#login-email`),
			chromedp.SendKeys(`#login-email`, "ann@example.com"),
			chromedp.Submit(`#login-email`),
			chromedp.WaitVisible(`#code`),
		); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Do(ctx, chromedp.SendKeys(`#code`, mail.code(t)), chromedp.Submit(`#code`)); err != nil {
			t.Fatal(err)
		}
	}

	// A new account: the code, then its name, then back to the client.
	signInWithCode()
	if err := chromedp.Do(ctx, chromedp.WaitVisible(`#name`), chromedp.SendKeys(`#name`, "Ann Example"), chromedp.Submit(`#name`)); err != nil {
		t.Fatal(err)
	}
	arrived("signing up")

	// Signed out, the account signs in again with a code, straight back to the client.
	if err := chromedp.Do(ctx,
		chromedp.Navigate(public+"/account/settings"),
		chromedp.Submit(`form[action="/logout"]`),
		chromedp.WaitReady(`body`),
	); err != nil {
		t.Fatal(err)
	}
	signInWithCode()
	arrived("signing in again")

	// Already signed in, the browser goes straight back, by a redirect.
	if err := chromedp.Do(ctx, chromedp.Navigate(authorize)); err != nil {
		t.Fatal(err)
	}
	arrived("signed in already")

	time.Sleep(200 * time.Millisecond) // let late console messages arrive
	mu.Lock()
	defer mu.Unlock()
	if len(problems) > 0 {
		t.Fatalf("browser problems:\n%s", strings.Join(problems, "\n"))
	}
}
