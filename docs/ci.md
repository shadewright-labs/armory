# CI

This page describes the checks under `.github/workflows/` that run on pull requests, on pushes to main and on a schedule. The layout follows the organisation's product repositories ([Shadewright](https://github.com/shadewright-labs/shadewright) and [Sablecourt](https://github.com/shadewright-labs/sablecourt)), without their image, chart and end-to-end jobs: Armory is a library with no binaries.

## Pull requests and pushes to main

`ci.yml` runs on pull requests, pushes to main and by hand, and calls the workflows below. Its `Changed files` job runs `hack/ci-changed.sh` on the files the pull request changes and sets two groups:

| Group | Runs when these change | Jobs |
| --- | --- | --- |
| go | any `.go` file; `go.mod`, `go.sum`, `go.work`, `go.work.sum`, `.golangci.yml`; any file inside a top-level directory other than `docs/` and `.github/` (package code, `testdata/`, fuzz seeds, `hack/`); `.github/workflows/fuzz.yml` | lint, test, govulncheck, gosec |
| codeql | `.go` files, `go.mod`, `go.sum`, workflows, `.github/actions/`, `.github/codeql/` | CodeQL (pull requests only) |

A change to `ci.yml`, to a workflow it calls, to `hack/ci-changed.sh` or to `hack/ci-ok.sh` selects both groups, and so does a push to main or a manual run. The script reads the diff as NUL-separated paths, tries to fetch a missing base commit, and selects every group when the diff still fails or anything in the classification fails. Its offline tests are `hack/test/ci-changed_test.sh`.

The groups call these workflows:

- `lint.yml` (go): `go mod tidy -diff`, `golangci-lint config verify` and `golangci-lint run ./...` with golangci-lint v2.15.0. The configuration is `.golangci.yml`; it enables the linters of Shadewright's configuration except the two that only apply there (`ginkgolinter`, `logcheck`).
- `test.yml` (go): `go build ./...`, `go vet ./...`, `go test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./...` (which also runs the seed corpus of every fuzz target), then ten seconds of fuzzing for each target that `hack/fuzz-targets.sh` lists. A second job, `coverage upload`, runs only after the test job succeeded and sends the profile to Codecov with the pinned `codecov/codecov-action` and uploader version. `ci.yml` passes it only the secret `CODECOV_TOKEN`; without it the job prints a notice and passes. `codecov.yml` makes the coverage statuses informational, so coverage never blocks a merge.
- `vulncheck.yml` (go): govulncheck v1.9.0 with the Go release of `go.mod`. It also runs every Monday and by hand. Its job condition is `github.event_name == 'schedule' || github.event_name == 'workflow_dispatch' || inputs.run != false`: a scheduled run has no inputs, and an empty input compares equal to `false`, so testing `inputs.run` alone would skip the weekly scan.
- `gosec.yml` (go): gosec v2.30.0 with G104, G304 and G204 excluded. The job also fails when gosec's output shows a package load or analysis error, or a scan of zero files, even if gosec exits 0.
- `codeql.yml` (codeql): CodeQL for Go and for the workflows (language `actions`). It also runs on pushes to main, every Monday and by hand.

Three jobs run on every pull request, whatever changed:

- `repo-checks.yml`: `python3 -I hack/docs-links.py check`, which follows the relative links and heading fragments of the tracked Markdown files and fails on a missing target (pages not written yet can be listed in `hack/docs-pages-pending.txt`), then the offline tests under `hack/test/`: the changed-files detector, the `ci-ok` predicate, the fuzz target list, the link checker and `workflows_test.sh`. The last one checks that every remote action is pinned to a full commit SHA, that every workflow but `pr-title.yml` starts with `permissions: {}`, that every checkout sets `persist-credentials: false`, that every download in `hack/` is checked against a sha256, the shape of the coverage upload, the condition of `vulncheck.yml` and that both fuzz runs take their targets from `hack/fuzz-targets.sh`.
- `workflow-lint.yml`: actionlint 1.7.12 (download checked against its sha256), shellcheck over `hack/*.sh` and `hack/test/*.sh`, and zizmor over `.github/workflows`, installed with `pip --require-hashes` from `hack/zizmor-requirements.txt`.
- `secret-scan.yml`: gitleaks 8.30.1 (installed by `hack/install-gitleaks.sh`, checked against its sha256) with the default rules and `.gitleaks.toml`, over every ref and the full history (`fetch-depth: 0`), including changes that only a merge commit introduces. The scan command is `hack/secret-scan.sh`, which fails closed when the history cannot be read: it compares the commits gitleaks reports as scanned with the commits `git` lists, and records the exit status of each `git log` gitleaks starts. `.gitleaks.toml` has no allowlist, because the history has no finding.

