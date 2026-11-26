#!/usr/bin/env bash
# Positive and negative controls for the secret scan: runs hack/secret-scan.sh (the command of secret-scan.yml)
# with this repository's .gitleaks.toml on throwaway repositories.
#   - The tracked files of this tree, committed to a new repository, pass (the fixtures hold no secret).
#   - A synthetic token or private key block fails the scan (exit 1): in an ordinary file, in a Go test file of a
#     package, in a testdata fixture, and appended to a tracked fixture of this tree.
#   - A token that only a merge commit introduces (added while resolving the merge) fails the scan.
#   - Every allowlist, if there is one, is one exact value in one file, scoped by commit.
#   - The scan fails closed when the history cannot be read (a failed `git log` of the script, a failed, empty
#     or cut short `git log` of gitleaks).
# The secrets are assembled at run time, so this file holds none. Usage: hack/test/gitleaks_test.sh [gitleaks]
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
gitleaks="${1:-gitleaks}"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0 n=0
test_email="scan-test@example.com"

alnum() { head -c "$1" < <(LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom); }
git_q() { git -c user.name=test -c user.email="${test_email}" -c commit.gpgsign=false "$@"; }

new_repo() {
  repo="${work}/r$((n += 1))"
  mkdir -p "${repo}"
  git_q -C "${repo}" init -q -b main
  cp "${root}/.gitleaks.toml" "${repo}/.gitleaks.toml"
}

# scan_repo <name> <want exit code>: scans ${repo} with the CI command.
scan_repo() {
  local got=0
  "${root}/hack/secret-scan.sh" "${gitleaks}" "${repo}" > "${work}/out" 2>&1 || got=$?
  if [ "${got}" = "$2" ]; then echo "ok   $1"; else echo "FAIL $1: exit ${got}, want $2"; sed 's/^/     /' "${work}/out"; fails=$((fails + 1)); fi
}

# scan <name> <want exit code> <path> <content> [file to start from]: the content goes before the copied file.
scan() {
  new_repo
  mkdir -p "${repo}/$(dirname "$3")"
  if [ -n "$4" ]; then printf '%s\n' "$4" > "${repo}/$3"; else : > "${repo}/$3"; fi
  if [ -n "${5:-}" ]; then cat "$5" >> "${repo}/$3"; fi
  git_q -C "${repo}" add -A
  git_q -C "${repo}" commit -qm fixture
  scan_repo "$1" "$2"
}

begin="-----BEGIN"" PRIVATE KEY-----"
end="-----END"" PRIVATE KEY-----"
pem_block() { printf '%s\n%s\n%s\n%s\n%s' "var fixtureKey = \`${begin}" "$(alnum 64)" "$(alnum 64)" "$(alnum 64)" "${end}\`"; }
token_line() { printf '%s' "var fixtureToken = \"ghp_$(alnum 36)\""; }

# The tree as it is: every tracked file in one new commit.
new_repo
git -C "${root}" ls-files -z | (cd "${root}" && xargs -0 tar -cf -) | tar -xf - -C "${repo}"
git_q -C "${repo}" add -A
git_q -C "${repo}" commit -qm tree
scan_repo "the tracked files of this tree pass" 0

scan "synthetic token in an ordinary file" 1 notes/config.txt "token = ghp_$(alnum 36)"
scan "private key in an ordinary file" 1 notes/key.txt "$(pem_block)"
scan "token in a Go test file" 1 somepkg/parse_test.go "$(token_line)"
scan "private key in a Go test file" 1 somepkg/parse_test.go "$(pem_block)"
scan "token in a testdata fixture" 1 somepkg/testdata/reply.json "{\"token\": \"ghp_$(alnum 36)\"}"

# A token added in front of a tracked fixture of this tree is found: the fixtures cannot hide one.
fixture="$(git -C "${root}" ls-files -- '*/testdata/*' | head -n 1)"
if [ -n "${fixture}" ]; then
  scan "token added to the fixture ${fixture}" 1 "${fixture}" "token = ghp_$(alnum 36)" "${root}/${fixture}"
fi

# Every allowlist has one path and one exact value, AND-ed, and names its commits. No [allowlist] table.
if python3 -I - "${root}/.gitleaks.toml" <<'PY'
import sys, tomllib
with open(sys.argv[1], "rb") as f:
    cfg = tomllib.load(f)
bad = []
for a in cfg.get("allowlists", []):
    paths, regexes = a.get("paths", []), a.get("regexes", [])
    if set(a) - {"description", "condition", "commits", "paths", "regexes"} or a.get("condition") != "AND" \
            or len(paths) != 1 or len(regexes) != 1 or not regexes[0].startswith("^") \
            or not regexes[0].endswith("$") or not a.get("commits"):
        bad.append(paths)
