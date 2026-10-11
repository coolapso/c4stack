# Architecture

C4Stack is a Go module of building blocks for small web applications on one European
stack: Go, Gin, Templ, HTMX and PostgreSQL, with Hanko, Creem and Scaleway as default
providers.

The application owns `main`, its Gin engine, its routes, its layout and its configuration.
C4Stack owns the parts that are hard to get right and should be fixed in one place:
- sign-in and accounts, including deletion
- browser and API security
- the credit ledger and payment webhooks
- migrations, health checks and graceful shutdown

An app calls C4Stack. C4Stack calls back into the app only through hooks the app
registers.

## Packages

| Package | Purpose |
|---|---|
| `web` | A Gin engine with security middleware, CSRF, an extensible CSP, request logging, graceful serving and health endpoints |
| `app` | The command entry point, and the registry modules join: database, commands, background jobs |
| `app/apptest` | A fresh disposable database per test, with the app's migrations applied |
| `ui` | The `Layout` type, the default design (CSS, vendored HTMX, a small script) and content-hashed assets |
| `identity` | The identity provider interface |
| `identity/hanko` | The Hanko adapter, its sign-in page and its profile UI |
| `accounts` | Accounts mapped from identities, sign-in middleware, the settings page and account deletion |
| `apikeys` | Opt-in. API keys, bearer middleware and the keys page |
| `payments` | The payment provider interface |
| `payments/creem` | The Creem adapter |
| `credits` | Opt-in. Ledger, holds, purchases, webhooks, history and the credits page |
| `credits/expiry` | Opt-in. Expires unused credit after a delivered warning |
| `email` | The email sender interface |
| `email/scaleway` | The Scaleway Transactional Email adapter |
| `cmd/c4stack` | The app generator |

PostgreSQL connection and migration code lives in `internal/postgres`. Apps reach it
through `app`.

### Dependency direction

- `web` and `internal/postgres` depend on nothing else in C4Stack. `app` depends on both;
  `ui` depends on `web`.
- The interface packages `identity`, `payments` and `email` depend on nothing in C4Stack.
  Each adapter depends on its interface, and on `web` and `ui` where it renders pages.
- `accounts` depends on `identity`. `apikeys` and `credits` depend on `accounts`.
  `credits/expiry` depends on `credits` and `email`. Nothing points back: `accounts` learns
  about other modules only through the deletion hooks they register.
- The app depends on everything it uses.

Errors live in the package that owns the concept, such as `accounts.ErrNotFound` and
`credits.ErrInsufficient`. No package re-exports another package's errors.

## How an app is put together

The generated `main.go` is short:

```go
func main() {
	app.Main(app.Config{
		Name:        "acme",
		DatabaseURL: os.Getenv("ACME_DATABASE_URL"),
		Migrations: []app.Migrations{
			accounts.Migrations,
			{Namespace: "app", Files: migrations.Files},
		},
		Setup: setup,
	})
}

// setup wires the app for every command except migrate.
func setup(ctx context.Context, a *app.App) error {
	cfg, err := loadConfig() // config.go: reads the other ACME_* variables
	if err != nil {
		return err
	}
	a.SetLogger(cfg.logger())
	idp, err := hanko.New(hanko.Config{APIURL: cfg.HankoAPIURL, SignedInPath: "/account"})
	if err != nil {
		return err
	}
	accts, err := accounts.New(a, idp, accounts.Config{})
	if err != nil {
		return err
	}
	handler, err := routes(cfg, idp, accts)
	if err != nil {
		return err
	}
	a.Serve(web.Server{Addr: cfg.Addr, Handler: handler, HealthPath: "/health"})
	return nil
}
```

`routes.go` is also app-owned. It decides what is mounted and where:

```go
r, err := web.New(web.Config{PublicURL: cfg.PublicURL, SecretKey: cfg.SecretKey})
assets := ui.NewAssets("/assets")
assets.Add("app", static)
layout := newLayout(assets) // layout.go starts from the default design

r.GET("/assets/*path", assets.Serve)
r.GET("/", home)
idp.Mount(r, layout)   // GET /login
accts.Mount(r, layout) // the account pages at Config.Paths: /account/settings, /logout, …
account := r.Group("/account", accts.RequireSignIn)
account.GET("", overview) // the app's own signed-in pages
```

### What a module brings

Each module package exports its migrations as a value, such as `accounts.Migrations`,
under the module's own namespace. The app lists them in `main.go`, in order: modules
first, then the app's own `migrations/` directory, whose tables may reference module
tables.

A module constructor registers the module's operator commands, its background jobs and
its account-deletion hook. It fails with a clear error when the app's migration list lacks
the module, so the two cannot get out of step unnoticed. `Mount` adds the module's
default pages where the app says: the keys and credits pages at the group the app
passes in, such as `/account/api-keys`, and the account pages at the paths in
`accounts.Config.Paths`. An app that wants its own version of one page builds it on the
module's functions and mounts the module's other pages.

Module tables are Searchbase Cloud's, the first app on these conventions, plus what the
modules add. Module migrations use `IF NOT EXISTS` throughout, so they adopt tables an
app created before it used C4Stack, and `migrate` converts a migration history recorded
without namespaces. `internal/porttest` moves Searchbase's database this way.

