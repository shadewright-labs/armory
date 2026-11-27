#!/usr/bin/env bash
# Structural tests of the workflows, for properties that a workflow edit could quietly lose:
#   - every action from another repository is pinned to a full commit SHA, every workflow but pr-title.yml
#     starts with `permissions: {}`, and every checkout sets persist-credentials: false;
#   - no script under hack/ downloads an executable without a sha256 check;
#   - the coverage upload: the race run writes the profile, a separate job uploads it only after the tests
#     passed, skips the upload without a token and cannot hide a failure from ci-ok (no continue-on-error), and
#     ci.yml passes it only the Codecov token;
#   - vulncheck.yml runs on its schedule and by hand whatever `inputs.run` holds there;
#   - every fuzz target is run briefly on pull requests and for minutes in fuzz.yml, both from hack/fuzz-targets.sh;
#   - release-please.yml and release-provenance.yml enforce per-job permissions, timeouts, sumdb/proxy guards,
#     Origin.Hash commit binding, SECURITY ERROR fast-fail, cross-job output hashes, rerun overwrite and pre-release skip.
# Usage: hack/test/workflows_test.sh [repository root]
set -euo pipefail

root="${1:-$(cd "$(dirname "$0")/../.." && pwd)}"
python3 -I - "${root}" <<'PY'
import os, re, sys

root = sys.argv[1]
fails = 0


def check(name, ok):
    global fails
    print(("ok   " if ok else "FAIL ") + name)
    if not ok:
        fails += 1


def read(path):
    with open(os.path.join(root, path), encoding="utf-8") as f:
        return f.read()


def jobs(text):
    """Maps each job name to its text (the lines indented under it in the jobs: block)."""
    out, cur, in_jobs = {}, None, False
    for line in text.splitlines():
        if re.match(r"^jobs:\s*$", line):
            in_jobs = True
            continue
        if in_jobs and re.match(r"^\S", line):
            in_jobs = False
        if not in_jobs:
            continue
        m = re.match(r"^  ([A-Za-z0-9_-]+):\s*$", line)
        if m:
            cur = m.group(1)
            out[cur] = ""
            continue
        if cur:
            out[cur] += line + "\n"
    return out


