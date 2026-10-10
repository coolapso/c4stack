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
| `app` | The registry that modules join (database, migrations, commands, background jobs), plus the command-line entry point |
| `app/apptest` | A fresh disposable database per test, with the app's migrations applied |
| `ui` | The `Layout` type, the default design (CSS, vendored HTMX, a small script) and content-hashed assets |
| `identity` | The identity provider interface |
| `identity/hanko` | The Hanko adapter, its login page and its profile UI |
| `accounts` | Accounts mapped from identities, sign-in middleware, the settings page and account deletion |
| `apikeys` | Opt-in. API keys, bearer middleware and the keys page |
| `payments` | The payment provider interface |
| `payments/creem` | The Creem adapter |
| `credits` | Opt-in. Ledger, holds hook, checkout, webhook, history and the credits page |
| `credits/expiry` | Opt-in. Expires inactive credit after a delivered warning |
| `email` | The email sender interface |
| `email/scaleway` | The Scaleway Transactional Email adapter |
| `cmd/c4stack` | The app generator |

PostgreSQL connection and migration code lives in `internal/postgres`. Apps reach it
through `app`.

### Dependency direction

- `web` and `internal/postgres` depend on nothing else in C4Stack. `app` depends on both;
  `ui` depends on `web`.
- The interface packages `identity`, `payments` and `email` depend on nothing in C4Stack.
  Each adapter depends on its interface and on `web` and `ui` where it renders pages.
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
			{Namespace: "app", Files: migrations},
		},
		Setup: setup,
	})
}

