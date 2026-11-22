package local

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/a-h/templ"
	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

// The profile UI manages the signed-in user's address, passkeys and signed-in devices.
// The accounts module embeds it in its settings page, and mounts its routes under the
// settings page's path, behind RequireSignIn. Everything but adding a passkey works
// without JavaScript.

// MountProfile adds the profile UI's routes under r, the settings page's group, which
// must require sign-in. The address change runs inside this provider's transaction
// through the accounts hooks, so emailChanged isn't used, and the browser stays signed
// in: logoutPath and emailChangedPath aren't used either.
func (l *Local) MountProfile(r gin.IRouter, layout ui.Layout, logoutPath, emailChangedPath string, emailChanged func(c *gin.Context, email string) error) {
	l.addAssets(layout.Assets)
	l.layout = layout
	l.settingsPath = joinPath(r, "")
	if l.settingsPath == "" {
		l.settingsPath = "/"
	}
	r.GET("/email", l.emailForm)
	r.POST("/email", l.emailStart)
	r.GET("/email/code", l.emailCodeForm)
	r.POST("/email/code", l.emailVerify)
	r.POST("/email/code/resend", l.emailResend)
	r.POST("/passkeys/options", l.passkeyAddOptions)
	r.POST("/passkeys", l.passkeyAdd)
	r.POST("/passkeys/:id/remove", l.passkeyRemove)
	r.POST("/devices/:id/sign-out", l.deviceSignOut)
	r.POST("/devices/sign-out-others", l.signOutOthers)
}

// ProfileCSP lists what pages that show the profile UI must allow: nothing beyond the
// default policy, since the passkey script is the app's own asset.
func (l *Local) ProfileCSP() web.CSP {
	return web.CSP{}
}

// done are the messages the settings page shows after a change, by the done query
// parameter the change redirects with.
var done = map[string]string{
	"address-changed": "Your address was changed. Your other devices were signed out.",
	"passkey-added":   "Your passkey was added. You can sign in with it now.",
	"passkey-removed": "The passkey was removed.",
	"device":          "The device was signed out.",
	"devices":         "Your other devices were signed out.",
}

// ProfileSection renders the profile UI for the signed-in user.
func (l *Local) ProfileSection(c *gin.Context) templ.Component {
	id, _ := identity.FromContext(c.Request.Context())
	d := profileData{
		c:         c,
		email:     id.Email,
		recent:    id.Recent(time.Now()),
		settings:  l.settingsPath,
		confirm:   l.ConfirmURL(l.settingsPath),
		notice:    done[c.Query("done")],
		scriptURL: l.assets.URL("local/passkey.js"),
	}
	err := l.listPasskeys(c, id.Subject, &d)
	if err == nil {
		err = l.listDevices(c, id.Subject, &d)
	}
	if err != nil {
		web.Log(c).Error("loading the profile failed", "error", err)
		d.err = "Your passkeys and devices could not be loaded. Reload the page to try again."
	}
	d.canAdd = len(d.passkeys) < maxPasskeys
	return profileSection(d)
}

func (l *Local) listPasskeys(c *gin.Context, userID string, d *profileData) error {
	rows, err := l.app.DB().QueryContext(c.Request.Context(), `
		SELECT id, name, created_at, last_used_at, backup_eligible
		FROM local_passkeys
		WHERE user_id = $1
		ORDER BY created_at`, userID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p passkeyView
		var used sql.NullTime
		if err := rows.Scan(&p.id, &p.name, &p.added, &used, &p.synced); err != nil {
			return err
		}
		p.used = used.Time
		d.passkeys = append(d.passkeys, p)
	}
	return rows.Err()
}

