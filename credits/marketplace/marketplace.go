// Package marketplace sells credit on a payment provider's marketplace, such as Creem's
// Agentic Marketplace, where AI agents buy it without anyone signing up and receive an
// API key.
//
// The provider asks the app, in a webhook, to grant or revoke what each order bought.
// A provision opens an account without an identity, issues its API key and credits the
// order, and answers with the key and a link that claims the account. Top-ups credit the
// same account; revocations take credit back, and a revoked provision ends the account.
// The provider may repeat a request and expects the same answer, so each answer is
// recorded by its operation, and a provision's credentials stay sealed until its
// deadline passes.
//
// The account's owner claims it by opening the link and signing in: its credit, API keys
// and purchases move to their own account, which then works like any other. Nobody can
// be told before an unclaimed account's credit expires, so it expires after
// Config.InactiveMonths without activity, with no warning, and the account is deleted.
// Apps state that rule wherever they sell.
package marketplace

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"time"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/apikeys"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/credits"
	"github.com/coolapso/c4stack/payments"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations create the marketplace tables. List them after accounts.Migrations,
// apikeys.Migrations and credits.Migrations.
var Migrations = app.Migrations{Namespace: "credits_marketplace", Files: sub(migrationFiles, "migrations")}

func sub(fsys fs.FS, dir string) fs.FS {
	s, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return s
}

var (
	// ErrClaimInvalid means a claim link is unknown or was used, or its account ended.
	ErrClaimInvalid = errors.New("marketplace: the claim link is not valid")
	// ErrBusy means the claimed account has requests in progress; claiming works once
	// they settle.
	ErrBusy = errors.New("marketplace: the account has requests in progress")
)

// Product is a product the app sells on the marketplace.
type Product struct {
	// ProviderID is the provider's product, as its requests name it.
	ProviderID string
	// CreditCents is the credit the product buys.
	CreditCents int64
	// PriceCents is the product's price before tax, in EUR cents: the credit plus the
	// app's fee. An order for less, or in another currency, is refused and refunded.
	PriceCents int64
}

// Entitlement is what a provision bought, for Config.Entitlement to describe.
type Entitlement struct {
	CreditCents    int64
	InactiveMonths int
	ClaimEnv       string
}

// Config configures the module.
type Config struct {
	// Payments is the provider whose marketplace sells the products. Nil turns selling
	// off: the webhook answers 404 and the hourly job doesn't run, while Status still
	// works, as in an API gateway.
	Payments payments.Marketplace
	// Products are the products sold. Required with Payments.
	Products []Product
	// Keys issues the API key each provision delivers.
	Keys *apikeys.Keys
	// KeyName names that key in the account's list, such as "Marketplace agent".
	KeyName string
	// KeyEnv and ClaimEnv are the names the listing declares for the key and the claim
	// link, such as ACME_API_KEY and ACME_CLAIM_URL.
	KeyEnv   string
	ClaimEnv string
	// ClaimURL is the claim page's absolute URL, where the app mounts Mount, such as
	// https://acme.example/account/claim. Each link adds its token as the query.
	ClaimURL string
	// ClaimedPath is where the claim page sends the owner once the account is theirs.
	// Zero means "/account".
	ClaimedPath string
	// SealKey encrypts a provision's key and claim link until the request's deadline, so
	// a repeated request gets the same answer: 32 random bytes, kept secret like any
	// other key.
	SealKey []byte
	// InactiveMonths is how long an unclaimed account keeps its credit without a
	// purchase, charge, adjustment or transfer. Then its credit expires, without a
	// warning, since nobody can be told, and the account is deleted.
	InactiveMonths int
	// Description names marketplace purchases in the ledger. Zero means "Credit bought
	// on the marketplace".
	Description string
	// Entitlement writes what a provision tells the buyer, as a JSON object of at most
	// 4 KB. Nil uses DefaultEntitlement.
	Entitlement func(e Entitlement) map[string]any
	// OnAnswer reports every answered request, for the app's metrics. It runs after the
	// reply is written, and must return quickly.
	OnAnswer func(ctx context.Context, a Answered)
	// OnRun reports each run of the hourly job. It must return quickly.
	OnRun func(ctx context.Context, r Result, err error)
}

