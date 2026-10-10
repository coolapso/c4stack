// Package web provides a Gin engine with the security middleware C4Stack apps share:
// request IDs and logging, safe panic recovery, a host check, security headers with an
// extensible Content Security Policy, body limits and CSRF protection. It also serves an
// http.Handler with health endpoints and graceful shutdown.
//
// The app owns the engine New returns. It adds its own routes and middleware, and decides
// which route groups need CSRF checks.
package web

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/idna"
)

// DefaultMaxBodyBytes is the request body limit when Config.MaxBodyBytes is zero.
const DefaultMaxBodyBytes = 1 << 20

// Config configures the engine New returns.
type Config struct {
	// PublicURL is the canonical origin users reach the app on, such as
	// https://acme.example. Plain HTTP is accepted only for loopback hosts.
	PublicURL string
	// SecretKey signs CSRF tokens and seals cookies. It must be at least 32 bytes and
	// the same on every replica.
	SecretKey string
	// CSP lists sources added to the default Content Security Policy on every route.
	CSP CSP
	// MaxBodyBytes limits request bodies. Zero means DefaultMaxBodyBytes.
	MaxBodyBytes int64
	// TrustedProxies lists the proxies whose forwarding headers set the client IP.
	// None are trusted by default.
	TrustedProxies []string
	// Logger receives request logs. Zero means slog.Default().
	Logger *slog.Logger
}

// settings is the validated configuration shared by every request.
type settings struct {
	origin     string // canonical scheme://host[:port]
	host       string // canonical host[:port] as sent in the Host header
	secure     bool
	csrfKey    []byte
	cookieKey  []byte
	policy     CSP
	maxBody    int64
	logger     *slog.Logger
	csrfCookie string
}

// New returns a Gin engine with C4Stack's security middleware installed.
func New(cfg Config) (*gin.Engine, error) {
	s, err := newSettings(cfg)
	if err != nil {
		return nil, err
	}
	engine := gin.New()
	engine.ContextWithFallback = true
	if err := engine.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		return nil, fmt.Errorf("web: trusted proxies: %w", err)
	}
	engine.Use(
		s.begin,
		s.logRequest,
		s.recoverPanic,
		s.checkHost,
		s.secureHeaders,
		s.limitBody,
		s.issueCSRFCookie,
	)
	return engine, nil
}

func newSettings(cfg Config) (*settings, error) {
	origin, host, secure, err := canonicalOrigin(cfg.PublicURL)
	if err != nil {
		return nil, err
	}
	if len(cfg.SecretKey) < 32 {
		return nil, errors.New("web: the secret key must be at least 32 bytes")
	}
	csrfKey, err := hkdf.Key(sha256.New, []byte(cfg.SecretKey), nil, "csrf", 32)
	if err != nil {
		return nil, err
	}
	cookieKey, err := hkdf.Key(sha256.New, []byte(cfg.SecretKey), nil, "sealed-cookie", 32)
	if err != nil {
		return nil, err
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody == 0 {
		maxBody = DefaultMaxBodyBytes
	}
	if maxBody < 0 {
		return nil, errors.New("web: the body limit cannot be negative")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &settings{
		origin:    origin,
		host:      host,
		secure:    secure,
		csrfKey:   csrfKey,
		cookieKey: cookieKey,
		policy:    defaultCSP().with(cfg.CSP),
		maxBody:   maxBody,
		logger:    logger,
	}
	s.csrfCookie = s.cookieName("csrf")
	return s, nil
}

// cookieName adds the __Host- prefix on HTTPS, which pins the cookie to this host.
func (s *settings) cookieName(name string) string {
	if s.secure {
		return "__Host-" + name
	}
	return name
}

// canonicalOrigin validates a public URL and returns its origin, the Host header value
// requests must carry, and whether it is HTTPS.
func canonicalOrigin(raw string) (origin, host string, secure bool, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", false, fmt.Errorf("web: public URL %q is not an absolute URL", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", "", false, fmt.Errorf("web: public URL %q must be an origin, without a path, query or credentials", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	host, err = canonicalHost(u.Host, scheme)
	if err != nil {
		return "", "", false, err
	}
	switch scheme {
	case "https":
		secure = true
	case "http":
		if !isLoopback(u.Hostname()) {
			return "", "", false, fmt.Errorf("web: public URL %q must use https unless the host is loopback", raw)
		}
	default:
		return "", "", false, fmt.Errorf("web: public URL %q must use http or https", raw)
	}
	return scheme + "://" + host, host, secure, nil
}

// canonicalHost lowercases a host, converts international names to ASCII and drops the
// scheme's default port.
func canonicalHost(hostport, scheme string) (string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, ""
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "" {
		return "", errors.New("web: empty host")
	}
	if net.ParseIP(host) == nil {
		ascii, err := idna.Lookup.ToASCII(host)
		if err != nil {
			return "", fmt.Errorf("web: invalid host %q", host)
		}
		host = ascii
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		return host + ":" + port, nil
	}
	return host, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// state is the per-request data the middleware shares through the Gin context.
type state struct {
	settings  *settings
	requestID string
	logger    *slog.Logger
	policy    CSP
	csrfValue string
}

const stateKey = "c4stack/web"

func current(c *gin.Context) *state {
	if v, ok := c.Get(stateKey); ok {
		return v.(*state)
	}
	panic("web: the request did not pass through an engine built by web.New")
}

// URL returns the absolute URL of path on the app's public origin.
func URL(c *gin.Context, path string) string {
	return current(c).settings.origin + path
}

// Log returns the request's logger, which carries its request ID.
func Log(c *gin.Context) *slog.Logger {
	return current(c).logger
}

// unsafeMethod reports whether a request method can change state.
func unsafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	}
	return true
}
