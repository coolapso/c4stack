# Guide

Recipes for the common changes to a C4Stack app. They start from the app `c4stack new`
generates: `main.go` wires modules, `routes.go` mounts routes, `layout.go` and
`pages.templ` hold the design. After editing a `.templ` file, run `task templ:generate`.
After adding a migration, run `task db:migrate`.

The account pages live under `/account` by default: settings at `/account/settings`,
deletion at `/account/delete`, API keys at `/account/api-keys` and credits at
`/account/credits`. The app's own signed-in pages go beside them, in the `/account`
group in `routes.go`. Every one of these paths can move; see "Move or replace an account
page".

`examples/textlab` in this repository is a complete app that uses every module.

## Add a signed-in page

Put the route in the `/account` group, which uses `accts.RequireSignIn`. Signed-in
routes then get CSRF checks, a valid session and the account. Read the account with
`accounts.Current`.

```go
// routes.go
account := r.Group("/account", accts.RequireSignIn)
account.GET("/notes", func(c *gin.Context) {
	layout.Render(c, http.StatusOK, "Notes", notesPage(c, accounts.Current(c).Email))
})
```

```templ
// pages.templ
templ notesPage(c *gin.Context, email string) {
	<div class="page-header">
		<h1>Notes</h1>
		<p class="lead">Signed in as { email }.</p>
	</div>
	<form class="stack" method="post" action="/account/notes">
		@ui.CSRFField(c)
		<div class="field">
			<label for="note">Note</label>
			<input id="note" name="body" required/>
		</div>
		<button class="btn primary">Save</button>
	</form>
}
```

Every form that changes something includes `@ui.CSRFField(c)`. HTMX requests send the
token on their own. To show the page in the navigation, add a `ui.Link` to `Nav` in
`layout.go`.

### A public form

Routes without sign-in that take form posts use `web.CSRF`:

```go
r.POST("/contact", web.CSRF, contact)
```

API routes and webhooks use neither `web.CSRF` nor `RequireSignIn`.

## Add an app table cleaned on account deletion

Add a migration to `migrations/`, numbered after the last one. Reference `accounts` with
`ON DELETE CASCADE`, so deleting an account deletes its rows:

```sql
-- migrations/001_notes.sql
CREATE TABLE notes (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    body text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
```

Query it through `a.DB()` with `database/sql`. Pass `c.Request.Context()`, not the Gin
context, to database calls.

```go
_, err := a.DB().ExecContext(c.Request.Context(), `
	INSERT INTO notes (account_id, body)
	VALUES ($1, $2)`, accounts.Current(c).ID, c.PostForm("body"),
)
```

When personal data cannot use a foreign key, remove it in an `OnDelete` hook. Hooks run
inside the transaction that deletes the account, so an error cancels the deletion:

```go
accts.OnDelete(func(ctx context.Context, tx *sql.Tx, accountID string) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM newsletter_signups
		WHERE email = (SELECT primary_email FROM accounts WHERE id = $1)`, accountID)
	return err
})
```

Never change a migration file once it has been applied anywhere. Add a new one.

## Enable API keys, credits or expiry

Each module needs its migrations in `main.go`, its constructor, and a `Mount` for its
page on a group of its own. List module migrations before the app's own.

```go
// main.go
Migrations: []app.Migrations{
	accounts.Migrations,
	apikeys.Migrations,
	credits.Migrations,
	expiry.Migrations,
	{Namespace: "app", Files: migrations.Files},
},
```

**API keys**:

```go
keys, err := apikeys.New(a, apikeys.Config{Prefix: "acme"}) // keys look like acme_…
// routes.go
keys.Mount(account.Group("/api-keys"), layout)
api := r.Group("/api/v1", keys.RequireKey)
```

Users create, rename and revoke keys, and delete revoked ones.

**Credits** take purchases through a payment provider. Add `ACME_CREEM_API_KEY`,
`ACME_CREEM_WEBHOOK_SECRET` and `ACME_CREEM_PRODUCT_ID` to `config.go`. The product is a
one-time product with a custom price.

```go
pay, err := creem.New(creem.Config{APIKey: cfg.CreemAPIKey, WebhookSecret: cfg.CreemWebhookSecret, ProductID: cfg.CreemProductID})
cr, err := credits.New(a, accts, credits.Config{
	Payments: pay,
	Policy:   credits.Policy{MinPurchaseCents: 500, MaxPurchaseCents: 50_000, FeeBasisPoints: 390, FeeFixedCents: 40},
})
// routes.go
cr.Mount(account.Group("/credits"), layout)
r.POST("/webhooks/payments", cr.Webhook)
```

Set the webhook URL in Creem to `https://<your host>/webhooks/payments`.

