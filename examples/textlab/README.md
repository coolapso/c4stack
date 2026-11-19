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
starts. Buying credit needs a Creem test-mode key, webhook secret and one-time product
with a custom price; give them in your shell, or from Infisical with `INFISICAL_PROJECT`:

```sh
export TEXTLAB_CREEM_API_KEY=creem_test_… TEXTLAB_CREEM_WEBHOOK_SECRET=… TEXTLAB_CREEM_PRODUCT_ID=…
task db:up mail:up db:migrate
task dev                  # http://localhost:8080; codes arrive at http://localhost:8025
```

At the repository root, `task serve` runs the same steps for the maintainer, with the
Creem keys from Infisical.

```sh
curl -X POST http://localhost:8080/api/v1/analyse \
  -H "Authorization: Bearer tl_…" -H "Idempotency-Key: $(uuidgen)" \
  -H "Content-Type: application/json" -d '{"text": "Hello, world"}'
```

Tests: `task test:integration` and `task test:browser`.
