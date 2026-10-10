// Package credits keeps prepaid credit for accounts: a ledger, purchases through a
// payment provider, and debits the app makes for its work.
//
// One credit is one EUR cent. Every mutation locks the account row, is idempotent per
// account and reference, and appends to the ledger; corrections are new entries. Apps
// that reserve credit for work in progress report those holds through Config.Holds,
// and use WithAccount to change their own rows under the same lock.
package credits

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/payments"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations create the credit tables. List them after accounts.Migrations.
var Migrations = app.Migrations{Namespace: "credits", Files: sub(migrationFiles, "migrations")}

func sub(fsys fs.FS, dir string) fs.FS {
	s, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return s
}

var (
	// ErrInsufficient means a debit exceeds the available credit.
	ErrInsufficient = errors.New("credits: insufficient credit")
	// ErrConflict means a reference was already used for a different amount or kind.
	ErrConflict = errors.New("credits: the reference was already used differently")
	// ErrNotFound means the account does not exist.
	ErrNotFound = errors.New("credits: account not found")
	// ErrInvalid means an amount, reference or description is out of range.
	ErrInvalid = errors.New("credits: invalid amount, reference or description")
)

// Policy holds the business rules for purchases. They are the app's to choose.
type Policy struct {
	// MinPurchaseCents and MaxPurchaseCents bound the credit one purchase buys.
	MinPurchaseCents int64
	MaxPurchaseCents int64
	// FeeBasisPoints and FeeFixedCents add a fee on top of the credit, grossed up so it
	// also covers the percentage charged on itself. Zero means no fee.
	FeeBasisPoints int64
	FeeFixedCents  int64
}

// Config configures the module.
type Config struct {
	// Payments takes purchases.
	Payments payments.Provider
	Policy   Policy
	// Holds reports how much of an account's credit the app has reserved for work in
	// progress. It runs inside the transaction that holds the account lock, and must
	// only read. Nil means the app keeps no holds.
	Holds func(ctx context.Context, tx *sql.Tx, accountID string) (int64, error)
}

// Credits is the credits module.
type Credits struct {
	app      *app.App
	payments payments.Provider
	policy   Policy
	holds    func(ctx context.Context, tx *sql.Tx, accountID string) (int64, error)
}

// New returns the credits module. It forfeits an account's remaining credit when the
// account is deleted, and registers the credits operator commands.
func New(a *app.App, accts *accounts.Accounts, cfg Config) (*Credits, error) {
	for _, m := range []app.Migrations{accounts.Migrations, Migrations} {
		if err := a.Require(m); err != nil {
			return nil, err
		}
	}
	if cfg.Payments == nil {
		return nil, errors.New("credits: a payment provider is required")
	}
	p := cfg.Policy
	switch {
	case p.MinPurchaseCents <= 0 || p.MaxPurchaseCents < p.MinPurchaseCents:
		return nil, errors.New("credits: set MinPurchaseCents above zero and MaxPurchaseCents at least as high")
	case p.MaxPurchaseCents > 99_999_999:
		return nil, errors.New("credits: MaxPurchaseCents cannot exceed 99,999,999")
	case p.FeeBasisPoints < 0 || p.FeeBasisPoints >= 10_000 || p.FeeFixedCents < 0:
		return nil, errors.New("credits: fees must be at least zero, and basis points below 10,000")
	}
	holds := cfg.Holds
	if holds == nil {
		holds = func(context.Context, *sql.Tx, string) (int64, error) { return 0, nil }
	}
	cr := &Credits{app: a, payments: cfg.Payments, policy: p, holds: holds}
	accts.OnDelete(cr.forfeit)
	cr.registerCommands()
	return cr, nil
}

// Entry is one ledger entry.
type Entry struct {
	ID          int64
	Kind        string
	AmountCents int64
	Reference   string
	Description string
	CreatedAt   time.Time
}

// Tx is an account's credit inside a transaction that holds the account's lock.
type Tx struct {
	ctx       context.Context
	cr        *Credits
	tx        *sql.Tx
	accountID string
}

// SQL returns the transaction, for the app's own tables. Do not commit or roll it back.
func (t *Tx) SQL() *sql.Tx { return t.tx }

// Balance returns the account's balance, holds included.
func (t *Tx) Balance() (int64, error) {
	var balance int64
	err := t.tx.QueryRowContext(t.ctx, `
		SELECT COALESCE(sum(amount_cents), 0)
		FROM credit_ledger
		WHERE account_id = $1`, t.accountID,
	).Scan(&balance)
	return balance, err
}

// Available returns the balance minus the app's holds. It is never more than the
// balance and never below zero unless the balance is.
func (t *Tx) Available() (int64, error) {
	balance, err := t.Balance()
	if err != nil {
		return 0, err
	}
	held, err := t.cr.holds(t.ctx, t.tx, t.accountID)
	if err != nil {
		return 0, err
	}
	if held < 0 {
		return 0, errors.New("credits: Holds reported a negative amount")
	}
	return available(balance, held), nil
}

func available(balance, held int64) int64 {
	if balance <= 0 {
		return balance
	}
	return max(0, balance-held)
}

// Debit takes credit for work. It fails with ErrInsufficient when it exceeds the
// available credit. Repeating a debit with the same reference and amount succeeds
// without taking credit twice.
func (t *Tx) Debit(cents int64, reference, description string) error {
	if cents <= 0 {
		return ErrInvalid
	}
	if err := checkEntry(reference, description); err != nil {
		return err
	}
	if done, err := t.recorded("debit", -cents, reference); done || err != nil {
		return err
	}
	avail, err := t.Available()
	if err != nil {
		return err
	}
	if cents > avail {
		return ErrInsufficient
	}
	return t.insert("debit", -cents, reference, description, nil)
}

