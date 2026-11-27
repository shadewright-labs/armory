# ADR-0002: Package `redact`, pattern redaction moved from Shadewright

- Status: Proposed
- Date: 2026-11-27

This record sets the boundary and the contract of `github.com/shadewright-labs/armory/redact` before any code moves (epic 40, issue 41). It builds on Shadewright [ADR-0048](https://github.com/shadewright-labs/shadewright/blob/main/docs/adr/0048-redaction-in-a-shared-module.md), which decided the two-stage move, the contract, version pinning and the dependency ceiling from Shadewright's side. This record states what Armory promises to every product and does not repeat the checks only Shadewright runs. Redaction of exact held values is a separate package and record (ADR-0003, `redact/exact`).

## Context

### What each product has

Shadewright, `internal/redact` on `main` at `80fb7abc`:

- 12,171 lines of production code in the package itself, plus `released` (the reading of the release before #845, 4,537 lines without its test copy), `meter` (297 lines) and `redacttest` (4,425 lines). The tests are 24,285 lines.
- Nine fuzz targets: `FuzzString`, `FuzzReplacementBoundaries`, `FuzzDiffString`, `FuzzDiffStringTwice`, `FuzzDiffStrings`, `FuzzDiffReplaceFrom`, `FuzzHeapWithinWeight`, `FuzzMonotonicAgainstMain` and, in `released`, `FuzzReleasedMatchesKeptCopy`. Six of them have a seed directory under `testdata/fuzz/`; `FuzzReplacementBoundaries`, `FuzzHeapWithinWeight` and `FuzzMonotonicAgainstMain` seed with `f.Add` in code.
- 32 files under `testdata/`. Two kinds live there. Library corpora, such as `main-redaction.jsonl` (the regression corpus of `String`, 1,760 rows with the input, the output and the hidden spans). And entry-point corpora that belong to Shadewright's integration tests and stay in Shadewright under ADR-0048 point 10 (`canonical-main-entry.tsv`, the largest file at 3.5 MB, `fields-main-entry.tsv`, `controls-gate-main.tsv`, `invalid-utf8-same.tsv`, `notes-main-entry.tsv`, `readings-tools-main.tsv`, `references-main-entry.tsv`).
- Rules run on RE2 (`regexp`). Each entry has a work budget of 64 Ki steps plus `WorkPerByte` (1,024 steps) per byte. An entry over `MaxEntry` (4 MiB) or over its budget is withheld whole. Output is never longer than input. A process-wide limit, half the Go memory limit or 64 MiB without one, caps the heap of the redactions running at once.
- Stage A of ADR-0048 is partly merged. Production code imports no Shadewright package outside its own tree, and no Prometheus, controller-runtime or logr; withheld entries and heap waits reach Shadewright's metrics through `Hooks`. The one third-party import left is `golang.org/x/sync/semaphore` (`heaplimit.go`, `shortentry.go`). Not done yet: 24 test files in the package still import other Shadewright packages or Kubernetes types (for example `diff_fuzz_test.go`, `export_shortflag_test.go`, `fields_entry_test.go`) and have not moved to an integration test package; three of them use helpers that exist only in `_test.go` files of package `redact` (`export_test.go`, `export_keys_test.go`, `export_shortflag_test.go`), which another module cannot import; `redacttest` does not yet wrap `released`, so `internal/incidentcontext` and `internal/tools` tests still import `internal/redact/released`; and the import allow-list test of point 9 does not exist. ADR-0048 also asks for one Shadewright release under Stage A before Stage B.
- The exported surface is about 60 package-level identifiers (a few more with `-tags redactwork`) and 22 methods of `Redactor`; the move pull request lists them exactly. Six of them have no reference outside the package: `ShortEntryHeap`, `PieceBytes`, `ItemBytes`, `WorkFixed`, `HeapPerByte` and `Unmetered`. ADR-0048 counted 82 importing files outside the directory.

Sablecourt, `internal/redact` on `main` at `ab08a63`: 118 lines, ten regular expressions applied one after another with `ReplaceAllString`, and an API of `Text`, `String` and `ContainsSensitive`. Thirty-five production files import it. It has no size bound, no work budget and no withheld state, and its markers can be longer than the text they replace. Sablecourt issue 126 lists forms it passes through: a nested `secret:` / `value:` pair, a URL with user and password, and `Authorization: Basic`.

### How they drifted

On 2026-11-27 Sablecourt's `String` was run over the 1,760 rows of Shadewright's `main-redaction.jsonl`. In 702 rows Shadewright hides at least one span. Sablecourt leaves 585 of those 702 inputs unchanged. Part of the gap is over-redaction Shadewright keeps on purpose (IP addresses, emails, long runs of hexadecimal), so the number measures how far the two rule sets are apart, not a count of leaks. The confirmed misses are the rows of Sablecourt issue 126.

The gap also runs the other way for a few token shapes. Sablecourt accepts shorter suffixes than Shadewright: `gh[pousr]_` with 20 or more characters (Shadewright: 36 to 255), `github_pat_` with 20 or more (Shadewright: 22 to 255) and `AIza` with 20 or more (Shadewright: exactly 35). Real tokens of those kinds have Shadewright's lengths, and Shadewright's long-token rule catches many shorter ones that mix letters and digits, but a short all-letter suffix that Sablecourt hides today passes through Shadewright's rules.

### Invariants touched

1. No product knowledge. The code takes no product type, but comments cite Shadewright issues and design sections, and some exported names were shaped by Shadewright's read tools (`Piece`, `Form`, `Written`, the `StringTwice` family that reads the release before).
2. Standard library first. `golang.org/x/sync/semaphore` would be Armory's first third-party dependency.
3. No secret in output. `New` has four error returns. Two quote the kind of an extra pattern (one with `%q`), and three wrap an error whose text quotes the expression: `regexp.Compile`'s, `meter.CompileExpr`'s and `released.New`'s. The package reads the Go memory limit with `debug.SetMemoryLimit(-1)`, which is not a credential.
4. Bounded and fuzzed. Redaction reads untrusted text. It is bounded by `MaxEntry`, the budget and the heap limit, and fuzzed. The bounds are package constants, not caller settings (see "Reading of invariant 4").
5. The API is a promise. The surface is large.

## Decision

### 1. Boundary

- `redact` hides credential-shaped text found by pattern. It decides nothing about where a product's boundary is, and it knows no held secret values.
- It is not composed with `redact/exact` inside Armory. A product that uses both runs the exact registry first and this package second (ADR-0003).
- The move changes no rule. A rule change after the move is an ordinary change under the contract in point 4.
- Layout: `redact` (public), `redact/redacttest` (public test helpers, the release-before reading for differential tests, the must-hide corpus runner), `redact/internal/meter` and `redact/internal/released`. Placing them under `redact/internal/` rather than the module's `internal/` keeps other Armory packages from importing them. This narrows ADR-0048 point 1, which named the module's `internal/`.
- Stage B (the move) starts only when the checklist "Before stage B starts" in ADR-0048 is complete, every item of it: Stage A is merged and has been through one Shadewright release; the attestation check (point 6), the tag-to-tag diff of the bump pull request (point 7), the disclosure path (point 8) and the dependency ceiling in both repositories (point 9) are in place; the module's repository has the `v*` tag ruleset of point 5 and its own CI of point 10; Shadewright's builds set `GOFLAGS=-mod=readonly` and `GOWORK=off`. This record adds one precondition: ADR-0048 point 5 is amended in Shadewright (raising Shadewright to go 1.27.4; its toolchain is already go1.27.4, so only GODEBUG defaults change). An Armory record cannot amend an accepted Shadewright record, so the amendment is Shadewright's to make (open question 1).
- Stage A is complete when the import allow-list test of point 9 exists and passes in Shadewright's `internal/redact`, with its test files that import other Shadewright packages moved out, the `export_*_test.go` helpers they use moved to `redacttest` or into those tests, and `redacttest` wrapping `released`. The test is the gate, not a count of files. Until then Shadewright's switch cannot be a change of import path alone.

### 2. Public API

The surface moves as it is, so that Shadewright's switch is a change of import path and nothing else, with two exceptions. The six identifiers that nothing outside the package references (`ShortEntryHeap`, `PieceBytes`, `ItemBytes`, `WorkFixed`, `HeapPerByte`, `Unmetered`) become unexported in the move, so they never enter Armory's API; `Unmetered` leaves ADR-0048 point 3's tag API for that reason. And `WithWorkBudget` moves into `redacttest` (below), so Shadewright's test files change that one call. Signatures are those on Shadewright's `main` at the commit named above. The core, which Armory documents as the contract for any product:

```go
func New(opts ...Option) (*Redactor, error)
type Option interface{ /* unexported method */ }
type Pattern struct{ Kind, Expr string } // an Option
type Hooks struct {                       // an Option
	Withheld func(reason string)
	HeapWait func()
}
func SetDefaultHooks(h Hooks, heapFallback func(limitBytes int64))

type Redactor struct{ /* unexported */ }
func (r *Redactor) WithContext(ctx context.Context) *Redactor
func (r *Redactor) String(s string) string
func (r *Redactor) StringChecked(s string) (out string, ok bool)
func (r *Redactor) StringKeeping(s string, keep ...string) string
func (r *Redactor) StringKeepingChecked(s string, keep ...string) (out string, ok bool)
func (r *Redactor) Strings(items []string) []string
func (r *Redactor) StringsChecked(items []string) (out []string, ok bool)
func (r *Redactor) Replace(s string, repl func(kind, text string) string, between func(string) string) string
func (r *Redactor) ReplaceChecked(s string, repl func(kind, text string) string, between func(string) string) (out string, ok bool)
func (r *Redactor) Clean(s string, keep ...string) bool
func (r *Redactor) CleanChecked(s string, keep ...string) (bool, error)

// Package-level String, StringChecked, StringKeeping, StringKeepingChecked, Strings, StringsChecked,
// Replace, ReplaceChecked and Clean run the same on a default redactor.

const Withheld = "[REDACTED:withheld]"
const ReasonWork, ReasonSize, ReasonHeap, ReasonWait = "work", "size", "heap", "wait"
const KindPrivateKey, KindJWT, KindToken, KindConnectionString, KindCredential, KindEmail, KindIP = ...
var ErrWait error
const MaxEntry, WorkPerByte = 4 << 20, 1024
func Marker(kind string, n int) string
func MarkerShapes(s string) [][]int
```

The rest of the surface moves too. This is the second tier, documented on each identifier as shaped by one consumer, Shadewright: structured answers (`Piece`, `Form` with `AsIs`, `Line` and `JSON`, `Pieces`, `PiecesWithin`, `PiecesFitting`, `PiecesSize`, `Written`, `FreePieces`), short entries (`StringShort`, `ReplaceCheckedShort`, `ReplaceCheckedShortShownAndRead`, `MaxShortEntry`), the release-before reading (`StringTwice`, `StringTwiceChecked`, `ReplaceFrom`, `ReleasedText`), the heap bounds (`LargestEntry`, `OverLargestEntry`, `HeapFixed`, `HeapWeightPerByte`), `ReplaceWhole`, `SecretKey`, `SecretCollection`, `Control`, `WithoutControls`, `WithWorkBudget` (in `redacttest`, not `redact`) and `Tick`.

No product other than Shadewright calls the second tier. Sablecourt's wrapper has an import check that enforces it: its `internal/redact` may use the core above and `Pattern`, and a test fails on any other identifier of `armory/redact` that the wrapper or the rest of Sablecourt references.

`WithWorkBudget` is decided: it lowers the work budget so tests can reach the withheld path, and no production code outside the package calls it. It is a constructor in `redacttest` that returns the `Option`, so it is not part of `redact`'s API, and the move pull request shows how it reaches the package's option without a new exported name in `redact`.

Retirement of this second tier:

- `StringTwice`, `StringTwiceChecked`, `ReplaceFrom` and `ReleasedText` are deprecated and removed with the release-before reading (Shadewright issue 922, point 4).
- Every other name is kept, generalised, or deprecated with a `// Deprecated:` comment before v1.0.0. One issue filed with the move lists them, and v1.0.0 is not tagged while it is open.

Metered and unmetered functions. The functions that redact an entry (the core above, `Pieces*`, `StringTwice*`, `ReplaceFrom` and the short entries) are metered: they enforce `MaxEntry` or `MaxShortEntry`, the work budget and, except the short entries, the heap limit. The helpers `Marker`, `MarkerShapes`, `SecretKey`, `SecretCollection`, `Control`, `WithoutControls`, `Written` and `ReplaceWhole` are not metered: they have no size check, no budget and no heap share. `ReplaceWhole` is also superlinear (it searches each key at each offset) and must not take untrusted input of unbounded length; its doc comment says so. The short entries differ from `String` in three ways their doc comments state: `StringShort` and `ReplaceCheckedShort` read only the main view, not the controls view `String` adds (so a credential split by a control character can pass them while `String` hides it), to keep Shadewright's collector fingerprints stable (Shadewright ADR-0039); they wait for a short-entry slot with a background context, ignoring `WithContext`; and they never call `Hooks.HeapWait`. `OverLargestEntry` reports `ReasonSize` to the default redactor's hooks only, not to a `Redactor` built with its own.

Unexported, and staying so: scanners, spans, rule tables, windows, the canonical and folded views, meters and the heap limiter.

The promise covers signatures, the withheld-whole rule, bounded work and "what is hidden only grows" (point 4). It does not cover the bytes of a marker, and the set of kinds may grow.

### 3. The `redactwork` test API

`Work`, `WorkOfReleased` and `WorkOfExpressions` exist only with `-tags redactwork`, in package `redact`, as ADR-0048 point 3 decided. `Tick` exists in every build and does nothing without the tag, because Shadewright's read tools call it in production. This answers the open question of issue 41: the counters are the package's unexported meter, and moving the API to `redacttest` would need an accessor exported in every build. The tag API is outside the semantic versioning promise: any minor release may change it, and each identifier says so in its doc comment.

### 4. Contract between releases

- What is hidden only grows. A patch or minor release never shows a byte that the release before it hid, on the module's corpora and on the must-hide corpus. Armory's CI checks this against the previous tag (point 8, `TestHiddenOnlyGrows`). A rule that hides more ships as a patch.
- The one planned exception is retiring the release-before reading (Shadewright issue 922). It is a minor release, and its notes state the exception.
- `released` moves unchanged to `redact/internal/released`. `New` still builds it into every `Redactor`. A consumer's differential tests reach it through `redacttest` (`redacttest.ReleasedString` for `released.String`; the move pull request names each wrapper), since Go refuses an import of another module's `internal/`. No consumer imports `meter` today, and nothing replaces it.
- Governance of the guarantees a product relies on stays in that product. Shadewright's ADR-0015 keeps governing its own (ADR-0048 point 4); Sablecourt's ADR-0015 governs its own.

### 5. Dependency

`golang.org/x/sync/semaphore` stays, at the version Shadewright pins today (v0.25.0). The reason, as invariant 2 asks: the heap limiter needs a weighted semaphore that serves waiters in the order they came, so that a heavy entry is not starved by light ones, and that gives up when a context ends. The standard library has no such type. A buffered channel is not weighted, and `sync.Cond` cannot wait on a context. The module is BSD-3-Clause, maintained by the Go team in the Go project's release process, and its `go.mod` declares no dependencies.

A standard-library replacement (a mutex and a queue of waiters with a weight and a channel each, about 100 lines) is possible. It is not part of the move, because the move changes no behaviour and the heap tests were measured with x/sync's ordering. A later pull request may replace it when `TestRedactionsAtOnceStayUnderTheLimit` and the heap-wait tests pass unchanged.

The production packages may import only `golang.org/x/sync/semaphore`, each other and the 21 standard library packages ADR-0048 point 9 lists. `redacttest` has its own list, which the move pull request writes down from what the moved helpers and `MustHide` import (already more than ADR-0048's 16: `canon_corpus.go` imports `math/rand/v2`, and `MustHide` adds `encoding/json`). A test fails on any other import, for `GOOS=linux` and `GOOS=darwin`, with and without `-tags redactwork`.

### 6. Fit to the Armory invariants

- Error text. All four error returns of `New` become fixed text with the index of the pattern only: "redact: extra pattern 2: invalid kind" for the kind check, "redact: extra pattern 2: invalid expression" for `regexp.Compile` and `meter.CompileExpr`, and "redact: extra pattern 2: invalid expression for the release before" when `released.New` refuses. None of them wraps the error it replaces, so the texts of `meter` and `released`, which quote kinds and expressions, never leave the module. `released` keeps its own error text, because it moves unchanged. `TestNewRefusesAPatternWhoseSearchCannotBeWrapped`, which matches on today's text, changes with it. This is the one change of observable behaviour in the move, and it touches errors only, not output.
- Comments that cite Shadewright issues and design sections are kept as history. The package doc is rewritten to state the rules without Shadewright's terms (cluster, read tools, analyzer). No code changes for it.
- Process-wide state is kept and documented under "Security consequences".
- Fuzz targets keep their names, and the six with seed directories keep them. `hack/fuzz-targets.sh` finds them without a list.

### 7. Behaviour guarantees

Each guarantee is a property a test checks. "Entry" means one call's input to a metered function.

| Property | Statement | Test |
| --- | --- | --- |
| Bounded size | An entry longer than `MaxEntry` (`MaxShortEntry` for the short entries) is withheld before any work, with reason `size`. | `budget_test.go` |
| Linear work | The work of an entry is at most 64 Ki + `WorkPerByte*len(entry)` steps; an entry that needs more is withheld whole, with reason `work`. Work grows linearly with length on every shape of the corpora. Time is linear because rules run on RE2 and every loop counts its work. | `TestCorpusWorkGrowsLinearly` (32 and 64 KiB, every metered entry point), `TestShownTextWorkGrowsLinearlyAtAMegabyte` (512 KiB and 1 MiB, the shown short entry), `TestMonotonicStaysLinear` (`String` at 1 MiB), `TestEveryLoopCountsItsWork`, the `*WorkIsLinear` tests |
| Withheld whole | A withheld entry gives nothing of itself: the checked forms return `ok == false`, the text forms return `Withheld`. | `budget_test.go`, `lowbudget_test.go` |
| Never longer | `len(String(s)) <= len(s)`. | `FuzzString` |
| Deterministic | `String(s) == String(s)`. | `FuzzString` |
| Idempotence | Not byte for byte: a marker in place of a value can be read as a value again (`token=[REDACTED:token]`). A second pass never grows: `len(String(String(s))) <= len(String(s))`. Whether `Clean(String(s))` holds for every `s` is not known: a second pass runs on the budget of the shorter output, and a short marker could start a new match. It is a target, not a promise, until `FuzzString` checks it (open question 3). | `FuzzString` |
| Only grows | No byte hidden by the previous tag is shown by this one, on the corpora and the must-hide corpus. | `FuzzMonotonicAgainstMain` (against a kept copy), `TestHiddenOnlyGrows` (new, against the previous tag) |
| Invalid UTF-8 | Any byte string gives no panic, no growth and a deterministic answer. Valid UTF-8 in gives valid UTF-8 out. For input that is not valid UTF-8, valid output is not promised; the shown-text entry points drop invalid bytes, as their doc says. | `FuzzString`, `FuzzReplacementBoundaries` |
| Long lines | There is no per-line limit. A line is part of its entry, bounded by `MaxEntry` and the budget. | the linear work tests above |
| Bounded heap | One entry holds at most `HeapFixed` plus a per-byte bound on the listed shapes, and the redactions running at once are weighed at `HeapWeightPerByte` per byte against the process limit. | `TestHeapPerByteOfEveryShapeStrings`, `...Twice`, `...Sequences`, `...Pieces`, `FuzzHeapWithinWeight`, `TestRedactionsAtOnceStayUnderTheLimit` |
| No secret in errors | Error text is fixed and never quotes an input, a pattern kind or an expression. Hooks receive a reason constant only. `Redactor` holds rules and no secret. | `TestErrorsQuoteNothing` (new: canaries in `Kind` and `Expr` for each of the four refusals, the whole error chain walked) |
| Concurrency | A `Redactor` is safe for concurrent use. | the race tests with `ParallelUnderRace` |

Several of these tests skip under `-race` (`TestCorpusWorkGrowsLinearly`, `TestShownTextWorkGrowsLinearlyAtAMegabyte`, the four heap tests and others) and some need `-tags redactwork`. Armory's `test.yml` today runs only `go test -race ./...`, so point 8 adds the jobs that run them.

### 8. Tests in Armory

- Corpora: the library corpora move with their tests; the entry-point corpora stay in Shadewright (ADR-0048 point 10). The move pull request carries the migration inventory of point 10 (each test, fixture and seed directory and where it lives after the move), and CI compares the count of tests before and after under both tag sets.
- Must-hide corpus (issue 20): `redact/redacttest/testdata/musthide.jsonl`, about 200 rows to start, each with an input template and the parts that must not survive. Values are generated at run time from a fixed seed, so no row holds a credential-shaped literal and the secret scan stays quiet. `redacttest.MustHide(t, redact func(string) (string, bool))` runs it against any redactor, so Sablecourt can run it against its own before it switches.
- CI jobs added with the move: a test job without `-race`, so the growth and heap tests run (the heap tests split into their four shards); a job with `-tags redactwork`, without `-race`, for the work counters and the work check of the differential fuzz targets; and fuzzing of the nine targets with `-tags redactwork`, briefly on every pull request and for minutes every week.
- Differential: `TestHiddenOnlyGrows` runs the corpora and the must-hide corpus through this tree and the previous tag, and fails on any byte newly shown. A nested module is skipped by `go test ./...` and is not bumped by release-please, so the previous tag is not pinned in a tracked `go.mod`. A separate CI job builds a throwaway module at run time, requires the newest `v*` tag that contains `redact`, and runs the test; it is the one job that reaches `proxy.golang.org`. Until a tag containing `redact` exists (the first release with the package), the job reports that there is no previous tag and passes, and `FuzzMonotonicAgainstMain` against the kept copy is the only check.
- The low-budget check (`TestLoweredBudgetOnlyInTests`) runs in Armory with its paths changed to `redact/redacttest`. Because `WithWorkBudget` lives in `redacttest`, which is a public package, the check in each product asserts that no non-test file imports `redacttest`. Shadewright keeps its own copy, and Sablecourt adds one with its adoption.

### 9. A fix for a leak

1. The report comes in privately (`SECURITY.md`) and the fix is built in the private fork of a GitHub security advisory on armory. The fix has a failing test first, whose corpus row names the leaked form.
2. GitHub Actions do not run in an advisory's private fork. The author runs the jobs of point 8 (the fuzz targets, the `redactwork` jobs and `TestHiddenOnlyGrows`) locally or on a private runner, and records the results in the advisory.
3. The fix merges with a pull request title that names the area and not the leak, for example `fix(redact): hide more forms of connection strings`, and the release pull request is merged at once. The fix release itself carries the `retract` directive for the leaky versions. The CVE is requested as `SECURITY.md` describes.
4. Each product bumps its pin in its own pull request, Shadewright and Sablecourt in either order, in the same working window as step 3. Neither description describes the leak. Each product's CI fails when its pinned version is retracted (Shadewright has this check per ADR-0048 point 8; Sablecourt adds it with its adoption).
5. The advisory is published only after each product has released with the bump.

From step 3 the merged diff on `main`, the corpus row of the test and the changelog are public, until step 5. Publishing after the product releases, and not after the bump pull requests, makes that window longer: it lasts until the slower product ships. That is the cost. Steps 3 and 4 run together to keep the window as short as it can be, and the title names no form, though the test row does (open question 4).

## Reading of invariant 4

Note, 2026-11-27. Invariant 4 asks that a parser read within "limits the caller sets". The bounds of `redact` (`MaxEntry`, the work budget, `MaxShortEntry`, the heap fallback of 64 MiB and the longest heap wait) are package constants, and only the heap limit follows a setting the caller owns (the Go memory limit). This is a deliberate reading, the same as ADR-0001's for its fixed bounds. The bounds were measured with the corpora and the heap tests, and the contract "what is hidden only grows" holds only for the budget it was measured on: a caller that raised the budget would get untested heap use, and one that lowered it would get entries withheld that are hidden today. Fixed bounds are a ceiling every caller gets, so they satisfy the purpose of invariant 4 without a setting. Whether `AGENTS.md` mentions this reading is for the user to decide when accepting this record.

## Security consequences

The library guarantees, under the tests of point 7: an entry that cannot be redacted within its bounds gives nothing of itself; output of `String` is never longer than input; error text and hook arguments hold no input, kind or expression; a later release hides at least what an earlier one hid, on the corpora.

Process-wide state, which the package doc states:

- The heap limiter is shared by every `Redactor` in a process, because it bounds the heap of that process. Its size is read once, at the first redaction.
- `SetDefaultHooks` writes the default hooks and the fallback callback without a lock. It must be called once, during initialisation, before any redaction: called concurrently with a redaction it is a data race, and called after the first redaction its fallback callback is never called, because the limiter has already sized itself.
- `released` caches a compiled expression per `*regexp.Regexp` in a process-wide map and never evicts it. `New` compiles fresh expressions on every call, so a product that calls `New` per request grows that map without bound. A product builds its `Redactor` values once at start-up while `released` exists (until Shadewright issue 922).

The products keep: which text reaches a redactor and where; what a withheld entry shows; calling the unmetered helpers only on bounded input; the order of `exact` and `redact` (ADR-0003); and the guarantees their own ADR-0015 records.

## How each product adopts it

### Shadewright, in 0.8.0

One pull request, as ADR-0048 stage B describes, after the Stage A work listed in point 1: `go.mod` requires an exact armory version, the importing files change their import path, the test files change `redacttest`'s path, and `internal/redact` is deleted. Its hook adapter and metric names, its entry-point corpora and its fuzz targets stay in Shadewright, and run with `-tags redactwork` against the pinned version. Output does not change: the full corpus and the differential runs are green before and after. What changes for Shadewright:

- Its `go` line. Armory's `go.mod` says `go 1.27.4`; Shadewright's says `go 1.26.0` with `toolchain go1.27.4`. Importing Armory raises Shadewright's line to at least 1.27.4, which contradicts ADR-0048 point 5 (the module's line at or below the lowest consumer's). An Armory record cannot amend that point, so the amendment is made in Shadewright and is a precondition of Stage B (point 1): ADR-0048 point 5 is amended in Shadewright (raising Shadewright to go 1.27.4; its toolchain is already go1.27.4, so only GODEBUG defaults change). It changes the parenthetical naming Shadewright's line and keeps the rule: Armory's line stays at or below the lowest consumer's, and a later Armory bump waits for both products. Go's default `GODEBUG` settings follow the `go` line and not the `toolchain` line, so the bump pull request lists the defaults that differ between 1.26 and 1.27 and checks each against Shadewright before it merges.
- The error text of `New` (point 6). The switch pull request checks that no caller matches on it.
- The import path of `released` disappears for consumers; the `redacttest` wrappers give the same reading.

### Sablecourt, after 0.1.0

Sablecourt's `internal/redact` becomes a thin wrapper over `armory/redact` that keeps `Text`, `String` and `ContainsSensitive`, so its 35 importing files do not change. What changes for Sablecourt, each item checked in its adoption pull request:

- It first runs `redacttest.MustHide` against its current redactor, which fails on the rows of its issue 126, then switches.
- Its own token shapes (`sc_task_`, `op_`) become a `Pattern` option, for example kind `sablecourt-token`. The shorter `gh[pousr]_`, `github_pat_` and `AIza` suffixes it hides today (Context) stay as `Pattern` options by default, so Sablecourt does not get weaker. Dropping one later is a change under its own ADR-0015.
- Markers change from Sablecourt's kinds (`openai_key`, `github_token`, `bearer_token`) to Armory's (`token`, `credential` and the others), and are never longer than the text they replace. `Result.Categories` is filled from the kinds `Replace` reports. Code that writes or matches a marker itself changes with it: `internal/runtime/coding/output.go` and `council.go` write `[REDACTED:private_key]`, where Armory writes `[REDACTED:private-key]`.
- A withheld entry (over 4 MiB or over budget) returns `ok == false` from the checked forms. Each call site decides what a withheld entry shows; the wrapper returns `Withheld` for the text form. `ContainsSensitive` is built on the checked forms and returns true when `ok == false`, so a withheld entry counts as sensitive; it does not search the output for `[REDACTED:`, because Armory's short markers (`[token]`, `***`) do not have that prefix and a withheld entry reports no hit.
- More is hidden: IP addresses, emails and long hexadecimal runs such as commit hashes. Where Sablecourt shows commit hashes on purpose (pull request evidence), it passes them as `keep` values to `StringKeeping`. Its learning validators reach `ContainsSensitive` through `ContainsLearningSecret` (`internal/store/learning.go`), so learning IDs, repository URLs with an IP host and path globs with a long hex segment start being refused; the adoption decides per validator whether that is wanted.
- Structured answers. The MCP gate (`internal/gate/mcp/gate.go`) marshals a result to JSON, runs `redact.Text` on the bytes and writes them as a 200 answer with `Content-Type: application/json`. Armory may replace a JSON object under a secret key with a marker, and a withheld entry is `[REDACTED:withheld]`, so the output can be invalid JSON. The adoption either redacts the decoded values before marshalling or checks `json.Valid` on the output and answers with an error when it fails, and handles `ok == false` as an error, not as a body.
- The stream paths that the inventory missed. Three places redact per chunk or per value and write their own markers: `internal/attach/redaction.go` (line 264 calls `redact.String` on each value, line 270 writes `[REDACTED:<kind>]` and line 17 a private key marker), `internal/runtime/docker/streams.go` (line 87 calls `redact.String` on the whole council output, line 89 writes `[REDACTED:private_key]`, line 56 the output limit marker) and `cmd/sablecourtd/councils.go` (line 53 writes `[REDACTED:sablecourt_token]` before `store.RedactCouncilAnswer`). They adopt it through the wrapper's checked forms. A withheld chunk (`ok == false`) is never passed on: the stream writes the one marker for that chunk and the text of the chunk goes to no buffer or log. In live attach, a chunk is cut at an arbitrary byte, so a credential can span two chunks; the adoption keeps its existing hold-back of a partial prefix, and does not rely on the redactor to join chunks. A heap wait (`Hooks.HeapWait`, `ErrWait`) blocks the stream goroutine for up to the longest wait, so the adoption decides how long a live attach may stall, counts the waits through `Hooks`, and on a wait that times out withholds the chunk and does not show it. Their own markers move to Armory's kinds with the rest, and the tests that match them change in the same pull request.
- The daemon sets a Go memory limit, or accepts the 64 MiB fallback for the redactions running at once. It builds its `Redactor` once at start-up (Security consequences).
- It pins an exact version, adds the retraction check of point 9 and its own copy of `TestLoweredBudgetOnlyInTests`.

## Alternatives considered

- Leave a copy in each product. No coordination, but on the measurement above Sablecourt would have to rebuild most of Shadewright's rules to close the gap, and every later fix is written twice.
- Sablecourt imports Shadewright's operator module. Go refuses an import of `internal/`, and exporting the package brings the operator's Kubernetes and model provider dependencies into Sablecourt's build.
- A smaller API designed fresh for Armory, with Shadewright keeping its extras. Shadewright's extras reach unexported scanners and spans, so they cannot live outside the package; Shadewright would keep its own copy, and the drift comes back.
- A third-party scanner such as gitleaks. It finds and reports secrets in files; it does not promise output no longer than input, a withheld state, a work budget or "hidden only grows", and it brings its own dependency tree into every product. Shadewright's corpus and history would be lost.
- Replace x/sync with a standard-library semaphore in the move. It saves one dependency, at the cost of re-measuring the heap tests in the same pull request that must change nothing. Deferred, not refused (point 5).
- The `redactwork` API in `redacttest`. Cleaner separation, but it needs a counter accessor exported in every build, and Shadewright's switch would also rename calls in its test files. Possible later in a minor release, since the tag API is outside the promise.
- Lower Armory's `go` line to 1.26.0 instead of raising Shadewright's. It keeps ADR-0048 point 5 untouched, but Armory raised its line to 1.27.4 on purpose (pull request 39), Sablecourt is on 1.27.4, and ADR-0003 relies on `encoding/json/jsontext`, which this toolchain ships without an experiment flag.

## Consequences

- Armory gains its first third-party dependency. Every product that requires Armory gets `golang.org/x/sync` in its module graph and `go.sum`, even if it imports only `modelusage`.
- About 80 exported identifiers become public API. The second tier is reviewed before v1.0.0, and some of it will be deprecated.
- A rule fix becomes an Armory pull request, a release and a bump in each product. An urgent fix waits for all of them, plus the advisory when it is a leak, which is published after each product has released.
- One bad release reaches every product at once. The must-hide corpus, the differential test against the previous tag, and each product's own entry-point tests stand against that.
- Armory's CI takes on the package's corpora, nine fuzz targets, a job without `-race`, the `redactwork` jobs and the previous-tag job. The move pull request reports the added CI time; it is not measured yet.
- Shadewright's `go` line rises to 1.27.4, after ADR-0048 point 5 is amended in Shadewright.
- The move waits for the rest of Stage A and one Shadewright release.

## Open questions

1. The `go` line: ADR-0048 point 5 is amended in Shadewright, raising Shadewright to go 1.27.4 (its toolchain is already go1.27.4, so only `GODEBUG` defaults change), as a precondition of Stage B. The amendment is Shadewright's decision, not this record's. The alternative is to lower Armory's line to 1.26.0 before the move.
2. The second tier of the API: which names stay for v1.0.0. This record moves them all (except `WithWorkBudget`, which moves to `redacttest`), retires the release-before names with issue 922 and decides nothing else about v1.
3. `Clean(String(s))`: run `FuzzString` with that check before the move. If it holds, it becomes a guarantee; if it fails, the record states the case and the check stays out.
4. Advisory timing (point 9): publish the advisory after each product has released with the bump (proposed here, with the longer public-diff window as the cost), or at the next Armory release.
5. Whether `AGENTS.md` records the reading of invariant 4 (see "Reading of invariant 4").

## Sources

Read on 2026-11-27.

- Shadewright, `main` at `80fb7abc`: `internal/redact/` (all files), `internal/modules/`, `go.mod`, [ADR-0048](https://github.com/shadewright-labs/shadewright/blob/main/docs/adr/0048-redaction-in-a-shared-module.md), epic [shadewright#1132](https://github.com/shadewright-labs/shadewright/issues/1132).
- Sablecourt, `main` at `ab08a63`: `internal/redact/redact.go`, `internal/store/learning.go`, `internal/gate/mcp/gate.go`, `internal/runtime/coding/`, issue [sablecourt#126](https://github.com/shadewright-labs/sablecourt/issues/126), ADR-0015.
- Armory: `AGENTS.md`, `go.mod`, `.github/workflows/test.yml`, ADR-0001, issues 20, 40 and 41.
