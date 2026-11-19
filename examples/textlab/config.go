package main

import (
	"errors"
	"log/slog"
	"os"
	"strings"
)

// config holds the settings Textlab reads from TEXTLAB_* environment variables.
type config struct {
	PublicURL string
	Addr      string
	SecretKey string
	// TrustedProxies are the proxies in front of Textlab, whose forwarding headers give
	// the client's address.
	TrustedProxies      []string
	CreemAPIKey         string
	CreemWebhookSecret  string
	CreemProductID      string
	CreemCheckoutOrigin string
	// CreemProProductID is the recurring product Textlab Pro sells. Without it, Textlab
	// sells no subscription.
	CreemProProductID string
	// Email goes through SMTP, such as Mailpit in development, or Scaleway. Without
	// Scaleway, credit never expires.
	EmailFrom         string
	SMTPURL           string
	ScalewaySecretKey string
	ScalewayProjectID string
	// One service that may sign people in with Textlab's accounts over OpenID Connect,
	// such as an app of your own. Without it, the provider has no clients.
	OIDCClientID     string
	OIDCClientSecret string
	OIDCRedirectURIs []string
	LogLevel         slog.Level
	LogFormat        string
}

func env(name string) string {
	return os.Getenv("TEXTLAB_" + name)
}

func loadConfig() (config, error) {
	cfg := config{
		PublicURL:           env("PUBLIC_URL"),
		Addr:                env("ADDR"),
		SecretKey:           env("SECRET_KEY"),
		TrustedProxies:      list(env("TRUSTED_PROXIES")),
		CreemAPIKey:         env("CREEM_API_KEY"),
		CreemWebhookSecret:  env("CREEM_WEBHOOK_SECRET"),
		CreemProductID:      env("CREEM_PRODUCT_ID"),
		CreemCheckoutOrigin: env("CREEM_CHECKOUT_ORIGIN"),
		CreemProProductID:   env("CREEM_PRO_PRODUCT_ID"),
		ScalewaySecretKey:   env("SCALEWAY_SECRET_KEY"),
		ScalewayProjectID:   env("SCALEWAY_PROJECT_ID"),
		EmailFrom:           env("EMAIL_FROM"),
		SMTPURL:             env("SMTP_URL"),
		OIDCClientID:        env("OIDC_CLIENT_ID"),
		OIDCClientSecret:    env("OIDC_CLIENT_SECRET"),
		OIDCRedirectURIs:    list(env("OIDC_REDIRECT_URIS")),
		LogFormat:           env("LOG_FORMAT"),
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if level := env("LOG_LEVEL"); level != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(level)); err != nil {
			return config{}, errors.New("TEXTLAB_LOG_LEVEL must be debug, info, warn or error")
		}
	}
	var missing []string
	for _, s := range []struct{ name, value string }{
		{"PUBLIC_URL", cfg.PublicURL},
		{"SECRET_KEY", cfg.SecretKey},
		{"EMAIL_FROM", cfg.EmailFrom},
		{"CREEM_API_KEY", cfg.CreemAPIKey},
		{"CREEM_WEBHOOK_SECRET", cfg.CreemWebhookSecret},
		{"CREEM_PRODUCT_ID", cfg.CreemProductID},
	} {
		if s.value == "" {
			missing = append(missing, "TEXTLAB_"+s.name)
		}
	}
	if len(missing) > 0 {
		return config{}, errors.New("set " + strings.Join(missing, ", "))
	}
	if cfg.OIDCClientID != "" && (cfg.OIDCClientSecret == "" || len(cfg.OIDCRedirectURIs) == 0) {
		return config{}, errors.New("with TEXTLAB_OIDC_CLIENT_ID, set TEXTLAB_OIDC_CLIENT_SECRET and TEXTLAB_OIDC_REDIRECT_URIS")
	}
	if (cfg.SMTPURL == "") == (cfg.ScalewaySecretKey == "") {
		return config{}, errors.New("set either TEXTLAB_SMTP_URL or TEXTLAB_SCALEWAY_SECRET_KEY and TEXTLAB_SCALEWAY_PROJECT_ID")
	}
	return cfg, nil
}

// list splits a comma-separated setting, dropping empty items.
func list(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func (c config) logger() *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.LogLevel}
	if c.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
