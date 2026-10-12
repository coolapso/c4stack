# Security policy

C4Stack handles sign-in, account deletion, API keys and money for the apps built on it,
so security reports matter here. Thank you for taking the time to send one.

## Reporting a vulnerability

Report it privately. Please don't open a public issue, discussion or pull request.

- **Preferred:** [report it on GitHub](https://github.com/coolapso/c4stack/security/advisories/new),
  from the repository's Security tab. Only you and the maintainers can see the report.
- **Or email** [thehq@coolapso.sh](mailto:thehq@coolapso.sh) with "C4Stack security" in
  the subject.

Include what you can of:
- the affected package, version or commit
- what an attacker can do, and what they need to start with: an account, an API key,
  a position on the network
- steps or a test that reproduces it, against an app generated with `c4stack new` or
  the example in `examples/textlab`

## What happens next

- You get an acknowledgement within 3 working days.
- Within 10 working days you hear whether the report is accepted, and its severity.
- The fix is prepared in a private advisory, which you are invited to join. It ships in
  a release, with an upgrade note in the release notes.
- Then the advisory is published, with a CVE when one is warranted. It credits you,
  unless you would rather not be named.

Please keep the issue private until the fix is released, or for 90 days from your
report, whichever comes first. If the fix needs longer, we'll agree a date with you.

There is no bug bounty.

## Supported versions

Fixes go into the latest release. Apps pin a release, so upgrading is how they receive a
fix: `task framework:update VERSION=…` (see [Upgrade the
framework](docs/guide.md#upgrade-the-framework)).

## Scope

In scope is everything in this repository:
- the modules: the web security middleware (host and origin checks, CSRF, security
  headers, the Content Security Policy, cookies), sign-in and account linking, account
  deletion, API keys, the credit ledger, payment webhooks, credit expiry and email
- migrations and the migration runner
- the generator, the starter files it writes and the shared Taskfile apps include,
  since every new app starts from them

Out of scope:
- apps built with C4Stack. Report those to whoever runs the app. If the cause is in
  C4Stack, a report here is welcome too.
- vulnerabilities in Hanko, Creem, Scaleway or a dependency. Report them to the vendor
  or upstream project, and tell us if C4Stack has to change or update something.
- findings that need control of the server, the database or the provider accounts
  first
- missing hardening without a demonstrated impact, and denial of service by volume.
  Rate limiting belongs to the ingress in front of an app.

## Testing safely

Test against your own deployment: an app generated with `c4stack new`, or the example.
Never test against an app you don't run, such as Searchbase Cloud, and never use real
people's data. Research that stays within these lines, and reports made under this
policy, are welcome. Coolapso AB will not pursue legal action over them.

## How C4Stack protects apps

[`docs/architecture.md`](docs/architecture.md) describes the protections and the reasons
for them, module by module. [`AGENTS.md`](AGENTS.md) lists the rules every change keeps,
such as never logging secrets, keys or request bodies.
