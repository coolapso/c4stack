// Package oidc makes the app an OpenID Connect provider, so other services sign people
// in with the app's accounts.
//
// It serves the authorization code flow with PKCE, for confidential clients: discovery,
// the authorization endpoint, the token endpoint, userinfo and the signing keys. The
// subject is the account's ID, which never changes, whichever identity signed in: a
// client keys its users by the issuer and the subject, never by email. The clients, and
// who may sign in to which, are the app's.
//
// A sign-in request is checked, sealed and carried in the URL of the page that
// continues it, so nothing is stored before someone signs in, and it only continues in
// the browser that started it. Codes are single-use, last two minutes and need the
// client's secret and the PKCE verifier. ID tokens are signed with RS256 keys kept
// sealed in the database, which rotate by themselves. docs/architecture.md has the rest.
package oidc

import (
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/ui"
	"github.com/gin-gonic/gin"
	zoidc "github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations create the provider's tables. List them after accounts.Migrations.
var Migrations = app.Migrations{Namespace: "oidc", Files: sub(migrationFiles, "migrations")}

func sub(fsys fs.FS, dir string) fs.FS {
	s, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return s
}

// ErrUnknownClient is what Config.Client returns for an ID that names no client.
var ErrUnknownClient = errors.New("oidc: unknown client")

// The provider's paths. Discovery is at the issuer's /.well-known/openid-configuration.
const (
	authorizePath = "/oidc/authorize"
	continuePath  = "/oidc/continue"
	tokenPath     = "/oidc/token"
	userinfoPath  = "/oidc/userinfo"
	keysPath      = "/oidc/keys"
)

// Its security values. Like sign-in's, they are fixed: each says what it prevents.
const (
	// A sign-in started at a client must finish within this, sign-up with an emailed
	// code included. It bounds how long a request's URL can be used.
	requestLifetime = 30 * time.Minute
	// A client exchanges its code right after the redirect; a short life leaves little
	// time to use one that leaked from a browser's history or a log.
	codeLifetime = 2 * time.Minute
	// Access tokens only read userinfo, right after the exchange.
	accessTokenLifetime = 10 * time.Minute
	// ID tokens are checked once, when the client receives them.
	idTokenLifetime = 10 * time.Minute
	// A signing key signs for keyLifetime, so a key that leaked stops working within it.
	keyLifetime = 30 * 24 * time.Hour
	// A key is published keyOverlap before it signs, so clients that cached the key set
	// already know it, and keyOverlap after, so tokens it signed can still be checked.
	keyOverlap = 24 * time.Hour
	// state and nonce are the client's; these bound a request's URL, which also travels
	// through sign-in as its next page.
	maxState = 512
	maxNonce = 256
)

// Client is a service that signs people in with the app's accounts. Every client is
// confidential: it authenticates to the token endpoint with its secret, and uses PKCE
// as well.
type Client struct {
	// ID is the client_id the service sends.
	ID string
	// SecretHash is HashSecret of the client's secret.
	SecretHash []byte
	// RedirectURIs are where the client receives codes, compared exactly. Each is https,
	// or http on a loopback host; others are ignored.
	RedirectURIs []string
}

// Config configures the provider.
type Config struct {
	// Issuer is the provider's URL: the app's public URL, such as
	// https://account.example.com, without a path. http is accepted only for loopback
	// hosts. Clients key their users by it, so it never changes once they sign people in.
	Issuer string
	// SecretKey is the app's secret key, the same value as web.Config's. The provider
	// derives its own keys from it: for sealed requests, access tokens and the signing
	// keys at rest. Replacing it ends requests in progress and rotates the signing keys.
	SecretKey string
	// Client returns the client with an ID, or ErrUnknownClient.
	Client func(ctx context.Context, id string) (Client, error)
	// Allow decides whether an account may sign in to a client. An error refuses, and
	// the client gets access_denied. Nil lets every account sign in to every client.
	Allow func(ctx context.Context, clientID string, account accounts.Account) error
	// Reauthenticate returns the identity provider's page that asks a signed-in person
	// to sign in again before going on to next, such as identity/local's ConfirmURL.
	// Requests with prompt=login or max_age need it; without it, a request whose sign-in
	// is too old gets login_required.
	Reauthenticate func(next string) string
}

// Provider is the OpenID Connect provider.
type Provider struct {
	app      *app.App
	accounts *accounts.Accounts
	cfg      Config
	issuer   string
	// names says the accounts have names, which the profile scope gives.
	names  bool
	op     *op.Provider
	server http.Handler
	layout ui.Layout
	// requestKey seals sign-in requests; sealKey seals private keys, and keyCheck says
	// which secret key sealed them.
	requestKey []byte
	sealKey    []byte
	keyCheck   []byte
}

// New returns the provider. It registers a job that rotates the signing keys and deletes
// expired codes and tokens.
func New(a *app.App, accts *accounts.Accounts, cfg Config) (*Provider, error) {
	if err := a.Require(Migrations); err != nil {
		return nil, err
	}
	if accts == nil {
		return nil, errors.New("oidc: New needs the accounts module")
	}
	if cfg.Client == nil {
		return nil, errors.New("oidc: Config.Client is required")
	}
	if len(cfg.SecretKey) < 32 {
		return nil, errors.New("oidc: Config.SecretKey must be at least 32 bytes")
	}
	issuer, insecure, err := parseIssuer(cfg.Issuer)
	if err != nil {
		return nil, err
	}
	p := &Provider{app: a, accounts: accts, cfg: cfg, issuer: issuer, names: accts.Names()}
	derive := func(label string, n int) []byte {
		if err != nil {
			return nil
		}
		var k []byte
		k, err = hkdf.Key(sha256.New, []byte(cfg.SecretKey), nil, label, n)
		return k
	}
	p.requestKey = derive("oidc-request", 32)
	p.sealKey = derive("oidc-signing-key", 32)
	p.keyCheck = derive("oidc-signing-key-check", 16)
	tokenKey := derive("oidc-access-token", 32)
	if err != nil {
		return nil, err
	}
	config := &op.Config{
		CodeMethodS256:  true,
		AuthMethodPost:  true,
		SupportedScopes: []string{"openid", "email"},
		SupportedClaims: []string{
			"iss", "sub", "aud", "exp", "iat", "auth_time", "nonce", "azp",
			"at_hash", "c_hash", "email", "email_verified",
		},
	}
	if p.names {
		config.SupportedScopes = append(config.SupportedScopes, "profile")
		config.SupportedClaims = append(config.SupportedClaims, "name")
	}
	copy(config.CryptoKey[:], tokenKey)
	var opts []op.Option
	if insecure {
		opts = append(opts, op.WithAllowInsecure())
	}
	if p.op, err = op.NewProvider(config, &storage{p: p}, op.StaticIssuer(issuer), opts...); err != nil {
		return nil, fmt.Errorf("oidc: %w", err)
	}
	endpoints := op.Endpoints{
		Authorization: op.NewEndpoint(authorizePath),
		Token:         op.NewEndpoint(tokenPath),
		Userinfo:      op.NewEndpoint(userinfoPath),
		JwksURI:       op.NewEndpoint(keysPath),
	}
	// Endpoints left nil are neither served nor advertised: no refresh tokens,
	// introspection, revocation, logout or device flow. The library's callback is
	// unused, because the continue page answers the client itself. No CORS: the endpoints
	// are for servers, and the library's default lets any site read credentialed
	// responses.
	p.server = op.RegisterLegacyServer(server{op.NewLegacyServer(p.op, endpoints)}, http.NotFound,
		op.WithServerCORSOptions(nil))
	a.Every("oidc maintenance", time.Hour, p.maintain)
	return p, nil
}

// server is the library's server, changed where its defaults offer more than the
// provider does.
type server struct {
	*op.LegacyServer
}

// CodeExchange needs the PKCE verifier: the library checks it only when one is sent, for
// confidential clients, which would let a stolen code be redeemed with the client's
// secret alone. The redirect URI, which the library compares with the code's, must
// still be registered, so removing one stops its codes too.
func (s server) CodeExchange(ctx context.Context, r *op.ClientRequest[zoidc.AccessTokenRequest]) (*op.Response, error) {
	if r.Data.CodeVerifier == "" {
		return nil, zoidc.ErrInvalidRequest().WithDescription("code_verifier is required")
	}
	if !slices.Contains(r.Client.RedirectURIs(), r.Data.RedirectURI) {
		return nil, zoidc.ErrInvalidGrant().WithDescription("the redirect_uri is not registered")
	}
	return s.LegacyServer.CodeExchange(ctx, r)
}

// JWTProfile refuses the JWT bearer grant, which the library offers to every provider.
func (s server) JWTProfile(context.Context, *op.Request[zoidc.JWTProfileGrantRequest]) (*op.Response, error) {
	return nil, zoidc.ErrUnsupportedGrantType()
}

// Discovery advertises what the provider offers: the library always lists the implicit
// and JWT bearer grants, other response types, public clients and endpoint
// authentication it doesn't serve.
func (s server) Discovery(ctx context.Context, r *op.Request[struct{}]) (*op.Response, error) {
	resp, err := s.LegacyServer.Discovery(ctx, r)
	if err != nil {
		return nil, err
	}
	if d, ok := resp.Data.(*zoidc.DiscoveryConfiguration); ok {
		d.GrantTypesSupported = []zoidc.GrantType{zoidc.GrantTypeCode}
		d.ResponseTypesSupported = []string{string(zoidc.ResponseTypeCode)}
		d.ResponseModesSupported = []string{string(zoidc.ResponseModeQuery)}
		d.TokenEndpointAuthMethodsSupported = []zoidc.AuthMethod{zoidc.AuthMethodBasic, zoidc.AuthMethodPost}
		d.TokenEndpointAuthSigningAlgValuesSupported = nil
		d.IntrospectionEndpointAuthMethodsSupported = nil
		d.IntrospectionEndpointAuthSigningAlgValuesSupported = nil
		d.RevocationEndpointAuthMethodsSupported = nil
		d.RevocationEndpointAuthSigningAlgValuesSupported = nil
		d.RequestObjectSigningAlgValuesSupported = nil
	}
	return resp, nil
}

// Mount adds the provider's routes at the root of r, which must serve the issuer's
// URL. layout renders the page shown when a sign-in can't continue.
//
//	GET  /.well-known/openid-configuration  discovery
//	GET  /oidc/authorize                    the authorization endpoint
//	GET  /oidc/continue                     signs in if needed, then answers the client
//	POST /oidc/token                        the token endpoint
//	GET and POST /oidc/userinfo             userinfo
//	GET  /oidc/keys                         the signing keys
func (p *Provider) Mount(r gin.IRouter, layout ui.Layout) {
	p.layout = layout
	serve := gin.WrapH(p.server)
	r.GET("/.well-known/openid-configuration", serve)
	r.GET(keysPath, serve)
	r.GET(authorizePath, noStore, p.authorize)
	r.GET(continuePath, p.accounts.RequireSignIn, p.continueSignIn)
	r.POST(tokenPath, noStore, serve)
	r.GET(userinfoPath, noStore, p.userinfo)
	r.POST(userinfoPath, noStore, p.userinfo)
}

// userinfo refuses an access token in the URL, where proxies' logs and browser history
// would keep it. It belongs in the Authorization header, or a POST body.
func (p *Provider) userinfo(c *gin.Context) {
	if c.Request.URL.Query().Has("access_token") {
		c.Header("WWW-Authenticate", `Bearer error="invalid_request"`)
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error":             "invalid_request",
			"error_description": "send the access token in the Authorization header",
		})
		return
	}
	p.server.ServeHTTP(c.Writer, c.Request)
}