An app that doesn't use a module gets none of its routes, migrations, commands or
configuration. Disabling a module later leaves its tables in place, because C4Stack never
drops data.

A release that adds a migration, a command or a page fix to an existing module reaches
the app through `go get` and a rebuild, because the module's `Migrations` value and
constructor carry them. Opting into a new module means one line in the migration list
plus its constructor and `Mount`.

### Commands

`app.Main` runs:
- `serve`: serves the handler, runs background jobs, and handles health and shutdown
- `migrate` and `migrate status`, which touch only the database
- `version` and `help`
- every command a module or the app registered, such as `accounts delete-local` or
  `credits adjust`

**`migrate` needs only the database.** It reads `Config.DatabaseURL` and
`Config.Migrations`, applies them and exits. It doesn't run `setup`, load other
configuration, construct providers or start a server. A deployment runs the same image
with the `migrate` command as a job before each rollout, with only the database secret.
That job can use a database role that owns the schema, while the server's role only
reads and writes data.

Every other command runs `setup` first, so it validates the configuration of every
enabled module. Operator commands run with the server's configuration.

## Module surfaces

The signatures below are abbreviated. The Go documentation has the details.

### web

```go
func New(cfg Config) (*gin.Engine, error)

type Config struct {
	PublicURL      string   // canonical origin, e.g. https://acme.example
	Origins        []string // further origins the app is served on, same scheme
	AnyHost        bool     // development only: accept any Host, but forms only from the origins
	SecretKey      string   // at least 32 bytes; signs CSRF tokens and seals cookies
	CSP            CSP      // sources added to the default policy everywhere
	MaxBodyBytes   int64    // default 1 MiB
	TrustedProxies []string // default: none
	Logger         *slog.Logger
}
type CSP struct{ Scripts, Styles, Images, Fonts, Connect, Frames, FormActions []string }

func CSRF(c *gin.Context) // checks token and Origin on unsafe methods
func CSRFToken(c *gin.Context) string
func AllowCSP(extra CSP) gin.HandlerFunc     // extends the policy for one route group
func MaxBody(n int64) gin.HandlerFunc        // changes the body limit for one group
func URL(c *gin.Context, path string) string // absolute URL on the public origin
func Log(c *gin.Context) *slog.Logger        // the request logger, with request_id
func SetSealedCookie(c *gin.Context, name string, value []byte, maxAge time.Duration)
func SealedCookie(c *gin.Context, name string) ([]byte, error) // encrypted and authenticated

type Server struct {
	Addr            string
	Handler         http.Handler
	HealthPath      string        // serves <path>/live and <path>/ready; empty disables
	DrainDelay      time.Duration // default 5s
	ShutdownTimeout time.Duration // default 30s
	Logger          *slog.Logger
}

func Serve(ctx context.Context, s Server, ready func(context.Context) error) error
```

**Middleware.** The engine runs, in order:
1. A generated request ID. Incoming IDs are ignored.
2. A request log with the route template, status and duration. It never logs query
   strings, headers or bodies.
3. Panic recovery, which logs the stack but not the panic value or the headers.
4. A host check, which answers 421 for any host but those of `PublicURL` and `Origins`.
   `AnyHost` turns it off, for development behind a proxy or tunnel.
5. Security headers.
6. A body limit.
7. CSRF cookie issuance.

The app keeps the engine. It can add middleware or set `NoRoute`, and C4Stack never
changes Gin's global mode.

**CSRF.** C4Stack uses a signed double-submit cookie (`__Host-csrf` on HTTPS, `csrf` on
plain-HTTP localhost) plus an exact Origin match against `PublicURL` or `Origins`. The cookie holds a
random value and the form token is its HMAC, so a cookie planted by an attacker never
matches a token they can produce. Forms send `csrf_token`. HTMX requests send
`X-CSRF-Token`, which the default page script copies from `<meta name="csrf-token">`.

Each kind of route uses one check:
- `accounts.RequireSignIn` includes the CSRF check. Sign-in is cookie-based, so a signed-in
  route cannot be left unprotected.
- Public forms use `web.CSRF`.
- API-key routes and webhooks use neither, and ignore cookies.

**CSP.** The default policy is:

```
default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:;
font-src 'self'; connect-src 'self'; frame-src 'none'; form-action 'self';
frame-ancestors 'none'; base-uri 'none'; object-src 'none'
```

`Config.CSP` extends the policy for every route; `AllowCSP` extends it for one group.
Modules use `AllowCSP` on the groups they mount:
- The Hanko sign-in page adds the Hanko origin to `connect-src`, and inline styles,
  because Hanko's elements inject them.
- The account settings page adds the Hanko origin to `connect-src` for the profile UI.
- The credits pages add the checkout origin to `form-action`, because the checkout POST
  answers with a redirect to the provider.

**Health and shutdown.** Health endpoints are served in front of the engine, so probes
that use pod IPs pass the host check and stay out of the request log. Readiness fails
while the server is draining, when PostgreSQL does not answer, or when a migration in
the app's list is missing.

