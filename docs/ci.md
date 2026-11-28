# CI

This page describes the checks under `.github/workflows/` that run on pull requests, on pushes to main and on a schedule. The layout follows the organisation's product repositories ([Shadewright](https://github.com/shadewright-labs/shadewright) and [Sablecourt](https://github.com/shadewright-labs/sablecourt)), without their image, chart and end-to-end jobs: Armory is a library with no binaries.

## Pull requests and pushes to main

`ci.yml` runs on pull requests, pushes to main and by hand, and calls the workflows below. Its `Changed files` job runs `hack/ci-changed.sh` on the files the pull request changes and sets three groups:

| Group | Runs when these change | Jobs |
| --- | --- | --- |
| go | any `.go` file; `go.mod`, `go.sum`, `go.work`, `go.work.sum`, `.golangci.yml`; any file inside a top-level directory other than `docs/`, `overrides/` and `.github/` (package code, `testdata/`, fuzz seeds, `hack/`); `.github/workflows/fuzz.yml` | lint, test, govulncheck, gosec |
| docs | `docs/`, `overrides/`, `zensical.toml`, `requirements-docs.in`, `requirements-docs.txt`, `hack/docs-site-check.py`, `hack/docs-pages-pending.txt`, `Makefile`, `GNUmakefile`, `makefile`, `.github/workflows/docs.yml` | docs site build and check |
| codeql | `.go` files, `go.mod`, `go.sum`, workflows, `.github/actions/`, `.github/codeql/` | CodeQL (pull requests only) |

A change to `ci.yml`, to a workflow it calls, to `hack/ci-changed.sh` or to `hack/ci-ok.sh` selects every group, and so does a push to main or a manual run. The script reads the diff as NUL-separated paths, tries to fetch a missing base commit, and selects every group when the diff still fails or anything in the classification fails. Its offline tests are `hack/test/ci-changed_test.sh`.

The groups call these workflows:

- `lint.yml` (go): `go mod tidy -diff`, `golangci-lint config verify` and `golangci-lint run ./...` with golangci-lint v2.15.0. The configuration is `.golangci.yml`; it enables the linters of Shadewright's configuration except the two that only apply there (`ginkgolinter`, `logcheck`).
- `test.yml` (go): `go build ./...`, `go vet ./...`, `go test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./...` (which also runs the seed corpus of every fuzz target), then ten seconds of fuzzing for each target that `hack/fuzz-targets.sh` lists. A second job, `coverage upload`, runs only after the test job succeeded and sends the profile to Codecov with the pinned `codecov/codecov-action` and uploader version. `ci.yml` passes it only the secret `CODECOV_TOKEN`; without it the job prints a notice and passes. `codecov.yml` makes the coverage statuses informational, so coverage never blocks a merge.
- `vulncheck.yml` (go): govulncheck v1.9.0 with the Go release of `go.mod`. It also runs every Monday and by hand. Its job condition is `github.event_name == 'schedule' || github.event_name == 'workflow_dispatch' || inputs.run != false`: a scheduled run has no inputs, and an empty input compares equal to `false`, so testing `inputs.run` alone would skip the weekly scan.
- `gosec.yml` (go): gosec v2.30.0 with G104, G304 and G204 excluded. The job also fails when gosec's output shows a package load or analysis error, or a scan of zero files, even if gosec exits 0.
- `docs-check.yml` (docs): installs `requirements-docs.txt` with `pip install --require-hashes` into `.venv` (`make -f Makefile docs-venv`), then runs `make -f Makefile docs-check` to build the Zensical site with `--clean --strict` and verify `site/` with `hack/docs-site-check.py`.
- `codeql.yml` (codeql): CodeQL for Go and for the workflows (language `actions`). It also runs on pushes to main, every Monday and by hand.

Three jobs run on every pull request, whatever changed:

- `repo-checks.yml`: `python3 -I hack/docs-links.py check`, which follows the relative links and heading fragments of the tracked Markdown files and fails on a missing target (pages not written yet can be listed in `hack/docs-pages-pending.txt`), then the offline tests under `hack/test/`: the changed-files detector, the `ci-ok` predicate, the fuzz target list, the link checker, the built docs site checker, the main-ancestry tag check and `workflows_test.sh`. The last one checks that every remote action is pinned to a full commit SHA, that every workflow but `pr-title.yml` starts with `permissions: {}`, that every checkout sets `persist-credentials: false`, that every download in `hack/` is checked against a sha256, the shape of the coverage upload, the condition of `vulncheck.yml` and that both fuzz runs take their targets from `hack/fuzz-targets.sh`.
- `workflow-lint.yml`: actionlint 1.7.12 (download checked against its sha256), shellcheck over `hack/*.sh` and `hack/test/*.sh`, and zizmor over `.github/workflows`, installed with `pip --require-hashes` from `hack/zizmor-requirements.txt`.
- `secret-scan.yml`: gitleaks 8.30.1 (installed by `hack/install-gitleaks.sh`, checked against its sha256) with the default rules and `.gitleaks.toml`, over every ref and the full history (`fetch-depth: 0`), including changes that only a merge commit introduces. The scan command is `hack/secret-scan.sh`, which fails closed when the history cannot be read: it compares the commits gitleaks reports as scanned with the commits `git` lists, and records the exit status of each `git log` gitleaks starts. `.gitleaks.toml` has no allowlist, because the history has no finding.

