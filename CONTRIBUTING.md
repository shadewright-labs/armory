# Contributing

Use an issue in this repository for a bug report or proposal about Armory. Include a small example and the expected behaviour. Open an issue before changing the public API so callers' needs can be discussed first.

## Making a change

1. Keep each pull request to one issue and link it with `Closes #<number>`.
2. Follow [AGENTS.md](AGENTS.md): the invariants, Conventional Commits with the allowed scopes, tests and handling sensitive data.
3. Add or update tests and Go documentation in the same pull request as the code. For a bug fix, include a test that fails before the fix. A new parser comes with a fuzz target and its limits.
4. Run `go build ./...`, `go vet ./...`, `go test -race -count=1 ./...`, `golangci-lint run ./...` and `govulncheck ./...` before asking for review. For workflow or script changes, also run `actionlint`, `zizmor .github/workflows`, `shellcheck hack/*.sh hack/test/*.sh` and the tests under `hack/test/`.
5. A new third-party dependency needs its reason in the pull request description (see invariant 2 in AGENTS.md).

CI ends in one required job, `ci-ok`; [docs/ci.md](docs/ci.md) lists what it runs. CI uploads the coverage profile to Codecov from pull requests in this repository. Coverage is reported only and never fails a check; forks and Dependabot pull requests have no upload token, so they skip the upload.

Decisions that fix a package boundary, an API contract, a security rule or a dependency go in an ADR under [docs/adr](docs/adr/README.md) first.

Report vulnerabilities privately using the [security policy](SECURITY.md). See the [code of conduct](CODE_OF_CONDUCT.md) for conduct reports.

## Releases

Releases are generated from Conventional Commits on `main` by `.github/workflows/release-please.yml`. Do not edit `CHANGELOG.md` or `.release-please-manifest.json` by hand, and do not tag `vX.Y.Z` manually. A maintainer merges the open `chore: release X.Y.Z` pull request when a release is ready; the merge creates the `vX.Y.Z` tag on `main` and a draft GitHub release. The tag push runs `.github/workflows/release-provenance.yml`, which downloads the module zip from `proxy.golang.org` with `sum.golang.org` verification, attests it, attaches the zip and its `.sha256` to the release and publishes the draft. See [docs/ci.md](docs/ci.md#releases) for details and verification commands (`go mod verify` and `gh attestation verify`).

## Licence

Contributions are licensed under the [Apache License 2.0](LICENSE). By opening a pull request, you confirm that you may contribute the change under that licence.
