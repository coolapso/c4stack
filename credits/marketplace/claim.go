package marketplace

import (
	"context"
	"database/sql"
	"errors"

	"github.com/coolapso/c4stack/credits"
)

// Claimable is what a claim link would move into the owner's account.
type Claimable struct {
	// BalanceCents is the account's credit now.
	BalanceCents int64
	// Keys counts its API keys that work.
	Keys int
}

// Claimable reports what a claim link would move, such as for the page that asks to
// confirm. It returns ErrClaimInvalid for a link that can't be claimed.
func (m *Marketplace) Claimable(ctx context.Context, token string) (Claimable, error) {
	var accountID string
	err := m.app.DB().QueryRowContext(ctx, `
		SELECT account_id
		FROM marketplace_grants
		WHERE claim_token_hash = $1
		    AND account_id IS NOT NULL AND claimed_at IS NULL AND revoked_at IS NULL AND ended_at IS NULL`,
		hashToken(token),
	).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return Claimable{}, ErrClaimInvalid
	}
	if err != nil {
		return Claimable{}, err
	}
	s, err := m.credits.Summary(ctx, accountID)
	if err != nil {
		return Claimable{}, err
	}
	var c Claimable
	c.BalanceCents = s.BalanceCents
	err = m.app.DB().QueryRowContext(ctx, `
		SELECT count(*)
		FROM api_keys
		WHERE account_id = $1 AND revoked_at IS NULL`, accountID,
	).Scan(&c.Keys)
	return c, err
}

// Claim makes the account a claim link names the owner's, in one transaction: its
// credit and purchases move to intoAccountID, an account someone signs in to, with its
// API keys, which keep working, and whatever the app's OnClaim hooks move; then the
// claimed account is deleted, and the link stops working. It returns ErrClaimInvalid
// for a link that can't be claimed, and ErrBusy while the account has requests in
// progress.
func (m *Marketplace) Claim(ctx context.Context, token, intoAccountID string) error {
	tx, err := m.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var grantRef, fromAccountID string
	err = tx.QueryRowContext(ctx, `
		SELECT id, account_id
		FROM marketplace_grants
		WHERE claim_token_hash = $1
		    AND account_id IS NOT NULL AND claimed_at IS NULL AND revoked_at IS NULL AND ended_at IS NULL
		FOR UPDATE`, hashToken(token),
	).Scan(&grantRef, &fromAccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrClaimInvalid
	}
	if err != nil {
		return err
	}
	if fromAccountID == intoAccountID {
		return ErrClaimInvalid
	}
	_, err = m.credits.Transfer(ctx, tx, fromAccountID, intoAccountID, "marketplace-claim:"+grantRef,
		"Credit from the marketplace account you claimed")
	switch {
	case errors.Is(err, credits.ErrHeld):
		return ErrBusy
	case errors.Is(err, credits.ErrNotFound):
		return ErrClaimInvalid
	case err != nil:
		return err
	}
	if _, err := m.keys.Move(ctx, tx, fromAccountID, intoAccountID); err != nil {
		return err
	}
	for _, hook := range m.hooks {
		if err := hook(ctx, tx, fromAccountID, intoAccountID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE marketplace_grants
		SET account_id = $2, claimed_at = now(), claim_token_hash = NULL
		WHERE id = $1`, grantRef, intoAccountID,
	); err != nil {
		return err
	}
	if err := m.accounts.DeleteWithoutIdentity(ctx, tx, fromAccountID); err != nil {
		return err
	}
	return tx.Commit()
}

// NewClaimLink replaces a grant's claim link, such as for an owner who lost it and
// proved the purchase to support, and returns the new one. The old link stops working.
// It returns ErrClaimInvalid for a grant that can't be claimed.
func (m *Marketplace) NewClaimLink(ctx context.Context, grantID string) (string, error) {
	if m.provider == nil || m.cfg.ClaimURL == "" {
		return "", errors.New("marketplace: claim links need the provider and ClaimURL")
	}
	token, hash, err := claimToken()
	if err != nil {
		return "", err
	}
	res, err := m.app.DB().ExecContext(ctx, `
		UPDATE marketplace_grants
		SET claim_token_hash = $3
		WHERE provider = $1 AND grant_id = $2
		    AND account_id IS NOT NULL AND claimed_at IS NULL AND revoked_at IS NULL AND ended_at IS NULL`,
		m.provider.Name(), grantID, hash,
	)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrClaimInvalid
	}
	return m.claimLink(token), nil
}
