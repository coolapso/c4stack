//go:build integration

package local

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSignUpThenSignIn(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	userID := b.signIn("Ann@Example.com ")

	got := b.account()
	if !strings.Contains(got, "email=ann@example.com") || !strings.Contains(got, "method=code") {
		t.Fatalf("account page says %q", got)
	}
	if m := f.mail.last(t, "ann@example.com"); m.Subject != "Your Acme sign-up code" {
		t.Fatalf("first email's subject is %q", m.Subject)
	}
	var primary string
	if err := f.db().QueryRow(`
		SELECT a.primary_email
		FROM account_identities i
		JOIN accounts a ON a.id = i.account_id
		WHERE i.provider = 'local' AND i.subject = $1`, userID).Scan(&primary); err != nil {
		t.Fatalf("the sign-up made no account: %v", err)
	}
	if primary != "ann@example.com" {
		t.Fatalf("the account's address is %q", primary)
	}

	// Signing in again from another browser finds the same user and account.
	b2 := f.browser("192.0.2.2")
	if id := b2.signIn("ann@example.com"); id != userID {
		t.Fatalf("a second sign-in made another user")
	}
	if m := f.mail.last(t, "ann@example.com"); m.Subject != "Your Acme sign-in code" {
		t.Fatalf("second email's subject is %q", m.Subject)
	}
	if n := f.count(`SELECT count(*) FROM accounts`); n != 1 {
		t.Fatalf("%d accounts", n)
	}
	if n := f.count(`SELECT count(*) FROM local_sessions WHERE user_id = $1`, userID); n != 2 {
		t.Fatalf("%d sessions", n)
	}
	if b.account() == "" {
		t.Fatal("the first browser was signed out")
	}
}

func TestCodeEmailNamesTheSiteAndNotTheCodeInTheSubject(t *testing.T) {
	f := newFixture(t)
	f.browser("192.0.2.1").askCode("ann@example.com")
	m := f.mail.last(t, "ann@example.com")
	code := f.mail.code(t, "ann@example.com")
	if strings.Contains(m.Subject, code[:4]) || !strings.Contains(m.Text, "localhost:8080") {
		t.Fatalf("email: %q\n%s", m.Subject, m.Text)
	}
	if strings.Contains(strings.ToLower(m.Subject+m.Text), "c4stack") {
		t.Fatal("the email names C4Stack")
	}
}

func TestSameAnswerWhetherOrNotAnAccountExists(t *testing.T) {
	f := newFixture(t)
	f.browser("192.0.2.1").signIn("ann@example.com")

	known := f.browser("192.0.2.2").askCode("ann@example.com")
	unknown := f.browser("192.0.2.3").askCode("bob@example.com")
	if known.status != unknown.status || known.location != unknown.location {
		t.Fatalf("known: %d %s, unknown: %d %s", known.status, known.location, unknown.status, unknown.location)
	}
	b := f.browser("192.0.2.4")
	b.askCode("ann@example.com")
	page1 := b.get("/login/code")
	c := f.browser("192.0.2.5")
	c.askCode("bob@example.com")
	page2 := c.get("/login/code")
	norm := func(s string) string {
		s = strings.ReplaceAll(s, "ann@example.com", "X")
		s = strings.ReplaceAll(s, "bob@example.com", "X")
		s = csrfPattern.ReplaceAllString(s, "")
		return csrfMeta.ReplaceAllString(s, "")
	}
	if norm(page1.body) != norm(page2.body) {
		t.Fatal("the code page differs between an address with an account and one without")
	}
}

var csrfMeta = regexp.MustCompile(`<meta name="csrf-token" content="[^"]+">`)

func TestInvalidAddress(t *testing.T) {
	f := newFixture(t)
	for _, bad := range []string{"", "ann", "Ann <ann@example.com>", "ann@", "@example.com", "a b@example.com", strings.Repeat("a", 250) + "@example.com"} {
		r := f.browser("192.0.2.1").askCode(bad)
		wantStatus(t, r, http.StatusUnprocessableEntity)
	}
	if len(f.mail.mails) != 0 {
		t.Fatal("an invalid address got an email")
	}
}