Before the scan, `hack/test/gitleaks_test.sh` runs the same script on throwaway repositories as positive and negative controls. The tracked files of the tree pass. A synthetic token or private key fails the scan in an ordinary file, in a Go test file, in a testdata fixture, appended to a tracked fixture, and when only a merge commit adds it. Four kinds of broken history read fail it too. The test also requires that any allowlist added later is one exact value in one file, scoped to the commits that hold it. The secrets in the test are assembled at run time, so the file holds none.

The last job, `ci-ok`, needs all of them and runs `hack/ci-ok.sh` on their results. It passes only when `Changed files` succeeded and each of its three outputs is exactly `true` or `false`, every job that the groups and the event select succeeded, and every job that always runs succeeded. A job missing from the results, a selected job that was skipped, and any failure or cancellation fail `ci-ok`. It sees one result per called workflow, so a step removed from a called workflow goes unnoticed there; `workflows_test.sh` guards the steps that matter most. Its tests are `hack/test/ci-ok_test.sh`, which also checks that the job list in the script matches the `needs` of `ci-ok`.

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
| `iana-registries.yml` | Wednesdays, by hand | downloads the three IANA registries the `netguard` table follows (each at most 256 KiB) and compares them with the copies pinned under `netguard/testdata/iana`; a difference fails the `compare` job (`contents: read`) and the `report` job (`issues: write` only) opens one issue per changed registry with the changed rows in a fenced block, or comments on the open issue for that registry. It is the only job with network access to a source outside GitHub, and the package itself makes none |
| `fuzz.yml` | Tuesdays, by hand | one job per fuzz target that `hack/fuzz-targets.sh` lists, four minutes each; the corpus grows in the Actions cache from week to week, and a failing input is uploaded as an artifact |

`hack/fuzz-targets.sh` takes its list from `go test -list '^Fuzz' ./...`, so a new target in any package is fuzzed without being listed anywhere, and it fails when `go test` fails or finds no target. `hack/fuzz-failures.sh` collects the untracked files under `testdata/fuzz/` that a failing run leaves. Their tests are `hack/test/fuzz-targets_test.sh`.

## Docs site

Two workflows handle the static documentation site built with Zensical from `zensical.toml`:

- `docs-check.yml` runs inside `ci.yml` when the `docs` group of `hack/ci-changed.sh` is selected (any change under `docs/` or `overrides/`, or to `zensical.toml`, `requirements-docs.in`, `requirements-docs.txt`, `hack/docs-site-check.py`, `hack/docs-pages-pending.txt`, a Makefile or `.github/workflows/docs.yml`, as well as pushes to `main` and manual `ci.yml` runs). It installs the hash-locked packages from `requirements-docs.txt` into `.venv` (`make -f Makefile docs-venv`) and runs `make -f Makefile docs-check`, which runs `.venv/bin/zensical build --clean --strict` and checks the built HTML in `site/` with `hack/docs-site-check.py`. Locally, `make docs-check` creates `.venv` via `docs-venv` when `.venv/docs-requirements.stamp` is missing or older than `requirements-docs.txt`. `ci-ok` requires `docs-check` to pass whenever `docs` is selected.
- `docs.yml` runs on pushes to `main` touching `docs/**`, `overrides/**`, `zensical.toml`, `requirements-docs.in`, `requirements-docs.txt` or `.github/workflows/docs.yml`, and on `workflow_dispatch`. Its `build` job installs `requirements-docs.txt` with `--require-hashes`, runs `.venv/bin/zensical build --clean --strict` and uploads `site` with `actions/upload-pages-artifact`. Its `deploy` job deploys that artifact with `actions/configure-pages` and `actions/deploy-pages` to the `github-pages` environment with `pages: write` and `id-token: write` under concurrency group `pages` (`cancel-in-progress: false`).

Publishing requires GitHub Pages to be enabled in the repository settings with "GitHub Actions" selected as the source. No workflow file configures that setting.

## Releases

Armory is published as Go source through the Go module proxy (`proxy.golang.org`) and the Go checksum database (`sum.golang.org`). There are no binaries and no container images.

Releases follow the same mechanism as Shadewright:

