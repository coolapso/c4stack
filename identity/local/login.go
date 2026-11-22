package local

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

// attemptLifetime is how long a browser remembers its code attempt: longer than the
// code, so the page can still say it expired and offer a new one.
const attemptLifetime = 3 * codeLifetime

// Mount adds the sign-in pages under r, rendered in layout:
//   - GET /login asks for an address, and offers the browser's passkeys.
//   - POST /login sends a code: to sign in if the address has a user, to sign up if not.
//     The page and the limits are the same either way.
//   - GET and POST /login/code take the code, and sign in.
//   - POST /login/code/resend sends a new code for the same attempt.
//   - GET and POST /login/confirm confirm a signed-in user, before a sensitive change.
//   - POST /login/passkey/options and /login/passkey sign in with a passkey.
//
// Every form needs the CSRF token and a matching Origin.
func (l *Local) Mount(r gin.IRouter, layout ui.Layout) {
	l.addAssets(layout.Assets)
	l.loginPath = joinPath(r, "/login")
	p := &loginPages{l: l, layout: layout}
	g := r.Group("/login", web.CSRF, noStore)
	g.GET("", p.form)
	g.POST("", p.send)
	g.GET("/code", p.codeForm)
	g.POST("/code", p.verify)
	g.POST("/code/resend", p.resend)
	g.GET("/confirm", p.confirmForm)
	g.POST("/confirm", p.confirmSend)
	g.POST("/passkey/options", l.passkeyOptions)
	g.POST("/passkey", l.passkeyFinish)
}

func (l *Local) addAssets(assets *ui.Assets) {
	if err := assets.Add("local", assetFiles); err != nil {
		panic(err)
	}
	l.assets = assets
}

// noStore keeps browsers and proxies from storing pages that carry personal data.
func noStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Next()
}

// joinPath returns the full path of a route registered on r.
func joinPath(r gin.IRouter, p string) string {
	if g, ok := r.(interface{ BasePath() string }); ok {
		return strings.TrimSuffix(g.BasePath(), "/") + p
	}
	return p
}

type loginPages struct {
	l      *Local
	layout ui.Layout
}

// attempt is a browser's code attempt, in its sealed signin cookie. Its codes carry its
// ID, so a new code ends the attempt's others even when an older copy of the cookie is
// replayed.
type attempt struct {
	ID      string    `json:"a"`
	CodeID  string    `json:"c"`
	Email   string    `json:"e"`
	Next    string    `json:"n"`
	SentAt  time.Time `json:"s"`
	Confirm bool      `json:"k,omitempty"`
}

func readAttempt(c *gin.Context) (attempt, bool) {
	data, err := web.SealedCookie(c, attemptCookie)
	if err != nil {
		return attempt{}, false
	}
	var a attempt
	if json.Unmarshal(data, &a) != nil || a.ID == "" || a.CodeID == "" || a.Email == "" || !localPath(a.Next) {
		return attempt{}, false
	}
	return a, true
}

func saveAttempt(c *gin.Context, a attempt) {
	data, _ := json.Marshal(a)
	web.SetSealedCookie(c, attemptCookie, data, attemptLifetime)
}

// next returns the local path to continue to, or the signed-in path.
func (l *Local) next(raw string) string {
	if localPath(raw) {
		return raw
	}
	return l.cfg.SignedInPath
}

func (l *Local) loginURL(next string) string {
	if next == "" || next == l.cfg.SignedInPath {
		return l.loginPath
	}
	return l.loginPath + "?" + url.Values{"next": {next}}.Encode()
}

func (p *loginPages) render(c *gin.Context, status int, title string, body templ.Component) {
	p.layout.Render(c, status, title, body)
}

func (p *loginPages) loginData(c *gin.Context, next, address string) loginData {
	return loginData{
		c:         c,
		action:    p.l.loginPath,
		next:      next,
		email:     address,
		scriptURL: p.l.assets.URL("local/passkey.js"),
		options:   p.l.loginPath + "/passkey/options",
		finish:    p.l.loginPath + "/passkey",
	}
}

// form asks for an address. A visitor with a valid session continues to next.
func (p *loginPages) form(c *gin.Context) {
	next := p.l.next(c.Query("next"))
	if _, err := p.l.Authenticate(c.Request); err == nil {
		c.Redirect(http.StatusSeeOther, next)
		return
	}
	p.render(c, http.StatusOK, "Sign in", loginPage(p.loginData(c, next, "")))
}

