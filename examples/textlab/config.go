package main

import (
	"errors"
	"log/slog"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
)

// config holds the settings Textlab reads from TEXTLAB_* environment variables.
type config struct {
	PublicURL string
	Addr      string
	SecretKey string
	// TrustedProxies are the proxies in front of Textlab, whose forwarding headers give
	// the client's address.
	TrustedProxies []string
	CreemAPIKey    string
	// CreemWebhookSecret is the secret of the webhook at /webhooks/payments.
	CreemWebhookSecret  string
	CreemProductID      string
	CreemCheckoutOrigin string
	// The plans' recurring products, one per plan and billing period (plans.go).
	// Without them, Textlab sells no plan.
	CreemProMonthly, CreemProYearly, CreemMaxMonthly, CreemMaxYearly string
	// CreemSubscriptionWebhookSecret is the secret of the webhook at
	// /webhooks/subscriptions: Creem gives each webhook its own. The plans need it.
	CreemSubscriptionWebhookSecret string
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
		PublicURL:                      env("PUBLIC_URL"),
		Addr:                           env("ADDR"),
		SecretKey:                      env("SECRET_KEY"),
		TrustedProxies:                 list(env("TRUSTED_PROXIES")),
		CreemAPIKey:                    env("CREEM_API_KEY"),
		CreemWebhookSecret:             env("CREEM_WEBHOOK_SECRET"),
		CreemProductID:                 env("CREEM_PRODUCT_ID"),
		CreemCheckoutOrigin:            env("CREEM_CHECKOUT_ORIGIN"),
		CreemProMonthly:                env("CREEM_PRO_MONTHLY"),
		CreemProYearly:                 env("CREEM_PRO_YEARLY"),
		CreemMaxMonthly:                env("CREEM_MAX_MONTHLY"),
		CreemMaxYearly:                 env("CREEM_MAX_YEARLY"),
		CreemSubscriptionWebhookSecret: env("CREEM_SUBSCRIPTION_WEBHOOK_SECRET"),
		ScalewaySecretKey:              env("SCALEWAY_SECRET_KEY"),
		ScalewayProjectID:              env("SCALEWAY_PROJECT_ID"),
		EmailFrom:                      env("EMAIL_FROM"),
		SMTPURL:                        env("SMTP_URL"),
		OIDCClientID:                   env("OIDC_CLIENT_ID"),
		OIDCClientSecret:               env("OIDC_CLIENT_SECRET"),
		OIDCRedirectURIs:               list(env("OIDC_REDIRECT_URIS")),
		LogFormat:                      env("LOG_FORMAT"),
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
	// Some plans without the others would offer changes to products Creem can't sell.
	plans := []string{cfg.CreemProMonthly, cfg.CreemProYearly, cfg.CreemMaxMonthly, cfg.CreemMaxYearly}
	if set := len(slices.DeleteFunc(plans, func(id string) bool { return id == "" })); set != 0 && set != 4 {
		return config{}, errors.New("set all of TEXTLAB_CREEM_PRO_MONTHLY, TEXTLAB_CREEM_PRO_YEARLY, TEXTLAB_CREEM_MAX_MONTHLY and TEXTLAB_CREEM_MAX_YEARLY, or none")
	}
	// Without it, the subscriptions' webhook would be checked with the payments' secret
	// and every event refused.
	if cfg.sellsPlans() && cfg.CreemSubscriptionWebhookSecret == "" {
		return config{}, errors.New("with the plans' products, set TEXTLAB_CREEM_SUBSCRIPTION_WEBHOOK_SECRET")
	}
	if cfg.OIDCClientID != "" && (cfg.OIDCClientSecret == "" || len(cfg.OIDCRedirectURIs) == 0) {
		return config{}, errors.New("with TEXTLAB_OIDC_CLIENT_ID, set TEXTLAB_OIDC_CLIENT_SECRET and TEXTLAB_OIDC_REDIRECT_URIS")
	}
	if (cfg.SMTPURL == "") == (cfg.ScalewaySecretKey == "") {
		return config{}, errors.New("set either TEXTLAB_SMTP_URL or TEXTLAB_SCALEWAY_SECRET_KEY and TEXTLAB_SCALEWAY_PROJECT_ID")
	}
	return cfg, nil
}

// sellsPlans reports whether Textlab sells its plans: their products are set.
func (c config) sellsPlans() bool { return c.CreemProMonthly != "" }

// local reports whether Textlab runs on this machine, as in development: its public
// URL's host is a loopback address.
func (c config) local() bool {
	u, err := url.Parse(c.PublicURL)
	if err != nil {
		return false
	}
	if u.Hostname() == "localhost" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
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
