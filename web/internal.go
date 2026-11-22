package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// InternalConfig configures NewInternal.
type InternalConfig struct {
	// TokenHashes are the SHA-256 hashes, in hex, of the tokens callers send as
	// "Authorization: Bearer <token>". At least one is required; a second lets a
	// caller's token be replaced without a moment where neither works. Keeping only
	// the hashes, the app holds no credential of its callers'.
	TokenHashes []string
	// MaxBodyBytes limits request bodies. Zero means DefaultMaxBodyBytes.
	MaxBodyBytes int64
	// Logger receives request logs. Zero means slog.Default().
	Logger *slog.Logger
}

// NewInternal returns a Gin engine for an API only other services call, served on a
// server of its own (app.App.Serve) at an address that only they can reach, never
// through the public ingress. Every request needs a bearer token whose SHA-256 is one
// of TokenHashes, or it gets 401.
//
// It has New's request IDs, request logs, panic recovery and body limit, and none of
// what serves browsers: no host check (callers reach it by any name the network gives
// it), no cookies, CSRF or Content Security Policy, and no forwarding headers trusted.
// Log works on its requests; URL and CSRF don't.
func NewInternal(cfg InternalConfig) (*gin.Engine, error) {
	if len(cfg.TokenHashes) == 0 {
		return nil, errors.New("web: an internal engine needs at least one token hash")
	}
	hashes := make([][]byte, 0, len(cfg.TokenHashes))
	for _, h := range cfg.TokenHashes {
		b, err := hex.DecodeString(strings.TrimSpace(h))
		if err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("web: a token hash must be a SHA-256 in hex, 64 characters")
		}
		hashes = append(hashes, b)
	}
	s := &settings{logger: cfg.Logger, maxBody: cfg.MaxBodyBytes}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.maxBody == 0 {
		s.maxBody = DefaultMaxBodyBytes
	}
	engine := gin.New()
	engine.ContextWithFallback = true
	if err := engine.SetTrustedProxies(nil); err != nil {
		return nil, fmt.Errorf("web: trusted proxies: %w", err)
	}
	engine.Use(
		s.begin,
		s.logRequest,
		s.recoverPanic,
		noSniff,
		s.limitBody,
		requireToken(hashes),
	)
	return engine, nil
}

func noSniff(c *gin.Context) {
	c.Header("X-Content-Type-Options", "nosniff")
	c.Next()
}

// requireToken lets a request through only with a bearer token whose SHA-256 is one of
// hashes. It compares every hash in constant time, and logs nothing of the header.
func requireToken(hashes [][]byte) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if ok && token != "" {
			sum := sha256.Sum256([]byte(token))
			match := 0
			for _, h := range hashes {
				match |= subtle.ConstantTimeCompare(sum[:], h)
			}
			if match == 1 {
				c.Next()
				return
			}
		}
		c.Header("WWW-Authenticate", "Bearer")
		c.AbortWithStatus(http.StatusUnauthorized)
	}
}
