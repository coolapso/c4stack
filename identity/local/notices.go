package local

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/coolapso/c4stack/email"
)

// Kind is what an email is for.
type Kind int

// The emails the provider sends. Codes go out at once, in the request; the others are
// security notices, queued in the transaction that made the change.
const (
	// SignInCode is a code to sign in, to an address with a user.
	SignInCode Kind = iota + 1
	// SignUpCode is a code to create an account, to an address without a user.
	SignUpCode
	// NewAddressCode is a code to the address someone wants to change to.
	NewAddressCode
	// AddressTaken goes instead of a code when another user has the new address.
	AddressTaken
	// AddressChanged tells the old address it was changed.
	AddressChanged
	// PasskeyAdded and PasskeyRemoved tell the address a passkey changed.
	PasskeyAdded
	PasskeyRemoved
	// CodesPaused tells the address that wrong codes paused its shared counters.
	CodesPaused
	// AddressChangedBySupport and AddressSetBySupport tell the old and the new address
	// about local set-email.
	AddressChangedBySupport
	AddressSetBySupport
)

var kindNames = map[Kind]string{
	SignInCode:              "sign_in_code",
	SignUpCode:              "sign_up_code",
	NewAddressCode:          "new_address_code",
	AddressTaken:            "address_taken",
	AddressChanged:          "address_changed",
	PasskeyAdded:            "passkey_added",
	PasskeyRemoved:          "passkey_removed",
	CodesPaused:             "codes_paused",
	AddressChangedBySupport: "address_changed_by_support",
	AddressSetBySupport:     "address_set_by_support",
}