- `release-please.yml` runs on pushes to `main` inside the `release` environment (which accepts deployments from `main` only). It mints a short-lived token for the `shadewright-release` GitHub App from `RELEASE_APP_CLIENT_ID` (variable) and `RELEASE_APP_PRIVATE_KEY` (secret), then runs `googleapis/release-please-action` against `release-please-config.json` and `.release-please-manifest.json`. Using the App token lets the release pull request trigger required checks and lets the resulting tag push trigger the provenance workflow.
- Conventional Commits on `main` determine the next version: `feat` bumps the minor version, `fix` and `perf` bump the patch version, and `!` or a `BREAKING CHANGE:` footer bumps the minor version while Armory is on `0.x` (`bump-minor-pre-major: true`, `bump-patch-for-minor-pre-major: false`). `revert` appears in the changelog; `refactor`, `test`, `docs`, `build`, `ci` and `chore` stay out of it.
- The workflow keeps one release pull request open (`chore: release X.Y.Z`). A maintainer merges it when the changes for that version are ready. Merging it updates `CHANGELOG.md` and `.release-please-manifest.json`, creates a plain `vX.Y.Z` tag on `main` (`include-component-in-tag: false`, `force-tag-creation: true`) and creates a **draft** GitHub release (`draft: true`). Never create a plain `vX.Y.Z` tag by hand, and never edit `CHANGELOG.md` or `.release-please-manifest.json` outside that pull request.
- `release-provenance.yml` runs on every `v*` tag push and splits permissions across three jobs. Pre-release tags (`vX.Y.Z-...`) are skipped at the first step with a notice (exit 0) because `release-please` does not create releases for them:
  - `fetch` (`contents: read`): verifies that the tag points at a commit on `main` (`hack/require-on-main.sh`), then downloads `github.com/shadewright-labs/armory@<tag>` from `https://proxy.golang.org` (without `,direct`) with `go mod download -json` in a clean directory. Both `go` invocations run with `GOENV=off`, `GOSUMDB=sum.golang.org`, `GOPROXY=https://proxy.golang.org`, `GOVCS` unset, and `GOPRIVATE`, `GONOSUMDB`, `GONOPROXY`, `GOINSECURE` and `GOFLAGS` cleared. A `SECURITY ERROR` checksum mismatch aborts immediately; only not-found (`404`/`410`) answers are retried (up to 20 times over five minutes). The job requires `Origin.Hash` and `Origin.Ref` in the proxy metadata to match the checked-out tag commit and `refs/tags/<tag>`, checks the cached ziphash, writes `armory-<tag>.zip` and `armory-<tag>.zip.sha256`, exports the zip's `sha256` and `h1` hash as job outputs, and uploads the `module-zip` artifact with `overwrite: true` and 7-day retention.
  - `attest` (`contents: read`, `id-token: write`, `attestations: write` on this job only): recomputes the downloaded zip's `sha256` and `h1` dirhash and compares both against `needs.fetch.outputs` (not only against the `.sha256` file inside the artifact), attests the zip with `actions/attest-build-provenance`, and verifies the attestation immediately with `gh attestation verify` against `shadewright-labs/armory/.github/workflows/release-provenance.yml` and `--source-ref refs/tags/<tag>`.
  - `upload` (`contents: write` only): verifies the downloaded zip's `sha256` and `h1` dirhash against `needs.fetch.outputs`, waits up to five minutes for the draft release, uploads `armory-<tag>.zip` and `armory-<tag>.zip.sha256`, confirms both assets are uploaded and non-empty, and publishes the draft (`gh release edit --draft=false`). Immutable releases stay enabled: if any step fails before publication, the release stays a draft. Within the 7-day artifact retention window, rerunning a failed `attest` or `upload` job reuses the `module-zip` artifact; after 7 days the artifact expires and a single-job rerun fails, so a maintainer must rerun all jobs (which lets `fetch` download the zip again and overwrite the artifact).

Who can cause an attestation depends on the `v*` tag-creation ruleset, which is a repository setting and is not configured by any file in this repository; the `attest` and `upload` jobs have no GitHub Actions environment gate. The intended ruleset restricts creating `v*` tags to repository admins and the `shadewright-release` App (bypass actor on creation only), and forbids updating or deleting `v*` tags for everyone. A published tag is never moved or deleted because `sum.golang.org` records it permanently; a fix to a release is a new patch version.

### Verifying a release

Callers check the Go module cache against `go.sum` (which `go` authenticates with `sum.golang.org`) and can also verify the SLSA build provenance attestation on the module zip attached to the GitHub release:

```bash
# Verify every downloaded module in your Go module cache against go.sum:
go mod verify

# Download the release zip and checksum, check the sha256, and verify its build provenance:
TAG=v0.1.0
gh release download "${TAG}" --repo shadewright-labs/armory \
  --pattern "armory-${TAG}.zip" --pattern "armory-${TAG}.zip.sha256"
sha256sum --check --strict "armory-${TAG}.zip.sha256"
gh attestation verify "armory-${TAG}.zip" \
  --repo shadewright-labs/armory \
  --signer-workflow shadewright-labs/armory/.github/workflows/release-provenance.yml \
  --source-ref "refs/tags/${TAG}"
```
