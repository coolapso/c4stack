// Package local is C4Stack's own identity provider. It is passwordless: people sign up
// and sign in with an 8-digit code sent to their email address, or with a passkey. Its
// users, sessions and passkeys live in the app's own database, so development works
// offline, staging is just another database, and tests run the real flows.
//
// The provider is an identity.TxProvider: with the accounts module, a sign-up creates
// the user, its account and its session in one transaction, an address change moves the
// user's and the account's address together, and deleting the account deletes the user
// in the same transaction.
//
// docs/sign-in.md is the design: the flows, the limits, the tables, the emails, and the
// threats each part answers. Its security values are fixed here, as
// identity.RecentSignIn is; an app can only add a ceiling on the codes it sends.
package local

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/email"
	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/ui"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations create the provider's tables. List them after accounts.Migrations.
var Migrations = app.Migrations{Namespace: "local", Files: sub(migrationFiles, "migrations")}

//go:embed assets
var embeddedAssets embed.FS

// assetFiles are served under the "local" prefix. It is one value, so mounting the
// sign-in pages and the profile UI adds the files once.
var assetFiles = sub(embeddedAssets, "assets")

func sub(fsys fs.FS, dir string) fs.FS {
	s, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return s
}

// The security values. They are not configuration: an app must not be able to weaken
// them by accident.
const (
	codeDigits        = 8
	codeLifetime      = 10 * time.Minute
	wrongPerCode      = 5
	resendAfter       = 30 * time.Second
	sessionIdle       = 30 * 24 * time.Hour
	sessionLifetime   = 90 * 24 * time.Hour
	lastSeenEvery     = time.Hour
	deviceLifetime    = 365 * 24 * time.Hour
	ceremonyLifetime  = 5 * time.Minute
	maxPasskeys       = 20
	maxPasskeyName    = 60
	noticeRetention   = 3 * 24 * time.Hour
	maxAddressLength  = 254
	sendTimeout       = 30 * time.Second
	defaultSettings   = "/account/settings"
	providerName      = "local"
	sessionCookie     = "session"
	attemptCookie     = "signin"
	ceremonyCookie    = "passkey"
	deviceCookie      = "device"
	methodCode        = "code"
	methodPasskey     = "passkey"
	purposeSignIn     = "sign_in"
	purposeNewAddress = "new_email"
)

// Config configures the provider.
type Config struct {
	// AppName names the app in emails and to passkeys, such as "Acme". Required.
	AppName string
	// PublicURL is the app's canonical origin, as in web.Config. Links, the emails'
	// site and passkeys' origin come from it. Required.
	PublicURL string
	// Origins are the further origins passkeys work on: the same list the app gives
	// web.Config.
	Origins []string
	// RPID is the domain passkeys belong to. Zero means PublicURL's host. It must never
	// change once passkeys exist. An app on app.acme.example that wants passkeys to work
	// across acme.example sets it to acme.example.
	RPID string
	// SecretKey is the app's secret key, the same value as web.Config's. The provider
	// derives its own keys from it, for codes and the limits' counters.
	SecretKey string
	// Sender sends the codes and the security notices. Required: sign-in depends on it.
	Sender email.Sender
	// SignedInPath is where sign-in goes without a next path. Zero means "/".
	SignedInPath string
	// MaxCodesPerHour is a ceiling on the codes the whole app sends in an hour, to
	// protect the sending domain. Zero means no ceiling. It only adds a limit: the
	// others are fixed.
	MaxCodesPerHour int
	// Message writes the emails. Zero uses plain defaults that name AppName. An email
	// always goes to Notice.To, whatever the message's To says.
	Message func(Notice) email.Message
}

// Local is the provider. It implements identity.TxProvider, and the profile UI that the
// accounts module embeds in its settings page.
type Local struct {
	app      *app.App
	cfg      Config
	secure   bool
	origin   string
	site     string
	codeKey  []byte
	limitKey []byte
	webauthn *webauthn.WebAuthn
	hooks    identity.AccountHooks
	// now is the clock for the times a browser keeps, such as when it last asked for
	// a code. Times in the database come from the database.
	now func() time.Time

	// loginPath is set by Mount, settingsPath and the profile's layout by MountProfile.
	loginPath    string
	settingsPath string
	assets       *ui.Assets
	layout       ui.Layout
}

var _ identity.TxProvider = (*Local)(nil)

