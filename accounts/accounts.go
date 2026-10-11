// Package accounts maps identities to local accounts, protects signed-in routes and
// deletes accounts.
//
// A sign-in finds its account by the identity provider and subject. An identity the
// app has not seen joins the account that has its verified email address, whichever
// provider it comes from, or gets a new account. Unverified identities cannot sign in.
//
// Deleting an account deletes the identity at the provider first, then removes local
// data in one transaction that modules and the app join through OnDelete.
package accounts

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"time"

	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/identity"
	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations create the accounts tables. List them before the other modules' in
// app.Config.Migrations.
var Migrations = app.Migrations{Namespace: "accounts", Files: sub(migrationFiles, "migrations")}

func sub(fsys fs.FS, dir string) fs.FS {
	s, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return s
}

// ErrNotFound means no account has the given ID.
var ErrNotFound = errors.New("accounts: account not found")

// Account is a local account.
type Account struct {
	ID string
	// Email is the account's primary address, verified by the identity provider.
	Email     string
	CreatedAt time.Time
}

// Paths are where the account pages live, as URL paths. RequireSignIn and the pages use
// them for every redirect and link. A zero field takes the default shown.
type Paths struct {
	Login        string // "/login", the identity provider's sign-in page
	Logout       string // "/logout"
	Unverified   string // "/auth/email-unverified"
	Settings     string // "/account/settings"
	Delete       string // "/account/delete"
	Deleted      string // "/account/deleted"
	EmailChanged string // "/account/email-changed"
}

func (p *Paths) defaults() error {
	for _, f := range []struct {
		path *string
		def  string
	}{
		{&p.Login, "/login"},
		{&p.Logout, "/logout"},
		{&p.Unverified, "/auth/email-unverified"},
		{&p.Settings, "/account/settings"},
		{&p.Delete, "/account/delete"},
		{&p.Deleted, "/account/deleted"},
		{&p.EmailChanged, "/account/email-changed"},
	} {
		if *f.path == "" {
			*f.path = f.def
		}
		if !localPath(*f.path) {
			return fmt.Errorf("accounts: %q is not a local path", *f.path)
		}
	}
	return nil
}

// Config configures the module.
type Config struct {
	Paths Paths
}

// Accounts is the accounts module.
type Accounts struct {
	app   *app.App
	idp   identity.Provider
	cfg   Config
	hooks []func(ctx context.Context, tx *sql.Tx, accountID string) error
}

// New returns the accounts module for idp. It registers the accounts delete-local
// command.
//
// A process that only needs account IDs, such as an API gateway that authenticates with
// API keys, passes a nil idp: it holds no sign-in configuration, and RequireSignIn,
// Mount and Delete are unavailable.
func New(a *app.App, idp identity.Provider, cfg Config) (*Accounts, error) {
	if err := a.Require(Migrations); err != nil {
		return nil, err
	}
	if err := cfg.Paths.defaults(); err != nil {
		return nil, err
	}
	s := &Accounts{app: a, idp: idp, cfg: cfg}
	a.Command(app.Command{
		Name:  "accounts delete-local",
		Usage: "<account-id>",
		Help:  "Delete an account's local data after its identity was deleted at the provider.",
		Run:   s.deleteLocalCommand,
	})
	return s, nil
}

// OnDelete adds a hook to account deletion. Hooks run in the order they were added,
// inside the transaction that deletes the account, so an error rolls everything back.
// Use it for personal data that a foreign key with ON DELETE CASCADE cannot remove.
func (s *Accounts) OnDelete(hook func(ctx context.Context, tx *sql.Tx, accountID string) error) {
	s.hooks = append(s.hooks, hook)
}

