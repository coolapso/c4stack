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
	"github.com/coolapso/c4stack/payments"
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
	// email is optional; without it, credit never expires.
	email email.Sender
}

// textlab holds what the app's own handlers use.
type textlab struct {
	accounts *accounts.Accounts
	keys     *apikeys.Keys
	credits  *credits.Credits
	layout   ui.Layout
	db       *sql.DB
}

// build wires the modules and routes. It is separate from setup so tests can pass
// their own providers.
func build(a *app.App, cfg config, p providers) (http.Handler, error) {
	accts, err := accounts.New(a, p.identity, accounts.Config{})
	if err != nil {
		return nil, err
	}
	keys, err := apikeys.New(a, apikeys.Config{Prefix: "tl"})
	if err != nil {
		return nil, err
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
		return nil, err
	}
	if p.email != nil {
		if _, err := expiry.New(a, cr, expiry.Config{
			Sender:     p.email,
			Policy:     expiry.Policy{InactiveMonths: 12, NoticeDays: 30},
			AppName:    "Textlab",
			CreditsURL: cfg.PublicURL + "/account/credits",
		}); err != nil {
			return nil, err
		}
	}

	gin.SetMode(gin.ReleaseMode)
	r, err := web.New(web.Config{PublicURL: cfg.PublicURL, SecretKey: cfg.SecretKey})
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	assets := ui.NewAssets("/assets")
	if err := assets.Add("app", static); err != nil {
		return nil, err
	}
	t := &textlab{accounts: accts, keys: keys, credits: cr, layout: consoleLayout(assets), db: a.DB()}

	r.GET("/assets/*path", assets.Serve)
	r.NoRoute(t.layout.NotFound)
	r.GET("/", t.home)
	if p.login != nil {
		p.login.Mount(r, t.layout)
	}

	// The account pages, and the console's own signed-in pages beside them.
	accts.Mount(r, t.layout)
	console := r.Group("/account", accts.RequireSignIn)
	console.GET("", t.overview)
	console.GET("/jobs", t.jobs)
	keys.Mount(console, t.layout)
	cr.Mount(console, t.layout)

	r.POST("/webhooks/payments", cr.Webhook)

	api := r.Group("/api/v1", keys.RequireKey, web.MaxBody(64<<10))
	api.GET("/balance", t.balance)
	api.POST("/analyse", t.analyse)
	api.POST("/jobs", t.createJob)
	api.POST("/jobs/:id/complete", t.completeJob)
	return r, nil
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
