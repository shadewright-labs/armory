# ADR-0002: Package `redact`, pattern redaction moved from Shadewright

- Status: Proposed
- Date: 2026-11-27

This record sets the boundary and the contract of `github.com/shadewright-labs/armory/redact` before any code moves (epic 40, issue 41). It builds on Shadewright [ADR-0048](https://github.com/shadewright-labs/shadewright/blob/main/docs/adr/0048-redaction-in-a-shared-module.md), which decided the two-stage move, the contract, version pinning and the dependency ceiling from Shadewright's side. This record states what Armory promises to every product and does not repeat the checks only Shadewright runs. Redaction of exact held values is a separate package and record (ADR-0003, `redact/exact`).

## Context

### What each product has

Shadewright, `internal/redact` on `main` at `80fb7abc`:

- 12,171 lines of production code in the package itself, plus `released` (the reading of the release before #845, 4,537 lines without its test copy), `meter` (297 lines) and `redacttest` (4,425 lines). The tests are 24,285 lines.
- Nine fuzz targets: `FuzzString`, `FuzzReplacementBoundaries`, `FuzzDiffString`, `FuzzDiffStringTwice`, `FuzzDiffStrings`, `FuzzDiffReplaceFrom`, `FuzzHeapWithinWeight`, `FuzzMonotonicAgainstMain` and, in `released`, `FuzzReleasedMatchesKeptCopy`.
- About 30 corpus files under `testdata/`. The largest, `main-redaction.jsonl`, has 1,760 rows with the input, the output and the hidden spans.
- Rules run on RE2 (`regexp`). Each entry has a work budget of `WorkFixed` (64 Ki steps) plus `WorkPerByte` (1,024 steps) per byte. An entry over `MaxEntry` (4 MiB) or over its budget is withheld whole. Output is never longer than input. A process-wide limit, half the Go memory limit or 64 MiB without one, caps the heap of the redactions running at once.
- Stage A of ADR-0048 is merged. Production code imports no Shadewright package outside its own tree, and no Prometheus, controller-runtime or logr. The one third-party import left is `golang.org/x/sync/semaphore` (`heaplimit.go`, `shortentry.go`). Withheld entries and heap waits reach Shadewright's metrics through `Hooks`.
- The exported surface is 64 package-level identifiers and 22 methods of `Redactor`. Every package-level identifier is referenced outside the package. ADR-0048 counted 82 importing files outside the directory.

Sablecourt, `internal/redact` on `main` at `ab08a63`: 118 lines, ten regular expressions applied one after another with `ReplaceAllString`, and an API of `Text`, `String` and `ContainsSensitive`. Thirty-five production files import it. It has no size bound, no work budget and no withheld state, and its markers can be longer than the text they replace. Sablecourt issue 126 lists forms it passes through: a nested `secret:` / `value:` pair, a URL with user and password, and `Authorization: Basic`.

### How they drifted

On 2026-11-27 Sablecourt's `String` was run over the 1,760 rows of Shadewright's `main-redaction.jsonl`. In 702 rows Shadewright hides at least one span. Sablecourt leaves 585 of those 702 inputs unchanged. Part of the gap is over-redaction Shadewright keeps on purpose (IP addresses, emails, long runs of hexadecimal), so the number measures how far the two rule sets are apart, not a count of leaks. The confirmed misses are the rows of Sablecourt issue 126.

### Invariants touched

1. No product knowledge. The code takes no product type, but comments cite Shadewright issues and design sections, and some exported names were shaped by Shadewright's read tools (`Piece`, `Form`, `Written`, the `StringTwice` family that reads the release before).
2. Standard library first. `golang.org/x/sync/semaphore` would be Armory's first third-party dependency.
3. No secret in output. `New` quotes the kind of an extra pattern with `%q` and wraps the error of `regexp.Compile`, which quotes the expression. The package reads the Go memory limit with `debug.SetMemoryLimit(-1)`, which is not a credential.
4. Bounded and fuzzed. Redaction reads untrusted text. It is bounded by `MaxEntry`, the budget and the heap limit, and fuzzed.
5. The API is a promise. The surface is large.

## Decision

### 1. Boundary

- `redact` hides credential-shaped text found by pattern. It decides nothing about where a product's boundary is, and it knows no held secret values.
- It is not composed with `redact/exact` inside Armory. A product that uses both runs the exact registry first and this package second (ADR-0003).
- The move changes no rule. A rule change after the move is an ordinary change under the contract in point 4.
- Layout: `redact` (public), `redact/redacttest` (public test helpers, the release-before reading for differential tests, the must-hide corpus runner), `redact/internal/meter` and `redact/internal/released`. Placing them under `redact/internal/` rather than the module's `internal/` keeps other Armory packages from importing them. This narrows ADR-0048 point 1, which named the module's `internal/`.

### 2. Public API

The surface moves as it is, so that Shadewright's switch is a change of import path and nothing else. Signatures are those on Shadewright's `main` at the commit named above. The core, which Armory documents as the contract for any product:

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
const MaxEntry, WorkPerByte, WorkFixed = 4 << 20, 1024, 64 << 10
func Marker(kind string, n int) string
func MarkerShapes(s string) [][]int
```

The rest of the surface moves too, documented on each identifier as shaped by one consumer: structured answers (`Piece`, `Form` with `AsIs`, `Line` and `JSON`, `Pieces`, `PiecesWithin`, `PiecesFitting`, `PiecesSize`, `Written`, `FreePieces`, `PieceBytes`, `ItemBytes`), short entries (`StringShort`, `ReplaceCheckedShort`, `ReplaceCheckedShortShownAndRead`, `MaxShortEntry`, `ShortEntryHeap`), the release-before reading (`StringTwice`, `StringTwiceChecked`, `ReplaceFrom`, `ReleasedText`), the heap bounds (`LargestEntry`, `OverLargestEntry`, `HeapPerByte`, `HeapFixed`, `HeapWeightPerByte`), `ReplaceWhole`, `SecretKey`, `SecretCollection`, `Control`, `WithoutControls`, `WithWorkBudget` and `Tick`. Before v1.0.0 each of these is kept, generalised, or deprecated with a `// Deprecated:` comment and removed; the issue that tracks it is filed with the move.

Unexported, and staying so: scanners, spans, rule tables, windows, the canonical and folded views, meters and the heap limiter.

The promise covers signatures, the withheld-whole rule, bounded work and "what is hidden only grows" (point 4). It does not cover the bytes of a marker, and the set of kinds may grow.

### 3. The `redactwork` test API

`Work`, `WorkOfReleased`, `WorkOfExpressions` and `Unmetered` exist only with `-tags redactwork`, in package `redact`, as ADR-0048 point 3 decided. `Tick` exists in every build and does nothing without the tag, because Shadewright's read tools call it in production. This answers the open question of issue 41: the counters are the package's unexported meter, and moving the API to `redacttest` would need an accessor exported in every build. The tag API is outside the semantic versioning promise: any minor release may change it, and each identifier says so in its doc comment.

### 4. Contract between releases

- What is hidden only grows. A patch or minor release never shows a byte that the release before it hid, on the module's corpora and on the must-hide corpus. Armory's CI checks this against the previous tag (point 8, `TestHiddenOnlyGrows`). A rule that hides more ships as a patch.
- The one planned exception is retiring the release-before reading (Shadewright issue 922). It is a minor release, and its notes state the exception.
- `released` moves unchanged to `redact/internal/released`. `New` still builds it into every `Redactor`. A consumer's differential tests reach it through `redacttest`, since Go refuses an import of another module's `internal/`.
- Governance of the guarantees a product relies on stays in that product. Shadewright's ADR-0015 keeps governing its own (ADR-0048 point 4); Sablecourt's ADR-0015 governs its own.

### 5. Dependency

`golang.org/x/sync/semaphore` stays, at the version Shadewright pins today (v0.25.0). The reason, as invariant 2 asks: the heap limiter needs a weighted semaphore that serves waiters in the order they came, so that a heavy entry is not starved by light ones, and that gives up when a context ends. The standard library has no such type. A buffered channel is not weighted, and `sync.Cond` cannot wait on a context. The module is BSD-3-Clause, maintained by the Go team in the Go project's release process, and its `go.mod` declares no dependencies.

A standard-library replacement (a mutex and a queue of waiters with a weight and a channel each, about 100 lines) is possible. It is not part of the move, because the move changes no behaviour and the heap tests were measured with x/sync's ordering. A later pull request may replace it when `TestRedactionsAtOnceStayUnderTheLimit` and the heap-wait tests pass unchanged.

The production packages may import only `golang.org/x/sync/semaphore`, each other and the 21 standard library packages ADR-0048 point 9 lists. `redacttest` has its own list. A test fails on any other import, for `GOOS=linux` and `GOOS=darwin`, with and without `-tags redactwork`.

### 6. Fit to the Armory invariants

- Error text. `New`'s three errors become fixed text with the index of the pattern only: "redact: extra pattern 2: invalid kind" and "redact: extra pattern 2: invalid expression". The kind, the expression and the text of the `regexp` error are dropped. This is the one change of observable behaviour in the move, and it touches errors only, not output.
- Comments that cite Shadewright issues and design sections are kept as history. The package doc is rewritten to state the rules without Shadewright's terms (cluster, read tools, analyzer). No code changes for it.
- Process-wide state is kept and documented: the heap limiter is shared by every `Redactor` in a process, because it bounds the heap of that process, and `SetDefaultHooks` is called once during initialisation.
- Fuzz targets keep their names and seed directories, and `hack/fuzz-targets.sh` finds them without a list.

### 7. Behaviour guarantees

Each guarantee is a property a test checks. "Entry" means one call's input.

| Property | Statement | Test |
| --- | --- | --- |
| Bounded size | An entry longer than `MaxEntry` is withheld before any work, with reason `size`. | `budget_test.go` |
| Linear work | The work of an entry is at most `WorkFixed + WorkPerByte*len(entry)` steps; an entry that needs more is withheld whole, with reason `work`. Work grows linearly with length on every shape of the corpora. Time is linear because rules run on RE2 and every loop counts its work. | `TestCorpusWorkGrowsLinearly`, `TestShownTextWorkGrowsLinearlyAtAMegabyte`, `TestEveryLoopCountsItsWork`, the `*WorkIsLinear` tests |
| Withheld whole | A withheld entry gives nothing of itself: the checked forms return `ok == false`, the text forms return `Withheld`. | `budget_test.go`, `lowbudget_test.go` |
| Never longer | `len(String(s)) <= len(s)`. | `FuzzString` |
| Deterministic | `String(s) == String(s)`. | `FuzzString` |
| Idempotence | Not byte for byte: a marker in place of a value can be read as a value again (`token=[REDACTED:token]`). What holds: `Clean(String(s))` is true for every `s` that was not withheld, and a second pass never grows, `len(String(String(s))) <= len(String(s))`. | `FuzzString` gains the `Clean` check (today `TestClean` checks five inputs) |
| Only grows | No byte hidden by the previous tag is shown by this one, on the corpora and the must-hide corpus. | `FuzzMonotonicAgainstMain` (against a kept copy), `TestHiddenOnlyGrows` (new, against the previous tag) |
| Invalid UTF-8 | Any byte string gives no panic, no growth and a deterministic answer. Valid UTF-8 in gives valid UTF-8 out. For input that is not valid UTF-8, valid output is not promised; the shown-text entry points drop invalid bytes, as their doc says. | `FuzzString`, `FuzzReplacementBoundaries`, `testdata/invalid-utf8-same.tsv` |
| Long lines | There is no per-line limit. A line is part of its entry, bounded by `MaxEntry` and the budget. One-line shapes repeated to 1 MiB stay linear. | `TestShownTextWorkGrowsLinearlyAtAMegabyte`, `TestCorpusWorkGrowsLinearly` |
| Bounded heap | One entry holds at most `HeapFixed + HeapPerByte*len` on the listed shapes, and the redactions running at once are weighed at `HeapWeightPerByte` per byte against the process limit. | `TestHeapPerByteOfEveryShape`, `FuzzHeapWithinWeight`, `TestRedactionsAtOnceStayUnderTheLimit` |
| No secret in errors | Error text is fixed and never quotes an input, a pattern kind or an expression. Hooks receive a reason constant only. `Redactor` holds rules and no secret. | `TestErrorsQuoteNothing` (new: canaries in `Kind` and `Expr`, the whole error chain walked) |
| Concurrency | A `Redactor` is safe for concurrent use. | the race tests with `ParallelUnderRace` |

### 8. Tests in Armory

- Corpora: every `testdata` file of the package moves with its test. The move pull request carries the migration inventory of ADR-0048 point 10 (each test, fixture and seed directory and where it lives after the move), and CI compares the count of tests before and after under both tag sets.
- Must-hide corpus (issue 20): `redact/redacttest/testdata/musthide.jsonl`, about 200 rows to start, each with an input template and the parts that must not survive. Values are generated at run time from a fixed seed, so no row holds a credential-shaped literal and the secret scan stays quiet. `redacttest.MustHide(t, redact func(string) (string, bool))` runs it against any redactor, so Sablecourt can run it against its own before it switches.
- Fuzz: the nine targets, briefly on every pull request and for minutes every week.
- Growth: the linear work tests above, with and without `-tags redactwork` as each needs.
- Differential: `TestHiddenOnlyGrows` runs the corpora and the must-hide corpus through this tree and the previous tag (fetched as a module in a test-only `go.mod` under `redact/internal/prevtag`) and fails on any byte newly shown.
- The low-budget check (`TestLoweredBudgetOnlyInTests`) runs in Armory, and Shadewright keeps its own copy.

### 9. A fix for a leak

1. The report comes in privately (`SECURITY.md`) and the fix is built in the private fork of a GitHub security advisory on armory.
2. It merges with a pull request title that names the area and not the leak, for example `fix(redact): hide more forms of connection strings`, and the release pull request is merged at once.
3. Each product bumps its pin in its own pull request, Shadewright and Sablecourt in either order. Neither description describes the leak.
4. Once both bumps are merged, the advisory is published and the next Armory release adds a `retract` directive for the leaky versions. Each product's CI fails when its pinned version is retracted (Shadewright has this check per ADR-0048 point 8; Sablecourt adds it with its adoption).

The changelog of step 2 is public before step 4. That is accepted, and is why the title names no form.

## How each product adopts it

### Shadewright, in 0.8.0

One pull request, as ADR-0048 stage B describes: `go.mod` requires an exact armory version, the importing files change their import path, the test files change `redacttest`'s path, and `internal/redact` is deleted. Its hook adapter and metric names, its entry-point corpora and its fuzz targets stay in Shadewright, and run with `-tags redactwork` against the pinned version. Output does not change: the full corpus and the differential runs are green before and after. What changes for Shadewright:

- Its `go` line. Armory's `go.mod` says `go 1.27.4`; Shadewright's says `go 1.26.0` with `toolchain go1.27.4`. Importing Armory raises Shadewright's line to at least 1.27.4, which contradicts ADR-0048 point 5 (the module's line at or below the lowest consumer's). Shadewright amends that point in 0.8.0, or Armory lowers its line first. This record proposes the amendment, since Shadewright already builds with 1.27.4.
- The error text of `New` (point 6). The switch pull request checks that no caller matches on it.
- The import paths of `meter` and `released` disappear for consumers; `redacttest` gives the same access.

### Sablecourt, after 0.1.0

Sablecourt's `internal/redact` becomes a thin wrapper over `armory/redact` that keeps `Text`, `String` and `ContainsSensitive`, so its 35 importing files do not change. What changes for Sablecourt:

- It first runs `redacttest.MustHide` against its current redactor, which fails on the rows of its issue 126, then switches.
- Its own token shapes (`sc_task_`, `op_`) become a `Pattern` option, for example kind `sablecourt-token`.
- Markers change from Sablecourt's kinds (`openai_key`, `github_token`, `bearer_token`) to Armory's (`token`, `credential` and the others), and are never longer than the text they replace. `Result.Categories` is filled from the kinds `Replace` reports.
- More is hidden: IP addresses, emails and long hexadecimal runs such as commit hashes. Where Sablecourt shows commit hashes on purpose (pull request evidence), it passes them as `keep` values to `StringKeeping`.
- A withheld entry (over 4 MiB or over budget) returns `ok == false` from the checked forms. Each call site decides what a withheld entry shows; the wrapper returns `Withheld` for the text form.
- The daemon sets a Go memory limit, or accepts the 64 MiB fallback for the redactions running at once.
- It pins an exact version and adds the retraction check of point 9.

## Alternatives considered

- Leave a copy in each product. No coordination, but on the measurement above Sablecourt would have to rebuild most of Shadewright's rules to close the gap, and every later fix is written twice.
- Sablecourt imports Shadewright's operator module. Go refuses an import of `internal/`, and exporting the package brings the operator's Kubernetes and model provider dependencies into Sablecourt's build.
- A smaller API designed fresh for Armory, with Shadewright keeping its extras. Shadewright's extras reach unexported scanners and spans, so they cannot live outside the package; Shadewright would keep its own copy, and the drift comes back.
- A third-party scanner such as gitleaks. It finds and reports secrets in files; it does not promise output no longer than input, a withheld state, a work budget or "hidden only grows", and it brings its own dependency tree into every product. Shadewright's corpus and history would be lost.
- Replace x/sync with a standard-library semaphore in the move. It saves one dependency, at the cost of re-measuring the heap tests in the same pull request that must change nothing. Deferred, not refused (point 5).
- The `redactwork` API in `redacttest`. Cleaner separation, but it needs a counter accessor exported in every build, and Shadewright's switch would also rename calls in its test files. Possible later in a minor release, since the tag API is outside the promise.

## Consequences

- Armory gains its first third-party dependency. Every product that requires Armory gets `golang.org/x/sync` in its module graph and `go.sum`, even if it imports only `modelusage`.
- 86 exported identifiers become public API. The second tier is reviewed before v1.0.0, and some of it will be deprecated.
- A rule fix becomes an Armory pull request, a release and a bump in each product. An urgent fix waits for all of them, plus the advisory when it is a leak.
- One bad release reaches every product at once. The must-hide corpus, the differential test against the previous tag, and each product's own entry-point tests stand against that.
- Armory's CI takes on the package's corpora, nine fuzz targets and the `redactwork` jobs. The move pull request reports the added CI time; it is not measured yet.
- Shadewright's `go` line rises, or ADR-0048 point 5 is kept by lowering Armory's.

## Open questions

1. The `go` line: amend Shadewright ADR-0048 point 5 so Shadewright moves to `go 1.27.4` in 0.8.0 (proposed here), or lower Armory's line to 1.26.0 before the move.
2. The second tier of the API: which names stay for v1.0.0. This record moves them all and decides nothing about v1.

## Sources

Read on 2026-11-27.

- Shadewright, `main` at `80fb7abc`: `internal/redact/` (all files), `go.mod`, [ADR-0048](https://github.com/shadewright-labs/shadewright/blob/main/docs/adr/0048-redaction-in-a-shared-module.md), epic [shadewright#1132](https://github.com/shadewright-labs/shadewright/issues/1132).
- Sablecourt, `main` at `ab08a63`: `internal/redact/redact.go`, issue [sablecourt#126](https://github.com/shadewright-labs/sablecourt/issues/126), ADR-0015.
- Armory: `AGENTS.md`, `go.mod`, issues 20, 40 and 41.
