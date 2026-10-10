package hanko

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

// The profile UI manages the signed-in user's email address, passkeys and signed-in
// devices. Passkeys and devices run in the browser through Hanko's SDK (profile.js).
// The email change runs on the server through Hanko's profile flow: the user confirms
// the new address with a passcode, the new address becomes primary, the old one is
// removed, other sessions are signed out, and the user signs in again. Between
// requests, the flow's state lives in a sealed cookie.
//
// The accounts module mounts the profile UI on its settings page.

const pendingCookie = "email-change"

// profileRoutes holds what the profile UI needs from where it is mounted.
type profileRoutes struct {
	settings     string // the settings page's path
	logoutPath   string
	emailChanged func(c *gin.Context, email string) error
	assets       *ui.Assets
}

// MountProfile adds the profile UI's routes under r, which must require sign-in.
// logoutPath is the sign-out route; emailChanged records a confirmed new address.
func (h *Hanko) MountProfile(r gin.IRouter, layout ui.Layout, logoutPath string, emailChanged func(c *gin.Context, email string) error) {
	h.addAssets(layout.Assets)
	h.profile = &profileRoutes{
		settings:     joinPath(r, ""),
		logoutPath:   logoutPath,
		emailChanged: emailChanged,
		assets:       layout.Assets,
	}
	g := r.Group("/profile")
	g.POST("/email", h.startEmailChange)
	g.POST("/email/verify", h.verifyEmail)
	g.POST("/email/resend", h.resendEmailCode)
	g.POST("/email/cleanup", h.finishEmailChange)
	g.POST("/passkeys/authorize", h.authorizePasskeyChange)
}

// ProfileCSP lists what pages that show the profile UI must allow.
func (h *Hanko) ProfileCSP() web.CSP {
	return h.csp(false)
}

// ProfileSection renders the profile UI for the signed-in user.
func (h *Hanko) ProfileSection(c *gin.Context) templ.Component {
	id, _ := identity.FromContext(c.Request.Context())
	p := h.profile
	settings := p.settings
	if settings == "" {
		settings = "/"
	}
	return profileSection(profileData{
		c:            c,
		email:        id.Email,
		verified:     id.EmailVerified,
		recent:       id.Recent(time.Now()),
		apiURL:       h.api.String(),
		moduleURL:    p.assets.URL("hanko/elements.js"),
		scriptURL:    p.assets.URL("hanko/profile.js"),
		base:         p.routes(),
		logoutPath:   p.logoutPath,
		signInAgain:  h.loginURL(settings, ""),
		settingsPath: settings,
	})
}

type profileData struct {
	c            *gin.Context
	email        string
	verified     bool
	recent       bool
	apiURL       string
	moduleURL    string
	scriptURL    string
	base         string
	logoutPath   string
	signInAgain  string
	settingsPath string
}

// authorizePasskeyChange answers 204 when the session is recent enough to add or
// remove a passkey, and 401 when the user must sign in again.
func (h *Hanko) authorizePasskeyChange(c *gin.Context) {
	id, _ := identity.FromContext(c.Request.Context())
	if !id.Recent(time.Now()) {
		c.Status(http.StatusUnauthorized)
		return
	}
	c.Status(http.StatusNoContent)
}

// pendingChange is the sealed state of an email change between requests.
type pendingChange struct {
	Subject       string      `json:"s"`
	Email         string      `json:"e"`
	NewEmailID    string      `json:"n,omitempty"`
	FormerEmailID string      `json:"f,omitempty"`
	Cleanup       bool        `json:"c,omitempty"`
	LocalSynced   bool        `json:"l,omitempty"`
	FormerDeleted bool        `json:"d,omitempty"`
	SessionsEnded bool        `json:"o,omitempty"`
	Actions       flowActions `json:"a"`
	CSRFToken     string      `json:"t"`
}

func (p *pendingChange) state() flowState {
	return flowState{Actions: p.Actions, CSRFToken: p.CSRFToken}
}

func (p *pendingChange) advance(s flowState) {
	if len(s.Actions) > 0 && s.CSRFToken != "" {
		p.Actions, p.CSRFToken = s.Actions, s.CSRFToken
	}
}

func savePending(c *gin.Context, p pendingChange) {
	data, _ := json.Marshal(p)
	web.SetSealedCookie(c, pendingCookie, data, time.Hour)
}

// loadPending returns the pending change if it belongs to the signed-in user.
func loadPending(c *gin.Context, subject string) (*pendingChange, bool) {
	data, err := web.SealedCookie(c, pendingCookie)
	if err != nil {
		return nil, false
	}
	var p pendingChange
	if json.Unmarshal(data, &p) != nil || p.Subject != subject || p.CSRFToken == "" {
		return nil, false
	}
	return &p, true
}

