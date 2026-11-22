package subscriptions

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

// Mount adds the subscription routes to r, a group behind accounts' RequireSignIn such
// as /account/subscriptions, and the account's subscriptions to the settings page:
//   - POST "" starts a checkout for the form's product, units (1 if empty) and target,
//     and sends the browser to the provider. The page with that form needs
//     web.AllowCSP(subs.CheckoutCSP()).
//   - POST "/:id/cancel" cancels a subscription at the end of its period.
//   - POST "/:id/change" changes a subscription's product or units, at once or at the
//     end of the period (Config.ChangeAtPeriodEnd). A change to what it has already
//     drops a change that waits.
//   - GET "/billing" sends the browser to the provider's billing page.
//   - GET "/started" is where a checkout returns: it confirms the account's checkouts
//     with the provider (Confirm), then continues to the local "next" path.
//
// Each POST continues to the form's local "next" path, or the settings page; a checkout
// returns there from the provider, through "/started". layout renders the pages shown
// when one fails.
func (s *Subscriptions) Mount(r gin.IRouter, layout ui.Layout) {
	p := &pages{s: s, layout: layout, base: "/"}
	if g, ok := r.(interface{ BasePath() string }); ok {
		p.base = strings.TrimSuffix(g.BasePath(), "/")
	}
	r.POST("", p.checkout)
	r.POST("/:id/cancel", p.cancel)
	r.POST("/:id/change", p.change)
	r.GET("/billing", p.billing)
	r.GET("/started", p.started)
	s.accounts.AddSection(p.section)
}

// CheckoutCSP is what a page with a checkout form allows, so the browser may follow the
// form to the provider's checkout.
func (s *Subscriptions) CheckoutCSP() web.CSP {
	return web.CSP{FormActions: []string{s.payments.CheckoutOrigin()}}
}

type pages struct {
	s      *Subscriptions
	layout ui.Layout
	base   string
}

// next returns the form's local next path, or the settings page.
func next(c *gin.Context) string {
	n := c.PostForm("next")
	if n == "" {
		n = c.Query("next")
	}
	if !localPath(n) {
		return "/account/settings"
	}
	return n
}

// started is where a checkout returns: the subscription shows at once, without waiting
// for the provider's webhook. A failure waits for the webhook instead.
func (p *pages) started(c *gin.Context) {
	if err := p.s.Confirm(c.Request.Context(), accounts.Current(c).ID); err != nil {
		web.Log(c).Warn("confirming checkouts failed; their webhooks start them", "error", err)
	}
	c.Redirect(http.StatusSeeOther, next(c))
}