// setup wires the application for every command except migrate.
func setup(ctx context.Context, a *app.App) error {
	cfg, err := loadConfig() // config.go: reads the other ACME_* variables
	if err != nil {
		return err
	}
	a.SetLogger(cfg.logger())
	idp, err := hanko.New(hanko.Config{APIURL: cfg.HankoURL})
	if err != nil {
		return err
	}
	accts, err := accounts.New(a, idp, accounts.Config{LoginPath: "/login"})
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
r.GET("/", home(layout))
idp.Mount(r, layout) // GET /login
accts.Mount(r.Group("/account", accts.RequireSignIn), layout) // settings, delete, logout
```

Enabling API keys adds `apikeys.Migrations` to the list in `main.go`, plus three lines:

```go
keys, err := apikeys.New(a, accts, apikeys.Config{Prefix: "acme"})
keys.Mount(r.Group("/account/keys", accts.RequireSignIn), layout)
api := r.Group("/api", keys.RequireKey)
```

### What a module brings

Each module package exports its migrations as a value, such as `accounts.Migrations`,
under the module's own namespace. The app lists them in `main.go`, in order: modules
first, then the app's own `migrations/` directory, whose tables may reference module
tables.

A module constructor registers the module's operator commands, its background jobs and
its account-deletion hook. It fails with a clear error when the app's migration list lacks
the module, so the two cannot get out of step unnoticed. `Mount` adds the module's
default pages under the router group the app passes in. The pages link to each other
relative to that group, so the app can put them anywhere.

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
- every command a module or the app registered, such as `accounts delete-local` or
  `credits adjust`
- `help`

**`migrate` needs only the database.** It reads `Config.DatabaseURL` and
`Config.Migrations`, applies them and exits. It doesn't run `setup`, load other
configuration, construct providers or start a server. It follows the least-privilege
principle, like Searchbase's migrate job: run the same image with the `migrate` command
as a Job before the rollout, with only the database secret. That Job can use a database
role that owns the schema, while the server's role only reads and writes data.

Every other command runs `setup` first, so it validates the configuration of every
enabled module. Operator commands run with the server's configuration.

## Module surfaces

The signatures below are a sketch. Names may still change during implementation; the
shape should not.

### web

```go
func New(cfg Config) (*gin.Engine, error)

type Config struct {
	PublicURL      string   // canonical origin, e.g. https://acme.example
	SecretKey      string   // at least 32 bytes; signs CSRF tokens and seals cookies
	CSP            CSP      // sources added to the default policy everywhere
	MaxBodyBytes   int64    // default 1 MiB
	TrustedProxies []string // default: none
	Logger         *slog.Logger
}
type CSP struct{ Scripts, Styles, Images, Fonts, Connect, Frames, FormActions []string }

func CSRF(c *gin.Context)                    // checks token and Origin on unsafe methods
func CSRFToken(c *gin.Context) string
func SetSealedCookie(c *gin.Context, name string, value []byte, maxAge time.Duration)
func SealedCookie(c *gin.Context, name string) ([]byte, error) // encrypted and authenticated
func AllowCSP(extra CSP) gin.HandlerFunc     // extends the policy for one route group
func MaxBody(n int64) gin.HandlerFunc        // overrides the body limit for one group
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
}
func Serve(ctx context.Context, s Server, ready func(context.Context) error) error
```

**Middleware.** The engine runs, in order:
1. A generated request ID. Incoming IDs are ignored.
2. A request log with the route template, status and duration. It never logs query
   strings, headers or bodies.
3. Panic recovery, which logs the stack but not the panic value or the headers.
4. A host check, which answers 421 for any host but the public origin.
5. Security headers.
6. A body limit.
7. CSRF cookie issuance.

The app keeps the engine. It can add middleware or set `NoRoute`, and C4Stack never
changes Gin's global mode.

**CSRF.** C4Stack uses a signed double-submit cookie (`__Host-csrf` on HTTPS, `csrf` on
plain-HTTP localhost) plus an exact Origin match against `PublicURL`. The cookie holds a
random value and the form token is its HMAC, so a cookie planted by an attacker never
matches a token they can produce. Forms send
`csrf_token`. HTMX requests send `X-CSRF-Token`, which the default page script copies
from `<meta name="csrf-token">`.

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
- The Hanko login page adds the Hanko origin to `connect-src`. It also adds inline styles,
  because Hanko's elements inject them.
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

**Database.** `Main` opens the pool before `setup` runs, using pgx through `database/sql`. For any host that isn't a Unix
socket or loopback address, it sets `sslmode=verify-full` with the system roots unless
the URL already sets `sslmode`. An explicit weaker mode is respected, with a startup
warning.

**Migrations.** Namespaces apply in the order of `Config.Migrations`. Each file runs in
its own transaction, all under one advisory lock. The `schema_migrations` table records
the namespace, version and checksum of each file.

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
type Design struct {
	Name        string          // header and <title>
	Nav         []Link
	Head        templ.Component // favicon, meta tags, extra scripts
	Stylesheets []string        // linked after the default stylesheet
	Footer      templ.Component
}
type Link struct{ Label, Href string }

func Default(d Design, assets *Assets) Layout

func NewAssets(base string) *Assets              // includes the default design's files
func (a *Assets) Add(prefix string, files fs.FS) error
func (a *Assets) URL(name string) string         // /assets/<hash>/ui/base.css
func (a *Assets) Serve(c *gin.Context)

// Components default pages and custom layouts share.
func Head(c *gin.Context, assets *Assets) templ.Component // CSRF meta tag, HTMX, page script
func CSRFField(c *gin.Context) templ.Component
func Dialog(p DialogProps) templ.Component
```

**Levels of design freedom.** Apps choose one of three:
1. **Default.** `ui.Default(ui.Design{Name: "Acme"}, assets)`.
2. **Restyle.** Add a stylesheet that overrides design tokens, plus a favicon in `Head`,
   `Nav` links and a `Footer`.
3. **Own layout.** `ui.Layout{Page: myLayout, Assets: assets}` with the app's own Templ
   and CSS. It includes `ui.Head` and `ui.LiveRegion` so HTMX and dialogs work. Default
   pages render inside it, using documented component classes. A custom layout either
   links `ui/base.css`, styles those classes itself, or replaces the pages.

**The default design.** It is one hand-written stylesheet of design tokens and component
classes, with no build step, supporting light and dark schemes. Class and token names are
neutral, with no framework prefix.

**Assets.** Asset URLs carry a per-file content hash. The current hash is served as
immutable for a year. Any other hash gets the current file with `no-cache`, so pages from
the previous release keep working during a rolling deploy.

Modules add their own files with `Add` when they mount; the Hanko adapter adds its
vendored elements and scripts.

### identity and identity/hanko