On SIGTERM, readiness fails at once while the listener stays open for `DrainDelay`, so
load balancers stop routing to the instance. In-flight requests then get
`ShutdownTimeout` to finish before connections close.

### app

```go
func Main(cfg Config)
func New(cfg Config, db *sql.DB) *App // what Main builds; for tests and tools

type Config struct {
	Name        string
	DatabaseURL string       // the only setting migrate reads
	Migrations  []Migrations // applied in this order; readiness checks the same list
	Setup       func(ctx context.Context, a *App) error // every command except migrate
}
type Migrations struct {
	Namespace string
	Files     fs.FS // NNN_name.sql
}

func (a *App) Require(m Migrations) error // module constructors check their migrations are listed
func (a *App) DB() *sql.DB
func (a *App) SetLogger(l *slog.Logger)
func (a *App) Log() *slog.Logger
func (a *App) Command(cmd Command)
func (a *App) Every(name string, interval time.Duration, job func(context.Context) error)
func (a *App) Serve(s web.Server)

type Command struct {
	Name  string // "credits adjust"
	Usage string // "<account-id> <cents> <reference> <description>"
	Help  string
	Run   func(ctx context.Context, args []string) error
}
```

**Database.** `Main` opens the pool before `setup` runs, using pgx through
`database/sql`. For any host that isn't a Unix socket or loopback address, it sets
`sslmode=verify-full` with the system roots unless the URL already sets `sslmode`. An
explicit weaker mode is respected, with a startup warning.

**Migrations.** Namespaces apply in the order of `Config.Migrations`. Each file runs in
its own transaction, all under one advisory lock. The `schema_migrations` table records
the namespace, version and checksum of each file.

A `schema_migrations` table without a namespace column, as a runner with a single
history writes it, is converted first: each recorded file goes to the listed set with
the same version and checksum, and a recorded file that no set has stops the run before
anything changes.

`migrate` refuses a changed checksum, an unapplied version below the highest applied one,
and a namespace listed twice. A database ahead of the binary is reported and left
alone, so `migrate` from a rolled-back release still succeeds.

Readiness requires every known file to be applied with a matching checksum, and tolerates
unknown newer versions so older replicas keep serving during a rolling upgrade.

### ui

```go
type Layout struct {
	Page   func(c *gin.Context, title string, body templ.Component) templ.Component
	Assets *Assets
}
func (l Layout) Render(c *gin.Context, status int, title string, body templ.Component)
func (l Layout) NotFound(c *gin.Context)
func (l Layout) Error(c *gin.Context, status int, title, text string)

type Design struct {
	Name        string          // header and <title>
	Nav         []Link
	Head        templ.Component // favicon, meta tags, extra scripts
	Stylesheets []string        // linked after the default stylesheet
	Footer      templ.Component
}
func Default(d Design, assets *Assets) Layout

func NewAssets(base string) *Assets              // includes the default design's files
func (a *Assets) Add(prefix string, files fs.FS) error
func (a *Assets) URL(name string) string         // /assets/<hash>/ui/base.css
func (a *Assets) Serve(c *gin.Context)

// Components that default pages and custom layouts share.
func Head(c *gin.Context, assets *Assets) templ.Component // CSRF meta tag, HTMX, page script
func LiveRegion() templ.Component
func CSRFField(c *gin.Context) templ.Component
func Dialog(p DialogProps) templ.Component
```

**Levels of design freedom.** Apps choose one of three:
1. **Default.** `ui.Default(ui.Design{Name: "Acme"}, assets)`.
2. **Restyle.** Add a stylesheet that overrides design tokens, plus a favicon in `Head`,
   `Nav` links and a `Footer`.
3. **Own layout.** `ui.Layout{Page: myLayout, Assets: assets}` with the app's own Templ
   and CSS. It includes `ui.Head` and `ui.LiveRegion` so HTMX and dialogs work. Default
   pages render inside it, using the component classes `ui/base.css` defines. A custom
   layout either links `ui/base.css`, styles those classes itself, or replaces the pages.

**The default design.** It is one hand-written stylesheet of design tokens and component
classes, with no build step, supporting light and dark schemes. Class and token names are
neutral, with no framework prefix. `ui.js` wires dialogs, copy buttons, busy states and
the CSRF header for HTMX through `data-*` attributes, so pages need no inline scripts.

**Assets.** Asset URLs carry a per-file content hash. The current hash is served as
immutable for a year. Any other hash gets the current file with `no-cache`, so pages from
the previous release keep working during a rolling deploy. Modules add their own files
with `Add` when they mount; the Hanko adapter adds its vendored elements and scripts.

### identity and identity/hanko

```go
type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	SignedInAt    time.Time // when the session was created
}
func (id Identity) Recent(now time.Time) bool // signed in within RecentSignIn (5 minutes)
func NewContext(ctx context.Context, id Identity) context.Context
func FromContext(ctx context.Context) (Identity, bool)

type Provider interface {
	Name() string                                   // stored with each identity, e.g. "hanko"
	Authenticate(r *http.Request) (Identity, error) // ErrNoSession when not signed in
	Logout(w http.ResponseWriter, r *http.Request) error
	Delete(ctx context.Context, r *http.Request) error // returns only after the provider confirms
}
```

