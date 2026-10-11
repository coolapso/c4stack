# Operations

How to run a C4Stack app in production. The examples use an app named `acme` with the
environment prefix `ACME_`.

## Providers to set up

| Provider | Needed for | Setup |
|---|---|---|
| Hanko | every app that signs users in | A tenant with the app's URL. Allow account deletion, so deletion can go through the app. Require email verification: accounts are linked by verified address. The `hanko` cookie must reach the app's host. |
| Creem | buying credit | A one-time product with a custom price, and a webhook to `https://<host>/webhooks/payments` (or wherever the app mounts `Webhook`). Use a `creem_test_` key until you go live. |
| Scaleway Transactional Email | credit expiry | A project with a verified sending domain and a key allowed to send. |
| PostgreSQL | every app | PostgreSQL 13 or later; C4Stack is tested with 18. |

Infisical, or any other secret store, injects the configuration as environment
variables. C4Stack reads none itself; the app's `config.go` does.

## Configuration

Every app needs these, plus what its modules need:

| Variable | Meaning |
|---|---|
| `ACME_PUBLIC_URL` | The origin users reach, such as `https://acme.example`. Requests for other hosts get 421. |
| `ACME_DATABASE_URL` | PostgreSQL URL. The only setting the migrate command reads. |
| `ACME_SECRET_KEY` | At least 32 bytes, the same on every replica. Signs CSRF tokens and seals cookies. |
| `ACME_HANKO_API_URL` | The Hanko tenant's API URL. |
| `ACME_ADDR` | Listen address, `:8080` by default. |

**Rotating the secret key.** Open forms and email changes in progress fail once and work
after a reload. Sign-in is unaffected, because Hanko owns sessions.

## Database

The database URL follows PostgreSQL's format. For any host that is not local, C4Stack
requires verified TLS (`sslmode=verify-full`, with the system's trusted roots) unless the
URL sets `sslmode`. If your provider uses its own certificate authority, add
`sslrootcert=/path/to/ca.pem`. An explicit weaker mode, such as `sslmode=require`, is
allowed and logged as a warning at startup.

The server's database role needs to read and write the app's tables. Only the migrate
job needs to create and alter them, so the two can use different roles.

## Build

```sh
task container:push REGISTRY=ghcr.io/you
```

This compiles the binaries on the build machine for `PLATFORMS`, copies them into a
distroless image, and pushes it tagged with `git describe` and `latest`. Building on the
host keeps private-module credentials out of image builds.

## Deploy

One image runs as two things:
1. **A migrate job.** Run `acme migrate` once per release, before the rollout. It applies
   pending migrations and needs only `ACME_DATABASE_URL`.
2. **The server.** Run `acme serve` with the full configuration.

```yaml
# Kubernetes, in outline
containers:
  - name: acme
    image: ghcr.io/you/acme:1.4.0
    args: [serve]
    readinessProbe:
      httpGet: {path: /health/ready, port: 8080}
    livenessProbe:
      httpGet: {path: /health/live, port: 8080}
terminationGracePeriodSeconds: 45
```

Health endpoints answer on any host name, so probes work with pod IPs.

### Readiness

`/health/ready` answers 204 when the server can take traffic. It answers 503 when:
- the server is shutting down
- PostgreSQL does not answer
- a migration this binary needs is missing

So a server started before its migrate job finished stays out of rotation until the
migrations are applied. A database with migrations newer than the binary is fine.

### Shutdown

On SIGTERM, the server:
1. Fails readiness at once, and keeps accepting requests for 5 seconds, so the load
   balancer stops sending traffic.
2. Gives in-flight requests 30 seconds to finish.
3. Closes the remaining connections.

Set `terminationGracePeriodSeconds` above 35. Apps can change both durations in
`web.Server`.

## Migrations and rolling upgrades

Each module has its own migration history, and the app has one more, all recorded in
`schema_migrations`. `acme migrate status` shows them.

During a rolling upgrade, old and new servers run against the new schema at the same
time. Keep every migration additive: add tables and nullable columns first, deploy code
that uses them, and remove old ones in a later release.

**Rolling back** a deployment does not roll back the schema. The older release tolerates
the newer migrations, and running its migrate job does nothing.

**Moving an existing app onto C4Stack** is a release like any other when its tables
already have the modules' shape, as the guide's "Adopt an existing database" describes.
The first migrate job converts the migration history and adds what the modules need,
without moving data, and the previous release keeps running against the result during
the switch. Back up the database before that first run anyway. Once the history has
namespaces, the previous release's own migrate job no longer understands it, so roll
back the image only, not to the old migrate job.

**Published migrations never change.** `migrate` refuses a file whose checksum differs
from the one recorded.

## Logs

Logs are JSON on standard error unless the app chooses text. Each request logs its
method, route template, status and duration, with a request ID that is also sent in
the `X-Request-ID` response header. Incoming request IDs are ignored.

Logs never contain query strings, request or webhook bodies, headers, cookies, API keys,
secrets, panic values, or buyer names and addresses. Errors from providers carry status
codes only.

## Runbooks

### Account deletion did not finish

A user deletes their account in two steps: the identity at Hanko first, then the local
data. If the second step fails, the log has:

```
the identity was deleted but local cleanup failed; run accounts delete-local  account_id=…
```

The user can no longer sign in. Finish the deletion:

```sh
acme accounts delete-local <account-id>
```

It runs every module's and the app's deletion hooks, forfeits remaining credit and deletes
the account, in one transaction. If the user signs up again with the same address before
you do, the sign-up finishes the deletion itself and starts a new, empty account; the
command then reports that the account no longer exists.

Every deletion goes through the app, so its data is removed with it.

### Payment reviews

Payment events that cannot be applied as they stand are recorded instead of guessed.
The log has `payment needs review` with a reason. Check them regularly:

```sh
acme credits reviews
acme credits adjust <account-id> <cents> <reference> <what and why>
acme credits resolve <review-id> <what you did>
```

`docs/credits.md` explains each reason and the usual fix.

### Payment webhooks were down

Creem retries a failed webhook after 30 seconds, 5 minutes, 30 minutes and 6 hours. If
the app or its database was unavailable for longer, compare the payments in the Creem
dashboard with the purchases in `payment_intents`. Credit the missed ones with
`credits adjust`, using the purchase ID as part of the reference.

### Credit expiry

With the expiry module, the server sends due warnings and expires due credit every
hour. One replica runs it at a time. To run it now:

```sh
acme credits expire
```

A warning that cannot be delivered is tried again on the next run. Credit only expires
after its owner's warning was delivered and the full notice period has passed. Without a
working email sender, nothing expires.

To shorten the notice period, wait until no warnings are outstanding: the new period
also applies to warnings already sent.
