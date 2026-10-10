// Package hanko is the Hanko identity provider adapter. Hanko owns credentials and
// sessions: its <hanko-auth> element signs users in and stores the session token in the
// "hanko" cookie, and this adapter validates that token with Hanko on every request.
//
// The Hanko tenant must allow account deletion (the adapter deletes users through
// Hanko's profile flow) and should require email verification. The "hanko" cookie must
// reach the app's host.
package hanko

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coolapso/c4stack/identity"
)

const (
	cookieName       = "hanko"
	maxResponseBytes = 1 << 20
)

//go:embed assets
var embedded embed.FS

// assetFiles are served under the "hanko" prefix. It is one value, so mounting the
// sign-in page and the profile UI adds the files once.
var assetFiles = func() fs.FS {
	sub, err := fs.Sub(embedded, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}()

// Config configures the adapter.
type Config struct {
	// APIURL is the tenant's API URL, such as https://<tenant>.hanko.io. A self-hosted
	// Hanko may have a base path.
	APIURL string
	// SignedInPath is where the login page sends users who arrive without a next
	// path. Zero means "/".
	SignedInPath string
}

// Hanko implements identity.Provider for Hanko.
type Hanko struct {
	api          *url.URL
	signedInPath string
	client       *http.Client
	// loginPath is set by Mount; profile is set by MountProfile.
	loginPath string
	profile   *profileRoutes
}

var _ identity.Provider = (*Hanko)(nil)

// New validates cfg and returns the adapter. It does not contact Hanko.
func New(cfg Config) (*Hanko, error) {
	api, err := url.Parse(strings.TrimSuffix(cfg.APIURL, "/"))
	if err != nil || api.Host == "" {
		return nil, errors.New("hanko: the API URL must be an absolute URL")
	}
	if api.User != nil || api.RawQuery != "" || api.Fragment != "" {
		return nil, errors.New("hanko: the API URL must not contain credentials, a query or a fragment")
	}
	if api.Scheme != "https" && !(api.Scheme == "http" && loopback(api.Hostname())) {
		return nil, errors.New("hanko: the API URL must use https unless the host is loopback")
	}
	signedIn := cfg.SignedInPath
	if signedIn == "" {
		signedIn = "/"
	}
	if !localPath(signedIn) {
		return nil, errors.New("hanko: the signed-in path must be a local path")
	}
	return &Hanko{
		api:          api,
		signedInPath: signedIn,
		client: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		loginPath: "/login",
	}, nil
}

func loopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// localPath reports whether p is a path on this site, not a URL to another one.
func localPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.HasPrefix(p, `/\`)
}

// Name returns "hanko".
func (h *Hanko) Name() string { return "hanko" }

// origin returns the API's origin for CSP rules.
func (h *Hanko) origin() string {
	return h.api.Scheme + "://" + h.api.Host
}

type claims struct {
	Subject  string    `json:"subject"`
	IssuedAt time.Time `json:"issued_at"`
	Email    struct {
		Address    string `json:"address"`
		IsVerified bool   `json:"is_verified"`
	} `json:"email"`
}

// Authenticate validates the session cookie with Hanko. Validation always asks Hanko,
// so a signed-out or deleted user's token stops working immediately.
func (h *Hanko) Authenticate(r *http.Request) (identity.Identity, error) {
	token := sessionToken(r)
	if token == "" {
		return identity.Identity{}, identity.ErrNoSession
	}
	body, _ := json.Marshal(map[string]string{"session_token": token})
	status, data, err := h.post(r.Context(), h.api.String()+"/sessions/validate", "", body)
	if err != nil {
		return identity.Identity{}, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return identity.Identity{}, identity.ErrNoSession
	default:
		return identity.Identity{}, fmt.Errorf("hanko: session validation answered %d", status)
	}
	var result struct {
		IsValid bool   `json:"is_valid"`
		Claims  claims `json:"claims"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return identity.Identity{}, errors.New("hanko: unreadable session validation response")
	}
	if !result.IsValid || result.Claims.Subject == "" {
		return identity.Identity{}, identity.ErrNoSession
	}
	return identity.Identity{
		Subject:       result.Claims.Subject,
		Email:         result.Claims.Email.Address,
		EmailVerified: result.Claims.Email.IsVerified,
		SignedInAt:    result.Claims.IssuedAt,
	}, nil
}

// Logout clears the session cookie, then ends the session at Hanko. The cookie is
// cleared even when Hanko cannot be reached; the error reports that case.
func (h *Hanko) Logout(w http.ResponseWriter, r *http.Request) error {
	token := sessionToken(r)
	clearCookie(w)
	if token == "" {
		return nil
	}
	status, _, err := h.post(r.Context(), h.api.String()+"/logout", token, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusUnauthorized {
		return fmt.Errorf("hanko: logout answered %d", status)
	}
	return nil
}

// Delete deletes the signed-in user through Hanko's profile flow. It returns nil only
// when Hanko reports the account_deleted state.
func (h *Hanko) Delete(ctx context.Context, r *http.Request) error {
	token := sessionToken(r)
	if token == "" {
		return identity.ErrNoSession
	}
	state, err := h.startProfile(ctx, token)
	if err != nil {
		return err
	}
	state, err = h.act(ctx, token, state, "account_delete", map[string]any{})
	if err != nil {
		return err
	}
	if state.Name != "account_deleted" || state.Status != http.StatusOK {
		return fmt.Errorf("hanko: account deletion ended in state %q", state.Name)
	}
	return nil
}

func sessionToken(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1})
}

// post sends a JSON request to Hanko, with the session token as Hanko's cookie when
// token is set. Errors carry the status at most, never Hanko's response body.
func (h *Hanko) post(ctx context.Context, endpoint, token string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("hanko: request failed: %w", redactURL(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, errors.New("hanko: reading the response failed")
	}
	return resp.StatusCode, data, nil
}

// redactURL drops the request URL, which can carry flow IDs, from transport errors.
func redactURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}
