package accounts

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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

// confirmer is a provider with its own page where a signed-in user confirms it's them.
// The delete page links there instead of signing out first.
type confirmer interface {
	ConfirmURL(next string) string
}

// Mount adds every account page to r, usually the app's engine, at Config.Paths:
//   - POST Logout signs out and continues to a local "next" path, or "/".
//   - GET Unverified asks an identity without a verified email to confirm it.
//   - GET Settings shows the email address, the provider's profile UI when it has one,
//     the name with Config.Names, signing out and account deletion. The profile UI's
//     routes go under it.
//   - GET and POST Name, with Config.Names, ask for the account's name and change it.
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
	if s.cfg.Names {
		r.GET(rel(paths.Name), s.RequireSignIn, p.NameForm)
		r.POST(rel(paths.Name), s.RequireSignIn, p.SaveName)
	}
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
// there. Settings, NameForm, SaveName, DeleteForm and Delete need RequireSignIn, and
// Logout needs web.CSRF.
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
	names   bool
	recent  bool
	// sections are other modules' settings panels.
	sections []templ.Component
	// refused is why the account can't be deleted now; the delete form shows it.
	refused string
	// signInAgain is the sign-out form's next path when deletion needs a fresh sign-in.
	signInAgain string
	// confirmURL is the provider's page to confirm it's the user, which replaces the
	// sign-out form when the provider has one.
	confirmURL string
	// deleteError reopens the delete form with a message.
	deleteError string
	// notice is shown at the top, for example after a failed deletion.
	notice string
}

func (p *Pages) view(c *gin.Context) settingsView {
	id, _ := identity.FromContext(c.Request.Context())
	paths := p.s.cfg.Paths
	v := settingsView{
		c:           c,
		account:     Current(c),
		paths:       paths,
		names:       p.s.cfg.Names,
		recent:      id.Recent(time.Now()),
		signInAgain: paths.Login + "?next=" + url.QueryEscape(paths.Delete),
	}
	if cf, ok := p.s.idp.(confirmer); ok {
		v.confirmURL = cf.ConfirmURL(paths.Delete)
	}
	refused, err := p.s.refusal(c.Request.Context(), v.account.ID)
	if err != nil {
		web.Log(c).Error("checking whether the account can be deleted failed", "error", err)
	}
	v.refused = refused
	return v
}

// Settings is the settings page.
func (p *Pages) Settings(c *gin.Context) {
	v := p.view(c)
	if p.profile != nil {
		v.profile = p.profile.ProfileSection(c)
	}
	for _, section := range p.s.sections {
		if s := section(c); s != nil {
			v.sections = append(v.sections, s)
		}
	}
	p.layout.Render(c, http.StatusOK, "Account settings", settingsPage(v))
}

// maxName is the longest name, in characters.
const maxName = 100

// normalizeName trims a name and reports whether it is one: 1 to maxName characters,
// without control or formatting characters, which could hide or reorder what others
// read.
func normalizeName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxName {
		return "", false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", false
		}
	}
	return name, true
}

// nameView is what the name page shows.
type nameView struct {
	c       *gin.Context
	action  string
	name    string
	next    string
	invalid bool
}

// NameForm asks for the account's name, then continues to a local "next" path.
func (p *Pages) NameForm(c *gin.Context) {
	next := c.Query("next")
	if !localPath(next) {
		next = p.s.cfg.Paths.Settings
	}
	v := nameView{c: c, action: p.s.cfg.Paths.Name, name: Current(c).Name, next: next}
	p.layout.Render(c, http.StatusOK, "Your name", namePage(v))
}

// SaveName stores the account's name and continues to a local "next" path, or the
// settings page.
func (p *Pages) SaveName(c *gin.Context) {
	next := c.PostForm("next")
	if !localPath(next) {
		next = p.s.cfg.Paths.Settings
	}
	name, ok := normalizeName(c.PostForm("name"))
	if !ok {
		v := nameView{c: c, action: p.s.cfg.Paths.Name, name: c.PostForm("name"), next: next, invalid: true}
		p.layout.Render(c, http.StatusUnprocessableEntity, "Your name", namePage(v))
		return
	}
	if _, err := p.s.app.DB().ExecContext(c.Request.Context(), `
		UPDATE accounts
		SET name = $2, updated_at = now()
		WHERE id = $1`, Current(c).ID, name,
	); err != nil {
		web.Log(c).Error("saving an account's name failed", "error", err)
		p.layout.Error(c, http.StatusInternalServerError, "Something went wrong", "Your name was not saved. Please try again.")
		return
	}
	c.Redirect(http.StatusSeeOther, next)
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
	var refused *DeletionRefused
	switch {
	case errors.As(err, &refused):
		v.refused = refused.Reason
		p.layout.Render(c, http.StatusConflict, "Delete account", deletePage(v))
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
