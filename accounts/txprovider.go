package accounts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/coolapso/c4stack/identity"
)

// signedIn is the SignedIn hook a TxProvider runs inside the transaction that signs a
// user in: it finds the identity's account, or joins or creates one, as resolve does,
// but never writes an address. The provider locked its user first, so a deletion or an
// address change of the same user waits for it, or it for them.
func (s *Accounts) signedIn(ctx context.Context, tx *sql.Tx, id identity.Identity) error {
	if id.Subject == "" || id.Email == "" || !id.EmailVerified {
		return fmt.Errorf("accounts: an identity without a verified address: %w", identity.ErrRefused)
	}
	provider := s.idp.Name()
	var accountID string
	var started, confirmed bool
	err := tx.QueryRowContext(ctx, `
		SELECT a.id, a.deletion_started_at IS NOT NULL, a.deletion_confirmed_at IS NOT NULL
		FROM account_identities i
		JOIN accounts a ON a.id = i.account_id
		WHERE i.provider = $1 AND i.subject = $2`, provider, id.Subject,
	).Scan(&accountID, &started, &confirmed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err := s.joinTx(ctx, tx, provider, id)
		return err
	case err != nil:
		return err
	case confirmed:
		// Another provider confirmed the account's deletion and its local cleanup did
		// not finish. Finishing it here would roll back with the refused sign-in.
		s.app.Log().Warn("a sign-in found an account whose deletion was confirmed but not finished; run accounts delete-local", "account_id", accountID)
		return errDeleted
	case started:
		// The identity still signs in, so no provider is deleting it.
		_, err := tx.ExecContext(ctx, `
			UPDATE accounts
			SET deletion_started_at = NULL
			WHERE id = $1 AND deletion_confirmed_at IS NULL`, accountID,
		)
		return err
	}
	return nil
}

// addressChanged is the AddressChanged hook a TxProvider runs inside the transaction that
// changes a user's address: the identity's recorded address and the account's primary
// address change with the provider's user, or none of them do.
func (s *Accounts) addressChanged(ctx context.Context, tx *sql.Tx, subject, email string) error {
	if subject == "" || email == "" {
		return errors.New("accounts: an address change needs a subject and an address")
	}
	provider := s.idp.Name()
	var accountID string
	err := tx.QueryRowContext(ctx, `
		SELECT account_id
		FROM account_identities
		WHERE provider = $1 AND subject = $2`, provider, subject,
	).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("accounts: the identity has no account: %w", ErrNotFound)
	}
	if err != nil {
		return err
	}
	// The email lock pairs with join, which locks the address before it looks for the
	// account that has it.
	if err := lock(ctx, tx, "email:"+email); err != nil {
		return err
	}
	var taken bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM accounts
		    WHERE primary_email = $1 AND id <> $2
		)`, email, accountID,
	).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return identity.ErrAddressTaken
	}
	return setEmailTx(ctx, tx, accountID, provider, subject, email)
}

// setEmailTx records an identity's confirmed new address as the account's primary one.
func setEmailTx(ctx context.Context, tx *sql.Tx, accountID, provider, subject, email string) error {
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
			WHERE account_id = $1 AND provider = $2 AND subject = $3`, []any{accountID, provider, subject, email}},
	} {
		res, err := tx.ExecContext(ctx, q.sql, q.args...)
		if isUniqueViolation(err) {
			return identity.ErrAddressTaken
		}
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
	return nil
}

// deleteProviderUsers deletes the users a TxProvider keeps for an account's identities,
// inside the transaction that deletes the account.
func (s *Accounts) deleteProviderUsers(ctx context.Context, tx *sql.Tx, accountID string, done map[string]bool) error {
	if s.txp == nil {
		return nil
	}
	subjects, err := subjectsOf(ctx, tx, accountID, s.txp.Name())
	if err != nil {
		return err
	}
	for _, subject := range subjects {
		if done[subject] {
			continue
		}
		if err := s.txp.DeleteTx(ctx, tx, subject); err != nil {
			return fmt.Errorf("accounts: deleting the provider's user: %w", err)
		}
		done[subject] = true
	}
	return nil
}

// subjectsOf returns the subjects of an account's identities at one provider.
func subjectsOf(ctx context.Context, tx *sql.Tx, accountID, provider string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT subject
		FROM account_identities
		WHERE account_id = $1 AND provider = $2
		ORDER BY subject`, accountID, provider,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subjects []string
	for rows.Next() {
		var subject string
		if err := rows.Scan(&subject); err != nil {
			return nil, err
		}
		subjects = append(subjects, subject)
	}
	return subjects, rows.Err()
}
