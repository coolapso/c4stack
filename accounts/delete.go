package accounts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/coolapso/c4stack/identity"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

var (
	// ErrNotDeleted means the identity provider did not confirm the deletion. Nothing
	// was deleted, and the user can try again.
	ErrNotDeleted = errors.New("accounts: the identity provider did not delete the account")
	// ErrCleanupPending means the identity was deleted at the provider but the local
	// data was not. An operator must run accounts delete-local for the account; if its
	// address signs up again first, the sign-up finishes a deletion the provider's
	// confirmation was recorded for.
	ErrCleanupPending = errors.New("accounts: local cleanup is pending")
)

// DeletionRefused is what a CheckDeletion check returns to refuse a deletion, and what
// Delete returns while a DeletionRequirement isn't met. Reason tells the person what to
// do first.
type DeletionRefused struct {
	Reason string
}

func (e *DeletionRefused) Error() string { return "accounts: deletion refused: " + e.Reason }

// DeletionRequirement is something that must be true before an account can be deleted,
// such as that the person closed their account on another service first. With
// RequireForDeletion, the delete page lists every requirement with its state, asks again
// every few seconds, and offers to delete only once all are met.
type DeletionRequirement struct {
	// Name says what must be true, as the list shows it, such as "No subscription
	// renews".
	Name string
	// Check reports the requirement's state for an account. It runs in a read-only
	// transaction of its own each time the delete page lists it, and again in the
	// deletion's transaction, which deletes nothing unless it reports RequirementMet.
	// An error shows the requirement as being checked, and fails a deletion.
	Check func(ctx context.Context, tx *sql.Tx, accountID string) (RequirementState, error)
}

// RequirementStatus is how far a DeletionRequirement is from being met.
type RequirementStatus int

const (
	// RequirementUnmet means the person has something to do first. It is the zero
	// value, so a state left empty never allows a deletion.
	RequirementUnmet RequirementStatus = iota
	// RequirementChecking means the answer isn't known yet, such as while another
	// service is asked. The page asks again in a few seconds.
	RequirementChecking
	// RequirementMet means nothing is left to do for it.
	RequirementMet
)

// RequirementState is a DeletionRequirement's state for one account.
type RequirementState struct {
	Status RequirementStatus
	// Detail says what to do while the requirement is unmet, or what is being checked.
	Detail string
	// Action is a link's text, such as "Open your settings", and URL where it goes,
	// shown while the requirement is unmet. It opens in a new tab, so the list stays
	// open and ticks the requirement once it's met.
	Action, URL string
}

// errRequirements refuses a deletion while a DeletionRequirement isn't met.
var errRequirements = &DeletionRefused{Reason: "Something on the list isn't done yet."}

// runChecks runs the CheckDeletion checks and the deletion requirements inside tx.
func (s *Accounts) runChecks(ctx context.Context, tx *sql.Tx, accountID string) error {
	for _, check := range s.checks {
		if err := check(ctx, tx, accountID); err != nil {
			return err
		}
	}
	for _, r := range s.requirements {
		state, err := r.Check(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if state.Status != RequirementMet {
			return errRequirements
		}
	}
	return nil
}

// requirementView is a deletion requirement as the delete page lists it.
type requirementView struct {
	Name string
	RequirementState
}

// requirementStates reports each deletion requirement's state for the delete page. Each
// runs in a read-only transaction of its own, so one that fails leaves the others'
// answers; it shows as being checked.
func (s *Accounts) requirementStates(ctx context.Context, accountID string, log *slog.Logger) []requirementView {
	views := make([]requirementView, 0, len(s.requirements))
	for _, r := range s.requirements {
		state, err := s.requirementState(ctx, r, accountID)
		if err != nil {
			log.Warn("checking a deletion requirement failed", "requirement", r.Name, "error", err)
			state = RequirementState{Status: RequirementChecking, Detail: "It couldn't be checked just now. Trying again."}
		}
		views = append(views, requirementView{Name: r.Name, RequirementState: state})
	}
	return views
}

func (s *Accounts) requirementState(ctx context.Context, r DeletionRequirement, accountID string) (RequirementState, error) {
	tx, err := s.app.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RequirementState{}, err
	}
	defer tx.Rollback()
	return r.Check(ctx, tx, accountID)
}

