// Package identity defines the identity provider interface. The provider owns
// credentials and sessions. C4Stack maps each identity to a local account by provider
// name and subject, and joins a new identity to the account that has its verified email
// address.
package identity

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"
)

var (
	// ErrNoSession means the request carries no valid session.
	ErrNoSession = errors.New("identity: no valid session")
	// ErrRefused means the accounts side turned an identity away, such as one whose
	// account is being deleted. A provider that signs people in tells them it cannot
	// sign them in to this account.
	ErrRefused = errors.New("identity: the account refused the sign-in")
	// ErrAddressTaken means another account has the address an identity changes to.
	ErrAddressTaken = errors.New("identity: another account has this address")
)

// RecentSignIn is how old a session may be for actions that need the owner to be
// present, such as deleting the account or changing passkeys.
const RecentSignIn = 5 * time.Minute

// Identity is the signed-in user as the provider reports it.
type Identity struct {
	// Subject is the provider's stable user ID.
	Subject string
	// Email is the user's primary address. EmailVerified reports whether the provider
	// verified it; an unverified identity cannot sign in to an account.
	Email         string
	EmailVerified bool
	// SignedInAt is when the session was created.
	SignedInAt time.Time
	// Method is how the session signed in, such as "code" or "passkey". It is empty
	// from providers that don't say.
	Method string
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

// TxProvider is a provider that keeps its users in the app's own database. accounts lends
// it hooks that run inside the provider's transactions, and deletes its users inside the
// transaction that deletes the account, so neither side is ever left half changed.
type TxProvider interface {
	Provider
	// UseAccounts receives the accounts side's hooks. accounts.New calls it once.
	UseAccounts(h AccountHooks)
	// SessionTx locks the user of the request's session inside tx, and returns the
	// session's identity as it is once locked. It returns ErrNoSession when the session
	// ended meanwhile, such as by a recovery that committed while the request waited.
	// accounts authorises a deletion with it, in the transaction that deletes.
	SessionTx(ctx context.Context, tx *sql.Tx, r *http.Request) (Identity, error)
	// DeleteTx deletes the user with this subject inside tx, and nothing when there is
	// none. It locks the user first, so it waits for the user's other changes.
	DeleteTx(ctx context.Context, tx *sql.Tx, subject string) error
}

// AccountHooks are the accounts side of a sign-in and of an address change. The provider
// calls them inside its own transaction, after locking its user, and rolls everything
// back when one fails.
type AccountHooks struct {
	// SignedIn finds, joins or creates the identity's account. An error wrapping
	// ErrRefused means the account turns the identity away.
	SignedIn func(ctx context.Context, tx *sql.Tx, id Identity) error
	// AddressChanged records the identity's new verified address and makes it the
	// account's primary address. It returns ErrAddressTaken if another account has it.
	AddressChanged func(ctx context.Context, tx *sql.Tx, subject, email string) error
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
