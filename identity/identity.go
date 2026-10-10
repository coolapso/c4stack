// Package identity defines the identity provider interface. The provider owns
// credentials and sessions; C4Stack maps each identity to a local account by provider
// name and subject, never by email.
package identity

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// ErrNoSession means the request carries no valid session.
var ErrNoSession = errors.New("identity: no valid session")

// RecentSignIn is how old a session may be for actions that need the owner to be
// present, such as deleting the account or changing passkeys.
const RecentSignIn = 10 * time.Minute

// Identity is the signed-in user as the provider reports it.
type Identity struct {
	// Subject is the provider's stable user ID.
	Subject string
	// Email is the user's primary address. EmailVerified reports whether the provider
	// verified it; unverified addresses must not receive mail.
	Email         string
	EmailVerified bool
	// SignedInAt is when the session was created.
	SignedInAt time.Time
}

// Recent reports whether the session was created within RecentSignIn of now.
func (id Identity) Recent(now time.Time) bool {
	if id.SignedInAt.IsZero() || id.SignedInAt.After(now.Add(time.Minute)) {
		return false
	}
	return now.Sub(id.SignedInAt) <= RecentSignIn
}

// Provider is an identity provider such as Hanko.
type Provider interface {
	// Name identifies the provider in stored accounts, such as "hanko". It must not
	// change once accounts exist.
	Name() string
	// Authenticate validates the request's session. It returns ErrNoSession when there
	// is none or it is no longer valid.
	Authenticate(r *http.Request) (Identity, error)
	// Logout ends the request's session at the provider and clears it in the browser.
	// It clears the browser session even when the provider cannot be reached.
	Logout(w http.ResponseWriter, r *http.Request) error
	// Delete deletes the signed-in user at the provider. It returns nil only once the
	// provider has confirmed the deletion.
	Delete(ctx context.Context, r *http.Request) error
}

type contextKey struct{}

// NewContext returns a context carrying the signed-in identity.
func NewContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// FromContext returns the identity stored by NewContext.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok
}