func TestWrongCodes(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.askCode("ann@example.com")
	code := f.mail.code(t, "ann@example.com")
	wrong := "00000000"
	if code == wrong {
		wrong = "11111111"
	}
	r := b.enter(wrong)
	wantStatus(t, r, http.StatusUnprocessableEntity)
	contains(t, r, "That code isn't right. 4 tries left.")
	r = b.enter("12")
	contains(t, r, "Enter the 8-digit code")
	for range 3 {
		b.enter(wrong)
	}
	contains(t, b.enter(wrong), "Too many wrong codes. Send a new one.")
	// The fifth wrong entry ended the code, so the right one no longer works.
	contains(t, b.enter(code), "This code has expired")
	if n := f.count(`SELECT count(*) FROM local_users`); n != 0 {
		t.Fatal("a user was created")
	}
}

func TestCodeWithSpaceAndOnlyTheNewestWorks(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.askCode("ann@example.com")
	first := f.mail.code(t, "ann@example.com")
	f.clock = f.clock.Add(31 * time.Second)
	f.exec(`UPDATE local_codes SET created_at = created_at - interval '31 seconds'`)
	r := b.post("/login/code/resend", nil)
	wantStatus(t, r, http.StatusOK)
	contains(t, r, "We sent a new code")
	second := f.mail.code(t, "ann@example.com")
	if first != second {
		contains(t, b.enter(first), "That code isn't right")
	}
	r = b.enter(second[:4] + " " + second[4:])
	if r.status != http.StatusSeeOther || r.location != "/account" {
		t.Fatalf("%d %s", r.status, r.body)
	}
}

func TestReplayedAttemptCookie(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.askCode("ann@example.com")
	old := b.clone("192.0.2.1") // a copy of the attempt's first cookie
	f.clock = f.clock.Add(31 * time.Second)
	f.exec(`UPDATE local_codes SET created_at = created_at - interval '31 seconds'`)
	wantStatus(t, b.post("/login/code/resend", nil), http.StatusOK)
	second := f.mail.code(t, "ann@example.com")

	// The replayed cookie's own clock says 31 seconds, but the attempt's newest code
	// is new.
	r := old.post("/login/code/resend", nil)
	wantStatus(t, r, http.StatusTooManyRequests)
	contains(t, r, "You can ask for a new code in")

	// Once it may, the replayed cookie's new code ends the attempt's others.
	f.exec(`UPDATE local_codes SET created_at = created_at - interval '31 seconds'`)
	wantStatus(t, old.post("/login/code/resend", nil), http.StatusOK)
	third := f.mail.code(t, "ann@example.com")
	if second != third {
		contains(t, b.enter(second), "This code has expired")
	}
	if n := f.count(`SELECT count(*) FROM local_codes`); n != 1 {
		t.Fatalf("%d codes for one attempt", n)
	}
	if r := old.enter(third); r.status != http.StatusSeeOther {
		t.Fatalf("%d %s", r.status, r.body)
	}
}

func TestResendWaits(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.askCode("ann@example.com")
	r := b.post("/login/code/resend", nil)
	wantStatus(t, r, http.StatusTooManyRequests)
	contains(t, r, "You can ask for a new code in")
	if n := len(f.mail.to("ann@example.com")); n != 1 {
		t.Fatalf("%d emails", n)
	}
}

func TestAnotherBrowsersAttemptKeepsItsCode(t *testing.T) {
	f := newFixture(t)
	owner := f.browser("192.0.2.1")
	owner.askCode("ann@example.com")
	code := f.mail.code(t, "ann@example.com")
	f.browser("198.51.100.1").askCode("ann@example.com")
	if r := owner.enter(code); r.status != http.StatusSeeOther {
		t.Fatalf("someone else's request cancelled the owner's code: %d %s", r.status, r.body)
	}
}