```go
type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	SignedInAt    time.Time // when the session was created
}
func (id Identity) Recent(now time.Time) bool // signed in within RecentSignIn (10 minutes)
func NewContext(ctx context.Context, id Identity) context.Context
func FromContext(ctx context.Context) (Identity, bool)
type Provider interface {
	Name() string                                   // stored with each account, e.g. "hanko"
	Authenticate(r *http.Request) (Identity, error) // ErrNoSession when not signed in
	Logout(w http.ResponseWriter, r *http.Request) error
	Delete(ctx context.Context, r *http.Request) error // returns only after the provider confirms
}
```

**How the Hanko adapter works.**
- `Authenticate` calls `POST /sessions/validate` on every request. There is no local JWT
  check, so a signed-out or deleted user's token stops working at once.
- `Logout` calls `POST /logout` and clears the cookie.
- `Delete` runs Hanko's profile flow action `account_delete` with the user's own session,
  and succeeds only on the `account_deleted` state. No admin API key is needed.
- `Mount` adds `GET /login`, a page with Hanko's `<hanko-auth>` element inside the app's
  layout. A user who already has a session continues to the `next` path.

**Profile UI.** The adapter can also offer a profile UI, through three methods that
`accounts` looks for: `MountProfile`, `ProfileCSP` and `ProfileSection`. When it does,
the accounts settings page embeds it. Hanko's profile UI has everything Searchbase's settings page has:
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
- **Sign-out.**

Passkey changes require a recent sign-in, as deletion does (see below). The profile UI
never offers account deletion, which always goes through `accounts`.

**Hanko tenant settings.** The tenant must:
- allow account deletion, for the flow action
- require email verification
- send the `hanko` cookie to the app's host

### accounts

```sql
accounts(id uuid, provider, subject, email, created_at, UNIQUE (provider, subject))
deleted_identities(identity_hash bytea, deleted_at)
```

```go
type Account struct{ ID, Email string; CreatedAt time.Time }
type Config struct{ LoginPath string }

func New(a *app.App, idp identity.Provider, cfg Config) (*Accounts, error)
func (s *Accounts) RequireSignIn(c *gin.Context)
func Current(c *gin.Context) Account
func (s *Accounts) Get(ctx context.Context, id string) (Account, error)
func (s *Accounts) OnDelete(hook func(ctx context.Context, tx *sql.Tx, accountID string) error)
func (s *Accounts) Delete(c *gin.Context) error // ErrNotDeleted or ErrCleanupPending on failure
func (s *Accounts) Mount(r gin.IRouter, layout ui.Layout)
// GET "" settings, POST "/delete", POST "/logout" (continues to a local "next" path)
```

Signing out lives here rather than in the provider adapter, because it is the same for
every provider: `Logout` ends the session at the provider and clears the browser.

The operator command is `accounts delete-local <account-id>`.

**`RequireSignIn`** checks CSRF, authenticates the session, then finds the account,
creating it on first sign-in. It sets `Cache-Control: no-store`. Unauthenticated requests
are redirected to `LoginPath` with a safe `next` path, or get `HX-Redirect` from HTMX.

Accounts are found by provider and subject only, and are never linked by email. `email`
holds the provider's verified address, or nothing if the address is unverified.

**Deletion** runs these steps:
1. The user submits the settings form with CSRF and types `DELETE`. The session must
   have been created in the last 10 minutes; otherwise the page asks the user to sign in
   again and then returns to settings. This proves the owner is present, not just an
   unattended browser.
2. `idp.Delete` runs with a context detached from the request, so a closed tab cannot
   stop it halfway, and with a one-minute budget.
3. Only after the provider confirms, one transaction does the local deletion:
   1. It locks the account row and the identity.
   2. It runs the `OnDelete` hooks: modules first, then the app.
   3. It records a hash of the identity in `deleted_identities`.
   4. It deletes the account. Foreign keys cascade.

If the provider fails, nothing is deleted locally and the user can retry. If the local
transaction fails after the provider deleted the identity, the failure is logged with the
account ID, and an operator runs `accounts delete-local`. The two systems are not atomic.

Account creation checks `deleted_identities` under the same lock, so a request that
validated its session just before deletion cannot recreate the account.

