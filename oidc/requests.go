package oidc

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// browserCookie holds a random value that ties sign-in requests to the browser that
// started them: a request continues only where its value is. Otherwise someone could
// start a request and have a signed-in victim open its URL.
const browserCookie = "oidc"

// authorize is the authorization endpoint. It checks the request and sends the browser
// to the continue page, with the request sealed in its URL. It is the module's own, not
// the library's, which logs refused requests with the client's state and the whole
// callback URL.
func (p *Provider) authorize(c *gin.Context) {
	ctx := c.Request.Context()
	q := c.Request.URL.Query()
	// Until the client and the redirect URI are known, the answer is a page: an error is
	// never sent to an address the client didn't register.
	client, err := p.client(ctx, q.Get("client_id"))
	if err != nil && !errors.Is(err, ErrUnknownClient) {
		web.Log(c).Error("oidc: client lookup failed", "error", err)
		p.layout.Error(c, http.StatusServiceUnavailable, "Something went wrong", "Please try again in a moment.")
		return
	}
	redirectURI := q.Get("redirect_uri")
	if err != nil || len(q["client_id"]) != 1 || len(q["redirect_uri"]) != 1 ||
		!slices.Contains(client.RedirectURIs, redirectURI) {
		p.layout.Error(c, http.StatusBadRequest, "This sign-in link isn't valid",
			"Go back to the app you came from and sign in again.")
		return
	}
	r, e := parseRequest(q, p.names)
	if e == nil && q.Get("id_token_hint") != "" {
		r.Hint, e = p.hintSubject(ctx, q.Get("id_token_hint"), client.ID)
	}
	if e != nil {
		if len(q["state"]) == 1 {
			e.State = q.Get("state")
		}
		p.answer(c, redirectURI, e)
		return
	}
	value, err := web.SealedCookie(c, browserCookie)
	if err != nil || len(value) != 32 {
		value = make([]byte, 32)
		_, _ = rand.Read(value) // crypto/rand never fails
	}
	web.SetSealedCookie(c, browserCookie, value, requestLifetime)
	h := sha256.Sum256(value)
	r.ClientID, r.RedirectURI, r.Browser, r.Created = client.ID, redirectURI, h[:], time.Now().UnixMilli()
	id, err := p.sealRequest(r)
	if err != nil {
		web.Log(c).Error("oidc: sealing a request failed", "error", err)
		p.answer(c, redirectURI, oidc.ErrServerError())
		return
	}
	c.Redirect(http.StatusSeeOther, continuePath+"?"+url.Values{"id": {id}}.Encode())
}

// request is a sign-in request the authorization endpoint accepted. It is not stored:
// its sealed form is its ID, in the URL of the continue page.
type request struct {
	ClientID    string   `json:"c"`
	RedirectURI string   `json:"r"`
	Scopes      []string `json:"s"`
	State       string   `json:"t,omitempty"`
	Nonce       string   `json:"n,omitempty"`
	Challenge   string   `json:"p"`
	// MaxAge is the most seconds since the person signed in, from max_age, or 0 for
	// prompt=login.
	MaxAge *uint `json:"m,omitempty"`
	// Hint is the subject of the request's id_token_hint.
	Hint string `json:"h,omitempty"`
	// Browser is the SHA-256 of the browser cookie's value.
	Browser []byte `json:"b"`
	// Created is when the request was accepted, in Unix milliseconds.
	Created int64 `json:"a"`
}

