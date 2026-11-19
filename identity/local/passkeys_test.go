//go:build integration

package local

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// addPasskey adds a passkey from a signed-in, recent browser.
func (b *browser) addPasskey(a *authenticator, name string) (int, map[string]any) {
	b.t.Helper()
	b.get("/account/settings")
	status, out, options := b.postJSON("/account/settings/passkeys/options", map[string]any{})
	if status != http.StatusOK {
		return status, out
	}
	status, out, _ = b.postJSON("/account/settings/passkeys", map[string]any{"name": name, "credential": a.create(options)})
	return status, out
}

// passkeySignIn signs in with a credential, and returns the finish's answer.
func (b *browser) passkeySignIn(a *authenticator, cred *softCredential) (int, map[string]any) {
	b.t.Helper()
	b.get("/login?next=/account")
	status, out, options := b.postJSON("/login/passkey/options", map[string]any{"purpose": "login", "next": "/account"})
	if status != http.StatusOK {
		b.t.Fatalf("options: %d %v", status, out)
	}
	status, out, _ = b.postJSON("/login/passkey", a.get(options, cred))
	return status, out
}

func passkeyUserFixture(t *testing.T) (*fixture, *browser, string, *authenticator) {
	t.Helper()
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	userID := b.signIn("ann@example.com")
	a := newAuthenticator(t)
	if status, out := b.addPasskey(a, "Work laptop"); status != http.StatusOK {
		t.Fatalf("adding a passkey: %d %v", status, out)
	}
	return f, b, userID, a
}

func TestAddPasskeyAndSignInWithIt(t *testing.T) {
	f, b, userID, a := passkeyUserFixture(t)
	if n := f.count(`SELECT count(*) FROM local_passkeys WHERE user_id = $1 AND name = 'Work laptop'`, userID); n != 1 {
		t.Fatalf("%d passkeys", n)
	}
	m := f.mail.last(t, "ann@example.com")
	if m.Subject != "A passkey was added to your Acme account" || !strings.Contains(m.Text, `"Work laptop"`) {
		t.Fatalf("notice: %q\n%s", m.Subject, m.Text)
	}
	page := b.get("/account/settings")
	contains(t, page, "Work laptop")

	c := f.browser("192.0.2.2")
	status, out := c.passkeySignIn(a, a.creds[0])
	if status != http.StatusOK || out["redirect"] != "/account" {
		t.Fatalf("%d %v", status, out)
	}
	if got := c.account(); !strings.Contains(got, "method=passkey") {
		t.Fatalf("account: %q", got)
	}
	var used *time.Time
	f.db().QueryRow(`SELECT last_used_at FROM local_passkeys`).Scan(&used)
	if used == nil {
		t.Fatal("the passkey's last use wasn't recorded")
	}
}

func TestPasskeysWithoutTransports(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.signIn("ann@example.com")
	for i, transports := range [][]string{nil, {}} {
		a := newAuthenticator(t)
		a.transports = transports
		if status, out := b.addPasskey(a, "Key "+string(rune('A'+i))); status != http.StatusOK {
			t.Fatalf("transports %v: %d %v", transports, status, out)
		}
		if status, out := f.browser("192.0.2.2").passkeySignIn(a, a.creds[0]); status != http.StatusOK {
			t.Fatalf("signing in: %d %v", status, out)
		}
	}
}

func TestAddingAPasskeyExcludesExistingOnes(t *testing.T) {
	_, b, _, a := passkeyUserFixture(t)
	_, _, options := b.postJSON("/account/settings/passkeys/options", map[string]any{})
	if !strings.Contains(string(options), b64.EncodeToString(a.creds[0].id)) {
		t.Fatal("registration doesn't exclude the user's passkey")
	}
	for _, want := range []string{`"residentKey":"required"`, `"userVerification":"required"`, `"attestation":"none"`} {
		if !strings.Contains(string(options), want) {
			t.Fatalf("registration options lack %s: %s", want, options)
		}
	}
}

func TestAddingAPasskeyNeedsARecentSignInAtBothEnds(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	userID := b.signIn("ann@example.com")
	a := newAuthenticator(t)
	f.backdateSessions(userID, 10*time.Minute)
	if status, _ := b.addPasskey(a, "Laptop"); status != http.StatusForbidden {
		t.Fatalf("status %d", status)
	}
	contains(t, b.get("/account/settings"), "Confirm it's you")

	f.backdateSessions(userID, -10*time.Minute)
	_, _, options := b.postJSON("/account/settings/passkeys/options", map[string]any{})
	f.backdateSessions(userID, 10*time.Minute)
	status, _, _ := b.postJSON("/account/settings/passkeys", map[string]any{"name": "Laptop", "credential": a.create(options)})
	if status == http.StatusOK {
		t.Fatal("a registration finished after the recent window")
	}
	if n := f.count(`SELECT count(*) FROM local_passkeys`); n != 0 {
		t.Fatal("a passkey was added")
	}
}

