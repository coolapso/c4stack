# Guide

Recipes for the common changes to a C4Stack app. They start from the app `c4stack new`
generates: `main.go` wires modules, `routes.go` mounts routes, `layout.go` and
`pages.templ` hold the design. After editing a `.templ` file, run `task templ:generate`.
After adding a migration, run `task db:migrate`.

`examples/textlab` in this repository is a complete app that uses every module.

## Add a signed-in page

Put the route in a group that uses `accts.RequireSignIn`. Signed-in routes then get
CSRF checks, a valid session and the account. Read the account with `accounts.Current`.

```go
// routes.go
notes := r.Group("/notes", accts.RequireSignIn)
notes.GET("", func(c *gin.Context) {
	account := accounts.Current(c)
	layout.Render(c, http.StatusOK, "Notes", notesPage(c, account.Email))
})
```

```templ
// pages.templ
templ notesPage(c *gin.Context, email string) {
	<div class="page-header">
		<h1>Notes</h1>
		<p class="lead">Signed in as { email }.</p>
	</div>
	<form class="stack" method="post" action="/notes">
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
		WHERE email = (SELECT email FROM accounts WHERE id = $1)`, accountID)
	return err
})
```

Never change a migration file once it has been applied anywhere. Add a new one.

## Enable API keys, credits or expiry

Each module needs its migrations in `main.go`, its constructor, and a `Mount` for its
pages. List module migrations before the app's own.

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
keys.Mount(r.Group("/account/keys", accts.RequireSignIn), layout)
api := r.Group("/api/v1", keys.RequireKey)
```

**Credits** need a payment provider. Add `ACME_CREEM_API_KEY`, `ACME_CREEM_WEBHOOK_SECRET`
and `ACME_CREEM_PRODUCT_ID` to `config.go`. The product is a one-time product with a
custom price.

```go
pay, err := creem.New(creem.Config{APIKey: cfg.CreemAPIKey, WebhookSecret: cfg.CreemWebhookSecret, ProductID: cfg.CreemProductID})
cr, err := credits.New(a, accts, credits.Config{
	Payments: pay,
	Policy:   credits.Policy{MinPurchaseCents: 500, MaxPurchaseCents: 50_000, FeeBasisPoints: 390, FeeFixedCents: 40},
})
// routes.go
cr.Mount(r.Group("/account/credits", accts.RequireSignIn), layout)
r.POST("/webhooks/payments", cr.Webhook)
```

Set the webhook URL in Creem to `https://<your host>/webhooks/payments`.

**Expiry** needs credits and an email sender. Without a sender it refuses to start, so
credit never expires unwarned.

```go
mail, err := scaleway.New(scaleway.Config{SecretKey: cfg.ScalewaySecretKey, ProjectID: cfg.ScalewayProjectID, From: "noreply@acme.example", FromName: "Acme"})
_, err = expiry.New(a, cr, expiry.Config{
	Sender:     mail,
	Policy:     expiry.Policy{InactiveMonths: 12, NoticeDays: 30},
	AppName:    "Acme",
	CreditsURL: cfg.PublicURL + "/account/credits",
})
```

## Add an API endpoint that debits credit idempotently

Debit with a reference the client controls, such as an `Idempotency-Key` header. A retry
with the same reference and amount succeeds without charging twice.

```go
api.POST("/summaries", func(c *gin.Context) {
	key := c.GetHeader("Idempotency-Key")
	if key == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "idempotency_key_required"})
		return
	}
	err := cr.Debit(c.Request.Context(), apikeys.Current(c).AccountID, 5, "summary:"+key, "Summary")
	switch {
	case errors.Is(err, credits.ErrInsufficient):
		c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{"error": "insufficient_credit"})
		return
	case errors.Is(err, credits.ErrConflict):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "idempotency_key_reused"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "unavailable"})
		return
	}
	c.JSON(http.StatusOK, summarise(c))
})
```

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
the debit commit together, or not at all:

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

// Capture: release the hold and charge what the job cost, in one transaction.
err = cr.WithAccount(ctx, accountID, func(tx *credits.Tx) error {
	if _, err := tx.SQL().ExecContext(ctx, `
		UPDATE jobs
		SET status = 'done'
		WHERE id = $1 AND status = 'pending'`, jobID); err != nil {
		return err
	}
	return tx.Debit(actualCost, "job:"+jobID, "Job")
})
```

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

**Replace one default page.** Mount the module's routes yourself and write the page you
want on the module's store. For example, list keys with `keys.List`, create them with
`keys.Create` and revoke them with `keys.Revoke`.

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
| `email.Sender` | `Send` | `expiry.Config.Sender` |

```go
type Keycloak struct{ /* ... */ }

func (k *Keycloak) Name() string { return "keycloak" }
func (k *Keycloak) Authenticate(r *http.Request) (identity.Identity, error) { /* validate the session */ }
func (k *Keycloak) Logout(w http.ResponseWriter, r *http.Request) error     { /* end it */ }
func (k *Keycloak) Delete(ctx context.Context, r *http.Request) error       { /* delete the user, confirmed */ }

accts, err := accounts.New(a, keycloak, accounts.Config{LoginPath: "/login"})
```

The contracts that matter:
- `Authenticate` returns `identity.ErrNoSession` when there is no valid session, and
  reports only verified email addresses as verified.
- `Delete` returns nil only after the provider confirmed the deletion.
- `ParseWebhook` verifies the signature before anything else.

Accounts are stored by provider name and subject. Switching the identity provider of an
app that already has users needs a data migration of your own.

Test substitutes live in `_test.go` files. `examples/textlab/testing_test.go` has a test
identity provider and a test payment provider.

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
1. Read the release notes in `docs/releases/`.
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