**App data.** App tables with personal data join deletion through
`REFERENCES accounts (id) ON DELETE CASCADE` or through `OnDelete`.

### apikeys (opt-in)

```sql
api_keys(id uuid, account_id → accounts ON DELETE CASCADE, name, prefix, suffix,
         hash bytea UNIQUE, created_at, last_used_at, revoked_at)
```

```go
type Config struct{ Prefix string } // "acme" issues acme_<43 characters>

func New(a *app.App, accts *accounts.Accounts, cfg Config) (*Keys, error)
func (k *Keys) Create(ctx context.Context, accountID, name string) (raw string, key Key, err error)
func (k *Keys) List(ctx context.Context, accountID string) ([]Key, error)
func (k *Keys) Revoke(ctx context.Context, accountID, keyID string) error
func (k *Keys) RequireKey(c *gin.Context)
func Current(c *gin.Context) Key
func (k *Keys) Mount(r gin.IRouter, layout ui.Layout)
```

**Storage.** A raw key is the prefix, an underscore and 32 random bytes in base64url.
C4Stack stores the SHA-256 of the whole raw key plus its last six characters. Lookup is by
hash, so changing `Prefix` never invalidates existing keys. The raw key is shown once in
the create response and never logged.

**`RequireKey`** requires exactly one `Authorization: Bearer` header and ignores cookies.
It answers JSON 401 with `WWW-Authenticate` when the key is missing or invalid. It records
last use at most every five minutes. A revocation applies from the next request.

### payments and payments/creem

```go
type Checkout struct {
	Reference   string // the credits intent ID, echoed back in webhook events
	AmountCents int64  // what the buyer pays, in EUR cents
	Email       string
	SuccessURL  string
}
type Event struct {
	ID          string    // provider event ID
	Kind        EventKind // Paid, Refunded, Disputed
	Reference   string
	CheckoutID  string
	AmountCents int64
	Currency    string
	Review      string // set when the event is ours but cannot be applied as it stands
}
type Provider interface {
	Name() string
	CheckoutOrigin() string
	CreateCheckout(ctx context.Context, c Checkout) (Session, error) // Session{ID, URL}
	ParseWebhook(header http.Header, body []byte) (Event, error) // ErrInvalid, ErrIgnored
}
```

**How the Creem adapter works.**
- Checkout uses one configured product with `custom_price`.
- An API key starting with `creem_test_` selects Creem's test API.
- Webhooks are verified with HMAC-SHA256 from the `creem-signature` header.
- The `checkout_url` Creem returns must match `CheckoutOrigin`.

### credits (opt-in)

Tables: `payment_intents`, `credit_ledger` (append-only, enforced by a trigger) and
`payment_reviews`.

```go
type Policy struct {
	MinPurchaseCents, MaxPurchaseCents int64 // required
	FeeBasisPoints, FeeFixedCents      int64 // added on top, grossed up to cover itself
}
type Config struct {
	Payments payments.Provider
	Policy   Policy
	Holds    func(ctx context.Context, tx *sql.Tx, accountID string) (int64, error) // optional
}

func New(a *app.App, accts *accounts.Accounts, cfg Config) (*Credits, error)
func (cr *Credits) Debit(ctx context.Context, accountID string, cents int64, reference, description string) error
func (cr *Credits) Adjust(ctx context.Context, accountID string, cents int64, reference, description string) error
func (cr *Credits) WithAccount(ctx context.Context, accountID string, fn func(tx *Tx) error) error
func (tx *Tx) SQL() *sql.Tx
func (tx *Tx) Available() (int64, error)
func (tx *Tx) Debit(cents int64, reference, description string) error
func (cr *Credits) Summary(ctx context.Context, accountID string) (Summary, error) // balance, held, available
func (cr *Credits) History(ctx context.Context, accountID string, before int64, limit int) ([]Entry, error)
func (cr *Credits) Mount(r gin.IRouter, layout ui.Layout) // GET "", POST "/checkout", GET "/balance"
func (cr *Credits) Webhook(c *gin.Context)
```

The operator commands are `credits adjust`, `credits reviews` and `credits resolve`.

