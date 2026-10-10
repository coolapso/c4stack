// Package postgres connects to PostgreSQL and applies namespaced migrations.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

var keywordSSLMode = regexp.MustCompile(`(^|\s)sslmode\s*=`)

// errInvalidURL deliberately omits the URL and the parser's message, which can quote
// the password.
var errInvalidURL = errors.New("postgres: the database URL cannot be parsed")

// Open connects to PostgreSQL and checks the connection.
//
// For a host that is not a Unix socket or loopback address, Open requires verified TLS
// (sslmode=verify-full, with the system's trusted roots unless sslrootcert is set) when
// neither the URL nor PGSSLMODE chooses a mode. An explicit weaker mode is respected,
// with a warning.
func Open(ctx context.Context, databaseURL string, logger *slog.Logger) (*sql.DB, error) {
	if databaseURL == "" {
		return nil, errors.New("postgres: the database URL is empty")
	}
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, errInvalidURL
	}
	explicit := hasSSLMode(databaseURL) || os.Getenv("PGSSLMODE") != ""
	if remote(cfg) {
		if !explicit {
			cfg, err = pgx.ParseConfig(withSSLMode(databaseURL, "verify-full"))
			if err != nil {
				return nil, errInvalidURL
			}
		} else if mode := sslMode(databaseURL); mode != "verify-full" {
			logger.Warn("the database connection does not verify the server's TLS certificate", "sslmode", mode)
		}
	}

	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	return db, nil
}

// remote reports whether any configured host is reached over the network.
func remote(cfg *pgx.ConnConfig) bool {
	hosts := []string{cfg.Host}
	for _, f := range cfg.Fallbacks {
		hosts = append(hosts, f.Host)
	}
	for _, h := range hosts {
		if strings.HasPrefix(h, "/") || strings.EqualFold(h, "localhost") {
			continue
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			continue
		}
		return true
	}
	return false
}

func isURL(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

func hasSSLMode(dsn string) bool {
	if isURL(dsn) {
		u, err := url.Parse(dsn)
		return err == nil && u.Query().Has("sslmode")
	}
	return keywordSSLMode.MatchString(dsn)
}

// sslMode returns the mode a DSN or PGSSLMODE chooses, for the warning message.
func sslMode(dsn string) string {
	if isURL(dsn) {
		if u, err := url.Parse(dsn); err == nil && u.Query().Has("sslmode") {
			return u.Query().Get("sslmode")
		}
	} else if loc := keywordSSLMode.FindStringIndex(dsn); loc != nil {
		rest := strings.TrimSpace(dsn[loc[1]:])
		if fields := strings.Fields(rest); len(fields) > 0 {
			return strings.Trim(fields[0], "'")
		}
	}
	return os.Getenv("PGSSLMODE")
}

func withSSLMode(dsn, mode string) string {
	if isURL(dsn) {
		u, err := url.Parse(dsn)
		if err != nil {
			return dsn
		}
		q := u.Query()
		q.Set("sslmode", mode)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " sslmode=" + mode
}