func (h *Hanko) startEmailChange(c *gin.Context) {
	id, _ := identity.FromContext(c.Request.Context())
	email := strings.TrimSpace(c.PostForm("email"))
	switch {
	case email == "":
		ui.Render(c, http.StatusUnprocessableEntity, ui.FormError("Enter your new email address."))
		return
	case strings.EqualFold(email, id.Email):
		ui.Render(c, http.StatusUnprocessableEntity, ui.FormError("This is already your email address."))
		return
	}
	web.ClearSealedCookie(c, pendingCookie)

	token := sessionToken(c.Request)
	state, err := h.startProfile(c.Request.Context(), token)
	if err == nil {
		state, err = h.act(c.Request.Context(), token, state, "email_create", map[string]string{"email": email})
	}
	if err != nil {
		h.profileFailure(c, "start email change", err)
		return
	}
	if state.HTTPStatus == http.StatusBadRequest {
		ui.Render(c, http.StatusUnprocessableEntity, ui.FormError("This address is invalid or already in use. Try another one."))
		return
	}
	if state.HTTPStatus != http.StatusOK || state.Name != "passcode_confirmation" {
		h.profileFailure(c, "start email change", flowError(state))
		return
	}
	savePending(c, pendingChange{Subject: id.Subject, Email: email, Actions: state.Actions, CSRFToken: state.CSRFToken})
	h.showDialog(c, verifyDialog(h.profile.routes(), email, "", false))
}

func (h *Hanko) verifyEmail(c *gin.Context) {
	id, _ := identity.FromContext(c.Request.Context())
	pending, ok := loadPending(c, id.Subject)
	if !ok || pending.Cleanup {
		h.showDialog(c, changeErrorDialog("This email change is no longer available. Start again."))
		return
	}
	code := strings.TrimSpace(c.PostForm("code"))
	if code == "" {
		h.showDialog(c, verifyDialog(h.profile.routes(), pending.Email, "Enter the code we sent you.", true))
		return
	}
	token := sessionToken(c.Request)
	state, err := h.act(c.Request.Context(), token, pending.state(), "verify_passcode", map[string]string{"code": code})
	if err != nil {
		h.profileFailure(c, "verify email", err)
		return
	}
	switch {
	case state.expired():
		web.ClearSealedCookie(c, pendingCookie)
		h.showDialog(c, changeErrorDialog("This email change expired. Start again."))
	case state.HTTPStatus == http.StatusUnauthorized:
		web.ClearSealedCookie(c, pendingCookie)
		h.showDialog(c, changeErrorDialog("This email change is no longer authorised. Sign in and start again."))
	case state.HTTPStatus == http.StatusBadRequest, state.HTTPStatus == http.StatusTooManyRequests:
		pending.advance(state)
		savePending(c, *pending)
		message := "That code is not right. Try again."
		if state.HTTPStatus == http.StatusTooManyRequests {
			message = "Too many attempts. Wait a moment and try again."
		}
		h.showDialog(c, verifyDialog(h.profile.routes(), pending.Email, message, true))
	case state.ok():
		state, err = h.makePrimary(c, token, id, pending, state)
		if err != nil {
			h.profileFailure(c, "set the new primary email", err)
			return
		}
		pending.advance(state)
		pending.Cleanup = true
		savePending(c, *pending)
		h.completeEmailChange(c, token, id, pending, state)
	default:
		h.profileFailure(c, "verify email", flowError(state))
	}
}

func (h *Hanko) resendEmailCode(c *gin.Context) {
	id, _ := identity.FromContext(c.Request.Context())
	pending, ok := loadPending(c, id.Subject)
	if !ok || pending.Cleanup {
		h.showDialog(c, changeErrorDialog("This email change is no longer available. Start again."))
		return
	}
	state, err := h.act(c.Request.Context(), sessionToken(c.Request), pending.state(), "resend_passcode", map[string]any{})
	if err != nil {
		h.profileFailure(c, "resend email code", err)
		return
	}
	switch {
	case state.expired():
		web.ClearSealedCookie(c, pendingCookie)
		h.showDialog(c, changeErrorDialog("This email change expired. Start again."))
	case state.HTTPStatus == http.StatusTooManyRequests:
		pending.advance(state)
		savePending(c, *pending)
		h.showDialog(c, verifyDialog(h.profile.routes(), pending.Email, "Wait a moment before asking for another code.", true))
	case state.HTTPStatus == http.StatusOK && state.Name == "passcode_confirmation":
		pending.advance(state)
		savePending(c, *pending)
		h.showDialog(c, verifyDialog(h.profile.routes(), pending.Email, "We sent a new code.", false))
	default:
		h.profileFailure(c, "resend email code", flowError(state))
	}
}

func (h *Hanko) finishEmailChange(c *gin.Context) {
	id, _ := identity.FromContext(c.Request.Context())
	pending, ok := loadPending(c, id.Subject)
	if !ok || !pending.Cleanup {
		web.ClearSealedCookie(c, pendingCookie)
		h.showDialog(c, cleanupErrorDialog("The remaining steps are no longer available. Sign out on your other devices from this page."))
		return
	}
	// The cleanup steps act on email and session IDs, so a fresh profile flow serves
	// them; the stored flow state may be stale after a failure.
	token := sessionToken(c.Request)
	state, err := h.startProfile(c.Request.Context(), token)
	if err != nil {
		h.profileFailure(c, "resume email change", err)
		return
	}
	h.completeEmailChange(c, token, id, pending, state)
}

