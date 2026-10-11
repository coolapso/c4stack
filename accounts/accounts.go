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

// Config configures the module.
type Config struct {
	// LoginPath is where RequireSignIn sends visitors without a session. Zero means
	// "/login".
	LoginPath string
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
	if cfg.LoginPath == "" {
		cfg.LoginPath = "/login"
	}
	if !localPath(cfg.LoginPath) {
		return nil, errors.New("accounts: the login path must be a local path")
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

// errDeleted means the identity's account was deleted and must not be recreated.
var errDeleted = errors.New("accounts: the identity was deleted")

// resolve returns the account of a verified identity, joining or creating one on its
// first sign-in. It keeps the identity's address current.
func (s *Accounts) resolve(ctx context.Context, id identity.Identity) (Account, error) {
	provider := s.idp.Name()
	var a Account
	var email string
	var verified, deleting bool
	err := s.app.DB().QueryRowContext(ctx, `
		SELECT a.id, a.primary_email, a.created_at, a.deletion_started_at IS NOT NULL,
		       i.email, i.email_verified
		FROM account_identities i
		JOIN accounts a ON a.id = i.account_id
		WHERE i.provider = $1 AND i.subject = $2`, provider, id.Subject,
	).Scan(&a.ID, &a.Email, &a.CreatedAt, &deleting, &email, &verified)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return s.join(ctx, provider, id)
	case err != nil:
		return Account{}, err
	}
	if email != id.Email || verified != id.EmailVerified {
		if _, err := s.app.DB().ExecContext(ctx, `
			UPDATE account_identities
			SET email = $3, email_verified = $4, updated_at = now()
			WHERE provider = $1 AND subject = $2`, provider, id.Subject, id.Email, id.EmailVerified,
		); err != nil {
			return Account{}, err
		}
	}
	if deleting {
		// The identity still signs in, so the provider did not delete it.
		if _, err := s.app.DB().ExecContext(ctx, `
			UPDATE accounts
			SET deletion_started_at = NULL
			WHERE id = $1`, a.ID,
		); err != nil {
			return Account{}, err
		}
	}
	return a, nil
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

	var deleting bool
	err = tx.QueryRowContext(ctx, `
		SELECT id, primary_email, created_at, deletion_started_at IS NOT NULL
		FROM accounts
		WHERE primary_email = $1
		FOR UPDATE`, id.Email,
	).Scan(&a.ID, &a.Email, &a.CreatedAt, &deleting)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Account{}, err
	}
	if found && deleting {
		// The account's owner asked to delete it, and the provider now lets a new
		// identity use the address, so the provider deleted the old one. Finish the
		// deletion and start a new account.
		if err := s.deleteTx(ctx, tx, a.ID); err != nil {
			return Account{}, err
		}
		s.app.Log().Info("finished deleting an account when its address signed up again", "account_id", a.ID)
		found = false
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
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("accounts: update email: %d rows, %v", n, err)
		}
	}
	return tx.Commit()
}