Without `Payments`, credits still work: the balance, the transactions and the app's
charges stay, the credits page says that buying credit is not available, and `Webhook`
answers 404. Use it while a payment provider reviews the site, or in a process that
only spends credit.

**Expiry** needs credits and an email sender that reports delivery, an
`email.TrackingSender` such as the Scaleway adapter. Without one it refuses to start, so
credit never expires without a delivered warning. Warnings go only to an account's
primary address while one of its identities verifies it, and a custom `Message` cannot
send them elsewhere.

```go
mail, err := scaleway.New(scaleway.Config{SecretKey: cfg.ScalewaySecretKey, ProjectID: cfg.ScalewayProjectID, From: "noreply@acme.example", FromName: "Acme"})
_, err = expiry.New(a, cr, expiry.Config{
	Sender:     mail,
	Policy:     expiry.Policy{InactiveMonths: 12, NoticeDays: 30},
	AppName:    "Acme",
	CreditsURL: cfg.PublicURL + "/account/credits",
})
```

To record metrics, set `OnRun`. It receives every run's outcome, hourly or from the
`credits expire` command: warnings sent and failed, accounts and cents expired, other
failures, and whether another replica's run made it skip. Record them with the app's
own metrics library:

```go
OnRun: func(ctx context.Context, r expiry.Result, err error) {
	if err != nil {
		runs.Add(ctx, 1, outcome("failure"))
		return
	}
	if !r.Skipped {
		runs.Add(ctx, 1, outcome("success"))
		lastSuccess.Record(ctx, time.Now().Unix())
	}
	warnings.Add(ctx, int64(r.Warned), outcome("sent"))
	warnings.Add(ctx, int64(r.WarningsFailed), outcome("failed"))
	expired.Add(ctx, r.ExpiredCents)
},
```

## Add an API endpoint that debits credit idempotently

A client retries a request it is not sure went through, with the same `Idempotency-Key`
header. The retry must not charge twice, and the key must not buy a different request
for free. Keep a row per key in an app table with a hash of the request and the result,
and charge and record it in one `WithAccount`:

```sql
-- migrations/002_summaries.sql
CREATE TABLE summaries (
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    idempotency_key text NOT NULL,
    request_hash bytea NOT NULL,
    result jsonb NOT NULL,
    PRIMARY KEY (account_id, idempotency_key)
);
```

