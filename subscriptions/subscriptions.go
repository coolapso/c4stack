// Package subscriptions sells subscriptions to accounts through a payment provider,
// and keeps their state from its webhooks: the product, the units, the status and when
// the current period ends.
//
// Prices live at the provider, on its products; the app lists the products it sells
// and decides what each gives an account. A subscription starts with a checkout, and
// every change after it arrives as a signed webhook, taken once, applied only if it is
// newer than the state the module holds. An event the module can't apply as it stands
// goes to review for an operator, as credits does. An account can't be deleted while a
// subscription would charge it again.
package subscriptions

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/payments"
	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations create the subscription tables. List them after accounts.Migrations.
var Migrations = app.Migrations{Namespace: "subscriptions", Files: sub(migrationFiles, "migrations")}

func sub(fsys fs.FS, dir string) fs.FS {
	s, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return s
}

var (
	// ErrNotFound means the account has no such subscription.
	ErrNotFound = errors.New("subscriptions: subscription not found")
	// ErrUnknownProduct means the app doesn't sell the product.
	ErrUnknownProduct = errors.New("subscriptions: unknown product")
	// ErrInvalid means units or a target are out of range.
	ErrInvalid = errors.New("subscriptions: invalid units or target")
	// ErrAlreadySubscribed means the account already has a live subscription in the
	// product's group, for the same target. Change it instead.
	ErrAlreadySubscribed = errors.New("subscriptions: already subscribed in this group")
	// ErrNotLive means the subscription won't charge again, so there is nothing to
	// cancel or change.
	ErrNotLive = errors.New("subscriptions: the subscription has ended or is ending")
	// ErrNoCustomer means the account has never paid for a subscription, so the provider
	// has no billing page for it.
	ErrNoCustomer = errors.New("subscriptions: no customer at the provider")
	// ErrBusy means another change to the subscription is in progress.
	ErrBusy = errors.New("subscriptions: another change to the subscription is in progress")
)

// Product is something the app sells as a subscription. Its price and billing period
// are the provider's product's.
type Product struct {
	// ID is the app's name for it, such as "plus-monthly". Stored with each
	// subscription, so it must stay the same.
	ID string
	// ProviderID is the provider's product, such as Creem's prod_….
	ProviderID string
	// Name is what people see, such as "Plus, monthly".
	Name string
	// MaxUnits is how many units one subscription may have, such as seats or packs.
	// Zero means 1.
	MaxUnits int
	// Group makes products exclusive: an account has at most one live subscription in a
	// group for a target, such as one plan. A subscription changes between the products
	// of its group. Empty means no group.
	Group string
}

// Config configures the module.
type Config struct {
	// Payments sells the subscriptions.
	Payments payments.Subscriber
	// Products are what the app sells.
	Products []Product
	// Proration says how a change's price difference is charged. Zero is
	// payments.ProrateNow.
	Proration payments.Proration
}

// Subscription is an account's subscription.
type Subscription struct {
	ID        string
	AccountID string
	Product   Product
	// Target is what the subscription is for, in the app's words, or empty.
	Target string
	Units  int
	Status payments.SubscriptionStatus
	// PeriodEnd is when the current period ends: the next renewal, or the end of a
	// canceling subscription. Zero until the provider says.
	PeriodEnd time.Time
	CreatedAt time.Time
}

// Live reports whether the subscription would charge again.
func (s Subscription) Live() bool { return s.Status.Live() }

// Subscriptions is the subscriptions module.
type Subscriptions struct {
	app        *app.App
	accounts   *accounts.Accounts
	payments   payments.Subscriber
	products   map[string]Product
	byProvider map[string]Product
	order      []Product
	proration  payments.Proration
}