func (l *Local) listDevices(c *gin.Context, userID string, d *profileData) error {
	var current []byte
	if token := l.sessionToken(c.Request); token != nil {
		current = hashToken(token)
	}
	rows, err := l.app.DB().QueryContext(c.Request.Context(), `
		SELECT id, device, created_at, last_seen_at, token_hash = $2
		FROM local_sessions
		WHERE user_id = $1
		    AND created_at > now() - make_interval(secs => $3)
		    AND last_seen_at > now() - make_interval(secs => $4)
		ORDER BY token_hash = $2 DESC, last_seen_at DESC`,
		userID, current, sessionLifetime.Seconds(), sessionIdle.Seconds(),
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var s deviceView
		if err := rows.Scan(&s.id, &s.label, &s.signedIn, &s.seen, &s.current); err != nil {
			return err
		}
		d.devices = append(d.devices, s)
	}
	return rows.Err()
}

// backToSettings redirects to the settings page with a message.
func (l *Local) backToSettings(c *gin.Context, message, anchor string) {
	c.Redirect(http.StatusSeeOther, l.settingsPath+"?done="+url.QueryEscape(message)+"#"+anchor)
}

func (l *Local) renderPage(c *gin.Context, status int, title string, body templ.Component) {
	l.layout.Render(c, status, title, body)
}

// profileFailure shows an error from a profile change on a page of its own.
func (l *Local) profileFailure(c *gin.Context, err error, step string) {
	status, msg := l.failure(c, err, step)
	l.renderPage(c, status, "Something went wrong", messagePage("Something went wrong", msg, l.settingsPath))
}

// emailForm asks for the new address. It needs a recent sign-in.
func (l *Local) emailForm(c *gin.Context) {
	id, _ := identity.FromContext(c.Request.Context())
	l.renderPage(c, http.StatusOK, "Change your address", emailPage(l.emailData(c, id, "")))
}

func (l *Local) emailData(c *gin.Context, id identity.Identity, newAddress string) emailData {
	return emailData{
		c:        c,
		current:  id.Email,
		email:    newAddress,
		recent:   id.Recent(time.Now()),
		action:   l.settingsPath + "/email",
		settings: l.settingsPath,
		confirm:  l.ConfirmURL(l.settingsPath + "/email"),
	}
}

// emailStart makes an address change's attempt. If another user has the address, the
// attempt is a decoy: its code is never sent and never matches, and the address gets
// the "address taken" email instead. Both count against the same limits and lead to the
// same page, so the form can't tell anyone which addresses have accounts.
func (l *Local) emailStart(c *gin.Context) {
	id, _ := identity.FromContext(c.Request.Context())
	raw := c.PostForm("email")
	address, ok := normalizeAddress(raw)
	d := l.emailData(c, id, raw)
	switch {
	case !d.recent:
		l.renderPage(c, http.StatusForbidden, "Change your address", emailPage(d))
		return
	case !ok:
		d.err = "Enter the new address, such as ann@example.com."
		l.renderPage(c, http.StatusUnprocessableEntity, "Change your address", emailPage(d))
		return
	case address == id.Email:
		d.err = "This is already your address."
		l.renderPage(c, http.StatusUnprocessableEntity, "Change your address", emailPage(d))
		return
	case !l.allowed(address):
		d.err = "This address can't be used here."
		l.renderPage(c, http.StatusUnprocessableEntity, "Change your address", emailPage(d))
		return
	}
	if status, msg := l.sendNewAddressCode(c, id.Subject, address); msg != "" {
		d.err = msg
		l.renderPage(c, status, "Change your address", emailPage(d))
		return
	}
	c.Redirect(http.StatusSeeOther, l.settingsPath+"/email/code")
}

