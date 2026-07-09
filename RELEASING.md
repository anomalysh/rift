# Releasing rift

Releases are semver, driven by [release-please](https://github.com/googleapis/release-please)
from Conventional Commit messages. You do not tag by hand.

## How it works

1. You merge normal commits to `master` using Conventional Commit subjects:
   `feat:`, `fix:`, `refactor:`, `docs:`, `perf:`, `chore:`, `test:`, `ci:`.
   A `!` or a `BREAKING CHANGE:` footer marks a breaking change.

2. The **release-please** workflow watches `master` and, whenever there are
   releasable commits, opens (or updates) a single **release PR** titled like
   `chore(main): release 0.2.0`. That PR:
   - computes the next version from the commits since the last release
     (`fix:` → patch, `feat:` → minor, breaking → minor while below 1.0),
   - bumps the version in `cli/package.json` and `docs-site/package.json`,
   - writes the changelog into `CHANGELOG.md`.

3. When you're ready to ship, **merge the release PR**. release-please then
   creates the `vX.Y.Z` git tag and the GitHub release.

4. That tag fires the existing workflows:
   - **release.yml** cross-compiles the CLI for every target and uploads the
     binaries + `SHA256SUMS` to the release,
   - **publish.yml** builds and pushes the container images tagged `X.Y.Z`,
     `X.Y`, and `latest` to `ghcr.io/anomalysh`.

The version in `cli/package.json` stays the single source of truth —
`tools/release.sh` reads it, and release-please keeps it in step with the tag.

## One-time setup: the release token

A tag pushed by the default `GITHUB_TOKEN` does **not** start other workflows
(GitHub blocks that to prevent runaway loops). So for step 4 to happen
automatically, release-please needs a token that can trigger them:

```sh
# a fine-grained PAT with Contents: read/write on anomalysh/rift
# (or a classic PAT with the `repo` scope)
gh secret set RELEASE_PLEASE_TOKEN --body <your-token>
```

With that secret set, cutting a release is fully automatic: merge the PR, and
the binaries and images build themselves.

**Without the secret**, release-please still does everything except trigger the
downstream builds. After merging the release PR, run them by hand:

```sh
gh workflow run release.yml --ref vX.Y.Z
gh workflow run publish.yml --ref vX.Y.Z
```

## Cutting the first release

The repo starts at `0.1.0`. Once there is at least one `feat:`/`fix:` commit on
`master`, release-please opens the first release PR. Merge it to publish
`v0.1.0` (or higher, depending on the commits). Until a release exists, the
install script reports "no published release found" — expected, not an error.

## Forcing a specific version

Add a `Release-As: 1.0.0` footer to a commit to override the computed version,
e.g. to cut the first stable release:

```
chore: release 1.0.0

Release-As: 1.0.0
```