// localPath reports whether p is a path on this site, the rules accounts uses: no
// scheme or host, no "//", backslashes or control characters.
func localPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return false
	}
	for _, r := range p {
		if r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (p *pages) checkout(c *gin.Context) {
	account := accounts.Current(c)
	units := 1
	if u := c.PostForm("units"); u != "" {
		n, err := strconv.Atoi(u)
		if err != nil {
			p.layout.Error(c, http.StatusBadRequest, "That can't be bought", "The number of units isn't a number.")
			return
		}
		units = n
	}
	url, err := p.s.Checkout(c.Request.Context(), account.ID, CheckoutRequest{
		Product:    c.PostForm("product"),
		Units:      units,
		Target:     c.PostForm("target"),
		Email:      account.Email,
		SuccessURL: web.URL(c, p.base+"/started?"+url.Values{"next": {next(c)}}.Encode()),
	})
	switch {
	case errors.Is(err, ErrUnknownProduct), errors.Is(err, ErrInvalid):
		p.layout.Error(c, http.StatusBadRequest, "That can't be bought", "Go back and choose again.")
	case errors.Is(err, ErrAlreadySubscribed):
		p.layout.Error(c, http.StatusConflict, "You already have this",
			"You have a subscription of this kind already. Change it instead of starting another.")
	case err != nil:
		web.Log(c).Error("starting a subscription checkout failed", "error", err)
		p.layout.Error(c, http.StatusBadGateway, "The checkout didn't open", "Nothing was charged. Please try again in a moment.")
	default:
		c.Redirect(http.StatusSeeOther, url)
	}
}

func (p *pages) cancel(c *gin.Context) {
	err := p.s.Cancel(c.Request.Context(), accounts.Current(c).ID, c.Param("id"))
	p.done(c, err, "canceling a subscription failed", "It wasn't canceled.")
}

func (p *pages) change(c *gin.Context) {
	units := 0
	if u := c.PostForm("units"); u != "" {
		n, err := strconv.Atoi(u)
		if err != nil {
			p.layout.Error(c, http.StatusBadRequest, "That can't be changed", "The number of units isn't a number.")
			return
		}
		units = n
	}
	err := p.s.Change(c.Request.Context(), accounts.Current(c).ID, c.Param("id"), ChangeRequest{Product: c.PostForm("product"), Units: units})
	p.done(c, err, "changing a subscription failed", "It wasn't changed.")
}

// done answers a cancel or a change.
func (p *pages) done(c *gin.Context, err error, logMessage, notDone string) {
	switch {
	case errors.Is(err, ErrNotFound):
		p.layout.NotFound(c)
	case errors.Is(err, ErrNotLive):
		c.Redirect(http.StatusSeeOther, next(c))
	case errors.Is(err, ErrBusy):
		p.layout.Error(c, http.StatusConflict, "A change is in progress", "Another change to this subscription is still going through. Please try again in a moment.")
	case errors.Is(err, ErrUnknownProduct), errors.Is(err, ErrInvalid):
		p.layout.Error(c, http.StatusBadRequest, "That can't be changed", "Go back and choose again.")
	case err != nil:
		web.Log(c).Error(logMessage, "error", err)
		p.layout.Error(c, http.StatusBadGateway, "Something went wrong", notDone+" Please try again in a moment.")
	default:
		c.Redirect(http.StatusSeeOther, next(c))
	}
}

func (p *pages) billing(c *gin.Context) {
	url, err := p.s.PortalURL(c.Request.Context(), accounts.Current(c).ID)
	switch {
	case errors.Is(err, ErrNoCustomer):
		p.layout.Error(c, http.StatusNotFound, "No billing yet", "You haven't paid for a subscription yet.")
	case err != nil:
		web.Log(c).Error("opening the billing page failed", "error", err)
		p.layout.Error(c, http.StatusBadGateway, "The billing page didn't open", "Please try again in a moment.")
	default:
		c.Redirect(http.StatusSeeOther, url)
	}
}

// panelView is what the settings page's panel shows.
type panelView struct {
	c       *gin.Context
	base    string
	next    string
	billing bool
	// billingNote says that billing opens on the provider's site.
	billingNote string
	running     []Subscription
}

// section is the settings page's panel: the account's subscriptions that haven't ended,
// with their renewal, and a way to cancel each and to reach billing, which an account
// has once it subscribed.
func (p *pages) section(c *gin.Context) templ.Component {
	subs, err := p.s.List(c.Request.Context(), accounts.Current(c).ID)
	if err != nil {
		web.Log(c).Error("listing subscriptions failed", "error", err)
		return nil
	}
	v := panelView{c: c, base: p.base, next: c.Request.URL.Path, billing: len(subs) > 0,
		billingNote: "Opens " + p.s.provider + ", our payment provider, in a new tab: your card and receipts are on its site."}
	if p.s.provider == "our payment provider" {
		v.billingNote = "Opens our payment provider's site in a new tab: your card and receipts are there."
	}
	for _, s := range subs {
		if s.Status != "canceled" {
			v.running = append(v.running, s)
		}
	}
	return panel(v)
}

// describe says where a subscription stands, for people.
func describe(s Subscription) string {
	date := ""
	if !s.PeriodEnd.IsZero() {
		date = s.PeriodEnd.UTC().Format("2 January 2006")
	}
	if s.Pending != nil && s.Live() {
		name := s.Pending.Product.Name
		if s.Pending.Product.MaxUnits > 1 {
			name += " × " + strconv.Itoa(s.Pending.Units)
		}
		return "Changes to " + name + " on " + s.Pending.At.UTC().Format("2 January 2006")
	}
	switch s.Status {
	case "active":
		if date != "" {
			return "Renews on " + date
		}
		return "Active"
	case "trialing":
		if date != "" {
			return "Trial, renews on " + date
		}
		return "Trial"
	case "past_due":
		return "A payment failed; it will be tried again"
	case "unpaid":
		return "A payment failed"
	case "paused":
		return "Paused"
	case "canceling":
		if date != "" {
			return "Ends on " + date
		}
		return "Ending"
	}
	return "Ended"
}

// label is a subscription's name, with its units when it can have several.
func label(s Subscription) string {
	if s.Product.MaxUnits > 1 {
		return s.Product.Name + " × " + strconv.Itoa(s.Units)
	}
	return s.Product.Name
}