// noStore keeps responses that carry codes, tokens or personal data out of caches.
func noStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
}

// NewSecret returns a new client secret and its hash, for Client.SecretHash. Give the
// secret to the client once and keep only the hash.
func NewSecret() (secret string, hash []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand never fails
	secret = base64.RawURLEncoding.EncodeToString(b)
	return secret, HashSecret(secret)
}

// HashSecret returns the hash a client secret is kept as. Secrets are random, so a fast
// hash is enough: there is nothing to guess.
func HashSecret(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// secretMatches compares a presented secret with a client's hash in constant time.
func secretMatches(c Client, secret string) bool {
	return len(c.SecretHash) == sha256.Size && secret != "" &&
		subtle.ConstantTimeCompare(HashSecret(secret), c.SecretHash) == 1
}

// parseIssuer checks an issuer and returns it without a trailing slash, and whether it
// is plain HTTP.
func parseIssuer(raw string) (string, bool, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return "", false, fmt.Errorf("oidc: the issuer %q must be a URL without a path, query or fragment", raw)
	}
	if !secureURL(u) {
		return "", false, fmt.Errorf("oidc: the issuer %q must use https, or http on a loopback host", raw)
	}
	return u.Scheme + "://" + u.Host, u.Scheme == "http", nil
}

// secureURL reports whether u is https, or http on a loopback host.
func secureURL(u *url.URL) bool {
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

// validRedirectURI reports whether a client may receive codes at raw.
func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.IsAbs() && u.Host != "" && u.User == nil && u.Fragment == "" &&
		!strings.HasSuffix(raw, "#") && secureURL(u)
}
