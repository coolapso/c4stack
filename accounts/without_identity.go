package accounts

import (
	"context"
	"database/sql"
	"errors"
)

// ErrHasIdentity means an account has an identity: someone signs in to it, so only
// Delete, through its owner, may remove it.
var ErrHasIdentity = errors.New("accounts: the account has an identity")

// CreateWithoutIdentity creates an account that nobody signs in to, inside tx, such as
// one a marketplace sale opens for an AI agent, which reaches it only through API keys.
// It has no address and no name. No sign-in ever joins it: a new identity joins an
// account only through its verified primary address. The module that created it ends
// it with DeleteWithoutIdentity, or moves what it holds to a signed-in account first.
func (s *Accounts) CreateWithoutIdentity(ctx context.Context, tx *sql.Tx) (Account, error) {
	var a Account
	err := tx.QueryRowContext(ctx, `
		INSERT INTO accounts (primary_email)
		VALUES (NULL)
		RETURNING id, created_at`,
	).Scan(&a.ID, &a.CreatedAt)
	return a, err
}

// DeleteWithoutIdentity deletes an account without an identity inside tx, running every
// OnDelete hook as Delete does. It returns ErrHasIdentity for an account someone signs
// in to, and ErrNotFound for an unknown one.
func (s *Accounts) DeleteWithoutIdentity(ctx context.Context, tx *sql.Tx, accountID string) error {
	if !validID(accountID) {
		return ErrNotFound
	}
	var identified bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM account_identities
		    WHERE account_id = a.id
		)
		FROM accounts a
		WHERE a.id = $1
		FOR UPDATE`, accountID,
	).Scan(&identified)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return err
	case identified:
		return ErrHasIdentity
	}
	return s.deleteTx(ctx, tx, accountID)
}
