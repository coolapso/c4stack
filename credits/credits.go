// Package credits keeps prepaid credit for accounts: a ledger, purchases through a
// payment provider, and the charges the app makes for its work.
//
// One credit is one EUR cent. Every mutation locks the account row and appends to the
// ledger; corrections are new entries. Purchases and reversals are idempotent per
// payment, and the app's charges per account and reference. Apps that reserve credit
// for work in progress report those holds through Config.Holds, and use WithAccount
// to change their own rows under the same lock.
package credits

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"
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
	// MinPurchaseCents and MaxPurchaseCents bound the credit one purchase buys. The
	// charge, fee included, cannot exceed MaxChargeCents either.
	MinPurchaseCents int64
	MaxPurchaseCents int64
	// FeeBasisPoints and FeeFixedCents add a fee on top of the credit, grossed up so it
	// also covers the percentage charged on itself. Zero means no fee.
	FeeBasisPoints int64
	FeeFixedCents  int64
}

// MaxChargeCents is the largest charge one purchase can have, fee included.
const MaxChargeCents = 99_999_999

// Config configures the module.
type Config struct {
	// Payments takes purchases. Nil turns purchases off: the balance and history stay,
	// the pages say that buying credit is not available, and Webhook answers 404. A
	// process that only spends credit, such as an API gateway, holds no payment
	// configuration this way.
	Payments payments.Provider
	// Policy is required with Payments.
	Policy Policy
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
	p := cfg.Policy
	switch {
	case cfg.Payments == nil:
	case p.MinPurchaseCents <= 0 || p.MaxPurchaseCents < p.MinPurchaseCents:
		return nil, errors.New("credits: set MinPurchaseCents above zero and MaxPurchaseCents at least as high")
	case p.MaxPurchaseCents > MaxChargeCents:
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

// The ledger's entry kinds.
const (
	KindTopUp      = "top_up"     // a purchase
	KindUsage      = "usage"      // the app's charge for work
	KindRefund     = "refund"     // a refunded purchase
	KindChargeback = "chargeback" // a disputed purchase
	KindAdjustment = "adjustment" // an operator's or the app's correction
	KindExpiry     = "expiry"     // credit that expired unused
	KindForfeit    = "forfeit"    // credit left when the account was deleted
)

// Entry is one ledger entry.
type Entry struct {
	ID          string
	Kind        string
	AmountCents int64
	// Reference is the app's reference for usage and adjustments, or empty.
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
	if done, err := t.recorded(KindUsage, -cents, reference); done || err != nil {
		return err
	}
	avail, err := t.Available()
	if err != nil {
		return err
	}
	if cents > avail {
		return ErrInsufficient
	}
	return t.insert(KindUsage, -cents, &reference, description)
}

// Capture charges for work a hold reserved, once the app has settled the hold in the
// same transaction. Unlike Debit it does not check the available credit: the hold did
// that when the work started, and finished work is charged even if a refund lowered
// the balance since. Repeating a capture with the same reference and amount succeeds
// without charging twice.
func (t *Tx) Capture(cents int64, reference, description string) error {
	if cents <= 0 {
		return ErrInvalid
	}
	if err := checkEntry(reference, description); err != nil {
		return err
	}
	if done, err := t.recorded(KindUsage, -cents, reference); done || err != nil {
		return err
	}
	return t.insert(KindUsage, -cents, &reference, description)
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
	if done, err := t.recorded(KindAdjustment, cents, reference); done || err != nil {
		return err
	}
	return t.insert(KindAdjustment, cents, &reference, description)
}

// Expire removes credit that expired, as one expiry entry. The credits/expiry module
// calls it after checking, under the account lock, that the credit is due; apps take
// credit with Debit.
func (t *Tx) Expire(cents int64) error {
	if cents <= 0 {
		return ErrInvalid
	}
	return t.insert(KindExpiry, -cents, nil, "Credit expired after inactivity")
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

// insert appends an entry for the account. Entries for payments are written by the
// webhook, which sets the payment's columns as well.
func (t *Tx) insert(kind string, cents int64, reference *string, description string) error {
	_, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO credit_ledger (account_id, kind, amount_cents, reference, description)
		VALUES ($1, $2, $3, $4, $5)`,
		t.accountID, kind, cents, reference, description,
	)
	return err
}

func checkEntry(reference, description string) error {
	if reference == "" || utf8.RuneCountInString(reference) > 128 || !utf8.ValidString(reference) ||
		utf8.RuneCountInString(description) > 160 || !utf8.ValidString(description) {
		return ErrInvalid
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

// MaxHistoryPage is the last page History serves, so a huge page number cannot make
// the query skip millions of rows.
const MaxHistoryPage = 100_000

// History returns one page of an account's entries, newest first, and whether more
// follow. Pages start at 1.
func (cr *Credits) History(ctx context.Context, accountID string, page, perPage int) ([]Entry, bool, error) {
	if perPage < 1 || perPage > 100 {
		return nil, false, errors.New("credits: perPage must be between 1 and 100")
	}
	page = min(max(page, 1), MaxHistoryPage)
	rows, err := cr.app.DB().QueryContext(ctx, `
		SELECT id, kind, amount_cents, COALESCE(reference, ''), description, created_at
		FROM credit_ledger
		WHERE account_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3`, accountID, perPage+1, (page-1)*perPage,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Kind, &e.AmountCents, &e.Reference, &e.Description, &e.CreatedAt); err != nil {
			return nil, false, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(entries) > perPage {
		return entries[:perPage], true, nil
	}
	return entries, false, nil
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
	return t.insert(KindForfeit, -balance, nil, "Credit forfeited when the account was deleted")
}
