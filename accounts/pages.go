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

// The pages' paths, relative to the router Mount gets.
const (
	logoutPath       = "/logout"
	unverifiedPath   = "/auth/email-unverified"
	settingsPath     = "/account/settings"
	deletePath       = "/account/delete"
	deletedPath      = "/account/deleted"
	emailChangedPath = "/account/email-changed"
)

// profileUI is the optional profile management a provider can offer. The settings
// page embeds it, and its routes go under the settings page's path.
type profileUI interface {
	MountProfile(r gin.IRouter, layout ui.Layout, logoutPath, emailChangedPath string, emailChanged func(c *gin.Context, email string) error)
	ProfileCSP() web.CSP
	ProfileSection(c *gin.Context) templ.Component
}

// Mount adds the account pages to r, usually the app's engine:
//   - POST /logout signs out and continues to a local "next" path, or "/".
//   - GET /auth/email-unverified asks an identity without a verified email to confirm it.
//   - GET /account/settings shows the email address, the provider's profile UI when it
//     has one, signing out and account deletion.
//   - GET and POST /account/delete confirm and delete the account.
//   - GET /account/deleted and /account/email-changed confirm those changes after the
//     session ended.
//
// The /account pages use RequireSignIn. Apps add their own signed-in pages in a group
// that uses it too.
func (s *Accounts) Mount(r gin.IRouter, layout ui.Layout) {
	if s.idp == nil {
		panic("accounts: Mount needs an identity provider")
	}
	root := basePath(r)
	p := pages{s: s, layout: layout, root: root}
	r.POST(logoutPath, web.CSRF, p.logout)
	r.GET(unverifiedPath, p.unverified)
	r.GET(deletedPath, p.deleted)
	r.GET(emailChangedPath, p.emailChanged)

	account := r.Group("", s.RequireSignIn)
	settings := account.Group(settingsPath)
	if profile, ok := s.idp.(profileUI); ok {
		settings = settings.Group("", web.AllowCSP(profile.ProfileCSP()))
		profile.MountProfile(settings, layout, root+logoutPath, root+emailChangedPath, func(c *gin.Context, email string) error {
			id, _ := identity.FromContext(c.Request.Context())
			return s.setEmail(c.Request.Context(), Current(c).ID, id.Subject, email)
		})
		p.profile = profile
	}
	settings.GET("", p.settings)
	account.GET(deletePath, p.deletePage)
	account.POST(deletePath, p.delete)
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
	root    string
	profile profileUI
}

// settingsView is what the settings and delete pages show.
type settingsView struct {
	c       *gin.Context
	account Account
	profile templ.Component
	root    string
	recent  bool
	// signInAgain is the sign-out form's next path when deletion needs a fresh sign-in.
	signInAgain string
	// deleteError reopens the delete form with a message.
	deleteError string
	// notice is shown at the top, for example after a failed deletion.
	notice string
}

func (p pages) view(c *gin.Context) settingsView {
	id, _ := identity.FromContext(c.Request.Context())
	return settingsView{
		c:           c,
		account:     Current(c),
		root:        p.root,
		recent:      id.Recent(time.Now()),
		signInAgain: p.s.cfg.LoginPath + "?next=" + url.QueryEscape(p.root+deletePath),
	}
}

func (p pages) settings(c *gin.Context) {
	v := p.view(c)
	if p.profile != nil {
		v.profile = p.profile.ProfileSection(c)
	}
	p.layout.Render(c, http.StatusOK, "Account settings", settingsPage(v))
}

func (p pages) deletePage(c *gin.Context) {
	p.layout.Render(c, http.StatusOK, "Delete account", deletePage(p.view(c)))
}

func (p pages) delete(c *gin.Context) {
	v := p.view(c)
	if c.PostForm("confirmation") != "DELETE" {
		v.deleteError = "Type DELETE to confirm."
		p.layout.Render(c, http.StatusUnprocessableEntity, "Delete account", deletePage(v))
		return
	}
	if !v.recent {
		p.layout.Render(c, http.StatusForbidden, "Delete account", deletePage(v))
		return
	}
	err := p.s.Delete(c)
	switch {
	case errors.Is(err, ErrNotDeleted):
		web.Log(c).Warn("account deletion failed at the identity provider", "error", err)
		v.notice = "Your account could not be deleted right now. Nothing was removed. Try again in a moment."
		p.layout.Render(c, http.StatusBadGateway, "Delete account", deletePage(v))
	case errors.Is(err, ErrCleanupPending):
		p.layout.Render(c, http.StatusInternalServerError, "Account deleted", deletedPage(p.root, true))
	case err != nil:
		web.Log(c).Error("account deletion failed", "error", err)
		c.AbortWithStatus(http.StatusInternalServerError)
	default:
		c.Redirect(http.StatusSeeOther, p.root+deletedPath)
	}
}

func (p pages) logout(c *gin.Context) {
	if err := p.s.idp.Logout(c.Writer, c.Request); err != nil {
		web.Log(c).Warn("signing out at the identity provider failed; the browser session was cleared", "error", err)
	}
	next := c.PostForm("next")
	if !localPath(next) {
		next = p.root + "/"
	}
	if c.GetHeader("HX-Request") == "true" {
		c.Header("HX-Redirect", next)
		c.Status(http.StatusNoContent)
		return
	}
	c.Redirect(http.StatusSeeOther, next)
}

func (p pages) unverified(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	p.layout.Render(c, http.StatusOK, "Confirm your email", unverifiedPage(c, p.root, p.s.cfg.LoginPath))
}

func (p pages) deleted(c *gin.Context) {
	p.layout.Render(c, http.StatusOK, "Account deleted", deletedPage(p.root, false))
}

func (p pages) emailChanged(c *gin.Context) {
	p.layout.Render(c, http.StatusOK, "Email updated", emailChangedPage(p.s.cfg.LoginPath))
}
