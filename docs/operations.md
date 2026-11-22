# Operations

How to run a C4Stack app in production. The examples use an app named `acme` with the
environment prefix `ACME_`.

## Providers to set up

| Provider | Needed for | Setup |
|---|---|---|
| An email sender | every app on C4Stack's own sign-in (`identity/local`) | Scaleway Transactional Email, or an SMTP server that offers STARTTLS. Sign-in depends on its codes arriving: each sending domain needs SPF, DKIM and DMARC. Security notices ask people to reply, so replies to the sending address (or its `ReplyTo`) must reach a person. |
| Hanko | every app on Hanko's sign-in | A tenant with the app's URL. Allow account deletion, so deletion can go through the app. Require email verification: accounts are linked by verified address. The `hanko` cookie must reach the app's host. |
| Creem | buying credit | A one-time product with a custom price, and a webhook to `https://<host>/webhooks/payments` (or wherever the app mounts `Webhook`). Use a `creem_test_` key until you go live. |
| Creem | subscriptions | A recurring product for each thing the app sells, and a webhook to `https://<host>/webhooks/subscriptions` (or wherever the app mounts the module's `Webhook`). The customer portal cancels at once unless the store's Settings > Billing > Subscription cancellation is "At period end"; the settings page always cancels at the period's end. |
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
| `ACME_HANKO_API_URL` | On Hanko: the tenant's API URL. |
| `ACME_ADDR` | Listen address, `:8080` by default. |
| `ACME_TRUSTED_PROXIES` | Behind a proxy: its addresses or CIDRs, comma-separated. See below. |

A generated app on C4Stack's own sign-in also needs its email settings:

| Variable | Meaning |
|---|---|
| `ACME_EMAIL_FROM` | The address sign-in codes and notices come from, on a domain with SPF, DKIM and DMARC. |
| `ACME_EMAIL_REPLY_TO` | Optional: where replies go. Notices ask people to reply, so a person must read them. |
| `ACME_SCALEWAY_SECRET_KEY`, `ACME_SCALEWAY_PROJECT_ID` | Scaleway Transactional Email. |
| `ACME_SMTP_URL` | Instead of Scaleway: `smtp://user:password@host:port`, with STARTTLS. |

**Rotating the secret key.** Open forms and email changes in progress fail once and work
after a reload. On Hanko, sign-in is unaffected, because Hanko owns sessions. On the
app's own sign-in, sessions stay too, but:
- codes then waiting fail, and people ask for a new one;
- the limits' counters start again from zero;
- code attempts and passkey ceremonies in progress start again;
- browsers that signed in before fall back to the shared counters until they sign in
  again.

During a rolling rotation, replicas hold different keys: a code made on one may fail on
another, and the limits are briefly doubled. Rotate on every replica together.

With the OpenID Connect provider (`oidc`), sign-ins in progress at a client fail once
and work when started again, and a new signing key starts at once. Clients that cached
the key set fetch it again when they meet the new key.

**The issuer.** The OpenID Connect provider's issuer is the app's public URL, and clients
key their users by it. Changing the public URL once clients sign people in makes every
account new to them: keep it.

**Trusted proxies.** The app's own sign-in limits codes per network, from the client IP
that `web.Config.TrustedProxies` makes; a generated app reads it from
`ACME_TRUSTED_PROXIES`, comma-separated addresses or CIDRs, and trusts none without it.
Behind an ingress or load balancer, set it to exactly the proxies in front of the app:
- trusting too few makes every request seem to come from the proxy, so one person's
  limit stops everyone;
- trusting forwarding headers from anyone lets an attacker choose the network they seem
  to come from.

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

The OpenID Connect provider's library logs the token and userinfo requests it refuses
at warning level, through slog's default logger and without a request ID: the error
and its description, never codes, tokens or secrets.

## Runbooks

### Account deletion did not finish

On the app's own sign-in, deletion is one transaction: it finishes or nothing changes,
and the user can try again. On Hanko, a user deletes their account in two steps: the
identity at Hanko first, then the local data. If the second step fails, the log has:

```
the identity was deleted but local cleanup failed; run accounts delete-local  account_id=…
```

The user can no longer sign in. Finish the deletion:

```sh
acme accounts delete-local <account-id>
```

It runs every module's and the app's deletion hooks, forfeits remaining credit and deletes
the account, in one transaction. If the user signs up again with the same address before
you do, the sign-up finishes the deletion itself, once the provider's confirmation was
recorded, and starts a new, empty account; the command then reports that the account no
longer exists. If even the confirmation could not be recorded, a sign-up with the
address is turned away until you run the command.

Every deletion goes through the app, so its data is removed with it.

### Someone can't sign in

On the app's own sign-in, codes are sent by email. The log never has addresses, so ask
for the user's ID (every security notice quotes it) or look them up by address in the
commands below, which take either.

- **Codes don't arrive.** Check the email provider and the sending domain's SPF, DKIM and
  DMARC. A browser the person signed in on before, or a passkey, still works.
- **"Too many attempts."** Codes were asked for, or wrong ones entered, too often for
  the address, and its shared limits stopped codes until the window ends. When it was
  wrong codes, the user got a notice. Browsers that signed in before and passkeys still work. To let codes work
  again at once:

  ```sh
  acme local unblock ann@example.com
  ```

  This helps once. Against someone who keeps going, the person signs in on a browser
  they used before, or with a passkey.
- **A lost or stolen device.** End every session of the user, and make every browser
  they signed in on before count as unknown again:

  ```sh
  acme local sign-out <user-id or address>
  ```

### Support recovery: a lost mailbox or a hijacked address

When someone lost their mailbox and has no passkey, or an attacker changed their
address, support gives the account a new address:

```sh
acme local set-email <user-id or old address> <new address>
```

In one transaction it changes the user's and the account's address, ends every session,
removes every passkey and waiting code, and queues a notice to both addresses; the
command says whether the email provider accepted them. The person then signs in with a
code to the new address and adds their passkeys again.

`set-email` trusts you. Check who is asking before you run it. None of these is enough
on its own:
- knowing the account's details;
- controlling the new address;
- a request sent from the new address.

The app's support policy says what is enough, such as payment records, or earlier mail
from the old address. Someone whose address was changed behind their back can quote the
reference from the notice: the user's ID.

### Payment reviews

Payment events that cannot be applied as they stand are recorded instead of guessed.
The log has `payment needs review` with a reason. Check them regularly:

```sh
acme credits reviews
acme credits adjust <account-id> <cents> <reference> <what and why>
acme credits resolve <review-id> <what you did>
```

`docs/credits.md` explains each reason and the usual fix.

### Subscription reviews

Subscription events that cannot be applied as they stand are recorded instead of
guessed. The log has `subscription event needs review` with a reason;
`docs/architecture.md` ("subscriptions") lists them. Check them regularly:

```sh
acme subscriptions reviews
acme subscriptions list <account-id>
acme subscriptions sync <subscription-id>   # read its state from the provider
acme subscriptions resolve <review-id> <what you did>
```

A `second_in_group` means the account pays twice for one plan: cancel or refund one in
the provider's dashboard. A `deleted_account` means a deleted account's subscription
was paid, such as a checkout finished after the deletion: the module canceled it at
once (the log says if that failed); refund as the policy says. `product_change` and
`too_many_units` come from changes made in the provider's dashboard that the app's
products don't allow: undo them there, or `sync` once they fit. A `refunded` is a
refund the module didn't cause: a change's own refund, of the time it left unused,
isn't a review.

Changes that wait for the end of a period (`ChangeAtPeriodEnd`) are made by the
server within the last hour of the period, so the server must run then. `subscriptions
list` shows a waiting change under THEN. The log says `made a subscription change that
waited for the end of its period`, or why it failed (it is tried again every five
minutes) or was dropped. If it says the subscription renewed first, the change went
through and refunded the new period's unused time: check the refund in the provider's
dashboard.

### Payment webhooks were down

Creem retries a failed webhook after 30 seconds, 5 minutes, 30 minutes and 6 hours. If
the app or its database was unavailable for longer, compare the payments in the Creem
dashboard with the purchases in `payment_intents`. Credit the missed ones with
`credits adjust`, using the purchase ID as part of the reference. For subscriptions,
`subscriptions sync <subscription-id>` reads each one's state from Creem.

### Credit expiry

With the expiry module, the server sends due warnings and expires due credit every
hour. One replica runs it at a time. To run it now:

```sh
acme credits expire
```

Each run asks Scaleway what became of the warnings it has not seen delivered. A warning
counts once Scaleway reports it `sent`, meaning the recipient's mail system took it.
One that fails, or is still undelivered after three days, is sent again. Credit only
expires after its owner's warning was delivered and the full notice period from
delivery has passed, and never before the date the warning announced. Without a working
email sender, nothing expires.

Each run that does something logs its counts. Apps that record metrics get every run's
outcome through the module's `OnRun`, so alert on runs that fail or stop succeeding.

Changing the policy applies to new warnings; deadlines already announced stay.