func TestAddingFinishesOnlyFromTheSessionThatStarted(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.signIn("ann@example.com")
	other := f.browser("192.0.2.2")
	other.signIn("ann@example.com")
	a := newAuthenticator(t)
	_, _, options := b.postJSON("/account/settings/passkeys/options", map[string]any{})
	// The other session gets the ceremony's cookie.
	u, _ := url.Parse(f.srv.URL)
	other.client.Jar.SetCookies(u, []*http.Cookie{b.cookie("passkey")})
	status, _, _ := other.postJSON("/account/settings/passkeys", map[string]any{"name": "Laptop", "credential": a.create(options)})
	if status != http.StatusUnauthorized {
		t.Fatalf("status %d", status)
	}
	if n := f.count(`SELECT count(*) FROM local_passkeys`); n != 0 {
		t.Fatal("a passkey was added")
	}
}

func TestPasskeyNames(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.signIn("ann@example.com")
	for _, name := range []string{"", "   ", strings.Repeat("x", 61), "bad\x00name"} {
		if status, _ := b.addPasskey(newAuthenticator(t), name); status != http.StatusUnprocessableEntity {
			t.Fatalf("name %q: status %d", name, status)
		}
	}
}

func TestReplayedAssertion(t *testing.T) {
	f, _, _, a := passkeyUserFixture(t)
	c := f.browser("192.0.2.2")
	c.get("/login")
	_, _, options := c.postJSON("/login/passkey/options", map[string]any{"purpose": "login", "next": "/account"})
	assertion := a.get(options, a.creds[0])
	copied := c.clone("198.51.100.1")
	if status, out, _ := c.postJSON("/login/passkey", assertion); status != http.StatusOK {
		t.Fatalf("%d %v", status, out)
	}
	status, _, _ := copied.postJSON("/login/passkey", assertion)
	if status == http.StatusOK || copied.account() != "" {
		t.Fatal("a replayed assertion signed in")
	}
}

func TestCounterRule(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.signIn("ann@example.com")
	a := newAuthenticator(t)
	a.counter = true
	if status, out := b.addPasskey(a, "Key"); status != http.StatusOK {
		t.Fatalf("%d %v", status, out)
	}
	for range 2 {
		if status, out := f.browser("192.0.2.2").passkeySignIn(a, a.creds[0]); status != http.StatusOK {
			t.Fatalf("%d %v", status, out)
		}
	}
	// A copy of the key reports a counter that went back.
	a.creds[0].count = 0
	status, out := f.browser("192.0.2.3").passkeySignIn(a, a.creds[0])
	if status != http.StatusUnauthorized || !strings.Contains(out["error"].(string), "Sign in with a code") {
		t.Fatalf("%d %v", status, out)
	}
	// A passkey that keeps no counter is accepted with zero every time.
	s := newAuthenticator(t)
	s.backup = true
	if status, out := b.addPasskey(s, "Phone"); status != http.StatusOK {
		t.Fatalf("%d %v", status, out)
	}
	for range 2 {
		if status, out := f.browser("192.0.2.4").passkeySignIn(s, s.creds[0]); status != http.StatusOK {
			t.Fatalf("%d %v", status, out)
		}
	}
	contains(t, b.get("/account/settings"), "synced")
}

func TestCeremonyForAnotherPurpose(t *testing.T) {
	f, _, _, a := passkeyUserFixture(t)
	c := f.browser("192.0.2.2")
	c.get("/login")
	_, _, options := c.postJSON("/login/passkey/options", map[string]any{"purpose": "login", "next": "/account"})
	status, out, _ := c.postJSON("/login/passkey?purpose=confirm", a.get(options, a.creds[0]))
	if status != http.StatusConflict || out["retry"] != true {
		t.Fatalf("%d %v", status, out)
	}
	if c.account() != "" {
		t.Fatal("signed in")
	}
}

func TestTwoTabs(t *testing.T) {
	f, _, _, a := passkeyUserFixture(t)
	c := f.browser("192.0.2.2")
	c.get("/login")
	_, _, first := c.postJSON("/login/passkey/options", map[string]any{"purpose": "login", "next": "/account"})
	c.postJSON("/login/passkey/options", map[string]any{"purpose": "login", "next": "/account"}) // the second tab
	status, out, _ := c.postJSON("/login/passkey", a.get(first, a.creds[0]))
	if status != http.StatusConflict || out["retry"] != true {
		t.Fatalf("%d %v", status, out)
	}
}

