# Textlab

Textlab is an example C4Stack app, kept in this repository as a test fixture. It is a
small paid text API:

- **Sign-in of its own.** People sign up and sign in with a code by email or a passkey
  (`identity/local`). In development, the codes arrive in Mailpit.
- **Names.** Accounts have names (`accounts.Config.Names`): a new account gives one
  before its first page, and changes it on the settings page.
- **Custom layout.** `layout.templ` and `static/console.css` replace the default design
  with a sidebar console. The default settings, API keys and credits pages render
  inside it.
- **API keys.** Keys start with `tl_`. The API under `/api/v1` authenticates them.
- **Credits.** Users buy credit through Creem. `POST /api/v1/analyse` debits 2 credits,
  once per `Idempotency-Key`.
- **A subscription.** Textlab Pro, €5 a month, halves the price of an analysis while
  it's in effect (`subscriptions`): renewing, or canceled until its period ends. It's on sale when `TEXTLAB_CREEM_PRO_PRODUCT_ID`
  names a recurring Creem product; the settings page lists it, cancels it at the
  period's end, and opens Creem's billing page. Coming back from Creem's checkout
  starts it at once; renewals and cancellations arrive at `/webhooks/subscriptions`,
  which Creem reaches locally through `task tunnel` at the repository root.
- **Holds.** `POST /api/v1/jobs` reserves a job's cost; `POST /api/v1/jobs/{id}/complete`
  charges it and releases the hold in one transaction. Pending jobs are the holds that
  `heldByJobs` reports.
- **An app table.** `jobs` references accounts with `ON DELETE CASCADE`, so deleting an
  account deletes its jobs.
- **Optional expiry.** With Scaleway email, which reports delivery, unused credit
  expires after a warning. With SMTP, as in development, it never expires.
- **An OpenID Connect provider.** Another service can sign people in with Textlab's
  accounts (`oidc`), with their names under the profile scope, as a client set by
  `TEXTLAB_OIDC_CLIENT_ID`,
  `TEXTLAB_OIDC_CLIENT_SECRET` and `TEXTLAB_OIDC_REDIRECT_URIS` (comma-separated). The
  issuer is `TEXTLAB_PUBLIC_URL`. Without a client, the provider has none.
- **Replaceable providers.** `build` takes the identity, payment and email providers,
  so the tests pass their own.

`Taskfile.yml` holds the development settings, with a new secret key each time a task
starts. It names the products of the maintainer's Creem test store, and its tasks fetch
the store's test key and webhook secret from the maintainer's Infisical project, with
the `infisical` CLI:

```sh
task db:up mail:up db:migrate
task dev                  # http://localhost:8080; codes arrive at http://localhost:8025
```

Inside tmux, `task dev:tmux` does both in a window of its own, with the database's and
Mailpit's logs. At the repository root, `task serve` and `task dev:tmux` serve Textlab on
http://localhost:8082 instead, and stop the database and Mailpit when it stops.

Without access to that Infisical project, use a Creem store of your own in test mode,
with a one-time product (checkouts set its price) and, for Textlab Pro, a recurring
one. Give their settings in your shell. With `TEXTLAB_CREEM_API_KEY` there, the tasks
leave Infisical out:

```sh
export TEXTLAB_CREEM_API_KEY=creem_test_… TEXTLAB_CREEM_WEBHOOK_SECRET=…
export TEXTLAB_CREEM_PRODUCT_ID=prod_… TEXTLAB_CREEM_PRO_PRODUCT_ID=prod_…
task dev
```

```sh
curl -X POST http://localhost:8080/api/v1/analyse \
  -H "Authorization: Bearer tl_…" -H "Idempotency-Key: $(uuidgen)" \
  -H "Content-Type: application/json" -d '{"text": "Hello, world"}'
```

Tests: `task test:integration` and `task test:browser`.