// New validates cfg and returns the provider. It registers the operator commands and
// the background jobs: security notices every minute, and the cleanup of expired rows
// every hour. It does not touch the database.
func New(a *app.App, cfg Config) (*Local, error) {
	if err := a.Require(Migrations); err != nil {
		return nil, err
	}
	l := &Local{app: a, cfg: cfg, loginPath: "/login", settingsPath: defaultSettings, now: time.Now}
	if err := l.validate(); err != nil {
		return nil, err
	}
	if l.cfg.Message == nil {
		l.cfg.Message = defaultMessage
	}
	var err error
	if l.codeKey, err = hkdf.Key(sha256.New, []byte(cfg.SecretKey), nil, "local-code", 32); err != nil {
		return nil, err
	}
	if l.limitKey, err = hkdf.Key(sha256.New, []byte(cfg.SecretKey), nil, "local-limit", 32); err != nil {
		return nil, err
	}
	origins := []string{l.origin}
	for _, o := range cfg.Origins {
		canonical, _, _, _ := canonicalOrigin(o)
		origins = append(origins, canonical)
	}
	l.webauthn, err = webauthn.New(&webauthn.Config{
		RPID:          l.cfg.RPID,
		RPDisplayName: cfg.AppName,
		RPOrigins:     origins,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		AttestationPreference: protocol.PreferNoAttestation,
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyLifetime, TimeoutUVD: ceremonyLifetime},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyLifetime, TimeoutUVD: ceremonyLifetime},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("local: passkeys: %w", err)
	}
	l.registerCommands()
	l.registerJobs()
	return l, nil
}

func (l *Local) validate() error {
	cfg := &l.cfg
	if strings.TrimSpace(cfg.AppName) == "" {
		return errors.New("local: AppName is required")
	}
	if cfg.Sender == nil {
		return errors.New("local: an email sender is required; sign-in sends codes by email")
	}
	if len(cfg.SecretKey) < 32 {
		return errors.New("local: the secret key must be at least 32 bytes")
	}
	if cfg.MaxCodesPerHour < 0 {
		return errors.New("local: MaxCodesPerHour cannot be negative")
	}
	origin, host, secure, err := canonicalOrigin(cfg.PublicURL)
	if err != nil {
		return err
	}
	l.origin, l.site, l.secure = origin, host, secure
	hosts := []string{hostOnly(host)}
	for _, raw := range cfg.Origins {
		_, h, sec, err := canonicalOrigin(raw)
		if err != nil {
			return err
		}
		if sec != secure {
			return fmt.Errorf("local: origin %q must use the public URL's scheme", raw)
		}
		hosts = append(hosts, hostOnly(h))
	}
	if cfg.RPID == "" {
		cfg.RPID = hostOnly(host)
	}
	if cfg.RPID != strings.ToLower(cfg.RPID) || strings.ContainsAny(cfg.RPID, ":/[]") || net.ParseIP(cfg.RPID) != nil {
		return fmt.Errorf("local: RPID %q must be a bare domain: lower case, without a scheme, port or path", cfg.RPID)
	}
	for _, h := range hosts {
		if h != cfg.RPID && !strings.HasSuffix(h, "."+cfg.RPID) {
			return fmt.Errorf("local: the host %q is not %q or under it, so passkeys could not work there", h, cfg.RPID)
		}
	}
	if cfg.SignedInPath == "" {
		cfg.SignedInPath = "/"
	}
	if !localPath(cfg.SignedInPath) {
		return errors.New("local: the signed-in path must be a local path")
	}
	return nil
}