// sendNewAddressCode replaces the user's address change attempt with a new one, and
// sends its code, or the "address taken" email for a decoy.
func (l *Local) sendNewAddressCode(c *gin.Context, userID, address string) (int, string) {
	ctx := c.Request.Context()
	codeID, code := newUUID(), newCode()
	var taken bool
	err := l.inTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (
			    SELECT 1
			    FROM local_users
			    WHERE email = $1 AND id <> $2
			)`, address, userID,
		).Scan(&taken); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM local_codes
			WHERE user_id = $1 AND purpose = 'new_email'`, userID,
		); err != nil {
			return err
		}
		if err := l.countNewAddress(ctx, tx, c, address, userID); err != nil {
			return err
		}
		hash := l.codeHash(codeID, code)
		if taken {
			hash = newToken() // never matches
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO local_codes (id, purpose, email, user_id, code_hash, decoy, expires_at)
			VALUES ($1, 'new_email', $2, $3, $4, $5, now() + make_interval(secs => $6))`,
			codeID, address, userID, hash, taken, codeLifetime.Seconds(),
		)
		return err
	})
	if status, msg := l.failure(c, err, "sending a new-address code"); msg != "" {
		return status, msg
	}
	n := l.notice(NewAddressCode, address)
	n.Code = formatCode(code)
	if taken {
		n = l.notice(AddressTaken, address)
	}
	if err := l.sendNow(ctx, n); err != nil {
		web.Log(c).Warn("the email provider refused a new-address code", "error", err)
		_ = deleteCodeNow(c, l, codeID)
		return http.StatusBadGateway, "We couldn't send a code right now. Try again in a moment."
	}
	return 0, ""
}

func deleteCodeNow(c *gin.Context, l *Local, id string) error {
	_, err := l.app.DB().ExecContext(c.Request.Context(), `
		DELETE FROM local_codes
		WHERE id = $1`, id,
	)
	return err
}

// countNewAddress checks and counts the limits on an address change's code, in their
// fixed order: the app, the network, the new address's shared counters, and the user's
// new-address counters. "Address taken" emails and decoys count exactly as codes do.
func (l *Local) countNewAddress(ctx context.Context, tx *sql.Tx, c *gin.Context, address, userID string) error {
	if l.cfg.MaxCodesPerHour > 0 {
		if err := l.count(ctx, tx, counter{l.sendAppHour(), ""}); err != nil {
			return err
		}
	}
	return l.count(ctx, tx,
		counter{sendNetworkHour, network(c)},
		counter{sendAddressHour, address},
		counter{sendAddressDay, address},
		counter{newAddressUserHour, userID},
		counter{newAddressUserDay, userID},
	)
}

// pendingChange returns the user's address change attempt, if one is waiting, and how
// long until it may send a new code.
func (l *Local) pendingChange(c *gin.Context, userID string) (address string, wait time.Duration, ok bool, err error) {
	var seconds float64
	err = l.app.DB().QueryRowContext(c.Request.Context(), `
		SELECT email, greatest(0, extract(epoch FROM created_at + make_interval(secs => $2) - now()))
		FROM local_codes
		WHERE user_id = $1 AND purpose = 'new_email' AND expires_at > now()`, userID, resendAfter.Seconds(),
	).Scan(&address, &seconds)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	return address, time.Duration(seconds * float64(time.Second)), err == nil, err
}

func (l *Local) emailCodeData(c *gin.Context, address string) codeData {
	return codeData{
		c:       c,
		email:   address,
		minutes: int(codeLifetime / time.Minute),
		action:  l.settingsPath + "/email/code",
		resend:  l.settingsPath + "/email/code/resend",
		restart: l.settingsPath + "/email",
		change:  true,
	}
}

// emailCodeForm asks for the code sent to the new address.
func (l *Local) emailCodeForm(c *gin.Context) {
	id, _ := identity.FromContext(c.Request.Context())
	address, _, ok, err := l.pendingChange(c, id.Subject)
	if err != nil {
		l.profileFailure(c, err, "loading an address change")
		return
	}
	if !ok {
		c.Redirect(http.StatusSeeOther, l.settingsPath+"/email")
		return
	}
	l.renderPage(c, http.StatusOK, "Confirm your new address", codePage(l.emailCodeData(c, address)))
}

// emailVerify checks the code and changes the address, in one transaction: it locks
// the user, rechecks the code and that the session asking still exists and is still
// recent, moves the address on the accounts side and here, ends every other session,
// gives this browser a new token and device cookie, and queues the notice to the old
// address.
func (l *Local) emailVerify(c *gin.Context) {
	id, _ := identity.FromContext(c.Request.Context())
	address, _, ok, err := l.pendingChange(c, id.Subject)
	if err != nil {
		l.profileFailure(c, err, "loading an address change")
		return
	}
	if !ok {
		c.Redirect(http.StatusSeeOther, l.settingsPath+"/email")
		return
	}
	d := l.emailCodeData(c, address)
	code, ok := parseCode(c.PostForm("code"))
	if !ok {
		d.err = fmt.Sprintf("Enter the %d-digit code from the email.", codeDigits)
		l.renderPage(c, http.StatusUnprocessableEntity, "Confirm your new address", codePage(d))
		return
	}
	ctx := c.Request.Context()
	var message, oldAddress string
	var notices []string
	var token []byte
	var issued time.Time
	var changed bool
	err = l.inTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			SELECT email
			FROM local_users
			WHERE id = $1
			FOR NO KEY UPDATE`, id.Subject,
		).Scan(&oldAddress); err != nil {
			return err
		}
		var codeID, sentTo string
		var hash []byte
		var decoy, expired bool
		var wrong int
		err := tx.QueryRowContext(ctx, `
			SELECT id, email, code_hash, decoy, wrong_entries, expires_at <= now()
			FROM local_codes
			WHERE user_id = $1 AND purpose = 'new_email'
			FOR UPDATE`, id.Subject,
		).Scan(&codeID, &sentTo, &hash, &decoy, &wrong, &expired)
		if errors.Is(err, sql.ErrNoRows) {
			message = "This code is no longer valid. Send a new one."
			return nil
		}
		if err != nil {
			return err
		}
		if expired || sentTo != address {
			message = "This code has expired. Send a new one."
			return deleteCode(ctx, tx, codeID)
		}
		if !l.allowed(address) {
			message = "This address can't be used here."
			return deleteCode(ctx, tx, codeID)
		}
		sessionID, recent, err := l.currentSession(ctx, tx, c.Request, id.Subject)
		if errors.Is(err, sql.ErrNoRows) {
			return identity.ErrNoSession
		}
		if err != nil {
			return err
		}
		if !recent {
			return errNotRecent
		}
		net := counter{wrongNetworkHour, network(c)}
		if err := l.check(ctx, tx, net); err != nil {
			return err
		}
		if decoy || !hmac.Equal(l.codeHash(codeID, code), hash) {
			if _, _, err := l.add(ctx, tx, net); err != nil {
				return err
			}
			wrong++
			if wrong >= wrongPerCode {
				message = "Too many wrong codes. Send a new one."
				return deleteCode(ctx, tx, codeID)
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE local_codes
				SET wrong_entries = wrong_entries + 1
				WHERE id = $1`, codeID,
			); err != nil {
				return err
			}
			message = fmt.Sprintf("That code isn't right. %d tries left.", wrongPerCode-wrong)
			if wrongPerCode-wrong == 1 {
				message = "That code isn't right. 1 try left."
			}
			return nil
		}
		if err := l.moveAddress(ctx, tx, id.Subject, address); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM local_sessions
			WHERE user_id = $1 AND id <> $2`, id.Subject, sessionID,
		); err != nil {
			return err
		}
		token = newToken()
		if err := tx.QueryRowContext(ctx, `
			UPDATE local_sessions
			SET token_hash = $2
			WHERE id = $1
			RETURNING now()`, sessionID, hashToken(token),
		).Scan(&issued); err != nil {
			return err
		}
		notice, err := l.queueNotice(ctx, tx, id.Subject, AddressChanged, oldAddress, noticeData{
			OldEmail: oldAddress, NewEmail: address, Device: deviceLabel(c.Request.UserAgent()),
			At: time.Now(), Reference: id.Subject,
		})
		if err != nil {
			return err
		}
		notices = append(notices, notice)
		changed = true
		return nil
	})
	switch {
	case errors.Is(err, identity.ErrAddressTaken):
		d.err = "This address belongs to another account."
		l.renderPage(c, http.StatusConflict, "Confirm your new address", codePage(d))
		return
	case errors.Is(err, errNotRecent):
		l.renderPage(c, http.StatusForbidden, "Confirm it's you", messagePage("Confirm it's you",
			"Changing your address needs a recent sign-in. Confirm it's you, then enter a new code.", l.ConfirmURL(l.settingsPath+"/email")))
		return
	case errors.Is(err, identity.ErrNoSession):
		c.Redirect(http.StatusSeeOther, l.loginURL(l.settingsPath))
		return
	case err != nil:
		l.profileFailure(c, err, "changing an address")
		return
	case !changed:
		d.err = message
		l.renderPage(c, http.StatusUnprocessableEntity, "Confirm your new address", codePage(d))
		return
	}
	l.setSessionCookie(c.Writer, token)
	l.setDevice(c, id.Subject, issued)
	l.deliver(ctx, notices...)
	web.Log(c).Info("changed a user's address", "user_id", id.Subject)
	l.backToSettings(c, "address-changed", "email")
}

