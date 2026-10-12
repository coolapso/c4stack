# C4Stack

C4Stack is a versioned Go module of building blocks for web apps, plus a generator for
new apps. Apps call it; it never owns their engine, routes, layout or configuration.
`docs/architecture.md` explains the design and the reasons behind it.

## Boundaries

- Public packages are the apps' API: `web`, `app`, `app/apptest`, `ui`, `identity`,
  `identity/hanko`, `accounts`, `apikeys`, `payments`, `payments/creem`, `credits`,
  `credits/expiry`, `email` and `email/scaleway`. Start new names unexported and export
  them when an app needs them. Keep changes additive after v0.1.0.
- Dependencies point one way: `web` and `internal/postgres` at the bottom; interfaces
  (`identity`, `payments`, `email`) depend on nothing in C4Stack; modules depend on
  `accounts`; nothing depends on a module. Errors live in the package that owns them.
- New capabilities, such as subscriptions, are new modules or optional interfaces,
  never new required methods on an existing interface. Provider-specific payloads stay
  in adapters.
- `internal/scaffold/templates` holds app-owned starter files only. Never put shared
  logic, SQL or assets there.
- `taskfiles/app.yml` is the shared Taskfile apps include remotely. Its tasks run from
  the app root (`dir: '{{.ROOT_DIR}}'`), push nothing on their own, and hold no secrets.
  Its own variables start with `SHARED_`, because Task merges them over the app's.
- Searchbase Cloud is the reference app, and the modules' compatibility target. It is
  private; the owner's checkout is `~/Dev/searchbasecloud`.
  Module tables, paths and behaviour are Searchbase's unless a rule here says otherwise;
  `internal/porttest` must keep proving that Searchbase moves onto C4Stack with one
  release. Searchbase is read-only: never build, test, format, migrate or change git
  state there.

## Rules

- C4Stack reads no environment variables and puts its name in nothing users or
  operators see: env vars, cookies, URLs, key prefixes, email subjects.
- The identity provider owns credentials and sessions. A sign-in finds its account by
  provider and subject; a new identity joins the account with its verified email, from
  any provider. Unverified identities get no account. Deletion goes to the provider
  first.
- Browser mutations need the CSRF token and a matching Origin; signed-in routes get both
  from `accounts.RequireSignIn`. API-key routes and webhooks use neither.
- Never log secrets, session tokens, API keys, request or webhook bodies, buyer details,
  or panic values.
- Money is integer EUR cents. Every credit mutation locks the account row and is
  idempotent per account and reference; the ledger is append-only. Webhooks grant what
  the stored purchase says, and send anything ambiguous to review.
- Business values (prices, limits, fees, durations) are app configuration.
- Migrations: one namespace per module, files `NNN_name.sql`, every statement idempotent
  (`IF NOT EXISTS`) so modules adopt existing tables. Never change a released file;
  before v0.1.0, each module keeps one initial migration. Runtime only connects;
  `migrate` applies schema and needs only the database URL.
- New personal data joins account deletion through `ON DELETE CASCADE` or `OnDelete`.
- Pass `c.Request.Context()` to database and HTTP calls, never the `*gin.Context`: they
  watch the context from other goroutines while middleware replaces `c.Request`.
- Real providers only in production code. Test substitutes live in `_test.go` files.
- Every protective mechanism must name the failure it prevents. Prefer recording rare,
  recoverable cases for an operator over adding machinery.
- Comments and docs describe the present. History belongs in git and release notes.
- SQL in multi-line strings, one clause per line. gofmt; no line-length reflowing.

## Open source

- The repository is public under Apache-2.0. C4Stack is owned personally by its author,
  coolapso (Carlos Colaço); Coolapso AB uses it under that license, as it does
  Searchbase. Contributions come in under `CLA.md`, which grants to the owner. Never commit secrets, real tenant, project or account IDs,
  customer data or internal hostnames. Tests and docs use example domains.
- A bundled third-party file keeps its license file next to it, an entry in `NOTICE`,
  and a `linguist-vendored` line in `.gitattributes`.
- Workflows run on GitHub-hosted runners, never self-hosted ones, and hold no secrets.
  Never check out pull request code in a `pull_request_target` workflow.
- Security reports follow `SECURITY.md`; `docs/maintaining.md` has the repository
  settings it relies on.

## Workflow

- `task check`, `task test`, `task test:integration`, `task test:app`,
  `task test:upgrade`, `task test:example`, `task cli:build`.
- Integration tests use `//go:build integration` and `internal/testdb`, which creates a
  fresh database on the disposable server `task test:integration` starts. Never point
  tests at a developer's or operator's database, and never start or stop their services.
- Run `task templ:generate` after editing `.templ` files and commit the generated code.
  It leaves the example module alone; the example runs its own.
- When an API changes, update `docs/guide.md`, `docs/architecture.md` and the example app
  with it. Behaviour changes and new migrations go in the next release's notes.
- `task release:verify VERSION=vX.Y.Z` before a release. Focused Conventional Commits.
  Do not tag or push tags; releases are the owner's call.