var productID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// New returns the module. It refuses to delete an account while one of its
// subscriptions would charge again, and forgets what ties the account's subscriptions
// to it when it is deleted. It deletes abandoned checkouts, and registers the
// subscriptions operator commands.
func New(a *app.App, accts *accounts.Accounts, cfg Config) (*Subscriptions, error) {
	for _, m := range []app.Migrations{accounts.Migrations, Migrations} {
		if err := a.Require(m); err != nil {
			return nil, err
		}
	}
	if cfg.Payments == nil {
		return nil, errors.New("subscriptions: Config.Payments is required")
	}
	if len(cfg.Products) == 0 {
		return nil, errors.New("subscriptions: list the products in Config.Products")
	}
	s := &Subscriptions{
		app:        a,
		accounts:   accts,
		payments:   cfg.Payments,
		products:   map[string]Product{},
		byProvider: map[string]Product{},
		proration:  cfg.Proration,
	}
	for _, p := range cfg.Products {
		if p.MaxUnits == 0 {
			p.MaxUnits = 1
		}
		switch {
		case !productID.MatchString(p.ID):
			return nil, fmt.Errorf("subscriptions: the product ID %q must be lowercase letters, digits, dots, dashes or underscores", p.ID)
		case p.ProviderID == "" || p.Name == "" || p.MaxUnits < 1:
			return nil, fmt.Errorf("subscriptions: the product %q needs a provider ID, a name and at least one unit", p.ID)
		case s.products[p.ID].ID != "" || s.byProvider[p.ProviderID].ID != "":
			return nil, fmt.Errorf("subscriptions: the product %q is listed twice", p.ID)
		}
		s.products[p.ID] = p
		s.byProvider[p.ProviderID] = p
		s.order = append(s.order, p)
	}
	accts.CheckDeletion(s.checkDeletion)
	accts.OnDelete(s.forget)
	a.Every("subscriptions cleanup", time.Hour, s.cleanup)
	s.registerCommands()
	return s, nil
}

// Products returns the products the app sells, in the order given.
func (s *Subscriptions) Products() []Product { return append([]Product(nil), s.order...) }

// CheckoutRequest is a subscription to start.
type CheckoutRequest struct {
	// Product is the app's product ID.
	Product string
	// Units is 1 or more, up to the product's MaxUnits. Zero means 1.
	Units int
	// Target is what the subscription is for, in the app's words, such as a vault's
	// ID: up to 128 printable characters, or empty.
	Target string
	// Email prefills the buyer's address.
	Email string
	// SuccessURL is where the provider sends the buyer after paying. The subscription
	// starts with the provider's webhook, not with this visit.
	SuccessURL string
}

// Checkout starts a subscription for an account and returns the provider's checkout
// URL. The subscription is pending until the provider confirms it.
func (s *Subscriptions) Checkout(ctx context.Context, accountID string, r CheckoutRequest) (string, error) {
	p, ok := s.products[r.Product]
	if !ok {
		return "", ErrUnknownProduct
	}
	if r.Units == 0 {
		r.Units = 1
	}
	if r.Units < 1 || r.Units > p.MaxUnits || !validTarget(r.Target) {
		return "", ErrInvalid
	}
	if p.Group != "" {
		taken, err := s.groupTaken(ctx, s.app.DB(), accountID, p.Group, r.Target, "")
		if err != nil {
			return "", err
		}
		if taken {
			return "", ErrAlreadySubscribed
		}
	}
	var id string
	err := s.app.DB().QueryRowContext(ctx, `
		INSERT INTO subscriptions (account_id, provider, product, target, units, status)
		VALUES ($1, $2, $3, $4, $5, 'pending')
		RETURNING id`, accountID, s.payments.Name(), p.ID, r.Target, r.Units,
	).Scan(&id)
	if err != nil {
		return "", err
	}
	session, err := s.payments.CreateSubscriptionCheckout(ctx, payments.SubscriptionCheckout{
		Reference:  id,
		ProductID:  p.ProviderID,
		Units:      r.Units,
		Email:      r.Email,
		SuccessURL: r.SuccessURL,
	})
	if err != nil {
		return "", err
	}
	if _, err := s.app.DB().ExecContext(ctx, `
		UPDATE subscriptions
		SET provider_checkout_id = $2, updated_at = now()
		WHERE id = $1`, id, session.ID,
	); err != nil {
		return "", err
	}
	return session.URL, nil
}