// Answered describes one answered request.
type Answered struct {
	Action payments.GrantAction
	Status payments.GrantStatus
	// Repeat holds when the request was answered before, and got the same answer.
	Repeat bool
	Test   bool
	// Duration is how long answering took.
	Duration time.Duration
}

// Marketplace is the marketplace module.
type Marketplace struct {
	app      *app.App
	accounts *accounts.Accounts
	credits  *credits.Credits
	keys     *apikeys.Keys
	provider payments.Marketplace
	cfg      Config
	products map[string]Product
	aead     cipher.AEAD
	hooks    []func(ctx context.Context, tx *sql.Tx, fromAccountID, toAccountID string) error
	now      func() time.Time
}

// New returns the marketplace module. With a provider it runs an hourly job that wipes
// sealed credentials past their deadline and expires inactive unclaimed accounts, and
// it registers the marketplace operator commands either way.
func New(a *app.App, accts *accounts.Accounts, cr *credits.Credits, cfg Config) (*Marketplace, error) {
	for _, m := range []app.Migrations{accounts.Migrations, apikeys.Migrations, credits.Migrations, Migrations} {
		if err := a.Require(m); err != nil {
			return nil, err
		}
	}
	if cfg.InactiveMonths < 1 || cfg.InactiveMonths > 120 {
		return nil, errors.New("marketplace: InactiveMonths must be between 1 and 120")
	}
	if cfg.ClaimedPath == "" {
		cfg.ClaimedPath = "/account"
	}
	if cfg.Description == "" {
		cfg.Description = "Credit bought on the marketplace"
	}
	if cfg.Entitlement == nil {
		cfg.Entitlement = DefaultEntitlement
	}
	m := &Marketplace{app: a, accounts: accts, credits: cr, keys: cfg.Keys, provider: cfg.Payments, cfg: cfg,
		products: map[string]Product{}, now: time.Now}
	if cfg.Payments != nil {
		if err := m.checkSelling(); err != nil {
			return nil, err
		}
		a.Every("marketplace", time.Hour, func(ctx context.Context) error {
			r, err := m.Run(ctx)
			if err == nil && !r.Skipped && r != (Result{}) {
				a.Log().Info("marketplace ran", "secrets_wiped", r.SecretsWiped, "expired", r.Expired,
					"expired_cents", r.ExpiredCents, "failed", r.Failed)
			}
			return err
		})
	}
	m.registerCommands()
	return m, nil
}

// checkSelling validates what selling needs.
func (m *Marketplace) checkSelling() error {
	cfg := m.cfg
	switch {
	case len(cfg.Products) == 0:
		return errors.New("marketplace: at least one product is required")
	case cfg.Keys == nil:
		return errors.New("marketplace: Keys is required to deliver API keys")
	case cfg.KeyName == "":
		return errors.New("marketplace: KeyName is required")
	case cfg.KeyEnv == "" || cfg.ClaimEnv == "" || cfg.KeyEnv == cfg.ClaimEnv:
		return errors.New("marketplace: KeyEnv and ClaimEnv are required, and differ")
	case len(cfg.SealKey) != 32:
		return errors.New("marketplace: SealKey must be 32 bytes")
	}
	u, err := url.Parse(cfg.ClaimURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("marketplace: ClaimURL must be an absolute URL without a query")
	}
	for _, p := range cfg.Products {
		switch {
		case p.ProviderID == "":
			return errors.New("marketplace: every product needs its provider's ID")
		case p.CreditCents <= 0 || p.PriceCents < p.CreditCents || p.PriceCents > credits.MaxChargeCents:
			return fmt.Errorf("marketplace: product %s needs credit above zero and a price at least as high", p.ProviderID)
		}
		if _, dup := m.products[p.ProviderID]; dup {
			return fmt.Errorf("marketplace: product %s is listed twice", p.ProviderID)
		}
		m.products[p.ProviderID] = p
	}
	block, err := aes.NewCipher(cfg.SealKey)
	if err != nil {
		return err
	}
	m.aead, err = cipher.NewGCM(block)
	return err
}

