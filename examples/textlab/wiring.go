package main

import (
	"context"
	"database/sql"
	"embed"
	"io/fs"
	"net/http"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/apikeys"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/credits"
	"github.com/coolapso/c4stack/credits/expiry"
	"github.com/coolapso/c4stack/email"
	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/oidc"
	"github.com/coolapso/c4stack/payments"
	"github.com/coolapso/c4stack/subscriptions"
	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

//go:embed static
var staticFiles embed.FS

// providers are the external services Textlab uses. Tests pass their own.
type providers struct {
	identity identity.Provider
	// login mounts the identity provider's sign-in page; nil skips it.
	login interface {
		Mount(r gin.IRouter, layout ui.Layout)
	}
	payments payments.Provider
	// email reports delivery; without it, credit never expires.
	email email.TrackingSender
}

// textlab holds what the app's own handlers use.
type textlab struct {
	accounts *accounts.Accounts
	keys     *apikeys.Keys
	credits  *credits.Credits
	// subs sells the plans; nil when Textlab sells none.
	subs   *subscriptions.Subscriptions
	layout ui.Layout
	db     *sql.DB
}

// handlers are what Textlab serves: the public site, and the internal API when its
// settings give it one.
type handlers struct {
	public http.Handler
	// internal is the API only other services call (internal.go); nil without
	// TEXTLAB_INTERNAL_ADDR.
	internal http.Handler
}

// build wires the modules and routes. It is separate from setup so tests can pass
// their own providers.
func build(a *app.App, cfg config, p providers) (handlers, error) {
	// Accounts have names: people give one after signing up, and services that sign in
	// with Textlab get it with the profile scope.
	accts, err := accounts.New(a, p.identity, accounts.Config{Names: true})
	if err != nil {
		return handlers{}, err
	}
	keys, err := apikeys.New(a, apikeys.Config{Prefix: "tl"})
	if err != nil {
		return handlers{}, err
	}
	cr, err := credits.New(a, accts, credits.Config{
		Payments: p.payments,
		Policy: credits.Policy{
			MinPurchaseCents: 500,
			MaxPurchaseCents: 50_000,
			FeeBasisPoints:   390,
			FeeFixedCents:    40,
		},
		Holds: heldByJobs,
	})
	if err != nil {
		return handlers{}, err
	}
	if p.email != nil {
		if _, err := expiry.New(a, cr, expiry.Config{
			Sender:     p.email,
			Policy:     expiry.Policy{InactiveMonths: 12, NoticeDays: 30},
			AppName:    "Textlab",
			CreditsURL: cfg.PublicURL + "/account/credits",
			Layout:     emailLayout,
		}); err != nil {
			return handlers{}, err
		}
	}

	provider, err := oidcProvider(a, accts, cfg, p.identity)
	if err != nil {
		return handlers{}, err
	}
	// The plans make analyses cheaper while their subscription is in effect (plans.go).
	var subs *subscriptions.Subscriptions
	if seller, ok := p.payments.(payments.Subscriber); ok && cfg.sellsPlans() {
		if subs, err = subscriptions.New(a, accts, subscriptions.Config{
			Payments:          seller,
			ProviderName:      "Creem",
			Products:          planProducts(cfg),
			ChangeAtPeriodEnd: changeAtPeriodEnd,
		}); err != nil {
			return handlers{}, err
		}
	}

	gin.SetMode(gin.ReleaseMode)
	// In development, Creem's webhooks come through the tunnel (task dev:tunnel at the
	// repository root), addressed to its hostname, so a local Textlab answers any host.
	// Forms still post only from the public URL.
	r, err := web.New(web.Config{
		PublicURL:      cfg.PublicURL,
		SecretKey:      cfg.SecretKey,
		TrustedProxies: cfg.TrustedProxies,
		AnyHost:        cfg.local(),
	})
	if err != nil {
		return handlers{}, err
	}
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return handlers{}, err
	}
	assets := ui.NewAssets("/assets")
	if err := assets.Add("app", static); err != nil {
		return handlers{}, err
	}
	t := &textlab{accounts: accts, keys: keys, credits: cr, subs: subs, layout: consoleLayout(assets), db: a.DB()}

	r.GET("/assets/*path", assets.Serve)
	r.NoRoute(t.layout.NotFound)
	r.GET("/", t.home)
	if p.login != nil {
		p.login.Mount(r, t.layout)
	}
	provider.Mount(r, t.layout)

	// The account pages, and the console's own signed-in pages beside them.
	accts.Mount(r, t.layout)
	console := r.Group("/account", accts.RequireSignIn)
	if subs != nil {
		// The overview's plans go on to the provider's checkout. A change asks first, with
		// what it costs; the module's own routes start, change and cancel plans.
		console.GET("", web.AllowCSP(subs.CheckoutCSP()), t.overview)
		console.GET("/plan/change/:product", t.changePlan)
		subs.Mount(console.Group("/subscriptions"), t.layout)
		r.POST("/webhooks/subscriptions", subs.Webhook)
	} else {
		console.GET("", t.overview)
	}
	console.GET("/jobs", t.jobs)
	keys.Mount(console.Group("/api-keys"), t.layout)
	cr.Mount(console.Group("/credits"), t.layout)

	r.POST("/webhooks/payments", cr.Webhook)

	api := r.Group("/api/v1", keys.RequireKey, web.MaxBody(64<<10))
	api.GET("/balance", t.balance)
	api.POST("/analyse", t.analyse)
	api.POST("/jobs", t.createJob)
	api.POST("/jobs/:id/complete", t.completeJob)

	h := handlers{public: r}
	if cfg.InternalAddr != "" {
		if h.internal, err = internalAPI(cfg, subs); err != nil {
			return handlers{}, err
		}
	}
	return h, nil
}

// oidcProvider makes Textlab an OpenID Connect provider for the client in its
// settings, if any. Every account may sign in to it. A sign-in the client wants fresh
// (prompt=login, max_age) goes through the identity provider's confirm page.
func oidcProvider(a *app.App, accts *accounts.Accounts, cfg config, idp identity.Provider) (*oidc.Provider, error) {
	clients := map[string]oidc.Client{}
	if cfg.OIDCClientID != "" {
		clients[cfg.OIDCClientID] = oidc.Client{
			ID:           cfg.OIDCClientID,
			SecretHash:   oidc.HashSecret(cfg.OIDCClientSecret),
			RedirectURIs: cfg.OIDCRedirectURIs,
		}
	}
	oc := oidc.Config{
		Issuer:    cfg.PublicURL,
		SecretKey: cfg.SecretKey,
		Client: func(_ context.Context, id string) (oidc.Client, error) {
			c, ok := clients[id]
			if !ok {
				return oidc.Client{}, oidc.ErrUnknownClient
			}
			return c, nil
		},
	}
	if c, ok := idp.(interface{ ConfirmURL(next string) string }); ok {
		oc.Reauthenticate = c.ConfirmURL
	}
	return oidc.New(a, accts, oc)
}

// heldByJobs reports the credit reserved by an account's pending jobs.
func heldByJobs(ctx context.Context, tx *sql.Tx, accountID string) (int64, error) {
	var held int64
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(sum(reserved_cents), 0)
		FROM jobs
		WHERE account_id = $1 AND status = 'pending'`, accountID,
	).Scan(&held)
	return held, err
}
