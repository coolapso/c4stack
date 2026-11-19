package oidc

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// storage is what the library calls. Only the authorization code flow is implemented;
// the rest refuses.
type storage struct {
	p *Provider
}

var _ op.Storage = (*storage)(nil)
var _ op.CanSetUserinfoFromRequest = (*storage)(nil)

var errUnsupported = errors.New("oidc: not supported")

func (s *storage) db() *sql.DB { return s.p.app.DB() }

// CreateAuthRequest, AuthRequestByID and SaveAuthCode serve the library's authorization
// endpoint and callback, which aren't used: the module checks requests itself, and the
// continue page issues codes.
func (s *storage) CreateAuthRequest(context.Context, *oidc.AuthRequest, string) (op.AuthRequest, error) {
	return nil, errUnsupported
}

func (s *storage) AuthRequestByID(context.Context, string) (op.AuthRequest, error) {
	return nil, errUnsupported
}

func (s *storage) SaveAuthCode(context.Context, string, string) error { return errUnsupported }

// AuthRequestByCode takes a code: it marks it used, so it gives tokens once. A code that
// was used already may have leaked, so a second attempt also revokes the access tokens
// it gave (RFC 6749, 4.1.2). The library then checks the client, the redirect URI and
// the PKCE verifier against the grant.
func (s *storage) AuthRequestByCode(ctx context.Context, code string) (op.AuthRequest, error) {
	h := sha256.Sum256([]byte(code))
	g := &grant{codeHash: h[:]}
	var scopes string
	err := s.db().QueryRowContext(ctx, `
		UPDATE oidc_codes
		SET used_at = now()
		WHERE code_hash = $1 AND used_at IS NULL AND expires_at > now()
		RETURNING account_id, client_id, redirect_uri, array_to_string(scopes, ' '), nonce,
		    code_challenge, auth_time, email, email_verified`, h[:],
	).Scan(&g.accountID, &g.clientID, &g.redirectURI, &scopes, &g.nonce,
		&g.challenge, &g.authTime, &g.email, &g.emailVerified)
	if errors.Is(err, sql.ErrNoRows) {
		if err := s.revokeCode(ctx, h[:]); err != nil {
			return nil, err
		}
		return nil, errors.New("oidc: the code is unknown, expired or used")
	}
	if err != nil {
		return nil, err
	}
	g.scopes = strings.Fields(scopes)
	return g, nil
}

