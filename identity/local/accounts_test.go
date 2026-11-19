//go:build integration

package local

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coolapso/c4stack/identity"
)

func (f *fixture) accountOf(userID string) (id, primary string) {
	f.t.Helper()
	err := f.db().QueryRow(`
		SELECT a.id, a.primary_email
		FROM account_identities i
		JOIN accounts a ON a.id = i.account_id
		WHERE i.provider = 'local' AND i.subject = $1`, userID).Scan(&id, &primary)
	if err == sql.ErrNoRows {
		return "", ""
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return id, primary
}

// changeAddress runs the address change from a signed-in browser up to the code.
func (b *browser) changeAddress(to string) response {
	b.t.Helper()
	b.get("/account/settings/email")
	return b.post("/account/settings/email", url.Values{"email": {to}})
}

func TestDeleteAccountInOneTransaction(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	userID := b.signIn("ann@example.com")
	other := f.browser("192.0.2.2")
	other.signIn("ann@example.com")
	accountID, _ := f.accountOf(userID)

	b.get("/account/delete")
	r := b.post("/account/delete", url.Values{"confirmation": {"DELETE"}})
	if r.status != http.StatusSeeOther || r.location != "/account/deleted" {
		t.Fatalf("%d %s %s", r.status, r.location, r.body)
	}
	for table, n := range map[string]int{
		"local_users":    f.count(`SELECT count(*) FROM local_users`),
		"local_sessions": f.count(`SELECT count(*) FROM local_sessions`),
		"accounts":       f.count(`SELECT count(*) FROM accounts WHERE id = $1`, accountID),
	} {
		if n != 0 {
			t.Errorf("%d rows left in %s", n, table)
		}
	}
	if n := f.count(`SELECT count(*) FROM deleted_identities`); n != 1 {
		t.Fatalf("%d tombstones", n)
	}
	if other.account() != "" {
		t.Fatal("another browser's session survived the deletion")
	}
	// The address signs up again as a new user, with a new, empty account.
	again := f.browser("192.0.2.3")
	newID := again.signIn("ann@example.com")
	if newID == userID {
		t.Fatal("the deleted user came back")
	}
	if id, _ := f.accountOf(newID); id == "" || id == accountID {
		t.Fatal("the new user has no new account")
	}
}

func TestDeletionNeedsARecentSignInAndLinksToConfirm(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	userID := b.signIn("ann@example.com")
	f.backdateSessions(userID, 10*time.Minute)
	r := b.get("/account/delete")
	contains(t, r, `href="/login/confirm?next=%2Faccount%2Fdelete"`)
	wantStatus(t, b.post("/account/delete", url.Values{"confirmation": {"DELETE"}}), http.StatusForbidden)

	// Confirming with a code gives a new, recent session.
	r = b.get("/login/confirm?next=/account/delete")
	contains(t, r, "We'll send a code to")
	r = b.post("/login/confirm", url.Values{"next": {"/account/delete"}})
	if r.location != "/login/code" {
		t.Fatalf("%d %s", r.status, r.location)
	}
	contains(t, b.get("/login/code"), "Confirm it's you")
	r = b.enter(f.mail.code(t, "ann@example.com"))
	if r.location != "/account/delete" {
		t.Fatalf("%d %s", r.status, r.location)
	}
	if n := f.count(`SELECT count(*) FROM local_sessions WHERE user_id = $1`, userID); n != 1 {
		t.Fatalf("%d sessions: the confirmation should replace the old one", n)
	}
	r = b.post("/account/delete", url.Values{"confirmation": {"DELETE"}})
	if r.location != "/account/deleted" {
		t.Fatalf("%d %s", r.status, r.body)
	}
}

func TestCodeSentBeforeDeletionFails(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.signIn("ann@example.com")
	attacker := f.browser("198.51.100.1")
	f.clock = f.clock.Add(time.Minute)
	attacker.askCode("ann@example.com")
	code := f.mail.code(t, "ann@example.com")
	b.get("/account/delete")
	b.post("/account/delete", url.Values{"confirmation": {"DELETE"}})
	r := attacker.enter(code)
	contains(t, r, "This code has expired")
	if n := f.count(`SELECT count(*) FROM local_users`); n != 0 {
		t.Fatal("the old code made a user")
	}
}

func TestAddressChange(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	userID := b.signIn("ann@example.com")
	other := f.browser("192.0.2.2")
	other.signIn("ann@example.com")
	b.signIn("ann@example.com") // a recent sign-in on b, after other's
	oldSession := b.cookie("session").Value
	thief := b.clone("198.51.100.1")

	r := b.changeAddress("ann@example.org")
	if r.status != http.StatusSeeOther || r.location != "/account/settings/email/code" {
		t.Fatalf("%d %s %s", r.status, r.location, r.body)
	}
	contains(t, b.get("/account/settings/email/code"), "ann@example.org")
	m := f.mail.last(t, "ann@example.org")
	if m.Subject != "Confirm your new address for Acme" {
		t.Fatalf("subject %q", m.Subject)
	}
	r = b.post("/account/settings/email/code", url.Values{"code": {f.mail.code(t, "ann@example.org")}})
	if r.status != http.StatusSeeOther || !strings.HasPrefix(r.location, "/account/settings?done=address-changed") {
		t.Fatalf("%d %s %s", r.status, r.location, r.body)
	}

	if got := b.account(); !strings.Contains(got, "email=ann@example.org") {
		t.Fatalf("this browser: %q", got)
	}
	if b.cookie("session").Value == oldSession {
		t.Fatal("this browser kept its old token")
	}
	if thief.account() != "" {
		t.Fatal("a copy of the old token still works")
	}
	if other.account() != "" {
		t.Fatal("another session survived the change")
	}
	_, primary := f.accountOf(userID)
	if primary != "ann@example.org" {
		t.Fatalf("the account's address is %q", primary)
	}
	var identityEmail string
	if err := f.db().QueryRow(`SELECT email FROM account_identities WHERE subject = $1`, userID).Scan(&identityEmail); err != nil || identityEmail != "ann@example.org" {
		t.Fatalf("the identity's address is %q (%v)", identityEmail, err)
	}
	notice := f.mail.last(t, "ann@example.com")
	if notice.Subject != "Your Acme address was changed" || !strings.Contains(notice.Text, userID) ||
		!strings.Contains(notice.Text, "Firefox on Linux") || !strings.Contains(notice.Text, "ann@example.org") {
		t.Fatalf("notice: %q\n%s", notice.Subject, notice.Text)
	}
	if n := f.count(`SELECT count(*) FROM local_notices`); n != 0 {
		t.Fatalf("%d notices still queued", n)
	}

	// The old mailbox signs up again as a new user, with a new, empty account.
	newID := f.browser("198.51.100.2").signIn("ann@example.com")
	if newID == userID {
		t.Fatal("the old address signed in to the user")
	}
	if id, _ := f.accountOf(newID); id == "" {
		t.Fatal("no account for the new user")
	}
	if a1, _ := f.accountOf(userID); a1 == "" {
		t.Fatal("the user's account is gone")
	} else if a2, _ := f.accountOf(newID); a1 == a2 {
		t.Fatal("the old address joined the account")
	}
}

func TestAddressChangeNeedsARecentSignInAtBothEnds(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	userID := b.signIn("ann@example.com")
	f.backdateSessions(userID, 10*time.Minute)
	r := b.changeAddress("ann@example.org")
	wantStatus(t, r, http.StatusForbidden)
	contains(t, b.get("/account/settings/email"), "Confirm it's you")

	// Started while recent, finished too late.
	f.backdateSessions(userID, -10*time.Minute)
	wantStatus(t, b.changeAddress("ann@example.org"), http.StatusSeeOther)
	code := f.mail.code(t, "ann@example.org")
	f.backdateSessions(userID, 10*time.Minute)
	r = b.post("/account/settings/email/code", url.Values{"code": {code}})
	wantStatus(t, r, http.StatusForbidden)
	if _, primary := f.accountOf(userID); primary != "ann@example.com" {
		t.Fatal("the address changed")
	}
}

func TestAddressChangeToATakenAddressIsADecoy(t *testing.T) {
	f := newFixture(t)
	f.browser("192.0.2.9").signIn("bob@example.com")
	b := f.browser("192.0.2.1")
	userID := b.signIn("ann@example.com")
	before := len(f.mail.to("bob@example.com"))

	r := b.changeAddress("bob@example.com")
	if r.status != http.StatusSeeOther || r.location != "/account/settings/email/code" {
		t.Fatalf("%d %s", r.status, r.location)
	}
	mails := f.mail.to("bob@example.com")
	if len(mails) != before+1 || mails[len(mails)-1].Subject != "Your address and Acme" {
		t.Fatalf("bob got %d emails", len(mails)-before)
	}
	if codePattern.MatchString(mails[len(mails)-1].Text) {
		t.Fatal("the address-taken email has a code")
	}
	// No code matches the decoy, and its answers are a real attempt's.
	for i, code := range []string{"12345678", "00000000"} {
		r := b.post("/account/settings/email/code", url.Values{"code": {code}})
		contains(t, r, "That code isn't right.")
		if i == 0 {
			contains(t, r, "4 tries left")
		}
	}
	if _, primary := f.accountOf(userID); primary != "ann@example.com" {
		t.Fatal("the address changed")
	}
}

func TestAddressChangeToAnotherAccountsAddress(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	userID := b.signIn("ann@example.com")
	// An account from another provider has the address; no local user does.
	f.exec(`INSERT INTO accounts (primary_email) VALUES ('carol@example.com')`)
	b.changeAddress("carol@example.com")
	r := b.post("/account/settings/email/code", url.Values{"code": {f.mail.code(t, "carol@example.com")}})
	wantStatus(t, r, http.StatusConflict)
	contains(t, r, "This address belongs to another account.")
	var address string
	f.db().QueryRow(`SELECT email FROM local_users WHERE id = $1`, userID).Scan(&address)
	if address != "ann@example.com" {
		t.Fatal("the user's address changed without the account's")
	}
}

func TestCodeSentBeforeAnAddressChangeFails(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.signIn("ann@example.com")
	old := f.browser("198.51.100.1")
	f.clock = f.clock.Add(time.Minute)
	old.askCode("ann@example.com")
	code := f.mail.code(t, "ann@example.com")
	b.changeAddress("ann@example.org")
	b.post("/account/settings/email/code", url.Values{"code": {f.mail.code(t, "ann@example.org")}})
	r := old.enter(code)
	contains(t, r, "This code has expired")
	if n := f.count(`SELECT count(*) FROM local_sessions`); n != 1 {
		t.Fatalf("%d sessions", n)
	}
}

func TestSetEmailCommand(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	userID := b.signIn("ann@example.com")
	f.exec(`INSERT INTO local_passkeys (user_id, credential_id, public_key, sign_count, backup_eligible, backed_up, name)
		VALUES ($1, 'x', 'y', 0, false, false, 'Attacker key')`, userID)
	f.browser("198.51.100.1").askCode("ann@example.com")

	if err := f.l.setEmailCommand(t.Context(), []string{"ann@example.com", "Ann@Example.org"}); err != nil {
		t.Fatal(err)
	}
	for table, n := range map[string]int{
		"local_sessions": f.count(`SELECT count(*) FROM local_sessions`),
		"local_passkeys": f.count(`SELECT count(*) FROM local_passkeys`),
		"local_codes":    f.count(`SELECT count(*) FROM local_codes`),
		"local_notices":  f.count(`SELECT count(*) FROM local_notices`),
	} {
		if n != 0 {
			t.Errorf("%d rows left in %s", n, table)
		}
	}
	if _, primary := f.accountOf(userID); primary != "ann@example.org" {
		t.Fatalf("the account's address is %q", primary)
	}
	if m := f.mail.last(t, "ann@example.com"); m.Subject != "Your Acme address was changed by support" || !strings.Contains(m.Text, userID) {
		t.Fatalf("old address: %q", m.Subject)
	}
	if m := f.mail.last(t, "ann@example.org"); m.Subject != "Your Acme account now uses this address" {
		t.Fatalf("new address: %q", m.Subject)
	}
	if id := f.browser("192.0.2.5").signIn("ann@example.org"); id != userID {
		t.Fatal("the new address isn't the user's")
	}
	// Taken addresses are refused, and nothing changes.
	f.browser("192.0.2.6").signIn("bob@example.com")
	if err := f.l.setEmailCommand(t.Context(), []string{userID, "bob@example.com"}); err == nil {
		t.Fatal("set-email took another user's address")
	}
	if err := f.l.setEmailCommand(t.Context(), []string{"nobody@example.com", "x@example.com"}); err == nil {
		t.Fatal("set-email found a user who doesn't exist")
	}
}

func TestSignOutCommand(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	userID := b.signIn("ann@example.com")
	if err := f.l.signOutCommand(t.Context(), []string{userID}); err != nil {
		t.Fatal(err)
	}
	if b.account() != "" {
		t.Fatal("still signed in")
	}
	// The browser's device cookie no longer counts as a known browser.
	var since time.Time
	f.db().QueryRow(`SELECT devices_since FROM local_users WHERE id = $1`, userID).Scan(&since)
	d, ok := deviceOf(t, f, b)
	if !ok || !d.IssuedAt.Before(since) {
		t.Fatalf("device cookie issued %v, devices since %v", d.IssuedAt, since)
	}
}

// deviceOf opens a browser's device cookie through the test route.
func deviceOf(t *testing.T, f *fixture, b *browser) (device, bool) {
	t.Helper()
	r := b.get("/test/device")
	if r.status != http.StatusOK {
		return device{}, false
	}
	var d device
	if err := json.Unmarshal([]byte(r.body), &d); err != nil {
		t.Fatal(err)
	}
	return d, true
}

func TestDevices(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	userID := b.signIn("ann@example.com")
	phone := f.browser("192.0.2.2")
	phone.ua = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1"
	phone.signIn("ann@example.com")
	laptop := f.browser("192.0.2.3")
	laptop.ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
	laptop.signIn("ann@example.com")

	page := b.get("/account/settings")
	for _, want := range []string{"Firefox on Linux", "Safari on iOS", "Chrome on Windows", "This device", "Sign out all others"} {
		contains(t, page, want)
	}
	var phoneSession string
	f.db().QueryRow(`SELECT id FROM local_sessions WHERE user_id = $1 AND device = 'Safari on iOS'`, userID).Scan(&phoneSession)
	r := b.post("/account/settings/devices/"+phoneSession+"/sign-out", nil)
	if r.status != http.StatusSeeOther {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if phone.account() != "" || laptop.account() == "" || b.account() == "" {
		t.Fatal("signing out one device signed out the wrong ones")
	}
	r = b.post("/account/settings/devices/sign-out-others", nil)
	if r.status != http.StatusSeeOther {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if laptop.account() != "" || b.account() == "" {
		t.Fatal("signing out the others signed out the wrong ones")
	}
	// The current session can't be signed out from the list.
	var mine string
	f.db().QueryRow(`SELECT id FROM local_sessions WHERE user_id = $1`, userID).Scan(&mine)
	b.post("/account/settings/devices/"+mine+"/sign-out", nil)
	if b.account() == "" {
		t.Fatal("the list signed out the current session")
	}
}

func TestSessionsPerUserAreBounded(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	userID := b.signIn("ann@example.com")
	// Sign-ins that dropped their cookies, each a little older than the last.
	f.exec(`INSERT INTO local_sessions (user_id, token_hash, method, device, last_seen_at)
		SELECT $1, sha256(i::text::bytea), 'passkey', 'Old', now() - make_interval(mins => i)
		FROM generate_series(1, 60) i`, userID)
	f.clock = f.clock.Add(time.Minute)
	c := f.browser("192.0.2.2")
	c.signIn("ann@example.com")
	if n := f.count(`SELECT count(*) FROM local_sessions WHERE user_id = $1`, userID); n != maxSessions {
		t.Fatalf("%d sessions", n)
	}
	// The new session and the most recently used stay; the oldest went.
	if b.account() == "" || c.account() == "" {
		t.Fatal("a recent session was signed out")
	}
	if n := f.count(`SELECT count(*) FROM local_sessions WHERE device = 'Old' AND last_seen_at < now() - interval '48 minutes 30 seconds'`); n != 0 {
		t.Fatalf("%d of the oldest sessions stayed", n)
	}
}

func TestLogout(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.signIn("ann@example.com")
	thief := b.clone("198.51.100.1")
	b.post("/logout", nil)
	if b.account() != "" || thief.account() != "" {
		t.Fatal("the session survived signing out")
	}
}

func TestSessionLifetimes(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	userID := b.signIn("ann@example.com")
	f.exec(`UPDATE local_sessions SET last_seen_at = now() - interval '2 hours'`)
	if b.account() == "" {
		t.Fatal("signed out after 2 idle hours")
	}
	var seen time.Time
	f.db().QueryRow(`SELECT last_seen_at FROM local_sessions`).Scan(&seen)
	if time.Since(seen) > time.Minute {
		t.Fatal("the session's last use wasn't recorded")
	}
	f.exec(`UPDATE local_sessions SET last_seen_at = now() - interval '31 days'`)
	if b.account() != "" {
		t.Fatal("signed in after 31 idle days")
	}
	b.signIn("ann@example.com")
	f.exec(`UPDATE local_sessions SET created_at = now() - interval '91 days' WHERE user_id = $1`, userID)
	if b.account() != "" {
		t.Fatal("signed in after 91 days")
	}
	if err := f.l.cleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM local_sessions`); n != 0 {
		t.Fatalf("cleanup left %d sessions", n)
	}
}

func TestPlantedSessionIsReplaced(t *testing.T) {
	f := newFixture(t)
	attacker := f.browser("198.51.100.1")
	attacker.signIn("mallory@example.com")
	victim := attacker.clone("192.0.2.1") // a cookie planted in the victim's browser
	victim.signIn("ann@example.com")
	if got := attacker.account(); got != "" {
		t.Fatalf("the planted session survived the victim's sign-in: %q", got)
	}
}

func TestNoticeQueue(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.signIn("ann@example.com")
	f.mail.failWith(errSend)
	b.changeAddress("ann@example.org") // the code can't be sent
	f.mail.failWith(nil)
	b.changeAddress("ann@example.org")
	code := f.mail.code(t, "ann@example.org")
	f.mail.failWith(errSend)
	r := b.post("/account/settings/email/code", url.Values{"code": {code}})
	if r.status != http.StatusSeeOther {
		t.Fatalf("a failing notice stopped the change: %d %s", r.status, r.body)
	}
	if n := f.count(`SELECT count(*) FROM local_notices`); n != 1 {
		t.Fatalf("%d notices queued", n)
	}
	f.mail.failWith(nil)
	// The job waits for the next attempt.
	f.l.sendNotices(t.Context())
	if n := f.count(`SELECT count(*) FROM local_notices`); n != 1 {
		t.Fatal("the job retried before the next attempt")
	}
	f.exec(`UPDATE local_notices SET next_attempt_at = now()`)
	if err := f.l.sendNotices(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM local_notices`); n != 0 {
		t.Fatal("the job didn't send the notice")
	}
	if m := f.mail.last(t, "ann@example.com"); m.Subject != "Your Acme address was changed" {
		t.Fatalf("subject %q", m.Subject)
	}

	// A notice that fails for 3 days is dropped.
	f.exec(`INSERT INTO local_notices (user_id, kind, recipient, data, created_at)
		VALUES (gen_random_uuid(), 'passkey_added', 'x@example.com', '{}', now() - interval '4 days')`)
	f.l.sendNotices(t.Context())
	if n := f.count(`SELECT count(*) FROM local_notices`); n != 0 {
		t.Fatal("an old notice wasn't dropped")
	}
}

func TestRefusedSignInRollsBack(t *testing.T) {
	f := newFixture(t)
	// An account whose deletion another provider started holds the address.
	f.exec(`INSERT INTO accounts (primary_email, deletion_started_at) VALUES ('ann@example.com', now())`)
	f.exec(`INSERT INTO account_identities (account_id, provider, subject, email, email_verified)
		SELECT id, 'hanko', 'h1', primary_email, true FROM accounts`)
	b := f.browser("192.0.2.1")
	b.askCode("ann@example.com")
	r := b.enter(f.mail.code(t, "ann@example.com"))
	wantStatus(t, r, http.StatusConflict)
	contains(t, r, "This account can't be signed in to right now.")
	if n := f.count(`SELECT count(*) FROM local_users`); n != 0 {
		t.Fatal("the refused sign-up left a user")
	}
	if n := f.count(`SELECT count(*) FROM local_codes`); n != 1 {
		t.Fatal("the refused sign-up used the code")
	}
}

// holdUser locks a user's row in a transaction the test holds open, so a request that
// needs the user waits for it.
func holdUser(t *testing.T, f *fixture, userID string) *sql.Tx {
	t.Helper()
	tx, err := f.db().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	if _, err := tx.Exec(`SELECT 1 FROM local_users WHERE id = $1 FOR UPDATE`, userID); err != nil {
		t.Fatal(err)
	}
	return tx
}

// waitBlocked waits until a request is waiting for a lock.
func waitBlocked(t *testing.T, f *fixture) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.count(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the request never waited for the lock")
}

func TestCodeSignInRacingADeletion(t *testing.T) {
	f := newFixture(t)
	owner := f.browser("192.0.2.1")
	userID := owner.signIn("ann@example.com")
	b := f.browser("198.51.100.1")
	f.clock = f.clock.Add(time.Minute)
	b.askCode("ann@example.com")
	code := f.mail.code(t, "ann@example.com")

	// The deletion locks the user first; the sign-in waits, then finds the user gone.
	tx := holdUser(t, f, userID)
	done := make(chan response)
	go func() { done <- b.enter(code) }()
	waitBlocked(t, f)
	if err := f.l.DeleteTx(context.Background(), tx, userID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// The sign-in waited for the user's lock, then found the user gone: a clean
	// answer, not a failure.
	r := <-done
	wantStatus(t, r, http.StatusUnprocessableEntity)
	contains(t, r, "This code is no longer valid. Send a new one.")
	if n := f.count(`SELECT count(*) FROM local_sessions`); n != 0 {
		t.Fatalf("%d sessions", n)
	}
}

func TestCodeSignInRacingARecovery(t *testing.T) {
	f := newFixture(t)
	owner := f.browser("192.0.2.1")
	userID := owner.signIn("ann@example.com")
	attacker := f.browser("198.51.100.1")
	f.clock = f.clock.Add(time.Minute)
	attacker.askCode("ann@example.com")
	code := f.mail.code(t, "ann@example.com")

	tx := holdUser(t, f, userID)
	done := make(chan response)
	go func() { done <- attacker.enter(code) }()
	waitBlocked(t, f)
	// What set-email does inside its transaction.
	if err := f.l.moveAddress(context.Background(), tx, userID, "ann@example.org"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM local_sessions WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// The recovery deleted the user's codes, so the code is gone.
	r := <-done
	wantStatus(t, r, http.StatusUnprocessableEntity)
	contains(t, r, "This code has expired. Send a new one.")
	if n := f.count(`SELECT count(*) FROM local_sessions`); n != 0 {
		t.Fatalf("%d sessions", n)
	}
}

func TestDeletionRacingARecovery(t *testing.T) {
	f := newFixture(t)
	owner := f.browser("192.0.2.1")
	userID := owner.signIn("ann@example.com")
	attacker := f.browser("198.51.100.1")
	attacker.signIn("ann@example.com")
	attacker.get("/account/delete")

	// Support's recovery holds the user's lock and ends every session; the attacker's
	// deletion passed its middleware before, and waits.
	tx := holdUser(t, f, userID)
	done := make(chan response)
	go func() { done <- attacker.post("/account/delete", url.Values{"confirmation": {"DELETE"}}) }()
	waitBlocked(t, f)
	if err := f.l.moveAddress(context.Background(), tx, userID, "ann@example.org"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM local_sessions WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r := <-done
	wantStatus(t, r, http.StatusBadGateway)
	if n := f.count(`SELECT count(*) FROM local_users`) + f.count(`SELECT count(*) FROM accounts`); n != 2 {
		t.Fatal("the deletion went ahead after the recovery")
	}
}

func TestDeviceSignOutRacingARecovery(t *testing.T) {
	f := newFixture(t)
	owner := f.browser("192.0.2.1")
	userID := owner.signIn("ann@example.com")
	attacker := f.browser("198.51.100.1")
	attacker.signIn("ann@example.com")
	var ownerSession string
	f.db().QueryRow(`SELECT id FROM local_sessions WHERE device = 'Firefox on Linux' ORDER BY created_at LIMIT 1`).Scan(&ownerSession)
	attacker.get("/account/settings")

	// The owner's change holds the lock and ends the attacker's session.
	tx := holdUser(t, f, userID)
	done := make(chan response)
	go func() { done <- attacker.post("/account/settings/devices/"+ownerSession+"/sign-out", nil) }()
	waitBlocked(t, f)
	if _, err := tx.Exec(`DELETE FROM local_sessions WHERE user_id = $1 AND id <> $2`, userID, ownerSession); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	<-done
	if owner.account() == "" {
		t.Fatal("a sign-out from an ended session signed the owner out")
	}
}

func TestAccountsHooksRunInTheProvidersTransaction(t *testing.T) {
	f := newFixture(t)
	var calls []string
	hooks := f.l.hooks
	f.l.UseAccounts(identity.AccountHooks{
		SignedIn: func(ctx context.Context, tx *sql.Tx, id identity.Identity) error {
			var exists bool
			// The user the provider created in its transaction is visible here.
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM local_users WHERE id = $1)`, id.Subject).Scan(&exists); err != nil || !exists {
				t.Errorf("the hook doesn't see the provider's user: %v", err)
			}
			calls = append(calls, "signed-in "+id.Method)
			return hooks.SignedIn(ctx, tx, id)
		},
		AddressChanged: hooks.AddressChanged,
	})
	f.browser("192.0.2.1").signIn("ann@example.com")
	if len(calls) != 1 || calls[0] != "signed-in code" {
		t.Fatalf("calls %v", calls)
	}
}