// Get returns an account by ID.
func (s *Accounts) Get(ctx context.Context, id string) (Account, error) {
	if !validID(id) {
		return Account{}, ErrNotFound
	}
	var a Account
	err := s.app.DB().QueryRowContext(ctx, `
		SELECT id, primary_email, created_at
		FROM accounts
		WHERE id = $1`, id,
	).Scan(&a.ID, &a.Email, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

var (
	// errDeleted means the identity's account was deleted and must not be recreated.
	errDeleted = errors.New("accounts: the identity was deleted")
	// errDeletionPending means the address belongs to an account whose deletion the
	// identity provider has not confirmed yet.
	errDeletionPending = errors.New("accounts: the address's account is being deleted")
	// errStaleAddress means the address is an account's primary address, but none of
	// the account's identities verifies it any more.
	errStaleAddress = errors.New("accounts: the address belongs to an account that no longer uses it")
)

// resolve returns the account of a verified identity, joining or creating one on its
// first sign-in. It keeps the identity's address current, and the account's primary
// address with it.
func (s *Accounts) resolve(ctx context.Context, id identity.Identity) (Account, error) {
	provider := s.idp.Name()
	var a Account
	var email string
	var verified, started, confirmed bool
	err := s.app.DB().QueryRowContext(ctx, `
		SELECT a.id, a.primary_email, a.created_at,
		       a.deletion_started_at IS NOT NULL, a.deletion_confirmed_at IS NOT NULL,
		       i.email, i.email_verified
		FROM account_identities i
		JOIN accounts a ON a.id = i.account_id
		WHERE i.provider = $1 AND i.subject = $2`, provider, id.Subject,
	).Scan(&a.ID, &a.Email, &a.CreatedAt, &started, &confirmed, &email, &verified)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return s.join(ctx, provider, id)
	case err != nil:
		return Account{}, err
	case confirmed:
		// The provider confirmed the account's deletion through another of its
		// identities, and the local cleanup did not finish. Finish it.
		if err := s.deleteLocal(ctx, a.ID); err != nil && !errors.Is(err, ErrNotFound) {
			return Account{}, err
		}
		return Account{}, errDeleted
	}
	if email != id.Email || verified != id.EmailVerified {
		if err := s.updateAddress(ctx, provider, id, &a); err != nil {
			return Account{}, err
		}
	}
	if started {
		// The identity still signs in, so the provider did not delete it.
		if _, err := s.app.DB().ExecContext(ctx, `
			UPDATE accounts
			SET deletion_started_at = NULL
			WHERE id = $1 AND deletion_confirmed_at IS NULL`, a.ID,
		); err != nil {
			return Account{}, err
		}
	}
	return a, nil
}

// updateAddress records an identity's new address. When no identity of the account
// verifies the account's primary address any more, a newly verified address becomes
// primary, so an address the account no longer uses never leads a new identity to it.
func (s *Accounts) updateAddress(ctx context.Context, provider string, id identity.Identity, a *Account) error {
	if _, err := s.app.DB().ExecContext(ctx, `
		UPDATE account_identities
		SET email = $3, email_verified = $4, updated_at = now()
		WHERE provider = $1 AND subject = $2`, provider, id.Subject, id.Email, id.EmailVerified,
	); err != nil {
		return err
	}
	if !id.EmailVerified || id.Email == a.Email {
		return nil
	}
	var email string
	err := s.app.DB().QueryRowContext(ctx, `
		UPDATE accounts a
		SET primary_email = $2, updated_at = now()
		WHERE a.id = $1
		    AND NOT EXISTS (
		        SELECT 1
		        FROM account_identities i
		        WHERE i.account_id = a.id AND i.email = a.primary_email AND i.email_verified
		    )
		RETURNING primary_email`, a.ID, id.Email,
	).Scan(&email)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil // another identity still verifies the primary address
	case isUniqueViolation(err):
		// Another account has the new address. The old one stays primary, and since no
		// identity verifies it, a new identity with it is turned away rather than joined.
		s.app.Log().Warn("an identity's new address belongs to another account", "account_id", a.ID)
		return nil
	case err != nil:
		return err
	}
	a.Email = email
	return nil
}

// recordUnverified records that an identity's address is not verified. The identity
// cannot sign in, but its previous address must stop counting as verified, or a new
// identity verifying that address could join the account.
func (s *Accounts) recordUnverified(ctx context.Context, id identity.Identity) error {
	_, err := s.app.DB().ExecContext(ctx, `
		UPDATE account_identities
		SET email = COALESCE(NULLIF($3, ''), email), email_verified = false, updated_at = now()
		WHERE provider = $1 AND subject = $2 AND (email_verified OR email <> COALESCE(NULLIF($3, ''), email))`,
		s.idp.Name(), id.Subject, id.Email,
	)
	return err
}

