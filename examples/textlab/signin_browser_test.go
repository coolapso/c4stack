//go:build browser

package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/webauthn"
	"github.com/chromedp/chromedp"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/app/apptest"
	"github.com/coolapso/c4stack/email"
	"github.com/coolapso/c4stack/identity/local"
	"github.com/gin-gonic/gin"
)

// mailbox keeps the emails sign-in sends.
type mailbox struct {
	mu   sync.Mutex
	last string
}

func (m *mailbox) Send(_ context.Context, msg email.Message) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = msg.Text
	return "id", nil
}

var codePattern = regexp.MustCompile(`\b(\d{4}) (\d{4})\b`)

func (m *mailbox) code(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	code := codePattern.FindStringSubmatch(m.last)
	if code == nil {
		t.Fatalf("no code in the last email:\n%s", m.last)
	}
	return code[1] + code[2]
}

// TestSignInBrowser drives Textlab's own sign-in in headless Chromium, with its virtual
// authenticator: sign up with an emailed code, give a name, add a passkey on the
// settings page, sign out, sign in with the passkey, and confirm with it before deleting
// the account. Any console error, uncaught exception or CSP violation on the way fails
// the test.
func TestSignInBrowser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Passkeys need a domain, not an IP address, so the app is reached as localhost.
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
	handler, err := build(a, config{PublicURL: public, SecretKey: strings.Repeat("s", 32)}, providers{
		identity: idp, login: idp, payments: &testPayments{},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: handler}}
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

	if err := chromedp.Do(ctx, chromedp.Navigate(public+"/login?next=/account")); err != nil {
		t.Fatal(err)
	}
	if _, err := chromedp.Call(ctx, webauthn.Enable, webauthn.EnableParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := chromedp.Call(ctx, webauthn.AddVirtualAuthenticator, webauthn.AddVirtualAuthenticatorParams{Options: &webauthn.VirtualAuthenticatorOptions{
		Protocol: webauthn.AuthenticatorProtocolCtap2, Transport: webauthn.AuthenticatorTransportInternal,
		HasResidentKey: true, HasUserVerification: true, IsUserVerified: true, AutomaticPresenceSimulation: true,
	}}); err != nil {
		t.Fatal(err)
	}
	// waitFor waits until the page is at path, across the navigations on the way.
	waitFor := func(path string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			at, err := chromedp.Run(ctx, chromedp.Location())
			if err == nil && strings.TrimPrefix(at, public) == path {
				if _, err := chromedp.Run(ctx, chromedp.WaitReady(`body`)); err == nil {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("waiting for %s, at %s", path, at)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Sign up with the emailed code.
	if err := chromedp.Do(ctx,
		chromedp.SendKeys(`#login-email`, "ann@example.com"),
		chromedp.Submit(`#login-email`),
		chromedp.WaitVisible(`#code`),
	); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Do(ctx, chromedp.SendKeys(`#code`, mail.code(t)), chromedp.Submit(`#code`)); err != nil {
		t.Fatal(err)
	}
	// A new account gives its name first.
	waitFor("/account/name?next=%2Faccount")
	if err := chromedp.Do(ctx, chromedp.SendKeys(`#name`, "Ann Example"), chromedp.Submit(`#name`)); err != nil {
		t.Fatal(err)
	}
	waitFor("/account")

	// Add a passkey on the settings page.
	if err := chromedp.Do(ctx,
		chromedp.Navigate(public+"/account/settings"),
		chromedp.WaitVisible(`form[data-passkey-add]`),
		chromedp.SendKeys(`#passkey-name`, "Virtual key"),
		chromedp.Click(`form[data-passkey-add] button[type="submit"]`),
	); err != nil {
		t.Fatal(err)
	}
	waitFor("/account/settings?done=passkey-added#passkeys")
	if list, _ := chromedp.Run(ctx, chromedp.Text(`#passkeys`)); !strings.Contains(list, "Virtual key") {
		t.Fatalf("passkeys: %q", list)
	}

	// Sign out, then sign in with the passkey.
	if err := chromedp.Do(ctx, chromedp.Submit(`form[action="/logout"]`)); err != nil {
		t.Fatal(err)
	}
	waitFor("/")
	// clickPasskey clicks the page's passkey button. The virtual authenticator answers at
	// once, so the page can navigate away while chromedp still finishes the click, or
	// before it, through the login page's autofill, which chromedp reports as a node it
	// can't find: the waitFor after it checks where it led.
	clickPasskey := func() {
		t.Helper()
		if err := chromedp.Do(ctx, chromedp.WaitVisible(`[data-passkey-start]`)); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Do(ctx, chromedp.Click(`[data-passkey-start]`)); err != nil && !strings.Contains(err.Error(), "Could not find node") {
			t.Fatal(err)
		}
	}
	// The login page's address field already asks the authenticator through autofill,
	// which the virtual authenticator sometimes answers by itself: either way, it's a
	// sign-in with the passkey. The button is clicked only if the page is still there.
	if err := chromedp.Do(ctx, chromedp.Navigate(public+"/login?next=/account")); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		if at, _ := chromedp.Run(ctx, chromedp.Location()); strings.TrimPrefix(at, public) == "/account" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("neither the passkey button nor autofill signed in")
		}
		// Never wait on the button for long: autofill may take the page away meanwhile.
		short, cancel := context.WithTimeout(ctx, time.Second)
		err := chromedp.Do(short, chromedp.Click(`[data-passkey-start]`, chromedp.NodeVisible))
		cancel()
		if err == nil || strings.Contains(err.Error(), "Could not find node") {
			break
		}
	}
	waitFor("/account")

	// Confirm with the passkey before deleting the account.
	if err := chromedp.Do(ctx, chromedp.Navigate(public+"/login/confirm?next=/account/delete")); err != nil {
		t.Fatal(err)
	}
	clickPasskey()
	waitFor("/account/delete")
	if page, _ := chromedp.Run(ctx, chromedp.Text(`main`)); !strings.Contains(page, "Type DELETE to confirm") {
		t.Fatalf("the delete page asks to confirm again:\n%s", page)
	}

	time.Sleep(200 * time.Millisecond) // let late console messages arrive
	mu.Lock()
	defer mu.Unlock()
	if len(problems) > 0 {
		t.Fatalf("browser problems:\n%s", strings.Join(problems, "\n"))
	}
}
