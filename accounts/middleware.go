package accounts

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

const accountKey = "c4stack/accounts"

// RequireSignIn protects browser routes that need a signed-in user. It checks CSRF on
// state-changing requests, because sign-in is cookie-based, then validates the session
// and finds the account, creating it on first sign-in. Visitors without a session are
// sent to the login page. Responses are marked not to be stored.
func (s *Accounts) RequireSignIn(c *gin.Context) {
	web.CSRF(c)
	if c.IsAborted() {
		return
	}
	id, err := s.idp.Authenticate(c.Request)
	if errors.Is(err, identity.ErrNoSession) {
		s.toLogin(c)
		return
	}
	if err != nil {
		web.Log(c).Error("session validation failed", "error", err)
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	account, err := s.resolve(c, id)
	if errors.Is(err, errDeleted) {
		_ = s.idp.Logout(c.Writer, c.Request)
		s.toLogin(c)
		return
	}
	if err != nil {
		web.Log(c).Error("account lookup failed", "error", err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Set(accountKey, account)
	c.Request = c.Request.WithContext(identity.NewContext(c.Request.Context(), id))
}

func (s *Accounts) toLogin(c *gin.Context) {
	target := s.cfg.LoginPath
	if c.Request.Method == http.MethodGet {
		target += "?next=" + url.QueryEscape(c.Request.URL.RequestURI())
	}
	if c.GetHeader("HX-Request") == "true" {
		c.Header("HX-Redirect", target)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	c.Redirect(http.StatusSeeOther, target)
	c.Abort()
}

// Current returns the signed-in account. It panics on routes without RequireSignIn,
// which is a programming error.
func Current(c *gin.Context) Account {
	v, ok := c.Get(accountKey)
	if !ok {
		panic("accounts: Current needs a route behind RequireSignIn")
	}
	return v.(Account)
}
