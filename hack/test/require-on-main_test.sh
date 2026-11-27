#!/usr/bin/env bash
# Offline tests of hack/require-on-main.sh in a throwaway repository: a commit on main (or equal to it) passes,
# a commit on a side branch and a checkout without main fail.
# Usage: hack/test/require-on-main_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
script="${root}/hack/require-on-main.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
cd "${tmp}"
git init -q -b main .
git config user.email tester
git config user.name t
git config commit.gpgsign false
git commit -q --allow-empty -m one
git commit -q --allow-empty -m two
git update-ref refs/remotes/origin/main HEAD
git checkout -q -b side
git commit -q --allow-empty -m side
fail=0

if bash "${script}" main > /dev/null 2>&1; then echo "ok   main commit"; else echo "FAIL main commit refused" >&2; fail=1; fi
if bash "${script}" main~1 > /dev/null 2>&1; then echo "ok   older main commit"; else echo "FAIL older main commit refused" >&2; fail=1; fi
if bash "${script}" side > /dev/null 2>&1; then echo "FAIL side branch commit accepted" >&2; fail=1; else echo "ok   side commit refused"; fi
if bash "${script}" HEAD > /dev/null 2>&1; then echo "FAIL HEAD on side accepted" >&2; fail=1; else echo "ok   HEAD on side refused"; fi
if bash "${script}" main origin/nope > /dev/null 2>&1; then echo "FAIL missing main accepted" >&2; fail=1; else echo "ok   no main refused"; fi
if [ "${fail}" -ne 0 ]; then
  exit 1
fi
echo "all cases passed"