**How the Hanko adapter works.**
- `Authenticate` calls `POST /sessions/validate` on every request. There is no local JWT
  check, so a signed-out or deleted user's token stops working at once.
- `Logout` clears the cookie and calls `POST /logout`.
- `Delete` runs Hanko's profile flow action `account_delete` with the user's own session,
  and succeeds only on the `account_deleted` state. No admin API key is needed.
- `Mount` adds `GET /login`, a page with Hanko's `<hanko-auth>` element inside the app's
  layout. A user who already has a session continues to the `next` path.

**Profile UI.** The adapter offers a profile UI through three methods that `accounts`
looks for: `MountProfile`, `ProfileCSP` and `ProfileSection`. The accounts settings page
embeds it:
- **Email change.** It runs server-side through Hanko's profile flow:
  1. The user enters a new address and confirms it with a passcode, which they can have
     resent.
  2. The new address becomes primary, and the old one is removed.
  3. Other sessions are signed out, and the user signs in again.

  Between steps, the flow state lives in a sealed cookie. An interrupted cleanup can be
  resumed. The account's stored address follows the new primary address.
- **Passkeys.** The user adds a named passkey or removes one, through the Hanko SDK in the
  browser.
- **Signed-in devices.** The user sees their sessions and signs out any of them.

Passkey changes require a recent sign-in, as deletion does. The profile UI's routes are
under `/account/settings`, and after an email change the user lands on
`/account/email-changed`. The profile UI never offers account deletion, which always
goes through `accounts`.

### accounts

```sql
accounts(id uuid, primary_email UNIQUE, created_at, updated_at,
         deletion_started_at, deletion_confirmed_at)
account_identities(id uuid, account_id → accounts ON DELETE CASCADE, provider, subject,
                   email, email_verified, created_at, updated_at, UNIQUE (provider, subject))
deleted_identities(identity_hash bytea, deleted_at)
```

```go
type Account struct{ ID, Email string; CreatedAt time.Time } // Email is the primary address
type Config struct{ Paths Paths }
type Paths struct{ Login, Logout, Unverified, Settings, Delete, Deleted, EmailChanged string }

func New(a *app.App, idp identity.Provider, cfg Config) (*Accounts, error) // nil idp: no sign-in
func (s *Accounts) RequireSignIn(c *gin.Context)
func Current(c *gin.Context) Account
func (s *Accounts) Get(ctx context.Context, id string) (Account, error)
func (s *Accounts) OnDelete(hook func(ctx context.Context, tx *sql.Tx, accountID string) error)
func (s *Accounts) Delete(c *gin.Context) error // ErrNotDeleted or ErrCleanupPending on failure
func (s *Accounts) Mount(r gin.IRouter, layout ui.Layout) // every page at its path
func (s *Accounts) Pages(layout ui.Layout) *Pages         // the handlers, to mount some
// Pages: Settings, DeleteForm, Delete, Logout, Unverified, Deleted, EmailChanged and
// MountProfile
```

The paths default to Searchbase's: `/login`, `/logout`, `/auth/email-unverified`,
`/account/settings`, `/account/delete`, `/account/deleted` and `/account/email-changed`.
`RequireSignIn` and the pages use the configured paths for every redirect and link.

Signing out lives here rather than in the provider adapter, because it is the same for
every provider. The operator command is `accounts delete-local <account-id>`.

**`RequireSignIn`** checks CSRF, authenticates the session, then finds the account,
creating it on first sign-in. It sets `Cache-Control: no-store`. Unauthenticated requests
are redirected to `Paths.Login` with a safe `next` path, or get `HX-Redirect` from
HTMX. An identity without a verified email goes to `Paths.Unverified` and gets no
account.

**Finding the account.** A sign-in finds its account by provider and subject. An
identity the app has not seen joins the account whose `primary_email` is its verified
address, from whichever provider, or gets a new account. Users therefore keep their
accounts when the app changes identity provider.

Each identity's address is kept current on sign-in, including an unverified one that
cannot sign in: its previous address then stops counting as verified. A new identity
joins an account only when one of the account's identities still verifies its primary
address. When none does, the verified address of its most recently updated identity
takes its place, if no other account has it. An identity's address and the account's
primary address change in one transaction, and an account still without a verified
primary address tries again on each of its identities' sign-ins.

A new identity whose address is the primary address of such an account gets a new
account, once one of that account's verified addresses has taken its place. If none
can, the new identity is turned away and an operator must decide.

A process that only needs account IDs, such as an API gateway, constructs the module
with a nil provider. It holds no sign-in configuration, and `RequireSignIn`, `Mount` and
`Delete` are unavailable.

**Deletion** runs these steps:
1. The user confirms on `/account/delete` with CSRF and types `DELETE`. The session must
   have been created in the last 5 minutes; otherwise the page asks the user to sign in
   again first. This proves the owner is present, not just an unattended browser.
2. The account is marked with `deletion_started_at`.
3. `idp.Delete` runs with a context detached from the request, so a closed tab cannot
   stop it halfway, and with a one-minute budget. If the provider fails, the mark is
   cleared and nothing else changes; the user can retry.