Before the scan, `hack/test/gitleaks_test.sh` runs the same script on throwaway repositories as positive and negative controls. The tracked files of the tree pass. A synthetic token or private key fails the scan in an ordinary file, in a Go test file, in a testdata fixture, appended to a tracked fixture, and when only a merge commit adds it. Four kinds of broken history read fail it too. The test also requires that any allowlist added later is one exact value in one file, scoped to the commits that hold it. The secrets in the test are assembled at run time, so the file holds none.

The last job, `ci-ok`, needs all of them and runs `hack/ci-ok.sh` on their results. It passes only when `Changed files` succeeded and each of its two outputs is exactly `true` or `false`, every job that the groups and the event select succeeded, and every job that always runs succeeded. A job missing from the results, a selected job that was skipped, and any failure or cancellation fail `ci-ok`. It sees one result per called workflow, so a step removed from a called workflow goes unnoticed there; `workflows_test.sh` guards the steps that matter most. Its tests are `hack/test/ci-ok_test.sh`, which also checks that the job list in the script matches the `needs` of `ci-ok`.

Two more workflows run on pull requests outside `ci.yml`. `dependency-review.yml` (job `Dependency review`) fails on a new or bumped dependency with a known vulnerability of moderate severity or higher, or with a GPL, AGPL or LGPL licence (ten SPDX identifiers). `pr-title.yml` (job `Conventional PR title`) checks the title against Conventional Commits and the scope list of AGENTS.md. It runs on `pull_request_target`, so GitHub takes the workflow from the base branch: a change to the scope list takes effect for other pull requests only after it is merged.

Every workflow except `pr-title.yml` starts with `permissions: {}` and grants each job its own permissions; `pr-title.yml` grants `pull-requests: read` and has no checkout step. Every action from another repository is pinned to a full commit SHA, and Dependabot proposes updates weekly.

### Required checks

The checks that block a merge are a repository setting, the branch ruleset for main; no file here sets it. The intended required checks are `ci-ok`, `Dependency review` and `Conventional PR title`, as in the product repositories.

## Scheduled runs

| Workflow | When | What |
| --- | --- | --- |
| `vulncheck.yml` | Mondays, by hand | govulncheck |
| `codeql.yml` | Mondays, pushes to main, by hand | CodeQL for Go and workflows |
| `scorecard.yml` | Mondays, pushes to main, branch protection changes, by hand | OpenSSF Scorecard, results to the Security tab and the README badge |
| `fuzz.yml` | Tuesdays, by hand | one job per fuzz target that `hack/fuzz-targets.sh` lists, four minutes each; the corpus grows in the Actions cache from week to week, and a failing input is uploaded as an artifact |

`hack/fuzz-targets.sh` takes its list from `go test -list '^Fuzz' ./...`, so a new target in any package is fuzzed without being listed anywhere, and it fails when `go test` fails or finds no target. `hack/fuzz-failures.sh` collects the untracked files under `testdata/fuzz/` that a failing run leaves. Their tests are `hack/test/fuzz-targets_test.sh`.

## Releases

Armory is published as Go source. A release is a tag `vX.Y.Z` on a commit of `main`; the Go module proxy builds the module zip from the tag and the checksum database records its hash, so every caller downloads the same bytes. There is no release workflow, no binary and no image. Creating `v*` tags should be limited to repository admins by a tag ruleset, as in the product repositories, and a published tag is never moved or deleted.

Provenance for a source-only module can take two forms, and neither is in place yet:

- A signed annotated tag (`git tag -s vX.Y.Z`), which GitHub shows as verified. It proves who made the tag, and the checksum database already proves the zip has not changed since.
- An attestation of the module zip: a workflow on the tag builds the zip with `go mod download -json` (or `golang.org/x/mod/zip`), checks its hash against `sum.golang.org` and attests it with `actions/attest-build-provenance`. This gives SLSA build provenance for the source archive and lets the Scorecard Signed-Releases check pass if the zip is also attached to a GitHub release.

The proposal is to start with signed tags at v0.1.0 and add the attestation workflow when a product first pins a release.
