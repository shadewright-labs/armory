#!/usr/bin/env bash
# Offline tests of hack/ci-changed.sh. Each case builds a throwaway Git repository with a base commit and one
# change on top, runs the detector the way the `changes` job does (EVENT_NAME=pull_request, BASE_SHA=<base>)
# and compares its three output lines. Usage: hack/test/ci-changed_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
detector="${root}/hack/ci-changed.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0 n=0

test_email="detector-test@example.com"
git_q() { git -c user.name=test -c user.email="${test_email}" -c commit.gpgsign=false "$@"; }

# new_repo: a repository whose base commit holds a package, its testdata, docs and a workflow.
new_repo() {
  repo="${work}/repo$((n += 1))"
  mkdir -p "${repo}" && cd "${repo}"
  git_q init -q -b main .
  mkdir -p pkg/testdata docs/adr .github/ISSUE_TEMPLATE
  echo 'package pkg' > pkg/a.go
  echo '{}' > pkg/testdata/reply.json
  echo 'module m' > go.mod
  echo '# adr' > docs/adr/README.md
  echo 'name: Task' > .github/ISSUE_TEMPLATE/task.yml
  echo '# readme' > README.md
  echo 'notes' > notes.txt
  git_q add -A && git_q commit -qm base
  base="$(git rev-parse HEAD)"
}

commit() { git_q add -A && git_q commit -qm change --allow-empty; }

# expect <name> <go> <docs> <codeql> [base override]
expect() {
  local name="$1" want got
  want="$(printf 'go=%s\ndocs=%s\ncodeql=%s' "$2" "$3" "$4")"
  got="$(EVENT_NAME=pull_request BASE_SHA="${5-${base}}" "${detector}" 2>/dev/null)" || { echo "FAIL ${name}: detector exited non-zero"; fails=$((fails + 1)); cd "${root}"; return; }
  if [ "${got}" = "${want}" ]; then
    echo "ok   ${name}"
  else
    echo "FAIL ${name}: got $(echo "${got}" | tr '\n' ' ')want $(echo "${want}" | tr '\n' ' ')"
    fails=$((fails + 1))
  fi
  cd "${root}"
}

new_repo; echo 'package x' > "café.go"; commit
expect "non-ASCII Go file name at the root" true false true

new_repo; printf 'package pkg\n' > "pkg/fixture	name.go"; commit
expect "tab in a path" true false true

new_repo; mkdir -p bulk; for i in $(seq 1 3500); do echo "$i" > "bulk/f$i.txt"; done; echo 'package x' > 00.go; commit
expect "00.go plus 3,500 other files" true false true

new_repo; echo 'package x' > "file with space.go"; commit
expect "spaces in a Go file name" true false true

new_repo; echo 'package x' > ./-leading.go; commit
expect "leading dash" true false true

new_repo; echo '{"a":1}' > pkg/testdata/reply.json; commit
expect "testdata of a package" true false false

new_repo; mkdir -p pkg/testdata/fuzz/FuzzX; printf 'go test fuzz v1\n' > pkg/testdata/fuzz/FuzzX/seed; commit
expect "fuzz seed" true false false

new_repo; echo 'more' >> pkg/README.md; commit
expect "Markdown inside a package directory" true false false

new_repo; mkdir -p newpkg; echo 'package newpkg' > newpkg/doc.go; commit
expect "new package" true false true

new_repo; git_q mv pkg/a.go notes-renamed.txt; commit
expect "Go file renamed to text" true false true

new_repo; git_q rm -q pkg/a.go; commit
expect "Go file deleted" true false true

new_repo; echo 'go 1.26.0' >> go.mod; commit
expect "go.mod only" true false true

new_repo; printf 'go 1.26.0\nuse ./absent\n' > go.work; commit
expect "go.work only" true false false

new_repo; echo 'example.invalid/m v1.0.0 h1:x=' > go.work.sum; commit
expect "go.work.sum only" true false false

new_repo; echo 'version: "2"' > .golangci.yml; commit
expect "linter configuration" true false false

new_repo; mkdir -p hack; echo 'echo' > hack/new.sh; commit
expect "script under hack" true false false

new_repo; echo 'more' >> docs/adr/README.md; commit
expect "docs only" false true false

new_repo; mkdir -p overrides; echo '<!-- x -->' > overrides/main.html; commit
expect "overrides file only" false true false

new_repo; echo 'site_name = "Armory"' > zensical.toml; commit
expect "zensical.toml only" false true false

new_repo; echo 'zensical==0.1.1' > requirements-docs.in; commit
expect "requirements-docs.in only" false true false

new_repo; echo 'zensical==0.1.1' > requirements-docs.txt; commit
expect "requirements-docs.txt only" false true false

new_repo; mkdir -p hack; echo '# checker' > hack/docs-site-check.py; commit
expect "built-site checker script" true true false

new_repo; mkdir -p hack; echo '# pending' > hack/docs-pages-pending.txt; commit
expect "docs pending list" true true false

new_repo; printf 'docs-check:\n\ttrue\n' > Makefile; commit
expect "Makefile only" false true false

new_repo; printf 'docs-check:\n\ttrue\n' > GNUmakefile; commit
expect "GNUmakefile only" false true false

new_repo; printf 'docs-check:\n\ttrue\n' > makefile; commit
expect "lowercase makefile only" false true false

new_repo; mkdir -p .github/workflows; echo 'on: push' > .github/workflows/docs.yml; commit
expect "docs deploy workflow" false true true

new_repo; echo 'more' >> README.md; commit
expect "README only" false false false

new_repo; echo 'more' >> .github/ISSUE_TEMPLATE/task.yml; commit
expect "issue template only" false false false

new_repo; commit
expect "empty diff" false false false

new_repo; echo x >> notes.txt; commit
expect "missing base object" true true true 0000000000000000000000000000000000000000

new_repo; echo x >> notes.txt; commit
expect "unset base" true true true ""

for wf in ci lint test vulncheck gosec docs-check workflow-lint codeql secret-scan repo-checks; do
  new_repo; mkdir -p .github/workflows; echo 'on: push' > ".github/workflows/${wf}.yml"; commit
  expect "workflow of the graph: ${wf}.yml" true true true
done

new_repo; mkdir -p hack; echo 'x' > hack/ci-ok.sh; commit
expect "ci-ok predicate" true true true

new_repo; mkdir -p .github/workflows; echo 'on: push' > .github/workflows/fuzz.yml; commit
expect "fuzz workflow" true false true

new_repo; mkdir -p .github/workflows; echo 'on: push' > .github/workflows/scorecard.yml; commit
expect "standalone workflow" false false true

new_repo; echo x >> notes.txt; commit
got="$(cd "${repo}" && TMPDIR="${work}/no/such/dir" EVENT_NAME=pull_request BASE_SHA="${base}" "${detector}" 2>/dev/null)"
if [ "${got}" = "$(printf 'go=true\ndocs=true\ncodeql=true')" ]; then
  echo "ok   classification error selects everything"
else
  echo "FAIL classification error selects everything: got ${got}"
  fails=$((fails + 1))
fi

got="$(EVENT_NAME=push "${detector}" 2>/dev/null)"
if [ "${got}" = "$(printf 'go=true\ndocs=true\ncodeql=true')" ]; then
  echo "ok   push selects everything"
else
  echo "FAIL push selects everything"
  fails=$((fails + 1))
fi

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
