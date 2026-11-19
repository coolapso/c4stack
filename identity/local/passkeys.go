package local

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// The purposes of a passkey ceremony. A ceremony finishes only for its own purpose.
const (
	purposeLogin   = "login"
	purposeConfirm = "confirm"
	purposeAdd     = "add"
)

// ceremony is a passkey ceremony in progress, in the browser's sealed passkey cookie:
// go-webauthn's whole session data, and what it is for. Confirming and adding name the
// user and the session that started them.
type ceremony struct {
	Purpose   string               `json:"p"`
	UserID    string               `json:"u,omitempty"`
	SessionID string               `json:"s,omitempty"`
	Next      string               `json:"n,omitempty"`
	Data      webauthn.SessionData `json:"d"`
}

func saveCeremony(c *gin.Context, cer ceremony) {
	data, _ := json.Marshal(cer)
	web.SetSealedCookie(c, ceremonyCookie, data, ceremonyLifetime)
}

func readCeremony(c *gin.Context, purpose string) (ceremony, bool) {
	data, err := web.SealedCookie(c, ceremonyCookie)
	if err != nil {
		return ceremony{}, false
	}
	var cer ceremony
	if json.Unmarshal(data, &cer) != nil || cer.Purpose != purpose || cer.Data.Challenge == "" {
		return ceremony{}, false
	}
	return cer, true
}

// passkeyUser is a user as go-webauthn sees it. The user handle is the user's ID,
// never the address.
type passkeyUser struct {
	id      string
	address string
	creds   []webauthn.Credential
}

func (u *passkeyUser) WebAuthnID() []byte                         { return uuidBytes(u.id) }
func (u *passkeyUser) WebAuthnName() string                       { return u.address }
func (u *passkeyUser) WebAuthnDisplayName() string                { return u.address }
func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// storedPasskey is a passkey row.
type storedPasskey struct {
	id         string
	credential webauthn.Credential
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// passkeysOf returns a user's passkeys.
func passkeysOf(ctx context.Context, q querier, userID string) ([]storedPasskey, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, credential_id, public_key, sign_count, aaguid::text,
		       array_to_string(transports, ','), backup_eligible, backed_up
		FROM local_passkeys
		WHERE user_id = $1
		ORDER BY created_at`, userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedPasskey
	for rows.Next() {
		var p storedPasskey
		var aaguid sql.NullString
		var transports string
		var count int64
		cr := &p.credential
		if err := rows.Scan(&p.id, &cr.ID, &cr.PublicKey, &count, &aaguid, &transports,
			&cr.Flags.BackupEligible, &cr.Flags.BackupState); err != nil {
			return nil, err
		}
		cr.Flags.UserPresent, cr.Flags.UserVerified = true, true
		cr.Authenticator.SignCount = uint32(count)
		if aaguid.Valid {
			cr.Authenticator.AAGUID = uuidBytes(aaguid.String)
		}
		for _, t := range strings.Split(transports, ",") {
			if t != "" {
				cr.Transport = append(cr.Transport, protocol.AuthenticatorTransport(t))
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func credentials(passkeys []storedPasskey) []webauthn.Credential {
	out := make([]webauthn.Credential, len(passkeys))
	for i, p := range passkeys {
		out[i] = p.credential
	}
	return out
}

// passkeyError is a passkey request's failure, for the page's script. Retry tells the
// script to start a new ceremony quietly, as when another tab replaced this one's.
type passkeyError struct {
	status  int
	message string
	retry   bool
}

func (e *passkeyError) Error() string { return e.message }

var (
	errCeremonyGone = &passkeyError{status: http.StatusConflict, message: "This sign-in expired. Try again.", retry: true}
	errPasskeyGone  = &passkeyError{status: http.StatusUnauthorized, message: "This passkey can't sign in here any more. Sign in with a code, then add it again."}
	errPasskeyFails = &passkeyError{status: http.StatusUnauthorized, message: "Your passkey didn't work. Try again, or sign in with a code."}
)

func passkeyJSONError(c *gin.Context, err error) {
	var pe *passkeyError
	if !errors.As(err, &pe) {
		pe = &passkeyError{status: http.StatusInternalServerError, message: "Something went wrong on our side. Try again in a moment."}
	}
	c.JSON(pe.status, gin.H{"error": pe.message, "retry": pe.retry})
}

// passkeyOptions starts a passkey ceremony to sign in, or to confirm the signed-in
// user. Signing in is discoverable: the browser offers the passkeys it has, including
// in the address field's autofill.
func (l *Local) passkeyOptions(c *gin.Context) {
	var req struct {
		Purpose string `json:"purpose"`
		Next    string `json:"next"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bad request."})
		return
	}
	cer := ceremony{Purpose: req.Purpose, Next: l.next(req.Next)}
	var options *protocol.CredentialAssertion
	var data *webauthn.SessionData
	var err error
	switch req.Purpose {
	case purposeLogin:
		options, data, err = l.webauthn.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	case purposeConfirm:
		var user *passkeyUser
		user, cer.SessionID, err = l.signedInUser(c)
		if err != nil {
			passkeyJSONError(c, err)
			return
		}
		cer.UserID = user.id
		options, data, err = l.webauthn.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bad request."})
		return
	}
	if err != nil {
		web.Log(c).Error("starting a passkey ceremony failed", "error", err)
		passkeyJSONError(c, err)
		return
	}
	cer.Data = *data
	saveCeremony(c, cer)
	c.JSON(http.StatusOK, options)
}

