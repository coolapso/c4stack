package accounts

import (
	"errors"
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

// profileUI is the optional profile management a provider can offer. The settings
// page embeds it.
type profileUI interface {
	MountProfile(r gin.IRouter, layout ui.Layout, logoutPath string, emailChanged func(c *gin.Context, email string) error)
	ProfileCSP() web.CSP
	ProfileSection(c *gin.Context) templ.Component
}

// Mount adds the default account pages under r, which must use RequireSignIn:
//   - GET "" is the settings page: the email address, the provider's profile UI when
//     it has one, signing out and deleting the account.
//   - POST "/delete" deletes the account.
//   - POST "/logout" signs out and continues to a local "next" path, or "/".
func (s *Accounts) Mount(r gin.IRouter, layout ui.Layout) {
	base := basePath(r)
	p := pages{s: s, layout: layout, base: base}
	profile, ok := s.idp.(profileUI)
	if ok {
		r = r.Group("", web.AllowCSP(profile.ProfileCSP()))
		profile.MountProfile(r, layout, base+"/logout", func(c *gin.Context, email string) error {
			return s.setEmail(c.Request.Context(), Current(c).ID, email)
		})
		p.profile = profile
	}
	r.GET("", p.settings)
	r.POST("/delete", p.delete)
	r.POST("/logout", p.logout)
}

func basePath(r gin.IRouter) string {
	if g, ok := r.(interface{ BasePath() string }); ok {
		return strings.TrimSuffix(g.BasePath(), "/")
	}
	return ""
}

type pages struct {
	s       *Accounts
	layout  ui.Layout
	base    string
	profile profileUI
}

// settingsView is what the settings page shows.
type settingsView struct {
	c           *gin.Context
	account     Account
	profile     templ.Component
	base        string
	recent      bool
	signInAgain string
	// deleteError reopens the delete dialog with a message.
	deleteError string
	// notice is shown at the top, for example after a failed deletion.
	notice string
}

func (p pages) view(c *gin.Context) settingsView {
	id, _ := identity.FromContext(c.Request.Context())
	settings := p.base
	if settings == "" {
		settings = "/"
	}
	v := settingsView{
		c:           c,
		account:     Current(c),
		base:        p.base,
		recent:      id.Recent(time.Now()),
		signInAgain: p.s.cfg.LoginPath + "?next=" + url.QueryEscape(settings),
	}
	if p.profile != nil {
		v.profile = p.profile.ProfileSection(c)
	}
	return v
}

func (p pages) settings(c *gin.Context) {
	p.layout.Render(c, http.StatusOK, "Account settings", settingsPage(p.view(c)))
}

func (p pages) delete(c *gin.Context) {
	v := p.view(c)
	if c.PostForm("confirmation") != "DELETE" {
		v.deleteError = "Type DELETE to confirm."
		p.layout.Render(c, http.StatusUnprocessableEntity, "Account settings", settingsPage(v))
		return
	}
	if !v.recent {
		v.notice = "To delete your account, sign in again first."
		p.layout.Render(c, http.StatusForbidden, "Account settings", settingsPage(v))
		return
	}
	err := p.s.Delete(c)
	switch {
	case errors.Is(err, ErrNotDeleted):
		web.Log(c).Warn("account deletion failed at the identity provider", "error", err)
		v.notice = "Your account could not be deleted right now. Nothing was removed. Try again in a moment."
		p.layout.Render(c, http.StatusBadGateway, "Account settings", settingsPage(v))
	case errors.Is(err, ErrCleanupPending):
		p.layout.Render(c, http.StatusInternalServerError, "Account deleted", deletedPage(true))
	case err != nil:
		web.Log(c).Error("account deletion failed", "error", err)
		c.AbortWithStatus(http.StatusInternalServerError)
	default:
		p.layout.Render(c, http.StatusOK, "Account deleted", deletedPage(false))
	}
}

func (p pages) logout(c *gin.Context) {
	if err := p.s.idp.Logout(c.Writer, c.Request); err != nil {
		web.Log(c).Warn("signing out at the identity provider failed; the browser session was cleared", "error", err)
	}
	next := c.PostForm("next")
	if !localPath(next) {
		next = "/"
	}
	if c.GetHeader("HX-Request") == "true" {
		c.Header("HX-Redirect", next)
		c.Status(http.StatusNoContent)
		return
	}
	c.Redirect(http.StatusSeeOther, next)
}