func TestCodeWorksOnce(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.askCode("ann@example.com")
	code := f.mail.code(t, "ann@example.com")
	copied := b.clone("192.0.2.2")

	var wg sync.WaitGroup
	results := make([]response, 2)
	for i, br := range []*browser{b, copied} {
		wg.Go(func() { results[i] = br.enter(code) })
	}
	wg.Wait()
	ok := 0
	for _, r := range results {
		if r.status == http.StatusSeeOther {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("%d submissions of one code signed in", ok)
	}
	if n := f.count(`SELECT count(*) FROM local_sessions`); n != 1 {
		t.Fatalf("%d sessions", n)
	}
}

func TestExpiredCode(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.askCode("ann@example.com")
	code := f.mail.code(t, "ann@example.com")
	f.exec(`UPDATE local_codes SET expires_at = now() - interval '1 second'`)
	contains(t, b.enter(code), "This code has expired. Send a new one.")
}

func TestSendFailureDeletesTheCode(t *testing.T) {
	f := newFixture(t)
	f.mail.failWith(errSend)
	r := f.browser("192.0.2.1").askCode("ann@example.com")
	wantStatus(t, r, http.StatusBadGateway)
	contains(t, r, "We couldn't send a code right now.")
	if n := f.count(`SELECT count(*) FROM local_codes`); n != 0 {
		t.Fatalf("%d codes left", n)
	}
	// The attempt still counts against the limits.
	if n := f.count(`SELECT count(*) FROM local_limits WHERE count > 0`); n == 0 {
		t.Fatal("the attempt counted nowhere")
	}
}

func TestSendLimitsPerAddress(t *testing.T) {
	f := newFixture(t)
	for i := range 5 {
		r := f.browser("192.0.2." + string(rune('1'+i))).askCode("ann@example.com")
		wantStatus(t, r, http.StatusSeeOther)
	}
	r := f.browser("192.0.2.9").askCode("ann@example.com")
	wantStatus(t, r, http.StatusTooManyRequests)
	contains(t, r, "Too many attempts. Try again after")
	contains(t, r, "a passkey, still works")
	if n := len(f.mail.to("ann@example.com")); n != 5 {
		t.Fatalf("%d emails", n)
	}
	// Another address is not affected.
	wantStatus(t, f.browser("192.0.2.9").askCode("bob@example.com"), http.StatusSeeOther)
}

func TestSendLimitPerNetwork(t *testing.T) {
	f := newFixture(t)
	for i := range 30 {
		r := f.browser("192.0.2.1").askCode("user" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "@example.com")
		wantStatus(t, r, http.StatusSeeOther)
	}
	wantStatus(t, f.browser("192.0.2.1").askCode("one-more@example.com"), http.StatusTooManyRequests)
	wantStatus(t, f.browser("192.0.2.2").askCode("one-more@example.com"), http.StatusSeeOther)
}

func TestAppCeiling(t *testing.T) {
	f := newFixture(t)
	f.l.cfg.MaxCodesPerHour = 2
	wantStatus(t, f.browser("192.0.2.1").askCode("a@example.com"), http.StatusSeeOther)
	wantStatus(t, f.browser("192.0.2.2").askCode("b@example.com"), http.StatusSeeOther)
	wantStatus(t, f.browser("192.0.2.3").askCode("c@example.com"), http.StatusTooManyRequests)
}

func TestWindowsEnd(t *testing.T) {
	f := newFixture(t)
	for range 5 {
		f.browser("192.0.2.1").askCode("ann@example.com")
	}
	wantStatus(t, f.browser("192.0.2.1").askCode("ann@example.com"), http.StatusTooManyRequests)
	f.exec(`UPDATE local_limits SET window_ends_at = now() - interval '1 second'`)
	wantStatus(t, f.browser("192.0.2.1").askCode("ann@example.com"), http.StatusSeeOther)
	// The ended window started again at one.
	if n := f.count(`SELECT max(count) FROM local_limits`); n != 1 {
		t.Fatalf("counter at %d", n)
	}
}

// guessUntilPaused enters wrong codes for an address from fresh browsers until its
// shared counters pause it.
func guessUntilPaused(t *testing.T, f *fixture, address string) {
	t.Helper()
	entered := 0
	for attempt := 0; entered < 20; attempt++ {
		f.exec(`DELETE FROM local_limits WHERE window_ends_at < now() + interval '2 hours'`) // the hourly send counters
		b := f.browser("198.51.100." + string(rune('1'+attempt%9)))
		if r := b.askCode(address); r.status != http.StatusSeeOther {
			t.Fatalf("asking for a code: %d %s", r.status, r.body)
		}
		for range 5 {
			if entered == 20 {
				break
			}
			b.enter("99999999")
			entered++
		}
	}
}

func TestWrongEntriesPauseTheAddressButNotKnownBrowsers(t *testing.T) {
	f := newFixture(t)
	owner := f.browser("192.0.2.1")
	userID := owner.signIn("ann@example.com")
	owner.post("/logout", nil)

	guessUntilPaused(t, f, "ann@example.com")

	// The user is told, once, by the notice job: the guesser's request sends nothing
	// that would make it slower for an address with an account.
	for _, m := range f.mail.to("ann@example.com") {
		if strings.Contains(m.Subject, "are paused") {
			t.Fatal("the request sent the paused notice")
		}
	}
	if err := f.l.sendNotices(t.Context()); err != nil {
		t.Fatal(err)
	}
	paused := 0
	for _, m := range f.mail.to("ann@example.com") {
		if strings.Contains(m.Subject, "are paused") {
			paused++
		}
	}
	if paused != 1 {
		t.Fatalf("%d paused notices", paused)
	}
	// The shared counters send no code now, and accept none.
	r := f.browser("203.0.113.1").askCode("ann@example.com")
	wantStatus(t, r, http.StatusTooManyRequests)
	// The owner's browser signed in before, so it has counters of its own.
	f.clock = f.clock.Add(time.Minute)
	if id := owner.signIn("ann@example.com"); id != userID {
		t.Fatal("another user")
	}
	// local unblock resets the shared counters.
	if err := f.l.unblockCommand(t.Context(), []string{"ann@example.com"}); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.browser("203.0.113.1").askCode("ann@example.com"), http.StatusSeeOther)
}