// canonicalOrigin validates an origin, as web.Config does, and returns it, its host as
// the Host header carries it, and whether it is HTTPS.
func canonicalOrigin(raw string) (origin, host string, secure bool, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", false, fmt.Errorf("local: %q is not an absolute URL", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", "", false, fmt.Errorf("local: %q must be an origin, without a path, query or credentials", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	host = strings.ToLower(u.Host)
	if (scheme == "https" && strings.HasSuffix(host, ":443")) || (scheme == "http" && strings.HasSuffix(host, ":80")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	switch scheme {
	case "https":
		secure = true
	case "http":
		if !loopback(u.Hostname()) {
			return "", "", false, fmt.Errorf("local: %q must use https unless the host is loopback", raw)
		}
	default:
		return "", "", false, fmt.Errorf("local: %q must use http or https", raw)
	}
	return scheme + "://" + host, host, secure, nil
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.Trim(h, "[]")
	}
	return hostport
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// localPath reports whether p is a path on this site, not a URL to another one.
func localPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return false
	}
	// Browsers read a backslash as a slash, and ignore tabs and newlines, while
	// http.Redirect cleans dot segments: "/../\evil.example" would leave as
	// "/\evil.example", which is another site. Without them, no cleaned path starts
	// with two separators.
	for _, r := range p {
		if r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Name returns "local".
func (l *Local) Name() string { return providerName }

// UseAccounts receives the accounts module's hooks.
func (l *Local) UseAccounts(h identity.AccountHooks) { l.hooks = h }

// cookieName adds the __Host- prefix on HTTPS, as web does for its cookies.
func (l *Local) cookieName(name string) string {
	if l.secure {
		return "__Host-" + name
	}
	return name
}

// Authenticate finds the request's session. A session ends sessionIdle after it was
// last used, and sessionLifetime after its sign-in whatever happens.
func (l *Local) Authenticate(r *http.Request) (identity.Identity, error) {
	token := l.sessionToken(r)
	if token == nil {
		return identity.Identity{}, identity.ErrNoSession
	}
	ctx := r.Context()
	var id identity.Identity
	var sessionID string
	var stale bool
	err := l.app.DB().QueryRowContext(ctx, `
		SELECT s.id, u.id, u.email, s.method, s.created_at,
		       s.last_seen_at < now() - make_interval(secs => $4)
		FROM local_sessions s
		JOIN local_users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		    AND s.created_at > now() - make_interval(secs => $2)
		    AND s.last_seen_at > now() - make_interval(secs => $3)`,
		hashToken(token), sessionLifetime.Seconds(), sessionIdle.Seconds(), lastSeenEvery.Seconds(),
	).Scan(&sessionID, &id.Subject, &id.Email, &id.Method, &id.SignedInAt, &stale)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Identity{}, identity.ErrNoSession
	}
	if err != nil {
		return identity.Identity{}, fmt.Errorf("local: session lookup: %w", err)
	}
	id.EmailVerified = true
	if stale {
		// The idle end can come up to lastSeenEvery late, in exchange for a write an
		// hour instead of one a request.
		if _, err := l.app.DB().ExecContext(ctx, `
			UPDATE local_sessions
			SET last_seen_at = now()
			WHERE id = $1`, sessionID,
		); err != nil {
			l.app.Log().Warn("recording a session's use failed", "session_id", sessionID, "error", err)
		}
	}
	return id, nil
}

// Logout ends the request's session and clears its cookie. The cookie is cleared even
// when the database fails; the error reports that case.
func (l *Local) Logout(w http.ResponseWriter, r *http.Request) error {
	token := l.sessionToken(r)
	l.clearSessionCookie(w)
	if token == nil {
		return nil
	}
	_, err := l.app.DB().ExecContext(r.Context(), `
		DELETE FROM local_sessions
		WHERE token_hash = $1`, hashToken(token),
	)
	return err
}

// Delete deletes the signed-in user. With accounts, deletion goes through DeleteTx
// instead, inside the transaction that deletes the account.
func (l *Local) Delete(ctx context.Context, r *http.Request) error {
	token := l.sessionToken(r)
	if token == nil {
		return identity.ErrNoSession
	}
	var userID string
	err := l.app.DB().QueryRowContext(ctx, `
		SELECT user_id
		FROM local_sessions
		WHERE token_hash = $1`, hashToken(token),
	).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.ErrNoSession
	}
	if err != nil {
		return err
	}
	return l.inTx(ctx, func(tx *sql.Tx) error {
		return l.DeleteTx(ctx, tx, userID)
	})
}

// SessionTx locks the user of the request's session inside tx, and returns the session
// as it is once locked: identity.ErrNoSession if it ended meanwhile.
func (l *Local) SessionTx(ctx context.Context, tx *sql.Tx, r *http.Request) (identity.Identity, error) {
	token := l.sessionToken(r)
	if token == nil {
		return identity.Identity{}, identity.ErrNoSession
	}
	var userID string
	err := tx.QueryRowContext(ctx, `
		SELECT user_id
		FROM local_sessions
		WHERE token_hash = $1`, hashToken(token),
	).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Identity{}, identity.ErrNoSession
	}
	if err != nil {
		return identity.Identity{}, err
	}
	id := identity.Identity{Subject: userID, EmailVerified: true}
	err = tx.QueryRowContext(ctx, `
		SELECT email
		FROM local_users
		WHERE id = $1
		FOR NO KEY UPDATE`, userID,
	).Scan(&id.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Identity{}, identity.ErrNoSession
	}
	if err != nil {
		return identity.Identity{}, err
	}
	err = tx.QueryRowContext(ctx, `
		SELECT method, created_at
		FROM local_sessions
		WHERE token_hash = $1 AND user_id = $2
		    AND created_at > now() - make_interval(secs => $3)
		    AND last_seen_at > now() - make_interval(secs => $4)`,
		hashToken(token), userID, sessionLifetime.Seconds(), sessionIdle.Seconds(),
	).Scan(&id.Method, &id.SignedInAt)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Identity{}, identity.ErrNoSession
	}
	if err != nil {
		return identity.Identity{}, err
	}
	return id, nil
}

// DeleteTx deletes a user, with its sessions, passkeys and codes, inside tx. It locks
// the user first, so a sign-in or a change that started before waits for the deletion
// and then finds the user gone, or finishes first and is deleted with it.
func (l *Local) DeleteTx(ctx context.Context, tx *sql.Tx, subject string) error {
	if !validUUID(subject) {
		return nil
	}
	var id string
	err := tx.QueryRowContext(ctx, `
		SELECT id
		FROM local_users
		WHERE id = $1
		FOR UPDATE`, subject,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM local_users
		WHERE id = $1`, id,
	); err != nil {
		return err
	}
	l.app.Log().Info("deleted a user", "user_id", id)
	return nil
}

// ConfirmURL is the page where a signed-in user confirms it's them, with a passkey or a
// code, and then continues to next. The accounts delete page links there.
func (l *Local) ConfirmURL(next string) string {
	return l.loginPath + "/confirm?" + url.Values{"next": {next}}.Encode()
}

// inTx runs fn in a transaction, and commits it when fn returns nil.
func (l *Local) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := l.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
