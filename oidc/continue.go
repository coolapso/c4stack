package oidc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// maxAgeLimit caps max_age, so a huge value can't overflow a duration. Anything longer
// means "any sign-in".
const maxAgeLimit = 10 * 365 * 24 * 60 * 60

// clockSkew allows for the app's clock, which dates requests, and the identity
// provider's, which dates sign-ins, being slightly apart, once the person was sent to
// sign in again: without it, the sign-in they just made could look older than the
// request, and never count.
const clockSkew = 30 * time.Second

// continueSignIn finishes a request once someone is signed in: RequireSignIn sent them
// through the sign-in pages first if they weren't. It checks the request again, asks
// the app whether the account may sign in to the client, and sends the browser back to
// the client with a code.
func (p *Provider) continueSignIn(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Query("id")
	r, err := p.openRequest(id)
	if err != nil {
		p.expired(c)
		return
	}
	value, err := web.SealedCookie(c, browserCookie)
	h := sha256.Sum256(value)
	if err != nil || subtle.ConstantTimeCompare(h[:], r.Browser) != 1 {
		p.layout.Error(c, http.StatusBadRequest, "Start again from the app",
			"This sign-in started in another browser, or this browser forgot it. Go back to the app you came from and sign in from there.")
		return
	}
	// The client, or its redirect URIs, may have changed since the request began.
	client, err := p.client(ctx, r.ClientID)
	if errors.Is(err, ErrUnknownClient) || err == nil && !slices.Contains(client.RedirectURIs, r.RedirectURI) {
		p.expired(c)
		return
	}
	if err != nil {
		web.Log(c).Error("oidc: client lookup failed", "error", err)
		p.layout.Error(c, http.StatusServiceUnavailable, "Something went wrong", "Please try again in a moment.")
		return
	}
	account := accounts.Current(c)
	who, _ := identity.FromContext(ctx)
	if r.Hint != "" && r.Hint != account.ID {
		p.refuse(c, r, oidc.ErrLoginRequired().WithDescription("signed in to another account"))
		return
	}
	again := c.Query("again") != ""
	if r.MaxAge != nil && *r.MaxAge < maxAgeLimit && who.SignedInAt.Before(signedInSince(r, again)) {
		if p.cfg.Reauthenticate == nil || again {
			p.refuse(c, r, oidc.ErrLoginRequired())
			return
		}
		next := continuePath + "?" + url.Values{"id": {id}, "again": {"1"}}.Encode()
		c.Redirect(http.StatusSeeOther, p.cfg.Reauthenticate(next))
		return
	}
	if p.cfg.Allow != nil {
		if err := p.cfg.Allow(ctx, client.ID, account); err != nil {
			p.refuse(c, r, oidc.ErrAccessDenied())
			return
		}
	}
	code, err := p.issueCode(ctx, r, account, who)
	if err != nil {
		web.Log(c).Error("oidc: issuing a code failed", "error", err)
		p.refuse(c, r, oidc.ErrServerError())
		return
	}
	p.answer(c, r.RedirectURI, &op.CodeResponseType{Code: code, State: r.State})
}

// signedInSince is the earliest sign-in a request accepts: max_age seconds ago, or for
// max_age 0, which prompt=login sets, when the request began. again says the person was
// sent to sign in again, which earns the clocks' allowance.
func signedInSince(r *request, again bool) time.Time {
	since := time.UnixMilli(r.Created)
	if *r.MaxAge > 0 {
		since = time.Now().Add(-time.Duration(*r.MaxAge) * time.Second)
	}
	if again {
		since = since.Add(-clockSkew)
	}
	return since
}

// issueCode stores a code for the request, signed in as account, and returns it. The
// address and whether it is verified are taken now, for the ID token and userinfo.
func (p *Provider) issueCode(ctx context.Context, r *request, account accounts.Account, who identity.Identity) (string, error) {
	code := newToken()
	h := sha256.Sum256([]byte(code))
	verified := who.EmailVerified && strings.EqualFold(account.Email, who.Email)
	_, err := p.app.DB().ExecContext(ctx, `
		INSERT INTO oidc_codes (
		    code_hash, account_id, client_id, redirect_uri, scopes, nonce, code_challenge,
		    auth_time, email, email_verified, expires_at
		)
		VALUES (
		    $1, $2, $3, $4, string_to_array($5, ' '), $6, $7, $8, $9, $10,
		    now() + make_interval(secs => $11)
		)`,
		h[:], account.ID, r.ClientID, r.RedirectURI, strings.Join(r.Scopes, " "), r.Nonce, r.Challenge,
		who.SignedInAt, account.Email, verified, codeLifetime.Seconds(),
	)
	return code, err
}

// answer sends the browser to a redirect URI the client registered, with response: a
// code, or an error.
func (p *Provider) answer(c *gin.Context, redirectURI string, response any) {
	target, err := op.AuthResponseURL(redirectURI, oidc.ResponseTypeCode, oidc.ResponseModeQuery, response, p.op.Encoder())
	if err != nil {
		web.Log(c).Error("oidc: building the response failed", "error", err)
		p.layout.Error(c, http.StatusInternalServerError, "Something went wrong", "Please go back to the app and try again.")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusSeeOther, target)
}

// refuse answers the client with an error, such as access_denied.
func (p *Provider) refuse(c *gin.Context, r *request, e *oidc.Error) {
	e.State = r.State
	p.answer(c, r.RedirectURI, e)
}

func (p *Provider) expired(c *gin.Context) {
	p.layout.Error(c, http.StatusBadRequest, "This sign-in has expired",
		"Go back to the app you came from and sign in again.")
}