func TestPausedAddressRefusesCodesSentBefore(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.askCode("ann@example.com")
	code := f.mail.code(t, "ann@example.com")
	f.exec(`DELETE FROM local_limits`)
	guessUntilPaused(t, f, "ann@example.com")
	r := b.enter(code)
	wantStatus(t, r, http.StatusTooManyRequests)
}

func TestWrongEntriesPerNetwork(t *testing.T) {
	f := newFixture(t)
	f.exec(`INSERT INTO local_limits (key, count, window_ends_at) VALUES ($1, 100, now() + interval '1 hour')`,
		f.l.counterKey(counter{wrongNetworkHour, "198.51.100.7"}))
	b := f.browser("198.51.100.7")
	b.askCode("ann@example.com")
	wantStatus(t, b.enter(f.mail.code(t, "ann@example.com")), http.StatusTooManyRequests)
}

func TestNextMustBeLocal(t *testing.T) {
	f := newFixture(t)
	for i, next := range []string{"https://evil.example/", "//evil.example", `/../\evil.example`, "/\t/evil.example"} {
		b := f.browser("192.0.2." + strconv.Itoa(i+1))
		b.get("/login")
		b.post("/login", url.Values{"email": {"ann@example.com"}, "next": {next}})
		r := b.enter(f.mail.code(t, "ann@example.com"))
		if r.location != "/account" {
			t.Fatalf("next %q redirected to %q", next, r.location)
		}
	}
}

func TestLoginNeedsCSRF(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.get("/login")
	b.csrf = "forged"
	wantStatus(t, b.post("/login", url.Values{"email": {"ann@example.com"}}), http.StatusForbidden)
}

func TestSignedInVisitorSkipsTheLoginPage(t *testing.T) {
	f := newFixture(t)
	b := f.browser("192.0.2.1")
	b.signIn("ann@example.com")
	r := b.get("/login?next=/account/settings")
	if r.status != http.StatusSeeOther || r.location != "/account/settings" {
		t.Fatalf("%d %s", r.status, r.location)
	}
}
