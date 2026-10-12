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

Releases work like Searchbase's, with [semrel](https://semrel.io) in place of
go-semantic-release. semrel picks each version from the Conventional Commits since the
last release, as `.semrel.yaml` configures: `feat` makes a minor release; `fix`, `ref`
and `build` make a patch release. Versions stay below 1.0.0, so a breaking change makes
a minor release too. The release notes are generated from the commit subjects.

- `task release:next` shows the next version and its notes.
- `task release:full` runs every check, then `task release:gh`. The Release workflow
  (Actions → Release → Run workflow) runs it on GitHub's runner.
- `task release:gh`:
  1. Pulls and pushes the current branch.
  2. Uses `GITHUB_TOKEN`, or `gh`'s token when it is not set.
  3. Restores the GitHub plugin pinned in `.semrel.lock`.
  4. Runs semrel, whose plugin publishes the GitHub release; GitHub creates the tag on
     the pushed commit.

To move to 1.0.0, raise `version_ceiling` in `.semrel.yaml`.