4. Once the provider confirms, `deletion_confirmed_at` records it.
5. One transaction does the local deletion:
   1. It locks the account row and its identities.
   2. It records a hash of each identity in `deleted_identities`.
   3. It runs the `OnDelete` hooks: modules first, then the app.
   4. It deletes the account. Foreign keys cascade or anonymise.
6. The browser goes to `Paths.Deleted`.

If the local transaction fails after the provider deleted the identity, the failure is
logged with the account ID, and an operator runs `accounts delete-local`. The two
systems are not atomic. Until then:
- A sign-up with the account's address finishes a confirmed deletion and gets a new,
  empty account.
- A sign-up with the address of an account whose deletion is started but not confirmed
  is turned away, from any provider: only the provider's confirmation lets local data
  go.
- A sign-in by another of the account's identities finishes a confirmed deletion, and
  clears a mark the provider refused to act on.

Account creation checks `deleted_identities` under the identity's lock, so a request
that validated its session just before deletion cannot recreate the account.

**App data.** App tables with personal data join deletion through
`REFERENCES accounts (id) ON DELETE CASCADE` or through `OnDelete`.

### apikeys (opt-in)

```sql
api_keys(id uuid, account_id → accounts ON DELETE CASCADE, name, key_hash bytea UNIQUE,
         key_suffix, created_at, last_used_at, revoked_at)
```

```go
type Config struct{ Prefix string } // "acme" issues acme_<43 characters>

func New(a *app.App, cfg Config) (*Keys, error) // keys cascade with their account
func (k *Keys) Create(ctx context.Context, accountID, name string) (raw string, key Key, err error)
func (k *Keys) List(ctx context.Context, accountID string) ([]Key, error)
func (k *Keys) Rename(ctx context.Context, accountID, keyID, name string) error // active keys
func (k *Keys) Revoke(ctx context.Context, accountID, keyID string) error
func (k *Keys) Delete(ctx context.Context, accountID, keyID string) error // revoked keys; ErrActive
func (k *Keys) Authenticate(ctx context.Context, raw string) (Key, error)
func (k *Keys) LockActive(ctx context.Context, tx *sql.Tx, accountID, keyID string) error
func (k *Keys) RequireKey(c *gin.Context)
func Current(c *gin.Context) Key
func (k *Keys) Mount(r gin.IRouter, layout ui.Layout) // r: the page's group, e.g. /account/api-keys
// GET "", GET "/list", POST "", POST "/:id" with action rename, revoke or delete
```

**Storage.** A raw key is the prefix, an underscore and 32 random bytes in base64url.
C4Stack stores the SHA-256 of the whole raw key plus its last six characters, and shows
keys as the current prefix, `_••••` and those characters. Lookup is by hash, so changing
`Prefix` never invalidates existing keys. The raw key is shown once in the create
response and never logged.

**`RequireKey`** requires exactly one `Authorization: Bearer` header and ignores cookies.
It answers JSON 401 with `WWW-Authenticate` when the key is missing or invalid, and
removes the header before the handler runs. It records last use at most every five
minutes. A revocation applies from the next request. `LockActive` checks a key inside
the transaction that spends credit for a request, so a key revoked meanwhile cannot
spend.

**The keys page.** Each row has dialogs to rename or revoke an active key, and to delete
a revoked one after typing its name. Changes close the dialog and reload the list from
the page's `/list`.

### payments and payments/creem

```go
type Checkout struct {
	Reference   string // the purchase's ID, echoed back in webhook events
	AmountCents int64  // what the buyer pays, in EUR cents
	Email       string
	SuccessURL  string
}
type Event struct {
	ID          string    // provider event ID
	Kind        EventKind // Paid, Refunded, Disputed
	Reference   string
	CheckoutID  string
	OrderID     string
	AmountCents int64  // the price for Paid, the reversed amount otherwise
	PaidCents   int64  // what the buyer paid, after taxes or discounts
	Currency    string
	CreatedAt   time.Time // when the provider created the event, if it says
	Review      string    // set when the event is ours but cannot be applied as it stands
}
type Provider interface {
	Name() string
	CheckoutOrigin() string
	CreateCheckout(ctx context.Context, c Checkout) (Session, error) // Session{ID, URL}
	ParseWebhook(header http.Header, body []byte) (Event, error) // ErrInvalid, ErrIgnored
}
```

**How the Creem adapter works.**
- Checkout uses one configured product with `custom_price`. The purchase ID travels as
  `request_id` and as `intent_id` metadata.
- An API key starting with `creem_test_` selects Creem's test API.
- Webhooks are verified with HMAC-SHA256 from the `creem-signature` header.
- The `checkout_url` Creem returns must match `CheckoutOrigin`.

### credits (opt-in)

```sql
payment_intents(id uuid, account_id → accounts ON DELETE SET NULL, provider,
                provider_checkout_id, provider_order_id, idempotency_key,
                credit_amount_cents, fee_amount_cents, checkout_amount_cents,
                paid_amount_cents, currency, provider_status, created_at, updated_at, paid_at)
credit_ledger(id uuid, account_id → accounts ON DELETE SET NULL, payment_intent_id,
              provider_event_id UNIQUE, amount_cents, kind, created_at, reference, description)
payment_reviews(id, provider, event_id, payment_intent_id, reason, created_at,
                resolved_at, resolution)
```