// String returns the kind's name, such as "address_changed".
func (k Kind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

func kindNamed(name string) (Kind, bool) {
	for k, s := range kindNames {
		if s == name {
			return k, true
		}
	}
	return 0, false
}

// Notice is what an email says. Fields a kind doesn't use are zero.
type Notice struct {
	Kind    Kind
	To      string
	AppName string
	// Site is the public URL's host, such as "acme.example".
	Site string
	// Code is the code, written "4821 0937", in the code emails, and Minutes how long
	// it works.
	Code    string
	Minutes int
	// OldEmail and NewEmail are the address changes'.
	OldEmail string
	NewEmail string
	// Passkey is the passkey's name.
	Passkey string
	// Device made the change, such as "Firefox on Linux".
	Device string
	// At is when it happened.
	At time.Time
	// Until is when codes work again everywhere, in CodesPaused.
	Until time.Time
	// Reference is the user's ID, for support.
	Reference   string
	SettingsURL string
}

// noticeData is what local_notices keeps of a notice: what can't be known again when
// it is sent.
type noticeData struct {
	OldEmail  string    `json:"old_email,omitempty"`
	NewEmail  string    `json:"new_email,omitempty"`
	Passkey   string    `json:"passkey,omitempty"`
	Device    string    `json:"device,omitempty"`
	At        time.Time `json:"at"`
	Until     time.Time `json:"until,omitzero"`
	Reference string    `json:"reference,omitempty"`
}

func (l *Local) notice(kind Kind, to string) Notice {
	return Notice{
		Kind:        kind,
		To:          to,
		AppName:     l.cfg.AppName,
		Site:        l.site,
		Minutes:     int(codeLifetime / time.Minute),
		SettingsURL: l.origin + l.settingsPath,
	}
}

// sendNow sends an email in the request, for codes the person is waiting for. Its
// error is logged, so it never carries what the email does: whatever the sender's
// error says, the addresses, the code and the passkey's name are taken out.
func (l *Local) sendNow(ctx context.Context, n Notice) error {
	clean := func(err error) string {
		said := []string{n.To, n.OldEmail, n.NewEmail, n.Code, strings.ReplaceAll(n.Code, " ", ""), n.Passkey}
		for _, v := range said {
			said = append(said, html.EscapeString(v)) // as the HTML has them
		}
		return redact(err.Error(), said...)
	}
	m, err := l.message(ctx, n)
	if err != nil {
		return errors.New("local: writing the email failed: " + clean(err))
	}
	if _, err := l.cfg.Sender.Send(ctx, m); err != nil {
		return errors.New("local: the email provider refused the message: " + clean(err))
	}
	return nil
}

// redact replaces each of the values in s, ignoring case, with "[redacted]".
func redact(s string, values ...string) string {
	for _, v := range values {
		if len(v) < 3 {
			continue
		}
		for {
			i := strings.Index(strings.ToLower(s), strings.ToLower(v))
			if i < 0 {
				break
			}
			s = s[:i] + "[redacted]" + s[i+len(v):]
		}
	}
	return s
}

// queueNotice writes a security notice inside the transaction that made the change, so
// the change and its notice commit together.
func (l *Local) queueNotice(ctx context.Context, tx *sql.Tx, userID string, kind Kind, to string, d noticeData) (string, error) {
	data, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO local_notices (user_id, kind, recipient, data)
		VALUES ($1, $2, $3, $4)
		RETURNING id`, userID, kind.String(), to, data,
	).Scan(&id)
	return id, err
}

// deliver sends queued notices right after their transaction committed, with a context
// detached from the request, so closing the tab cannot stop them. A notice that fails
// stays queued for the job. It reports whether every notice was accepted.
func (l *Local) deliver(ctx context.Context, ids ...string) bool {
	if len(ids) == 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()
	all := true
	for _, id := range ids {
		sent, err := l.deliverOne(ctx, id)
		if err != nil {
			l.app.Log().Warn("sending a security notice failed; it stays queued", "notice_id", id, "error", err)
		}
		all = all && sent
	}
	return all
}

// deliverOne claims a queued notice, sends it, and deletes it once the email provider
// accepted it. The claim moves its next attempt forward, with growing gaps, so a
// replica or the job doesn't send it again meanwhile. It reports whether this call sent
// it.
func (l *Local) deliverOne(ctx context.Context, id string) (bool, error) {
	var userID, kindName, to string
	var data []byte
	err := l.app.DB().QueryRowContext(ctx, `
		UPDATE local_notices
		SET attempts = attempts + 1,
		    next_attempt_at = now() + least(power(2, attempts) * interval '1 minute', interval '1 hour')
		WHERE id = $1 AND next_attempt_at <= now()
		RETURNING user_id, kind, recipient, data`, id,
	).Scan(&userID, &kindName, &to, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // sent already, or claimed by another attempt
	}
	if err != nil {
		return false, err
	}
	kind, ok := kindNamed(kindName)
	if !ok {
		return false, fmt.Errorf("local: unknown notice kind %q", kindName)
	}
	var d noticeData
	if err := json.Unmarshal(data, &d); err != nil {
		return false, err
	}
	n := l.notice(kind, to)
	n.OldEmail, n.NewEmail, n.Passkey, n.Device = d.OldEmail, d.NewEmail, d.Passkey, d.Device
	n.At, n.Until, n.Reference = d.At, d.Until, d.Reference
	if err := l.sendNow(ctx, n); err != nil {
		return false, err
	}
	if _, err := l.app.DB().ExecContext(ctx, `
		DELETE FROM local_notices
		WHERE id = $1`, id,
	); err != nil {
		// It was sent; the next attempt sends it again, which is better than never.
		return true, err
	}
	l.app.Log().Info("sent a security notice", "kind", kindName, "user_id", userID)
	return true, nil
}

// sendNotices retries the notices still queued, and drops those older than
// noticeRetention.
func (l *Local) sendNotices(ctx context.Context) error {
	if err := l.dropOldNotices(ctx); err != nil {
		return err
	}
	due, err := l.dueNotices(ctx)
	if err != nil {
		return err
	}
	for _, id := range due {
		if _, err := l.deliverOne(ctx, id); err != nil {
			l.app.Log().Warn("sending a security notice failed; it stays queued", "notice_id", id, "error", err)
		}
	}
	return nil
}

// dropOldNotices deletes the notices that could not be sent for noticeRetention, and
// logs each.
func (l *Local) dropOldNotices(ctx context.Context) error {
	rows, err := l.app.DB().QueryContext(ctx, `
		DELETE FROM local_notices
		WHERE created_at < now() - make_interval(secs => $1)
		RETURNING kind, user_id`, noticeRetention.Seconds(),
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, userID string
		if err := rows.Scan(&kind, &userID); err != nil {
			return err
		}
		l.app.Log().Error("dropped a security notice that could not be sent for 3 days", "kind", kind, "user_id", userID)
	}
	return rows.Err()
}

// dueNotices returns the queued notices whose next attempt is due.
func (l *Local) dueNotices(ctx context.Context) ([]string, error) {
	rows, err := l.app.DB().QueryContext(ctx, `
		SELECT id
		FROM local_notices
		WHERE next_attempt_at <= now()
		ORDER BY next_attempt_at
		LIMIT 100`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var due []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		due = append(due, id)
	}
	return due, rows.Err()
}

// formatTime writes a time as the emails do: "19 November 2026 at 14:02 UTC".
func formatTime(t time.Time) string {
	return t.UTC().Format("2 January 2006 at 15:04") + " UTC"
}

// message writes the email for n: the app's Message, or the default words in the app's
// Layout. It always goes to n.To.
func (l *Local) message(ctx context.Context, n Notice) (email.Message, error) {
	var m email.Message
	if l.cfg.Message != nil {
		m = l.cfg.Message(n)
	} else {
		subject, body := defaultEmail(n)
		var err error
		if m, err = email.Compose(ctx, subject, body, l.cfg.Layout); err != nil {
			return email.Message{}, err
		}
	}
	m.To = n.To
	return m, nil
}

// defaultEmail writes an email's subject and body in the default words.
func defaultEmail(n Notice) (string, email.Body) {
	var subject string
	var b email.Body
	b.Paragraph("Hello,")
	switch n.Kind {
	case SignInCode:
		subject = fmt.Sprintf("Your %s sign-in code", n.AppName)
		b.Paragraph(fmt.Sprintf("Here is your code to sign in to %s:", n.AppName))
		b.Code(n.Code)
		b.Paragraph(fmt.Sprintf("Enter it at %s, on the page where you asked for it. It works once, within %d minutes.", n.Site, n.Minutes))
		b.Paragraph(fmt.Sprintf("If you didn't ask for it, ignore this email: nobody can sign in without the code. Never give it to anyone. %s will never ask you for it.", n.AppName))
	case SignUpCode:
		subject = fmt.Sprintf("Your %s sign-up code", n.AppName)
		b.Paragraph(fmt.Sprintf("Here is your code to create %s account with this address:", article(n.AppName)))
		b.Code(n.Code)
		b.Paragraph(fmt.Sprintf("Enter it at %s, on the page where you asked for it. It works once, within %d minutes.", n.Site, n.Minutes))
		b.Paragraph("If you didn't ask for it, ignore this email: no account is created without the code.")
	case NewAddressCode:
		subject = fmt.Sprintf("Confirm your new address for %s", n.AppName)
		b.Paragraph(fmt.Sprintf("Someone signed in to %s asked to use this address for their account. To confirm it, enter this code at %s:", n.AppName, n.Site))
		b.Code(n.Code)
		b.Paragraph(fmt.Sprintf("It works once, within %d minutes. If you didn't ask for it, ignore this email: the address isn't used without the code.", n.Minutes))
	case AddressTaken:
		subject = fmt.Sprintf("Your address and %s", n.AppName)
		b.Paragraph(fmt.Sprintf("Someone signed in to %s asked to use this address for their account. This address already has %s account, so nothing changes, and that account stays as it is.", n.AppName, article(n.AppName)))
		b.Paragraph("If it was you, sign in with this address to use that account, or choose another address.")
	case AddressChanged:
		subject = fmt.Sprintf("Your %s address was changed", n.AppName)
		b.Paragraph(fmt.Sprintf("The address of your %s account was changed from %s to %s on %s, from %s. Sign-in codes now go to the new address, and your other devices were signed out.", n.AppName, n.OldEmail, n.NewEmail, formatTime(n.At), n.Device))
		b.Paragraph("If you made this change, there is nothing to do.")
		b.Paragraph("If you didn't, someone else may be signed in to your account. Reply to this email right away, and quote this reference: " + n.Reference)
	case PasskeyAdded:
		subject = fmt.Sprintf("A passkey was added to your %s account", n.AppName)
		b.Paragraph(fmt.Sprintf("A passkey named %q was added to your %s account on %s, from %s. It can sign in to your account without a code.", n.Passkey, n.AppName, formatTime(n.At), n.Device))
		b.Paragraph("If you added it, there is nothing to do.")
		b.Paragraph("If you didn't, sign in, remove it, and sign out your other devices.")
		b.Link("Open your settings", n.SettingsURL)
	case PasskeyRemoved:
		subject = fmt.Sprintf("A passkey was removed from your %s account", n.AppName)
		b.Paragraph(fmt.Sprintf("The passkey named %q was removed from your %s account on %s, from %s. It can no longer sign in.", n.Passkey, n.AppName, formatTime(n.At), n.Device))
		b.Paragraph("If you removed it, there is nothing to do.")
		b.Paragraph("If you didn't, sign in and check your passkeys and devices.")
		b.Link("Open your settings", n.SettingsURL)
	case CodesPaused:
		subject = fmt.Sprintf("Sign-in codes for your %s account are paused", n.AppName)
		b.Paragraph(fmt.Sprintf("Wrong sign-in codes for your %s account were entered many times today. Until %s, %s only accepts codes for it on browsers where you signed in before. Nobody signed in with the wrong codes.", n.AppName, formatTime(n.Until), n.AppName))
		b.Paragraph("You can also sign in with a passkey, if you added one. If you can't sign in and need to, reply to this email.")
	case AddressChangedBySupport:
		subject = fmt.Sprintf("Your %s address was changed by support", n.AppName)
		b.Paragraph(fmt.Sprintf("%s's support changed the address of your %s account from %s to %s on %s. Every device was signed out and every passkey removed. Sign-in codes now go to the new address.", n.AppName, n.AppName, n.OldEmail, n.NewEmail, formatTime(n.At)))
		b.Paragraph("If you didn't ask for this, reply to this email right away, and quote this reference: " + n.Reference)
	case AddressSetBySupport:
		subject = fmt.Sprintf("Your %s account now uses this address", n.AppName)
		b.Paragraph(fmt.Sprintf("%s's support set this address on your %s account on %s. Every device was signed out and every passkey removed. To sign in, ask for a code with this address at %s, then add your passkeys again.", n.AppName, n.AppName, formatTime(n.At), n.Site))
		b.Paragraph("If you didn't ask for this, reply to this email.")
	}
	return subject, b
}

// article puts "a" or "an" before a name: "an Acme account", "a Textlab account".
func article(name string) string {
	if name != "" && strings.ContainsRune("AEIOUaeiou", rune(name[0])) {
		return "an " + name
	}
	return "a " + name
}