// signedInUser returns the request's user with their passkeys, and its session's ID.
func (l *Local) signedInUser(c *gin.Context) (*passkeyUser, string, error) {
	token := l.sessionToken(c.Request)
	if token == nil {
		return nil, "", &passkeyError{status: http.StatusUnauthorized, message: "Sign in again to continue."}
	}
	ctx := c.Request.Context()
	user := &passkeyUser{}
	var sessionID string
	err := l.app.DB().QueryRowContext(ctx, `
		SELECT s.id, u.id, u.email
		FROM local_sessions s
		JOIN local_users u ON u.id = s.user_id
		WHERE s.token_hash = $1`, hashToken(token),
	).Scan(&sessionID, &user.id, &user.address)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", &passkeyError{status: http.StatusUnauthorized, message: "Sign in again to continue."}
	}
	if err != nil {
		return nil, "", err
	}
	passkeys, err := passkeysOf(ctx, l.app.DB(), user.id)
	if err != nil {
		return nil, "", err
	}
	user.creds = credentials(passkeys)
	return user, sessionID, nil
}

// passkeyFinish checks a passkey assertion and signs in.
func (l *Local) passkeyFinish(c *gin.Context) {
	body, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bad request."})
		return
	}
	purpose := purposeLogin
	if c.Query("purpose") == purposeConfirm {
		purpose = purposeConfirm
	}
	cer, ok := readCeremony(c, purpose)
	if !ok {
		passkeyJSONError(c, errCeremonyGone)
		return
	}
	web.ClearSealedCookie(c, ceremonyCookie)
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		passkeyJSONError(c, errPasskeyFails)
		return
	}
	if parsed.Response.CollectedClientData.Challenge != cer.Data.Challenge {
		// Another tab started a ceremony and replaced this one's cookie.
		passkeyJSONError(c, errCeremonyGone)
		return
	}
	userID := cer.UserID
	if handle := parsed.Response.UserHandle; len(handle) == 16 {
		if handleID := uuidString(handle); userID == "" {
			userID = handleID
		} else if handleID != userID {
			passkeyJSONError(c, errPasskeyFails)
			return
		}
	}
	if userID == "" {
		passkeyJSONError(c, errPasskeyFails)
		return
	}
	s, err := l.passkeySignIn(c, cer, userID, parsed)
	if err != nil {
		var pe *passkeyError
		if !errors.As(err, &pe) {
			if status, msg := l.failure(c, err, "signing in with a passkey"); msg != "" {
				c.JSON(status, gin.H{"error": msg})
				return
			}
		}
		passkeyJSONError(c, err)
		return
	}
	l.finishSignIn(c, s)
	web.Log(c).Info("signed in with a passkey", "user_id", s.userID, "session_id", s.id)
	c.JSON(http.StatusOK, gin.H{"redirect": cer.Next})
}