**Ledger rules.**
- Every mutation locks the account row.
- Every mutation is idempotent per account and reference. A retry with the same amount
  succeeds without a new entry. A retry with a different amount returns `ErrConflict`.
- Debits never take available credit below zero. Available credit is the balance minus
  what the app's `Holds` function reports.
- `WithAccount` lets app reservations share the lock and transaction with framework
  debits.

**Webhooks.** The webhook grants credit from the stored intent, never from the payload.
- A paid event credits once.
- A full refund or dispute of a paid intent reverses once.
- Any other event about one of our intents becomes a review row.
- Foreign events are acknowledged and ignored.

On account deletion, the credits hook forfeits a positive balance and anonymises the
account's ledger and intents. `docs/credits.md` has the full contract and the webhook
outcome table.

### credits/expiry (opt-in)

```go
type Policy struct{ InactiveMonths, NoticeDays int } // required

type Config struct {
	Sender     email.Sender // required
	Policy     Policy
	CreditsURL string                     // linked from the warning
	Message    func(Notice) email.Message // optional; the default names the app
}

func New(a *app.App, cr *credits.Credits, cfg Config) (*Expiry, error)
```

`New` registers the `credit_expiry_notices` table, the `credits expire` command and an
hourly job during `serve`. The job runs under an advisory lock, so replicas don't
duplicate it.

**Activity.** Purchases, debits and adjustments count as activity.

**Warnings.** A warning goes out only to a verified email address, outside any
transaction. Its deadline is recorded only after delivery succeeds. A failing address is
retried at most daily.

**Expiry.** Credit expires only after the full notice period from delivery has passed.
New activity starts a fresh cycle, and a hold defers expiry. An announced deadline never
gets shorter.

### email and email/scaleway

```go
type Message struct{ To, Subject, Text string }
type Sender interface{ Send(ctx context.Context, m Message) error }
```

`scaleway.New(scaleway.Config{SecretKey, ProjectID, Region, From, FromName, ReplyTo})`. The
region defaults to `fr-par`. Errors carry the HTTP status, never the provider's response
body or the recipient.

## Configuration and naming

**Environment variables.** C4Stack reads no environment variables. The app's `config.go`
reads `<PREFIX>_*`, where the generator derives the prefix from the app name, and passes
the values in code. A minimal app needs four, plus an optional `ACME_ADDR`:
- `ACME_PUBLIC_URL`
- `ACME_DATABASE_URL`, the only variable `migrate` reads
- `ACME_SECRET_KEY`, from which C4Stack derives separate CSRF and cookie keys with HKDF
- `ACME_HANKO_API_URL`

**Other names users can see.**
- Cookies: `csrf` and `__Host-csrf`, plus a sealed cookie during an email change. The
  `hanko` cookie is Hanko's own.
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
  migration, so there is no no-op version 1 to trap the app.
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
  FRAMEWORK_VERSION:
    sh: awk '$1 == "github.com/coolapso/c4stack" {print $2}' go.mod
includes:
  shared:
    taskfile: https://github.com/coolapso/c4stack.git//taskfiles/app.yml?ref={{.FRAMEWORK_VERSION}}
    flatten: true
    excludes: []
