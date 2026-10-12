# Maintaining C4Stack

## Repository settings

The repository is public. These GitHub settings keep it safe; each names the failure it
prevents.

- **Code security:**
  - private vulnerability reporting, so the link in `SECURITY.md` works
  - secret scanning with push protection, so a credential is blocked before it is
    pushed
  - Dependabot alerts, so a vulnerable dependency gets noticed
- **A ruleset on `main`** that blocks force pushes and deletion, so published history
  stays as published.
- **A ruleset on `v*` tags** that blocks updates and deletion. Apps pin releases, and the
  Go checksum database records each version, so a moved tag breaks every app that
  downloads it afterwards.

The CLA workflow creates the `cla-signatures` branch on the first pull request. Leave it
out of the `main` ruleset's targets.

## Security reports

Reports arrive as private advisories, or by email to security@coolapso.sh. `SECURITY.md`
promises the reporter:
1. An acknowledgement within 3 working days.
2. A decision, with severity, within 10 working days.
3. A fix prepared in the advisory's private fork, released with an upgrade note in the
   release notes.
4. The advisory published after the release, with a CVE requested from GitHub when
   warranted, crediting the reporter unless they decline.

## Releases

1. Write the release notes in `docs/releases/vX.Y.Z.md`, with behaviour changes and
   migrations.
2. Run `task release:verify VERSION=vX.Y.Z`.
3. The owner tags and pushes the release.