A purchase's `provider_status` is `created`, `pending` once its checkout exists, then
`paid`, `refunded` or `charged_back`. Ledger kinds are `top_up`, `usage`, `refund`,
`chargeback`, `adjustment`, `expiry` and `forfeit`. A trigger keeps the ledger
append-only.

```go
type Policy struct {
	MinPurchaseCents, MaxPurchaseCents int64 // required with Payments
	FeeBasisPoints, FeeFixedCents      int64 // added on top, grossed up to cover itself
}
type Config struct {
	Payments payments.Provider // optional: nil turns purchases off
	Policy   Policy
	Holds    func(ctx context.Context, tx *sql.Tx, accountID string) (int64, error) // optional
}

func New(a *app.App, accts *accounts.Accounts, cfg Config) (*Credits, error)
func (cr *Credits) Debit(ctx context.Context, accountID string, cents int64, reference, description string) error
func (cr *Credits) Adjust(ctx context.Context, accountID string, cents int64, reference, description string) error
func (cr *Credits) WithAccount(ctx context.Context, accountID string, fn func(tx *Tx) error) error
func (tx *Tx) SQL() *sql.Tx
func (tx *Tx) Balance() (int64, error)
func (tx *Tx) Available() (int64, error)
func (tx *Tx) Debit(cents int64, reference, description string) error   // within available credit
func (tx *Tx) Capture(cents int64, reference, description string) error // work a hold reserved
func (tx *Tx) Expire(cents int64) error                                  // for credits/expiry
func (cr *Credits) Summary(ctx context.Context, accountID string) (Summary, error) // balance, held, available
func (cr *Credits) History(ctx context.Context, accountID string, page, perPage int) ([]Entry, bool, error)
func Label(e Entry) string
func (cr *Credits) Quote(creditCents int64) (Quote, error)
func (cr *Credits) Purchase(ctx context.Context, account accounts.Account, creditCents int64, successURL string) (Purchase, error)
func (cr *Credits) PaymentStatus(ctx context.Context, accountID, intentID string) (string, error)
func (cr *Credits) Mount(r gin.IRouter, layout ui.Layout) // r: the page's group, e.g. /account/credits
// GET and POST "", GET "/quote", "/summary", "/balance" and "/payment-status"
func (cr *Credits) Webhook(c *gin.Context)
```

The operator commands are `credits adjust`, `credits balance`, `credits reviews` and
`credits resolve`.

**Ledger rules.**
- Every mutation locks the account row.
- Purchases and reversals are idempotent per payment: a payment is credited at most once
  and reversed at most once. The app's charges and adjustments are idempotent per
  account and reference: a retry with the same amount succeeds without a new entry, and
  one with a different amount returns `ErrConflict`.
- `Debit` never takes available credit below zero. Available credit is the balance minus
  what the app's `Holds` function reports. `Capture` charges work a hold reserved without
  that check, so finished work is charged even if a refund lowered the balance since.
- `WithAccount` lets app reservations share the lock and transaction with the module's
  charges.

**Webhooks.** The webhook grants credit from the stored purchase, never from the payload.
- A paid event credits once.
- A full refund or dispute of a paid purchase reverses once.
- Any other event about one of the app's purchases becomes a review row.
- Foreign events are acknowledged and ignored.

**Without payments**, the pages show the balance and transactions and say that buying
credit is not available, and `Webhook` answers 404. A process that only spends credit
holds no payment configuration this way.

**The credits page** shows the available balance, a purchase form with a live quote,
and the transactions in pages of ten. Starting a purchase remembers it in a sealed
cookie, so when the buyer returns from the checkout the page reports it: confirming,
confirmed, not completed, or taking longer than a minute.

On account deletion, the credits hook forfeits a positive balance. The foreign keys then
anonymise the account's ledger and purchases; the ledger's append-only trigger allows that
one change and clears the descriptions. [The credits contract](credits.md) has the full
rules and the webhook outcome table.

### credits/expiry (opt-in)

```go
type Policy struct{ InactiveMonths, NoticeDays int } // required

type Config struct {
	Sender     email.TrackingSender // required: sends and reports delivery
	Policy     Policy
	AppName    string                                         // named in the default warning
	CreditsURL string                                         // linked from the warning
	Message    func(Notice) email.Message                     // optional
	OnRun      func(ctx context.Context, r Result, err error) // optional: each run's outcome
}

type Result struct {
	Skipped                bool // another run held the lock
	Warned, WarningsFailed int
	Expired                int // accounts
	ExpiredCents           int64
	Failed                 int // delivery checks, recording and expiries that failed
}

func New(a *app.App, cr *credits.Credits, cfg Config) (*Expiry, error)
func (x *Expiry) Run(ctx context.Context) (Result, error)
```

```sql
credit_expiry_warnings(account_id → accounts ON DELETE CASCADE, activity_at, email,
                       message_id, sent_at, notice_days, announced_expiry, delivered_at,
                       expires_at)
```

