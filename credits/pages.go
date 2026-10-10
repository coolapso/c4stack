package credits

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

// Mount adds the credits page under r, which must use accounts.RequireSignIn:
//   - GET "" shows the balance, a purchase form and the history.
//   - GET "/quote" prices an amount for the purchase form.
//   - POST "/checkout" starts a purchase and redirects to the provider's checkout.
//   - GET "/status" reports the latest purchase after the buyer returns.
//
// The pages allow the provider's checkout origin as a form target, because starting a
// purchase redirects there.
func (cr *Credits) Mount(r gin.IRouter, layout ui.Layout) {
	p := creditPages{cr: cr, layout: layout, base: basePath(r)}
	g := r.Group("", web.AllowCSP(web.CSP{FormActions: []string{cr.payments.CheckoutOrigin()}}))
	g.GET("", p.page)
	g.GET("/quote", p.quote)
	g.POST("/checkout", p.checkout)
	g.GET("/status", p.status)
}

func basePath(r gin.IRouter) string {
	if g, ok := r.(interface{ BasePath() string }); ok {
		return strings.TrimSuffix(g.BasePath(), "/")
	}
	return ""
}

type creditPages struct {
	cr     *Credits
	layout ui.Layout
	base   string
}

// creditsView is what the credits page shows.
type creditsView struct {
	c        *gin.Context
	base     string
	summary  Summary
	policy   Policy
	entries  []Entry
	older    int64 // the before cursor for the next page, or zero
	returned bool  // the buyer just came back from the checkout
	amount   string
	err      string
}

const historyPage = 25

func (p creditPages) view(c *gin.Context) (creditsView, error) {
	id := accounts.Current(c).ID
	v := creditsView{c: c, base: p.base, policy: p.cr.policy, returned: c.Query("checkout") != ""}
	summary, err := p.cr.Summary(c.Request.Context(), id)
	if err != nil {
		return v, err
	}
	v.summary = summary
	before, _ := strconv.ParseInt(c.Query("before"), 10, 64)
	entries, err := p.cr.History(c.Request.Context(), id, before, historyPage+1)
	if err != nil {
		return v, err
	}
	if len(entries) > historyPage {
		entries = entries[:historyPage]
		v.older = entries[historyPage-1].ID
	}
	v.entries = entries
	return v, nil
}

func (p creditPages) page(c *gin.Context) {
	v, err := p.view(c)
	if err != nil {
		p.fail(c, err)
		return
	}
	p.layout.Render(c, http.StatusOK, "Credits", creditsPage(v))
}

func (p creditPages) quote(c *gin.Context) {
	ui.Render(c, http.StatusOK, quoteFragment(p.price(c.Query("amount"))))
}

// priced is a quote, or why an amount cannot be bought.
type priced struct {
	quote Quote
	err   string
}

func (p creditPages) price(amount string) priced {
	if strings.TrimSpace(amount) == "" {
		return priced{}
	}
	cents, ok := ParseEuros(amount)
	if !ok {
		return priced{err: "Enter an amount in euros, such as 10 or 12.50."}
	}
	q, err := p.cr.Quote(cents)
	if err != nil {
		return priced{err: "Buy between " + FormatEuros(p.cr.policy.MinPurchaseCents) + " and " + FormatEuros(p.cr.policy.MaxPurchaseCents) + "."}
	}
	return priced{quote: q}
}

func (p creditPages) checkout(c *gin.Context) {
	amount := c.PostForm("amount")
	priced := p.price(amount)
	if priced.err != "" || priced.quote.CreditCents == 0 {
		v, err := p.view(c)
		if err != nil {
			p.fail(c, err)
			return
		}
		v.amount, v.err = amount, priced.err
		if v.err == "" {
			v.err = "Enter an amount to buy."
		}
		p.layout.Render(c, http.StatusUnprocessableEntity, "Credits", creditsPage(v))
		return
	}
	success := web.URL(c, p.base+"?checkout=returned")
	purchase, err := p.cr.Purchase(c.Request.Context(), accounts.Current(c), priced.quote.CreditCents, success)
	if err != nil {
		web.Log(c).Error("starting a purchase failed", "error", err)
		v, verr := p.view(c)
		if verr != nil {
			p.fail(c, verr)
			return
		}
		v.amount, v.err = amount, "The payment could not start. Try again in a moment."
		p.layout.Render(c, http.StatusBadGateway, "Credits", creditsPage(v))
		return
	}
	if c.GetHeader("HX-Request") == "true" {
		c.Header("HX-Redirect", purchase.CheckoutURL)
		c.Status(http.StatusOK)
		return
	}
	c.Redirect(http.StatusSeeOther, purchase.CheckoutURL)
}

// status reports the latest purchase: still pending, confirmed, or taking long enough
// that the buyer should check back later.
func (p creditPages) status(c *gin.Context) {
	var status string
	var created time.Time
	err := p.cr.app.DB().QueryRowContext(c.Request.Context(), `
		SELECT status, created_at
		FROM payment_intents
		WHERE account_id = $1 AND checkout_id IS NOT NULL
		ORDER BY created_at DESC
		LIMIT 1`, accounts.Current(c).ID,
	).Scan(&status, &created)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		p.fail(c, err)
		return
	}
	switch {
	case status == "paid":
		c.Header("HX-Trigger", "credits-changed")
		ui.Render(c, http.StatusOK, statusFragment(p.base, "paid"))
	case status == "pending" && time.Since(created) < 2*time.Minute:
		ui.Render(c, http.StatusOK, statusFragment(p.base, "pending"))
	default:
		ui.Render(c, http.StatusOK, statusFragment(p.base, "slow"))
	}
}

func (p creditPages) fail(c *gin.Context, err error) {
	web.Log(c).Error("credits page failed", "error", err)
	if c.GetHeader("HX-Request") == "true" {
		ui.Render(c, http.StatusInternalServerError, ui.FormError("Something went wrong on our side. Try again in a moment."))
		return
	}
	p.layout.Error(c, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
}
