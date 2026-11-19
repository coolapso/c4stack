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
	"github.com/coolapso/c4stack/email/smtp"
	"github.com/coolapso/c4stack/identity/local"
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
	local.Migrations,
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

	pay, err := creem.New(creem.Config{
		APIKey:         cfg.CreemAPIKey,
		WebhookSecret:  cfg.CreemWebhookSecret,
		ProductID:      cfg.CreemProductID,
		CheckoutOrigin: cfg.CreemCheckoutOrigin,
	})
	if err != nil {
		return err
	}
	// Email goes through Scaleway, or through SMTP to Mailpit in development. Only
	// Scaleway reports delivery, so credit expires only with it.
	var mailer email.Sender
	var tracking email.TrackingSender
	if cfg.SMTPURL != "" {
		if mailer, err = smtp.New(smtp.Config{URL: cfg.SMTPURL, From: cfg.EmailFrom, FromName: "Textlab"}); err != nil {
			return err
		}
	} else {
		s, err := scaleway.New(scaleway.Config{
			SecretKey: cfg.ScalewaySecretKey,
			ProjectID: cfg.ScalewayProjectID,
			From:      cfg.EmailFrom,
			FromName:  "Textlab",
		})
		if err != nil {
			return err
		}
		mailer, tracking = s, s
	}
	idp, err := local.New(a, local.Config{
		AppName:      "Textlab",
		PublicURL:    cfg.PublicURL,
		SecretKey:    cfg.SecretKey,
		Sender:       mailer,
		SignedInPath: "/account",
	})
	if err != nil {
		return err
	}
	handler, err := build(a, cfg, providers{identity: idp, login: idp, payments: pay, email: tracking})
	if err != nil {
		return err
	}
	a.Serve(web.Server{Addr: cfg.Addr, Handler: handler, HealthPath: "/health"})
	return nil
}
