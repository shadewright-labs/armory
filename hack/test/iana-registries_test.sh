#!/usr/bin/env bash
# Offline tests of the issue body that the report job of .github/workflows/iana-registries.yml posts. The test
# takes the script of the step "Open or update one issue per registry" from the workflow text, runs it with a
# fake gh that keeps each --body-file, and checks:
#   - a worst-case diff (200 rows of 300 characters, all tildes, which the escaping makes longer) gives a body
#     within GitHub's 65536-character limit that still has the closing fence after the rows, says after it that
#     the rows were truncated and keeps the run link;
#   - a worst case of multibyte rows stays within the limit too;
#   - a small diff is posted whole, with no truncation notice;
#   - no row inside the fence holds four tildes in a row, so a row cannot close the fence.
# Usage: hack/test/iana-registries_test.sh [repository root]
set -euo pipefail

root="${1:-$(cd "$(dirname "$0")/../.." && pwd)}"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

python3 -I - "${root}/.github/workflows/iana-registries.yml" > "${tmp}/report.sh" <<'PY'
import re, sys

lines = open(sys.argv[1], encoding="utf-8").read().splitlines()
start = next(i for i, l in enumerate(lines) if l.strip() == "- name: Open or update one issue per registry")
run = next(i for i in range(start, len(lines)) if lines[i].strip() == "run: |")
indent = len(lines[run]) - len(lines[run].lstrip()) + 2
out = []
for l in lines[run + 1:]:
    if l.strip() and len(l) - len(l.lstrip()) < indent:
        break
    out.append(l[indent:])
print("\n".join(out))
PY

mkdir -p "${tmp}/bin"
cat > "${tmp}/bin/gh" <<'SH'
#!/usr/bin/env bash
# Fake gh: no open issue exists; a new issue's body file is kept under GH_FAKE_OUT.
set -euo pipefail
if [ "$1 $2" = "issue list" ]; then
  exit 0
fi
if [ "$1 $2" = "issue create" ]; then
  while [ "$#" -gt 0 ]; do
    if [ "$1" = "--body-file" ]; then
      n="$(find "${GH_FAKE_OUT}" -type f | wc -l | tr -d ' ')"
      cp "$2" "${GH_FAKE_OUT}/body-${n}.md"
    fi
    shift
  done
  exit 0
fi
echo "fake gh: unexpected call" >&2
exit 1
SH
chmod +x "${tmp}/bin/gh"

run_url="https://github.com/shadewright-labs/armory/actions/runs/123456789012"
fail=0

# run_case <name> <generator> runs the report script on one diff file and leaves the body in ${tmp}/<name>.md.
run_case() {
  local name="$1" gen="$2" case_dir="${tmp}/$1"
  mkdir -p "${case_dir}/runner/diff" "${case_dir}/out"
  python3 -I -c "${gen}" > "${case_dir}/runner/diff/iana-ipv4-special-registry-1.csv.txt"
  if ! PATH="${tmp}/bin:${PATH}" RUNNER_TEMP="${case_dir}/runner" GH_FAKE_OUT="${case_dir}/out" \
    GH_TOKEN=x GH_REPO=x RUN_URL="${run_url}" bash "${tmp}/report.sh" > "${case_dir}/log" 2>&1; then
    echo "FAIL ${name}: the report script failed" >&2
    cat "${case_dir}/log" >&2
    fail=1
    return
  fi
  cp "${case_dir}/out/body-0.md" "${tmp}/${name}.md"
}

# check_body <name> <expect truncated: yes|no> <rows that must all be present, or 0>
check_body() {
  python3 -I - "${tmp}/$1.md" "$2" "$3" "${run_url}" <<'PY'
import sys

path, truncated, rows, run_url = sys.argv[1], sys.argv[2] == "yes", int(sys.argv[3]), sys.argv[4]
body = open(path, encoding="utf-8").read()
fails = []
if len(body) > 65536:
    fails.append(f"{len(body)} characters, over 65536")
if run_url not in body:
    fails.append("no run link")
head, sep, rest = body.partition("\n~~~~text\n")
if not sep:
    fails.append("no opening fence")
close = rest.find("\n~~~~\n")
if close < 0:
    fails.append("no closing fence")
inside, after = rest[: close + 1], rest[close + 6 :]
if "~~~~" in inside:
    fails.append("a row holds four tildes")
if after.strip() and "truncated" not in after:
    fails.append("text after the closing fence")
notice = "truncated" in after
if truncated and not notice:
    fails.append("no truncation notice")
if not truncated and notice:
    fails.append("truncation notice on a small diff")
if rows and len([l for l in inside.split("\n") if l.startswith(("+", "-"))]) != rows:
    fails.append("rows missing")
name = path.rsplit("/", 1)[-1][:-3]
if fails:
    print(f"FAIL {name}: " + "; ".join(fails), file=sys.stderr)
    sys.exit(1)
print(f"ok   {name} ({len(body)} characters)")
PY
}

run_case tildes 'print("\n".join("+" + "~" * 299 for _ in range(200)))'
if [ -f "${tmp}/tildes.md" ]; then check_body tildes yes 0 || fail=1; fi

run_case multibyte 'print("\n".join("-" + "é" * 149 for _ in range(200)))'
if [ -f "${tmp}/multibyte.md" ]; then check_body multibyte no 200 || fail=1; fi

run_case mixed 'print("\n".join(("+" if i % 2 else "-") + "~~~~a" * 59 + "~~~~" for i in range(200)))'
if [ -f "${tmp}/mixed.md" ]; then check_body mixed yes 0 || fail=1; fi

run_case small 'print("-192.0.0.0/24,old,row\n+192.0.0.0/24,new,row")'
if [ -f "${tmp}/small.md" ]; then check_body small no 2 || fail=1; fi

if [ "${fail}" -ne 0 ]; then
  exit 1
fi
