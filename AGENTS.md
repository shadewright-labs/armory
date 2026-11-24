# Armory contributor guide

Armory is a Go module for shared redaction, hygiene checks and tamper-evident audit chains. It has no dependency on any product.

- Write issues, pull requests, commits, code comments and docs in English.
- Open an issue before changing the public API. Describe the effect on callers there.
- Use Conventional Commits for commits and pull request titles. The optional scopes are `redact`, `hygiene`, `audit`, `docs`, `ci` and `deps`.
  Start the summary with a lower-case letter and omit the final full stop. Mark incompatible changes with `!` and a `BREAKING CHANGE:` footer.
- Keep secrets and personal data out of the repository, issues, pull requests and CI output. Use synthetic test fixtures.
- Write table-driven tests and run `go test -race ./...` once the module exists. Include a regression test with each bug fix.