// adjust records an operator or app correction, positive or negative. It may take the
// balance below zero.
func (t *Tx) adjust(cents int64, reference, description string) error {
	if cents == 0 {
		return ErrInvalid
	}
	if err := checkEntry(reference, description); err != nil {
		return err
	}
	if done, err := t.recorded("adjustment", cents, reference); done || err != nil {
		return err
	}
	return t.insert("adjustment", cents, reference, description, nil)
}

// Expire removes credit that expired, as one expiry entry. The credits/expiry module
// calls it; apps take credit with Debit.
func (t *Tx) Expire(cents int64, reference string) error {
	if cents <= 0 || !strings.HasPrefix(reference, "expiry:") || len(reference) > 128 {
		return ErrInvalid
	}
	if done, err := t.recorded("expiry", -cents, reference); done || err != nil {
		return err
	}
	return t.insert("expiry", -cents, reference, "Credit expired after inactivity", nil)
}

// recorded reports whether the reference already has the same entry, and returns
// ErrConflict if it has a different one.
func (t *Tx) recorded(kind string, cents int64, reference string) (bool, error) {
	var gotKind string
	var gotCents int64
	err := t.tx.QueryRowContext(t.ctx, `
		SELECT kind, amount_cents
		FROM credit_ledger
		WHERE account_id = $1 AND reference = $2`, t.accountID, reference,
	).Scan(&gotKind, &gotCents)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	case gotKind != kind || gotCents != cents:
		return false, ErrConflict
	}
	return true, nil
}

func (t *Tx) insert(kind string, cents int64, reference, description string, intentID *string) error {
	_, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO credit_ledger (account_id, intent_id, kind, amount_cents, reference, description)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		t.accountID, intentID, kind, cents, reference, description,
	)
	return err
}

// reserved are the reference prefixes the framework writes itself.
var reserved = []string{"purchase:", "refund:", "dispute:", "expiry:", "forfeit:"}

func checkEntry(reference, description string) error {
	if reference == "" || len(reference) > 128 || !utf8.ValidString(reference) ||
		utf8.RuneCountInString(description) > 160 || !utf8.ValidString(description) {
		return ErrInvalid
	}
	for _, prefix := range reserved {
		if strings.HasPrefix(reference, prefix) {
			return fmt.Errorf("%w: references starting with %q are reserved", ErrInvalid, prefix)
		}
	}
	return nil
}

// WithAccount runs fn in a transaction that holds the account's lock. Framework debits
// and the app's own changes through Tx.SQL commit together, or not at all. Lock the
// account before any app rows, and do no remote calls inside fn.
func (cr *Credits) WithAccount(ctx context.Context, accountID string, fn func(tx *Tx) error) error {
	tx, err := cr.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `
		SELECT id
		FROM accounts
		WHERE id = $1
		FOR UPDATE`, accountID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := fn(&Tx{ctx: ctx, cr: cr, tx: tx, accountID: accountID}); err != nil {
		return err
	}
	return tx.Commit()
}

// Debit takes credit for work. See Tx.Debit.
func (cr *Credits) Debit(ctx context.Context, accountID string, cents int64, reference, description string) error {
	return cr.WithAccount(ctx, accountID, func(tx *Tx) error {
		return tx.Debit(cents, reference, description)
	})
}

// Adjust records a correction or grant, positive or negative, idempotent per reference.
// Unlike Debit it may take the balance below zero.
func (cr *Credits) Adjust(ctx context.Context, accountID string, cents int64, reference, description string) error {
	return cr.WithAccount(ctx, accountID, func(tx *Tx) error {
		return tx.adjust(cents, reference, description)
	})
}

// Summary is an account's credit at one moment.
type Summary struct {
	BalanceCents   int64
	HeldCents      int64
	AvailableCents int64
}

// Summary reads the balance and holds in one consistent snapshot, without locking.
func (cr *Credits) Summary(ctx context.Context, accountID string) (Summary, error) {
	tx, err := cr.app.DB().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return Summary{}, err
	}
	defer tx.Rollback()
	t := &Tx{ctx: ctx, cr: cr, tx: tx, accountID: accountID}
	balance, err := t.Balance()
	if err != nil {
		return Summary{}, err
	}
	held, err := cr.holds(ctx, tx, accountID)
	if err != nil {
		return Summary{}, err
	}
	return Summary{BalanceCents: balance, HeldCents: held, AvailableCents: available(balance, held)}, tx.Commit()
}

// History returns up to limit entries older than the entry with ID before, newest
// first. Zero before starts at the newest entry. Pass the last entry's ID to continue.
func (cr *Credits) History(ctx context.Context, accountID string, before int64, limit int) ([]Entry, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("credits: the history limit must be between 1 and 100")
	}
	if before <= 0 {
		before = 1<<63 - 1
	}
	rows, err := cr.app.DB().QueryContext(ctx, `
		SELECT id, kind, amount_cents, reference, description, created_at
		FROM credit_ledger
		WHERE account_id = $1 AND id < $2
		ORDER BY id DESC
		LIMIT $3`, accountID, before, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Kind, &e.AmountCents, &e.Reference, &e.Description, &e.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// forfeit posts a deleted account's positive balance as forfeited. It runs inside the
// account deletion transaction; afterwards the foreign keys anonymise the account's
// ledger entries and payments.
func (cr *Credits) forfeit(ctx context.Context, tx *sql.Tx, accountID string) error {
	t := &Tx{ctx: ctx, cr: cr, tx: tx, accountID: accountID}
	balance, err := t.Balance()
	if err != nil || balance <= 0 {
		return err
	}
	return t.insert("forfeit", -balance, "forfeit:"+accountID, "Credit forfeited when the account was deleted", nil)
}
