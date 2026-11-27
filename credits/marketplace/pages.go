package marketplace

import (
	"errors"
	"net/http"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

// Mount adds the claim page to r, a group at the path Config.ClaimURL names that uses
// accounts.RequireSignIn, such as /account/claim. Someone opening a claim link signs in
// or signs up first, then:
//   - GET "" shows what the link would move into their account, and asks to confirm.
//   - POST "" claims it and sends them to Config.ClaimedPath.
//
// An app that wants its own page builds it on Claimable and Claim.
func (m *Marketplace) Mount(r gin.IRouter, layout ui.Layout) {
	p := claimPages{m: m, layout: layout}
	r.GET("", p.page)
	r.POST("", p.claim)
}

type claimPages struct {
	m      *Marketplace
	layout ui.Layout
}

const claimTitle = "Claim your agent's account"

func (p claimPages) page(c *gin.Context) {
	p.render(c, http.StatusOK, c.Query("token"), "")
}

func (p claimPages) claim(c *gin.Context) {
	token := c.PostForm("token")
	err := p.m.Claim(c.Request.Context(), token, accounts.Current(c).ID)
	switch {
	case err == nil:
		web.Log(c).Info("marketplace account claimed", "account_id", accounts.Current(c).ID)
		c.Redirect(http.StatusSeeOther, p.m.cfg.ClaimedPath)
	case errors.Is(err, ErrBusy):
		p.render(c, http.StatusConflict, token, "Your agent has requests in progress. Try again in a minute.")
	case errors.Is(err, ErrClaimInvalid):
		p.render(c, http.StatusNotFound, token, "")
	default:
		web.Log(c).Error("claiming a marketplace account failed", "error", err)
		p.render(c, http.StatusInternalServerError, token, "Something went wrong, and nothing was claimed. Try again.")
	}
}

// render shows the claim page for token, or says the link can't be used.
func (p claimPages) render(c *gin.Context, status int, token, message string) {
	claimable, err := p.m.Claimable(c.Request.Context(), token)
	if errors.Is(err, ErrClaimInvalid) {
		p.layout.Error(c, http.StatusNotFound, "This link can't be used",
			"It was used already, or the account it claims was deleted. Ask the support team if you think it should work.")
		return
	}
	if err != nil {
		web.Log(c).Error("reading a marketplace claim failed", "error", err)
		p.layout.Error(c, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	p.layout.Render(c, status, claimTitle, claimPage(claimView{c: c, token: token, claimable: claimable, err: message}))
}
