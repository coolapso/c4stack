# Contributing to C4Stack

Bug reports, fixes and documentation improvements are welcome.

C4Stack is opinionated: it is the stack [Coolapso](https://coolapso.sh) builds its own
products on. Open an issue before you start a new feature or a larger change, and expect
some ideas to be declined because they don't fit the stack.
[Not in scope](docs/architecture.md#not-in-scope) lists what it leaves out on purpose.

Report security issues privately, as [`SECURITY.md`](SECURITY.md) describes, never in a
public issue.

## Before opening a pull request

Read [`AGENTS.md`](AGENTS.md). It holds the boundaries, rules and workflow every change
follows, for people and coding agents alike.

You need Go 1.26 or later, [Task](https://taskfile.dev) 3.53.1 or later, Docker with
Compose, and [golangci-lint](https://golangci-lint.run) v2. Then run:

```sh
task check             # vet, lint, formatting, tidiness and generated code
task test              # unit tests
task test:integration  # with a disposable PostgreSQL
```

A change to the generator, the shared Taskfile or the example also needs
`task test:app`, `task test:upgrade` and `task test:example`. The last one drives the
example in headless Chromium.

When an API changes, update `docs/guide.md`, `docs/architecture.md` and the example app
with it. Never change a migration file that has been released.

Use focused [Conventional Commits](https://www.conventionalcommits.org). They decide
releases, and their subjects become the release notes: `feat` makes a minor release,
`fix`, `ref` and `build` a patch release. Name any behaviour change or new migration in
the subject, and mark a breaking change with `!` or a `BREAKING CHANGE:` footer.

## Contributor License Agreement

All contributors must accept the C4Stack [Contributor License Agreement](CLA.md) before a
pull request can be merged.

The CLA Assistant workflow checks whether every pull request author has signed. If a
signature is missing, the bot comments with signing instructions. Sign by commenting
exactly:

```text
I have read the CLA Document and I hereby sign the CLA
```

Accepted signatures are stored in this repository on the `cla-signatures` branch under
`.github/cla/signatures/v1/cla.json`.