// DefaultEntitlement tells the buyer how much credit they bought, how to claim the
// account, and when unclaimed credit expires.
func DefaultEntitlement(e Entitlement) map[string]any {
	return map[string]any{
		"credit": credits.FormatEuros(e.CreditCents),
		"claim": fmt.Sprintf("Give the link in %s to the person you work for. Signing in there makes the account "+
			"theirs: they see its usage, manage its keys and buy credit, and are told by email before credit expires.", e.ClaimEnv),
		"expiry": fmt.Sprintf("Credit in an account nobody has claimed expires, without warning, %d months after "+
			"the account was last used or topped up, and the account is then deleted.", e.InactiveMonths),
	}
}

// OnClaim adds a hook that moves the app's own rows of a claimed account, such as its
// usage history, to its owner's account. Hooks run in the order they were added, inside
// the claim's transaction, after the module moved the credit and keys and before the
// claimed account is deleted; an error rolls the claim back.
func (m *Marketplace) OnClaim(hook func(ctx context.Context, tx *sql.Tx, fromAccountID, toAccountID string) error) {
	m.hooks = append(m.hooks, hook)
}

// Status is what a marketplace sale means for an account.
type Status struct {
	// Marketplace holds when a marketplace sale opened the account, or was claimed into
	// it.
	Marketplace bool
	// Claimed holds when someone signs in to the account.
	Claimed bool
	// ExpiresAt is when an unclaimed account's credit expires unless it is used or
	// topped up first; zero for any other account.
	ExpiresAt time.Time
}

// Status reports what a marketplace sale means for an account, such as for an API that
// tells an agent when its credit expires.
func (m *Marketplace) Status(ctx context.Context, accountID string) (Status, error) {
	var s Status
	var last time.Time
	err := m.app.DB().QueryRowContext(ctx, `
		SELECT bool_or(g.claimed_at IS NOT NULL), max(`+lastActivity+`)
		FROM marketplace_grants g
		WHERE g.account_id = $1
		HAVING count(*) > 0`, accountID,
	).Scan(&s.Claimed, &last)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	s.Marketplace = true
	if !s.Claimed {
		s.ExpiresAt = last.AddDate(0, m.cfg.InactiveMonths, 0).UTC()
	}
	return s, nil
}

// lastActivity is an account's latest purchase, charge, adjustment or transfer, or its
// grant's creation before any, for a grant g.
const lastActivity = `GREATEST(g.created_at, COALESCE((
	SELECT max(l.created_at)
	FROM credit_ledger l
	WHERE l.account_id = g.account_id AND l.kind IN ('top_up', 'usage', 'adjustment', 'transfer')
), g.created_at))`

// seal encrypts credentials for the operation they answer.
func (m *Marketplace) seal(operationID string, plain []byte) ([]byte, error) {
	nonce := make([]byte, m.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return m.aead.Seal(nonce, nonce, plain, m.additional(operationID)), nil
}

// unseal decrypts what seal encrypted for the same operation.
func (m *Marketplace) unseal(operationID string, sealed []byte) ([]byte, error) {
	n := m.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("marketplace: sealed credentials are too short")
	}
	return m.aead.Open(nil, sealed[:n], sealed[n:], m.additional(operationID))
}

func (m *Marketplace) additional(operationID string) []byte {
	return []byte(m.provider.Name() + "\x00" + operationID)
}
