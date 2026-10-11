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
// page embeds it, and its routes go under the settings page's path.
type profileUI interface {
	MountProfile(r gin.IRouter, layout ui.Layout, logoutPath, emailChangedPath string, emailChanged func(c *gin.Context, email string) error)
	ProfileCSP() web.CSP
	ProfileSection(c *gin.Context) templ.Component
}

// Mount adds every account page to r, usually the app's engine, at Config.Paths:
//   - POST Logout signs out and continues to a local "next" path, or "/".
//   - GET Unverified asks an identity without a verified email to confirm it.
//   - GET Settings shows the email address, the provider's profile UI when it has one,
//     signing out and account deletion. The profile UI's routes go under it.
//   - GET and POST Delete confirm and delete the account.
//   - GET Deleted and EmailChanged confirm those changes after the session ended.
//
// The settings and delete pages use RequireSignIn. When r is a group, every path must be
// under its prefix. To serve one of the pages yourself, register the others with Pages
// instead.
func (s *Accounts) Mount(r gin.IRouter, layout ui.Layout) {
	p := s.Pages(layout)
	paths := s.cfg.Paths
	rel := relativeTo(r)
	r.POST(rel(paths.Logout), web.CSRF, p.Logout)
	r.GET(rel(paths.Unverified), p.Unverified)
	r.GET(rel(paths.Deleted), p.Deleted)
	r.GET(rel(paths.EmailChanged), p.EmailChanged)
	settings := r.Group(rel(paths.Settings), s.RequireSignIn)
	p.MountProfile(settings)
	settings.GET("", p.Settings)
	r.GET(rel(paths.Delete), s.RequireSignIn, p.DeleteForm)
	r.POST(rel(paths.Delete), s.RequireSignIn, p.Delete)
}

// relativeTo returns a function that turns a URL path into a route path relative to r.
// It panics for a path outside r's prefix, which is a programming error.
func relativeTo(r gin.IRouter) func(string) string {
	base := ""
	if g, ok := r.(interface{ BasePath() string }); ok {
		base = strings.TrimSuffix(g.BasePath(), "/")
	}
	return func(path string) string {
		if base == "" {
			return path
		}
		if path != base && !strings.HasPrefix(path, base+"/") {
			panic("accounts: the path " + path + " is not under " + base)
		}
		return strings.TrimPrefix(path, base)
	}
}

// Pages are the account pages' handlers, for apps that register some of them
// themselves. Register each at its path in Config.Paths: the pages link to each other
// there. Settings, DeleteForm and Delete need RequireSignIn, and Logout needs web.CSRF.
type Pages struct {
	s       *Accounts
	layout  ui.Layout
	profile profileUI
}

// Pages returns the account pages rendered in layout. It panics without an identity
// provider, which is a programming error.
func (s *Accounts) Pages(layout ui.Layout) *Pages {
	if s.idp == nil {
		panic("accounts: the account pages need an identity provider")
	}
	p := &Pages{s: s, layout: layout}
	if profile, ok := s.idp.(profileUI); ok {
		p.profile = profile
	}
	return p
}

// MountProfile adds the provider's profile UI routes, if it has any, to settings: a
// group at the settings path that uses RequireSignIn. The Settings page embeds the UI.
func (p *Pages) MountProfile(settings gin.IRouter) {
	if p.profile == nil {
		return
	}
	paths := p.s.cfg.Paths
	p.profile.MountProfile(settings.Group("", web.AllowCSP(p.profile.ProfileCSP())), p.layout, paths.Logout, paths.EmailChanged,
		func(c *gin.Context, email string) error {
			id, _ := identity.FromContext(c.Request.Context())
			return p.s.setEmail(c.Request.Context(), Current(c).ID, id.Subject, email)
		})
}

// settingsView is what the settings and delete pages show.
type settingsView struct {
	c       *gin.Context
	account Account
	profile templ.Component
	paths   Paths
	recent  bool
	// signInAgain is the sign-out form's next path when deletion needs a fresh sign-in.
	signInAgain string
	// deleteError reopens the delete form with a message.
	deleteError string
	// notice is shown at the top, for example after a failed deletion.
	notice string
}

func (p *Pages) view(c *gin.Context) settingsView {
	id, _ := identity.FromContext(c.Request.Context())
	paths := p.s.cfg.Paths
	return settingsView{
		c:           c,
		account:     Current(c),
		paths:       paths,
		recent:      id.Recent(time.Now()),
		signInAgain: paths.Login + "?next=" + url.QueryEscape(paths.Delete),
	}
}

// Settings is the settings page.
func (p *Pages) Settings(c *gin.Context) {
	v := p.view(c)
	if p.profile != nil {
		v.profile = p.profile.ProfileSection(c)
	}
	p.layout.Render(c, http.StatusOK, "Account settings", settingsPage(v))
}

// DeleteForm asks to confirm the deletion.
func (p *Pages) DeleteForm(c *gin.Context) {
	p.layout.Render(c, http.StatusOK, "Delete account", deletePage(p.view(c)))
}

// Delete deletes the account once the user typed DELETE after a recent sign-in.
func (p *Pages) Delete(c *gin.Context) {
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
		p.layout.Render(c, http.StatusInternalServerError, "Account deleted", deletedPage(true))
	case err != nil:
		web.Log(c).Error("account deletion failed", "error", err)
		c.AbortWithStatus(http.StatusInternalServerError)
	default:
		c.Redirect(http.StatusSeeOther, p.s.cfg.Paths.Deleted)
	}
}

// Logout signs out and continues to a local "next" path, or "/".
func (p *Pages) Logout(c *gin.Context) {
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

// Unverified asks an identity without a verified email to confirm its address.
func (p *Pages) Unverified(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	p.layout.Render(c, http.StatusOK, "Confirm your email", unverifiedPage(c, p.s.cfg.Paths))
}

// Deleted confirms a deletion. With "unfinished", it says that the deletion of the
// address's account has not finished yet.
func (p *Pages) Deleted(c *gin.Context) {
	p.layout.Render(c, http.StatusOK, "Account deleted", deletedPage(c.Query("unfinished") != ""))
}

// EmailChanged confirms an email change, after which the user signs in again.
func (p *Pages) EmailChanged(c *gin.Context) {
	p.layout.Render(c, http.StatusOK, "Email updated", emailChangedPage(p.s.cfg.Paths.Login))
}
