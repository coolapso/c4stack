# C4Stack

C4Stack is a versioned Go module of building blocks for web apps, plus a generator for
new apps. Apps call it; it never owns their engine, routes, layout or configuration.
`docs/architecture.md` explains the design and the reasons behind it.

## Boundaries

- Public packages are the apps' API: `web`, `app`, `app/apptest`, `ui`, `identity`,
  `identity/hanko`, `accounts` and the modules added later. Start new names unexported
  and export them when an app needs them. Keep changes additive after v0.1.0.
- Dependencies point one way: `web` and `internal/postgres` at the bottom; interfaces
  (`identity`, `payments`, `email`) depend on nothing in C4Stack; modules depend on
  `accounts`; nothing depends on a module. Errors live in the package that owns them.
- `internal/scaffold/templates` holds app-owned starter files only. Never put shared
  logic, SQL or assets there.
- `taskfiles/app.yml` is the shared Taskfile apps include remotely. Its tasks run from
  the app root (`dir: '{{.ROOT_DIR}}'`), push nothing on their own, and hold no secrets.
- `~/Dev/searchbasecloud` is a read-only reference. Never build, test, format, migrate or
  change git state there.

## Rules

- C4Stack reads no environment variables and puts its name in nothing users or
  operators see: env vars, cookies, URLs, key prefixes, email subjects.
- The identity provider owns credentials and sessions. Accounts map by provider and
  subject, never by email. Deletion goes to the provider first.
- Browser mutations need the CSRF token and a matching Origin; signed-in routes get both
  from `accounts.RequireSignIn`. API-key routes and webhooks use neither.
- Never log secrets, session tokens, API keys, request or webhook bodies, or panic values.
- Business values (prices, limits, fees, durations) are app configuration.
- Migrations: one namespace per module, files `NNN_name.sql`. Never change a released
  file. Runtime only connects; `migrate` applies schema and needs only the database URL.
- New personal data joins account deletion through `ON DELETE CASCADE` or `OnDelete`.
- Pass `c.Request.Context()` to database and HTTP calls, never the `*gin.Context`: they
  watch the context from other goroutines while middleware replaces `c.Request`.
- Every protective mechanism must name the failure it prevents. Prefer recording rare,
  recoverable cases for an operator over adding machinery.
- Comments and docs describe the present. History belongs in git and release notes.
- SQL in multi-line strings, one clause per line. gofmt; no line-length reflowing.

## Workflow

- `task check`, `task test`, `task test:integration`, `task test:app`, `task cli:build`.
- Integration tests use `//go:build integration` and `internal/testdb`, which creates a
  fresh database on the disposable server `task test:integration` starts. Never point
  tests at a developer's or operator's database.
- Run `go tool templ generate` from the repository root after editing `.templ` files and
  commit the generated code.
- Focused Conventional Commits. Do not tag or push tags; releases are the owner's call.