// refusal returns the reason a CheckDeletion check refuses to delete the account now, if
// one does. The delete form shows it instead of asking.
func (s *Accounts) refusal(ctx context.Context, accountID string) (string, error) {
	if len(s.checks) == 0 {
		return "", nil
	}
	tx, err := s.app.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var refused *DeletionRefused
	for _, check := range s.checks {
		if err := check(ctx, tx, accountID); errors.As(err, &refused) {
			return refused.Reason, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", nil
}

// Delete deletes the signed-in account: first the identity at the provider, then, once
// the provider confirms, the local data in one transaction with every OnDelete hook.
// It continues if the browser disconnects, and clears the browser session afterwards.
// A CheckDeletion check that refuses, or a DeletionRequirement that isn't met, stops it
// before the provider is asked, with an error that wraps ErrNotDeleted and a
// *DeletionRefused.
//
// A provider that keeps its users in the app's database (an identity.TxProvider) deletes
// them inside that one transaction, so everything is deleted or nothing is, and the
// deletion marks below aren't used.
//
// With any other provider, the two systems are not atomic. The account is marked before the provider is asked,
// the mark is cleared if the provider refuses, and the provider's confirmation is
// recorded before the local data is deleted. If the local transaction fails after the
// provider deleted the identity, Delete logs the account ID and returns
// ErrCleanupPending.
func (s *Accounts) Delete(c *gin.Context) error {
	if s.idp == nil {
		return errors.New("accounts: Delete needs an identity provider")
	}
	account := Current(c)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), time.Minute)
	defer cancel()

	if s.txp != nil {
		if err := s.deleteSignedIn(ctx, c.Request, account.ID); err != nil {
			return fmt.Errorf("%w: %w", ErrNotDeleted, err)
		}
		_ = s.idp.Logout(c.Writer, c.Request)
		return nil
	}
	if err := s.checkInTx(ctx, account.ID); err != nil {
		return fmt.Errorf("%w: %w", ErrNotDeleted, err)
	}
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
	_, confirmErr := s.app.DB().ExecContext(ctx, `
		UPDATE accounts
		SET deletion_confirmed_at = now()
		WHERE id = $1`, account.ID,
	)
	if err := s.deleteLocal(ctx, account.ID); err != nil {
		web.Log(c).Error("the identity was deleted but local cleanup failed; run accounts delete-local",
			"account_id", account.ID, "confirmation_recorded", confirmErr == nil, "error", err)
		return fmt.Errorf("%w: %w", ErrCleanupPending, err)
	}
	return nil
}

// errSessionChanged means the session that asked for a deletion ended, or is no longer
// recent, once its user was locked.
var errSessionChanged = errors.New("accounts: the session that asked ended or is no longer recent")

// deleteSignedIn deletes the signed-in account of a TxProvider in one transaction. It
// first locks the user through the request's session and rechecks that the session
// still exists and is recent, so a request that waited behind a recovery or a sign-out
// everywhere deletes nothing.
func (s *Accounts) deleteSignedIn(ctx context.Context, r *http.Request, accountID string) error {
	asked, _ := identity.FromContext(r.Context())
	tx, err := s.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	id, err := s.txp.SessionTx(ctx, tx, r)
	if err != nil {
		return err
	}
	if id.Subject != asked.Subject || !id.Recent(time.Now()) {
		return errSessionChanged
	}
	if err := s.runChecks(ctx, tx, accountID); err != nil {
		return err
	}
	if err := s.deleteTx(ctx, tx, accountID); err != nil {
		return err
	}
	return tx.Commit()
}

// checkInTx runs the checks in a transaction of their own, for a provider outside the
// app's database, before it is asked to delete the identity.
func (s *Accounts) checkInTx(ctx context.Context, accountID string) error {
	if len(s.checks) == 0 && len(s.requirements) == 0 {
		return nil
	}
	tx, err := s.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.runChecks(ctx, tx, accountID); err != nil {
		return err
	}
	return tx.Commit()
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

// deleteTx deletes an account inside tx: it deletes a TxProvider's users, records the
// account's identities as deleted, runs the OnDelete hooks, and deletes the account,
// which removes its identities and anonymises the records that refer to it.
//
// A TxProvider's users are deleted before the account is locked, because a sign-in locks
// its user before the account. A user that joined the account in the meantime is
// deleted after the lock.
func (s *Accounts) deleteTx(ctx context.Context, tx *sql.Tx, accountID string) error {
	done := map[string]bool{}
	if err := s.deleteProviderUsers(ctx, tx, accountID, done); err != nil {
		return err
	}
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
	if err := s.deleteProviderUsers(ctx, tx, accountID, done); err != nil {
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