// send sends a code for the address the visitor entered.
func (p *loginPages) send(c *gin.Context) {
	next := p.l.next(c.PostForm("next"))
	raw := c.PostForm("email")
	address, ok := normalizeAddress(raw)
	if !ok {
		d := p.loginData(c, next, strings.TrimSpace(raw))
		d.err = "Enter your email address, such as ann@example.com."
		p.render(c, http.StatusUnprocessableEntity, "Sign in", loginPage(d))
		return
	}
	a := attempt{ID: newUUID(), Email: address, Next: next}
	if status, msg := p.l.sendCode(c, &a, false); msg != "" {
		d := p.loginData(c, next, address)
		d.err = msg
		p.render(c, status, "Sign in", loginPage(d))
		return
	}
	c.Redirect(http.StatusSeeOther, p.l.loginPath+"/code")
}

// tooSoonError means the attempt's last code is younger than resendAfter.
type tooSoonError struct {
	wait time.Duration
}

func (e *tooSoonError) Error() string { return "local: a new code too soon" }

// sendCode makes a code for the attempt's address, emails it, and saves the attempt in
// the browser. With replace, the new code ends the attempt's others, no sooner than
// resendAfter after the newest. It returns a status and a message for the page when it
// sent nothing. For an address Config.Allow refuses, the code is a decoy, counted as
// any other, and nothing is sent.
func (l *Local) sendCode(c *gin.Context, a *attempt, replace bool) (int, string) {
	ctx := c.Request.Context()
	dev, hasDevice := readDevice(c)
	codeID, code := newUUID(), newCode()
	refused := !l.allowed(a.Email)
	var userID sql.NullString
	err := l.inTx(ctx, func(tx *sql.Tx) error {
		var since time.Time
		err := tx.QueryRowContext(ctx, `
			SELECT id, devices_since
			FROM local_users
			WHERE email = $1`, a.Email,
		).Scan(&userID, &since)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		known := ""
		if hasDevice && userID.Valid && dev.UserID == userID.String && !dev.IssuedAt.Before(since) {
			known = dev.ID
		}
		if replace {
			if err := endAttemptCodes(ctx, tx, a.ID); err != nil {
				return err
			}
		}
		if err := l.countSend(ctx, tx, c, a.Email, known); err != nil {
			return err
		}
		hash := l.codeHash(codeID, code)
		if refused {
			hash = newToken() // never matches
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO local_codes (id, purpose, email, user_id, code_hash, decoy, device, attempt, expires_at)
			VALUES ($1, 'sign_in', $2, $3, $4, $5, $6, $7, now() + make_interval(secs => $8))`,
			codeID, a.Email, userID, hash, refused, known, a.ID, codeLifetime.Seconds(),
		)
		return err
	})
	var soon *tooSoonError
	if errors.As(err, &soon) {
		return http.StatusTooManyRequests, fmt.Sprintf("You can ask for a new code in %d seconds.", int(soon.wait.Seconds())+1)
	}
	if status, msg := l.failure(c, err, "sending a sign-in code"); msg != "" {
		return status, msg
	}
	if refused {
		a.CodeID, a.SentAt = codeID, l.now()
		saveAttempt(c, *a)
		return 0, ""
	}
	kind := SignUpCode
	if userID.Valid {
		kind = SignInCode
	}
	n := l.notice(kind, a.Email)
	n.Code = formatCode(code)
	if err := l.sendNow(ctx, n); err != nil {
		web.Log(c).Warn("the email provider refused a sign-in code", "error", err)
		if _, err := l.app.DB().ExecContext(ctx, `
			DELETE FROM local_codes
			WHERE id = $1`, codeID,
		); err != nil {
			web.Log(c).Error("deleting an unsent code failed", "error", err)
		}
		return http.StatusBadGateway, "We couldn't send a code right now. Try again in a moment."
	}
	a.CodeID, a.SentAt = codeID, l.now()
	saveAttempt(c, *a)
	return 0, ""
}

// endAttemptCodes deletes an attempt's codes, under a lock on the attempt, so concurrent
// requests for a new code take turns. It returns a tooSoonError when the newest is
// younger than resendAfter.
func endAttemptCodes(ctx context.Context, tx *sql.Tx, attemptID string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "local-attempt:"+attemptID); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `
		DELETE FROM local_codes
		WHERE attempt = $1 AND purpose = 'sign_in'
		RETURNING extract(epoch FROM created_at + make_interval(secs => $2) - now())`,
		attemptID, resendAfter.Seconds(),
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	var wait float64
	for rows.Next() {
		var w float64
		if err := rows.Scan(&w); err != nil {
			return err
		}
		wait = max(wait, w)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if wait > 0 {
		return &tooSoonError{wait: time.Duration(wait * float64(time.Second))}
	}
	return nil
}

// countSend checks and counts the limits on sending a sign-in code, in their fixed
// order: the app, the network, then the address's shared counters or, for a browser
// that signed in to the address's user before, its own.
func (l *Local) countSend(ctx context.Context, tx *sql.Tx, c *gin.Context, address, known string) error {
	if l.cfg.MaxCodesPerHour > 0 {
		if err := l.count(ctx, tx, counter{l.sendAppHour(), ""}); err != nil {
			return err
		}
	}
	if err := l.count(ctx, tx, counter{sendNetworkHour, network(c)}); err != nil {
		return err
	}
	if known != "" {
		if err := l.check(ctx, tx, counter{wrongDeviceDay, known}); err != nil {
			return err
		}
		return l.count(ctx, tx, counter{sendDeviceHour, known}, counter{sendDeviceDay, known})
	}
	// While wrong codes have paused the address, no code is sent through the shared
	// counters.
	if err := l.check(ctx, tx, counter{wrongAddressDay, address}); err != nil {
		return err
	}
	return l.count(ctx, tx, counter{sendAddressHour, address}, counter{sendAddressDay, address})
}

// failure turns an error from a sign-in step into a status and a message for the page,
// and logs what isn't the visitor's doing. It returns no message for a nil error.
func (l *Local) failure(c *gin.Context, err error, step string) (int, string) {
	var limited *limitedError
	switch {
	case err == nil:
		return 0, ""
	case errors.As(err, &limited):
		return http.StatusTooManyRequests, fmt.Sprintf(
			"Too many attempts. Try again after %s. A browser you've signed in on before, or a passkey, still works.",
			formatTime(limited.until))
	case errors.Is(err, identity.ErrRefused):
		web.Log(c).Warn("the accounts side refused a sign-in", "error", err)
		return http.StatusConflict, "This account can't be signed in to right now. Try again later, or contact support."
	}
	web.Log(c).Error("sign-in failed", "step", step, "error", err)
	return http.StatusInternalServerError, "Something went wrong on our side. Try again in a moment."
}

func (p *loginPages) codeData(c *gin.Context, a attempt) codeData {
	return codeData{
		c:       c,
		email:   a.Email,
		minutes: int(codeLifetime / time.Minute),
		action:  p.l.loginPath + "/code",
		resend:  p.l.loginPath + "/code/resend",
		restart: p.l.loginURL(a.Next),
		confirm: a.Confirm,
	}
}

func (p *loginPages) renderCode(c *gin.Context, status int, d codeData) {
	title := "Enter your code"
	if d.confirm {
		title = "Confirm it's you"
	}
	p.render(c, status, title, codePage(d))
}

// codeForm asks for the code. Without an attempt, it goes back to the start.
func (p *loginPages) codeForm(c *gin.Context) {
	a, ok := readAttempt(c)
	if !ok {
		c.Redirect(http.StatusSeeOther, p.l.loginPath)
		return
	}
	p.renderCode(c, http.StatusOK, p.codeData(c, a))
}

// verify checks the code and signs in.
func (p *loginPages) verify(c *gin.Context) {
	a, ok := readAttempt(c)
	if !ok {
		c.Redirect(http.StatusSeeOther, p.l.loginPath)
		return
	}
	d := p.codeData(c, a)
	code, ok := parseCode(c.PostForm("code"))
	if !ok {
		d.err = fmt.Sprintf("Enter the %d-digit code from the email.", codeDigits)
		p.renderCode(c, http.StatusUnprocessableEntity, d)
		return
	}
	res, err := p.l.verifyCode(c, a, code)
	if status, msg := p.l.failure(c, err, "checking a sign-in code"); msg != "" {
		d.err = msg
		p.renderCode(c, status, d)
		return
	}
	if res.message != "" {
		d.err = res.message
		p.renderCode(c, http.StatusUnprocessableEntity, d)
		return
	}
	p.l.finishSignIn(c, res.session)
	web.ClearSealedCookie(c, attemptCookie)
	web.Log(c).Info("signed in with a code", "user_id", res.session.userID, "session_id", res.session.id)
	c.Redirect(http.StatusSeeOther, a.Next)
}

// verifyResult is what checking a code did: a session to set, or a message for the
// page.
type verifyResult struct {
	session session
	message string
}

// verifyCode checks a code in one transaction. It locks the code's user, then the code,
// so two submissions of one code can't both succeed, and a code sent before a deletion
// or an address change finds its user gone or changed. A wrong code is counted and
// committed; a refused sign-in rolls everything back.
func (l *Local) verifyCode(c *gin.Context, a attempt, code string) (verifyResult, error) {
	ctx := c.Request.Context()
	var res verifyResult
	err := l.inTx(ctx, func(tx *sql.Tx) error {
		var userID sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT user_id
			FROM local_codes
			WHERE id = $1 AND purpose = 'sign_in'`, a.CodeID,
		).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			res.message = "This code has expired. Send a new one."
			return nil
		}
		if err != nil {
			return err
		}
		userAddress := ""
		if userID.Valid {
			err := tx.QueryRowContext(ctx, `
				SELECT email
				FROM local_users
				WHERE id = $1
				FOR NO KEY UPDATE`, userID.String,
			).Scan(&userAddress)
			if errors.Is(err, sql.ErrNoRows) {
				res.message = "This code is no longer valid. Send a new one."
				return nil
			}
			if err != nil {
				return err
			}
		}
		var sentTo, device string
		var hash []byte
		var decoy, expired bool
		var wrong int
		var codeUser sql.NullString
		err = tx.QueryRowContext(ctx, `
			SELECT email, user_id, code_hash, decoy, device, wrong_entries, expires_at <= now()
			FROM local_codes
			WHERE id = $1 AND purpose = 'sign_in'
			FOR UPDATE`, a.CodeID,
		).Scan(&sentTo, &codeUser, &hash, &decoy, &device, &wrong, &expired)
		if errors.Is(err, sql.ErrNoRows) {
			res.message = "This code has expired. Send a new one."
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case expired:
			res.message = "This code has expired. Send a new one."
			return deleteCode(ctx, tx, a.CodeID)
		case sentTo != a.Email || codeUser != userID || (userID.Valid && userAddress != sentTo):
			// The user's address changed since the code went out.
			res.message = "This code is no longer valid. Send a new one."
			return deleteCode(ctx, tx, a.CodeID)
		}
		wrongs := counter{wrongAddressDay, sentTo}
		if device != "" {
			wrongs = counter{wrongDeviceDay, device}
		}
		net := counter{wrongNetworkHour, network(c)}
		if err := l.check(ctx, tx, net, wrongs); err != nil {
			return err
		}
		if decoy || !hmac.Equal(l.codeHash(a.CodeID, code), hash) {
			res.message, err = l.wrongCode(ctx, tx, a.CodeID, wrong+1, net, wrongs, userID.String)
			return err
		}
		if err := deleteCode(ctx, tx, a.CodeID); err != nil {
			return err
		}
		if !l.allowed(sentTo) {
			// Allowed when the code went out, no longer.
			res.message = "This code is no longer valid. Send a new one."
			return nil
		}
		uid := userID.String
		if !userID.Valid {
			if uid, err = l.createUser(ctx, tx, sentTo); err != nil {
				return err
			}
		}
		res.session, err = l.signIn(ctx, tx, c.Request, uid, sentTo, methodCode)
		return err
	})
	return res, err
}