`New` registers the `credits expire` command and an hourly job during `serve`. The job
runs under an advisory lock, so replicas and deployments sharing the database don't
duplicate it. Each run:
1. **Checks delivery.** It asks the email provider about every warning not yet
   delivered. A delivered warning gets its deadline: a full notice period from delivery,
   and never before the date it announced. A failed warning, or one undelivered for
   three days, is removed so it is sent again.
2. **Warns.** A warning goes out `NoticeDays` before the credit would expire, outside
   any transaction, announcing the nominal date or a full notice period from now,
   whichever is later. It goes to the account's primary address, and only while one of
   the account's identities verifies that address. A new primary address gets a new
   warning.
3. **Expires.** Credit expires once its delivered warning's deadline has passed, if the
   warning is about the account's last activity, went to the account's current
   address, which an identity still verifies, and the app holds none of it.

Purchases, charges and adjustments count as activity, and new activity needs a new
warning. Deadlines are stored, so changing the policy never brings one forward.

### email and email/scaleway

```go
type Message struct{ To, Subject, Text string }
type Sender interface{ Send(ctx context.Context, m Message) (id string, err error) }
type Tracker interface {
	Delivery(ctx context.Context, id string) (Delivery, time.Time, error) // Pending, Delivered, Failed
}
type TrackingSender interface{ Sender; Tracker }
```

A nil error from `Send` means the provider accepted the message, not that it arrived.
The Scaleway adapter reports a message delivered once Scaleway's status is `sent`: the
recipient's mail system took it.

`scaleway.New(scaleway.Config{SecretKey, ProjectID, Region, From, FromName, ReplyTo})`.
The region defaults to `fr-par`. Errors carry the HTTP status, never the provider's
response body or the recipient.

## Configuration and naming

**Environment variables.** C4Stack reads no environment variables. The app's `config.go`
reads `<PREFIX>_*`, where the generator derives the prefix from the app name, and passes
the values in code. A minimal app needs four, plus an optional `ACME_ADDR`:
- `ACME_PUBLIC_URL`
- `ACME_DATABASE_URL`, the only variable `migrate` reads
- `ACME_SECRET_KEY`, from which C4Stack derives separate CSRF and cookie keys with HKDF
- `ACME_HANKO_API_URL`

**Other names users can see.**
- Cookies: `csrf` and `__Host-csrf`, plus sealed cookies during an email change
  (`email-change`) and after starting a purchase (`pending-payment`). The `hanko`
  cookie is Hanko's own.
- Public URLs, the asset base and API-key prefixes: chosen by the app.
- Email subjects: built from the app's name.

Database objects use plain names: `accounts`, `api_keys`, `credit_ledger` and so on.

**Business values.** Purchase limits, fees and expiry durations are required fields in
config structs. C4Stack has no business-value constants.

## Generator and shared Taskfile

**What `c4stack new acme` does.**
- It creates `acme/` exclusively and never writes into an existing directory.
- It pins the C4Stack release it was built from, or `--version`. A `--local <path>`
  option writes a development-only `replace` and includes the shared Taskfile from that
  checkout.
- It runs `go mod tidy` and `templ generate`.
- It warns when Task is older than 3.53.1, the first release where remote Taskfiles
  work without opting in to an experiment.

It writes these files:
- `main.go`, with the migration list, plus `config.go`, `routes.go`, `layout.go` and
  `pages.templ`
- `static/app.css` and `static/favicon.svg`
- `migrations/migrations.go`, which embeds the directory. There is no starter
  migration, so there is no empty version 1 to trap the app.
- `Taskfile.yml`, `.taskrc.yml`, `.env` (with a fresh secret key), `.env.example` and
  `.gitignore`
- `docker-compose.yml`, `Containerfile` and `.dockerignore`
- `README.md` and `AGENTS.md`

**The include.** The app's Taskfile includes `taskfiles/app.yml` from this repository,
flattened:

```yaml
vars:
  APP: acme
  ENV_PREFIX: ACME
  FRAMEWORK_VERSION: v0.1.0 # the C4Stack release in go.mod
includes:
  shared:
    taskfile: https://github.com/coolapso/c4stack.git//taskfiles/app.yml?ref={{.FRAMEWORK_VERSION}}
    flatten: true
    excludes: []
```

**One version.** `framework:update` changes the Go dependency and `FRAMEWORK_VERSION`
together, then tidies, regenerates, tests and builds. `check` fails when the two differ.
The version has to be written in the Taskfile because Task does not evaluate dynamic
(`sh:`) variables in include URLs: the ref would come out empty, and Task would fetch the
newest commit.

**Task mechanics.** These hold for Task 3.53.1 and later:
- A Git include uses the developer's Git credentials, so it works for a private
  repository.
- App variables reach the shared tasks. Task merges an included file's variables over
  the app's, so the shared file's own variables all start with `SHARED_` and never
  share a name with an app variable.
- An app redefines a shared task by listing it in `excludes`.
- Shared tasks set `dir: '{{.ROOT_DIR}}'`, so they run from the app root, including
  under `--local` and from subdirectories.
- `.taskrc.yml` trusts `github.com`, so there is no trust prompt, and caches the shared
  file. `.task/` is gitignored, and `--offline` uses that cache.
