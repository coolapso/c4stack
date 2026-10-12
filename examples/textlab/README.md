# Textlab

Textlab is an example C4Stack app, kept in this repository as a test fixture. It is a
small paid text API:

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
- **Optional expiry.** With Scaleway email configured, unused credit expires after a
  warning.
- **Replaceable providers.** `build` takes the identity, payment and email providers,
  so the tests pass their own.

```sh
cp .env.example .env      # and fill it in
task db:up db:migrate
task dev                  # http://localhost:8080
```

At the repository root, `task serve` runs the same steps for the maintainer, with
settings from Infisical instead of `.env`.

```sh
curl -X POST http://localhost:8080/api/v1/analyse \
  -H "Authorization: Bearer tl_…" -H "Idempotency-Key: $(uuidgen)" \
  -H "Content-Type: application/json" -d '{"text": "Hello, world"}'
```

Tests: `task test:integration` and `task test:browser`.
