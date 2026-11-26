# Contributing

Use an issue in this repository for a bug report or proposal about Armory. Include a small example and the expected behaviour. Open an issue before changing the public API so callers' needs can be discussed first.

## Making a change

1. Keep each pull request to one issue and link it with `Closes #<number>`.
2. Follow [AGENTS.md](AGENTS.md) for Conventional Commits, tests and handling sensitive data.
3. Add or update tests and Go documentation in the same pull request as the code. For a bug fix, include a test that fails before the fix.
4. Once `go.mod` exists, run `go build ./...`, `go vet ./...`, `go test -race ./...`, `golangci-lint run ./...` and `govulncheck ./...` before asking for review. Run `actionlint` for workflow changes.

CI uploads the coverage profile to Codecov from pull requests in this repository. Coverage is reported only and never fails a check; forks and Dependabot pull requests have no upload token, so they skip the upload.

Report vulnerabilities privately using the [security policy](SECURITY.md). See the [code of conduct](CODE_OF_CONDUCT.md) for conduct reports.

## Licence

Contributions are licensed under the [Apache License 2.0](LICENSE). By opening a pull request, you confirm that you may contribute the change under that licence.