```go
api.POST("/summaries", func(c *gin.Context) {
	ctx, accountID := c.Request.Context(), apikeys.Current(c).AccountID
	key := c.GetHeader("Idempotency-Key")
	body, err := io.ReadAll(c.Request.Body)
	if key == "" || err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "idempotency_key_required"})
		return
	}
	hash := sha256.Sum256(body)
	result := summarise(body)
	err = cr.WithAccount(ctx, accountID, func(tx *credits.Tx) error {
		var storedHash, stored []byte
		err := tx.SQL().QueryRowContext(ctx, `
			SELECT request_hash, result
			FROM summaries
			WHERE account_id = $1 AND idempotency_key = $2`, accountID, key,
		).Scan(&storedHash, &stored)
		switch {
		case err == nil && !bytes.Equal(storedHash, hash[:]):
			return errKeyReused // the key belongs to another request
		case err == nil:
			return json.Unmarshal(stored, &result) // a retry: the stored result, no charge
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if err := tx.Debit(5, "summary:"+key, "Summary"); err != nil {
			return err
		}
		data, _ := json.Marshal(result)
		_, err = tx.SQL().ExecContext(ctx, `
			INSERT INTO summaries (account_id, idempotency_key, request_hash, result)
			VALUES ($1, $2, $3, $4)`, accountID, key, hash[:], data)
		return err
	})
	switch {
	case errors.Is(err, credits.ErrInsufficient):
		c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{"error": "insufficient_credit"})
	case errors.Is(err, errKeyReused):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "idempotency_key_reused"})
	case err != nil:
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "unavailable"})
	default:
		c.JSON(http.StatusOK, result)
	}
})
```

The account lock in `WithAccount` makes concurrent retries wait for each other. The
ledger's own reference check is a second line of defence: it never charges one key
twice, but only the app's table knows whether a retry is the same request.
`examples/textlab` does this in `analyse`.

Charge before the work when the work is cheap and cannot fail on your side. When it
takes time or can fail, reserve the credit first with a hold.

## Add a credit hold

A hold reserves credit for work in progress, so the same credit cannot be spent twice.
The app keeps holds in its own table and reports their total through `Config.Holds`:

```go
credits.Config{
	// ...
	Holds: func(ctx context.Context, tx *sql.Tx, accountID string) (int64, error) {
		var held int64
		err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(sum(reserved_cents), 0)
			FROM jobs
			WHERE account_id = $1 AND status = 'pending'`, accountID,
		).Scan(&held)
		return held, err
	},
}
```

Reserve and capture inside `WithAccount`, which locks the account. The app's changes and
the charge commit together, or not at all:

```go
// Reserve.
err := cr.WithAccount(ctx, accountID, func(tx *credits.Tx) error {
	available, err := tx.Available()
	if err != nil {
		return err
	}
	if cost > available {
		return credits.ErrInsufficient
	}
	_, err = tx.SQL().ExecContext(ctx, `
		INSERT INTO jobs (id, account_id, reserved_cents)
		VALUES ($1, $2, $3)`, jobID, accountID, cost)
	return err
})

