# Armory: guide for contributors and coding agents

Armory is the Go module `github.com/shadewright-labs/armory`: code that more than one Shadewright Labs product needs, such as reading model token usage, redaction, hygiene checks and tamper-evident audit chains. Every product may import it, so a mistake here ships in all of them.

## Invariants

A change that weakens one of these needs an ADR in `docs/adr/` first.

1. **Armory depends on no product.** No package imports a Shadewright Labs product module, and nothing here knows a product's types, policy, tokens, model clients or configuration. Each product keeps its own policy and passes what Armory needs as arguments.
2. **Standard library first.** A new third-party dependency needs a reason in the pull request: what it does that the standard library cannot, its licence (Apache-2.0 compatible, no GPL, AGPL or LGPL), its maintenance and its own dependencies. Every product inherits it, and `Dependency review` checks the licence and known vulnerabilities.
3. **No credentials, and no secret in output.** Armory never reads credentials, environment secrets or files a caller did not pass. It makes no network calls unless that is the package's whole purpose and the caller supplies the client. It does not log. Error text is fixed: it never quotes the input, a header, a token or a URL with credentials, so an error is safe to log in any product.
4. **Every parser is bounded and fuzzed.** Code that reads input from outside (a reply, a stream, a file, a header) reads it within byte, line and count limits the caller sets, returns an error instead of panicking on malformed input, and has a fuzz target (`func FuzzXxx(f *testing.F)`) with a seed corpus. CI runs each target briefly on every pull request and for minutes every week; `hack/fuzz-targets.sh` finds new targets without a list.
5. **The public API is a promise.** Armory follows semantic versioning. Before v1.0.0 a minor version may break the API; from v1.0.0 a breaking change needs a new major version and module path (`/v2`). Open an issue before changing an exported identifier, and say there what callers must change. Deprecate with a `// Deprecated:` comment before removing.

## Commits and pull requests

Commits on `main` follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/): `type(scope)!: summary`. Pull requests are squash-merged with the title as the commit message, so the pull request title is what counts, and the required check `Conventional PR title` validates it.

- Types: `feat`, `fix`, `perf`, `refactor`, `test`, `docs`, `build`, `ci`, `chore`, `revert`.
- The scope is optional. When used it is a package (`modelusage`, `githost`, `redact`, `audit`, `hygiene`) or one of `adr`, `ci`, `docs`, `deps`, `release`. A new package adds its scope to `.github/workflows/pr-title.yml`, the issue templates and this list in the same pull request.
- The summary is in English, imperative, starts with a lower-case letter and has no final full stop.
- Mark an incompatible change with `!` and a `BREAKING CHANGE:` footer that says what callers change.

## Writing

- Issues, pull requests, commits, code comments, docs and ADRs are in English, in plain sentences.
- No long dashes (em or en dash): use a comma, colon, parentheses or a full stop.
- No generation credits or tool attributions in files, commits, pull requests or issues.
- No credentials, emails, host names, IPs or local paths in tracked files, issues, pull requests or CI output. Test fixtures are synthetic; build a fake token from split strings at run time so the secret scan stays quiet without an allowlist.

## Checks

Run these before asking for review:

```bash
go build ./...
go vet ./...
go test -race -count=1 ./...
golangci-lint run ./...
govulncheck ./...
```

For a change to a parser, also fuzz its targets for a while, for example `go test ./modelusage -run '^$' -fuzz '^FuzzParseStream$' -fuzztime 60s`. For a change under `.github/workflows/` or `hack/`, run `actionlint`, `zizmor .github/workflows`, `shellcheck hack/*.sh hack/test/*.sh` and the script tests under `hack/test/`. `docs/ci.md` describes every workflow.

Write table-driven tests, keep fixtures under the package's `testdata/`, and add a test that fails before the fix with every bug fix.

## Releases

A release is a Git tag `vX.Y.Z` on a commit of `main`; the Go module proxy and checksum database serve the source from it. There are no binaries, images or release assets. Only a repository admin creates a tag, after the pull requests of that version are merged. Never move or delete a published tag: the checksum database has recorded it, and callers would get a checksum mismatch. A fix to a release is a new patch version. See "Releases" in `docs/ci.md`.

## Decisions

Decisions that set a package's boundary, its API contract, a security rule or a dependency are recorded as ADRs in `docs/adr/` (template: `docs/adr/0000-template.md`) before the code depends on them.
