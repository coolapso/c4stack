# C4Stack

C4Stack is a Go module of building blocks for small web apps on one European stack: Go,
Gin, Templ, HTMX and PostgreSQL, with Hanko for sign-in. A generator creates new apps
that pin a C4Stack release, so fixes reach them with `go get` and a rebuild.

The app owns its `main`, routes, layout and configuration. C4Stack provides the parts
that are hard to get right once: sign-in and accounts, account deletion, browser
security, migrations, health checks and graceful shutdown.

## Quickstart

You need Go 1.26 or later, [Task](https://taskfile.dev) 3.53.1 or later, and Docker.

```sh
go install github.com/coolapso/c4stack/cmd/c4stack@latest
c4stack new acme --module example.com/acme
cd acme        # put your Hanko tenant's API URL in .env
task db:up db:migrate
task dev       # http://localhost:8080
```

While the repository is private, set `GOPRIVATE=github.com/coolapso/c4stack` and make
sure Git can reach GitHub with your credentials.

## Modules

| Package | Purpose |
|---|---|
| `web` | Gin engine with security middleware, CSRF, extensible CSP, health and graceful serving |
| `app` | The command entry point: `serve`, `migrate` and operator commands |
| `ui` | Default design, layout types and content-hashed assets |
| `identity`, `identity/hanko` | Identity provider interface and the Hanko adapter |
| `accounts` | Accounts, sign-in middleware, settings and account deletion |

## Docs

- [Architecture](docs/architecture.md)
