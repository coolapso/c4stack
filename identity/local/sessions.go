package local

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

// sessionToken returns the request's session token, if it carries a well-formed one.
func (l *Local) sessionToken(r *http.Request) []byte {
	c, err := r.Cookie(l.cookieName(sessionCookie))
	if err != nil {
		return nil
	}
	return decodeToken(c.Value)
}

func (l *Local) setSessionCookie(w http.ResponseWriter, token []byte) {
	http.SetCookie(w, &http.Cookie{
		Name:     l.cookieName(sessionCookie),
		Value:    encodeToken(token),
		Path:     "/",
		MaxAge:   int(sessionLifetime.Seconds()),
		HttpOnly: true,
		Secure:   l.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (l *Local) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     l.cookieName(sessionCookie),
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   l.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// session is a session made by a sign-in, to be set in the browser once its
// transaction commits.
type session struct {
	id      string
	userID  string
	token   []byte
	created time.Time
}

// signIn runs the accounts side's SignedIn hook and starts a session for the user,
// inside tx, which locked the user already.
func (l *Local) signIn(ctx context.Context, tx *sql.Tx, r *http.Request, userID, address, method string) (session, error) {
	if l.hooks.SignedIn != nil {
		id := identity.Identity{
			Subject:       userID,
			Email:         address,
			EmailVerified: true,
			SignedInAt:    time.Now(),
			Method:        method,
		}
		if err := l.hooks.SignedIn(ctx, tx, id); err != nil {
			return session{}, err
		}
	}
	return l.startSession(ctx, tx, r, userID, method)
}

// startSession ends the session the browser had, if any, so a planted cookie never
// becomes a signed-in session, and creates a new one.
func (l *Local) startSession(ctx context.Context, tx *sql.Tx, r *http.Request, userID, method string) (session, error) {
	if old := l.sessionToken(r); old != nil {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM local_sessions
			WHERE token_hash = $1`, hashToken(old),
		); err != nil {
			return session{}, err
		}
	}
	s := session{userID: userID, token: newToken()}
	err := tx.QueryRowContext(ctx, `
		INSERT INTO local_sessions (user_id, token_hash, method, device)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at`, userID, hashToken(s.token), method, deviceLabel(r.UserAgent()),
	).Scan(&s.id, &s.created)
	return s, err
}

// currentSession returns the ID of the request's session and whether it signed in
// within identity.RecentSignIn, reading it inside tx after the user's lock, so a change
// that needs it is refused when the session ended meanwhile.
func (l *Local) currentSession(ctx context.Context, tx *sql.Tx, r *http.Request, userID string) (id string, recent bool, err error) {
	token := l.sessionToken(r)
	if token == nil {
		return "", false, sql.ErrNoRows
	}
	err = tx.QueryRowContext(ctx, `
		SELECT id, created_at >= now() - make_interval(secs => $3)
		FROM local_sessions
		WHERE token_hash = $1 AND user_id = $2`, hashToken(token), userID, identity.RecentSignIn.Seconds(),
	).Scan(&id, &recent)
	return id, recent, err
}

// finishSignIn sets the browser's session and device cookies after the sign-in's
// transaction committed.
func (l *Local) finishSignIn(c *gin.Context, s session) {
	l.setSessionCookie(c.Writer, s.token)
	l.setDevice(c, s.userID, s.created)
}

// device is a browser that signed in before. Its cookie signs nobody in: it only
// chooses which counters apply when the browser asks for a code for its user.
type device struct {
	UserID   string    `json:"u"`
	ID       string    `json:"d"`
	IssuedAt time.Time `json:"i"`
}

// readDevice returns the browser's device cookie, if it has a readable one.
func readDevice(c *gin.Context) (device, bool) {
	data, err := web.SealedCookie(c, deviceCookie)
	if err != nil {
		return device{}, false
	}
	var d device
	if json.Unmarshal(data, &d) != nil || d.UserID == "" || d.ID == "" {
		return device{}, false
	}
	return d, true
}

// setDevice gives the browser a device cookie for the user, issued at the database's
// time, which devices_since is compared with. A browser that already had one for the
// user keeps its ID, and so its counters.
func (l *Local) setDevice(c *gin.Context, userID string, issued time.Time) {
	d := device{UserID: userID, ID: newDeviceID(), IssuedAt: issued}
	if old, ok := readDevice(c); ok && old.UserID == userID {
		d.ID = old.ID
	}
	data, _ := json.Marshal(d)
	web.SetSealedCookie(c, deviceCookie, data, deviceLifetime)
}

// deviceLabel names a browser from its user agent, such as "Firefox on Linux", so
// people recognise their sessions. It is made only of fixed words, never of the header's
// own text.
func deviceLabel(ua string) string {
	has := func(s ...string) bool {
		for _, x := range s {
			if strings.Contains(ua, x) {
				return true
			}
		}
		return false
	}
	browser := ""
	switch {
	case has("Edg/", "EdgA/", "EdgiOS/"):
		browser = "Edge"
	case has("OPR/", "Opera"):
		browser = "Opera"
	case has("SamsungBrowser/"):
		browser = "Samsung Internet"
	case has("Firefox/", "FxiOS/"):
		browser = "Firefox"
	case has("Chrome/", "CriOS/", "Chromium/"):
		browser = "Chrome"
	case has("Safari/") && has("Version/"):
		browser = "Safari"
	}
	system := ""
	switch {
	case has("iPhone", "iPad", "iPod"):
		system = "iOS"
	case has("Android"):
		system = "Android"
	case has("CrOS"):
		system = "ChromeOS"
	case has("Windows"):
		system = "Windows"
	case has("Macintosh", "Mac OS X"):
		system = "macOS"
	case has("Linux"):
		system = "Linux"
	}
	switch {
	case browser != "" && system != "":
		return browser + " on " + system
	case browser != "":
		return browser
	case system != "":
		return "A browser on " + system
	}
	return "An unknown device"
}
