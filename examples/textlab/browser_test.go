//go:build browser

package main

import (
	"context"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/gin-gonic/gin"
)

// TestBrowser drives the default pages inside Textlab's layout in headless Chromium,
// where HTMX and the page script matter: the key dialogs, the one-time key reveal, the
// list's refresh after a revocation, and the live price quote. Any console error, uncaught exception
// or CSP violation on the way fails the test.
func TestBrowser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	srv := httptest.NewUnstartedServer(nil)
	public := "http://" + srv.Listener.Addr().String()
	_, handler, _ := newTextlab(t, public)
	srv.Config.Handler = handler
	srv.Start()
	defer srv.Close()

	alloc, cancel := chromedp.NewExecAllocator(context.Background(), append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true), chromedp.NoSandbox)...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Collect console problems.
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

	if _, err := chromedp.Call(ctx, network.SetCookie, network.SetCookieParams{Name: "session", Value: "ada", URL: public + "/"}); err != nil {
		t.Fatal(err)
	}
	err := chromedp.Do(ctx,
		chromedp.Navigate(public+"/account/api-keys"),
		chromedp.WaitVisible(`.console-side`),
		chromedp.Click(`#create-key-button`),
		chromedp.WaitVisible(`#create-key-dialog`),
		chromedp.SendKeys(`#create-key-name`, "Laptop"),
		chromedp.Click(`#create-key-dialog button[type="submit"]`),
		chromedp.WaitVisible(`#key-reveal-dialog`),
	)
	if err != nil {
		t.Fatal(err)
	}
	key, err := chromedp.Run(ctx, chromedp.Text(`#new-key`))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^tl_[A-Za-z0-9_-]{43}$`).MatchString(key) {
		t.Fatalf("revealed key %q", key)
	}
	err = chromedp.Do(ctx,
		chromedp.Click(`#key-reveal-dialog .btn.primary`),
		chromedp.WaitNotPresent(`#key-reveal-dialog`),
		chromedp.WaitNotVisible(`#create-key-dialog`),
	)
	if err != nil {
		t.Fatal(err)
	}
	list, err := chromedp.Run(ctx, chromedp.Text(`#keys-list`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "Laptop") || strings.Contains(list, key) {
		t.Fatalf("key list %q", list)
	}

	err = chromedp.Do(ctx,
		chromedp.Click(`#keys-list button[data-dialog-open^="revoke-key-"]`),
		chromedp.WaitVisible(`dialog[id^="revoke-key-"]`),
		chromedp.Click(`dialog[id^="revoke-key-"] button[type="submit"]`),
		chromedp.WaitVisible(`#keys-list button[data-dialog-open^="delete-key-"]`),
		chromedp.Navigate(public+"/account/credits"),
		chromedp.Clear(`#credit-amount`),
		chromedp.SendKeys(`#credit-amount`, "10"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chromedp.Run(ctx, chromedp.Poll[bool](`document.querySelector('#credit-quote').textContent.includes('€10.83')`,
		chromedp.WithPollingTimeout(10*time.Second))); err != nil {
		quote, _ := chromedp.Run(ctx, chromedp.Text(`#credit-quote`))
		t.Fatalf("quote %q: %v", quote, err)
	}

	time.Sleep(200 * time.Millisecond) // let late console messages arrive
	mu.Lock()
	defer mu.Unlock()
	if len(problems) > 0 {
		t.Fatalf("browser problems:\n%s", strings.Join(problems, "\n"))
	}
}
