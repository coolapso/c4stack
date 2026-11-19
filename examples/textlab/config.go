package main

import (
	"errors"
	"log/slog"
	"os"
	"strings"
)

// config holds the settings Textlab reads from TEXTLAB_* environment variables.
type config struct {
	PublicURL           string
	Addr                string
	SecretKey           string
	CreemAPIKey         string
	CreemWebhookSecret  string
	CreemProductID      string
	CreemCheckoutOrigin string
	// Email goes through SMTP, such as Mailpit in development, or Scaleway. Without
	// Scaleway, credit never expires.
	EmailFrom         string
	SMTPURL           string
	ScalewaySecretKey string
	ScalewayProjectID string
	LogLevel          slog.Level
	LogFormat         string
}

func env(name string) string {
	return os.Getenv("TEXTLAB_" + name)
}

func loadConfig() (config, error) {
	cfg := config{
		PublicURL:           env("PUBLIC_URL"),
		Addr:                env("ADDR"),
		SecretKey:           env("SECRET_KEY"),
		CreemAPIKey:         env("CREEM_API_KEY"),
		CreemWebhookSecret:  env("CREEM_WEBHOOK_SECRET"),
		CreemProductID:      env("CREEM_PRODUCT_ID"),
		CreemCheckoutOrigin: env("CREEM_CHECKOUT_ORIGIN"),
		ScalewaySecretKey:   env("SCALEWAY_SECRET_KEY"),
		ScalewayProjectID:   env("SCALEWAY_PROJECT_ID"),
		EmailFrom:           env("EMAIL_FROM"),
		SMTPURL:             env("SMTP_URL"),
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
	if (cfg.SMTPURL == "") == (cfg.ScalewaySecretKey == "") {
		return config{}, errors.New("set either TEXTLAB_SMTP_URL or TEXTLAB_SCALEWAY_SECRET_KEY and TEXTLAB_SCALEWAY_PROJECT_ID")
	}
	return cfg, nil
}

func (c config) logger() *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.LogLevel}
	if c.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