var errNotRecent = errors.New("local: the session is not recent")

// emailResend sends a new code for the address change, no sooner than resendAfter after
// the last.
func (l *Local) emailResend(c *gin.Context) {
	id, _ := identity.FromContext(c.Request.Context())
	address, wait, ok, err := l.pendingChange(c, id.Subject)
	if err != nil {
		l.profileFailure(c, err, "loading an address change")
		return
	}
	if !ok {
		c.Redirect(http.StatusSeeOther, l.settingsPath+"/email")
		return
	}
	d := l.emailCodeData(c, address)
	if wait > 0 {
		d.err = fmt.Sprintf("You can ask for a new code in %d seconds.", int(wait.Seconds())+1)
		l.renderPage(c, http.StatusTooManyRequests, "Confirm your new address", codePage(d))
		return
	}
	if !id.Recent(time.Now()) {
		l.renderPage(c, http.StatusForbidden, "Confirm it's you", messagePage("Confirm it's you",
			"Changing your address needs a recent sign-in. Confirm it's you, then start again.", l.ConfirmURL(l.settingsPath+"/email")))
		return
	}
	if status, msg := l.sendNewAddressCode(c, id.Subject, address); msg != "" {
		d.err = msg
		l.renderPage(c, status, "Confirm your new address", codePage(d))
		return
	}
	d.info = "We sent a new code. Only the newest one works."
	l.renderPage(c, http.StatusOK, "Confirm your new address", codePage(d))
}