// wrongCode counts a wrong entry on the code and on the counters it was sent under, and
// returns the message for the page. A code ends at its fifth wrong entry. When the entry
// pauses the address's shared counters, the address's user gets a notice: once a
// window, because the counter reaches its limit once a window. The notice job sends it,
// never this request: an email sent only for an address with a user would make the
// answer slower for it, and tell anyone timing it that the address has an account.
func (l *Local) wrongCode(ctx context.Context, tx *sql.Tx, codeID string, wrong int, net, wrongs counter, userID string) (string, error) {
	if _, err := tx.ExecContext(ctx, `
		UPDATE local_codes
		SET wrong_entries = wrong_entries + 1
		WHERE id = $1`, codeID,
	); err != nil {
		return "", err
	}
	if _, _, err := l.add(ctx, tx, net); err != nil {
		return "", err
	}
	n, until, err := l.add(ctx, tx, wrongs)
	if err != nil {
		return "", err
	}
	if wrongs.limit == wrongAddressDay && n == wrongAddressDay.max {
		if userID == "" {
			err := tx.QueryRowContext(ctx, `
				SELECT id
				FROM local_users
				WHERE email = $1`, wrongs.subject,
			).Scan(&userID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return "", err
			}
		}
		if userID != "" {
			if _, err := l.queueNotice(ctx, tx, userID, CodesPaused, wrongs.subject, noticeData{At: time.Now(), Until: until}); err != nil {
				return "", err
			}
		}
	}
	if wrong >= wrongPerCode {
		return "Too many wrong codes. Send a new one.", deleteCode(ctx, tx, codeID)
	}
	left := wrongPerCode - wrong
	if left == 1 {
		return "That code isn't right. 1 try left.", nil
	}
	return fmt.Sprintf("That code isn't right. %d tries left.", left), nil
}