func TestRemovePasskey(t *testing.T) {
	f, b, userID, a := passkeyUserFixture(t)
	var passkeyID string
	f.db().QueryRow(`SELECT id FROM local_passkeys WHERE user_id = $1`, userID).Scan(&passkeyID)

	f.backdateSessions(userID, 10*time.Minute)
	wantStatus(t, b.post("/account/settings/passkeys/"+passkeyID+"/remove", nil), http.StatusForbidden)
	f.backdateSessions(userID, -10*time.Minute)
	r := b.post("/account/settings/passkeys/"+passkeyID+"/remove", nil)
	if r.status != http.StatusSeeOther || !strings.Contains(r.location, "done=passkey-removed") {
		t.Fatalf("%d %s", r.status, r.location)
	}
	if m := f.mail.last(t, "ann@example.com"); m.Subject != "A passkey was removed from your Acme account" {
		t.Fatalf("subject %q", m.Subject)
	}
	status, _ := f.browser("192.0.2.2").passkeySignIn(a, a.creds[0])
	if status != http.StatusUnauthorized {
		t.Fatalf("a removed passkey signed in: %d", status)
	}
}

func TestConfirmWithAPasskey(t *testing.T) {
	f, b, userID, a := passkeyUserFixture(t)
	f.backdateSessions(userID, 10*time.Minute)
	contains(t, b.get("/login/confirm?next=/account/delete"), "Use your passkey")
	_, _, options := b.postJSON("/login/passkey/options", map[string]any{"purpose": "confirm", "next": "/account/delete"})
	if !strings.Contains(string(options), b64.EncodeToString(a.creds[0].id)) {
		t.Fatal("confirming doesn't name the user's passkeys")
	}
	status, out, _ := b.postJSON("/login/passkey?purpose=confirm", a.get(options, a.creds[0]))
	if status != http.StatusOK || out["redirect"] != "/account/delete" {
		t.Fatalf("%d %v", status, out)
	}
	var recent bool
	f.db().QueryRow(`SELECT bool_and(created_at > now() - interval '1 minute') FROM local_sessions WHERE user_id = $1`, userID).Scan(&recent)
	if !recent {
		t.Fatal("the confirmation didn't make a recent session")
	}
}

func TestConfirmingIsForTheSignedInUserOnly(t *testing.T) {
	f, b, _, a := passkeyUserFixture(t)
	// Another user's browser asks to confirm, then answers with Ann's passkey.
	other := f.browser("192.0.2.9")
	other.signIn("bob@example.com")
	_, _, options := other.postJSON("/login/passkey/options", map[string]any{"purpose": "confirm", "next": "/account"})
	status, _, _ := other.postJSON("/login/passkey?purpose=confirm", a.get(options, a.creds[0]))
	if status == http.StatusOK {
		t.Fatal("another user's passkey confirmed the session")
	}
	_ = b
}

func TestPasskeySignInRacingADeletion(t *testing.T) {
	f, _, userID, a := passkeyUserFixture(t)
	c := f.browser("192.0.2.2")
	c.get("/login")
	_, _, options := c.postJSON("/login/passkey/options", map[string]any{"purpose": "login", "next": "/account"})
	assertion := a.get(options, a.creds[0])

	tx := holdUser(t, f, userID)
	done := make(chan int)
	go func() {
		status, _, _ := c.postJSON("/login/passkey", assertion)
		done <- status
	}()
	waitBlocked(t, f)
	if err := f.l.DeleteTx(context.Background(), tx, userID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if status := <-done; status != http.StatusUnauthorized {
		t.Fatalf("status %d", status)
	}
	if n := f.count(`SELECT count(*) FROM local_sessions`); n != 0 {
		t.Fatalf("%d sessions", n)
	}
}

func TestPasskeyRegistrationRacingARecovery(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	userID := b.signIn("ann@example.com")
	a := newAuthenticator(t)
	b.get("/account/settings")
	_, _, options := b.postJSON("/account/settings/passkeys/options", map[string]any{})
	credential := a.create(options)

	tx := holdUser(t, f, userID)
	done := make(chan int)
	go func() {
		status, _, _ := b.postJSON("/account/settings/passkeys", map[string]any{"name": "Attacker", "credential": credential})
		done <- status
	}()
	waitBlocked(t, f)
	if err := f.l.moveAddress(context.Background(), tx, userID, "ann@example.org"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DELETE FROM local_sessions WHERE user_id = $1`, `DELETE FROM local_passkeys WHERE user_id = $1`} {
		if _, err := tx.Exec(q, userID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if status := <-done; status == http.StatusOK {
		t.Fatal("a registration that raced the recovery survived it")
	}
	if n := f.count(`SELECT count(*) FROM local_passkeys`); n != 0 {
		t.Fatal("the passkey was added")
	}
}

func TestPasskeysSurviveAnAddressChange(t *testing.T) {
	f, b, _, a := passkeyUserFixture(t)
	b.changeAddress("ann@example.org")
	b.post("/account/settings/email/code", url.Values{"code": {f.mail.code(t, "ann@example.org")}})
	status, out := f.browser("192.0.2.2").passkeySignIn(a, a.creds[0])
	if status != http.StatusOK {
		t.Fatalf("%d %v", status, out)
	}
}