// Capture: settle the hold and charge what the job cost, in one transaction.
err = cr.WithAccount(ctx, accountID, func(tx *credits.Tx) error {
	if _, err := tx.SQL().ExecContext(ctx, `
		UPDATE jobs
		SET status = 'done'
		WHERE id = $1 AND status = 'pending'`, jobID); err != nil {
		return err
	}
	return tx.Capture(actualCost, "job:"+jobID, "Job")
})
```

`Capture` does not check the available credit, because the hold did when the work
started: finished work is charged even if a refund lowered the balance since. Charge no
more than the hold reserved.

Rules for holds:
- Lock nothing of the app's before `WithAccount`, and do no remote calls inside it.
- Release holds of failed or abandoned work, for example with a job that `a.Every` runs.
  Holds also defer credit expiry.

## Change the design

**Restyle.** Override the design tokens in `static/app.css`:

```css
:root {
    --accent: #0f766e;
    --radius: 4px;
    --font-sans: "Inter", system-ui, sans-serif;
}
```

Change the name, navigation, footer and head content through `ui.Design` in
`layout.go`.

**Replace.** Write your own layout and pass it wherever a `ui.Layout` goes. Include
`ui.Head` in `<head>` and `ui.LiveRegion` at the end of `<body>`, so HTMX and dialogs
work. Link `ui/base.css` to keep the default pages styled, or style the component
classes it defines yourself.

```go
layout := ui.Layout{
	Assets: assets,
	Page: func(c *gin.Context, title string, body templ.Component) templ.Component {
		return myLayout(c, assets, title, body)
	},
}
```

`examples/textlab/layout.templ` is a complete custom layout.

## Move or replace an account page

**Move.** The keys and credits pages go wherever their group is. The account pages go
where `accounts.Config.Paths` says; every redirect and link follows:

```go
accts, err := accounts.New(a, idp, accounts.Config{Paths: accounts.Paths{
	Settings: "/preferences",
	Delete:   "/preferences/close",
}})
```

**Replace.** Write the page you want on the module's functions instead of mounting the
module's page. The keys page uses `keys.List`, `keys.Create`, `keys.Rename`,
`keys.Revoke` and `keys.Delete`; the credits page uses `cr.Summary`, `cr.History`,
`cr.Quote`, `cr.Purchase` and `cr.PaymentStatus`.

To replace one account page, mount the others from `accts.Pages` at their paths, and
yours at its path:

```go
p := accts.Pages(layout)
r.GET("/account/settings", accts.RequireSignIn, mySettings) // yours
r.GET("/account/delete", accts.RequireSignIn, p.DeleteForm)
r.POST("/account/delete", accts.RequireSignIn, p.Delete)
r.POST("/logout", web.CSRF, p.Logout)
r.GET("/auth/email-unverified", p.Unverified)
r.GET("/account/deleted", p.Deleted)
r.GET("/account/email-changed", p.EmailChanged)
```

Keeping the default settings page, mount `p.Settings` on a group at its path and call
`p.MountProfile` on that group, so the provider's profile UI works.

## Allow more in the Content Security Policy

The default policy allows only the app's own origin. Add sources for every route:

```go
web.New(web.Config{
	// ...
	CSP: web.CSP{Images: []string{"https://images.acme.example"}, Connect: []string{"https://api.acme.example"}},
})
```

Or for one group only:

```go
r.Group("/maps", web.AllowCSP(web.CSP{Frames: []string{"https://maps.example"}}))
```

## Replace a provider

Providers are small interfaces. Implement one and pass it where the default goes:

| Interface | Methods | Passed to |
|---|---|---|
| `identity.Provider` | `Name`, `Authenticate`, `Logout`, `Delete` | `accounts.New` |
| `payments.Provider` | `Name`, `CheckoutOrigin`, `CreateCheckout`, `ParseWebhook` | `credits.Config.Payments` |
| `email.TrackingSender` | `Send`, `Delivery` | `expiry.Config.Sender` |

```go
type Keycloak struct{ /* ... */ }

func (k *Keycloak) Name() string { return "keycloak" }
func (k *Keycloak) Authenticate(r *http.Request) (identity.Identity, error) { /* validate the session */ }
func (k *Keycloak) Logout(w http.ResponseWriter, r *http.Request) error     { /* end it */ }
func (k *Keycloak) Delete(ctx context.Context, r *http.Request) error       { /* delete the user, confirmed */ }

accts, err := accounts.New(a, keycloak, accounts.Config{})
```

The contracts that matter:
- `Authenticate` returns `identity.ErrNoSession` when there is no valid session, and
  reports only verified email addresses as verified. A new identity joins the account
  that has its verified address, so this is what keeps accounts safe.
- `Delete` returns nil only after the provider confirmed the deletion.
- `ParseWebhook` verifies the signature before anything else.

Accounts find their identities by provider name and subject. When an app switches
identity provider, each user's first sign-in with the new provider joins the account
that has their verified address, so users keep their accounts.

Test substitutes live in `_test.go` files. `examples/textlab/testing_test.go` has a test
identity provider and a test payment provider.

## Run a process without sign-in or payments

An API gateway that authenticates with API keys and charges credit needs neither the
identity provider nor the payment provider, and should not hold their secrets. Pass a
nil identity provider and no payments:

```go
accts, err := accounts.New(a, nil, accounts.Config{})
keys, err := apikeys.New(a, apikeys.Config{Prefix: "acme"})
cr, err := credits.New(a, accts, credits.Config{Holds: heldByRequests})

