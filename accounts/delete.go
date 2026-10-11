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
	// data was not. An operator must run accounts delete-local for the account; if its
	// address signs up again first, the sign-up finishes the deletion.
	ErrCleanupPending = errors.New("accounts: local cleanup is pending")
)

// Delete deletes the signed-in account: first the identity at the provider, then, once
// the provider confirms, the local data in one transaction with every OnDelete hook.
// It continues if the browser disconnects, and clears the browser session afterwards.
//
// The two systems are not atomic. The account is marked before the provider is asked,
// and the mark is cleared if the provider refuses. If the local transaction fails after
// the provider deleted the identity, Delete logs the account ID and returns
// ErrCleanupPending.
func (s *Accounts) Delete(c *gin.Context) error {
	if s.idp == nil {
		return errors.New("accounts: Delete needs an identity provider")
	}
	account := Current(c)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), time.Minute)
	defer cancel()

	if _, err := s.app.DB().ExecContext(ctx, `
		UPDATE accounts
		SET deletion_started_at = now()
		WHERE id = $1`, account.ID,
	); err != nil {
		return fmt.Errorf("%w: %w", ErrNotDeleted, err)
	}
	if err := s.idp.Delete(ctx, c.Request); err != nil {
		// If clearing the mark fails too, the account's next sign-in clears it.
		_, _ = s.app.DB().ExecContext(ctx, `
			UPDATE accounts
			SET deletion_started_at = NULL
			WHERE id = $1`, account.ID,
		)
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
	if err := s.deleteTx(ctx, tx, accountID); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteTx deletes an account inside tx: it records the account's identities as
// deleted, runs the OnDelete hooks, and deletes the account, which removes its
// identities and anonymises the records that refer to it.
func (s *Accounts) deleteTx(ctx context.Context, tx *sql.Tx, accountID string) error {
	var id string
	err := tx.QueryRowContext(ctx, `
		SELECT id
		FROM accounts
		WHERE id = $1
		FOR UPDATE`, accountID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	identities, err := identitiesOf(ctx, tx, accountID)
	if err != nil {
		return err
	}
	for _, i := range identities {
		if err := lock(ctx, tx, "identity:"+i[0]+":"+i[1]); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO deleted_identities (identity_hash)
			VALUES ($1)
			ON CONFLICT DO NOTHING`, identityHash(i[0], i[1]),
		); err != nil {
			return err
		}
	}
	for _, hook := range s.hooks {
		if err := hook(ctx, tx, accountID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `
		DELETE FROM accounts
		WHERE id = $1`, accountID,
	)
	return err
}

// identitiesOf returns the provider and subject of each of an account's identities.
func identitiesOf(ctx context.Context, tx *sql.Tx, accountID string) ([][2]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT provider, subject
		FROM account_identities
		WHERE account_id = $1
		ORDER BY provider, subject`, accountID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var identities [][2]string
	for rows.Next() {
		var provider, subject string
		if err := rows.Scan(&provider, &subject); err != nil {
			return nil, err
		}
		identities = append(identities, [2]string{provider, subject})
	}
	return identities, rows.Err()
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