```

**Deviation from the brief.** The include ref is read from `go.mod`, not written in a
second place. The ref and the Go dependency therefore cannot drift. `framework:update`
only bumps the Go dependency, then tidies, regenerates, tests and builds. `check` doesn't
need a drift check, so it has none.

**Task mechanics.** These were verified with Task 3.53.1 and 3.54.0 against a local Git
server. Task 3.51.1 and 3.52.0 still treat remote Taskfiles as an experiment.
- A Git include works with the developer's Git credentials, and its ref can come from a
  dynamic variable.
- Top-level app variables reach the shared tasks.
- An app redefines a shared task by listing it in `excludes`.
- Shared tasks set `dir: '{{.ROOT_DIR}}'`, so they run from the app root, including
  under `--local`.
- `.taskrc.yml` trusts `github.com`, so there is no trust prompt.
- `.task/` is gitignored, and `--offline` uses that cache.
- No checksum is pinned, because the ref is a release tag.

**Shared tasks.**

| Area | Tasks |
|---|---|
| Develop | `dev` (templ watch with live reload), `templ:generate`, `templ:watch` |
| Local database | `db:up`, `db:down`, `db:logs`, `db:connect`, `db:wipe` (asks first) |
| Operations | `db:migrate`, `run -- <command>` (passthrough to the app binary) |
| Quality | `test`, `test:integration`, `check` (vet, staticcheck, gofmt, templ fmt, `go mod tidy -diff`, generated-code drift) |
| Build | `build`, `container:build`, `container:push` (`PLATFORMS` variable, tags from `git describe`) |
| Framework | `framework:update VERSION=…` |
| Secrets | optional `infisical run` wrapping when the app sets a project and environment |

**Integration tests.** `test:integration` starts a disposable PostgreSQL under its own
Compose project on its own port, with the Compose file inline. It overrides the app's
database variable with a fixed test DSN and tears the database down with `defer`. Tests
then create a fresh database per test through `app/apptest`.

**Containers.** `container:build` compiles the binaries on the host and copies them into
the image. Private-module credentials therefore never enter an image build.

`staticcheck` runs with `go run` at a version the shared Taskfile pins, so apps carry no
lint dependency in `go.mod` and a newer version arrives with the framework.

This repository's own `Taskfile.yml` includes `taskfiles/app.yml` locally and adds
framework tasks: `check` (adds golangci-lint), `test`, `test:integration`, `test:app`
(generate a starter app from this checkout, run its tasks and serve it end to end),
`test:upgrade`, `cli:build` and `release:verify`.

## Decisions on carried-over features

| Old feature | Decision | Reason |
|---|---|---|
| Automatic deferred application of refunds that arrive before their payment | Dropped | Rare. The reversal becomes a review row and an operator adjusts. This removes the most intricate code. |
| Payment review and pending-reversal tables | One `payment_reviews` table | Every event that can't be applied is recorded with a fixed reason. Commands list and resolve them. |
| Deletion-pending state and cancel commands | Dropped | Deletion is one request: the provider first, then one local transaction. A failure in between is logged and fixed with `accounts delete-local`. |
| Identity tombstones | Kept, simpler | One hash table, checked at account creation under the identity lock. It prevents an in-flight request recreating a deleted person's account with their email. Per-request session validation already rejects stale sessions. |
| Expiry metrics, scheduler flag, warning-reset command | Dropped | Expiry runs hourly under a lock and on demand with `credits expire`, and outcomes are logged. Announced deadlines never shorten, so a policy change applies to new warnings without a reset. Apps add metrics if they need them. |
| Six server-timeout env vars | Dropped | Safe fixed defaults. `DrainDelay` and `ShutdownTimeout` are `web.Server` fields; an app exposes them as env vars if it wants. |
| Strict DB transport policy | Simplified | `verify-full` by default for remote hosts, using system roots (`sslrootcert` is optional). An explicit `sslmode` is respected, with a warning. Ambient `PG*` variables are not policed. |
| API-key `creation_id` replay protection | Dropped | A double submit creates a second key, which the user can revoke. |
| Revocation recheck inside credit transactions | Dropped | A revocation applies from the next request. |
| Email-unverified page | Dropped | Unverified addresses are not stored, so nothing is mailed to them. The Hanko tenant should require verification. |
| Ledger metadata JSON | Dropped | A description and a reference are enough. App-specific records go in app tables. |
| Payment completed after its account was deleted | Review instead of grant-and-forfeit | The right action is a refund, which is an operator decision. |
| Recent sign-in before deletion | Kept, simple | The session's creation time must be within 10 minutes. It proves the owner is present rather than an unattended browser, and the same check covers passkey changes. |
| Snapshot read for the credit summary | Kept | One read-only, repeatable-read transaction keeps the balance and holds consistent. |
| Old asset hashes served with `no-cache` | Kept | Rolling deploys keep working. |

## Not in scope

These are not built now:
- subscriptions, organisations, roles and entitlements
- internationalisation
- rate limiting, which belongs to the ingress
- metrics exporters
- an admin UI

Subscriptions or organisations would be new modules that depend on `accounts` and join
deletion through `OnDelete`, in the same way `credits` does.