api := r.Group("/v1", keys.RequireKey)
```

`RequireSignIn`, the account pages and `Delete` are then unavailable. Inside the
transaction that reserves credit for a request, `keys.LockActive` checks that the key is
still active, so a key revoked meanwhile cannot spend:

```go
err := cr.WithAccount(ctx, key.AccountID, func(tx *credits.Tx) error {
	if err := keys.LockActive(ctx, tx.SQL(), key.AccountID, key.ID); err != nil {
		return err
	}
	// reserve, as in "Add a credit hold"
})
```

## Adopt an existing database

An app that created its tables before it used C4Stack can keep them. Module migrations
use `IF NOT EXISTS` throughout, so they adopt tables that already have the module's
shape and add what is missing, such as new columns and tables.

1. Match the module tables. `docs/architecture.md` lists each module's tables; the
   module's `migrations/001_initial.sql` is the exact shape. Columns the app added of
   its own can stay.
2. List the app's existing migration files first, in their own namespace, then the
   modules, then a new namespace for the app's later migrations:

   ```go
   Migrations: []app.Migrations{
   	{Namespace: "acme", Files: legacy.Files}, // the files that created the tables
   	accounts.Migrations,
   	apikeys.Migrations,
   	credits.Migrations,
   	{Namespace: "acme_app", Files: migrations.Files},
   },
   ```

3. Run `migrate`. A history recorded without namespaces is converted: each recorded
   file goes to the set that has a file with the same version and checksum. A recorded
   file that no set has stops the run before anything changes.

Keep each step additive, so the release before the move keeps running against the
converged database while a deployment switches over.

Expiry warnings the app sent before the move count only if it knows they were
delivered. Otherwise leave them out: their recipients are warned again, and their
credit expires a full notice period after that warning arrives. `internal/porttest` moves
Searchbase Cloud's database this way and checks every step.

## Add an operator command

Register it in `setup`. It runs as `acme <name> [arguments]`, or `task run -- <name>`
during development:

```go
a.Command(app.Command{
	Name:  "notes purge",
	Usage: "<days>",
	Help:  "Delete notes older than the given number of days.",
	Run: func(ctx context.Context, args []string) error {
		// ...
		return nil
	},
})
```

For work on a schedule while the app serves, use `a.Every`. Take a database lock if
replicas must not run it twice at once.

## Test the app

Integration tests use a fresh database on a disposable PostgreSQL server that
`task test:integration` starts. Mark them with the `integration` build tag:

```go
//go:build integration

func TestNotes(t *testing.T) {
	a := apptest.New(t, app.Config{Name: "acme", Migrations: []app.Migrations{
		accounts.Migrations,
		{Namespace: "app", Files: migrations.Files},
	}})
	accts, err := accounts.New(a, testIdentity{}, accounts.Config{})
	// ...
}
```

`apptest.New` applies the migrations and gives the test an `App`. Wire what the test
needs with test providers.

## Upgrade the framework

```sh
task framework:update VERSION=v0.2.0
```

It moves the Go dependency and the shared tasks to the release together, then tidies,
regenerates, tests and builds. Then:
1. Read the [release notes](https://github.com/coolapso/c4stack/releases).
2. Run `task db:migrate` locally.
3. Deploy: run the migrate job before rolling out the new image.

`task check` fails if the `FRAMEWORK_VERSION` in `Taskfile.yml` and the release in
`go.mod` ever differ.

## Change a shared task

List the task under `excludes` in the include, and define your own with the same name:

```yaml
includes:
  shared:
    taskfile: https://github.com/coolapso/c4stack.git//taskfiles/app.yml?ref={{.FRAMEWORK_VERSION}}
    flatten: true
    excludes: [build]

tasks:
  build:
    cmds:
      - CGO_ENABLED=0 go build -tags netgo -o bin/acme .
```

Add new tasks under `tasks` the same way.
