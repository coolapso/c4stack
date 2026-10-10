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
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/gin-gonic/gin"
)

// TestBrowser drives the default pages inside Textlab's layout in headless Chromium,
// where HTMX and the page script matter: the key dialog, the one-time key reveal, the
// revoke confirmation and the live price quote. Any console error, uncaught exception
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

	// Collect console problems, and accept the revoke confirmation when it opens.
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
	dialogs := chromedp.Events(ctx, page.JavascriptDialogOpening)
	go func() {
		for _, err := range dialogs {
			if err != nil {
				return
			}
			chromedp.Call(ctx, page.HandleJavaScriptDialog, page.HandleJavaScriptDialogParams{Accept: true})
		}
	}()

	if _, err := chromedp.Call(ctx, network.SetCookie, network.SetCookieParams{Name: "session", Value: "ada", URL: public + "/"}); err != nil {
		t.Fatal(err)
	}
	err := chromedp.Do(ctx,
		chromedp.Navigate(public+"/console/keys"),
		chromedp.WaitVisible(`.console-side`),
		chromedp.Click(`button[data-dialog-open="key-create-dialog"]`),
		chromedp.WaitVisible(`#key-create-dialog`),
		chromedp.SendKeys(`#key-name`, "Laptop"),
		chromedp.Click(`#key-create-dialog button[type="submit"]`),
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
		chromedp.WaitNotVisible(`#key-create-dialog`),
	)
	if err != nil {
		t.Fatal(err)
	}
	list, err := chromedp.Run(ctx, chromedp.Text(`#key-list`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "Laptop") || strings.Contains(list, key) {
		t.Fatalf("key list %q", list)
	}

	err = chromedp.Do(ctx,
		chromedp.Click(`#key-list button[type="submit"]`),
		chromedp.WaitVisible(`#key-list .tag.danger`),
		chromedp.Navigate(public+"/console/credits"),
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