// Confirm reads an account's checkouts that haven't started a subscription yet from
// the provider, and starts those that completed, as their webhooks would. Call it when
// the buyer comes back from a checkout, so the subscription shows at once rather than
// when the webhook arrives; the routes Mount adds do. It reads only the provider, never
// what the browser brings.
func (s *Subscriptions) Confirm(ctx context.Context, accountID string) error {
	checkouts, err := s.openCheckouts(ctx, accountID)
	if err != nil {
		return err
	}
	for _, id := range checkouts {
		e, err := s.payments.CompletedCheckout(ctx, id)
		if errors.Is(err, payments.ErrIgnored) {
			continue
		}
		if err != nil {
			return err
		}
		res, err := s.take(ctx, e)
		if err != nil {
			return err
		}
		if res.review != "" {
			s.app.Log().Error("subscription event needs review", "event_id", e.ID, "subscription_id", res.subscriptionID, "reason", res.review)
		}
	}
	return nil
}

// openCheckouts returns the provider's IDs of an account's recent checkouts that
// haven't started a subscription yet.
func (s *Subscriptions) openCheckouts(ctx context.Context, accountID string) (_ []string, err error) {
	rows, err := s.app.DB().QueryContext(ctx, `
		SELECT provider_checkout_id
		FROM subscriptions
		WHERE account_id::text = $1 AND provider = $2 AND status = 'pending'
		    AND provider_checkout_id IS NOT NULL AND created_at > now() - interval '1 day'
		ORDER BY created_at DESC
		LIMIT 5`, accountID, s.payments.Name(),
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	var checkouts []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		checkouts = append(checkouts, id)
	}
	return checkouts, rows.Err()
}

// querier is a database or a transaction.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// groupTaken reports whether the account has a live subscription in group for target,
// other than except.
func (s *Subscriptions) groupTaken(ctx context.Context, q querier, accountID, group, target, except string) (bool, error) {
	var products []string
	for _, p := range s.order {
		if p.Group == group {
			products = append(products, p.ID)
		}
	}
	var taken bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM subscriptions
		    WHERE account_id = $1 AND target = $2 AND product = ANY (string_to_array($3, ' '))
		        AND status IN (`+liveStatuses+`) AND id::text <> $4
		)`, accountID, target, joinIDs(products), except,
	).Scan(&taken)
	return taken, err
}

// liveStatuses are the statuses that would charge again, as SQL.
const liveStatuses = `'active', 'trialing', 'past_due', 'unpaid', 'paused'`

func joinIDs(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += " "
		}
		out += id
	}
	return out
}

// validTarget reports whether t is empty or up to 128 printable characters.
func validTarget(t string) bool {
	if !utf8.ValidString(t) || utf8.RuneCountInString(t) > 128 {
		return false
	}
	for _, r := range t {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

const columns = `id, COALESCE(account_id::text, ''), product, target, units, status, period_end, created_at`

func (s *Subscriptions) scan(row interface{ Scan(...any) error }) (Subscription, error) {
	var sub Subscription
	var product, status string
	var periodEnd sql.NullTime
	if err := row.Scan(&sub.ID, &sub.AccountID, &product, &sub.Target, &sub.Units, &status, &periodEnd, &sub.CreatedAt); err != nil {
		return Subscription{}, err
	}
	sub.Product = s.products[product]
	if sub.Product.ID == "" {
		// A product the app no longer lists still shows, under its ID.
		sub.Product = Product{ID: product, Name: product, MaxUnits: 1}
	}
	sub.Status = payments.SubscriptionStatus(status)
	sub.PeriodEnd = periodEnd.Time
	return sub, nil
}

// List returns an account's subscriptions, newest first, without checkouts that
// haven't completed.
func (s *Subscriptions) List(ctx context.Context, accountID string) (_ []Subscription, err error) {
	rows, err := s.app.DB().QueryContext(ctx, `
		SELECT `+columns+`
		FROM subscriptions
		WHERE account_id::text = $1 AND status <> 'pending'
		ORDER BY created_at DESC, id`, accountID,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	var out []Subscription
	for rows.Next() {
		sub, err := s.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// Get returns one of an account's subscriptions.
func (s *Subscriptions) Get(ctx context.Context, accountID, id string) (Subscription, error) {
	sub, err := s.scan(s.app.DB().QueryRowContext(ctx, `
		SELECT `+columns+`
		FROM subscriptions
		WHERE id::text = $1 AND account_id::text = $2 AND status <> 'pending'`, id, accountID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	return sub, err
}

// locked is one of an account's live subscriptions, locked for a change in tx.
type locked struct {
	tx         *sql.Tx
	sub        Subscription
	providerID string
}

// lock begins a transaction that locks one of an account's live subscriptions, so
// changes to it go one at a time: two at once could each charge a proration. The
// provider's calls happen while it is held; a second change meanwhile gets ErrBusy.
func (s *Subscriptions) lock(ctx context.Context, accountID, id string) (*locked, error) {
	tx, err := s.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	var providerID sql.NullString
	var sub Subscription
	var product, status string
	var periodEnd sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT id, COALESCE(account_id::text, ''), product, target, units, status, period_end,
		    created_at, provider_subscription_id
		FROM subscriptions
		WHERE id::text = $1 AND account_id::text = $2 AND status <> 'pending'
		FOR UPDATE NOWAIT`, id, accountID,
	).Scan(&sub.ID, &sub.AccountID, &product, &sub.Target, &sub.Units, &status, &periodEnd, &sub.CreatedAt, &providerID)
	if err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "55P03" {
			return nil, ErrBusy
		}
		return nil, err
	}
	sub.Product = s.products[product]
	if sub.Product.ID == "" {
		sub.Product = Product{ID: product, Name: product, MaxUnits: 1}
	}
	sub.Status = payments.SubscriptionStatus(status)
	sub.PeriodEnd = periodEnd.Time
	if !sub.Live() || !providerID.Valid {
		_ = tx.Rollback()
		if !providerID.Valid {
			return nil, ErrNotFound
		}
		return nil, ErrNotLive
	}
	return &locked{tx: tx, sub: sub, providerID: providerID.String}, nil
}

