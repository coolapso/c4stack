# C4Stack

C4Stack is a Go module of building blocks for small web apps on one European stack: Go,
Gin, Templ, HTMX and PostgreSQL, with Hanko for sign-in, Creem for payments and Scaleway
for email. A generator creates new apps that pin a C4Stack release, so fixes and new
features reach them with one command.

The app owns its `main`, routes, layout and configuration. C4Stack provides the parts
that are hard to get right: sign-in and account deletion, browser and API security,
API keys, a prepaid credit ledger with payment webhooks, migrations, health checks and
graceful shutdown.

## Quickstart

You need Go 1.26 or later, [Task](https://taskfile.dev) 3.53.1 or later, Docker, and a
[Hanko Cloud](https://cloud.hanko.io) project with `http://localhost:8080` as its app URL.

```sh
go install github.com/coolapso/c4stack/cmd/c4stack@latest
c4stack new acme --module example.com/acme
cd acme                   # put your Hanko API URL in .env
task db:up db:migrate
task dev                  # http://localhost:8080
```

While the repository is private, set `GOPRIVATE=github.com/coolapso/c4stack` and make
sure Git reaches GitHub with your credentials; Go and Task both fetch from it.

## Modules

| Package | Purpose |
|---|---|
| `web` | Gin engine with security middleware, CSRF, an extensible CSP, health endpoints and graceful shutdown |
| `app` | The command entry point: `serve`, `migrate` and operator commands |
| `ui` | The default design, layout types and content-hashed assets |
| `identity`, `identity/hanko` | The identity provider interface and the Hanko adapter, with profile management |
| `accounts` | Accounts, sign-in middleware, the settings page and account deletion |
| `apikeys` | Opt-in API keys with bearer authentication and a keys page |
| `payments`, `payments/creem` | The payment provider interface and the Creem adapter |
| `credits` | Opt-in prepaid credit: ledger, holds, purchases, webhooks and history |
| `credits/expiry` | Opt-in expiry of unused credit, after a delivered warning |
| `email`, `email/scaleway` | The email sender interface and the Scaleway adapter |

## Docs

- [Guide](docs/guide.md): recipes for pages, tables, API endpoints, holds, design,
  providers, upgrades and tasks
- [Operations](docs/operations.md): configuration, deployment, health, migrations and
  runbooks
- [Architecture](docs/architecture.md): packages, extension points and design decisions
- [Credits contract](docs/credits.md): ledger rules and webhook outcomes
- [Release notes](docs/releases)
- [Textlab](examples/textlab): an example app that uses every module

## Developing C4Stack

```sh
task check             # vet, lint, formatting, tidiness and generated code
task test              # unit tests
task test:integration  # with a disposable PostgreSQL
task test:app          # generate the starter app and serve it end to end
task test:upgrade      # move a generated app to the next release
task test:example      # the example app, with browser tests
```

`task release:verify VERSION=vX.Y.Z` runs everything a release needs. It neither tags nor
publishes.