func deleteCode(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM local_codes
		WHERE id = $1`, id,
	)
	return err
}

// createUser finds or creates the user of an address that just confirmed a sign-up
// code, and locks it. A concurrent sign-up of the same address finds the other's user,
// because the address is unique.
func (l *Local) createUser(ctx context.Context, tx *sql.Tx, address string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `
		INSERT INTO local_users (email)
		VALUES ($1)
		ON CONFLICT (email) DO NOTHING
		RETURNING id`, address,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `
			SELECT id
			FROM local_users
			WHERE email = $1
			FOR NO KEY UPDATE`, address,
		).Scan(&id)
	}
	return id, err
}

// resend replaces the attempt's code with a new one.
func (p *loginPages) resend(c *gin.Context) {
	a, ok := readAttempt(c)
	if !ok {
		c.Redirect(http.StatusSeeOther, p.l.loginPath)
		return
	}
	d := p.codeData(c, a)
	if wait := resendAfter - p.l.now().Sub(a.SentAt); wait > 0 {
		d.err = fmt.Sprintf("You can ask for a new code in %d seconds.", int(wait.Seconds())+1)
		p.renderCode(c, http.StatusTooManyRequests, d)
		return
	}
	if status, msg := p.l.sendCode(c, &a, true); msg != "" {
		d.err = msg
		p.renderCode(c, status, d)
		return
	}
	d = p.codeData(c, a)
	d.info = "We sent a new code. Only the newest one works."
	p.renderCode(c, http.StatusOK, d)
}

// confirmForm asks a signed-in user to confirm it's them, with a passkey or a code to
// their address, before a change that needs a recent sign-in.
func (p *loginPages) confirmForm(c *gin.Context) {
	next := p.l.next(c.Query("next"))
	id, err := p.l.Authenticate(c.Request)
	if err != nil {
		c.Redirect(http.StatusSeeOther, p.l.loginURL(next))
		return
	}
	var passkeys int
	if err := p.l.app.DB().QueryRowContext(c.Request.Context(), `
		SELECT count(*)
		FROM local_passkeys
		WHERE user_id = $1`, id.Subject,
	).Scan(&passkeys); err != nil {
		web.Log(c).Error("counting passkeys failed", "error", err)
	}
	p.render(c, http.StatusOK, "Confirm it's you", confirmPage(p.confirmData(c, id.Email, next, passkeys > 0)))
}

func (p *loginPages) confirmData(c *gin.Context, address, next string, passkeys bool) confirmData {
	return confirmData{
		c:         c,
		email:     address,
		next:      next,
		action:    p.l.loginPath + "/confirm",
		passkeys:  passkeys,
		scriptURL: p.l.assets.URL("local/passkey.js"),
		options:   p.l.loginPath + "/passkey/options",
		finish:    p.l.loginPath + "/passkey",
	}
}

// confirmSend sends a code to the signed-in user's own address.
func (p *loginPages) confirmSend(c *gin.Context) {
	next := p.l.next(c.PostForm("next"))
	id, err := p.l.Authenticate(c.Request)
	if err != nil {
		c.Redirect(http.StatusSeeOther, p.l.loginURL(next))
		return
	}
	a := attempt{ID: newUUID(), Email: id.Email, Next: next, Confirm: true}
	if status, msg := p.l.sendCode(c, &a, false); msg != "" {
		d := p.confirmData(c, id.Email, next, false)
		d.err = msg
		p.render(c, status, "Confirm it's you", confirmPage(d))
		return
	}
	c.Redirect(http.StatusSeeOther, p.l.loginPath+"/code")
}
