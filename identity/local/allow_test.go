//go:build integration

package local

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

// An allow-list: a refused address is answered as any other, but gets no code and no
// account; an allowed one signs in. Once refused, its sessions stop working, and it
// can't sign in with a passkey or change to an address the list refuses.
func TestAllow(t *testing.T) {
	var mu sync.Mutex
	allowed := map[string]bool{"ann@example.com": true}
	setAllowed := func(address string, ok bool) {
		mu.Lock()
		defer mu.Unlock()
		allowed[address] = ok
	}
	f := newFixtureWith(t, func(cfg *Config) {
		cfg.Allow = func(address string) bool {
			mu.Lock()
			defer mu.Unlock()
			return allowed[address]
		}
	})

	// Bob isn't on the list: the same pages as anyone, no email, no account.
	bob := f.browser("192.0.2.1")
	if r := bob.askCode("Bob@example.com"); r.status != http.StatusSeeOther || r.location != "/login/code" {
		t.Fatalf("asking for a code: %d %s", r.status, r.location)
	}
	if got := f.mail.to("bob@example.com"); len(got) != 0 {
		t.Fatalf("a refused address got %d emails", len(got))
	}
	contains(t, bob.enter("12345678"), "That code isn't right.")
	if n := f.count(`SELECT count(*) FROM local_users WHERE email = 'bob@example.com'`); n != 0 {
		t.Fatalf("%d users for a refused address", n)
	}

	// Ann is: she signs in, and adds a passkey.
	ann := f.browser("192.0.2.2")
	ann.signIn("ann@example.com")
	a := newAuthenticator(t)
	if status, out := ann.addPasskey(a, "Work laptop"); status != http.StatusOK {
		t.Fatalf("adding a passkey: %d %v", status, out)
	}
	if r := ann.changeAddress("ann@example.org"); r.status != http.StatusUnprocessableEntity || !strings.Contains(r.body, "can&#39;t be used here") {
		t.Fatalf("changing to a refused address: %d\n%s", r.status, r.body)
	}

	// Taken off the list: her session stops working, and neither a passkey nor a code
	// signs her in.
	setAllowed("ann@example.com", false)
	if ann.account() != "" {
		t.Fatal("a refused account's session still works")
	}
	if status, _ := f.browser("192.0.2.3").passkeySignIn(a, a.creds[0]); status != http.StatusForbidden {
		t.Fatalf("passkey sign-in of a refused account: %d", status)
	}
	sent := len(f.mail.to("ann@example.com"))
	other := f.browser("192.0.2.4")
	other.askCode("ann@example.com")
	if len(f.mail.to("ann@example.com")) != sent {
		t.Fatal("a refused account got a code")
	}

	// Back on it: the session she kept works again.
	setAllowed("ann@example.com", true)
	if ann.account() == "" {
		t.Fatal("the session doesn't work once allowed again")
	}
}
