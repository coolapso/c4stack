package credits

import (
	"encoding/json"
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

// Mount adds the credits page to r, a group at the page's path that uses
// accounts.RequireSignIn, such as /account/credits:
//   - GET "" shows the balance, a purchase form and the transactions.
//   - POST "" starts a purchase and redirects to the provider's checkout.
//   - GET "/quote" prices an amount for the purchase form.
//   - GET "/summary" renders one page of transactions ("page", from 1).
//   - GET "/balance" renders the available balance.
//   - GET "/payment-status" reports a purchase started from this browser after the
//     buyer returns, or answers 204 when there is none.
//
// The pages allow the provider's checkout origin as a form target, because starting a
// purchase redirects there. Without a payment provider they show the balance and
// transactions, and say that buying credit is not available.
func (cr *Credits) Mount(r gin.IRouter, layout ui.Layout) {
	p := creditPages{cr: cr, layout: layout, base: basePath(r)}
	if cr.payments != nil {
		r = r.Group("", web.AllowCSP(web.CSP{FormActions: []string{cr.payments.CheckoutOrigin()}}))
	}
	r.GET("", p.page)
	r.POST("", p.checkout)
	r.GET("/quote", p.quote)
	r.GET("/summary", p.summary)
	r.GET("/balance", p.balance)
	r.GET("/payment-status", p.paymentStatus)
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
	c          *gin.Context
	base       string
	summary    Summary
	policy     Policy
	purchasing bool
	amount     string
	err        string
}

// transactionsView is one page of transactions.
type transactionsView struct {
	base    string
	entries []Entry
	page    int
	more    bool
	failed  bool
}

const transactionsPerPage = 10

func htmx(c *gin.Context) bool {
	return c.GetHeader("HX-Request") == "true"
}

func (p creditPages) view(c *gin.Context) (creditsView, error) {
	v := creditsView{c: c, base: p.base, policy: p.cr.policy, purchasing: p.cr.Purchasing(), amount: defaultAmount(p.cr.policy)}
	summary, err := p.cr.Summary(c.Request.Context(), accounts.Current(c).ID)
	v.summary = summary
	return v, err
}

// defaultAmount is the amount the purchase form starts with: €25, within the policy.
func defaultAmount(policy Policy) string {
	cents := min(max(int64(2500), policy.MinPurchaseCents), max(policy.MaxPurchaseCents, policy.MinPurchaseCents))
	return strings.TrimPrefix(FormatEuros(cents), "€")
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
	if !p.cr.Purchasing() {
		return priced{err: "Buying credit is not available right now."}
	}
	if strings.TrimSpace(amount) == "" {
		return priced{err: "Enter an amount to buy."}
	}
	cents, ok := ParseEuros(amount)
	if !ok {
		return priced{err: "Enter an amount in euros, such as 25.00."}
	}
	q, err := p.cr.Quote(cents)
	if err != nil {
		return priced{err: "Buy between " + FormatEuros(p.cr.policy.MinPurchaseCents) + " and " + FormatEuros(p.cr.policy.MaxPurchaseCents) + "."}
	}
	return priced{quote: q}
}

// pendingCookie remembers the purchase this browser started, so the credits page can
// report it when the buyer returns from the checkout.
const pendingCookie = "pending-payment"

type pendingPayment struct {
	IntentID string `json:"i"`
	// Since is when the buyer returned and the page first asked for the status.
	Since int64 `json:"s,omitempty"`
}

func (p creditPages) checkout(c *gin.Context) {
	amount := c.PostForm("amount")
	priced := p.price(amount)
	if priced.err != "" {
		p.checkoutError(c, http.StatusUnprocessableEntity, amount, priced.err)
		return
	}
	purchase, err := p.cr.Purchase(c.Request.Context(), accounts.Current(c), priced.quote.CreditCents, web.URL(c, p.base))
	if err != nil {
		web.Log(c).Error("starting a purchase failed", "error", err)
		p.checkoutError(c, http.StatusBadGateway, amount, "We could not start the checkout. Try again in a moment.")
		return
	}
	data, _ := json.Marshal(pendingPayment{IntentID: purchase.IntentID})
	web.SetSealedCookie(c, pendingCookie, data, 24*time.Hour)
	if htmx(c) {
		c.Header("HX-Redirect", purchase.CheckoutURL)
		c.Status(http.StatusOK)
		return
	}
	c.Redirect(http.StatusSeeOther, purchase.CheckoutURL)
}

// checkoutError shows why a purchase did not start: below the form for HTMX, or on the
// page.
func (p creditPages) checkoutError(c *gin.Context, status int, amount, message string) {
	if htmx(c) {
		ui.Render(c, status, checkoutErrorSlot(message))
		return
	}
	v, err := p.view(c)
	if err != nil {
		p.fail(c, err)
		return
	}
	v.amount, v.err = amount, message
	p.layout.Render(c, status, "Credits", creditsPage(v))
}

func (p creditPages) summary(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	page = min(max(page, 1), MaxHistoryPage)
	v := transactionsView{base: p.base, page: page}
	entries, more, err := p.cr.History(c.Request.Context(), accounts.Current(c).ID, page, transactionsPerPage)
	if err != nil {
		web.Log(c).Error("loading credit transactions failed", "error", err)
		v.failed = true
	}
	v.entries, v.more = entries, more
	ui.Render(c, http.StatusOK, transactions(v))
}

func (p creditPages) balance(c *gin.Context) {
	s, err := p.cr.Summary(c.Request.Context(), accounts.Current(c).ID)
	if err != nil {
		web.Log(c).Error("loading the credit balance failed", "error", err)
		ui.Render(c, http.StatusOK, balanceUnavailable())
		return
	}
	ui.Render(c, http.StatusOK, balanceValue(s))
}

// paymentStatus reports the purchase this browser started: confirming, confirmed, not
// completed, or taking longer than expected.
func (p creditPages) paymentStatus(c *gin.Context) {
	data, err := web.SealedCookie(c, pendingCookie)
	var pending pendingPayment
	if err != nil || json.Unmarshal(data, &pending) != nil || pending.IntentID == "" {
		c.Status(http.StatusNoContent)
		return
	}
	if pending.Since == 0 {
		pending.Since = time.Now().Unix()
		data, _ := json.Marshal(pending)
		web.SetSealedCookie(c, pendingCookie, data, 24*time.Hour)
	}
	waited := time.Since(time.Unix(pending.Since, 0))
	status, err := p.cr.PaymentStatus(c.Request.Context(), accounts.Current(c).ID, pending.IntentID)
	switch {
	case errors.Is(err, ErrNotFound):
		web.ClearSealedCookie(c, pendingCookie)
		c.Status(http.StatusNoContent)
	case err != nil:
		web.Log(c).Warn("checking a payment failed", "error", err)
		ui.Render(c, http.StatusOK, paymentStatus(p.base, waitingState(waited)))
	case status == "paid":
		web.ClearSealedCookie(c, pendingCookie)
		c.Header("HX-Trigger", "credits-confirmed")
		ui.Render(c, http.StatusOK, paymentStatus(p.base, "confirmed"))
	case status == "failed" || status == "canceled" || status == "expired":
		web.ClearSealedCookie(c, pendingCookie)
		ui.Render(c, http.StatusOK, paymentStatus(p.base, "not-completed"))
	default:
		ui.Render(c, http.StatusOK, paymentStatus(p.base, waitingState(waited)))
	}
}

// waitingState is what a buyer sees while a payment is unconfirmed: confirming, then
// still confirming after 30 seconds, then a timeout after a minute.
func waitingState(waited time.Duration) string {
	switch {
	case waited > time.Minute:
		return "timeout"
	case waited > 30*time.Second:
		return "still-confirming"
	}
	return "confirming"
}

func (p creditPages) fail(c *gin.Context, err error) {
	web.Log(c).Error("credits page failed", "error", err)
	if htmx(c) {
		ui.Render(c, http.StatusInternalServerError, ui.FormError("Something went wrong on our side. Try again in a moment."))
		return
	}
	p.layout.Error(c, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
}