if cfg.get("allowlist") or bad or cfg.get("extend", {}).get("useDefault") is not True:
    print(f"bad entries: {bad}")
    sys.exit(1)
PY
then
  echo "ok   default rules on, and every allowlist is one exact value in one file, scoped by commit"
else
  echo "FAIL allowlists not scoped, or the default rules are off"
  fails=$((fails + 1))
fi

# merge_only <name> <file> <content>: the secret exists only in the merge commit; neither parent has it.
merge_only() {
  new_repo
  echo base > "${repo}/README.txt"
  git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm base
  git_q -C "${repo}" checkout -q -b topic
  echo topic > "${repo}/topic.txt"
  git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm topic
  git_q -C "${repo}" checkout -q main
  echo main > "${repo}/main.txt"
  git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm main
  git_q -C "${repo}" merge -q --no-ff --no-commit topic > /dev/null 2>&1
  mkdir -p "$(dirname "${repo}/$2")"
  printf '%s\n' "$3" >> "${repo}/$2"
  git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm merge
  scan_repo "$1" 1
}
merge_only "token added only in a merge commit" notes/merged.txt "token = ghp_$(alnum 36)"
merge_only "key added only in a merge commit" somepkg/parse_test.go "$(pem_block)"
merge_only "token added only in a merge commit to an existing file" README.txt "token = ghp_$(alnum 36)"

# The scan fails closed when the history cannot be read. A stub git first in PATH breaks `git log`, either for
# every caller or only for gitleaks' own call (`log -p -U0` with no --format), while every other git command
# runs the real git. Without the check, gitleaks reads no commits, finds nothing and exits 0.
real_git="$(command -v git)"
stub="${work}/stub"
mkdir -p "${stub}"
cat > "${stub}/git" <<EOF
#!/bin/sh
case "\${GIT_STUB_MODE}" in
  fail-log|fail-gitleaks-log|empty-gitleaks-log|partial-gitleaks-log) ;;
  *) exec "${real_git}" "\$@" ;;
esac
is_log=0 has_format=0
for a in "\$@"; do
  case "\${a}" in
    log) is_log=1 ;;
    --format=*) has_format=1 ;;
  esac
done
if [ "\${is_log}" = 1 ]; then
  if [ "\${GIT_STUB_MODE}" = fail-log ]; then echo "stub: log failed" >&2; exit 128; fi
  if [ "\${has_format}" = 0 ]; then
    if [ "\${GIT_STUB_MODE}" = fail-gitleaks-log ]; then echo "stub: log failed" >&2; exit 128; fi
    # The read stops inside the last commit: every commit header arrives, the end of the last hunk does not.
    if [ "\${GIT_STUB_MODE}" = partial-gitleaks-log ]; then
      "${real_git}" "\$@" > "${work}/full.log"
      n=\$(wc -c < "${work}/full.log")
      head -c "\$((n - 8))" "${work}/full.log"
      exit 74
    fi
    exit 0
  fi
fi
exec "${real_git}" "\$@"
EOF
chmod +x "${stub}/git"

# history_repo: a clean repository whose history has every kind of commit gitleaks does not count (empty, mode
# change, pure rename, binary, deletion, a merge with no first-parent change) next to ordinary ones.
history_repo() {
  new_repo
  printf 'a\nb\n' > "${repo}/a.txt"
  git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm text
  git_q -C "${repo}" commit -q --allow-empty -m empty
  chmod +x "${repo}/a.txt" && git_q -C "${repo}" commit -qam mode
  git_q -C "${repo}" mv a.txt b.txt && git_q -C "${repo}" commit -qm rename
  printf '\000\001\002' > "${repo}/bin.dat" && git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm binary
  git_q -C "${repo}" rm -q bin.dat && git_q -C "${repo}" commit -qm delete
  git_q -C "${repo}" checkout -q -b side
  echo side > "${repo}/side.txt" && git_q -C "${repo}" add -A && git_q -C "${repo}" commit -qm side
  git_q -C "${repo}" checkout -q main
  git_q -C "${repo}" merge -q --no-ff -s ours side -m merge
}

history_repo
scan_repo "clean history with uncounted commits passes" 0
for mode in fail-log fail-gitleaks-log empty-gitleaks-log partial-gitleaks-log; do
  history_repo
  GIT_STUB_MODE="${mode}" PATH="${stub}:${PATH}" scan_repo "history read broken (${mode}) fails" 1
done

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