// Cancel cancels one of an account's subscriptions at the end of its period: it runs
// until then, and doesn't renew.
func (s *Subscriptions) Cancel(ctx context.Context, accountID, id string) error {
	l, err := s.lock(ctx, accountID, id)
	if err != nil {
		return err
	}
	defer func() { _ = l.tx.Rollback() }()
	if err := s.payments.CancelSubscription(ctx, l.providerID, true); err != nil {
		return err
	}
	if err := s.readBack(ctx, l, payments.SubscriptionCanceling); err != nil {
		return err
	}
	return l.tx.Commit()
}

// ChangeRequest is a change to a subscription. A zero field stays as it is.
type ChangeRequest struct {
	// Product is another product of the subscription's group.
	Product string
	Units   int
}

// Change moves one of an account's subscriptions to another product of its group, or
// another number of units. The price difference is charged as Config.Proration says.
//
// It reads the provider's state first and sends only what isn't so already, so a retry
// after a change that half happened doesn't charge its proration again. Whatever the
// provider then reports is applied, even when the change failed.
func (s *Subscriptions) Change(ctx context.Context, accountID, id string, r ChangeRequest) error {
	l, err := s.lock(ctx, accountID, id)
	if err != nil {
		return err
	}
	defer func() { _ = l.tx.Rollback() }()
	sub := l.sub
	target := sub.Product
	if r.Product != "" && r.Product != sub.Product.ID {
		p, ok := s.products[r.Product]
		if !ok {
			return ErrUnknownProduct
		}
		if p.Group == "" || p.Group != sub.Product.Group {
			return fmt.Errorf("%w: %s and %s aren't in one group", ErrInvalid, sub.Product.ID, p.ID)
		}
		target = p
	}
	// The units the subscription will have: a product's limit holds for what it keeps.
	units := sub.Units
	if r.Units != 0 {
		units = r.Units
	}
	if units < 1 || units > target.MaxUnits {
		return ErrInvalid
	}
	current, err := s.payments.Subscription(ctx, l.providerID)
	if err != nil {
		return err
	}
	have := current.Units
	if have == 0 {
		have = sub.Units
	}
	ch := payments.SubscriptionChange{Proration: s.proration}
	if current.ProductID != target.ProviderID {
		ch.ProductID = target.ProviderID
	}
	if units != have {
		ch.Units = units
	}
	var changeErr error
	if ch.ProductID != "" || ch.Units != 0 {
		changeErr = s.payments.ChangeSubscription(ctx, l.providerID, ch)
	}
	if err := s.readBack(ctx, l, ""); err != nil {
		return err
	}
	if err := l.tx.Commit(); err != nil {
		return err
	}
	return changeErr
}

