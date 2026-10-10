// Package accounts maps identities to local accounts, protects signed-in routes and
// deletes accounts.
//
// Accounts are found by identity provider and subject, never linked by email. Deleting
// an account deletes the identity at the provider first, then removes local data in
// one transaction that modules and the app join through OnDelete.
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

// Migrations create the accounts tables. List them first in app.Config.Migrations.
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
	// Email is the provider's verified address, or empty when there is none.
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
	var email sql.NullString
	err := s.app.DB().QueryRowContext(ctx, `
		SELECT id, email, created_at
		FROM accounts
		WHERE id = $1`, id,
	).Scan(&a.ID, &email, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	a.Email = email.String
	return a, err
}

// errDeleted means the identity's account was deleted and must not be recreated.
var errDeleted = errors.New("accounts: the identity was deleted")

// resolve returns the identity's account, creating it on first sign-in. It records
// the provider's address when the provider verified it.
func (s *Accounts) resolve(ctx context.Context, id identity.Identity) (Account, error) {
	provider := s.idp.Name()
	verified := sql.NullString{String: id.Email, Valid: id.EmailVerified && id.Email != ""}

	var a Account
	var email sql.NullString
	err := s.app.DB().QueryRowContext(ctx, `
		SELECT id, email, created_at
		FROM accounts
		WHERE provider = $1 AND subject = $2`, provider, id.Subject,
	).Scan(&a.ID, &email, &a.CreatedAt)
	switch {
	case err == nil:
		if verified.Valid && verified != email {
			if _, err := s.app.DB().ExecContext(ctx, `
				UPDATE accounts
				SET email = $2
				WHERE id = $1`, a.ID, verified,
			); err != nil {
				return Account{}, err
			}
			email = verified
		}
		a.Email = email.String
		return a, nil
	case !errors.Is(err, sql.ErrNoRows):
		return Account{}, err
	}
	return s.create(ctx, provider, id.Subject, verified)
}

// create inserts an account under the identity lock that deletion also takes, unless
// the identity was deleted.
func (s *Accounts) create(ctx context.Context, provider, subject string, email sql.NullString) (Account, error) {
	tx, err := s.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback()
	if err := lockIdentity(ctx, tx, provider, subject); err != nil {
		return Account{}, err
	}
	var deleted bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM deleted_identities
		    WHERE identity_hash = $1
		)`, identityHash(provider, subject),
	).Scan(&deleted); err != nil {
		return Account{}, err
	}
	if deleted {
		return Account{}, errDeleted
	}
	var a Account
	var stored sql.NullString
	err = tx.QueryRowContext(ctx, `
		INSERT INTO accounts (provider, subject, email)
		VALUES ($1, $2, $3)
		ON CONFLICT (provider, subject) DO NOTHING
		RETURNING id, email, created_at`, provider, subject, email,
	).Scan(&a.ID, &stored, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `
			SELECT id, email, created_at
			FROM accounts
			WHERE provider = $1 AND subject = $2`, provider, subject,
		).Scan(&a.ID, &stored, &a.CreatedAt)
	}
	if err != nil {
		return Account{}, err
	}
	a.Email = stored.String
	return a, tx.Commit()
}

func lockIdentity(ctx context.Context, tx *sql.Tx, provider, subject string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "identity:"+provider+":"+subject)
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

func (s *Accounts) setEmail(ctx context.Context, accountID, email string) error {
	_, err := s.app.DB().ExecContext(ctx, `
		UPDATE accounts
		SET email = $2
		WHERE id = $1`, accountID, email,
	)
	if err != nil {
		return fmt.Errorf("accounts: update email: %w", err)
	}
	return nil
}