// parseRequest checks an authorization request's parameters, once its client and
// redirect URI are known, and returns the request or the error for the client. Only the
// authorization code flow with PKCE S256 is offered, answered in the query. The profile
// scope is granted only when accounts have names.
func parseRequest(q url.Values, names bool) (*request, *oidc.Error) {
	for _, v := range q {
		if len(v) > 1 {
			return nil, oidc.ErrInvalidRequest().WithDescription("a parameter is repeated")
		}
	}
	if q.Has("request") {
		return nil, oidc.ErrRequestNotSupported()
	}
	if q.Has("request_uri") {
		return nil, &oidc.Error{ErrorType: "request_uri_not_supported"}
	}
	if q.Get("response_type") != string(oidc.ResponseTypeCode) {
		return nil, &oidc.Error{ErrorType: "unsupported_response_type", Description: "only the code response type is supported"}
	}
	if m := q.Get("response_mode"); m != "" && m != string(oidc.ResponseModeQuery) {
		return nil, oidc.ErrInvalidRequest().WithDescription("only the query response mode is supported")
	}
	scopes := strings.Fields(q.Get("scope"))
	if !slices.Contains(scopes, oidc.ScopeOpenID) {
		return nil, oidc.ErrInvalidScope().WithDescription("the openid scope is required")
	}
	prompts := strings.Fields(q.Get("prompt"))
	if slices.Contains(prompts, oidc.PromptNone) {
		if len(prompts) > 1 {
			return nil, oidc.ErrInvalidRequest().WithDescription("prompt=none goes alone")
		}
		// The provider never signs in without showing its pages.
		return nil, oidc.ErrLoginRequired()
	}
	var maxAge *uint
	if s := q.Get("max_age"); s != "" {
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return nil, oidc.ErrInvalidRequest().WithDescription("max_age is not a number of seconds")
		}
		v := uint(n)
		maxAge = &v
	}
	if slices.Contains(prompts, oidc.PromptLogin) {
		zero := uint(0)
		maxAge = &zero
	}
	if q.Get("code_challenge_method") != string(oidc.CodeChallengeMethodS256) || !validChallenge(q.Get("code_challenge")) {
		return nil, oidc.ErrInvalidRequest().WithDescription("PKCE with S256 is required")
	}
	if len(q.Get("state")) > maxState || len(q.Get("nonce")) > maxNonce {
		return nil, oidc.ErrInvalidRequest().WithDescription("state or nonce is too long")
	}
	granted := []string{oidc.ScopeOpenID}
	if slices.Contains(scopes, oidc.ScopeEmail) {
		granted = append(granted, oidc.ScopeEmail)
	}
	if names && slices.Contains(scopes, oidc.ScopeProfile) {
		granted = append(granted, oidc.ScopeProfile)
	}
	return &request{
		Scopes:    granted,
		State:     q.Get("state"),
		Nonce:     q.Get("nonce"),
		Challenge: q.Get("code_challenge"),
		MaxAge:    maxAge,
	}, nil
}

// hintSubject returns the subject of an id_token_hint the provider issued to the
// client. An expired hint is fine: it only names who should be signed in.
func (p *Provider) hintSubject(ctx context.Context, hint, clientID string) (string, *oidc.Error) {
	ctx = op.ContextWithIssuer(ctx, p.issuer)
	claims, err := op.VerifyIDTokenHint[*oidc.TokenClaims](ctx, hint, p.op.IDTokenHintVerifier(ctx))
	if err != nil && !errors.As(err, &op.IDTokenHintExpiredError{}) || claims == nil {
		return "", oidc.ErrLoginRequired().WithDescription("the id_token_hint is invalid")
	}
	if !slices.Contains(claims.Audience, clientID) || claims.AuthorizedParty != "" && claims.AuthorizedParty != clientID {
		return "", oidc.ErrInvalidRequest().WithDescription("the id_token_hint was issued to another client")
	}
	return claims.Subject, nil
}

// validChallenge reports whether s is an S256 code challenge: the base64url SHA-256 of
// the verifier, 43 characters.
func validChallenge(s string) bool {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(s) == 43 && len(b) == sha256.Size
}

var errBadRequest = errors.New("oidc: the request is invalid or expired")

const requestLabel = "oidc-request"

// sealRequest returns the request's ID.
func (p *Provider) sealRequest(r *request) (string, error) {
	plain, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(seal(p.requestKey, requestLabel, plain)), nil
}

// openRequest returns the request an ID seals, if it hasn't expired.
func (p *Provider) openRequest(id string) (*request, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return nil, errBadRequest
	}
	plain, err := open(p.requestKey, requestLabel, sealed)
	if err != nil {
		return nil, errBadRequest
	}
	var r request
	if json.Unmarshal(plain, &r) != nil || r.ClientID == "" || len(r.Browser) != sha256.Size {
		return nil, errBadRequest
	}
	age := time.Since(time.UnixMilli(r.Created))
	if age > requestLifetime || age < -time.Minute {
		return nil, errBadRequest
	}
	return &r, nil
}

func challenge(s string) *oidc.CodeChallenge {
	return &oidc.CodeChallenge{Challenge: s, Method: oidc.CodeChallengeMethodS256}
}

// seal encrypts plain with AES-GCM under key. label is authenticated too, so a value
// sealed for one purpose is refused for another.
func seal(key []byte, label string, plain []byte) []byte {
	aead := gcm(key)
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce) // crypto/rand never fails
	return aead.Seal(nonce, nonce, plain, []byte(label))
}

func open(key []byte, label string, sealed []byte) ([]byte, error) {
	aead := gcm(key)
	if len(sealed) < aead.NonceSize() {
		return nil, errBadRequest
	}
	nonce, ciphertext := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	return aead.Open(nil, nonce, ciphertext, []byte(label))
}

func gcm(key []byte) cipher.AEAD {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // keys are always 32 bytes
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return aead
}

// newToken returns 32 random bytes in base64url, for codes and access tokens.
func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand never fails
	return base64.RawURLEncoding.EncodeToString(b)
}
