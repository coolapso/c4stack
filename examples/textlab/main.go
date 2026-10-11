// Command textlab is an example C4Stack app: a small paid text API. It shows a custom
// layout, API keys, prepaid credit with holds and expiry, and an app table that
// references accounts.
package main

import (
	"context"
	"os"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/apikeys"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/credits"
	"github.com/coolapso/c4stack/credits/expiry"
	"github.com/coolapso/c4stack/email"
	"github.com/coolapso/c4stack/email/scaleway"
	"github.com/coolapso/c4stack/identity/hanko"
	"github.com/coolapso/c4stack/payments/creem"
	"github.com/coolapso/c4stack/web"

	"example.com/textlab/migrations"
)

func main() {
	app.Main(app.Config{
		Name:        "textlab",
		DatabaseURL: os.Getenv("TEXTLAB_DATABASE_URL"),
		Migrations:  appMigrations,
		Setup:       setup,
	})
}

var appMigrations = []app.Migrations{
	accounts.Migrations,
	apikeys.Migrations,
	credits.Migrations,
	expiry.Migrations,
	{Namespace: "app", Files: migrations.Files},
}

// setup builds the real providers from the configuration and wires the app.
func setup(ctx context.Context, a *app.App) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	a.SetLogger(cfg.logger())

	idp, err := hanko.New(hanko.Config{APIURL: cfg.HankoAPIURL, SignedInPath: "/console"})
	if err != nil {
		return err
	}
	pay, err := creem.New(creem.Config{
		APIKey:         cfg.CreemAPIKey,
		WebhookSecret:  cfg.CreemWebhookSecret,
		ProductID:      cfg.CreemProductID,
		CheckoutOrigin: cfg.CreemCheckoutOrigin,
	})
	if err != nil {
		return err
	}
	var mail email.TrackingSender
	if cfg.ScalewaySecretKey != "" {
		if mail, err = scaleway.New(scaleway.Config{
			SecretKey: cfg.ScalewaySecretKey,
			ProjectID: cfg.ScalewayProjectID,
			From:      cfg.EmailFrom,
			FromName:  "Textlab",
		}); err != nil {
			return err
		}
	}
	handler, err := build(a, cfg, providers{identity: idp, payments: pay, email: mail})
	if err != nil {
		return err
	}
	a.Serve(web.Server{Addr: cfg.Addr, Handler: handler, HealthPath: "/health"})
	return nil
}
