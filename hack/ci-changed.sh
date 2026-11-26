#!/usr/bin/env bash
# Decides which CI job groups have to run, from the files a pull request changes. Called once by the `changes`
# job of .github/workflows/ci.yml, which passes the result to the jobs it calls.
# Usage: hack/ci-changed.sh    (prints go= and codeql= lines, each true or false; append to $GITHUB_OUTPUT)
# Environment:
#   EVENT_NAME  github.event_name; anything but "pull_request" (push to main, manual run) runs everything
#   BASE_SHA    base commit of the pull request; HEAD is compared with it (the checkout is the merge commit)
# Groups: go (lint, test, govulncheck, gosec), codeql (CodeQL analysis of Go code and workflows). The link
# check, the script tests, workflow lint and the secret scan run on every pull request.
# The go group takes every file inside a top-level directory other than docs/ and .github/ (each package
# directory, its testdata, and hack/), every .go file, the module and workspace files and the linter
# configuration. A change to ci.yml, to a workflow it calls or to this script runs every group. Fails open: when
# the diff cannot be computed, or anything in the classification fails, every group runs, so a problem here never
# lets a change skip its checks. Paths are read NUL-delimited (no quoting of unusual names) and matched in the
# shell, with no pipeline that a match could close early. Tests: hack/test/ci-changed_test.sh.
set -Eeuo pipefail
# Byte-wise matching: a path that is not valid UTF-8 still matches `.*`.
export LC_ALL=C

all() {
  printf 'go=true\ncodeql=true\n'
}
# Any failing command from here on prints every group as selected and exits 0. Nothing else is printed before
# the final two lines, so the output is always exactly one decision.
fail_open() {
  echo "classification failed (line ${1:-?}): running everything" >&2
  all
  exit 0
}
trap 'fail_open "$LINENO"' ERR

# Files at the root that the Go checks read, and the Go workflow that ci.yml does not call (fuzz.yml).
go_root_re='^(.*\.go|go\.mod|go\.sum|go\.work|go\.work\.sum|\.golangci\.yml|\.github/workflows/fuzz\.yml)$'
# Anything inside a top-level directory is Go input (package code, embedded files, testdata, hack/), except the
# docs and the GitHub configuration.
go_dir_re='^[^/]+/'
not_go_dir_re='^(docs|\.github)/'
# CodeQL analyses Go (built from the module) and the workflows and composite actions themselves.
codeql_re='^(.*\.go|go\.mod|go\.sum|\.github/workflows/.*|\.github/actions/.*|\.github/codeql/.*)$'
# ci.yml and the workflows it calls, and the two scripts of the graph.
self_re='^(\.github/workflows/(ci|lint|test|vulncheck|gosec|workflow-lint|codeql|secret-scan|repo-checks)\.yml|hack/ci-changed\.sh|hack/ci-ok\.sh)$'

if [ "${EVENT_NAME:-}" != "pull_request" ]; then
  all
  exit 0
fi

if [ -z "${BASE_SHA:-}" ] || ! git cat-file -e "${BASE_SHA}^{commit}" 2>/dev/null; then
  git fetch --no-tags --depth=1 origin "${BASE_SHA:-}" >&2 || true
fi
# The ERR trap is inherited by the command substitution (set -E). Without clearing it there, a failing mktemp
# would run fail_open inside the subshell and its two lines would become the file name.
list="$(trap - ERR; mktemp "${TMPDIR:-/tmp}/ci-changed.XXXXXX")"
trap 'rm -f "${list}"' EXIT
if ! git diff -z --name-only --no-renames "${BASE_SHA:-}" HEAD -- > "${list}" 2>/dev/null; then
  echo "cannot diff against ${BASE_SHA:-unset}: running everything" >&2
  all
  exit 0
fi

go=false codeql=false self=false
count=0
while IFS= read -r -d '' f; do
  count=$((count + 1))
  if [[ "${f}" =~ ${self_re} ]]; then self=true; fi
  if [[ "${f}" =~ ${go_root_re} ]]; then go=true; fi
  if [[ "${f}" =~ ${go_dir_re} ]] && ! [[ "${f}" =~ ${not_go_dir_re} ]]; then go=true; fi
  if [[ "${f}" =~ ${codeql_re} ]]; then codeql=true; fi
done < "${list}"
echo "changed files: ${count}" >&2

if [ "${self}" = true ]; then
  all
  exit 0
fi
printf 'go=%s\ncodeql=%s\n' "${go}" "${codeql}"