for wf in sorted(os.listdir(os.path.join(root, ".github/workflows"))):
    text = read(".github/workflows/" + wf)
    uses = re.findall(r"^\s*(?:-\s+)?uses:\s*(\S+)", text, re.M)
    remote = [u for u in uses if not u.startswith("./")]
    check(f"{wf}: every remote action pinned to a commit SHA",
          all(re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}", u) for u in remote))
    if wf != "pr-title.yml":
        check(f"{wf}: permissions: {{}} at workflow level", re.search(r"^permissions: \{\}$", text, re.M) is not None)
    checkouts = len(re.findall(r"uses: actions/checkout@", text))
    check(f"{wf}: every checkout drops its credentials", text.count("persist-credentials: false") == checkouts)
check("pr-title.yml: no checkout", "actions/checkout@" not in read(".github/workflows/pr-title.yml"))

for script in sorted(os.listdir(os.path.join(root, "hack"))):
    if script.endswith(".sh"):
        text = read("hack/" + script)
        if re.search(r"^\s*curl ", text, re.M):
            check(f"{script}: every download is checked against a sha256",
                  text.count("sha256sum --check --strict") >= len(re.findall(r"^\s*curl ", text, re.M)))

tj = jobs(read(".github/workflows/test.yml"))
test_job, cov = tj.get("test", ""), tj.get("coverage", "")
check("test: the race run writes the coverage profile",
      "go test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./..." in test_job)
check("test: the profile is uploaded as an artifact",
      "actions/upload-artifact@" in test_job and "path: coverage.out" in test_job and "if-no-files-found: error" in test_job)
check("test: every fuzz target from hack/fuzz-targets.sh is run briefly",
      "hack/fuzz-targets.sh" in test_job and "-fuzztime 10s" in test_job)
check("coverage: a separate job after a successful test job",
      "needs: test" in cov and "needs.test.result == 'success'" in cov)
check("coverage: no continue-on-error anywhere in test.yml", "continue-on-error" not in read(".github/workflows/test.yml"))
check("coverage: every step but the notice needs the token",
      cov.count("if: env.CODECOV_TOKEN != ''") == 3 and cov.count("if: env.CODECOV_TOKEN == ''") == 1)
check("coverage: pinned action, uploader version and slug",
      re.search(r"codecov/codecov-action@[0-9a-f]{40} ", cov) is not None and "version: v11.3.2" in cov
      and "slug: shadewright-labs/armory" in cov and "files: coverage.out" in cov and "disable_search: true" in cov)
check("coverage: only contents: read", re.findall(r"^\s+[a-z-]+: (?:read|write)$", cov, re.M) == ["      contents: read"])
ci = read(".github/workflows/ci.yml")
cj = jobs(ci)
check("ci: the test job gets the Codecov token and nothing else",
      re.search(r"secrets:\n\s+CODECOV_TOKEN: \$\{\{ secrets\.CODECOV_TOKEN \}\}\n", cj.get("test", "")) is not None
      and "secrets: inherit" not in ci)

vuln = read(".github/workflows/vulncheck.yml")
check("vulncheck: scheduled and manual runs never skip",
      "if: ${{ github.event_name == 'schedule' || github.event_name == 'workflow_dispatch' || inputs.run != false }}" in vuln
      and re.search(r"^  schedule:", vuln, re.M) is not None)
fuzz = read(".github/workflows/fuzz.yml")
check("fuzz: targets come from hack/fuzz-targets.sh, on a schedule",
      "hack/fuzz-targets.sh" in fuzz and re.search(r"^  schedule:", fuzz, re.M) is not None)

def job_permissions(job_text):
    """Returns 'scope: level' entries from the job's permissions: block."""
    perms, in_perms = [], False
    for line in job_text.splitlines():
        if re.match(r"^    permissions:\s*$", line):
            in_perms = True
            continue
        if in_perms:
            if not line.strip() or line.lstrip().startswith("#"):
                continue
            if not line.startswith("      "):
                break
            perms.append(re.sub(r"\s+#.*$", "", line).strip())
    return perms


rpj = jobs(read(".github/workflows/release-please.yml"))
rp = rpj.get("release-please", "")
check("release-please: only contents: read",
      job_permissions(rp) == ["contents: read"])
check("release-please: runs in release environment",
      re.search(r"^\s+environment:\s*release\s*$", rp, re.M) is not None)
check("release-please: timeout-minutes is set",
      re.search(r"^\s+timeout-minutes:\s*[1-9][0-9]*\s*$", rp, re.M) is not None)

prov = read(".github/workflows/release-provenance.yml")
pj = jobs(prov)
p_fetch, p_attest, p_upload = pj.get("fetch", ""), pj.get("attest", ""), pj.get("upload", "")
for jname, jbody in (("fetch", p_fetch), ("attest", p_attest), ("upload", p_upload)):
    check(f"release-provenance/{jname}: timeout-minutes is set",
          re.search(r"^\s+timeout-minutes:\s*[1-9][0-9]*\s*$", jbody, re.M) is not None)
check("release-provenance/fetch: only contents: read",
      job_permissions(p_fetch) == ["contents: read"])
check("release-provenance/attest: exact permissions (contents: read, id-token: write, attestations: write)",
      job_permissions(p_attest) == [
          "contents: read",
          "id-token: write",
          "attestations: write",
      ])
check("release-provenance/upload: only contents: write",
      job_permissions(p_upload) == ["contents: write"])
check("release-provenance/fetch: GOENV=off on both go invocations",
      p_fetch.count("GOENV=off") >= 2)
check("release-provenance/fetch: explicit GOPROXY without direct on both go invocations",
      len(re.findall(r"\bGOPROXY=https://proxy\.golang\.org(?:\s|\\|$)", p_fetch)) >= 2
      and ",direct" not in p_fetch and "GOPROXY=direct" not in p_fetch)
check("release-provenance/fetch: explicit GOSUMDB=sum.golang.org on both go invocations",
      p_fetch.count("GOSUMDB=sum.golang.org") >= 2)
check("release-provenance/fetch: clears override env vars and unsets GOVCS on both go invocations",
      all(p_fetch.count(tok) >= 2 for tok in (
          "GOPRIVATE=", "GONOSUMDB=", "GONOPROXY=", "GOINSECURE=", "GOFLAGS=", "-u GOVCS"
      )))
check("release-provenance and docs: no bogus GONOSUMCHECK variable",
      "GONOSUMCHECK" not in prov and "GONOSUMCHECK" not in read("docs/ci.md"))
check("release-provenance/fetch: ties Origin.Hash to checked-out tag commit",
      "Origin.Hash" in p_fetch and "git rev-parse" in p_fetch)
check("release-provenance/attest: verifies attestation with --source-ref",
      "--source-ref" in p_attest)
check("release-provenance/fetch: aborts immediately on SECURITY ERROR",
      "SECURITY ERROR" in p_fetch)
check("release-provenance/fetch: exports zip_sha256 and mod_sum outputs",
      re.search(r"^\s+zip_sha256:", p_fetch, re.M) is not None
      and re.search(r"^\s+mod_sum:", p_fetch, re.M) is not None)
check("release-provenance/attest: verifies against needs.fetch.outputs",
      "needs.fetch.outputs.zip_sha256" in p_attest and "needs.fetch.outputs.mod_sum" in p_attest)
check("release-provenance/upload: verifies against needs.fetch.outputs",
      "needs.fetch.outputs.zip_sha256" in p_upload and "needs.fetch.outputs.mod_sum" in p_upload)
check("release-provenance/fetch: upload-artifact enables overwrite for reruns",
      "overwrite: true" in p_fetch)
check("release-provenance: skips pre-release tags cleanly with notice",
      "::notice::" in p_fetch and "skip=true" in p_fetch
      and "needs.fetch.outputs.skip != 'true'" in p_attest
      and "needs.fetch.outputs.skip != 'true'" in p_upload)

if fails:
    print(f"{fails} case(s) failed", file=sys.stderr)
    sys.exit(1)
print("all cases passed")
PY