- No checksum is pinned, because the ref is a release tag.

**Shared tasks.**

| Area | Tasks |
|---|---|
| Develop | `dev` (templ watch with live reload), `templ:generate`, `templ:watch` |
| Local database | `db:up`, `db:down`, `db:logs`, `db:connect`, `db:wipe` (asks first) |
| Operations | `db:migrate`, `run -- <command>` (passthrough to the app binary) |
| Quality | `test`, `test:integration`, `check` (version drift, vet, staticcheck, gofmt, templ fmt, `go mod tidy -diff`, generated-code drift) |
| Build | `build`, `container:build`, `container:push` (`PLATFORMS` variable, tags from `git describe`) |
| Framework | `framework:update VERSION=…` |
| Secrets | optional `infisical run` wrapping when the app sets a project and environment |

**Integration tests.** `test:integration` starts a disposable PostgreSQL under its own
Compose project on its own port, with the Compose file inline. It overrides the app's
database variable with a fixed test DSN and tears the database down with `defer`. Tests
then create a fresh database per test through `app/apptest`.

**Containers.** `container:build` compiles the binaries on the host and copies them into
the image. Private-module credentials therefore never enter an image build.

**Lint.** `staticcheck` runs with `go run` at a version the shared Taskfile pins, so apps
carry no lint dependency in `go.mod` and a newer version arrives with the framework.

**This repository's tasks.** The repository's own `Taskfile.yml` reuses the shared
test-database tasks and adds `check` (with golangci-lint), `test`, `test:integration`,
`test:app` (generate a starter app from this checkout, run its tasks and serve it end to
end), `test:upgrade` (move a generated app to the next release), `test:example`,
`cli:build` and `release:verify`.

## Design decisions

These features were considered and decided as follows.

| Feature | Decision | Reason |
|---|---|---|
| Applying a refund automatically when it arrives before its payment | No | Rare. The refund becomes a review row and an operator adjusts. Automatic deferred application would be the most intricate code in the module. |
| Separate tables for reviews and for refunds waiting for their payment | One `payment_reviews` table | Every event that can't be applied is recorded with a fixed reason. Commands list and resolve them. |
| Module tables | Searchbase Cloud's, plus additions | Searchbase is the first app on these conventions. With its tables, it adopts the modules in one release, without moving or copying data, and its previous release keeps running during the switch. `internal/porttest` proves it. |
| Linking accounts | By verified email | A new identity joins the account with its verified address, from any provider, so users keep their accounts across provider changes and re-registrations. The provider's verified flag is trusted, and deleted accounts are gone, so an address never inherits someone else's account. |
| Deletion marks | Two columns: started and confirmed | The started mark stops a sign-up from joining, or deleting, an account while the provider decides. Only the confirmed mark lets a sign-up finish a deletion, so local data never goes before the provider confirms. Cancel commands are not needed. |
| Identity tombstones | Yes, as one hash table | Checked at account creation under the identity lock, it stops an in-flight request recreating a deleted person's account with their email. Per-request session validation already rejects stale sessions. |
| Expiry metrics | A hook, no metrics library | The module schedules its own runs, so `OnRun` hands the app every run's `Result`, hourly or from `credits expire`. The app records it with its own metrics library; C4Stack depends on none. |
| A scheduler switch and a warning-reset command | No | Expiry runs hourly under a lock and on demand with `credits expire`, and logs its outcomes. |
| Server timeouts as environment variables | No | Safe fixed defaults. `DrainDelay` and `ShutdownTimeout` are `web.Server` fields; an app exposes them as variables if it wants. |
| A strict database transport policy | Verified TLS by default | `verify-full` with the system roots for remote hosts, and `sslrootcert` when needed. An explicit `sslmode` is respected, with a warning. Ambient `PG*` variables are not policed. |
| Replay protection for API-key creation | No | A double submit creates a second key, which the user can revoke. |
| Rechecking a key's revocation inside credit transactions | Optional, with `LockActive` | A revocation applies from the next request; an app that spends credit per request checks the key in the same transaction. |
| Unverified email addresses | Turned away | Linking by email trusts verified addresses only, so an unverified identity gets a page asking to confirm the address, and no account. |
| JSON metadata on ledger entries | No | A description and a reference are enough. App-specific records go in app tables. |
| A payment completed after its account was deleted | Review | It is recorded as paid without credit; the right action is a refund, which is an operator decision. |
| A recent sign-in before deletion and passkey changes | Yes, 5 minutes | It proves the owner is present rather than an unattended browser. Five minutes is Searchbase's window for passkey changes; one window keeps one rule and one message. |
| A snapshot read for the credit summary | Yes | One read-only, repeatable-read transaction keeps the balance and holds consistent. |
| Serving outdated asset hashes | Yes, uncached | Pages from the previous release keep working during a rolling deploy. |

## Not in scope

These are not built:
- subscriptions, organisations, roles and entitlements
- internationalisation
- rate limiting, which belongs to the ingress
- metrics exporters
- an admin UI

Subscriptions or organisations would be new modules that depend on `accounts` and join
deletion through `OnDelete`, in the same way `credits` does.
