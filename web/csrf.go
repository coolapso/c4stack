package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// CSRFField is the form field that carries the CSRF token. HTMX and scripts can send
// the token in the CSRFHeader header instead.
const (
	CSRFField  = "csrf_token"
	CSRFHeader = "X-CSRF-Token"
)

// issueCSRFCookie makes sure the browser holds a CSRF cookie. The cookie holds a random
// value; the token forms submit is that value's HMAC, so an attacker who can plant a
// cookie still cannot produce a matching token.
func (s *settings) issueCSRFCookie(c *gin.Context) {
	st := current(c)
	if v, err := c.Cookie(s.csrfCookie); err == nil && validCSRFValue(v) {
		st.csrfValue = v
	} else {
		raw := make([]byte, 32)
		_, _ = rand.Read(raw)
		st.csrfValue = base64.RawURLEncoding.EncodeToString(raw)
		http.SetCookie(c.Writer, &http.Cookie{
			Name:     s.csrfCookie,
			Value:    st.csrfValue,
			Path:     "/",
			HttpOnly: true,
			Secure:   s.secure,
			SameSite: http.SameSiteLaxMode,
		})
	}
	c.Next()
}

func validCSRFValue(v string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(v)
	return err == nil && len(raw) == 32
}

func (s *settings) csrfToken(value string) string {
	mac := hmac.New(sha256.New, s.csrfKey)
	mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// CSRFToken returns the token that forms on this page must submit.
func CSRFToken(c *gin.Context) string {
	st := current(c)
	return st.settings.csrfToken(st.csrfValue)
}

// CSRF rejects state-changing requests that lack a valid CSRF token or come from
// another origin. Use it on public routes with forms; accounts.RequireSignIn already
// includes it. API-key routes and webhooks must not use it.
//
// Other middleware can call CSRF directly and check c.IsAborted afterwards.
func CSRF(c *gin.Context) {
	if !unsafeMethod(c.Request.Method) {
		return
	}
	st := current(c)
	if !st.settings.sameOrigin(c.Request.Header.Get("Origin")) {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	token := c.GetHeader(CSRFHeader)
	if token == "" {
		if err := parseForm(c.Request); err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				c.AbortWithStatus(http.StatusRequestEntityTooLarge)
			} else {
				c.AbortWithStatus(http.StatusBadRequest)
			}
			return
		}
		token = c.Request.PostFormValue(CSRFField)
	}
	want := st.settings.csrfToken(st.csrfValue)
	if !hmac.Equal([]byte(token), []byte(want)) {
		c.AbortWithStatus(http.StatusForbidden)
	}
}

// parseForm parses a urlencoded or multipart body. ParseMultipartForm alone would hide
// a body-limit error behind http.ErrNotMultipart for urlencoded forms.
func parseForm(r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		return r.ParseMultipartForm(32 << 10)
	}
	return r.ParseForm()
}

// sameOrigin reports whether an Origin header names one of the app's origins. Browsers
// send a bare origin; anything with a path is not one.
func (s *settings) sameOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Path != "" {
		return false
	}
	canonical, _, _, err := canonicalOrigin(origin)
	return err == nil && s.origins[canonical]
}