// passkeySignIn checks the assertion in one transaction: it locks the user its handle
// names, then the passkey, rechecks that the passkey still belongs to the user, applies
// the counter rule, records the challenge so the assertion can't be replayed, and
// starts a session.
func (l *Local) passkeySignIn(c *gin.Context, cer ceremony, userID string, parsed *protocol.ParsedCredentialAssertionData) (session, error) {
	ctx := c.Request.Context()
	var s session
	err := l.inTx(ctx, func(tx *sql.Tx) error {
		user := &passkeyUser{id: userID}
		err := tx.QueryRowContext(ctx, `
			SELECT email
			FROM local_users
			WHERE id = $1
			FOR NO KEY UPDATE`, userID,
		).Scan(&user.address)
		if errors.Is(err, sql.ErrNoRows) {
			return errPasskeyGone
		}
		if err != nil {
			return err
		}
		var passkeyID string
		err = tx.QueryRowContext(ctx, `
			SELECT id
			FROM local_passkeys
			WHERE credential_id = $1 AND user_id = $2
			FOR UPDATE`, parsed.RawID, userID,
		).Scan(&passkeyID)
		if errors.Is(err, sql.ErrNoRows) {
			return errPasskeyGone
		}
		if err != nil {
			return err
		}
		if cer.Purpose == purposeConfirm {
			if err := sessionExists(ctx, tx, cer.SessionID, userID); err != nil {
				return err
			}
		}
		passkeys, err := passkeysOf(ctx, tx, userID)
		if err != nil {
			return err
		}
		user.creds = credentials(passkeys)
		var stored uint32
		for _, p := range passkeys {
			if p.id == passkeyID {
				stored = p.credential.Authenticator.SignCount
			}
		}
		var cred *webauthn.Credential
		if cer.Purpose == purposeConfirm {
			cred, err = l.webauthn.ValidateLogin(user, cer.Data, parsed)
		} else {
			_, cred, err = l.webauthn.ValidatePasskeyLogin(func(_, _ []byte) (webauthn.User, error) {
				return user, nil
			}, cer.Data, parsed)
		}
		if err != nil {
			web.Log(c).Info("a passkey assertion failed validation", "user_id", userID, "passkey_id", passkeyID, "error", err)
			return errPasskeyFails
		}
		// The counter rule: a passkey that keeps a signature counter must report a
		// higher number each time. Both zero means it keeps none. Otherwise it may
		// have been copied.
		reported := parsed.Response.AuthenticatorData.Counter
		forward := reported > stored || (reported == 0 && stored == 0)
		if cred.Authenticator.CloneWarning || !forward {
			web.Log(c).Warn("a passkey's signature counter went back; it may have been copied, so it can't sign in",
				"user_id", userID, "passkey_id", passkeyID)
			return &passkeyError{status: http.StatusUnauthorized, message: "This passkey can't be used to sign in. Sign in with a code instead."}
		}
		if err := useChallenge(ctx, tx, cer.Data); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE local_passkeys
			SET sign_count = $2, backed_up = $3, last_used_at = now()
			WHERE id = $1`, passkeyID, int64(reported), parsed.Response.AuthenticatorData.Flags.HasBackupState(),
		); err != nil {
			return err
		}
		s, err = l.signIn(ctx, tx, c.Request, userID, user.address, methodPasskey)
		return err
	})
	return s, err
}

// useChallenge records an answered challenge, inside the transaction that uses it. A
// challenge recorded already is refused, so a captured assertion cannot be replayed
// with a copy of the ceremony's cookie.
func useChallenge(ctx context.Context, tx *sql.Tx, data webauthn.SessionData) error {
	sum := sha256.Sum256([]byte(data.Challenge))
	expires := data.Expires
	if expires.IsZero() {
		expires = time.Now().Add(ceremonyLifetime)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO local_used_challenges (challenge_hash, expires_at)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, sum[:], expires,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return errCeremonyGone
	}
	return nil
}

// sessionExists rechecks, inside a transaction, that the session that started a
// ceremony still exists.
func sessionExists(ctx context.Context, tx *sql.Tx, sessionID, userID string) error {
	var one int
	err := tx.QueryRowContext(ctx, `
		SELECT 1
		FROM local_sessions
		WHERE id = $1 AND user_id = $2`, sessionID, userID,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return &passkeyError{status: http.StatusUnauthorized, message: "Sign in again to continue."}
	}
	return err
}

// passkeyAddOptions starts adding a passkey for the signed-in user. It needs a recent
// sign-in. Registration excludes the user's passkeys, and asks for a discoverable
// credential, user verification and no attestation.
func (l *Local) passkeyAddOptions(c *gin.Context) {
	id, _ := identity.FromContext(c.Request.Context())
	if !id.Recent(time.Now()) {
		passkeyJSONError(c, l.notRecent())
		return
	}
	user, sessionID, err := l.signedInUser(c)
	if err != nil {
		passkeyJSONError(c, err)
		return
	}
	if user.id != id.Subject {
		passkeyJSONError(c, &passkeyError{status: http.StatusUnauthorized, message: "Sign in again to continue."})
		return
	}
	if len(user.creds) >= maxPasskeys {
		passkeyJSONError(c, tooManyPasskeys)
		return
	}
	options, data, err := l.webauthn.BeginRegistration(user, webauthn.WithExclusions(webauthn.Credentials(user.creds).CredentialDescriptors()))
	if err != nil {
		web.Log(c).Error("starting a passkey registration failed", "error", err)
		passkeyJSONError(c, err)
		return
	}
	saveCeremony(c, ceremony{Purpose: purposeAdd, UserID: user.id, SessionID: sessionID, Data: *data})
	c.JSON(http.StatusOK, options)
}

var tooManyPasskeys = &passkeyError{status: http.StatusConflict, message: "You have the most passkeys an account can have. Remove one to add another."}

func (l *Local) notRecent() *passkeyError {
	return &passkeyError{status: http.StatusForbidden, message: "For your security, confirm it's you first.", retry: false}
}

// passkeyAdd finishes adding a passkey. The finish must come from the session that
// started it, still within a recent sign-in, so a prompt left open past the window asks
// to confirm again.
func (l *Local) passkeyAdd(c *gin.Context) {
	id, _ := identity.FromContext(c.Request.Context())
	var req struct {
		Name       string          `json:"name"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bad request."})
		return
	}
	name, ok := passkeyName(req.Name)
	if !ok {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Name the passkey in 1 to 60 characters, such as “Work laptop”."})
		return
	}
	cer, ok := readCeremony(c, purposeAdd)
	if !ok || cer.UserID != id.Subject {
		passkeyJSONError(c, errCeremonyGone)
		return
	}
	web.ClearSealedCookie(c, ceremonyCookie)
	parsed, err := protocol.ParseCredentialCreationResponseBytes(req.Credential)
	if err != nil {
		passkeyJSONError(c, &passkeyError{status: http.StatusBadRequest, message: "Your device didn't create a passkey. Try again."})
		return
	}
	ctx := c.Request.Context()
	var notices []string
	err = l.inTx(ctx, func(tx *sql.Tx) error {
		user := &passkeyUser{id: id.Subject}
		err := tx.QueryRowContext(ctx, `
			SELECT email
			FROM local_users
			WHERE id = $1
			FOR NO KEY UPDATE`, id.Subject,
		).Scan(&user.address)
		if err != nil {
			return err
		}
		sessionID, recent, err := l.currentSession(ctx, tx, c.Request, id.Subject)
		if errors.Is(err, sql.ErrNoRows) || sessionID != cer.SessionID {
			return &passkeyError{status: http.StatusUnauthorized, message: "Sign in again to continue."}
		}
		if err != nil {
			return err
		}
		if !recent {
			return l.notRecent()
		}
		passkeys, err := passkeysOf(ctx, tx, id.Subject)
		if err != nil {
			return err
		}
		if len(passkeys) >= maxPasskeys {
			return tooManyPasskeys
		}
		user.creds = credentials(passkeys)
		cred, err := l.webauthn.CreateCredential(user, cer.Data, parsed)
		if err != nil {
			web.Log(c).Info("a passkey registration failed validation", "user_id", id.Subject, "error", err)
			return &passkeyError{status: http.StatusBadRequest, message: "Your device didn't create a usable passkey. Try again."}
		}
		if err := useChallenge(ctx, tx, cer.Data); err != nil {
			return err
		}
		var aaguid any
		if len(cred.Authenticator.AAGUID) == 16 {
			aaguid = uuidString(cred.Authenticator.AAGUID)
		}
		transports := make([]string, len(cred.Transport))
		for i, t := range cred.Transport {
			transports[i] = string(t)
		}
		var passkeyID string
		err = tx.QueryRowContext(ctx, `
			INSERT INTO local_passkeys (user_id, credential_id, public_key, sign_count, aaguid, transports,
			                            backup_eligible, backed_up, name)
			VALUES ($1, $2, $3, $4, $5, string_to_array($6, ','), $7, $8, $9)
			ON CONFLICT (credential_id) DO NOTHING
			RETURNING id`,
			id.Subject, cred.ID, cred.PublicKey, int64(cred.Authenticator.SignCount), aaguid, strings.Join(transports, ","),
			cred.Flags.BackupEligible, cred.Flags.BackupState, name,
		).Scan(&passkeyID)
		if errors.Is(err, sql.ErrNoRows) {
			return &passkeyError{status: http.StatusConflict, message: "This passkey was added already."}
		}
		if err != nil {
			return err
		}
		notice, err := l.queueNotice(ctx, tx, id.Subject, PasskeyAdded, user.address, noticeData{
			Passkey: name, Device: deviceLabel(c.Request.UserAgent()), At: time.Now(),
		})
		if err != nil {
			return err
		}
		notices = append(notices, notice)
		web.Log(c).Info("added a passkey", "user_id", id.Subject, "passkey_id", passkeyID)
		return nil
	})
	if err != nil {
		var pe *passkeyError
		if !errors.As(err, &pe) {
			web.Log(c).Error("adding a passkey failed", "error", err)
		}
		passkeyJSONError(c, err)
		return
	}
	l.deliver(ctx, notices...)
	c.JSON(http.StatusOK, gin.H{"redirect": l.settingsPath + "?done=passkey-added#passkeys"})
}

// passkeyName checks a passkey's name: 1 to maxPasskeyName characters, without control
// characters.
func passkeyName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" || utf8.RuneCountInString(name) > maxPasskeyName || !utf8.ValidString(name) {
		return "", false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return name, true
}
