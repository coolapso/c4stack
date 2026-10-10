package accounts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

var (
	// ErrNotDeleted means the identity provider did not confirm the deletion. Nothing
	// was deleted, and the user can try again.
	ErrNotDeleted = errors.New("accounts: the identity provider did not delete the account")
	// ErrCleanupPending means the identity was deleted at the provider but the local
	// data was not. An operator must run accounts delete-local for the account.
	ErrCleanupPending = errors.New("accounts: local cleanup is pending")
)

// Delete deletes the signed-in account: first the identity at the provider, then, once
// the provider confirms, the local data in one transaction with every OnDelete hook.
// It continues if the browser disconnects, and clears the browser session afterwards.
//
// The two systems are not atomic. If the local transaction fails after the provider
// deleted the identity, Delete logs the account ID and returns ErrCleanupPending.
func (s *Accounts) Delete(c *gin.Context) error {
	account := Current(c)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), time.Minute)
	defer cancel()

	if err := s.idp.Delete(ctx, c.Request); err != nil {
		return fmt.Errorf("%w: %w", ErrNotDeleted, err)
	}
	_ = s.idp.Logout(c.Writer, c.Request)
	if err := s.deleteLocal(ctx, account.ID); err != nil {
		web.Log(c).Error("the identity was deleted but local cleanup failed; run accounts delete-local",
			"account_id", account.ID, "error", err)
		return fmt.Errorf("%w: %w", ErrCleanupPending, err)
	}
	return nil
}

// deleteLocal deletes an account and its data in one transaction.
func (s *Accounts) deleteLocal(ctx context.Context, accountID string) error {
	if !validID(accountID) {
		return ErrNotFound
	}
	tx, err := s.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var provider, subject string
	err = tx.QueryRowContext(ctx, `
		SELECT provider, subject
		FROM accounts
		WHERE id = $1
		FOR UPDATE`, accountID,
	).Scan(&provider, &subject)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := lockIdentity(ctx, tx, provider, subject); err != nil {
		return err
	}
	for _, hook := range s.hooks {
		if err := hook(ctx, tx, accountID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO deleted_identities (identity_hash)
		VALUES ($1)
		ON CONFLICT DO NOTHING`, identityHash(provider, subject),
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM accounts
		WHERE id = $1`, accountID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Accounts) deleteLocalCommand(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: accounts delete-local <account-id>")
	}
	if err := s.deleteLocal(ctx, args[0]); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Deleted account %s and its local data.\n", args[0])
	return nil
}