// passkeyRemove removes a passkey. It needs a recent sign-in, and tells the address.
// Removing the last passkey is allowed: codes still work.
func (l *Local) passkeyRemove(c *gin.Context) {
	id, _ := identity.FromContext(c.Request.Context())
	if !id.Recent(time.Now()) {
		l.renderPage(c, http.StatusForbidden, "Confirm it's you", messagePage("Confirm it's you",
			"Removing a passkey needs a recent sign-in.", l.ConfirmURL(l.settingsPath)))
		return
	}
	passkeyID := c.Param("id")
	if !validUUID(passkeyID) {
		l.backToSettings(c, "", "passkeys")
		return
	}
	ctx := c.Request.Context()
	var notices []string
	err := l.inTx(ctx, func(tx *sql.Tx) error {
		var address string
		if err := tx.QueryRowContext(ctx, `
			SELECT email
			FROM local_users
			WHERE id = $1
			FOR NO KEY UPDATE`, id.Subject,
		).Scan(&address); err != nil {
			return err
		}
		_, recent, err := l.currentSession(ctx, tx, c.Request, id.Subject)
		if errors.Is(err, sql.ErrNoRows) {
			return identity.ErrNoSession
		}
		if err != nil {
			return err
		}
		if !recent {
			return errNotRecent
		}
		var name string
		err = tx.QueryRowContext(ctx, `
			DELETE FROM local_passkeys
			WHERE id = $1 AND user_id = $2
			RETURNING name`, passkeyID, id.Subject,
		).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // removed already
		}
		if err != nil {
			return err
		}
		notice, err := l.queueNotice(ctx, tx, id.Subject, PasskeyRemoved, address, noticeData{
			Passkey: name, Device: deviceLabel(c.Request.UserAgent()), At: time.Now(),
		})
		if err != nil {
			return err
		}
		notices = append(notices, notice)
		web.Log(c).Info("removed a passkey", "user_id", id.Subject, "passkey_id", passkeyID)
		return nil
	})
	switch {
	case errors.Is(err, errNotRecent):
		l.renderPage(c, http.StatusForbidden, "Confirm it's you", messagePage("Confirm it's you",
			"Removing a passkey needs a recent sign-in.", l.ConfirmURL(l.settingsPath)))
		return
	case errors.Is(err, identity.ErrNoSession):
		c.Redirect(http.StatusSeeOther, l.loginURL(l.settingsPath))
		return
	case err != nil:
		l.profileFailure(c, err, "removing a passkey")
		return
	}
	l.deliver(ctx, notices...)
	l.backToSettings(c, "passkey-removed", "passkeys")
}

