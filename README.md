# C4Stack

C4Stack is a Go module of building blocks for small web apps on one European stack: Go,
Gin, Templ, HTMX and PostgreSQL, with Hanko for sign-in, Creem for payments and Scaleway
for email. A generator creates new apps that pin a C4Stack release, so fixes and new
features reach them with one command.

The app owns its `main`, routes, layout and configuration. C4Stack provides the parts
that are hard to get right: sign-in and account deletion, browser and API security,
API keys, a prepaid credit ledger with payment webhooks, migrations, health checks and
graceful shutdown.

C4Stack is an independent open-source project by [@coolapso](https://github.com/coolapso).
[Coolapso](https://coolapso.sh), the company behind Searchbase Cloud, builds its products
on it. C4Stack grew out of [Searchbase Cloud](https://searchbase.md), and its tests prove
that Searchbase's database moves onto it in one release, without moving or copying data.

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

## Modules

| Package | Purpose |
|---|---|
| `web` | Gin engine with security middleware, CSRF, an extensible CSP, health endpoints and graceful shutdown |
| `app` | The command entry point: `serve`, `migrate` and operator commands |
| `ui` | The default design, layout types and content-hashed assets |
| `identity`, `identity/hanko` | The identity provider interface and the Hanko adapter, with profile management |
| `accounts` | Accounts linked by verified email, sign-in middleware, settings and account deletion |
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
- [Releases](https://github.com/coolapso/c4stack/releases), with notes generated from the
  commits
- [Textlab](examples/textlab): an example app that uses every module

## Status

C4Stack is young, and opinionated: it is the stack Coolapso builds its own products on.
From v0.1.0, the public packages change only by addition and released migrations never
change. Each release's notes list its behaviour changes and migrations.
[Not in scope](docs/architecture.md#not-in-scope) lists what C4Stack leaves out on
purpose.

## Contributing and security

Bug reports and fixes are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) explains how,
including the Contributor License Agreement. Report vulnerabilities privately, as
[SECURITY.md](SECURITY.md) describes.

Want help building on C4Stack? Coolapso offers consulting: [coolapso.sh](https://coolapso.sh).

## Developing C4Stack

```sh
task check             # vet, lint, formatting, tidiness and generated code
task test              # unit tests
task test:integration  # with a disposable PostgreSQL
task test:app          # generate the starter app and serve it end to end
task test:upgrade      # move a generated app to the next release
task test:example      # the example app, with browser tests
task serve             # run the example app on http://localhost:8082
```

`task serve` starts the example's database, migrates it, and serves with live reload.
When the server stops, it stops the database too, keeping its data. It fetches the Creem
test keys from the maintainer's Infisical project, so it needs the `infisical` CLI,
logged in, with access to that project. Without it, run the example with its own `.env`,
as [its README](examples/textlab/README.md) describes.

Releases are the owner's: `task release:full` runs every check above, then semrel tags
the next version and publishes its GitHub release. [Maintaining](docs/maintaining.md)
covers repository settings, security reports and releases.

## License

C4Stack is licensed under the [Apache License 2.0](LICENSE), copyright 2026 Carlos Colaço (coolapso).
Bundled third-party files keep their own licenses; [NOTICE](NOTICE) lists them.

The starter files that `c4stack new` writes into your app are yours: use, change and
license them as you like, without attribution.