// readBack reads a subscription's state from the provider after the module changed it,
// so the account sees the change before the webhook arrives. The read isn't ordered
// against webhooks, which carry the provider's time, so it leaves state_at as it is. If
// the provider can't be read, a fallback status, when given, stands in until the
// webhook.
func (s *Subscriptions) readBack(ctx context.Context, l *locked, fallback payments.SubscriptionStatus) error {
	state, err := s.payments.Subscription(ctx, l.providerID)
	if err != nil {
		s.app.Log().Warn("reading a subscription after changing it failed; its webhook updates it", "subscription_id", l.sub.ID, "error", err)
		if fallback == "" {
			return nil
		}
		_, err := l.tx.ExecContext(ctx, `
			UPDATE subscriptions
			SET status = $2, updated_at = now()
			WHERE id = $1`, l.sub.ID, string(fallback),
		)
		return err
	}
	out, err := s.applyState(ctx, l.tx, l.sub.ID, state, time.Time{})
	if out.review != "" {
		s.app.Log().Warn("a subscription's state needs an operator's look", "subscription_id", l.sub.ID, "reason", out.review)
	}
	return err
}

// PortalURL returns the provider's billing page for an account: payment methods,
// invoices and the account's subscriptions there.
func (s *Subscriptions) PortalURL(ctx context.Context, accountID string) (string, error) {
	var customer sql.NullString
	err := s.app.DB().QueryRowContext(ctx, `
		SELECT provider_customer_id
		FROM subscriptions
		WHERE account_id::text = $1 AND provider = $2 AND provider_customer_id IS NOT NULL
		ORDER BY updated_at DESC
		LIMIT 1`, accountID, s.payments.Name(),
	).Scan(&customer)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoCustomer
	}
	if err != nil {
		return "", err
	}
	return s.payments.CustomerPortal(ctx, customer.String)
}

// checkDeletion refuses to delete an account while one of its subscriptions would
// charge it again: deleting the account would leave it charging nobody.
func (s *Subscriptions) checkDeletion(ctx context.Context, tx *sql.Tx, accountID string) error {
	var live bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM subscriptions
		    WHERE account_id = $1 AND status IN (`+liveStatuses+`)
		)`, accountID,
	).Scan(&live); err != nil {
		return err
	}
	if live {
		return &accounts.DeletionRefused{Reason: "You have a subscription that renews. Cancel it on your account settings page first; you can delete your account right after."}
	}
	return nil
}

// forget runs when an account is deleted: the subscriptions' records stay, for the
// accounts, but lose what ties them to the person and to the app's things: the target
// and the provider's customer. The provider's subscription ID stays, so a late webhook
// can still find the record, and the module can cancel a subscription that comes back
// to life.
func (s *Subscriptions) forget(ctx context.Context, tx *sql.Tx, accountID string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE subscriptions
		SET target = '', provider_customer_id = NULL, updated_at = now()
		WHERE account_id = $1`, accountID,
	)
	return err
}

// cleanup deletes checkouts nobody completed within 30 days, and the record of events
// older than 30 days, long after the provider stops resending them. A checkout's page
// may stay open for long, and one completed after its row is gone has no account to
// go to, so they are kept well beyond a session.
func (s *Subscriptions) cleanup(ctx context.Context) error {
	if _, err := s.app.DB().ExecContext(ctx, `
		DELETE FROM subscriptions
		WHERE status = 'pending' AND created_at < now() - interval '30 days'`,
	); err != nil {
		return err
	}
	_, err := s.app.DB().ExecContext(ctx, `
		DELETE FROM subscription_events
		WHERE received_at < now() - interval '30 days'`,
	)
	return err
}

func (s *Subscriptions) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