// join adds a new identity to the account with its verified email, or to a new account.
func (s *Accounts) join(ctx context.Context, provider string, id identity.Identity) (Account, error) {
	tx, err := s.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback()
	// The identity lock pairs with deletion; the email lock stops two new identities
	// with one address from creating two accounts.
	if err := lock(ctx, tx, "identity:"+provider+":"+id.Subject); err != nil {
		return Account{}, err
	}
	if err := lock(ctx, tx, "email:"+id.Email); err != nil {
		return Account{}, err
	}
	var deleted bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM deleted_identities
		    WHERE identity_hash = $1
		)`, identityHash(provider, id.Subject),
	).Scan(&deleted); err != nil {
		return Account{}, err
	}
	if deleted {
		return Account{}, errDeleted
	}
	var a Account
	// A concurrent first sign-in of the same identity may have joined it already.
	err = tx.QueryRowContext(ctx, `
		SELECT a.id, a.primary_email, a.created_at
		FROM account_identities i
		JOIN accounts a ON a.id = i.account_id
		WHERE i.provider = $1 AND i.subject = $2`, provider, id.Subject,
	).Scan(&a.ID, &a.Email, &a.CreatedAt)
	if err == nil {
		return a, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Account{}, err
	}

	var started, confirmed, current bool
	err = tx.QueryRowContext(ctx, `
		SELECT a.id, a.primary_email, a.created_at,
		       a.deletion_started_at IS NOT NULL, a.deletion_confirmed_at IS NOT NULL,
		       EXISTS (
		           SELECT 1
		           FROM account_identities i
		           WHERE i.account_id = a.id AND i.email = a.primary_email AND i.email_verified
		       )
		FROM accounts a
		WHERE a.primary_email = $1
		FOR UPDATE`, id.Email,
	).Scan(&a.ID, &a.Email, &a.CreatedAt, &started, &confirmed, &current)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Account{}, err
	}
	switch {
	case found && confirmed:
		// The provider confirmed this account's deletion and the local cleanup did not
		// finish. Finish it, and start a new account.
		if err := s.deleteTx(ctx, tx, a.ID); err != nil {
			return Account{}, err
		}
		s.app.Log().Info("finished deleting an account when its address signed up again", "account_id", a.ID)
		found = false
	case found && started:
		return Account{}, errDeletionPending
	case found && !current:
		s.app.Log().Warn("a new identity's address is the primary address of an account none of whose identities uses it; an operator must decide", "account_id", a.ID)
		return Account{}, errStaleAddress
	}
	if !found {
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO accounts (primary_email)
			VALUES ($1)
			RETURNING id, primary_email, created_at`, id.Email,
		).Scan(&a.ID, &a.Email, &a.CreatedAt); err != nil {
			return Account{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO account_identities (account_id, provider, subject, email, email_verified)
		VALUES ($1, $2, $3, $4, $5)`, a.ID, provider, id.Subject, id.Email, id.EmailVerified,
	); err != nil {
		return Account{}, err
	}
	return a, tx.Commit()
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// lock takes a transaction-scoped advisory lock on a name.
func lock(ctx context.Context, tx *sql.Tx, name string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, name)
	return err
}

func identityHash(provider, subject string) []byte {
	sum := sha256.Sum256([]byte(provider + "\x00" + subject))
	return sum[:]
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validID(id string) bool {
	return uuidPattern.MatchString(strings.ToLower(id))
}

// localPath reports whether p is a path on this site, not a URL to another one.
func localPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.HasPrefix(p, `/\`)
}

// setEmail records an identity's confirmed new address as the account's primary one.
func (s *Accounts) setEmail(ctx context.Context, accountID, subject, email string) error {
	tx, err := s.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`
			UPDATE accounts
			SET primary_email = $2, updated_at = now()
			WHERE id = $1`, []any{accountID, email}},
		{`
			UPDATE account_identities
			SET email = $4, email_verified = true, updated_at = now()
			WHERE account_id = $1 AND provider = $2 AND subject = $3`, []any{accountID, s.idp.Name(), subject, email}},
	} {
		res, err := tx.ExecContext(ctx, q.sql, q.args...)
		if err != nil {
			return fmt.Errorf("accounts: update email: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("accounts: update email: %w", err)
		}
		if n != 1 {
			return fmt.Errorf("accounts: updating the email changed %d rows", n)
		}
	}
	return tx.Commit()
}