// revokeCode marks a used code as used again and deletes its access tokens. The mark
// stops an exchange still in progress from adding one: CreateAccessToken locks the
// code's row and checks it.
func (s *storage) revokeCode(ctx context.Context, codeHash []byte) error {
	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE oidc_codes
		SET reused_at = now()
		WHERE code_hash = $1 AND used_at IS NOT NULL`, codeHash)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM oidc_access_tokens
		WHERE code_hash = $1`, codeHash,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteAuthRequest leaves a used code's row in place until it expires, so a second
// attempt is caught; the maintenance job deletes it.
func (s *storage) DeleteAuthRequest(context.Context, string) error { return nil }

// CreateAccessToken stores an access token for a grant, unless its code was used again
// meanwhile, and returns the token's ID, which the library encrypts into the token.
func (s *storage) CreateAccessToken(ctx context.Context, tr op.TokenRequest) (string, time.Time, error) {
	g, ok := tr.(*grant)
	if !ok {
		return "", time.Time{}, errUnsupported
	}
	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	err = tx.QueryRowContext(ctx, `
		SELECT 1
		FROM oidc_codes
		WHERE code_hash = $1 AND reused_at IS NULL
		FOR SHARE`, g.codeHash,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, oidc.ErrInvalidGrant().WithDescription("the code was used again")
	}
	if err != nil {
		return "", time.Time{}, err
	}
	id := newToken()
	h := sha256.Sum256([]byte(id))
	var expires time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO oidc_access_tokens (
		    token_hash, code_hash, account_id, client_id, scopes, email, email_verified,
		    expires_at
		)
		VALUES (
		    $1, $2, $3, $4, string_to_array($5, ' '), $6, $7,
		    now() + make_interval(secs => $8)
		)
		RETURNING expires_at`,
		h[:], g.codeHash, g.accountID, g.clientID, strings.Join(g.scopes, " "), g.email,
		g.emailVerified, accessTokenLifetime.Seconds(),
	).Scan(&expires)
	if err != nil {
		return "", time.Time{}, err
	}
	return id, expires, tx.Commit()
}

// SetUserinfoFromRequest fills the ID token's claims from the grant: the subject, and
// the address if the email scope was granted.
func (s *storage) SetUserinfoFromRequest(_ context.Context, info *oidc.UserInfo, r op.IDTokenRequest, scopes []string) error {
	g, ok := r.(*grant)
	if !ok {
		return errUnsupported
	}
	info.Subject = g.accountID
	if slices.Contains(scopes, oidc.ScopeEmail) {
		info.Email = g.email
		info.EmailVerified = oidc.Bool(g.emailVerified)
	}
	return nil
}

// SetUserinfoFromToken answers userinfo for an unexpired access token.
func (s *storage) SetUserinfoFromToken(ctx context.Context, info *oidc.UserInfo, tokenID, subject, _ string) error {
	h := sha256.Sum256([]byte(tokenID))
	var accountID, scopes, email string
	var verified bool
	err := s.db().QueryRowContext(ctx, `
		SELECT account_id, array_to_string(scopes, ' '), email, email_verified
		FROM oidc_access_tokens
		WHERE token_hash = $1 AND expires_at > now()`, h[:],
	).Scan(&accountID, &scopes, &email, &verified)
	if errors.Is(err, sql.ErrNoRows) || err == nil && accountID != subject {
		return oidc.ErrAccessDenied().WithDescription("the access token is invalid or expired")
	}
	if err != nil {
		return err
	}
	info.Subject = accountID
	if slices.Contains(strings.Fields(scopes), oidc.ScopeEmail) {
		info.Email = email
		info.EmailVerified = oidc.Bool(verified)
	}
	return nil
}

// SetUserinfoFromScopes is deprecated in the library, which calls it before
// SetUserinfoFromRequest.
func (s *storage) SetUserinfoFromScopes(context.Context, *oidc.UserInfo, string, string, []string) error {
	return nil
}

func (s *storage) GetPrivateClaimsFromScopes(context.Context, string, string, []string) (map[string]any, error) {
	return nil, nil
}

// GetClientByClientID returns the app's client. An unknown client is invalid_client.
func (s *storage) GetClientByClientID(ctx context.Context, id string) (op.Client, error) {
	c, err := s.p.client(ctx, id)
	if errors.Is(err, ErrUnknownClient) {
		return nil, oidc.ErrInvalidClient().WithDescription("unknown client")
	}
	if err != nil {
		return nil, oidc.ErrServerError().WithParent(err)
	}
	return &opClient{c: c, issuer: s.p.issuer}, nil
}

// AuthorizeClientIDSecret checks a client's secret.
func (s *storage) AuthorizeClientIDSecret(ctx context.Context, id, secret string) error {
	c, err := s.p.client(ctx, id)
	if errors.Is(err, ErrUnknownClient) {
		return errors.New("oidc: unknown client")
	}
	if err != nil {
		return oidc.ErrServerError().WithParent(err)
	}
	if !secretMatches(c, secret) {
		return errors.New("oidc: wrong client secret")
	}
	return nil
}

func (s *storage) SigningKey(ctx context.Context) (op.SigningKey, error) {
	return s.p.signingKey(ctx)
}

func (s *storage) SignatureAlgorithms(context.Context) ([]jose.SignatureAlgorithm, error) {
	return []jose.SignatureAlgorithm{jose.RS256}, nil
}

func (s *storage) KeySet(ctx context.Context) ([]op.Key, error) {
	return s.p.keySet(ctx)
}

func (s *storage) Health(context.Context) error { return nil }

// What follows serves flows the provider doesn't offer: refresh tokens, logout,
// revocation, introspection and JWT client assertions.

func (s *storage) CreateAccessAndRefreshTokens(context.Context, op.TokenRequest, string) (string, string, time.Time, error) {
	return "", "", time.Time{}, errUnsupported
}

func (s *storage) TokenRequestByRefreshToken(context.Context, string) (op.RefreshTokenRequest, error) {
	return nil, op.ErrInvalidRefreshToken
}

func (s *storage) TerminateSession(context.Context, string, string) error { return errUnsupported }

func (s *storage) RevokeToken(context.Context, string, string, string) *oidc.Error {
	return oidc.ErrInvalidRequest().WithDescription("revocation is not supported")
}

func (s *storage) GetRefreshTokenInfo(context.Context, string, string) (string, string, error) {
	return "", "", op.ErrInvalidRefreshToken
}

func (s *storage) SetIntrospectionFromToken(context.Context, *oidc.IntrospectionResponse, string, string, string) error {
	return errUnsupported
}

func (s *storage) GetKeyByIDAndClientID(context.Context, string, string) (*jose.JSONWebKey, error) {
	return nil, errUnsupported
}

func (s *storage) ValidateJWTProfileScopes(context.Context, string, []string) ([]string, error) {
	return nil, errUnsupported
}

// grant is a code that was taken: what the library makes tokens from.
type grant struct {
	codeHash      []byte
	accountID     string
	clientID      string
	redirectURI   string
	scopes        []string
	nonce         string
	challenge     string
	authTime      time.Time
	email         string
	emailVerified bool
}

var _ op.AuthRequest = (*grant)(nil)

func (g *grant) GetID() string                         { return hex.EncodeToString(g.codeHash) }
func (g *grant) GetACR() string                        { return "" }
func (g *grant) GetAMR() []string                      { return nil }
func (g *grant) GetAudience() []string                 { return []string{g.clientID} }
func (g *grant) GetAuthTime() time.Time                { return g.authTime }
func (g *grant) GetClientID() string                   { return g.clientID }
func (g *grant) GetCodeChallenge() *oidc.CodeChallenge { return challenge(g.challenge) }
func (g *grant) GetNonce() string                      { return g.nonce }
func (g *grant) GetRedirectURI() string                { return g.redirectURI }
func (g *grant) GetResponseType() oidc.ResponseType    { return oidc.ResponseTypeCode }
func (g *grant) GetResponseMode() oidc.ResponseMode    { return oidc.ResponseModeQuery }
func (g *grant) GetScopes() []string                   { return g.scopes }
func (g *grant) GetState() string                      { return "" }
func (g *grant) GetSubject() string                    { return g.accountID }
func (g *grant) Done() bool                            { return true }

// client returns the app's client with id, keeping only the redirect URIs it may use.
func (p *Provider) client(ctx context.Context, id string) (Client, error) {
	if id == "" {
		return Client{}, ErrUnknownClient
	}
	c, err := p.cfg.Client(ctx, id)
	if err != nil {
		return Client{}, err
	}
	if c.ID != id {
		return Client{}, ErrUnknownClient
	}
	uris := make([]string, 0, len(c.RedirectURIs))
	for _, u := range c.RedirectURIs {
		if validRedirectURI(u) {
			uris = append(uris, u)
		}
	}
	c.RedirectURIs = uris
	return c, nil
}

// opClient is a Client as the library sees it: confidential, authorization code only.
type opClient struct {
	c      Client
	issuer string
}

var _ op.Client = (*opClient)(nil)

func (o *opClient) GetID() string                       { return o.c.ID }
func (o *opClient) RedirectURIs() []string              { return o.c.RedirectURIs }
func (o *opClient) PostLogoutRedirectURIs() []string    { return nil }
func (o *opClient) ApplicationType() op.ApplicationType { return op.ApplicationTypeWeb }
func (o *opClient) AuthMethod() oidc.AuthMethod         { return oidc.AuthMethodBasic }
func (o *opClient) ResponseTypes() []oidc.ResponseType {
	return []oidc.ResponseType{oidc.ResponseTypeCode}
}
func (o *opClient) GrantTypes() []oidc.GrantType { return []oidc.GrantType{oidc.GrantTypeCode} }
func (o *opClient) LoginURL(id string) string {
	return o.issuer + continuePath + "?" + url.Values{"id": {id}}.Encode()
}
func (o *opClient) AccessTokenType() op.AccessTokenType { return op.AccessTokenTypeBearer }
func (o *opClient) IDTokenLifetime() time.Duration      { return idTokenLifetime }
func (o *opClient) DevMode() bool                       { return false }
func (o *opClient) RestrictAdditionalIdTokenScopes() func([]string) []string {
	return func(scopes []string) []string { return scopes }
}
func (o *opClient) RestrictAdditionalAccessTokenScopes() func([]string) []string {
	return func(scopes []string) []string { return scopes }
}
func (o *opClient) IsScopeAllowed(string) bool           { return false }
func (o *opClient) IDTokenUserinfoClaimsAssertion() bool { return true }
func (o *opClient) ClockSkew() time.Duration             { return 0 }