// deviceSignOut ends one of the user's other sessions. It only takes access away, so it
// needs no recent sign-in, but it locks the user and rechecks that the session asking
// still exists, so a request that waited behind an address change or a recovery that
// ended it signs nobody out.
func (l *Local) deviceSignOut(c *gin.Context) {
	id, _ := identity.FromContext(c.Request.Context())
	sessionID := c.Param("id")
	if !validUUID(sessionID) {
		l.backToSettings(c, "", "devices")
		return
	}
	ctx := c.Request.Context()
	err := l.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			SELECT 1
			FROM local_users
			WHERE id = $1
			FOR NO KEY UPDATE`, id.Subject,
		); err != nil {
			return err
		}
		current, _, err := l.currentSession(ctx, tx, c.Request, id.Subject)
		if errors.Is(err, sql.ErrNoRows) {
			return identity.ErrNoSession
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			DELETE FROM local_sessions
			WHERE id = $1 AND user_id = $2 AND id <> $3`, sessionID, id.Subject, current,
		)
		return err
	})
	if errors.Is(err, identity.ErrNoSession) {
		c.Redirect(http.StatusSeeOther, l.loginURL(l.settingsPath))
		return
	}
	if err != nil {
		l.profileFailure(c, err, "signing out a device")
		return
	}
	web.Log(c).Info("signed out a device", "user_id", id.Subject, "session_id", sessionID)
	l.backToSettings(c, "device", "devices")
}

// signOutOthers ends every session of the user but this one, and resets their known
// browsers: this one gets a new device cookie.
func (l *Local) signOutOthers(c *gin.Context) {
	id, _ := identity.FromContext(c.Request.Context())
	ctx := c.Request.Context()
	var issued time.Time
	err := l.inTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			SELECT now()
			FROM local_users
			WHERE id = $1
			FOR NO KEY UPDATE`, id.Subject,
		).Scan(&issued); err != nil {
			return err
		}
		sessionID, _, err := l.currentSession(ctx, tx, c.Request, id.Subject)
		if errors.Is(err, sql.ErrNoRows) {
			return identity.ErrNoSession
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM local_sessions
			WHERE user_id = $1 AND id <> $2`, id.Subject, sessionID,
		); err != nil {
			return err
		}
		return resetDevices(ctx, tx, id.Subject)
	})
	if errors.Is(err, identity.ErrNoSession) {
		c.Redirect(http.StatusSeeOther, l.loginURL(l.settingsPath))
		return
	}
	if err != nil {
		l.profileFailure(c, err, "signing out other devices")
		return
	}
	l.setDevice(c, id.Subject, issued)
	web.Log(c).Info("signed out every other device", "user_id", id.Subject)
	l.backToSettings(c, "devices", "devices")
}