// makePrimary makes the verified new address primary and remembers both addresses.
func (h *Hanko) makePrimary(c *gin.Context, token string, id identity.Identity, pending *pendingChange, state flowState) (flowState, error) {
	p, err := profileFor(state, id.Subject)
	if err != nil {
		return state, err
	}
	former, ok := p.email(func(e profileEmail) bool { return e.IsPrimary && strings.EqualFold(e.Address, id.Email) })
	if !ok {
		return state, errUnexpectedProfile
	}
	added, ok := p.email(func(e profileEmail) bool {
		return e.IsVerified && !e.IsPrimary && strings.EqualFold(e.Address, pending.Email)
	})
	if !ok {
		return state, errUnexpectedProfile
	}
	state, err = h.act(c.Request.Context(), token, state, "email_set_primary", map[string]string{"email_id": added.ID})
	if err != nil {
		return state, err
	}
	if !state.ok() {
		return state, flowError(state)
	}
	p, err = profileFor(state, id.Subject)
	if err != nil {
		return state, err
	}
	if _, ok := p.email(func(e profileEmail) bool { return e.ID == added.ID && e.IsPrimary }); !ok {
		return state, errUnexpectedProfile
	}
	pending.NewEmailID, pending.FormerEmailID = added.ID, former.ID
	return state, nil
}

// completeEmailChange finishes the steps after the new address became primary, starting
// from a profile flow state. Each step is recorded as it succeeds, so a retry continues
// where a failure stopped.
func (h *Hanko) completeEmailChange(c *gin.Context, token string, id identity.Identity, pending *pendingChange, state flowState) {
	fail := func(err error) {
		if state.expired() {
			web.ClearSealedCookie(c, pendingCookie)
			h.showDialog(c, cleanupErrorDialog("The remaining steps expired. Sign out on your other devices from this page."))
			return
		}
		pending.advance(state)
		savePending(c, *pending)
		web.Log(c).Warn("email change cleanup failed", "error", err)
		h.showDialog(c, cleanupDialog(h.profile.routes(), pending.Email))
	}

	if !pending.LocalSynced {
		if err := h.profile.emailChanged(c, pending.Email); err != nil {
			fail(err)
			return
		}
		pending.LocalSynced = true
		savePending(c, *pending)
	}
	if !pending.FormerDeleted {
		var err error
		state, err = h.act(c.Request.Context(), token, state, "email_delete", map[string]string{"email_id": pending.FormerEmailID})
		if err == nil && !state.ok() {
			err = flowError(state)
		}
		if err != nil {
			fail(err)
			return
		}
		pending.advance(state)
		pending.FormerDeleted = true
		savePending(c, *pending)
	}
	if !pending.SessionsEnded {
		p, err := profileFor(state, id.Subject)
		if err != nil {
			fail(err)
			return
		}
		for _, s := range p.Sessions {
			if s.Current {
				continue
			}
			state, err = h.act(c.Request.Context(), token, state, "session_delete", map[string]string{"session_id": s.ID})
			if err == nil && !state.ok() {
				err = flowError(state)
			}
			if err != nil {
				fail(err)
				return
			}
		}
		pending.advance(state)
		pending.SessionsEnded = true
		savePending(c, *pending)
	}
	if err := h.Logout(c.Writer, c.Request); err != nil {
		fail(err)
		return
	}
	web.ClearSealedCookie(c, pendingCookie)
	redirect(c, h.loginURL("", "email-changed"))
}

// showDialog swaps a dialog into the profile UI's modal container and closes the dialog
// that sent the request.
func (h *Hanko) showDialog(c *gin.Context, dialog templ.Component) {
	c.Header("HX-Retarget", "#profile-modal")
	c.Header("HX-Reswap", "innerHTML")
	c.Header("HX-Trigger", "dialog:close")
	ui.Render(c, http.StatusOK, dialog)
}

func (h *Hanko) profileFailure(c *gin.Context, step string, err error) {
	if errors.Is(err, errSessionRejected) {
		redirect(c, h.loginURL(h.profile.routes(), ""))
		return
	}
	web.Log(c).Error("profile request failed", "step", step, "error", err)
	ui.Render(c, http.StatusBadGateway, ui.FormError("Something went wrong on our side. Try again in a moment."))
}

func redirect(c *gin.Context, to string) {
	if c.GetHeader("HX-Request") == "true" {
		c.Header("HX-Redirect", to)
		c.Status(http.StatusOK)
		return
	}
	c.Redirect(http.StatusSeeOther, to)
}

// routes returns the path the profile UI's routes are under.
func (p *profileRoutes) routes() string {
	return p.settings + "/profile"
}

var errUnexpectedProfile = errors.New("hanko: the profile did not contain the expected email addresses")

// flowError describes an unexpected flow state without Hanko's message text.
func flowError(s flowState) error {
	return fmt.Errorf("hanko: unexpected flow state %q (status %d)", s.Name, s.HTTPStatus)
}
